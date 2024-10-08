// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"runtime/internal/atomic"
	"runtime/internal/sys"
	"unsafe"
)

// 每线程（在 Go 中，每 P）的小对象缓存。
// 这包括一个小对象缓存和本地分配统计信息。
// 无需锁定，因为它是一个线程局部的缓存。
//
// mcaches 从非 GC 内存分配，因此任何堆指针都需要特别处理。
type mcache struct {
	_ sys.NotInHeap

	// 以下成员在每次分配时都会被访问，
	// 因此它们被放在一起以提高缓存性能。

	nextSample uintptr // 分配这么多字节后触发堆采样,下一次触发堆采样的阈值
	scanAlloc  uintptr // 分配的可扫描堆字节数

	// 用于无指针的小对象的分配器缓存。
	// 请参阅 malloc.go 中的“Tiny 分配器”注释。

	tiny       uintptr // 指向当前 tiny 块的开头，如果没有当前 tiny 块则为 nil。
	tinyoffset uintptr //  当前 tiny 块中已分配的偏移量。
	tinyAllocs uintptr // 是拥有这个 mcache 的 P 执行的 tiny 分配次数。

	// 以下成员不在每次分配时访问。

	alloc      [numSpanClasses]*mspan         // 用于存储不同大小类别的 span，方便快速分配
	stackcache [_NumStackOrders]stackfreelist // 栈缓存，用于管理栈分配的空闲列表

	// 表示此 mcache 最后一次刷新时的 sweepgen。
	// 如果 flushGen != mheap_.sweepgen，则表示此 mcache 中的 span 已过时，
	// 需要刷新以便清扫。这在 acquirep 中完成。
	flushGen atomic.Uint32
}

// A gclink is a node in a linked list of blocks, like mlink,
// but it is opaque to the garbage collector.
// The GC does not trace the pointers during collection,
// and the compiler does not emit write barriers for assignments
// of gclinkptr values. Code should store references to gclinks
// as gclinkptr, not as *gclink.
type gclink struct {
	next gclinkptr
}

// A gclinkptr is a pointer to a gclink, but it is opaque
// to the garbage collector.
type gclinkptr uintptr

// ptr returns the *gclink form of p.
// The result should be used for accessing fields, not stored
// in other data structures.
func (p gclinkptr) ptr() *gclink {
	return (*gclink)(unsafe.Pointer(p))
}

type stackfreelist struct {
	list gclinkptr // linked list of free stacks
	size uintptr   // total size of stacks in list
}

// dummy mspan that contains no free objects.
var emptymspan mspan

func allocmcache() *mcache {
	var c *mcache
	systemstack(func() {
		lock(&mheap_.lock)
		c = (*mcache)(mheap_.cachealloc.alloc())
		c.flushGen.Store(mheap_.sweepgen)
		unlock(&mheap_.lock)
	})
	for i := range c.alloc {
		c.alloc[i] = &emptymspan
	}
	c.nextSample = nextSample()
	return c
}

// freemcache releases resources associated with this
// mcache and puts the object onto a free list.
//
// In some cases there is no way to simply release
// resources, such as statistics, so donate them to
// a different mcache (the recipient).
func freemcache(c *mcache) {
	systemstack(func() {
		c.releaseAll()
		stackcache_clear(c)

		// NOTE(rsc,rlh): If gcworkbuffree comes back, we need to coordinate
		// with the stealing of gcworkbufs during garbage collection to avoid
		// a race where the workbuf is double-freed.
		// gcworkbuffree(c.gcworkbuf)

		lock(&mheap_.lock)
		mheap_.cachealloc.free(unsafe.Pointer(c))
		unlock(&mheap_.lock)
	})
}

// getMCache is a convenience function which tries to obtain an mcache.
//
// Returns nil if we're not bootstrapping or we don't have a P. The caller's
// P must not change, so we must be in a non-preemptible state.
func getMCache(mp *m) *mcache {
	// Grab the mcache, since that's where stats live.
	pp := mp.p.ptr()
	var c *mcache
	if pp == nil {
		// We will be called without a P while bootstrapping,
		// in which case we use mcache0, which is set in mallocinit.
		// mcache0 is cleared when bootstrapping is complete,
		// by procresize.
		c = mcache0
	} else {
		c = pp.mcache
	}
	return c
}

// 为缓存 c 中央列表mcentral中获取一个新的 span，该 span 至少包含一个空闲的对象。
// 当前缓存中的 span 必须已满。
//
// 必须在不可抢占的上下文中运行，因为否则缓存的所有权可能会发生变化。
func (c *mcache) refill(spc spanClass) {
	// 将当前缓存的 span 返回到中央列表。
	s := c.alloc[spc]

	if uintptr(s.allocCount) != s.nelems {
		throw("refill of span with free space remaining")
	}
	if s != &emptymspan {
		// 标记这个 span 不再被缓存。
		if s.sweepgen != mheap_.sweepgen+3 {
			throw("bad sweepgen in refill")
		}
		mheap_.central[spc].mcentral.uncacheSpan(s)

		// 统计使用了多少个槽位并记录。
		stats := memstats.heapStats.acquire()
		slotsUsed := int64(s.allocCount) - int64(s.allocCountBeforeCache)
		atomic.Xadd64(&stats.smallAllocCount[spc.sizeclass()], slotsUsed)

		// 刷新 tinyAllocs。
		if spc == tinySpanClass {
			atomic.Xadd64(&stats.tinyAllocCount, int64(c.tinyAllocs))
			c.tinyAllocs = 0
		}
		memstats.heapStats.release()

		// 在不一致的内部统计中计算分配的字节数。
		bytesAllocated := slotsUsed * int64(s.elemsize)
		gcController.totalAlloc.Add(bytesAllocated)

		// 为了安全起见，清空第二次的 allocCount。
		s.allocCountBeforeCache = 0
	}

	// 从中央列表获取一个新的缓存 span。
	s = mheap_.central[spc].mcentral.cacheSpan()
	if s == nil {
		throw("out of memory")
	}

	if uintptr(s.allocCount) == s.nelems {
		throw("span has no free space")
	}

	// 标记这个 span 被缓存，并防止在下一个清扫阶段的异步清扫。
	s.sweepgen = mheap_.sweepgen + 3

	// 存储当前的 allocCount 以备后续会计使用。
	s.allocCountBeforeCache = s.allocCount

	// 更新 heapLive 和冲刷新的 scanAlloc。
	//
	// 我们还没有向 span 分配任何新的对象，但我们假设 span 中的所有槽位都会被使用，
	// 所以这使得 heapLive 成为一个高估值。
	//
	// 当 span 被取消缓存时，我们会根据需要修正这个高估值（参见 releaseAll）。
	//
	// 我们在这里选择高估值，因为低估会导致 pacer 相信它比实际情况更好，
	// 这似乎会导致更多的内存使用。详情请参考 issue #53738。
	usedBytes := uintptr(s.allocCount) * s.elemsize
	gcController.update(int64(s.npages*pageSize)-int64(usedBytes), int64(c.scanAlloc))
	c.scanAlloc = 0

	c.alloc[spc] = s // 更新缓存中的 span
}

// allocLarge allocates a span for a large object.
func (c *mcache) allocLarge(size uintptr, noscan bool) *mspan {
	if size+_PageSize < size {
		throw("out of memory")
	}
	npages := size >> _PageShift
	if size&_PageMask != 0 {
		npages++
	}

	// Deduct credit for this span allocation and sweep if
	// necessary. mHeap_Alloc will also sweep npages, so this only
	// pays the debt down to npage pages.
	deductSweepCredit(npages*_PageSize, npages)

	spc := makeSpanClass(0, noscan)
	s := mheap_.alloc(npages, spc)
	if s == nil {
		throw("out of memory")
	}

	// Count the alloc in consistent, external stats.
	stats := memstats.heapStats.acquire()
	atomic.Xadd64(&stats.largeAlloc, int64(npages*pageSize))
	atomic.Xadd64(&stats.largeAllocCount, 1)
	memstats.heapStats.release()

	// Count the alloc in inconsistent, internal stats.
	gcController.totalAlloc.Add(int64(npages * pageSize))

	// Update heapLive.
	gcController.update(int64(s.npages*pageSize), 0)

	// Put the large span in the mcentral swept list so that it's
	// visible to the background sweeper.
	mheap_.central[spc].mcentral.fullSwept(mheap_.sweepgen).push(s)
	s.limit = s.base() + size
	s.initHeapBits(false)
	return s
}

func (c *mcache) releaseAll() {
	// Take this opportunity to flush scanAlloc.
	scanAlloc := int64(c.scanAlloc)
	c.scanAlloc = 0

	sg := mheap_.sweepgen
	dHeapLive := int64(0)
	for i := range c.alloc {
		s := c.alloc[i]
		if s != &emptymspan {
			slotsUsed := int64(s.allocCount) - int64(s.allocCountBeforeCache)
			s.allocCountBeforeCache = 0

			// Adjust smallAllocCount for whatever was allocated.
			stats := memstats.heapStats.acquire()
			atomic.Xadd64(&stats.smallAllocCount[spanClass(i).sizeclass()], slotsUsed)
			memstats.heapStats.release()

			// Adjust the actual allocs in inconsistent, internal stats.
			// We assumed earlier that the full span gets allocated.
			gcController.totalAlloc.Add(slotsUsed * int64(s.elemsize))

			if s.sweepgen != sg+1 {
				// refill conservatively counted unallocated slots in gcController.heapLive.
				// Undo this.
				//
				// If this span was cached before sweep, then gcController.heapLive was totally
				// recomputed since caching this span, so we don't do this for stale spans.
				dHeapLive -= int64(uintptr(s.nelems)-uintptr(s.allocCount)) * int64(s.elemsize)
			}

			// Release the span to the mcentral.
			mheap_.central[i].mcentral.uncacheSpan(s)
			c.alloc[i] = &emptymspan
		}
	}
	// Clear tinyalloc pool.
	c.tiny = 0
	c.tinyoffset = 0

	// Flush tinyAllocs.
	stats := memstats.heapStats.acquire()
	atomic.Xadd64(&stats.tinyAllocCount, int64(c.tinyAllocs))
	c.tinyAllocs = 0
	memstats.heapStats.release()

	// Update heapLive and heapScan.
	gcController.update(dHeapLive, scanAlloc)
}

// prepareForSweep flushes c if the system has entered a new sweep phase
// since c was populated. This must happen between the sweep phase
// starting and the first allocation from c.
func (c *mcache) prepareForSweep() {
	// Alternatively, instead of making sure we do this on every P
	// between starting the world and allocating on that P, we
	// could leave allocate-black on, allow allocation to continue
	// as usual, use a ragged barrier at the beginning of sweep to
	// ensure all cached spans are swept, and then disable
	// allocate-black. However, with this approach it's difficult
	// to avoid spilling mark bits into the *next* GC cycle.
	sg := mheap_.sweepgen
	flushGen := c.flushGen.Load()
	if flushGen == sg {
		return
	} else if flushGen != sg-2 {
		println("bad flushGen", flushGen, "in prepareForSweep; sweepgen", sg)
		throw("bad flushGen")
	}
	c.releaseAll()
	stackcache_clear(c)
	c.flushGen.Store(mheap_.sweepgen) // Synchronizes with gcStart
}
