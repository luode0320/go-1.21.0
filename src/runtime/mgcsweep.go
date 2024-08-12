// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Garbage collector: sweeping

// The sweeper consists of two different algorithms:
//
// * The object reclaimer finds and frees unmarked slots in spans. It
//   can free a whole span if none of the objects are marked, but that
//   isn't its goal. This can be driven either synchronously by
//   mcentral.cacheSpan for mcentral spans, or asynchronously by
//   sweepone, which looks at all the mcentral lists.
//
// * The span reclaimer looks for spans that contain no marked objects
//   and frees whole spans. This is a separate algorithm because
//   freeing whole spans is the hardest task for the object reclaimer,
//   but is critical when allocating new spans. The entry point for
//   this is mheap_.reclaim and it's driven by a sequential scan of
//   the page marks bitmap in the heap arenas.
//
// Both algorithms ultimately call mspan.sweep, which sweeps a single
// heap span.

package runtime

import (
	"runtime/internal/atomic"
	"unsafe"
)

var sweep sweepdata

// State of background sweep.
type sweepdata struct {
	lock   mutex
	g      *g
	parked bool

	nbgsweep    uint32
	npausesweep uint32

	// active tracks outstanding sweepers and the sweep
	// termination condition.
	active activeSweep

	// centralIndex is the current unswept span class.
	// It represents an index into the mcentral span
	// sets. Accessed and updated via its load and
	// update methods. Not protected by a lock.
	//
	// Reset at mark termination.
	// Used by mheap.nextSpanForSweep.
	centralIndex sweepClass
}

// sweepClass is a spanClass and one bit to represent whether we're currently
// sweeping partial or full spans.
type sweepClass uint32

const (
	numSweepClasses            = numSpanClasses * 2
	sweepClassDone  sweepClass = sweepClass(^uint32(0))
)

func (s *sweepClass) load() sweepClass {
	return sweepClass(atomic.Load((*uint32)(s)))
}

func (s *sweepClass) update(sNew sweepClass) {
	// Only update *s if its current value is less than sNew,
	// since *s increases monotonically.
	sOld := s.load()
	for sOld < sNew && !atomic.Cas((*uint32)(s), uint32(sOld), uint32(sNew)) {
		sOld = s.load()
	}
	// TODO(mknyszek): This isn't the only place we have
	// an atomic monotonically increasing counter. It would
	// be nice to have an "atomic max" which is just implemented
	// as the above on most architectures. Some architectures
	// like RISC-V however have native support for an atomic max.
}

func (s *sweepClass) clear() {
	atomic.Store((*uint32)(s), 0)
}

// split returns the underlying span class as well as
// whether we're interested in the full or partial
// unswept lists for that class, indicated as a boolean
// (true means "full").
func (s sweepClass) split() (spc spanClass, full bool) {
	return spanClass(s >> 1), s&1 == 0
}

// nextSpanForSweep finds and pops the next span for sweeping from the
// central sweep buffers. It returns ownership of the span to the caller.
// Returns nil if no such span exists.
func (h *mheap) nextSpanForSweep() *mspan {
	sg := h.sweepgen
	for sc := sweep.centralIndex.load(); sc < numSweepClasses; sc++ {
		spc, full := sc.split()
		c := &h.central[spc].mcentral
		var s *mspan
		if full {
			s = c.fullUnswept(sg).pop()
		} else {
			s = c.partialUnswept(sg).pop()
		}
		if s != nil {
			// Write down that we found something so future sweepers
			// can start from here.
			sweep.centralIndex.update(sc)
			return s
		}
	}
	// Write down that we found nothing.
	sweep.centralIndex.update(sweepClassDone)
	return nil
}

const sweepDrainedMask = 1 << 31

// 是一个类型，用于捕获清扫是否完成，以及是否有任何正在进行的清扫器。
//
// 每个潜在的清扫器在查找清扫工作之前必须调用 begin()，
// 并在完成清扫后调用 end()。
type activeSweep struct {
	// state 被分为两部分。
	//
	// 最高位（由 sweepDrainedMask 掩码屏蔽）是一个布尔值，表示清扫队列中的所有清扫工作是否已被清空。
	// 其余的位是一个计数器，表示正在进行的并发清扫器的数量。
	state atomic.Uint32
}

// begin registers a new sweeper. Returns a sweepLocker
// for acquiring spans for sweeping. Any outstanding sweeper blocks
// sweep termination.
//
// If the sweepLocker is invalid, the caller can be sure that all
// outstanding sweep work has been drained, so there is nothing left
// to sweep. Note that there may be sweepers currently running, so
// this does not indicate that all sweeping has completed.
//
// Even if the sweepLocker is invalid, its sweepGen is always valid.
func (a *activeSweep) begin() sweepLocker {
	for {
		state := a.state.Load()
		if state&sweepDrainedMask != 0 {
			return sweepLocker{mheap_.sweepgen, false}
		}
		if a.state.CompareAndSwap(state, state+1) {
			return sweepLocker{mheap_.sweepgen, true}
		}
	}
}

// end deregisters a sweeper. Must be called once for each time
// begin is called if the sweepLocker is valid.
func (a *activeSweep) end(sl sweepLocker) {
	if sl.sweepGen != mheap_.sweepgen {
		throw("sweeper left outstanding across sweep generations")
	}
	for {
		state := a.state.Load()
		if (state&^sweepDrainedMask)-1 >= sweepDrainedMask {
			throw("mismatched begin/end of activeSweep")
		}
		if a.state.CompareAndSwap(state, state-1) {
			if state != sweepDrainedMask {
				return
			}
			if debug.gcpacertrace > 0 {
				live := gcController.heapLive.Load()
				print("pacer: sweep done at heap size ", live>>20, "MB; allocated ", (live-mheap_.sweepHeapLiveBasis)>>20, "MB during sweep; swept ", mheap_.pagesSwept.Load(), " pages at ", mheap_.sweepPagesPerByte, " pages/byte\n")
			}
			return
		}
	}
}

// markDrained marks the active sweep cycle as having drained
// all remaining work. This is safe to be called concurrently
// with all other methods of activeSweep, though may race.
//
// Returns true if this call was the one that actually performed
// the mark.
func (a *activeSweep) markDrained() bool {
	for {
		state := a.state.Load()
		if state&sweepDrainedMask != 0 {
			return false
		}
		if a.state.CompareAndSwap(state, state|sweepDrainedMask) {
			return true
		}
	}
}

// sweepers returns the current number of active sweepers.
func (a *activeSweep) sweepers() uint32 {
	return a.state.Load() &^ sweepDrainedMask
}

// isDone returns true if all sweep work has been drained and no more
// outstanding sweepers exist. That is, when the sweep phase is
// completely done.
func (a *activeSweep) isDone() bool {
	return a.state.Load() == sweepDrainedMask
}

// reset sets up the activeSweep for the next sweep cycle.
//
// The world must be stopped.
func (a *activeSweep) reset() {
	assertWorldStopped()
	a.state.Store(0)
}

// 函数确保所有 span 已经完成清扫。
//
// 世界必须被停止。这确保了没有正在进行的清扫操作。
//
//go:nowritebarrier
func finishsweep_m() {
	// 断言世界已被停止
	assertWorldStopped()

	// 使用循环和 sweepone() 函数清扫任何未清扫的 span
	for sweepone() != ^uintptr(0) { // 每次调用 sweepone() 清扫一个 span，直到没有更多 span 可清扫为止
		sweep.npausesweep++ // 记录清扫暂停次数
	}

	// 确保没有任何未完成的清扫 goroutine 存在。
	// 此时，由于世界被停止，这意味着两种情况之一：
	// 1. 我们能够抢占清扫 goroutine；
	// 2. 清扫 goroutine 没有在其应该结束时调用 sweep.active.end。
	// 这两种情况都表明存在错误，因此抛出异常。
	if sweep.active.sweepers() != 0 {
		throw("active sweepers found at start of mark phase")
	}

	// 重置所有未清扫缓冲区，这些缓冲区应该是空的。
	// 在清扫终止时而非标记终止时执行此操作，以便尽快捕捉未清扫的 span 并回收 block。
	sg := mheap_.sweepgen

	// 遍历所有 central，并重置其中的未清扫缓冲区
	// 这些缓冲区应该是空的，因为所有 span 应该已经被清扫
	for i := range mheap_.central {
		c := &mheap_.central[i].mcentral
		c.partialUnswept(sg).reset() // 重置部分未清扫缓冲区
		c.fullUnswept(sg).reset()    // 重置完全未清扫缓冲区
	}

	// 清扫已完成，因此一段时间内不会有新的内存需要回收。
	//
	// 如果 scavenger 当前没有处于活动状态，则唤醒 scavenger。此时肯定有工作需要 scavenger 处理。
	scavenger.wake()

	// 进入下一个标记位 arena 时期。
	nextMarkBitArenaEpoch()
}

// 函数负责后台清扫 goroutine 的主体。负责清扫（sweep）垃圾回收器标记为不可达的对象所在的 span（内存块）
// 它会清扫 span，并在必要时让出时间。
// 通过这些步骤，这段代码确保了清扫 goroutine 能够有效地清扫 span，并在必要时让出时间，以避免过度占用 CPU 时间。
func bgsweep(c chan int) {
	sweep.g = getg() // 设置清扫 goroutine

	lockInit(&sweep.lock, lockRankSweep)                                   // 初始化清扫锁。
	lock(&sweep.lock)                                                      // 获取清扫锁。
	sweep.parked = true                                                    // 设置清扫状态为停放。
	c <- 1                                                                 // 通过通道发送信号，表示清扫 goroutine 已经启动。
	goparkunlock(&sweep.lock, waitReasonGCSweepWait, traceBlockGCSweep, 1) // 将清扫 goroutine 设置为等待状态。

	for {
		// 尝试作为一个“低优先级”的 goroutine，通过故意让出时间来实现。
		// 如果它没有运行也没关系，因为分配内存的 goroutine 会进行清扫，并确保所有 span 在下一次 GC 周期之前被清扫。
		// 我们只希望在空闲时运行它。
		//
		// 然而，在每次清扫 span 后调用 Gosched 会产生大量的跟踪事件，有时会占到跟踪事件的 50%。
		// 它还因为清扫单个 span 通常是一个非常快速的操作，通常只需要 30ns，在现代硬件上。
		// （见 #54767。）
		//
		// 结果，bgsweep 以批次方式清扫，并且只在每个批次结束时调用调度器。
		// 此外，它只有在其他核心上没有可用的空闲时间时才会让出时间。
		// 如果有可用的空闲时间，帮助清扫可以降低分配延迟，通过领先于按比例清扫，并使 span 为分配做好准备。

		const sweepBatchSize = 10 // 批次清扫的大小。
		nSwept := 0               // 清扫的 span 数量。
		for sweepone() != ^uintptr(0) {
			sweep.nbgsweep++ // 记录清扫次数。
			nSwept++         // 增加清扫的 span 数量。
			if nSwept%sweepBatchSize == 0 {
				goschedIfBusy() // 如果忙碌，则让出时间。
			}
		}
		for freeSomeWbufs(true) {
			// 注意：freeSomeWbufs 内部已经批量处理。
			goschedIfBusy() // 如果忙碌，则让出时间。
		}

		lock(&sweep.lock) // 获取清扫锁。

		// 函数检查清扫是否完成

		// 如果清扫未完成，则继续清扫
		if !isSweepDone() {
			unlock(&sweep.lock) // 释放清扫锁。
			continue            // 继续下一轮清扫。
		}

		// 如果清扫完成，则设置清扫状态为停放，并将清扫 goroutine 设置为等待状态
		sweep.parked = true                                                    // 设置清扫状态为停放。
		goparkunlock(&sweep.lock, waitReasonGCSweepWait, traceBlockGCSweep, 1) // 将清扫 goroutine 设置为等待状态。
	}
}

// sweepLocker acquires sweep ownership of spans.
type sweepLocker struct {
	// sweepGen is the sweep generation of the heap.
	sweepGen uint32
	valid    bool
}

// sweepLocked represents sweep ownership of a span.
type sweepLocked struct {
	*mspan
}

// tryAcquire attempts to acquire sweep ownership of span s. If it
// successfully acquires ownership, it blocks sweep completion.
func (l *sweepLocker) tryAcquire(s *mspan) (sweepLocked, bool) {
	if !l.valid {
		throw("use of invalid sweepLocker")
	}
	// Check before attempting to CAS.
	if atomic.Load(&s.sweepgen) != l.sweepGen-2 {
		return sweepLocked{}, false
	}
	// Attempt to acquire sweep ownership of s.
	if !atomic.Cas(&s.sweepgen, l.sweepGen-2, l.sweepGen-1) {
		return sweepLocked{}, false
	}
	return sweepLocked{s}, true
}

// 清扫某个未清扫的堆空间，并返回释放的页数，
// 如果没有可清扫的空间，则返回 ^uintptr(0)。
// 垃圾回收器会遍历所有的 span，并回收那些在标记阶段未被标记的对象所占用的空间
func sweepone() uintptr {
	// 获取当前 goroutine。
	gp := getg()

	// 增加锁计数以确保协程在清扫过程中不会被抢占，
	// 避免留下不一致的状态供下次垃圾回收使用。
	gp.m.locks++

	// 开始清扫操作，获取清扫锁。
	sl := sweep.active.begin()
	if !sl.valid {
		// 如果没有有效的清扫工作，释放锁计数并返回。
		gp.m.locks--
		return ^uintptr(0)
	}

	// 查找一个要清扫的空间。
	npages := ^uintptr(0)
	var noMoreWork bool // 标记清扫列表是否为空。
	for {
		// 获取下一个要清扫的空间。
		s := mheap_.nextSpanForSweep()
		if s == nil {
			// 如果找不到可清扫的空间，则标记清扫列表为空。
			noMoreWork = sweep.active.markDrained()
			break
		}

		// 如果空间的状态不是 mSpanInUse，跳过该空间。
		// 注意：如果该空间已被直接清扫，则其清扫代号应与当前清扫代号一致或 +3。
		if state := s.state.get(); state != mSpanInUse {
			if !(s.sweepgen == sl.sweepGen || s.sweepgen == sl.sweepGen+3) {
				print("runtime: bad span s.state=", state, " s.sweepgen=", s.sweepgen, " sweepgen=", sl.sweepGen, "\n")
				throw("non in-use span in unswept list")
			}
			continue
		}

		if s, ok := sl.tryAcquire(s); ok {
			// 找到并获取要清扫的空间。
			npages = s.npages
			if s.sweep(false) { // 释放或收集未在标记阶段标记的块的终结器。它清除标记位，为下一轮 GC 做准备
				// 整个空间被释放。将其计入页面回收信用，因为这些页面现在可用于空间分配。
				mheap_.reclaimCredit.Add(npages)
			} else {
				// 空间仍在使用中，因此没有返回任何页面给堆，且空间需要移动到已清扫的使用中列表。
				npages = 0
			}
			break
		}
	}

	// 结束清扫操作。
	sweep.active.end(sl)

	// 清扫列表为空。可能存在并发清扫，但我们至少非常接近完成清扫。
	if noMoreWork {
		// 移动回收代号向前（指示有新的工作要做）并唤醒回收器。
		//
		// 回收器由最后一个清扫者唤醒，因为一旦清扫完成，
		// 我们肯定会有对回收器有用的工作，因为回收器仅在每个垃圾回收周期运行一次。
		// 这个更新不在清扫终止时进行，因为在某些情况下清扫完成和清扫终止之间可能会有很长的延迟，
		// 如不足以触发垃圾回收的分配，这期间填充回收工作会很好。
		if debug.scavtrace > 0 {
			systemstack(func() {
				// 获取堆锁。
				lock(&mheap_.lock)

				// 获取释放统计信息。
				releasedBg := mheap_.pages.scav.releasedBg.Load()
				releasedEager := mheap_.pages.scav.releasedEager.Load()

				// 打印统计信息。
				printScavTrace(releasedBg, releasedEager, false)

				// 更新统计信息。
				mheap_.pages.scav.releasedBg.Add(-releasedBg)
				mheap_.pages.scav.releasedEager.Add(-releasedEager)

				// 释放堆锁。
				unlock(&mheap_.lock)
			})
		}

		scavenger.ready() // 唤醒 scavenger goroutine。
	}

	// 释放锁计数。
	gp.m.locks--
	return npages // 返回释放的页数。
}

// isSweepDone reports whether all spans are swept.
//
// Note that this condition may transition from false to true at any
// time as the sweeper runs. It may transition from true to false if a
// GC runs; to prevent that the caller must be non-preemptible or must
// somehow block GC progress.
func isSweepDone() bool {
	return sweep.active.isDone()
}

// Returns only when span s has been swept.
//
//go:nowritebarrier
func (s *mspan) ensureSwept() {
	// Caller must disable preemption.
	// Otherwise when this function returns the span can become unswept again
	// (if GC is triggered on another goroutine).
	gp := getg()
	if gp.m.locks == 0 && gp.m.mallocing == 0 && gp != gp.m.g0 {
		throw("mspan.ensureSwept: m is not locked")
	}

	// If this operation fails, then that means that there are
	// no more spans to be swept. In this case, either s has already
	// been swept, or is about to be acquired for sweeping and swept.
	sl := sweep.active.begin()
	if sl.valid {
		// The caller must be sure that the span is a mSpanInUse span.
		if s, ok := sl.tryAcquire(s); ok {
			s.sweep(false)
			sweep.active.end(sl)
			return
		}
		sweep.active.end(sl)
	}

	// Unfortunately we can't sweep the span ourselves. Somebody else
	// got to it first. We don't have efficient means to wait, but that's
	// OK, it will be swept fairly soon.
	for {
		spangen := atomic.Load(&s.sweepgen)
		if spangen == sl.sweepGen || spangen == sl.sweepGen+3 {
			break
		}
		osyield()
	}
}

// sweep 释放或收集未在标记阶段标记的块的终结器。
// 它清除标记位，为下一次垃圾回收周期做准备。
// 如果 span 被归还到堆，则返回 true。
// 如果 preserve 为 true，则不将其归还到堆也不重新链接到 mcentral 列表中；
// 调用方会负责后续处理。
func (sl *sweepLocked) sweep(preserve bool) bool {
	// 关键：我们必须在这个函数开始时禁用抢占，
	// 垃圾回收不得在我们处理此函数的过程中启动。

	gp := getg()
	// 确保当前 goroutine 已经禁用了抢占。
	if gp.m.locks == 0 && gp.m.mallocing == 0 && gp != gp.m.g0 {
		throw("mspan.sweep: m is not locked")
	}

	s := sl.mspan
	if !preserve {
		// 我们将放弃对此 span 的所有权。将其设为 nil 以防调用方意外使用。
		sl.mspan = nil
	}

	sweepgen := mheap_.sweepgen
	// 检查 span 的状态是否为 mSpanInUse 并且 sweep 代号是否正确
	if state := s.state.get(); state != mSpanInUse || s.sweepgen != sweepgen-1 {
		print("mspan.sweep: state=", state, " sweepgen=", s.sweepgen, " mheap.sweepgen=", sweepgen, "\n")
		throw("mspan.sweep: bad span state")
	}

	// 如果启用了追踪，则记录清扫 span 的统计信息
	if traceEnabled() {
		traceGCSweepSpan(s.npages * _PageSize)
	}

	mheap_.pagesSwept.Add(int64(s.npages))

	spc := s.spanclass
	size := s.elemsize

	// allocBits 指示哪些未标记的对象不需要处理，
	// 因为它们在上次 GC 周期结束时就是空闲的，并且此后没有被分配。
	// 如果 allocBits 索引 >= s.freeindex 且位未被标记，则对象自上次 GC 以来一直是未分配的。
	// 这种情况类似于对象在空闲列表上。

	// 解除链接并释放任何我们即将释放的对象的特殊记录。
	// 存在两个复杂性：
	// 1. 一个对象可以同时有终结器和剖析特殊记录。
	//    在这种情况下，我们需要排队执行终结器，
	//    标记对象为存活，并保留剖析特殊记录。
	// 2. 一个微小对象可以为不同偏移量设置多个终结器。
	//    如果此类对象未被标记，我们需要同时排队所有终结器。
	// 两种情况都可以同时发生。
	hadSpecials := s.specials != nil
	siter := newSpecialsIter(s)
	for siter.valid() {
		// 一个终结器可以为对象内部的某个字节设置，找到对象的起始位置。
		objIndex := uintptr(siter.s.offset) / size
		p := s.base() + objIndex*size
		mbits := s.markBitsForIndex(objIndex)

		// 此对象未被标记且至少有一个特殊记录。
		if !mbits.isMarked() {
			// 第一遍：查看它是否至少有一个终结器。
			hasFin := false
			endOffset := p - s.base() + size
			for tmp := siter.s; tmp != nil && uintptr(tmp.offset) < endOffset; tmp = tmp.next {
				if tmp.kind == _KindSpecialFinalizer {
					// 如果对象有终结器，则停止释放。
					mbits.setMarkedNonAtomic()
					hasFin = true
					break
				}
			}
			// 第二遍：排队所有终结器 _或_ 处理剖析记录。
			for siter.valid() && uintptr(siter.s.offset) < endOffset {
				// Find the exact byte for which the special was setup
				// (as opposed to object beginning).
				special := siter.s
				p := s.base() + uintptr(special.offset)
				if special.kind == _KindSpecialFinalizer || !hasFin {
					siter.unlinkAndNext()
					freeSpecial(special, unsafe.Pointer(p), size)
				} else {
					// 对象有终结器，因此我们保留它存活。
					// 其他所有特殊记录仅在对象释放时适用，
					// 因此只需保留特殊记录。
					siter.next()
				}
			}
		} else {
			// 如果对象被标记且有可达性特殊记录，则处理可达性记录
			if siter.s.kind == _KindSpecialReachable {
				special := siter.unlinkAndNext()
				(*specialReachable)(unsafe.Pointer(special)).reachable = true
				freeSpecial(special, unsafe.Pointer(p), size)
			} else {
				// 如果对象被标记且没有可达性特殊记录，则保留特殊记录
				siter.next()
			}
		}
	}

	// 如果 span 之前有特殊记录，现在没有了，则调用 spanHasNoSpecials 函数
	if hadSpecials && s.specials == nil {
		spanHasNoSpecials(s)
	}

	// 继续清扫 span 的剩余部分。
	if debug.allocfreetrace != 0 || debug.clobberfree != 0 || raceenabled || msanenabled || asanenabled {
		// 查找所有新释放的对象。这不需要高效；allocfreetrace 有巨大的开销。
		mbits := s.markBitsForBase()
		abits := s.allocBitsForIndex(0)
		for i := uintptr(0); i < s.nelems; i++ {
			if !mbits.isMarked() && (abits.index < s.freeindex || abits.isMarked()) {
				x := s.base() + i*s.elemsize
				if debug.allocfreetrace != 0 {
					tracefree(unsafe.Pointer(x), size)
				}
				if debug.clobberfree != 0 {
					clobberfree(unsafe.Pointer(x), size)
				}
				// 用户 arena 在显式释放时处理。
				if raceenabled && !s.isUserArenaChunk {
					racefree(unsafe.Pointer(x), size)
				}
				if msanenabled && !s.isUserArenaChunk {
					msanfree(unsafe.Pointer(x), size)
				}
				if asanenabled && !s.isUserArenaChunk {
					asanpoison(unsafe.Pointer(x), size)
				}
			}
			mbits.advance()
			abits.advance()
		}
	}

	// 检查僵尸对象。
	if s.freeindex < s.nelems {
		// 所有 < freeindex 的都是已分配的，因此不可能是僵尸。
		//
		// 检查第一个位图字节，这里需要小心处理 freeindex。
		obj := s.freeindex
		if (*s.gcmarkBits.bytep(obj / 8)&^*s.allocBits.bytep(obj / 8))>>(obj%8) != 0 {
			s.reportZombies()
		}
		// 检查剩余的字节。
		for i := obj/8 + 1; i < divRoundUp(s.nelems, 8); i++ {
			if *s.gcmarkBits.bytep(i)&^*s.allocBits.bytep(i) != 0 {
				s.reportZombies()
			}
		}
	}

	// 计算 span 中空闲对象的数量。
	nalloc := uint16(s.countAlloc())
	nfreed := s.allocCount - nalloc
	if nalloc > s.allocCount {
		// 僵尸检查应该更详细地捕获这种情况。
		print("runtime: nelems=", s.nelems, " nalloc=", nalloc, " previous allocCount=", s.allocCount, " nfreed=", nfreed, "\n")
		throw("sweep increased allocation count")
	}

	s.allocCount = nalloc
	s.freeindex = 0 // 重置分配索引到 span 的起始位置。
	s.freeIndexForScan = 0
	if traceEnabled() {
		getg().m.p.ptr().trace.reclaimed += uintptr(nfreed) * s.elemsize
	}

	// gcmarkBits 成为 allocBits。
	// 为下次 GC 准备一个全新的清空的 gcmarkBits。
	s.allocBits = s.gcmarkBits
	s.gcmarkBits = newMarkBits(s.nelems)

	// 如果存在，则刷新 pinnerBits。
	if s.pinnerBits != nil {
		s.refreshPinnerBits()
	}

	// 初始化 alloc 缓存。
	s.refillAllocCache(0)

	// 检查 span 的状态，确保没有潜在的竞态条件。
	if state := s.state.get(); state != mSpanInUse || s.sweepgen != sweepgen-1 {
		print("mspan.sweep: state=", state, " sweepgen=", s.sweepgen, " mheap.sweepgen=", sweepgen, "\n")
		throw("mspan.sweep: bad span state after sweep")
	}
	if s.sweepgen == sweepgen+1 || s.sweepgen == sweepgen+3 {
		throw("swept cached span")
	}

	// 设置 s.sweepgen = h.sweepgen 只有在所有块都被清扫后才能设置，
	// 因为可能存在并发 free/SetFinalizer 的情况。
	//
	// 但在我们使 span 可用于分配（将其归还到堆或 mcentral）之前必须设置它，
	// 因为分配代码假设如果 span 可用于分配，则已经清扫过。
	//
	// 序列化点。
	// 在这一点上，标记位已被清空，分配已准备好，
	// 所以释放 span。
	atomic.Store(&s.sweepgen, sweepgen)

	// 处理用户 arena。
	if s.isUserArenaChunk {
		if preserve {
			// 这是一种不应该由保留 span 以供重用的清扫器处理的情况。
			throw("sweep: tried to preserve a user arena span")
		}
		if nalloc > 0 {
			// 仍然存在指向 span 的指针或 span 尚未被释放。
			// 它还没有准备好重用。将其放回全清扫列表以供下一次周期使用。
			mheap_.central[spc].mcentral.fullSwept(sweepgen).push(s)
			return false
		}

		// 只有在此时清扫器实际上不再需要查看此 arena，
		// 所以现在从 pagesInUse 中减去。
		mheap_.pagesInUse.Add(-s.npages)
		s.state.set(mSpanDead)

		// arena 已准备好回收。从隔离列表中删除并放置在准备列表中。
		// 不再添加到任何清扫列表。
		systemstack(func() {
			// 到所有指向 chunk 的引用消失时，arena 代码有责任将 chunk 放在隔离列表上。
			if s.list != &mheap_.userArena.quarantineList {
				throw("user arena span is on the wrong list")
			}
			lock(&mheap_.lock)
			mheap_.userArena.quarantineList.remove(s)
			mheap_.userArena.readyList.insert(s)
			unlock(&mheap_.lock)
		})
		return false
	}

	// 处理小对象的 span。
	if spc.sizeclass() != 0 {
		if nfreed > 0 {
			// 只有当我们释放了一些对象时才标记 span 需要清零，
			// 因为新的 span 如果没有完全填充就被清扫，则所有空闲槽已经被清零。
			s.needzero = 1
			stats := memstats.heapStats.acquire()
			atomic.Xadd64(&stats.smallFreeCount[spc.sizeclass()], int64(nfreed))
			memstats.heapStats.release()

			// 在不一致的内部统计中计算释放的数量。
			gcController.totalFree.Add(int64(nfreed) * int64(s.elemsize))
		}
		if !preserve {
			// 直接将完全空闲的 span 归还到堆。
			if nalloc == 0 {
				// Free totally free span directly back to the heap.
				mheap_.freeSpan(s)
				return true
			}
			// 将 span 归还到正确的 mcentral 列表。
			if uintptr(nalloc) == s.nelems {
				mheap_.central[spc].mcentral.fullSwept(sweepgen).push(s)
			} else {
				mheap_.central[spc].mcentral.partialSwept(sweepgen).push(s)
			}
		}
	} else if !preserve {
		// 处理大对象的 span。
		if nfreed != 0 {
			// 将大对象 span 归还到堆。

			// 在一致的外部统计中计算释放的数量。
			//
			// 在 freeSpan 之前计算，因为它可能会更新 heapStats 的 inHeap 值。
			// 如果它这样做，那么从对象足迹中减去 inHeap 的指标可能会溢出。参见 #67019。
			stats := memstats.heapStats.acquire()
			atomic.Xadd64(&stats.largeFreeCount, 1)
			atomic.Xadd64(&stats.largeFree, int64(size))
			memstats.heapStats.release()

			// 在不一致的内部统计中计算释放的数量。
			gcController.totalFree.Add(int64(size))

			// 如果启用 efence，则使用 sysFault 代替 sysFree。
			if debug.efence > 0 {
				s.limit = 0 // 防止 mlookup 找到这个 span
				sysFault(unsafe.Pointer(s.base()), size)
			} else {
				mheap_.freeSpan(s)
			}
			return true
		}

		// 将大 span 直接放在全清扫列表上。
		mheap_.central[spc].mcentral.fullSwept(sweepgen).push(s)
	}
	return false
}

// reportZombies reports any marked but free objects in s and throws.
//
// This generally means one of the following:
//
// 1. User code converted a pointer to a uintptr and then back
// unsafely, and a GC ran while the uintptr was the only reference to
// an object.
//
// 2. User code (or a compiler bug) constructed a bad pointer that
// points to a free slot, often a past-the-end pointer.
//
// 3. The GC two cycles ago missed a pointer and freed a live object,
// but it was still live in the last cycle, so this GC cycle found a
// pointer to that object and marked it.
func (s *mspan) reportZombies() {
	printlock()
	print("runtime: marked free object in span ", s, ", elemsize=", s.elemsize, " freeindex=", s.freeindex, " (bad use of unsafe.Pointer? try -d=checkptr)\n")
	mbits := s.markBitsForBase()
	abits := s.allocBitsForIndex(0)
	for i := uintptr(0); i < s.nelems; i++ {
		addr := s.base() + i*s.elemsize
		print(hex(addr))
		alloc := i < s.freeindex || abits.isMarked()
		if alloc {
			print(" alloc")
		} else {
			print(" free ")
		}
		if mbits.isMarked() {
			print(" marked  ")
		} else {
			print(" unmarked")
		}
		zombie := mbits.isMarked() && !alloc
		if zombie {
			print(" zombie")
		}
		print("\n")
		if zombie {
			length := s.elemsize
			if length > 1024 {
				length = 1024
			}
			hexdumpWords(addr, addr+length, nil)
		}
		mbits.advance()
		abits.advance()
	}
	throw("found pointer to free object")
}

// deductSweepCredit deducts sweep credit for allocating a span of
// size spanBytes. This must be performed *before* the span is
// allocated to ensure the system has enough credit. If necessary, it
// performs sweeping to prevent going in to debt. If the caller will
// also sweep pages (e.g., for a large allocation), it can pass a
// non-zero callerSweepPages to leave that many pages unswept.
//
// deductSweepCredit makes a worst-case assumption that all spanBytes
// bytes of the ultimately allocated span will be available for object
// allocation.
//
// deductSweepCredit is the core of the "proportional sweep" system.
// It uses statistics gathered by the garbage collector to perform
// enough sweeping so that all pages are swept during the concurrent
// sweep phase between GC cycles.
//
// mheap_ must NOT be locked.
func deductSweepCredit(spanBytes uintptr, callerSweepPages uintptr) {
	if mheap_.sweepPagesPerByte == 0 {
		// Proportional sweep is done or disabled.
		return
	}

	if traceEnabled() {
		traceGCSweepStart()
	}

	// Fix debt if necessary.
retry:
	sweptBasis := mheap_.pagesSweptBasis.Load()
	live := gcController.heapLive.Load()
	liveBasis := mheap_.sweepHeapLiveBasis
	newHeapLive := spanBytes
	if liveBasis < live {
		// Only do this subtraction when we don't overflow. Otherwise, pagesTarget
		// might be computed as something really huge, causing us to get stuck
		// sweeping here until the next mark phase.
		//
		// Overflow can happen here if gcPaceSweeper is called concurrently with
		// sweeping (i.e. not during a STW, like it usually is) because this code
		// is intentionally racy. A concurrent call to gcPaceSweeper can happen
		// if a GC tuning parameter is modified and we read an older value of
		// heapLive than what was used to set the basis.
		//
		// This state should be transient, so it's fine to just let newHeapLive
		// be a relatively small number. We'll probably just skip this attempt to
		// sweep.
		//
		// See issue #57523.
		newHeapLive += uintptr(live - liveBasis)
	}
	pagesTarget := int64(mheap_.sweepPagesPerByte*float64(newHeapLive)) - int64(callerSweepPages)
	for pagesTarget > int64(mheap_.pagesSwept.Load()-sweptBasis) {
		if sweepone() == ^uintptr(0) {
			mheap_.sweepPagesPerByte = 0
			break
		}
		if mheap_.pagesSweptBasis.Load() != sweptBasis {
			// Sweep pacing changed. Recompute debt.
			goto retry
		}
	}

	if traceEnabled() {
		traceGCSweepDone()
	}
}

// clobberfree sets the memory content at x to bad content, for debugging
// purposes.
func clobberfree(x unsafe.Pointer, size uintptr) {
	// size (span.elemsize) is always a multiple of 4.
	for i := uintptr(0); i < size; i += 4 {
		*(*uint32)(add(x, i)) = 0xdeadbeef
	}
}

// gcPaceSweeper updates the sweeper's pacing parameters.
//
// Must be called whenever the GC's pacing is updated.
//
// The world must be stopped, or mheap_.lock must be held.
func gcPaceSweeper(trigger uint64) {
	assertWorldStoppedOrLockHeld(&mheap_.lock)

	// Update sweep pacing.
	if isSweepDone() {
		mheap_.sweepPagesPerByte = 0
	} else {
		// Concurrent sweep needs to sweep all of the in-use
		// pages by the time the allocated heap reaches the GC
		// trigger. Compute the ratio of in-use pages to sweep
		// per byte allocated, accounting for the fact that
		// some might already be swept.
		heapLiveBasis := gcController.heapLive.Load()
		heapDistance := int64(trigger) - int64(heapLiveBasis)
		// Add a little margin so rounding errors and
		// concurrent sweep are less likely to leave pages
		// unswept when GC starts.
		heapDistance -= 1024 * 1024
		if heapDistance < _PageSize {
			// Avoid setting the sweep ratio extremely high
			heapDistance = _PageSize
		}
		pagesSwept := mheap_.pagesSwept.Load()
		pagesInUse := mheap_.pagesInUse.Load()
		sweepDistancePages := int64(pagesInUse) - int64(pagesSwept)
		if sweepDistancePages <= 0 {
			mheap_.sweepPagesPerByte = 0
		} else {
			mheap_.sweepPagesPerByte = float64(sweepDistancePages) / float64(heapDistance)
			mheap_.sweepHeapLiveBasis = heapLiveBasis
			// Write pagesSweptBasis last, since this
			// signals concurrent sweeps to recompute
			// their debt.
			mheap_.pagesSweptBasis.Store(pagesSwept)
		}
	}
}
