// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Garbage collector: marking and scanning

package runtime

import (
	"internal/abi"
	"internal/goarch"
	"runtime/internal/atomic"
	"runtime/internal/sys"
	"unsafe"
)

const (
	fixedRootFinalizers = iota
	fixedRootFreeGStacks
	fixedRootCount

	// rootBlockBytes is the number of bytes to scan per data or
	// BSS root.
	rootBlockBytes = 256 << 10

	// maxObletBytes is the maximum bytes of an object to scan at
	// once. Larger objects will be split up into "oblets" of at
	// most this size. Since we can scan 1–2 MB/ms, 128 KB bounds
	// scan preemption at ~100 µs.
	//
	// This must be > _MaxSmallSize so that the object base is the
	// span base.
	maxObletBytes = 128 << 10

	// drainCheckThreshold specifies how many units of work to do
	// between self-preemption checks in gcDrain. Assuming a scan
	// rate of 1 MB/ms, this is ~100 µs. Lower values have higher
	// overhead in the scan loop (the scheduler check may perform
	// a syscall, so its overhead is nontrivial). Higher values
	// make the system less responsive to incoming work.
	drainCheckThreshold = 100000

	// pagesPerSpanRoot indicates how many pages to scan from a span root
	// at a time. Used by special root marking.
	//
	// Higher values improve throughput by increasing locality, but
	// increase the minimum latency of a marking operation.
	//
	// Must be a multiple of the pageInUse bitmap element size and
	// must also evenly divide pagesPerArena.
	pagesPerSpanRoot = 512
)

// gcMarkRootPrepare queues root scanning jobs (stacks, globals, and
// some miscellany) and initializes scanning-related state.
//
// The world must be stopped.
func gcMarkRootPrepare() {
	assertWorldStopped()

	// Compute how many data and BSS root blocks there are.
	nBlocks := func(bytes uintptr) int {
		return int(divRoundUp(bytes, rootBlockBytes))
	}

	work.nDataRoots = 0
	work.nBSSRoots = 0

	// Scan globals.
	for _, datap := range activeModules() {
		nDataRoots := nBlocks(datap.edata - datap.data)
		if nDataRoots > work.nDataRoots {
			work.nDataRoots = nDataRoots
		}
	}

	for _, datap := range activeModules() {
		nBSSRoots := nBlocks(datap.ebss - datap.bss)
		if nBSSRoots > work.nBSSRoots {
			work.nBSSRoots = nBSSRoots
		}
	}

	// Scan span roots for finalizer specials.
	//
	// We depend on addfinalizer to mark objects that get
	// finalizers after root marking.
	//
	// We're going to scan the whole heap (that was available at the time the
	// mark phase started, i.e. markArenas) for in-use spans which have specials.
	//
	// Break up the work into arenas, and further into chunks.
	//
	// Snapshot allArenas as markArenas. This snapshot is safe because allArenas
	// is append-only.
	mheap_.markArenas = mheap_.allArenas[:len(mheap_.allArenas):len(mheap_.allArenas)]
	work.nSpanRoots = len(mheap_.markArenas) * (pagesPerArena / pagesPerSpanRoot)

	// Scan stacks.
	//
	// Gs may be created after this point, but it's okay that we
	// ignore them because they begin life without any roots, so
	// there's nothing to scan, and any roots they create during
	// the concurrent phase will be caught by the write barrier.
	work.stackRoots = allGsSnapshot()
	work.nStackRoots = len(work.stackRoots)

	work.markrootNext = 0
	work.markrootJobs = uint32(fixedRootCount + work.nDataRoots + work.nBSSRoots + work.nSpanRoots + work.nStackRoots)

	// Calculate base indexes of each root type
	work.baseData = uint32(fixedRootCount)
	work.baseBSS = work.baseData + uint32(work.nDataRoots)
	work.baseSpans = work.baseBSS + uint32(work.nBSSRoots)
	work.baseStacks = work.baseSpans + uint32(work.nSpanRoots)
	work.baseEnd = work.baseStacks + uint32(work.nStackRoots)
}

// gcMarkRootCheck checks that all roots have been scanned. It is
// purely for debugging.
func gcMarkRootCheck() {
	if work.markrootNext < work.markrootJobs {
		print(work.markrootNext, " of ", work.markrootJobs, " markroot jobs done\n")
		throw("left over markroot jobs")
	}

	// Check that stacks have been scanned.
	//
	// We only check the first nStackRoots Gs that we should have scanned.
	// Since we don't care about newer Gs (see comment in
	// gcMarkRootPrepare), no locking is required.
	i := 0
	forEachGRace(func(gp *g) {
		if i >= work.nStackRoots {
			return
		}

		if !gp.gcscandone {
			println("gp", gp, "goid", gp.goid,
				"status", readgstatus(gp),
				"gcscandone", gp.gcscandone)
			throw("scan missed a g")
		}

		i++
	})
}

// ptrmask for an allocation containing a single pointer.
var oneptrmask = [...]uint8{1}

// markroot 扫描第 i 个根对象。
//
// 必须禁用抢占（因为这使用了 gcWork）。
//
// 返回操作产生的 GC 工作量。
// 如果 flushBgCredit 为 true，则还会将该工作量刷新到后台信用池。
//
// 在此处 nowritebarrier 仅是一个建议性的声明。
//
// - `gcw` 是一个指向 `gcWork` 结构的指针，用于管理垃圾回收工作的队列。
// - `i` 是一个 `uint32` 类型的整数，表示当前扫描的根对象的索引。
// - `flushBgCredit` 是一个布尔值，指示是否应该将产生的 GC 工作量刷新到后台信用池。
//
//go:nowritebarrier
func markroot(gcw *gcWork, i uint32, flushBgCredit bool) int64 {
	// 注意：如果在此处添加一个 case，请同时更新 heapdump.go:dumproots。
	var workDone int64
	var workCounter *atomic.Int64
	switch {
	// 处理基础数据段（.data 到 .edata 之间的数据）: 已初始化的全局变量和静态变量
	case work.baseData <= i && i < work.baseBSS:
		workCounter = &gcController.globalsScanWork
		for _, datap := range activeModules() {
			workDone += markrootBlock(datap.data, datap.edata-datap.data, datap.gcdatamask.bytedata, gcw, int(i-work.baseData))
		}

	// 处理基础 BSS 段（.bss 到 .ebss 之间的数据）: 未初始化的全局变量和静态变量
	case work.baseBSS <= i && i < work.baseSpans:
		workCounter = &gcController.globalsScanWork
		for _, datap := range activeModules() {
			workDone += markrootBlock(datap.bss, datap.ebss-datap.bss, datap.gcbssmask.bytedata, gcw, int(i-work.baseBSS))
		}

	// 处理固定根 finalizer
	// 在 Go 语言中，finalizer 是一个指定的函数，当一个对象即将被垃圾回收器回收时，会调用该函数来执行一些清理操作，比如关闭文件、释放资源等。
	// 这些对象的 finalizer 需要在垃圾回收过程中被特殊处理，确保在清理操作时正常工作
	case i == fixedRootFinalizers:
		for fb := allfin; fb != nil; fb = fb.alllink {
			cnt := uintptr(atomic.Load(&fb.cnt))
			scanblock(uintptr(unsafe.Pointer(&fb.fin[0])), cnt*unsafe.Sizeof(fb.fin[0]), &finptrmask[0], gcw, nil)
		}

	// 处理固定根 free G stacks
	// 在 Go 中，每个 Goroutine 都有一个对应的栈空间，用于执行函数。当 Goroutine 不再活跃时，其栈空间可能需要被释放以节省资源。
	// 在垃圾回收过程中，需要特殊处理这些对象，确保正确释放不再使用的 Goroutine 栈。
	case i == fixedRootFreeGStacks:
		// 切换到系统栈以便调用 stackfree。
		systemstack(markrootFreeGStacks)

	// 处理基础 Span 段（.span 到 .espan 之间的数据）: 分配 Span 的数据结构
	// 在 Go 内存管理中，Span 是用于分配和管理内存的数据结构，通常代表一段连续的内存区域。
	// .span 到 .espan 之间的数据表示的是用于表示内存分配 Span 的相关数据。
	// 在垃圾回收过程中，这些数据结构需要被标记，以确保垃圾回收器正确地跟踪内存的使用情况，对未使用的内存进行回收。
	case work.baseSpans <= i && i < work.baseStacks:
		// mark mspan.specials
		markrootSpans(gcw, int(i-work.baseSpans))

	// 其余情况为扫描 goroutine 堆栈
	default:
		workCounter = &gcController.stackScanWork
		if i < work.baseStacks || work.baseEnd <= i {
			printlock()
			print("runtime: markroot index ", i, " not in stack roots range [", work.baseStacks, ", ", work.baseEnd, ")\n")
			throw("markroot: bad index")
		}

		// 获取要扫描的 goroutine
		gp := work.stackRoots[i-work.baseStacks]

		// 如果 goroutine 处于等待或系统调用状态，记录等待时间。
		status := readgstatus(gp) // 我们不在扫描状态
		if (status == _Gwaiting || status == _Gsyscall) && gp.waitsince == 0 {
			gp.waitsince = work.tstart
		}

		systemstack(func() {
			userG := getg().m.curg
			selfScan := gp == userG && readgstatus(userG) == _Grunning

			// 若为自身扫描，则将用户 G 置于 _Gwaiting 以防止自身死锁。
			// 若这是一个标记工作者或者我们处于标记终止，则可能已经处于 _Gwaiting 状态。
			if selfScan {
				casGToWaiting(userG, _Grunning, waitReasonGarbageCollectionScan)
			}

			// TODO: suspendG 阻塞（并自旋）直到 gp 停止，
			// 这可能需要一段时间，尤其是对于运行中的 goroutine。
			// 第一阶段是非阻塞的：我们扫描可以扫描的堆栈，并要求运行中的 goroutine 自行扫描；
			// 第二阶段则是阻塞的。

			// 使用 `suspendG` 函数暂停 goroutine，以便扫描其堆栈
			stopped := suspendG(gp)
			// 如果 goroutine 已经死亡，则标记为扫描完成并返回
			if stopped.dead {
				gp.gcscandone = true
				return
			}
			// 如果 goroutine 已经被扫描过，则抛出异常
			if gp.gcscandone {
				throw("g already scanned")
			}

			// 使用 scanstack 函数来扫描 goroutine 的堆栈
			workDone += scanstack(gp, gcw)
			gp.gcscandone = true

			// 完成扫描后，使用 resumeG 函数恢复 goroutine 的执行
			resumeG(stopped)

			// 如果是自我扫描，恢复到 _Grunning 状态
			if selfScan {
				casgstatus(userG, _Gwaiting, _Grunning)
			}
		})
	}

	// 如果 workCounter 不为 nil 并且 workDone 不为零，则添加工作量到 workCounter 中。
	// 如果 flushBgCredit 为 true，则将工作量刷新到后台信用池。
	// 根据扫描的根对象类型，函数可能会更新不同的工作量计数器
	if workCounter != nil && workDone != 0 {
		workCounter.Add(workDone)
		if flushBgCredit {
			gcFlushBgCredit(workDone)
		}
	}
	return workDone
}

// markrootBlock scans the shard'th shard of the block of memory [b0,
// b0+n0), with the given pointer mask.
//
// Returns the amount of work done.
//
//go:nowritebarrier
func markrootBlock(b0, n0 uintptr, ptrmask0 *uint8, gcw *gcWork, shard int) int64 {
	if rootBlockBytes%(8*goarch.PtrSize) != 0 {
		// This is necessary to pick byte offsets in ptrmask0.
		throw("rootBlockBytes must be a multiple of 8*ptrSize")
	}

	// Note that if b0 is toward the end of the address space,
	// then b0 + rootBlockBytes might wrap around.
	// These tests are written to avoid any possible overflow.
	off := uintptr(shard) * rootBlockBytes
	if off >= n0 {
		return 0
	}
	b := b0 + off
	ptrmask := (*uint8)(add(unsafe.Pointer(ptrmask0), uintptr(shard)*(rootBlockBytes/(8*goarch.PtrSize))))
	n := uintptr(rootBlockBytes)
	if off+n > n0 {
		n = n0 - off
	}

	// Scan this shard.
	scanblock(b, n, ptrmask, gcw, nil)
	return int64(n)
}

// markrootFreeGStacks frees stacks of dead Gs.
//
// This does not free stacks of dead Gs cached on Ps, but having a few
// cached stacks around isn't a problem.
func markrootFreeGStacks() {
	// Take list of dead Gs with stacks.
	lock(&sched.gFree.lock)
	list := sched.gFree.stack
	sched.gFree.stack = gList{}
	unlock(&sched.gFree.lock)
	if list.empty() {
		return
	}

	// Free stacks.
	q := gQueue{list.head, list.head}
	for gp := list.head.ptr(); gp != nil; gp = gp.schedlink.ptr() {
		stackfree(gp.stack)
		gp.stack.lo = 0
		gp.stack.hi = 0
		// Manipulate the queue directly since the Gs are
		// already all linked the right way.
		q.tail.set(gp)
	}

	// Put Gs back on the free list.
	lock(&sched.gFree.lock)
	sched.gFree.noStack.pushAll(q)
	unlock(&sched.gFree.lock)
}

// markrootSpans marks roots for one shard of markArenas.
//
//go:nowritebarrier
func markrootSpans(gcw *gcWork, shard int) {
	// Objects with finalizers have two GC-related invariants:
	//
	// 1) Everything reachable from the object must be marked.
	// This ensures that when we pass the object to its finalizer,
	// everything the finalizer can reach will be retained.
	//
	// 2) Finalizer specials (which are not in the garbage
	// collected heap) are roots. In practice, this means the fn
	// field must be scanned.
	sg := mheap_.sweepgen

	// Find the arena and page index into that arena for this shard.
	ai := mheap_.markArenas[shard/(pagesPerArena/pagesPerSpanRoot)]
	ha := mheap_.arenas[ai.l1()][ai.l2()]
	arenaPage := uint(uintptr(shard) * pagesPerSpanRoot % pagesPerArena)

	// Construct slice of bitmap which we'll iterate over.
	specialsbits := ha.pageSpecials[arenaPage/8:]
	specialsbits = specialsbits[:pagesPerSpanRoot/8]
	for i := range specialsbits {
		// Find set bits, which correspond to spans with specials.
		specials := atomic.Load8(&specialsbits[i])
		if specials == 0 {
			continue
		}
		for j := uint(0); j < 8; j++ {
			if specials&(1<<j) == 0 {
				continue
			}
			// Find the span for this bit.
			//
			// This value is guaranteed to be non-nil because having
			// specials implies that the span is in-use, and since we're
			// currently marking we can be sure that we don't have to worry
			// about the span being freed and re-used.
			s := ha.spans[arenaPage+uint(i)*8+j]

			// The state must be mSpanInUse if the specials bit is set, so
			// sanity check that.
			if state := s.state.get(); state != mSpanInUse {
				print("s.state = ", state, "\n")
				throw("non in-use span found with specials bit set")
			}
			// Check that this span was swept (it may be cached or uncached).
			if !useCheckmark && !(s.sweepgen == sg || s.sweepgen == sg+3) {
				// sweepgen was updated (+2) during non-checkmark GC pass
				print("sweep ", s.sweepgen, " ", sg, "\n")
				throw("gc: unswept span")
			}

			// Lock the specials to prevent a special from being
			// removed from the list while we're traversing it.
			lock(&s.speciallock)
			for sp := s.specials; sp != nil; sp = sp.next {
				if sp.kind != _KindSpecialFinalizer {
					continue
				}
				// don't mark finalized object, but scan it so we
				// retain everything it points to.
				spf := (*specialfinalizer)(unsafe.Pointer(sp))
				// A finalizer can be set for an inner byte of an object, find object beginning.
				p := s.base() + uintptr(spf.special.offset)/s.elemsize*s.elemsize

				// Mark everything that can be reached from
				// the object (but *not* the object itself or
				// we'll never collect it).
				if !s.spanclass.noscan() {
					scanobject(p, gcw)
				}

				// The special itself is a root.
				scanblock(uintptr(unsafe.Pointer(&spf.fn)), goarch.PtrSize, &oneptrmask[0], gcw, nil)
			}
			unlock(&s.speciallock)
		}
	}
}

// gcAssistAlloc performs GC work to make gp's assist debt positive.
// gp must be the calling user goroutine.
//
// This must be called with preemption enabled.
func gcAssistAlloc(gp *g) {
	// Don't assist in non-preemptible contexts. These are
	// generally fragile and won't allow the assist to block.
	if getg() == gp.m.g0 {
		return
	}
	if mp := getg().m; mp.locks > 0 || mp.preemptoff != "" {
		return
	}

	traced := false
retry:
	if gcCPULimiter.limiting() {
		// If the CPU limiter is enabled, intentionally don't
		// assist to reduce the amount of CPU time spent in the GC.
		if traced {
			traceGCMarkAssistDone()
		}
		return
	}
	// Compute the amount of scan work we need to do to make the
	// balance positive. When the required amount of work is low,
	// we over-assist to build up credit for future allocations
	// and amortize the cost of assisting.
	assistWorkPerByte := gcController.assistWorkPerByte.Load()
	assistBytesPerWork := gcController.assistBytesPerWork.Load()
	debtBytes := -gp.gcAssistBytes
	scanWork := int64(assistWorkPerByte * float64(debtBytes))
	if scanWork < gcOverAssistWork {
		scanWork = gcOverAssistWork
		debtBytes = int64(assistBytesPerWork * float64(scanWork))
	}

	// Steal as much credit as we can from the background GC's
	// scan credit. This is racy and may drop the background
	// credit below 0 if two mutators steal at the same time. This
	// will just cause steals to fail until credit is accumulated
	// again, so in the long run it doesn't really matter, but we
	// do have to handle the negative credit case.
	bgScanCredit := gcController.bgScanCredit.Load()
	stolen := int64(0)
	if bgScanCredit > 0 {
		if bgScanCredit < scanWork {
			stolen = bgScanCredit
			gp.gcAssistBytes += 1 + int64(assistBytesPerWork*float64(stolen))
		} else {
			stolen = scanWork
			gp.gcAssistBytes += debtBytes
		}
		gcController.bgScanCredit.Add(-stolen)

		scanWork -= stolen

		if scanWork == 0 {
			// We were able to steal all of the credit we
			// needed.
			if traced {
				traceGCMarkAssistDone()
			}
			return
		}
	}

	if traceEnabled() && !traced {
		traced = true
		traceGCMarkAssistStart()
	}

	// Perform assist work
	systemstack(func() {
		gcAssistAlloc1(gp, scanWork)
		// The user stack may have moved, so this can't touch
		// anything on it until it returns from systemstack.
	})

	completed := gp.param != nil
	gp.param = nil
	if completed {
		gcMarkDone()
	}

	if gp.gcAssistBytes < 0 {
		// We were unable steal enough credit or perform
		// enough work to pay off the assist debt. We need to
		// do one of these before letting the mutator allocate
		// more to prevent over-allocation.
		//
		// If this is because we were preempted, reschedule
		// and try some more.
		if gp.preempt {
			Gosched()
			goto retry
		}

		// Add this G to an assist queue and park. When the GC
		// has more background credit, it will satisfy queued
		// assists before flushing to the global credit pool.
		//
		// Note that this does *not* get woken up when more
		// work is added to the work list. The theory is that
		// there wasn't enough work to do anyway, so we might
		// as well let background marking take care of the
		// work that is available.
		if !gcParkAssist() {
			goto retry
		}

		// At this point either background GC has satisfied
		// this G's assist debt, or the GC cycle is over.
	}
	if traced {
		traceGCMarkAssistDone()
	}
}

// gcAssistAlloc1 is the part of gcAssistAlloc that runs on the system
// stack. This is a separate function to make it easier to see that
// we're not capturing anything from the user stack, since the user
// stack may move while we're in this function.
//
// gcAssistAlloc1 indicates whether this assist completed the mark
// phase by setting gp.param to non-nil. This can't be communicated on
// the stack since it may move.
//
//go:systemstack
func gcAssistAlloc1(gp *g, scanWork int64) {
	// Clear the flag indicating that this assist completed the
	// mark phase.
	gp.param = nil

	if atomic.Load(&gcBlackenEnabled) == 0 {
		// The gcBlackenEnabled check in malloc races with the
		// store that clears it but an atomic check in every malloc
		// would be a performance hit.
		// Instead we recheck it here on the non-preemptable system
		// stack to determine if we should perform an assist.

		// GC is done, so ignore any remaining debt.
		gp.gcAssistBytes = 0
		return
	}
	// Track time spent in this assist. Since we're on the
	// system stack, this is non-preemptible, so we can
	// just measure start and end time.
	//
	// Limiter event tracking might be disabled if we end up here
	// while on a mark worker.
	startTime := nanotime()
	trackLimiterEvent := gp.m.p.ptr().limiterEvent.start(limiterEventMarkAssist, startTime)

	decnwait := atomic.Xadd(&work.nwait, -1)
	if decnwait == work.nproc {
		println("runtime: work.nwait =", decnwait, "work.nproc=", work.nproc)
		throw("nwait > work.nprocs")
	}

	// gcDrainN requires the caller to be preemptible.
	casGToWaiting(gp, _Grunning, waitReasonGCAssistMarking)

	// drain own cached work first in the hopes that it
	// will be more cache friendly.
	gcw := &getg().m.p.ptr().gcw
	workDone := gcDrainN(gcw, scanWork)

	casgstatus(gp, _Gwaiting, _Grunning)

	// Record that we did this much scan work.
	//
	// Back out the number of bytes of assist credit that
	// this scan work counts for. The "1+" is a poor man's
	// round-up, to ensure this adds credit even if
	// assistBytesPerWork is very low.
	assistBytesPerWork := gcController.assistBytesPerWork.Load()
	gp.gcAssistBytes += 1 + int64(assistBytesPerWork*float64(workDone))

	// If this is the last worker and we ran out of work,
	// signal a completion point.
	incnwait := atomic.Xadd(&work.nwait, +1)
	if incnwait > work.nproc {
		println("runtime: work.nwait=", incnwait,
			"work.nproc=", work.nproc)
		throw("work.nwait > work.nproc")
	}

	if incnwait == work.nproc && !gcMarkWorkAvailable(nil) {
		// This has reached a background completion point. Set
		// gp.param to a non-nil value to indicate this. It
		// doesn't matter what we set it to (it just has to be
		// a valid pointer).
		gp.param = unsafe.Pointer(gp)
	}
	now := nanotime()
	duration := now - startTime
	pp := gp.m.p.ptr()
	pp.gcAssistTime += duration
	if trackLimiterEvent {
		pp.limiterEvent.stop(limiterEventMarkAssist, now)
	}
	if pp.gcAssistTime > gcAssistTimeSlack {
		gcController.assistTime.Add(pp.gcAssistTime)
		gcCPULimiter.update(now)
		pp.gcAssistTime = 0
	}
}

// gcWakeAllAssists wakes all currently blocked assists. This is used
// at the end of a GC cycle. gcBlackenEnabled must be false to prevent
// new assists from going to sleep after this point.
func gcWakeAllAssists() {
	lock(&work.assistQueue.lock)
	list := work.assistQueue.q.popList()
	injectglist(&list)
	unlock(&work.assistQueue.lock)
}

// gcParkAssist puts the current goroutine on the assist queue and parks.
//
// gcParkAssist reports whether the assist is now satisfied. If it
// returns false, the caller must retry the assist.
func gcParkAssist() bool {
	lock(&work.assistQueue.lock)
	// If the GC cycle finished while we were getting the lock,
	// exit the assist. The cycle can't finish while we hold the
	// lock.
	if atomic.Load(&gcBlackenEnabled) == 0 {
		unlock(&work.assistQueue.lock)
		return true
	}

	gp := getg()
	oldList := work.assistQueue.q
	work.assistQueue.q.pushBack(gp)

	// Recheck for background credit now that this G is in
	// the queue, but can still back out. This avoids a
	// race in case background marking has flushed more
	// credit since we checked above.
	if gcController.bgScanCredit.Load() > 0 {
		work.assistQueue.q = oldList
		if oldList.tail != 0 {
			oldList.tail.ptr().schedlink.set(nil)
		}
		unlock(&work.assistQueue.lock)
		return false
	}
	// Park.
	goparkunlock(&work.assistQueue.lock, waitReasonGCAssistWait, traceBlockGCMarkAssist, 2)
	return true
}

// gcFlushBgCredit flushes scanWork units of background scan work
// credit. This first satisfies blocked assists on the
// work.assistQueue and then flushes any remaining credit to
// gcController.bgScanCredit.
//
// Write barriers are disallowed because this is used by gcDrain after
// it has ensured that all work is drained and this must preserve that
// condition.
//
//go:nowritebarrierrec
func gcFlushBgCredit(scanWork int64) {
	if work.assistQueue.q.empty() {
		// Fast path; there are no blocked assists. There's a
		// small window here where an assist may add itself to
		// the blocked queue and park. If that happens, we'll
		// just get it on the next flush.
		gcController.bgScanCredit.Add(scanWork)
		return
	}

	assistBytesPerWork := gcController.assistBytesPerWork.Load()
	scanBytes := int64(float64(scanWork) * assistBytesPerWork)

	lock(&work.assistQueue.lock)
	for !work.assistQueue.q.empty() && scanBytes > 0 {
		gp := work.assistQueue.q.pop()
		// Note that gp.gcAssistBytes is negative because gp
		// is in debt. Think carefully about the signs below.
		if scanBytes+gp.gcAssistBytes >= 0 {
			// Satisfy this entire assist debt.
			scanBytes += gp.gcAssistBytes
			gp.gcAssistBytes = 0
			// It's important that we *not* put gp in
			// runnext. Otherwise, it's possible for user
			// code to exploit the GC worker's high
			// scheduler priority to get itself always run
			// before other goroutines and always in the
			// fresh quantum started by GC.
			ready(gp, 0, false)
		} else {
			// Partially satisfy this assist.
			gp.gcAssistBytes += scanBytes
			scanBytes = 0
			// As a heuristic, we move this assist to the
			// back of the queue so that large assists
			// can't clog up the assist queue and
			// substantially delay small assists.
			work.assistQueue.q.pushBack(gp)
			break
		}
	}

	if scanBytes > 0 {
		// Convert from scan bytes back to work.
		assistWorkPerByte := gcController.assistWorkPerByte.Load()
		scanWork = int64(float64(scanBytes) * assistWorkPerByte)
		gcController.bgScanCredit.Add(scanWork)
	}
	unlock(&work.assistQueue.lock)
}

// scanstack 扫描 goroutine 的堆栈，将堆栈上的所有指针标记为灰色。
//
// 返回扫描操作产生的工作量，但不会更新 gcController.stackScanWork 或刷新任何信用。
// 任何由该函数产生的后台信用应由调用者刷新。scanstack 本身无法安全地刷新信用，
// 因为这可能会导致尝试唤醒刚刚扫描过的 goroutine，从而导致自我死锁。
//
// scanstack 也会安全地缩小堆栈。如果无法安全缩小，则会在下一个同步安全点安排堆栈缩小。
//
// scanstack 被标记为 go:systemstack，因为它在使用 workbuf 时不能被抢占。
//
// - gp 是一个指向 g 结构的指针，代表当前要扫描的 goroutine。
// - gcw 是一个指向 gcWork 结构的指针，用于管理垃圾回收工作的队列。
//
//go:nowritebarrier
//go:systemstack
func scanstack(gp *g, gcw *gcWork) int64 {
	// 函数首先检查 goroutine 的状态是否适合扫描

	if readgstatus(gp)&_Gscan == 0 {
		print("runtime:scanstack: gp=", gp, ", goid=", gp.goid, ", gp->atomicstatus=", hex(readgstatus(gp)), "\n")
		throw("scanstack - bad status")
	}

	switch readgstatus(gp) &^ _Gscan {
	default:
		print("runtime: gp=", gp, ", goid=", gp.goid, ", gp->atomicstatus=", readgstatus(gp), "\n")
		throw("mark - bad status")
	case _Gdead:
		return 0
	case _Grunning:
		print("runtime: gp=", gp, ", goid=", gp.goid, ", gp->atomicstatus=", readgstatus(gp), "\n")
		throw("scanstack: goroutine not stopped")
	case _Grunnable, _Gsyscall, _Gwaiting:
		// ok
	}

	if gp == getg() {
		throw("can't scan our own stack")
	}

	// scannedSize 是我们将会报告的工作量。
	//
	// 它小于分配的大小（即 hi-lo）。
	var sp uintptr
	if gp.syscallsp != 0 {
		sp = gp.syscallsp // 如果正在系统调用中，这是堆栈指针（gp.sched.sp 在这种情况下可能为 0，例如在 Windows 上）。
	} else {
		sp = gp.sched.sp
	}
	scannedSize := gp.stack.hi - sp

	// 获取当前 goroutine 的堆栈指针。保留统计信息以计算初始堆栈大小。
	// 注意，这累积的是扫描的大小，而非分配的大小。
	p := getg().m.p.ptr()
	p.scannedStackSize += uint64(scannedSize)
	p.scannedStacks++

	if isShrinkStackSafe(gp) {
		// 如果堆栈未被大量使用，则缩小堆栈。
		shrinkstack(gp)
	} else {
		// 否则，在下一个同步安全点时缩小堆栈。
		gp.preemptShrink = true
	}

	var state stackScanState
	state.stack = gp.stack

	if stackTraceDebug {
		println("stack trace goroutine", gp.goid)
	}

	if debugScanConservative && gp.asyncSafePoint {
		print("scanning async preempted goroutine ", gp.goid, " stack [", hex(gp.stack.lo), ",", hex(gp.stack.hi), ")\n")
	}

	// 扫描保存的上下文寄存器。这实质上是一个活动寄存器，它会在寄存器和 sched.ctxt 之间来回移动，无需写屏障。
	if gp.sched.ctxt != nil {
		scanblock(uintptr(unsafe.Pointer(&gp.sched.ctxt)), goarch.PtrSize, &oneptrmask[0], gcw, &state)
	}

	// 扫描堆栈。积累堆栈对象列表。
	var u unwinder
	for u.init(gp, 0); u.valid(); u.next() {
		scanframeworker(&u.frame, &state, gcw)
	}

	// 寻找从堆指向堆栈的其他指针。目前这包括 defer 和 panic。

	// 扫描 defer 记录中的其他指针。
	for d := gp._defer; d != nil; d = d.link {
		if d.fn != nil {
			// 扫描函数值，它可能是堆栈分配的闭包。
			// 参见 issue 30453。
			scanblock(uintptr(unsafe.Pointer(&d.fn)), goarch.PtrSize, &oneptrmask[0], gcw, &state)
		}
		if d.link != nil {
			// defer 记录的 link 字段可能指向堆分配的 defer 记录。保留堆记录的活性。
			scanblock(uintptr(unsafe.Pointer(&d.link)), goarch.PtrSize, &oneptrmask[0], gcw, &state)
		}
		// 保留 defer 记录本身。
		// defer 记录可能无法通过常规的堆追踪从 G 达到，因为 defer 链表可能在堆栈和堆之间交织。
		if d.heap {
			scanblock(uintptr(unsafe.Pointer(&d)), goarch.PtrSize, &oneptrmask[0], gcw, &state)
		}
	}
	if gp._panic != nil {
		// panic 总是堆栈分配的。
		state.putPtr(uintptr(unsafe.Pointer(gp._panic)), false)
	}

	// 寻找并扫描所有可达的堆栈对象。构建索引，用于精确扫描堆栈对象

	// state 的指针队列优先考虑精确指针而非保守指针，这样我们会倾向于精确扫描堆栈对象。
	state.buildIndex()
	// 通过一个循环来遍历 state 中的指针队列，每次取出一个指针 p 及其是否为保守指针的标志 conservative。
	for {
		// 如果 p 为 0，说明队列已空，结束循环。
		p, conservative := state.getPtr()
		if p == 0 {
			break
		}

		// 查找指针 p 所指向的堆栈对象 obj
		obj := state.findObject(p)
		if obj == nil {
			continue
		}
		r := obj.r

		// 我们已经扫描了这个对象。
		if r == nil {
			continue
		}
		obj.setRecord(nil) // 不再扫描它。

		// 打印有关扫描对象的信息
		if stackTraceDebug {
			printlock()
			print("  live stkobj at", hex(state.stack.lo+uintptr(obj.off)), "of size", obj.size)
			if conservative {
				print(" (conservative)")
			}
			println()
			printunlock()
		}

		// 获取对象的 GC 数据 gcdata，如果对象使用了 GC 程序，则需要进一步处理
		gcdata := r.gcdata()
		var s *mspan
		if r.useGCProg() {
			// 这条路径不太可能发生，即足够大的需要 GC 程序的堆栈分配对象。
			// 我们需要一些空间将程序解压缩为直的位掩码，我们在这里分配/释放空间。
			// TODO: 如果有一种方法可以在不存储所有位的情况下运行 GC 程序，那就太好了。
			// 我们需要从 Lempel-Ziv 风格的程序转换为其他东西。
			// 或者我们可以禁止将需要 GC 程序的对象放在堆栈上（参见 issue 27447）。
			s = materializeGCProg(r.ptrdata(), gcdata)
			gcdata = (*byte)(unsafe.Pointer(s.startAddr))
		}

		b := state.stack.lo + uintptr(obj.off)

		// scanblock 和 scanConservative 函数都会扫描对象中的指针，并递归地扫描指向的对象

		// 根据 conservative 的值决定使用哪种扫描方式：
		if conservative {
			//如果 conservative 为 true，则调用 scanConservative。
			scanConservative(b, r.ptrdata(), gcdata, gcw, &state) // 它负责保守地扫描给定的内存区间，并将区间内的潜在指针标记为灰色
		} else {
			//如果 conservative 为 false，则调用 scanblock。
			scanblock(b, r.ptrdata(), gcdata, gcw, &state)
		}

		// 如果使用了额外的空间来解压 GC 程序，则调用 dematerializeGCProg 清理这些临时数据
		if s != nil {
			dematerializeGCProg(s)
		}
	}

	// 释放不再需要的对象缓冲区

	// （指针缓冲区已在上述循环中全部释放。）
	for state.head != nil {
		x := state.head
		state.head = x.next
		if stackTraceDebug {
			for i := 0; i < x.nobj; i++ {
				obj := &x.obj[i]
				if obj.r == nil { // reachable
					continue
				}
				println("  dead stkobj at", hex(gp.stack.lo+uintptr(obj.off)), "of size", obj.r.size)
				// 注意：不一定真正死亡 - 只是不可从指针到达。
			}
		}
		x.nobj = 0
		putempty((*workbuf)(unsafe.Pointer(x)))
	}
	if state.buf != nil || state.cbuf != nil || state.freeBuf != nil {
		throw("remaining pointer buffers")
	}
	return int64(scannedSize)
}

// Scan a stack frame: local variables and function arguments/results.
//
//go:nowritebarrier
func scanframeworker(frame *stkframe, state *stackScanState, gcw *gcWork) {
	if _DebugGC > 1 && frame.continpc != 0 {
		print("scanframe ", funcname(frame.fn), "\n")
	}

	isAsyncPreempt := frame.fn.valid() && frame.fn.funcID == abi.FuncID_asyncPreempt
	isDebugCall := frame.fn.valid() && frame.fn.funcID == abi.FuncID_debugCallV2
	if state.conservative || isAsyncPreempt || isDebugCall {
		if debugScanConservative {
			println("conservatively scanning function", funcname(frame.fn), "at PC", hex(frame.continpc))
		}

		// Conservatively scan the frame. Unlike the precise
		// case, this includes the outgoing argument space
		// since we may have stopped while this function was
		// setting up a call.
		//
		// TODO: We could narrow this down if the compiler
		// produced a single map per function of stack slots
		// and registers that ever contain a pointer.
		if frame.varp != 0 {
			size := frame.varp - frame.sp
			if size > 0 {
				scanConservative(frame.sp, size, nil, gcw, state)
			}
		}

		// Scan arguments to this frame.
		if n := frame.argBytes(); n != 0 {
			// TODO: We could pass the entry argument map
			// to narrow this down further.
			scanConservative(frame.argp, n, nil, gcw, state)
		}

		if isAsyncPreempt || isDebugCall {
			// This function's frame contained the
			// registers for the asynchronously stopped
			// parent frame. Scan the parent
			// conservatively.
			state.conservative = true
		} else {
			// We only wanted to scan those two frames
			// conservatively. Clear the flag for future
			// frames.
			state.conservative = false
		}
		return
	}

	locals, args, objs := frame.getStackMap(&state.cache, false)

	// Scan local variables if stack frame has been allocated.
	if locals.n > 0 {
		size := uintptr(locals.n) * goarch.PtrSize
		scanblock(frame.varp-size, size, locals.bytedata, gcw, state)
	}

	// Scan arguments.
	if args.n > 0 {
		scanblock(frame.argp, uintptr(args.n)*goarch.PtrSize, args.bytedata, gcw, state)
	}

	// Add all stack objects to the stack object list.
	if frame.varp != 0 {
		// varp is 0 for defers, where there are no locals.
		// In that case, there can't be a pointer to its args, either.
		// (And all args would be scanned above anyway.)
		for i := range objs {
			obj := &objs[i]
			off := obj.off
			base := frame.varp // locals base pointer
			if off >= 0 {
				base = frame.argp // arguments and return values base pointer
			}
			ptr := base + uintptr(off)
			if ptr < frame.sp {
				// object hasn't been allocated in the frame yet.
				continue
			}
			if stackTraceDebug {
				println("stkobj at", hex(ptr), "of size", obj.size)
			}
			state.addObject(ptr, obj)
		}
	}
}

type gcDrainFlags int

const (
	gcDrainUntilPreempt gcDrainFlags = 1 << iota
	gcDrainFlushBgCredit
	gcDrainIdle
	gcDrainFractional
)

// gcDrain 函数是在并发标记阶段执行的, 主要目的是执行扫描工作，即黑化灰色对象。
// 直到无法获得更多的工作。它可能在 GC 完成之前就返回；
// 调用者需要平衡来自其他处理器（P）的工作。
//
// 如果 flags & gcDrainUntilPreempt != 0，则 gcDrain 在 g.preempt 被设置时返回。
//
// 如果 flags & gcDrainIdle != 0，则 gcDrain 在有其他工作要做时返回。
//
// 如果 flags & gcDrainFractional != 0，则 gcDrain 在 pollFractionalWorkerExit() 返回 true 时自我抢占。
// 这意味着 gcDrainNoBlock。
//
// 如果 flags & gcDrainFlushBgCredit != 0，则 gcDrain 每隔 gcCreditSlack 单位的扫描工作就将扫描工作信用
// 冲刷到 gcController.bgScanCredit。
//
// gcDrain 总是在有待定的停止世界（STW）操作时返回。
//
//go:nowritebarrier
func gcDrain(gcw *gcWork, flags gcDrainFlags) {
	// 如果写屏障不需要，则抛出错误，因为这表明垃圾回收阶段不正确
	if !writeBarrier.needed {
		throw("gcDrain phase incorrect")
	}

	gp := getg().m.curg                              // 获取当前正在运行的 goroutine。
	preemptible := flags&gcDrainUntilPreempt != 0    // 如果设置了 gcDrainUntilPreempt 标志，则在当前 goroutine 被抢占时返回。
	flushBgCredit := flags&gcDrainFlushBgCredit != 0 // 如果设置了 gcDrainFlushBgCredit 标志，则定期将扫描工作信用冲刷到全局账户。
	idle := flags&gcDrainIdle != 0                   // 如果设置了 gcDrainIdle 标志，则在有其他工作可以做时返回。

	initScanWork := gcw.heapScanWork // 记录初始扫描工作量。

	// checkWork 表示在执行下一次自我抢占检查之前的扫描工作量。
	// 初始值设为一个大数，表示首次检查将在执行大量工作之后。
	checkWork := int64(1<<63 - 1) // 初始值为最大整数值减一。
	var check func() bool         // 声明检查函数。

	// 如果设置了 gcDrainIdle 或 gcDrainFractional 标志，则设置检查工作量和检查函数。
	if flags&(gcDrainIdle|gcDrainFractional) != 0 {
		// 将初始扫描工作量加上阈值作为新的检查工作量。
		checkWork = initScanWork + drainCheckThreshold
		// 根据标志设置检查函数。
		if idle {
			// 如果设置了 gcDrainIdle 标志，则使用 pollWork 函数检查是否有其他工作可以做。
			check = pollWork
		} else if flags&gcDrainFractional != 0 {
			// 如果设置了 gcDrainFractional 标志，则使用 pollFractionalWorkerExit 函数检查是否应自我抢占。
			check = pollFractionalWorkerExit
		}
	}

	// 如果仍有未完成的根对象标记任务。
	if work.markrootNext < work.markrootJobs {
		// 如果可抢占或者有人想要执行 stw（停止世界）操作，则停止。
		for !(gp.preempt && (preemptible || sched.gcwaiting.Load())) {
			// 原子增加 work.markrootNext 并获取下一个任务的索引。
			job := atomic.Xadd(&work.markrootNext, +1) - 1
			// 如果所有根集标记任务已完成，则跳出循环。
			if job >= work.markrootJobs {
				break
			}

			markroot(gcw, job, flushBgCredit) // 扫描第 job 个根对象, 将根对象标记会灰色。

			// 如果设置了检查函数并且检查函数返回 true，则提前结束循环。
			if check != nil && check() {
				goto done
			}
		}
	}

	// 循环处理堆中的对象，直到无法获取更多工作为止。
	for !(gp.preempt && (preemptible || sched.gcwaiting.Load())) {
		// 如果 work.full == 0，则调用 gcw.balance() 来平衡工作负载
		if work.full == 0 {
			gcw.balance()
		}

		b := gcw.tryGetFast() // 尝试快速获取一个对象 b。
		if b == 0 {
			b = gcw.tryGet() // 如果快速获取失败，则尝试正常获取。
			if b == 0 {
				// 将当前 P 的写屏障缓冲区刷新到垃圾回收的工作缓冲区
				wbBufFlush() // 如果获取失败，刷新写屏障缓冲区并再次尝试获取。
				b = gcw.tryGet()
			}
		}
		if b == 0 {
			break
		}

		scanobject(b, gcw) // 扫描对象, 即黑化灰色对象

		// 如果本地积累的扫描工作信用足够多，则将其冲刷到全局账户，
		// 以便协程协助可以使用它。
		if gcw.heapScanWork >= gcCreditSlack {
			gcController.heapScanWork.Add(gcw.heapScanWork) // 将本地扫描工作信用添加到全局账户。
			if flushBgCredit {
				gcFlushBgCredit(gcw.heapScanWork - initScanWork) // 冲刷扫描工作信用到全局账户。
				initScanWork = 0
			}
			checkWork -= gcw.heapScanWork
			gcw.heapScanWork = 0

			if checkWork <= 0 {
				checkWork += drainCheckThreshold // 重置检查工作量。
				// 如果检查函数返回 true，则跳出循环。
				if check != nil && check() {
					break
				}
			}
		}
	}

done:
	// 冲刷剩余的扫描工作信用。
	if gcw.heapScanWork > 0 {
		gcController.heapScanWork.Add(gcw.heapScanWork)
		if flushBgCredit {
			gcFlushBgCredit(gcw.heapScanWork - initScanWork)
		}
		gcw.heapScanWork = 0
	}
}

// gcDrainN 主要目的是执行扫描工作，即黑化灰色对象，直到执行了大约 scanWork 单位的扫描工作或 goroutine 被抢占。
// 这是一个尽力而为的操作，因此如果无法获得工作缓冲区，则可能执行较少的工作。
// 否则，它将至少执行 n 单位的工作，但由于扫描总是以整个对象为单位进行，因此可能会执行更多的工作。
// 它返回执行的扫描工作量。
//
// 调用方 goroutine 必须处于可抢占状态（例如，_Gwaiting）以防止在堆栈扫描期间发生死锁。
// 因此，这必须在系统堆栈上调用。
//
// - gcw 是一个指向 gcWork 结构的指针，用于管理垃圾回收工作的队列。
// - scanWork 是一个 int64 类型的整数，表示要执行的扫描工作单位的数量。
//
//go:nowritebarrier
//go:systemstack
func gcDrainN(gcw *gcWork, scanWork int64) int64 {
	// 如果写屏障不需要，则抛出错误，因为这表明垃圾回收阶段不正确
	if !writeBarrier.needed {
		throw("gcDrainN phase incorrect")
	}

	// 在 gcw 中可能已经有了一些扫描工作，我们不希望把这些工作计入本次调用。
	workFlushed := -gcw.heapScanWork

	// 获取一个 goroutine
	gp := getg().m.curg

	// 循环条件：
	// !gp.preempt：goroutine 未被抢占。
	// !gcCPULimiter.limiting()：GC CPU 限制器未启用。
	// workFlushed + gcw.heapScanWork < scanWork：执行的工作量小于期望的工作量。
	for !gp.preempt && !gcCPULimiter.limiting() && workFlushed+gcw.heapScanWork < scanWork {
		// 如果 work.full == 0，则调用 gcw.balance() 来平衡工作负载
		if work.full == 0 {
			gcw.balance()
		}

		// 获取待扫描对象:

		b := gcw.tryGetFast() // 尝试快速获取一个对象 b。
		if b == 0 {
			b = gcw.tryGet() // 如果快速获取失败，则尝试正常获取。
			if b == 0 {
				// 将当前 P 的写屏障缓冲区刷新到垃圾回收的工作缓冲区
				wbBufFlush() // 如果获取失败，刷新写屏障缓冲区并再次尝试获取。
				b = gcw.tryGet()
			}
		}

		// 如果没有灰色的标记对象, 会触发扫描根对象, 将根对象标记会灰色。
		if b == 0 {
			// 尝试执行一个根集任务。
			if work.markrootNext < work.markrootJobs {
				// 原子增加并获取下一个根集任务索引。
				job := atomic.Xadd(&work.markrootNext, +1) - 1
				if job < work.markrootJobs {
					workFlushed += markroot(gcw, job, false) // 扫描第 i 个根对象, 将根对象标记会灰色。
					continue
				}
			}
			// 没有堆或根集任务。
			break
		}

		scanobject(b, gcw) // 扫描对象, 即黑化灰色对象

		// 刷新后台扫描工作信用。
		if gcw.heapScanWork >= gcCreditSlack {
			gcController.heapScanWork.Add(gcw.heapScanWork)
			workFlushed += gcw.heapScanWork
			gcw.heapScanWork = 0
		}
	}

	// 与 gcDrain 不同，这里不需要刷新剩余的工作，
	// 因为这里永远不会刷新到 bgScanCredit，而 gcw.dispose 会刷新剩余的工作到 scanWork。

	return workFlushed + gcw.heapScanWork
}

// scanblock scans b as scanobject would, but using an explicit
// pointer bitmap instead of the heap bitmap.
//
// This is used to scan non-heap roots, so it does not update
// gcw.bytesMarked or gcw.heapScanWork.
//
// If stk != nil, possible stack pointers are also reported to stk.putPtr.
//
//go:nowritebarrier
func scanblock(b0, n0 uintptr, ptrmask *uint8, gcw *gcWork, stk *stackScanState) {
	// Use local copies of original parameters, so that a stack trace
	// due to one of the throws below shows the original block
	// base and extent.
	b := b0
	n := n0

	for i := uintptr(0); i < n; {
		// Find bits for the next word.
		bits := uint32(*addb(ptrmask, i/(goarch.PtrSize*8)))
		if bits == 0 {
			i += goarch.PtrSize * 8
			continue
		}
		for j := 0; j < 8 && i < n; j++ {
			if bits&1 != 0 {
				// Same work as in scanobject; see comments there.
				p := *(*uintptr)(unsafe.Pointer(b + i))
				if p != 0 {
					if obj, span, objIndex := findObject(p, b, i); obj != 0 {
						greyobject(obj, b, i, span, gcw, objIndex) // 将对象标记为灰色，并将其加入 gcw 的工作队列中。
					} else if stk != nil && p >= stk.stack.lo && p < stk.stack.hi {
						stk.putPtr(p, false)
					}
				}
			}
			bits >>= 1
			i += goarch.PtrSize
		}
	}
}

// 扫描从 b 开始的对象，遍历将下一个子对象标记为灰色, 并将指针添加到 gcw 中。
// b 必须指向堆对象或 oblet 的开始位置。
// scanobject 会查询 GC 位图以获取指针掩码，并查询 span 以获取对象的大小。
//
// - b 是一个 uintptr 类型的指针，表示要扫描的对象的起始地址。
// - gcw 是一个指向 gcWork 结构的指针，用于管理垃圾回收工作的队列。
//
//go:nowritebarrier
func scanobject(b uintptr, gcw *gcWork) {
	// 预取对象，以便在扫描之前进行预加载。
	//
	// 这将重叠获取对象的开头与扫描对象之前的初始设置。
	sys.Prefetch(b)

	// 查找 b 的位图信息和 b 处对象的大小。
	//
	// b 要么是对象的开头，这时就是要扫描的对象的大小；
	// 要么是指向 oblet 的位置，这时我们将在下面计算要扫描的大小。
	s := spanOfUnchecked(b)
	n := s.elemsize
	if n == 0 {
		throw("scanobject n == 0")
	}
	if s.spanclass.noscan() {
		// 从技术上讲这是可以接受的，但如果 noscan 对象到达这里，效率较低。
		throw("scanobject of a noscan object")
	}

	// 如果对象大小超过 maxObletBytes，则将其拆分成 oblets。
	// 如果 b 是对象的起始位置，则将其他 oblets 加入队列以稍后扫描。
	// 计算 oblet 的大小，确保不超过 maxObletBytes。
	if n > maxObletBytes {
		// 大对象。为了更好的并行性和更低的延迟，将其拆分成 oblets。
		if b == s.base() {
			// 将其他 oblets 加入队列以稍后扫描。
			// 有些 oblets 可能在 b 的标量尾部，但这些 oblets 将被标记为 "不再有指针"，
			// 因此当我们去扫描它们时会立即退出。
			for oblet := b + maxObletBytes; oblet < s.base()+s.elemsize; oblet += maxObletBytes {
				if !gcw.putFast(oblet) {
					gcw.put(oblet)
				}
			}
		}

		// 计算 oblet 的大小。因为这个对象一定是大对象，所以 s.base() 是对象的开始位置。
		n = s.base() + s.elemsize - b
		if n > maxObletBytes {
			n = maxObletBytes
		}
	}

	hbits := heapBitsForAddr(b, n)

	// 循环遍历对象中的指针。调用 greyobject 将其标记为灰色
	var scanSize uintptr
	for {
		var addr uintptr
		// 使用 hbits.nextFast 和 hbits.next 获取下一个可能的指针地址 addr。
		if hbits, addr = hbits.nextFast(); addr == 0 {
			if hbits, addr = hbits.next(); addr == 0 {
				break
			}
		}

		// 更新 scanSize 以跟踪最远的指针位置。
		// 记录找到的最远指针的位置，以便更新 heapScanWork。
		// TODO: 是否有更好的指标，现在我们能非常有效地跳过标量部分？
		scanSize = addr - b + goarch.PtrSize

		// 提取指针 obj。
		// 下面的工作在 scanblock 和上面的部分都有重复。
		// 如果你在这里做出更改，请也在那里做出相应的更改。
		obj := *(*uintptr)(unsafe.Pointer(addr))

		// 如果 obj 不是零且不在当前对象内，则测试 obj 是否指向 Go 堆中的对象。
		if obj != 0 && obj-b >= n {
			if obj, span, objIndex := findObject(obj, b, addr-b); obj != 0 {
				greyobject(obj, b, addr-b, span, gcw, objIndex) // 如果 obj 指向堆中的对象，则调用 greyobject 将其标记为灰色
			}
		}
	}

	// 标记为黑色
	gcw.bytesMarked += uint64(n)
	gcw.heapScanWork += int64(scanSize)
}

// scanConservative 它负责保守地扫描给定的内存区间，并将区间内的潜在指针标记为灰色
//
// 如果 ptrmask != nil，则只有在 ptrmask 中被标记的字才被视为潜在的指针。
//
// 如果 state != nil，则假设 [b, b+n) 是堆栈中的一个块，并且可能包含指向堆栈对象的指针。
// - b 是一个 uintptr 类型的指针，表示要扫描的块的起始地址。
// - n 是一个 uintptr 类型的整数，表示要扫描的块的长度。
// - ptrmask 是一个指向 uint8 的指针，表示指针掩码，用于指示哪些字可能包含指针。
// - gcw 是一个指向 gcWork 结构的指针，用于管理垃圾回收工作的队列。
// - state 是一个指向 stackScanState 结构的指针，表示堆栈扫描的状态信息。
func scanConservative(b, n uintptr, ptrmask *uint8, gcw *gcWork, state *stackScanState) {
	// 如果启用了 debugScanConservative，则打印保守扫描的区间，并使用 hexdumpWords 打印区间的内容
	if debugScanConservative {
		printlock()
		print("conservatively scanning [", hex(b), ",", hex(b+n), ")\n")
		hexdumpWords(b, b+n, func(p uintptr) byte {
			if ptrmask != nil {
				word := (p - b) / goarch.PtrSize
				bits := *addb(ptrmask, word/8)
				if (bits>>(word%8))&1 == 0 {
					return '$'
				}
			}

			val := *(*uintptr)(unsafe.Pointer(p))
			if state != nil && state.stack.lo <= val && val < state.stack.hi {
				return '@'
			}

			span := spanOfHeap(val)
			if span == nil {
				return ' '
			}
			idx := span.objIndex(val)
			if span.isFree(idx) {
				return ' '
			}
			return '*'
		})
		printunlock()
	}

	// 使用一个循环来遍历区间 [b, b+n)，每次增加 goarch.PtrSize 字节
	for i := uintptr(0); i < n; i += goarch.PtrSize {
		if ptrmask != nil {
			// 如果提供了 ptrmask，则检查当前字是否被标记
			word := i / goarch.PtrSize
			bits := *addb(ptrmask, word/8)
			if bits == 0 {
				// 跳过 8 个字（循环增量会处理第 8 个字）
				//
				// 这应该是第一次看到这个字的 ptrmask，所以 i
				// 必须是 8 字对齐的，但这里还是检查一下。
				if i%(goarch.PtrSize*8) != 0 {
					throw("misaligned mask")
				}
				i += goarch.PtrSize*8 - goarch.PtrSize
				continue
			}
			if (bits>>(word%8))&1 == 0 {
				continue
			}
		}

		val := *(*uintptr)(unsafe.Pointer(b + i))

		// 检查 val 是否指向堆栈内。
		if state != nil && state.stack.lo <= val && val < state.stack.hi {
			// val 可能指向一个堆栈对象。这个
			// 对象可能在上一个周期中就已经死亡，
			// 因此可能包含指向未分配对象的指针，
			// 但与堆对象不同，我们无法判断它是否已经死亡。
			// 因此，如果所有指向这个对象的指针都是通过
			// 保守扫描找到的，我们必须对其进行防御性扫描。
			state.putPtr(val, true)
			continue
		}

		// 检查 val 是否指向一个堆 span。
		span := spanOfHeap(val)
		if span == nil {
			continue
		}

		// 检查 val 是否指向一个已分配的对象。
		idx := span.objIndex(val)
		if span.isFree(idx) {
			continue
		}

		// val 指向一个已分配的对象。标记它。
		obj := span.base() + idx*span.elemsize
		greyobject(obj, b, i, span, gcw, idx) // 函数来标记对象为灰色，表示该对象正在处理中
	}
}

// Shade the object if it isn't already.
// The object is not nil and known to be in the heap.
// Preemption must be disabled.
//
//go:nowritebarrier
func shade(b uintptr) {
	if obj, span, objIndex := findObject(b, 0, 0); obj != 0 {
		gcw := &getg().m.p.ptr().gcw
		greyobject(obj, 0, 0, span, gcw, objIndex)
	}
}

// 将对象标记为灰色，并将其加入 gcw 的工作队列中。
// - obj 是一个 uintptr 类型的指针，表示要标记的对象的起始地址。
// - base 是一个 uintptr 类型的指针，表示对象所在内存块的基地址，仅用于调试。
// - off 是一个 uintptr 类型的整数，表示对象相对于 base 的偏移量，仅用于调试。
// - span 是一个指向 mspan 结构的指针，表示对象所在的 span。
// - gcw 是一个指向 gcWork 结构的指针，用于管理垃圾回收工作的队列。
// - objIndex 是一个 uintptr 类型的整数，表示对象在 span 中的索引。
//
// 请参考 wbBufFlush1，它部分复制了此函数的逻辑。
//
//go:nowritebarrierrec
func greyobject(obj, base, off uintptr, span *mspan, gcw *gcWork, objIndex uintptr) {
	// obj 应该是分配的起始位置，因此必须至少是指针对齐的。
	if obj&(goarch.PtrSize-1) != 0 {
		throw("greyobject: obj not pointer-aligned")
	}
	mbits := span.markBitsForIndex(objIndex)

	if useCheckmark {
		if setCheckmark(obj, base, off, mbits) {
			// 已经标记过。
			return
		}
	} else {
		if debug.gccheckmark > 0 && span.isFree(objIndex) {
			print("runtime: marking free object ", hex(obj), " found at *(", hex(base), "+", hex(off), ")\n")
			gcDumpObject("base", base, off)
			gcDumpObject("obj", obj, ^uintptr(0))
			getg().m.traceback = 2
			throw("marking free object")
		}

		// 如果已经标记，就没有事情要做。
		if mbits.isMarked() {
			return
		}
		mbits.setMarked()

		// 标记 span。
		arena, pageIdx, pageMask := pageIndexOf(span.base())
		if arena.pageMarks[pageIdx]&pageMask == 0 {
			atomic.Or8(&arena.pageMarks[pageIdx], pageMask)
		}

		// 如果这是一个不需要扫描的对象，快速将其标记为黑色，
		// 而不是将其标记为灰色。
		if span.spanclass.noscan() {
			gcw.bytesMarked += uint64(span.elemsize)
			return
		}
	}

	// 我们正在将 obj 添加到 P 的本地工作队列中，因此很可能
	// 这个对象很快就会被同一个 P 处理。
	// 即使工作队列被刷新，对于具有包容共享缓存的平台而言，
	// 仍然可能会有一些好处。
	sys.Prefetch(obj)
	// 将 obj 排队以供扫描。
	if !gcw.putFast(obj) {
		gcw.put(obj)
	}
}

// gcDumpObject dumps the contents of obj for debugging and marks the
// field at byte offset off in obj.
func gcDumpObject(label string, obj, off uintptr) {
	s := spanOf(obj)
	print(label, "=", hex(obj))
	if s == nil {
		print(" s=nil\n")
		return
	}
	print(" s.base()=", hex(s.base()), " s.limit=", hex(s.limit), " s.spanclass=", s.spanclass, " s.elemsize=", s.elemsize, " s.state=")
	if state := s.state.get(); 0 <= state && int(state) < len(mSpanStateNames) {
		print(mSpanStateNames[state], "\n")
	} else {
		print("unknown(", state, ")\n")
	}

	skipped := false
	size := s.elemsize
	if s.state.get() == mSpanManual && size == 0 {
		// We're printing something from a stack frame. We
		// don't know how big it is, so just show up to an
		// including off.
		size = off + goarch.PtrSize
	}
	for i := uintptr(0); i < size; i += goarch.PtrSize {
		// For big objects, just print the beginning (because
		// that usually hints at the object's type) and the
		// fields around off.
		if !(i < 128*goarch.PtrSize || off-16*goarch.PtrSize < i && i < off+16*goarch.PtrSize) {
			skipped = true
			continue
		}
		if skipped {
			print(" ...\n")
			skipped = false
		}
		print(" *(", label, "+", i, ") = ", hex(*(*uintptr)(unsafe.Pointer(obj + i))))
		if i == off {
			print(" <==")
		}
		print("\n")
	}
	if skipped {
		print(" ...\n")
	}
}

// gcmarknewobject 将新分配的对象标记为黑色。obj不能包含任何非零指针。
//
// 这是nospliit，因此它可以在没有抢占的情况下操纵gcWork。
//
//go:nowritebarrier
//go:nosplit
func gcmarknewobject(span *mspan, obj, size uintptr) {
	if useCheckmark { // The world should be stopped so this should not happen.
		throw("gcmarknewobject called while doing checkmark")
	}

	// Mark object.
	objIndex := span.objIndex(obj)
	span.markBitsForIndex(objIndex).setMarked()

	// Mark span.
	arena, pageIdx, pageMask := pageIndexOf(span.base())
	if arena.pageMarks[pageIdx]&pageMask == 0 {
		atomic.Or8(&arena.pageMarks[pageIdx], pageMask)
	}

	gcw := &getg().m.p.ptr().gcw
	gcw.bytesMarked += uint64(size)
}

// gcMarkTinyAllocs greys all active tiny alloc blocks.
//
// The world must be stopped.
func gcMarkTinyAllocs() {
	assertWorldStopped()

	for _, p := range allp {
		c := p.mcache
		if c == nil || c.tiny == 0 {
			continue
		}
		_, span, objIndex := findObject(c.tiny, 0, 0)
		gcw := &p.gcw
		greyobject(c.tiny, 0, 0, span, gcw, objIndex)
	}
}
