// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"internal/race"
	"runtime"
	"sync/atomic"
	"unsafe"
)

// Pool 是一个可以单独保存和检索的临时对象集合。
//
// 存储在 Pool 中的任何项都可能在任何时候自动移除而无需通知。如果在这种情况下 Pool 拥有唯一引用，那么该项可能被释放。
//
// Pool 可以安全地同时被多个 goroutines 使用。
//
// Pool 的目的是为了缓存已分配但未使用的项以便以后重用，从而减轻垃圾收集器的压力。换言之，它使得构建高效、线程安全的空闲列表变得更容易。然而，并不是所有的空闲列表都适合使用 Pool。
//
// Pool 的适当用法是管理一组暂时性项，在包的并发独立客户端之间悄无声息共享并有可能重用。Pool 提供了一种方法在许多客户端之间分摊分配开销。
//
// fmt 包的一个良好使用示例是使用 Pool 维护动态大小的临时输出缓冲区存储器。当有很多 goroutines 正在活跃打印时，存储器会扩展；在空闲时则会缩小。
//
// 另一方面，作为短期对象的空闲列表并不适合作为 Pool 的一种用法，因为在这种情况下开销不容易分摊。更有效的方式是让这些对象实现自己的空闲列表。
//
// Pool 在第一次使用后不能被复制。
//
// 根据 Go 内存模型的术语，对于调用 Put(x) “先于” 返回相同值 x 的 Get 调用。
// 类似地，调用 New 返回 x 的 Get 调用之前是“先于” New 的调用。
type Pool struct {
	noCopy     noCopy         // 禁止复制
	local      unsafe.Pointer // 每个 P 固定大小的本地池，实际类型是 [P]poolLocal
	localSize  uintptr        // 本地数组的大小
	victim     unsafe.Pointer // 上个周期的本地池
	victimSize uintptr        // victims 数组的大小

	// New 可选地指定一个函数，在 Get 返回 nil 时调用生成一个值。
	// 不能在调用 Get 时并发更改它。
	New func() any
}

// Local per-P Pool appendix.
type poolLocalInternal struct {
	private any       // Can be used only by the respective P.
	shared  poolChain // Local P can pushHead/popHead; any P can popTail.
}

type poolLocal struct {
	poolLocalInternal

	// Prevents false sharing on widespread platforms with
	// 128 mod (cache line size) = 0 .
	pad [128 - unsafe.Sizeof(poolLocalInternal{})%128]byte
}

// from runtime
func fastrandn(n uint32) uint32

var poolRaceHash [128]uint64

// poolRaceAddr returns an address to use as the synchronization point
// for race detector logic. We don't use the actual pointer stored in x
// directly, for fear of conflicting with other synchronization on that address.
// Instead, we hash the pointer to get an index into poolRaceHash.
// See discussion on golang.org/cl/31589.
func poolRaceAddr(x any) unsafe.Pointer {
	ptr := uintptr((*[2]unsafe.Pointer)(unsafe.Pointer(&x))[1])
	h := uint32((uint64(uint32(ptr)) * 0x85ebca6b) >> 16)
	return unsafe.Pointer(&poolRaceHash[h%uint32(len(poolRaceHash))])
}

// Put 将x添加到池中。
func (p *Pool) Put(x any) {
	if x == nil {
		return
	}
	if race.Enabled {
		if fastrandn(4) == 0 {
			// Randomly drop x on floor.
			return
		}
		race.ReleaseMerge(poolRaceAddr(x))
		race.Disable()
	}
	l, _ := p.pin()
	if l.private == nil {
		l.private = x
	} else {
		l.shared.pushHead(x)
	}
	runtime_procUnpin()
	if race.Enabled {
		race.Enable()
	}
}

// Get 从 Pool 中选择任意一个项，将其从 Pool 中移除，并将其返回给调用者。
// Get 可能选择忽略 Pool 并将其视为空。
// 调用者不应假定 Put 传递的值与 Get 返回的值之间存在任何关系。
//
// 如果 Get 否则会返回 nil，且 p.New 非空，则 Get 返回调用 p.New 的结果。
func (p *Pool) Get() any {
	// 禁用竟态
	if race.Enabled {
		race.Disable()
	}

	l, pid := p.pin()
	x := l.private
	l.private = nil
	if x == nil {
		// 尝试弹出本地池的头。我们更倾向于使用头而不是尾部以保证重复利用的时间局部性。
		x, _ = l.shared.popHead()
		if x == nil {
			x = p.getSlow(pid)
		}
	}
	runtime_procUnpin()
	if race.Enabled {
		race.Enable()
		if x != nil {
			race.Acquire(poolRaceAddr(x))
		}
	}
	if x == nil && p.New != nil {
		x = p.New()
	}
	return x
}

func (p *Pool) getSlow(pid int) any {
	// See the comment in pin regarding ordering of the loads.
	size := runtime_LoadAcquintptr(&p.localSize) // load-acquire
	locals := p.local                            // load-consume
	// Try to steal one element from other procs.
	for i := 0; i < int(size); i++ {
		l := indexLocal(locals, (pid+i+1)%int(size))
		if x, _ := l.shared.popTail(); x != nil {
			return x
		}
	}

	// Try the victim cache. We do this after attempting to steal
	// from all primary caches because we want objects in the
	// victim cache to age out if at all possible.
	size = atomic.LoadUintptr(&p.victimSize)
	if uintptr(pid) >= size {
		return nil
	}
	locals = p.victim
	l := indexLocal(locals, pid)
	if x := l.private; x != nil {
		l.private = nil
		return x
	}
	for i := 0; i < int(size); i++ {
		l := indexLocal(locals, (pid+i)%int(size))
		if x, _ := l.shared.popTail(); x != nil {
			return x
		}
	}

	// Mark the victim cache as empty for future gets don't bother
	// with it.
	atomic.StoreUintptr(&p.victimSize, 0)

	return nil
}

// 将当前 goroutine 固定到 Pool，禁用抢占并返回 P 的 poolLocal 池和 P 的 id。
// 当使用完池时，调用者必须调用 runtime_procUnpin()。
func (p *Pool) pin() (*poolLocal, int) {
	pid := runtime_procPin()

	// 在 pinSlow 中我们先存储到 local 本地再存储 localSize 大小，这里需要相反顺序加载。
	// 由于我们禁用了抢占，GC 无法在其中发生。
	// 因此，这里我们必须至少观察 local 本地大于等于 localSize 大小。
	// 我们可以观察到一个更新/更大的 local，这没问题（我们必须观察其是否是零值初始化）。
	s := runtime_LoadAcquintptr(&p.localSize) // load-acquire
	l := p.local                              // load-consume
	if uintptr(pid) < s {
		return indexLocal(l, pid), pid
	}
	return p.pinSlow()
}

func (p *Pool) pinSlow() (*poolLocal, int) {
	// Retry under the mutex.
	// Can not lock the mutex while pinned.
	runtime_procUnpin()
	allPoolsMu.Lock()
	defer allPoolsMu.Unlock()
	pid := runtime_procPin()
	// poolCleanup won't be called while we are pinned.
	s := p.localSize
	l := p.local
	if uintptr(pid) < s {
		return indexLocal(l, pid), pid
	}
	if p.local == nil {
		allPools = append(allPools, p)
	}
	// If GOMAXPROCS changes between GCs, we re-allocate the array and lose the old one.
	size := runtime.GOMAXPROCS(0)
	local := make([]poolLocal, size)
	atomic.StorePointer(&p.local, unsafe.Pointer(&local[0])) // store-release
	runtime_StoreReluintptr(&p.localSize, uintptr(size))     // store-release
	return &local[pid], pid
}

func poolCleanup() {
	// This function is called with the world stopped, at the beginning of a garbage collection.
	// It must not allocate and probably should not call any runtime functions.

	// Because the world is stopped, no pool user can be in a
	// pinned section (in effect, this has all Ps pinned).

	// Drop victim caches from all pools.
	for _, p := range oldPools {
		p.victim = nil
		p.victimSize = 0
	}

	// Move primary cache to victim cache.
	for _, p := range allPools {
		p.victim = p.local
		p.victimSize = p.localSize
		p.local = nil
		p.localSize = 0
	}

	// The pools with non-empty primary caches now have non-empty
	// victim caches and no pools have primary caches.
	oldPools, allPools = allPools, nil
}

var (
	allPoolsMu Mutex

	// allPools is the set of pools that have non-empty primary
	// caches. Protected by either 1) allPoolsMu and pinning or 2)
	// STW.
	allPools []*Pool

	// oldPools is the set of pools that may have non-empty victim
	// caches. Protected by STW.
	oldPools []*Pool
)

func init() {
	runtime_registerPoolCleanup(poolCleanup)
}

func indexLocal(l unsafe.Pointer, i int) *poolLocal {
	lp := unsafe.Pointer(uintptr(l) + uintptr(i)*unsafe.Sizeof(poolLocal{}))
	return (*poolLocal)(lp)
}

// Implemented in runtime.
func runtime_registerPoolCleanup(cleanup func())
func runtime_procPin() int
func runtime_procUnpin()

// The below are implemented in runtime/internal/atomic and the
// compiler also knows to intrinsify the symbol we linkname into this
// package.

//go:linkname runtime_LoadAcquintptr runtime/internal/atomic.LoadAcquintptr
func runtime_LoadAcquintptr(ptr *uintptr) uintptr

//go:linkname runtime_StoreReluintptr runtime/internal/atomic.StoreReluintptr
func runtime_StoreReluintptr(ptr *uintptr, val uintptr) uintptr
