// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Garbage collector (GC).
//
// The GC runs concurrently with mutator threads, is type accurate (aka precise), allows multiple
// GC thread to run in parallel. It is a concurrent mark and sweep that uses a write barrier. It is
// non-generational and non-compacting. Allocation is done using size segregated per P allocation
// areas to minimize fragmentation while eliminating locks in the common case.
//
// The algorithm decomposes into several steps.
// This is a high level description of the algorithm being used. For an overview of GC a good
// place to start is Richard Jones' gchandbook.org.
//
// The algorithm's intellectual heritage includes Dijkstra's on-the-fly algorithm, see
// Edsger W. Dijkstra, Leslie Lamport, A. J. Martin, C. S. Scholten, and E. F. M. Steffens. 1978.
// On-the-fly garbage collection: an exercise in cooperation. Commun. ACM 21, 11 (November 1978),
// 966-975.
// For journal quality proofs that these steps are complete, correct, and terminate see
// Hudson, R., and Moss, J.E.B. Copying Garbage Collection without stopping the world.
// Concurrency and Computation: Practice and Experience 15(3-5), 2003.
//
// 1. GC performs sweep termination.
//
//    a. Stop the world. This causes all Ps to reach a GC safe-point.
//
//    b. Sweep any unswept spans. There will only be unswept spans if
//    this GC cycle was forced before the expected time.
//
// 2. GC performs the mark phase.
//
//    a. Prepare for the mark phase by setting gcphase to _GCmark
//    (from _GCoff), enabling the write barrier, enabling mutator
//    assists, and enqueueing root mark jobs. No objects may be
//    scanned until all Ps have enabled the write barrier, which is
//    accomplished using STW.
//
//    b. Start the world. From this point, GC work is done by mark
//    workers started by the scheduler and by assists performed as
//    part of allocation. The write barrier shades both the
//    overwritten pointer and the new pointer value for any pointer
//    writes (see mbarrier.go for details). Newly allocated objects
//    are immediately marked black.
//
//    c. GC performs root marking jobs. This includes scanning all
//    stacks, shading all globals, and shading any heap pointers in
//    off-heap runtime data structures. Scanning a stack stops a
//    goroutine, shades any pointers found on its stack, and then
//    resumes the goroutine.
//
//    d. GC drains the work queue of grey objects, scanning each grey
//    object to black and shading all pointers found in the object
//    (which in turn may add those pointers to the work queue).
//
//    e. Because GC work is spread across local caches, GC uses a
//    distributed termination algorithm to detect when there are no
//    more root marking jobs or grey objects (see gcMarkDone). At this
//    point, GC transitions to mark termination.
//
// 3. GC performs mark termination.
//
//    a. Stop the world.
//
//    b. Set gcphase to _GCmarktermination, and disable workers and
//    assists.
//
//    c. Perform housekeeping like flushing mcaches.
//
// 4. GC performs the sweep phase.
//
//    a. Prepare for the sweep phase by setting gcphase to _GCoff,
//    setting up sweep state and disabling the write barrier.
//
//    b. Start the world. From this point on, newly allocated objects
//    are white, and allocating sweeps spans before use if necessary.
//
//    c. GC does concurrent sweeping in the background and in response
//    to allocation. See description below.
//
// 5. When sufficient allocation has taken place, replay the sequence
// starting with 1 above. See discussion of GC rate below.

// Concurrent sweep.
//
// The sweep phase proceeds concurrently with normal program execution.
// The heap is swept span-by-span both lazily (when a goroutine needs another span)
// and concurrently in a background goroutine (this helps programs that are not CPU bound).
// At the end of STW mark termination all spans are marked as "needs sweeping".
//
// The background sweeper goroutine simply sweeps spans one-by-one.
//
// To avoid requesting more OS memory while there are unswept spans, when a
// goroutine needs another span, it first attempts to reclaim that much memory
// by sweeping. When a goroutine needs to allocate a new small-object span, it
// sweeps small-object spans for the same object size until it frees at least
// one object. When a goroutine needs to allocate large-object span from heap,
// it sweeps spans until it frees at least that many pages into heap. There is
// one case where this may not suffice: if a goroutine sweeps and frees two
// nonadjacent one-page spans to the heap, it will allocate a new two-page
// span, but there can still be other one-page unswept spans which could be
// combined into a two-page span.
//
// It's critical to ensure that no operations proceed on unswept spans (that would corrupt
// mark bits in GC bitmap). During GC all mcaches are flushed into the central cache,
// so they are empty. When a goroutine grabs a new span into mcache, it sweeps it.
// When a goroutine explicitly frees an object or sets a finalizer, it ensures that
// the span is swept (either by sweeping it, or by waiting for the concurrent sweep to finish).
// The finalizer goroutine is kicked off only when all spans are swept.
// When the next GC starts, it sweeps all not-yet-swept spans (if any).

// GC rate.
// Next GC is after we've allocated an extra amount of memory proportional to
// the amount already in use. The proportion is controlled by GOGC environment variable
// (100 by default). If GOGC=100 and we're using 4M, we'll GC again when we get to 8M
// (this mark is computed by the gcController.heapGoal method). This keeps the GC cost in
// linear proportion to the allocation cost. Adjusting GOGC just changes the linear constant
// (and also the amount of extra memory used).

// Oblets
//
// In order to prevent long pauses while scanning large objects and to
// improve parallelism, the garbage collector breaks up scan jobs for
// objects larger than maxObletBytes into "oblets" of at most
// maxObletBytes. When scanning encounters the beginning of a large
// object, it scans only the first oblet and enqueues the remaining
// oblets as new scan jobs.

package runtime

import (
	"internal/cpu"
	"runtime/internal/atomic"
	"unsafe"
)

const (
	// 默认情况下，它的值为 0，表示不启用调试模式
	_DebugGC              = 0           // 当 _DebugGC 的值不为 0 时，垃圾回收器将采用调试模式下的特殊行为
	_ConcurrentSweep      = true        // 当 _ConcurrentSweep 的值为 true 时，垃圾回收器将在清扫阶段尝试并发执行清扫工作，以减少暂停时间
	_FinBlockSize         = 4 * 1024    // 细粒度块是在某些场景下使用的较小的内存块，例如用于分配较小的对象(4kb)
	debugScanConservative = false       // 开启调试日志记录，针对保守扫描的堆栈帧
	sweepMinHeapDistance  = 1024 * 1024 // 保留一定的内存区域(1MB)用于并发清扫，以减少清扫阶段的暂停时间
)

// heapObjectsCanMove always returns false in the current garbage collector.
// It exists for go4.org/unsafe/assume-no-moving-gc, which is an
// unfortunate idea that had an even more unfortunate implementation.
// Every time a new Go release happened, the package stopped building,
// and the authors had to add a new file with a new //go:build line, and
// then the entire ecosystem of packages with that as a dependency had to
// explicitly update to the new version. Many packages depend on
// assume-no-moving-gc transitively, through paths like
// inet.af/netaddr -> go4.org/intern -> assume-no-moving-gc.
// This was causing a significant amount of friction around each new
// release, so we added this bool for the package to //go:linkname
// instead. The bool is still unfortunate, but it's not as bad as
// breaking the ecosystem on every new release.
//
// If the Go garbage collector ever does move heap objects, we can set
// this to true to break all the programs using assume-no-moving-gc.
//
//go:linkname heapObjectsCanMove
func heapObjectsCanMove() bool {
	return false
}

// 函数初始化 Go 语言的垃圾回收系统。
func gcinit() {
	// 确保了 workbuf 的大小与预定义的大小相匹配，以保证最佳性能
	if unsafe.Sizeof(workbuf{}) != _WorkbufSize {
		throw("size of Workbuf is suboptimal")
	}

	// 设置清扫状态: 第一个垃圾回收周期不需要清扫。
	// state 被分为两部分。
	//
	// 最高位（由 sweepDrainedMask 掩码屏蔽）是一个布尔值，表示清扫队列中的所有清扫工作是否已被清空。
	// 其余的位是一个计数器，表示正在进行的并发清扫器的数量。
	sweep.active.state.Store(sweepDrainedMask)

	// 初始化垃圾回收步调状态:
	// 使用环境变量 GOGC 来设置初始的 gcPercent 值, 使用百分比控制垃圾回收的频率。
	// 使用环境变量 GOMEMLIMIT 来设置初始的 memoryLimit 值, 运行时的最大堆内存限制。
	gcController.init(readGOGC(), readGOMEMLIMIT())

	// 初始化工作信号量。
	work.startSema = 1
	work.markDoneSema = 1

	// 初始化锁。
	lockInit(&work.sweepWaiters.lock, lockRankSweepWaiters) // 用于同步清扫等待队列
	lockInit(&work.assistQueue.lock, lockRankAssistQueue)   // 用于同步协助队列
	lockInit(&work.wbufSpans.lock, lockRankWbufSpans)       // 用于同步工作缓冲区跨度
}

// 函数在运行时初始化的大部分工作完成之后被调用，
// 在即将开始让用户代码运行之前。
// 它启动了后台清扫 goroutine、后台 scavenger goroutine，并启用了垃圾回收。
func gcenable() {
	// 启动清扫和 scavenger。
	c := make(chan int, 2) // 创建一个容量为 2 的通道。

	go bgsweep(c)    // 启动后台清扫 goroutine。负责清扫（sweep）垃圾回收器标记为不可达的对象所在的 span（内存块）
	go bgscavenge(c) // 启动后台 scavenger goroutine。

	<-c // 等待清扫 goroutine 准备完成发送信号。
	<-c // 等待 scavenger goroutine 准备完成发送信号。

	memstats.enablegc = true // 现在运行时已经初始化，垃圾回收可以开始了。
}

// Garbage collector phase.
// Indicates to write barrier and synchronization task to perform.
var gcphase uint32

// The compiler knows about this variable.
// If you change it, you must change builtin/runtime.go, too.
// If you change the first four bytes, you must also change the write
// barrier insertion code.
var writeBarrier struct {
	enabled bool    // compiler emits a check of this before calling write barrier
	pad     [3]byte // compiler uses 32-bit load for "enabled" field
	needed  bool    // identical to enabled, for now (TODO: dedup)
	alignme uint64  // guarantee alignment so that compiler can use a 32 or 64-bit load
}

// gcBlackenEnabled 如果允许 Mutator 协助和背景标记工作者将对象涂黑，则为 1。这只能在 gcphase == _GCmark时设置。
var gcBlackenEnabled uint32

const (
	_GCoff             = iota // 表示垃圾回收器未运行的状态，此时背景清扫正在执行，写屏障被禁用
	_GCmark                   // 表示垃圾回收器正在进行标记阶段，此时新分配的对象会被标记为黑色，写屏障被启用。
	_GCmarktermination        // 表示垃圾回收器的标记终止阶段，此时处理器会帮助完成标记过程，写屏障仍然被启用。
)

// 函数用于设置当前的垃圾回收阶段，并根据垃圾回收阶段调整写屏障的启用状态。
// 如果当前阶段是标记阶段 _GCmark 或标记终止阶段 _GCmarktermination ，则需要启用写屏障
// 参数:
//
//	x - 设置的新垃圾回收阶段。
//
// 此函数会根据垃圾回收阶段来决定是否需要启用写屏障。
// 写屏障在标记阶段（_GCmark）和标记终止阶段（_GCmarktermination）是必需的，
// 因为在这两个阶段需要记录对象之间的引用关系。
//
//go:nosplit
func setGCPhase(x uint32) {
	// 使用原子操作设置当前的垃圾回收阶段。
	atomic.Store(&gcphase, x)

	// 如果当前阶段是标记阶段 _GCmark 或标记终止阶段 _GCmarktermination ，则需要启用写屏障
	writeBarrier.needed = gcphase == _GCmark || gcphase == _GCmarktermination
	writeBarrier.enabled = writeBarrier.needed
}

// gcMarkWorkerMode represents the mode that a concurrent mark worker
// should operate in.
//
// Concurrent marking happens through four different mechanisms. One
// is mutator assists, which happen in response to allocations and are
// not scheduled. The other three are variations in the per-P mark
// workers and are distinguished by gcMarkWorkerMode.
type gcMarkWorkerMode int

const (
	// 表示下一个被调度的 G 不会开始工作，模式应该被忽略。
	gcMarkWorkerNotWorker gcMarkWorkerMode = iota

	// 表示一个 P 专门用于运行标记工作 goroutine。
	// 标记工作 goroutine 应该不间断地运行。
	gcMarkWorkerDedicatedMode

	// 表示一个 P 当前正在运行“分割”标记工作 goroutine。
	// 分割工作 goroutine 是必要的，当 GOMAXPROCS*gcBackgroundUtilization 不是一个整数时，
	// 仅使用专用工作 goroutine 会导致利用率偏离目标 gcBackgroundUtilization。
	// 分割工作 goroutine 应该运行直到被抢占，并被调度以承担 GOMAXPROCS*gcBackgroundUtilization 的非整数部分。
	gcMarkWorkerFractionalMode

	// 表示一个 P 正在运行标记工作 goroutine，因为它没有其他事情可做。
	// 空闲工作 goroutine 应该运行直到被抢占，并将其时间计入 gcController.idleMarkTime。
	gcMarkWorkerIdleMode
)

// gcMarkWorkerModeStrings are the strings labels of gcMarkWorkerModes
// to use in execution traces.
var gcMarkWorkerModeStrings = [...]string{
	"Not worker",
	"GC (dedicated)",
	"GC (fractional)",
	"GC (idle)",
}

// pollFractionalWorkerExit reports whether a fractional mark worker
// should self-preempt. It assumes it is called from the fractional
// worker.
func pollFractionalWorkerExit() bool {
	// This should be kept in sync with the fractional worker
	// scheduler logic in findRunnableGCWorker.
	now := nanotime()
	delta := now - gcController.markStartTime
	if delta <= 0 {
		return true
	}
	p := getg().m.p.ptr()
	selfTime := p.gcFractionalMarkTime + (now - p.gcMarkWorkerStartTime)
	// Add some slack to the utilization goal so that the
	// fractional worker isn't behind again the instant it exits.
	return float64(selfTime)/float64(delta) > 1.2*gcController.fractionalUtilizationGoal
}

var work workType

type workType struct {
	full  lfstack          // 垃圾回收的全局缓冲区, 这个变量如果为空, 则标志并发标记阶段已经完成所有对象标记
	_     cpu.CacheLinePad // 防止满空和空之间错误共享
	empty lfstack          // lock-free list of empty blocks workbuf
	_     cpu.CacheLinePad // 防止 Empty 和 nproc/nwait 之间的错误共享

	wbufSpans struct {
		lock mutex
		// free is a list of spans dedicated to workbufs, but
		// that don't currently contain any workbufs.
		free mSpanList
		// busy is a list of all spans containing workbufs on
		// one of the workbuf lists.
		busy mSpanList
	}

	// Restore 64-bit alignment on 32-bit.
	_ uint32

	// bytesMarked is the number of bytes marked this cycle. This
	// includes bytes blackened in scanned objects, noscan objects
	// that go straight to black, and permagrey objects scanned by
	// markroot during the concurrent scan phase. This is updated
	// atomically during the cycle. Updates may be batched
	// arbitrarily, since the value is only read at the end of the
	// cycle.
	//
	// Because of benign races during marking, this number may not
	// be the exact number of marked bytes, but it should be very
	// close.
	//
	// Put this field here because it needs 64-bit atomic access
	// (and thus 8-byte alignment even on 32-bit architectures).
	bytesMarked uint64

	markrootNext uint32 // 表示下一个要执行的根对象标记任务的索引。
	markrootJobs uint32 // 表示总共需要执行的根对象标记任务的数量。

	nproc  uint32
	tstart int64
	nwait  uint32

	// nDataRoots 表示数据段根对象的数量。
	// nBSSRoots 表示 BSS 段根对象的数量。
	// nSpanRoots 表示 span 根对象的数量。
	// nStackRoots 表示栈根对象的数量。
	// 这些值由 gcMarkRootPrepare 函数设置，并用于跟踪每种根类型的数量。
	nDataRoots, nBSSRoots, nSpanRoots, nStackRoots int

	// baseData 表示数据段根对象的起始索引。
	// baseBSS 表示 BSS 段根对象的起始索引。
	// baseSpans 表示 span 根对象的起始索引。
	// baseStacks 表示栈根对象的起始索引。
	// baseEnd 表示所有根对象的结束索引。
	// 这些值由 gcMarkRootPrepare 函数设置，并用于确定各种根类型在根扫描队列中的位置。
	baseData, baseBSS, baseSpans, baseStacks, baseEnd uint32

	// stackRoots 是一个快照，包含了所有在并发标记开始之前存在的 goroutine。
	// 这个变量的底层存储不能被修改，因为它可能与其他 goroutine 共享。
	stackRoots []*g

	// 每种类型的垃圾回收状态转换都受到一个锁的保护。
	// 由于多个线程可以同时检测到状态转换条件，任何检测到转换条件的线程
	// 必须获取适当的转换锁，重新检查转换条件，并在条件不再成立时返回，
	// 或者在条件成立时执行状态转换。
	// 同样，任何状态转换必须在释放锁之前使转换条件失效。
	// 这样可以确保每个状态转换只由一个线程执行，并且需要状态转换发生的线程
	// 会阻塞直到状态转换完成。
	// startSema 保护从 "off" 状态到 "mark" 状态或 "mark termination" 状态的转换。
	// "off" 状态表示垃圾回收未激活。
	// "mark" 状态表示垃圾回收的标记阶段正在进行。
	// "mark termination" 状态表示标记阶段即将结束。
	startSema uint32
	// markDoneSema protects transitions from mark to mark termination.
	markDoneSema uint32

	bgMarkReady note   // signal background mark worker has started
	bgMarkDone  uint32 // cas to 1 when at a background mark completion point
	// Background mark completion signaling

	// mode is the concurrency mode of the current GC cycle.
	mode gcMode

	// userForced indicates the current GC cycle was forced by an
	// explicit user call.
	userForced bool

	// initialHeapLive is the value of gcController.heapLive at the
	// beginning of this GC cycle.
	initialHeapLive uint64

	// assistQueue is a queue of assists that are blocked because
	// there was neither enough credit to steal or enough work to
	// do.
	assistQueue struct {
		lock mutex
		q    gQueue
	}

	// sweepWaiters 是当我们从标记终止过渡到扫描时要唤醒的被阻止的 goroutines 的列表。
	sweepWaiters struct {
		lock mutex
		list gList
	}

	// cycles is the number of completed GC cycles, where a GC
	// cycle is sweep termination, mark, mark termination, and
	// sweep. This differs from memstats.numgc, which is
	// incremented at mark termination.
	cycles atomic.Uint32

	// Timing/utilization stats for this cycle.
	stwprocs, maxprocs                 int32
	tSweepTerm, tMark, tMarkTerm, tEnd int64 // nanotime() of phase start

	pauseNS    int64 // total STW time this cycle
	pauseStart int64 // nanotime() of last STW

	// debug.gctrace heap sizes for this cycle.
	heap0, heap1, heap2 uint64

	// Cumulative estimated CPU usage.
	cpuStats
}

// GC 函数运行一次垃圾回收，并阻塞调用者直到垃圾回收完成。
// 该函数可能会阻塞整个程序。
func GC() {
	// 我们认为一个完整的 GC 周期包括：扫尾终止、标记、标记终止和清扫。
	// 这个函数不应该返回，直到一个完整的 GC 周期从开始到结束都已完成。
	// 因此，我们总是要完成当前的周期并开始一个新的周期。这意味着：
	//
	// 1. 在清扫终止、标记或标记终止阶段 N，等待直到标记终止 N 完成并过渡到清扫阶段 N。
	//
	// 2. 在清扫阶段 N，帮助完成清扫阶段 N。
	//
	// 到这一点，我们可以开始一个新的完整周期 N+1。
	//
	// 3. 通过开始清扫终止 N+1 来触发周期 N+1。
	//
	// 4. 等待标记终止 N+1 完成。
	//
	// 5. 帮助完成清扫阶段 N+1 直到它完成。
	//
	// 这一切都必须考虑到垃圾回收可能自行前进的事实。例如，当我们阻塞直到标记终止 N 完成时，
	// 我们可能醒来时已经在周期 N+2 中了。

	// 等待当前的清扫终止、标记和标记终止完成。
	n := work.cycles.Load() // 获取当前的周期数。
	gcWaitOnMark(n)         // 等待当前周期的标记终止完成。

	// 触发新的 GC 周期: gcTriggerCycle: 手动GC标识
	gcStart(gcTrigger{kind: gcTriggerCycle, n: n + 1}) // 手动触发一个新的 GC 周期 N+1，其中 n 是当前周期数。
	gcWaitOnMark(n + 1)                                // 等待新的周期 N+1 的标记终止完成

	// 完成清扫阶段 N+1:
	// 循环清扫: 使用 for 循环和 sweepone 函数来帮助完成清扫阶段 N+1。
	// 检查周期数: 使用 work.cycles.Load() 来检查当前是否仍处于周期 N+1。
	for work.cycles.Load() == n+1 && sweepone() != ^uintptr(0) {
		sweep.nbgsweep++
		Gosched() // 出 CPU 时间片，以便其他 goroutine 可以运行
	}

	// 使用 for 循环和 isSweepDone 函数来等待清扫完成
	for work.cycles.Load() == n+1 && !isSweepDone() {
		Gosched() // 出 CPU 时间片，以便其他 goroutine 可以运行
	}

	// 现在我们真的完成了清扫，所以我们可以发布稳定的堆配置文件。只有在我们还没有到达另一个标记终止时才这样做。3

	// 获取内存锁
	mp := acquirem()

	cycle := work.cycles.Load() // 检查当前是否仍处于周期 N+1 或者处于下一个周期的标记阶段
	if cycle == n+1 || (gcphase == _GCmark && cycle == n+2) {
		mProf_PostSweep() // 记录最后一次标记终止时的堆配置文件快照
	}

	// 释放内存锁
	releasem(mp)
}

// 函数用于阻塞直到垃圾回收器完成指定编号的标记阶段。
// 如果垃圾回收器已经完成了指定的标记阶段，则立即返回。
//
// 参数:
//
//	n - 要等待完成的标记阶段的编号。
//
// 此函数会一直等待，直到垃圾回收器完成了指定编号的标记阶段。
// 如果垃圾回收器已经完成了指定的标记阶段，则直接返回。
func gcWaitOnMark(n uint32) {
	for {
		// 禁止阶段转换。
		lock(&work.sweepWaiters.lock) // 锁定 sweepWaiters 的锁

		// 获取当前已完成的标记周期数量。
		nMarks := work.cycles.Load()

		// 如果当前不在标记阶段，则表示当前周期的标记阶段已经完成。
		if gcphase != _GCmark {
			// We've already completed this cycle's mark.
			nMarks++
		}

		// 检查是否已经完成了指定编号的标记阶段。
		if nMarks > n {
			// 已经完成指定编号的标记阶段。
			unlock(&work.sweepWaiters.lock) // 解锁 sweepWaiters 的锁
			return
		}

		// 等待直到指定编号的标记周期完成。
		// 将当前 goroutine 添加到 sweepWaiters 的等待列表中。
		// sweepWaiters 是当我们从标记终止过渡到扫描时要唤醒的被阻止的 goroutines 的列表。
		work.sweepWaiters.list.push(getg())

		// 等待直到标记周期完成。
		goparkunlock(&work.sweepWaiters.lock, waitReasonWaitForGCCycle, traceBlockUntilGCEnds, 1)
	}
}

// gcMode indicates how concurrent a GC cycle should be.
type gcMode int

const (
	// 表示垃圾回收器在后台并发执行的状态。
	// 在这种模式下，垃圾回收和清扫操作可以在程序执行的同时并发地进行。
	// 这种模式尽量减少了程序暂停的时间，提高了程序的响应性。
	// 当垃圾回收器检测到内存使用率较高时，它会自动进入这种模式。
	gcBackgroundMode gcMode = iota // 并发的垃圾回收和清扫

	// 表示强制进行停止世界的垃圾回收，同时清扫操作是并发的。
	// 在这种模式下，垃圾回收器会立即执行一次完整的垃圾回收周期，
	// 但是清扫操作仍然是并发进行的，以减少程序的暂停时间。
	// 这种模式通常是由运行时自动触发的，当内存压力达到一定程度时。
	gcForceMode // 立即执行停止世界的垃圾回收，清扫操作并发

	// 表示强制进行停止世界的垃圾回收，清扫也是停止世界的。
	// 在这种模式下，垃圾回收器会立即执行一次完整的垃圾回收周期，
	// 清扫操作也是在程序暂停期间完成的。
	// 这种模式通常是用户显式请求的，例如通过调用 runtime.GC() 函数。
	gcForceBlockMode // 立即执行停止世界的垃圾回收和清扫（由用户强制触发）
)

// A gcTrigger is a predicate for starting a GC cycle. Specifically,
// it is an exit condition for the _GCoff phase.
type gcTrigger struct {
	kind gcTriggerKind
	now  int64  // gcTriggerTime: 当前时间
	n    uint32 // gcTriggerCycle: 要开始的周期数
}

// 是一个枚举类型，用于定义垃圾回收触发的不同条件。
type gcTriggerKind int

const (
	// 表示当堆内存大小达到由控制器计算出的触发堆大小时，应该开始一个新的垃圾回收周期。
	//
	// 这是最常见的触发条件，它基于堆内存的使用情况来决定何时启动 GC。
	// 当堆内存使用量达到一个阈值时，GC 将被触发，以释放不再使用的内存。
	gcTriggerHeap gcTriggerKind = iota

	// 表示当距离上次 GC 的时间超过 forcegcperiod 纳秒时，默认 2 分钟, 应该开始一个新的垃圾回收周期。
	//
	// 这个条件是为了确保不会长时间没有进行 GC，从而避免内存使用率过高。
	// 默认情况下，如果超过 2 分钟没有进行 GC，就会触发强制 GC。
	gcTriggerTime

	// 表示如果尚未开始第 n 个 GC 周期（相对于 work.cycles 来说），应该开始一个新的垃圾回收周期。
	//
	// 在手动触发的 runtime.GC 方法中涉及
	gcTriggerCycle
)

// 根据不同的触发条件来决定是否需要启动一个新的 GC 周期。
func (t gcTrigger) test() bool {
	// 如果 GC 被禁用、当前处于 panic 状态或 GC 阶段不是 _GCoff，则返回 false。
	if !memstats.enablegc || panicking.Load() != 0 || gcphase != _GCoff {
		return false
	}

	// 根据不同的触发条件类型进行检查。
	switch t.kind {
	case gcTriggerHeap:
		trigger, _ := gcController.trigger() // 返回当前应触发垃圾回收的阈值以及堆的目标大小。
		// heapLive: 表示垃圾回收认为还活着的字节数。这是指最近一次垃圾回收保留的字节数加上自那以后分配的字节数。
		return gcController.heapLive.Load() >= trigger // 检查当前活对象的大小是否达到了触发阈值。
	case gcTriggerTime:
		// 如果 GC 的目标利用率百分比小于 0，则返回 false。
		if gcController.gcPercent.Load() < 0 {
			return false
		}
		// 获取上次 GC 的时间戳。
		lastgc := int64(atomic.Load64(&memstats.last_gc_nanotime))
		// 计算当前时间与上次 GC 时间的差值。
		// 如果差值大于 forcegcperiod，则返回 true。
		return lastgc != 0 && t.now-lastgc > forcegcperiod
	case gcTriggerCycle:
		// 检查当前周期数与预设的周期数的差值。
		// 如果差值大于 0，则返回 true。
		// 注意这里考虑了周期数溢出的情况。
		return int32(t.n-work.cycles.Load()) > 0
	}
	return true
}

// 函数启动 GC。方法完成了从开始到进入并发标记阶段的关键操作，为后续的垃圾回收过程做好了准备
//
// 在某些情况下，此函数可能会不执行过渡就返回，例如当它在不可抢占的上下文中被调用或持有锁时。
func gcStart(trigger gcTrigger) {
	// 检查不可抢占或潜在不稳定的情况:
	// 由于此函数可能在 malloc 被调用时调用，而 malloc 可能在许多持有锁的库内部被调用，
	// 因此不要尝试在不可抢占或潜在不稳定的情况下启动 GC。

	// 获取内存锁
	mp := acquirem()
	//检查 goroutine: 使用 getg() 获取当前 goroutine。
	//检查是否持有锁: 检查是否持有锁 (mp.locks > 1) 或者是否不可抢占 (mp.preemptoff != "")。
	if gp := getg(); gp == mp.g0 || mp.locks > 1 || mp.preemptoff != "" {
		releasem(mp) // 释放内存锁
		return
	}
	// 释放内存锁
	releasem(mp)
	mp = nil

	// 并发清扫剩余区间

	// 循环清扫: 使用 for 循环和 sweepone 函数来帮助完成清扫阶段。
	// 检查过渡条件: 使用 trigger.test() 根据不同的触发条件来决定是否需要启动一个新的 GC 周期。
	for trigger.test() && sweepone() != ^uintptr(0) {
		sweep.nbgsweep++ // 增加清扫计数: 使用 sweep.nbgsweep++ 增加清扫计数。
	}

	// 执行 GC 初始化和清扫终止过渡

	semacquire(&work.startSema) // 获取启动信号量
	// 在过渡锁下重新检查过渡条件。
	if !trigger.test() {
		// 执行 GC 初始化和清扫终止过渡。
		semrelease(&work.startSema)
		return
	}

	// 在 gcstoptheworld 调试模式下，根据需要升级模式。
	// 我们在重新检查过渡条件之后这样做，以防止多个 goroutines 检测到堆触发条件并开始多个 STW GC。
	mode := gcBackgroundMode // 并发的垃圾回收和清扫
	if debug.gcstoptheworld == 1 {
		mode = gcForceMode // 立即执行停止世界的垃圾回收，清扫操作并发
	} else if debug.gcstoptheworld == 2 {
		mode = gcForceBlockMode // 立即执行停止世界的垃圾回收和清扫（由用户强制触发）
	}

	semacquire(&gcsema)    // 获取GC信号量
	semacquire(&worldsema) // 获取stw世界信号量

	// 为了统计，检查这次 GC 是否是由用户手动强制触发的。
	// 在 gcsema 下更新它以避免 gctrace 获取错误的值。
	work.userForced = trigger.kind == gcTriggerCycle

	// 如果启用了跟踪，则记录 GC 开始
	if traceEnabled() {
		traceGCStart()
	}

	// 检查所有 Ps 是否已完成延迟的 mcache 刷新。
	for _, p := range allp {
		if fg := p.mcache.flushGen.Load(); fg != mheap_.sweepgen {
			println("runtime: p", p.id, "flushGen", fg, "!= sweepgen", mheap_.sweepgen)
			throw("p mcache not flushed")
		}
	}

	gcBgMarkStartWorkers()                                // 创建多个后台 goroutine, 准备并发标记
	systemstack(gcResetMarkState)                         // 在标记阶段开始之前重置全局状态，并重置所有 G 的栈扫描状态。
	work.stwprocs, work.maxprocs = gomaxprocs, gomaxprocs // 设置 STW 进程数
	if work.stwprocs > ncpu {
		// 这用于计算 STW 阶段的 CPU 时间，
		// 因此它不能超过 ncpu，即使 GOMAXPROCS 更大。
		work.stwprocs = ncpu
	}
	work.heap0 = gcController.heapLive.Load()
	work.pauseNS = 0
	work.mode = mode

	now := nanotime()
	work.tSweepTerm = now
	work.pauseStart = now
	systemstack(func() { stopTheWorldWithSema(stwGCSweepTerm) }) // stw 暂停世界
	// 在开始并发扫描之前完成清扫。
	systemstack(func() {
		finishsweep_m() // 函数确保所有 span 已经完成清扫, 被认为是垃圾的span被清扫
	})

	clearpools()                                           // 函数用于清除各种缓存和池，以减少内存占用并准备下一轮垃圾回收。
	work.cycles.Add(1)                                     // 增加周期数
	gcController.startCycle(now, int(gomaxprocs), trigger) // 协助和 worker 可以在我们开始世界的同时开始。
	gcCPULimiter.startGCTransition(true, now)              // 通知 CPU 限制器协助可以开始。

	// 如果不是并发的垃圾回收和清扫模式, 应该禁用用户 goroutine 的调度
	if mode != gcBackgroundMode {
		schedEnableUser(false) // 函数用于禁用用户 goroutine 的调度
	}

	// 进入并发标记阶段并启用写屏障。

	// 如果当前阶段是标记阶段 _GCmark 或标记终止阶段 _GCmarktermination ，则需要启用写屏障
	setGCPhase(_GCmark) // 设置 GC 并发标记阶段, 函数用于设置当前的垃圾回收阶段，并根据垃圾回收阶段调整写屏障的启用状态
	gcBgMarkPrepare()   // 准备后台标记, 这个条件被用来判断是否所有的标记工作都已经完成

	// 函数用于准备根扫描工作。这包括将全局变量、栈以及其他杂项放入队列中，并初始化扫描相关的状态。
	// go的垃圾回收并没有将跟对象标记为黑色, 而是收集到一个跟对象队列里面, 这个队列引用不会被垃圾回收。
	// 在根扫描阶段，根对象引用的对象会被标记为灰色，并加入到工作队列中
	// 根对象永远不会被垃圾回收，因为它们始终被认为是可达的
	gcMarkRootPrepare() // 函数用于准备根扫描工作, 这包括将全局变量、栈以及其他杂项放入队列中，并初始化扫描相关的状态。
	gcMarkTinyAllocs()  // 函数用于将所有活动的小块(小于 16 字节)分配（tiny allocation）标记为灰色。

	// 到此为止，所有 Ps 都已启用写屏障，从而维护了无白色到黑色的不变性。
	// 启用 mutator 协助以对快速分配的 mutator 施加反压。
	atomic.Store(&gcBlackenEnabled, 1)

	// 在 STW 模式下，我们可能会在 systemstack 返回后立即被阻止，因此确保我们不是可抢占的。

	// 获取内存锁
	mp = acquirem()

	// 并发标记。
	systemstack(func() {
		now = startTheWorldWithSema()                      // 函数用于恢复所有 goroutine 的执行。
		work.pauseNS += now - work.pauseStart              // 计算并累加暂停时间
		work.tMark = now                                   // 设置标记开始时间
		memstats.gcPauseDist.record(now - work.pauseStart) // 记录垃圾回收暂停时间分布

		// 计算清扫终止阶段消耗的 CPU 时间
		sweepTermCpu := int64(work.stwprocs) * (work.tMark - work.tSweepTerm)
		work.cpuStats.gcPauseTime += sweepTermCpu // 累加垃圾回收暂停时间
		work.cpuStats.gcTotalTime += sweepTermCpu // 累加总的垃圾回收时间

		// 释放 CPU 限制器。
		gcCPULimiter.finishGCTransition(now)
	})

	// 在 STW 模式下，在 Gosched() 之前释放世界 sema，因为我们需要稍后重新获取它，
	// 但在这个 goroutine 可以再次运行之前，否则可能会自我死锁。
	semrelease(&worldsema) // 释放stw世界信号量

	// 释放内存锁
	releasem(mp)

	// 如果不是并发回收, 则让出 cpu
	if mode != gcBackgroundMode {
		Gosched() // 出 CPU 时间片，以便其他 goroutine 可以运行
	}

	semrelease(&work.startSema) // 释放启动信号量
}

// gcMarkDoneFlushed counts the number of P's with flushed work.
//
// Ideally this would be a captured local in gcMarkDone, but forEachP
// escapes its callback closure, so it can't capture anything.
//
// This is protected by markDoneSema.
var gcMarkDoneFlushed uint32

// 函数用于将垃圾回收从标记阶段过渡到标记终止阶段，如果所有可达的对象已经被标记。
// 如果还有未标记的对象存在或未来可能有新对象产生，则将所有本地工作刷新到全局队列中，
// 使其可以被其他工作者发现并处理。
//
// 调用上下文必须是可以抢占的。
//
// 刷新本地工作非常重要，因为空闲的 P 可能有本地队列中的工作。这是使这些工作可见并驱动垃圾回收完成的唯一途径。
//
// 在此函数中显式允许使用写屏障。如果它确实转换到标记终止阶段，那么所有可达的对象都已被标记，
// 因此写屏障不会再遮蔽任何对象。
func gcMarkDone() {
	// 确保只有一个线程在同一时间运行 ragged barrier。
	semacquire(&work.markDoneSema)

top:
	// 在转换锁下重新检查转换条件。
	//
	// 关键是必须在执行 ragged barrier 之前检查全局工作队列是否为空。
	// 否则，可能存在全局工作，而 P 在通过 ragged barrier 后可能会获取这些工作。
	if !(gcphase == _GCmark && work.nwait == work.nproc && !gcMarkWorkAvailable(nil)) {
		semrelease(&work.markDoneSema)
		return
	}

	// forEachP 需要 worldsema 执行，而我们稍后也需要它来停止世界，因此现在获取 worldsema。
	semacquire(&worldsema) // 获取stw世界信号量

	// 刷新所有本地缓冲区并收集 flushedWork 标志。
	gcMarkDoneFlushed = 0
	systemstack(func() {
		// 获取当前正在运行的 goroutine
		gp := getg().m.curg
		// 标记用户栈为可以抢占，以便它可以被扫描
		casGToWaiting(gp, _Grunning, waitReasonGCMarkTermination)

		// 遍历所有 P 线程调度器
		forEachP(func(pp *p) {
			// 刷新写屏障缓冲区，因为这可能会向 gcWork 添加工作。
			wbBufFlush1(pp)

			// 刷新 gcWork，因为这可能会创建全局工作并设置 flushedWork 标志。
			//
			// TODO(austin): 分解这些工作缓冲区以更好地分配工作。
			pp.gcw.dispose() // 将所有缓存的指针返回到全局队列
			// 收集 flushedWork 标志。
			if pp.gcw.flushedWork {
				atomic.Xadd(&gcMarkDoneFlushed, 1)
				pp.gcw.flushedWork = false
			}
		})

		// 转化线程状态
		casgstatus(gp, _Gwaiting, _Grunning)
	})

	// 检查是否存在更多灰色对象
	if gcMarkDoneFlushed != 0 {
		// 继续执行。有可能在 ragged barrier 期间转换条件再次变为真，因此重新检查。
		semrelease(&worldsema) // 释放stw世界信号量
		goto top
	}

	// 没有全局工作，没有本地工作，并且没有任何 P 通信了工作，自从获取了 markDoneSema。
	// 因此没有灰色对象，也不会有更多对象被遮蔽。转换到标记终止阶段。

	now := nanotime()                                           // 获取当前时间
	work.tMarkTerm = now                                        // 设置标记终止时间
	work.pauseStart = now                                       // 设置暂停开始时间
	getg().m.preemptoff = "gcing"                               // 设置不可抢占标志
	systemstack(func() { stopTheWorldWithSema(stwGCMarkTerm) }) // stw 停止世界

	restart := false // 初始化重启标志
	systemstack(func() {
		for _, p := range allp {
			wbBufFlush1(p)
			// 检查是否存在未完成的工作
			if !p.gcw.empty() {
				restart = true
				break
			}
		}
	})

	// 如果存在未完成的标记工作，则重新开始并发标记阶段
	if restart {
		getg().m.preemptoff = ""
		systemstack(func() {
			now := startTheWorldWithSema() // 函数用于恢复所有 goroutine 的执行。
			work.pauseNS += now - work.pauseStart
			memstats.gcPauseDist.record(now - work.pauseStart)
		})
		semrelease(&worldsema) // 释放stw世界信号量
		goto top
	}

	gcComputeStartingStackSize() // 用于计算新的 goroutines 启动时应分配的初始栈大小

	// 禁用协助和后台工作者。必须在唤醒被阻塞的协助之前这样做。
	atomic.Store(&gcBlackenEnabled, 0)

	// 通知 CPU 限制器 GC 协助现在将停止。
	gcCPULimiter.startGCTransition(false, now)

	gcWakeAllAssists() // 用于唤醒所有当前被阻塞的协助（assists）, 因为调度器 p 被 stw 暂停了,所以还不能调度

	// 同样，释放转换锁。被阻塞的工作者和协助将在启动世界时运行。
	semrelease(&work.markDoneSema)

	// 在 STW 模式下，重新启用用户 goroutines。这些将在 stw 启动世界后排队运行。
	schedEnableUser(true) // 函数用于启动用户 goroutine 的调度, 因为调度器 p 被 stw 暂停了,所以还不能调度

	// endCycle 依赖于所有 gcWork 缓存统计信息被刷新。
	// 上面的终止算法确保了从 ragged barrier 以来的所有分配。
	gcController.endCycle(now, int(gomaxprocs), work.userForced)

	// 从标记阶段过渡到标记终止阶段完成

	// 执行标记终止。
	gcMarkTermination()
}

// 函数用于完成垃圾回收周期的标记阶段，并重新启动世界。
// 该函数执行了标记终止阶段的多个步骤，包括最终标记, 清扫、统计更新、资源释放等。
func gcMarkTermination() {
	setGCPhase(_GCmarktermination) // 开始标记终止阶段（写屏障仍然启用）。

	// heapLive: 表示垃圾回收认为还活着的字节数
	work.heap1 = gcController.heapLive.Load()
	startTime := nanotime()

	// 获取当前的 m 结构体。
	mp := acquirem()
	mp.preemptoff = "gcing"
	mp.traceback = 2
	curgp := mp.curg

	// 最终标记

	// 将当前 goroutine 状态改为等待状态。
	casGToWaiting(curgp, _Grunning, waitReasonGarbageCollection)

	systemstack(func() {
		gcMark(startTime) // 系统确保所有可达对象都被正确标记，并为下一次垃圾回收周期做准备
		// 必须立即返回。
		// 外部函数的栈可能在 gcMark 过程中移动了（它会缩小栈，包括外部函数的栈），
		// 所以我们不能引用它的任何变量。返回非系统栈以获取新的地址再继续。
	})

	// 执行最终标记检查
	systemstack(func() {
		// 记录标记过程中扫描的字节数。
		work.heap2 = work.bytesMarked

		// 这段代码用于执行一个完全非并行的、停止世界的标记过程，目的是检查是否有对象在并发标记过程中被遗漏
		if debug.gccheckmark > 0 {
			startCheckmarks()             // 函数来初始化检查标记位。检查标记位是一种额外的机制，用来确保所有的可达对象都被正确地标记
			gcResetMarkState()            // 函数来重置标记状态。这通常意味着清除所有先前的标记信息，准备进行新的标记过程
			gcw := &getg().m.p.ptr().gcw  // 函数获取当前 goroutine 的结构体的 GC 工作缓冲区
			gcDrain(gcw, 0)               // 执行扫描工作，即黑化灰色对象
			wbBufFlush1(getg().m.p.ptr()) // 函数来清空写屏障缓冲区
			gcw.dispose()                 // 来释放 gcw，确保所有相关资源都被正确清理
			endCheckmarks()               // 函数来结束使用检查标记位的过程。这通常意味着清理所有与检查标记位相关的临时数据结构
		}

		setGCPhase(_GCoff) // 标记已完成，所以我们可以关闭写屏障。
		gcSweep(work.mode) // 正式回收垃圾, 因为调度器 p 被 stw 暂停了,所以还不能调度后台清扫 goroutine
	})

	mp.traceback = 0

	// 将当前 goroutine 状态改回运行状态。
	casgstatus(curgp, _Gwaiting, _Grunning)

	// 此时标记终止(最终标记)阶段已经完成

	// 更新统计信息和清扫状态

	if traceEnabled() {
		traceGCDone()
	}

	// 所有操作完成。
	mp.preemptoff = ""

	if gcphase != _GCoff {
		throw("gc done but gcphase != _GCoff")
	}

	// 记录 heapInUse 供 scavenger 使用。
	memstats.lastHeapInUse = gcController.heapInUse.load()

	systemstack(gcControllerCommit) // 重新计算所有步调参数，这些参数用于垃圾回收的触发阈值和堆的目标大小。为下一个周期做准备。

	// 更新与 GC 周期相关的统计信息，包括 GC 暂停时间、暂停时间分布、最近一次 GC 的时间戳以及 GC 过程中消耗的 CPU 时间
	now := nanotime()                                                                       // 函数获取当前时间点（纳秒精度）。
	sec, nsec, _ := time_now()                                                              // 函数获取当前时间的秒数和纳秒数。
	unixNow := sec*1e9 + int64(nsec)                                                        // 计算 Unix 时间戳（纳秒精度）
	work.pauseNS += now - work.pauseStart                                                   // 计算当前时间与上次记录的时间差
	work.tEnd = now                                                                         // 更新 work.tEnd 为当前时间点
	memstats.gcPauseDist.record(now - work.pauseStart)                                      // 录 GC 暂停时间分布
	atomic.Store64(&memstats.last_gc_unix, uint64(unixNow))                                 // 必须是 Unix 时间以对用户有意义
	atomic.Store64(&memstats.last_gc_nanotime, uint64(now))                                 // 单调时间用于内部使用
	memstats.pause_ns[memstats.numgc%uint32(len(memstats.pause_ns))] = uint64(work.pauseNS) // 更新 GC 暂停时间数组
	memstats.pause_end[memstats.numgc%uint32(len(memstats.pause_end))] = uint64(unixNow)    // 更新 GC 暂停结束时间数组
	memstats.pause_total_ns += uint64(work.pauseNS)                                         // 更新总的 GC 暂停时间

	markTermCpu := int64(work.stwprocs) * (work.tEnd - work.tMarkTerm) // 计算标记终止阶段消耗的 CPU 时间
	work.cpuStats.gcPauseTime += markTermCpu                           // 更新 GC 暂停时间统计
	work.cpuStats.gcTotalTime += markTermCpu                           // 更新 GC 总 CPU 时间统计

	// 累计 CPU 统计数据。
	// 传递 gcMarkPhase=true 以便我们可以获取最新的 GC CPU 统计数据。
	work.cpuStats.accumulate(now, true)

	// 计算总体 GC CPU 利用率。
	// 从总体利用率中排除空闲标记时间，因为它被认为是“免费”的。
	memstats.gc_cpu_fraction = float64(work.cpuStats.gcTotalTime-work.cpuStats.gcIdleTime) / float64(work.cpuStats.totalTime)

	// 重置协助时间和后台时间统计。
	//
	// 在这里重置，而不是在下一个 GC 周期开始时，因为即使 GC 不活跃，这两个也可能持续累加。
	scavenge.assistTime.Store(0)
	scavenge.backgroundTime.Store(0)

	// 重置空闲时间统计。
	sched.idleTime.Store(0)

	// 重置清扫状态。
	sweep.nbgsweep = 0
	sweep.npausesweep = 0

	if work.userForced {
		memstats.numforcedgc++
	}

	// 完成 GC 并唤醒等待的 goroutines

	// 增加 GC 周期计数并唤醒等待清扫的 goroutines。
	lock(&work.sweepWaiters.lock)
	memstats.numgc++
	injectglist(&work.sweepWaiters.list)
	unlock(&work.sweepWaiters.lock)

	// 增加 scavenger 生成。
	//
	// 这个时刻代表了峰值使用的堆，因为我们正要开始清扫。
	mheap_.pages.scav.index.nextGen()

	// 释放 CPU 限速器。
	gcCPULimiter.finishGCTransition(now)

	// 完成当前的堆剖析周期并开始一个新的堆剖析周期。
	// 我们在启动世界之前完成这个操作，以避免事件泄露到错误的周期。
	mProf_NextCycle()

	// 可能有需要清扫的过时 span 存在于 mcaches 中。
	// 这些不在任何清扫列表中，所以我们需要将它们计入清扫完成前的状态，直到确保所有的 span 都被强制清理。
	sl := sweep.active.begin()
	if !sl.valid {
		throw("failed to set sweep barrier")
	}

	systemstack(func() { startTheWorldWithSema() }) // stw启动世界。

	// 清空堆剖析数据以便开始新的周期。
	// 这个操作相对昂贵，所以我们不在世界停止时执行它。
	mProf_Flush()

	// 为清扫器准备释放 workbufs。我们异步执行这个操作，因为它可能需要一段时间。
	prepareFreeWorkbufs()

	// 释放栈 span。这必须在 GC 周期之间完成。
	systemstack(freeStackSpans)

	// 确保所有 mcaches 都被清空。每个 P 在分配之前都会清空自己的 mcache，但空闲的 P 可能不会。
	// 由于这是清扫所有 span 所必需的，我们需要在开始下一个 GC 周期之前确保所有 mcaches 都被清空。
	//
	// 当我们在处理这些时，清空空闲 P 的页面缓存以避免页面卡在那里。
	// 这些页面对 scavenger 是隐藏的，所以在小型空闲堆中可能会保留大量额外内存。
	//
	// 同时，清空 pinner 缓存，以避免无限泄漏这部分内存。
	systemstack(func() {
		forEachP(func(pp *p) {
			pp.mcache.prepareForSweep()
			if pp.status == _Pidle {
				systemstack(func() {
					lock(&mheap_.lock)
					pp.pcache.flush(&mheap_.pages)
					unlock(&mheap_.lock)
				})
			}
			pp.pinnerCache = nil
		})
	})

	// 现在我们已经清扫了 mcaches 中的过时 span，它们不再计入未清扫 span 的统计。
	sweep.active.end(sl)

	// 在释放 worldsema 之前打印 gctrace。一旦我们释放了 worldsema，另一个周期可能会开始并覆盖我们试图打印的统计信息。
	if debug.gctrace > 0 {
		util := int(memstats.gc_cpu_fraction * 100)

		var sbuf [24]byte
		printlock()
		print("gc ", memstats.numgc,
			" @", string(itoaDiv(sbuf[:], uint64(work.tSweepTerm-runtimeInitTime)/1e6, 3)), "s ",
			util, "%: ")
		prev := work.tSweepTerm
		for i, ns := range []int64{work.tMark, work.tMarkTerm, work.tEnd} {
			if i != 0 {
				print("+")
			}
			print(string(fmtNSAsMS(sbuf[:], uint64(ns-prev))))
			prev = ns
		}
		print(" ms clock, ")
		for i, ns := range []int64{
			int64(work.stwprocs) * (work.tMark - work.tSweepTerm),
			gcController.assistTime.Load(),
			gcController.dedicatedMarkTime.Load() + gcController.fractionalMarkTime.Load(),
			gcController.idleMarkTime.Load(),
			markTermCpu,
		} {
			if i == 2 || i == 3 {
				// 分隔标记时间的不同部分。
				print("/")
			} else if i != 0 {
				print("+")
			}
			print(string(fmtNSAsMS(sbuf[:], uint64(ns))))
		}
		print(" ms cpu, ",
			work.heap0>>20, "->", work.heap1>>20, "->", work.heap2>>20, " MB, ",
			gcController.lastHeapGoal>>20, " MB goal, ",
			gcController.lastStackScan.Load()>>20, " MB stacks, ",
			gcController.globalsScan.Load()>>20, " MB globals, ",
			work.maxprocs, " P")
		if work.userForced {
			print(" (forced)")
		}
		print("\n")
		printunlock()
	}

	// 设置任何被推迟到故障的 arena chunk。
	lock(&userArenaState.lock)
	faultList := userArenaState.fault
	userArenaState.fault = nil
	unlock(&userArenaState.lock)
	for _, lc := range faultList {
		lc.mspan.setUserArenaChunkToFault()
	}

	// 如果堆大小超过某个阈值，则在某些元数据上启用大页。
	if gcController.heapGoal() > minHeapForMetadataHugePages {
		systemstack(func() {
			mheap_.enableMetadataHugePages()
		})
	}

	semrelease(&worldsema) // 释放stw世界信号量
	semrelease(&gcsema)

	// 注意：现在可能会开始另一个 GC 周期。

	releasem(mp)
	mp = nil

	// 现在 GC 已完成，如果需要的话启动终结器线程。
	if !concurrentSweep {
		// 给排队的终结器一个运行的机会。
		Gosched()
	}
}

// 函数准备后台标记工作 goroutine。创建足够数量的后台标记工作 goroutine。
// 这些 goroutine 将负责执行并发标记阶段的工作，以减少垃圾回收对应用程序性能的影响。
// 这些 goroutine 不会在标记阶段之前运行，但必须在工作未停止且从常规 G 栈上启动。
// 调用者必须持有 worldsema 信号量。
func gcBgMarkStartWorkers() {
	// 后台标记是由每个 P 上的 G 来执行的。确保每个 P 都有一个后台 GC G。
	//
	// 工作 G 在 GOMAXPROCS 减少时不会退出。如果 GOMAXPROCS 再次增加，
	// 我们可以重用旧的工作 G，无需创建新的工作 G。
	for gcBgMarkWorkerCount < gomaxprocs {
		// 在并发标记阶段执行的, 主要目的是执行扫描工作，即黑化灰色对象。
		go gcBgMarkWorker() // 启动后台标记工作 goroutine,直到达到当前的 gomaxprocs 数量

		// 使用 notetsleepg 和 noteclear 来通知后台标记工作准备就绪
		// 这确保后台标记工作 G 在其所属的 P 下一个 findRunnableGCWorker 调用之前被加入池中
		notetsleepg(&work.bgMarkReady, -1)
		noteclear(&work.bgMarkReady)

		// 每启动一个后台标记工作 goroutine，就增加 gcBgMarkWorkerCount 的值
		gcBgMarkWorkerCount++
	}
}

// 函数用于设置后台标记的准备工作。
//
// 注意：此时不允许辅助器（mutator assists）启用。
//
// 当工作线程开始工作时，它会递减 work.nwait。
// 如果 work.nproc 等于 work.nwait，则说明所有的工作线程都已经停止工作，没有工作线程在进行标记工作。
// 这个条件被用来判断是否所有的标记工作都已经完成。
func gcBgMarkPrepare() {
	// 设置 nproc 和 nwait 为最大值，表示有很多工作线程，几乎所有的都在等待。
	// 使用 ^uint32(0) 来设置最大值，这相当于 -1。
	// 当工作线程开始工作时，它会递减 nwait。
	// 如果 nproc 等于 nwait，则没有工作线程在工作。
	// there are no workers.
	work.nproc = ^uint32(0)
	work.nwait = ^uint32(0)
}

// gcBgMarkWorkerNode is an entry in the gcBgMarkWorkerPool. It points to a single
// gcBgMarkWorker goroutine.
type gcBgMarkWorkerNode struct {
	// Unused workers are managed in a lock-free stack. This field must be first.
	node lfnode

	// The g of this worker.
	gp guintptr

	// Release this m on park. This is used to communicate with the unlock
	// function, which cannot access the G's stack. It is unused outside of
	// gcBgMarkWorker().
	m muintptr
}

// 函数是后台标记工作 goroutine 的主体。
// 它执行并发标记阶段的工作，以减少垃圾回收对应用程序性能的影响。
// 在并发标记阶段执行的, 主要目的是执行扫描工作，即黑化灰色对象。
func gcBgMarkWorker() {
	// 获取当前 goroutine
	gp := getg()

	// 初始化后台标记工作 goroutine

	gp.m.preemptoff = "GC worker init" // 禁止抢占，防止在初始化后台标记工作 goroutine 时发生死锁。
	node := new(gcBgMarkWorkerNode)    // 创建后台标记工作节点
	gp.m.preemptoff = ""
	node.gp.set(gp)               // 设置节点中的 goroutine
	node.m.set(acquirem())        // 获取内存锁
	notewakeup(&work.bgMarkReady) // 通知后台标记工作准备就绪

	// 从这里开始，后台标记工作 goroutine 通常由 gcController.findRunnableGCWorker 协同调度。
	// 当执行 P 上的工作时，禁止抢占，因为我们正在处理 P 本地的工作缓冲区。
	// 当 preempt 标志设置时，这将使自己进入 _Gwaiting 状态，以等待 gcController.findRunnableGCWorker 在适当的时候唤醒。
	// 当启用抢占时（例如，在 gcMarkDone 中），这个工作 goroutine 可能会被抢占，并作为 _Grunnable G 从 runq 中调度。
	// 这是可以接受的；它最终会再次调用 gopark 以等待 gcController.findRunnableGCWorker 的进一步调度。

	// 等待被唤醒
	for {
		// 等待直到被 gcController.findRunnableGCWorker 唤醒。
		gopark(func(g *g, nodep unsafe.Pointer) bool {
			node := (*gcBgMarkWorkerNode)(nodep)

			if mp := node.m.ptr(); mp != nil {
				// 后台标记工作 goroutine 不再运行；释放 M。
				// 注意：一旦我们不再执行 P 本地标记工作，就可以安全地释放 M。
				// 但是，由于我们协同停止工作，当 gp.preempt 设置时，如果我们在循环中释放内存锁，
				// 接下来的 gopark 调用会立即抢占这个 G。这是安全的，但效率低下：G 必须重新调度，
				// 只是为了再次进入 gopark 并停车。因此，我们将释放内存锁的操作延迟到 G 停车之后。
				releasem(mp)
			}

			// 将这个 G 放回池中。
			gcBgMarkWorkerPool.push(&node.node)
			// 注意：此时，G 可能立即被重新调度并可能正在运行。
			return true
		}, unsafe.Pointer(node), waitReasonGCWorkerIdle, traceBlockSystemGoroutine, 0)

		// 执行标记工作
		// 禁止在此处发生抢占，否则另一个 G 可能会看到 p.gcMarkWorkerMode。

		// 禁止抢占，以便我们可以使用 gcw。如果调度器想要抢占我们，
		// 我们将停止工作，释放 gcw，然后抢占。
		node.m.set(acquirem()) // 获取内存锁
		pp := gp.m.p.ptr()     // 获取当前 P

		// 检查标记是否启用
		if gcBlackenEnabled == 0 {
			println("worker mode", pp.gcMarkWorkerMode)
			throw("gcBgMarkWorker: blackening not enabled")
		}

		// 检查工作模式是否已设置
		if pp.gcMarkWorkerMode == gcMarkWorkerNotWorker {
			throw("gcBgMarkWorker: mode not set")
		}

		startTime := nanotime() // 获取开始时间
		pp.gcMarkWorkerStartTime = startTime
		var trackLimiterEvent bool
		if pp.gcMarkWorkerMode == gcMarkWorkerIdleMode {
			trackLimiterEvent = pp.limiterEvent.start(limiterEventIdleMarkWork, startTime)
		}

		decnwait := atomic.Xadd(&work.nwait, -1)
		if decnwait == work.nproc {
			println("runtime: work.nwait=", decnwait, "work.nproc=", work.nproc)
			throw("work.nwait was > work.nproc")
		}

		// 使用 systemstack 根据工作模式执行不同的标记工作
		systemstack(func() {
			// 标记我们的 goroutine 为可抢占，以便其栈可以被扫描。
			// 这使得两个标记工作 goroutine 可以相互扫描（否则，它们会死锁）。
			// 我们必须不修改 G 栈上的任何内容。但是，标记工作 goroutine 禁用了栈缩小，所以从 G 栈读取是安全的。
			casGToWaiting(gp, _Grunning, waitReasonGCWorkerActive)

			// 使用 systemstack 根据工作模式执行不同的标记工作
			switch pp.gcMarkWorkerMode {
			default:
				throw("gcBgMarkWorker: unexpected gcMarkWorkerMode")

			// 专用模式下的标记工作
			case gcMarkWorkerDedicatedMode:
				gcDrain(&pp.gcw, gcDrainUntilPreempt|gcDrainFlushBgCredit) // 在并发标记阶段执行的, 主要目的是执行扫描工作，即黑化灰色对象。
				if gp.preempt {
					// 我们被抢占了。这是一个有用的信号，用于将所有内容从 runq 中踢出，
					// 以便它可以在其他地方运行。
					if drainQ, n := runqdrain(pp); n > 0 {
						lock(&sched.lock)
						globrunqputbatch(&drainQ, int32(n))
						unlock(&sched.lock)
					}
				}
				// 再次执行排水操作，这次不允许抢占。
				gcDrain(&pp.gcw, gcDrainFlushBgCredit) // 在并发标记阶段执行的, 主要目的是执行扫描工作，即黑化灰色对象。

			// 分割模式下的标记工作
			case gcMarkWorkerFractionalMode:
				gcDrain(&pp.gcw, gcDrainFractional|gcDrainUntilPreempt|gcDrainFlushBgCredit) // 在并发标记阶段执行的, 主要目的是执行扫描工作，即黑化灰色对象。

			// 空闲模式下的标记工作
			case gcMarkWorkerIdleMode:
				gcDrain(&pp.gcw, gcDrainIdle|gcDrainUntilPreempt|gcDrainFlushBgCredit) // 在并发标记阶段执行的, 主要目的是执行扫描工作，即黑化灰色对象。
			}

			// 恢复goroutine为可运行状态
			casgstatus(gp, _Gwaiting, _Grunning)
		})

		// 计算时间和标记我们为停止状态。
		now := nanotime()
		duration := now - startTime
		gcController.markWorkerStop(pp.gcMarkWorkerMode, duration)
		if trackLimiterEvent {
			pp.limiterEvent.stop(limiterEventIdleMarkWork, now)
		}
		if pp.gcMarkWorkerMode == gcMarkWorkerFractionalMode {
			atomic.Xaddint64(&pp.gcFractionalMarkTime, duration)
		}

		// 这是我们最后一个工作 goroutine 并且我们是否耗尽了工作？
		incnwait := atomic.Xadd(&work.nwait, +1)
		if incnwait > work.nproc {
			println("runtime: p.gcMarkWorkerMode=", pp.gcMarkWorkerMode,
				"work.nwait=", incnwait, "work.nproc=", work.nproc)
			throw("work.nwait > work.nproc")
		}

		// 我们必须清除工作模式，以避免将该模式归因于不同的（非工作）G 在 traceGoStart 中。
		pp.gcMarkWorkerMode = gcMarkWorkerNotWorker

		// 如果这个工作 goroutine 达到了后台标记完成点，向主 GC goroutine 发送信号。
		// incnwait == work.nproc 检查工作线程的等待计数是否等于当前的工作线程总数。
		//!gcMarkWorkAvailable(nil) 检查是否还有未完成的标记工作。
		// 这意味着如果所有可达的对象已经被标记，垃圾回收将进入标记终止阶段。
		if incnwait == work.nproc && !gcMarkWorkAvailable(nil) {
			releasem(node.m.ptr())
			node.m.set(nil)

			// 转换到标记终止阶段:
			// 调用 gcMarkDone() 函数将垃圾回收从标记阶段转换到标记终止阶段。
			// 这意味着如果所有可达的对象已经被标记，垃圾回收将进入下一个阶段。
			gcMarkDone() // 将垃圾回收从标记阶段转换到标记终止阶段，如果所有可达的对象已经被标记。
		}
	}
}

// 用于报告在给定的 P 上执行标记工作是否可能有用。
// 如果 p 为 nil，则仅检查全局的工作源。
//
// - p 是一个指向 p 结构体的指针，表示要检查的 P。
//
// 返回值是一个布尔值，表示是否有可用的工作。
func gcMarkWorkAvailable(p *p) bool {
	// 如果 p 不为 nil 且 p 的 gcw 不为空，则表明有可用的工作。
	if p != nil && !p.gcw.empty() {
		return true
	}

	// 如果全局工作队列不为空，则表明有可用的工作。
	if !work.full.empty() {
		return true // 全局工作可用
	}

	// 如果根扫描工作还没有完成，则表明有可用的工作。
	if work.markrootNext < work.markrootJobs {
		return true // 根扫描工作可用
	}

	// 如果以上条件都不满足，则表明没有可用的工作
	return false
}

// 系统确保所有可达对象都被正确标记，并为下一次垃圾回收周期做准备
// 所有的 gcWork 缓冲区必须为空。
// 它负责清理缓冲区、检查标记队列的状态、执行根标记检查，并重置控制器状态。
// 这些步骤确保了 GC 过程的完整性，并为下一个 GC 周期做好准备。
func gcMark(startTime int64) {
	// 如果启用了分配/释放跟踪，则记录 GC 事件。
	if debug.allocfreetrace > 0 {
		tracegc()
	}

	// 检查 GC 阶段是否正确。
	if gcphase != _GCmarktermination {
		throw("in gcMark expecting to see gcphase as _GCmarktermination")
	}

	// 设置标记阶段开始时间。
	work.tstart = startTime

	// 检查是否还有标记工作未完成。如果标记队列非空，则抛出异常。
	if work.full != 0 || work.markrootNext < work.markrootJobs {
		print("runtime: full=", hex(work.full), " next=", work.markrootNext, " jobs=", work.markrootJobs, " nDataRoots=", work.nDataRoots, " nBSSRoots=", work.nBSSRoots, " nSpanRoots=", work.nSpanRoots, " nStackRoots=", work.nStackRoots, "\n")
		panic("non-empty mark queue after concurrent mark")
	}

	// 如果启用了 gccheckmark，进行根标记检查。
	if debug.gccheckmark > 0 {
		gcMarkRootCheck()
	}

	// 丢弃 allg 快照。allgs 可能已增长，在这种情况下，
	// 这是旧的备份存储的唯一引用，无需保留。
	work.stackRoots = nil

	// 清除缓冲区并再次检查所有 gcWork 缓冲区是否为空。
	// 这应该由 gcMarkDone 在进入标记终止之前确保。
	for _, p := range allp {
		// 写屏障可能已缓冲指针，因为它们是在 gcMarkDone 障碍之后。
		// 但是，由于障碍确保了所有可达对象都已标记，因此所有这些
		// 必须是指向黑色对象的指针。因此我们可以简单地丢弃写屏障缓冲区。
		if debug.gccheckmark > 0 {
			// 为了调试，清空缓冲区并确保它们实际上都是已标记的。
			wbBufFlush1(p)
		} else {
			p.wbBuf.reset() // 重置写屏障缓冲区。
		}

		gcw := &p.gcw

		// 如果 GC 工作单元非空，则抛出异常。
		if !gcw.empty() {
			printlock()
			print("runtime: P ", p.id, " flushedWork ", gcw.flushedWork)
			if gcw.wbuf1 == nil {
				print(" wbuf1=<nil>")
			} else {
				print(" wbuf1.n=", gcw.wbuf1.nobj)
			}
			if gcw.wbuf2 == nil {
				print(" wbuf2=<nil>")
			} else {
				print(" wbuf2.n=", gcw.wbuf2.nobj)
			}
			print("\n")
			throw("P has cached GC work at end of mark termination")
		}
		// 可能仍然有缓存的空缓冲区，我们需要清空它们，
		// 因为我们即将释放它们。此外，可能有非零统计信息，
		// 因为我们可能在 gcMarkDone 障碍之后分配了黑色对象。
		gcw.dispose()
	}

	// 从每个 mcache 中刷新 scanAlloc，因为我们即将修改 heapScan。
	// 如果稍后刷新，那么 scanAlloc 可能包含不正确的信息。
	for _, p := range allp {
		c := p.mcache
		if c == nil {
			continue
		}
		c.scanAlloc = 0
	}

	// 重置控制器状态。
	gcController.resetLive(work.bytesMarked) // 重置控制器状态，并使用已标记字节数进行初始化。
}

// gcSweep 负责垃圾回收的清扫阶段
// 它会设置清扫参数，并根据清扫模式选择同步清扫或后台清扫。
//
//	同步清扫会急切地清扫所有 span 并释放工作缓冲区。
//	后台清扫会唤醒清扫 goroutine 进行清扫。
//	这些步骤确保了垃圾回收器能够有效地清理未标记的对象，并准备好下一次垃圾回收周期。
//
// 世界必须处于停止状态。
//
//go:systemstack
func gcSweep(mode gcMode) {
	// 断言世界处于停止状态。
	assertWorldStopped()

	// 如果当前不在 GCoff 阶段，则抛出异常。
	if gcphase != _GCoff {
		throw("gcSweep being done but phase is not GCoff")
	}

	lock(&mheap_.lock)                    // 获取堆锁。
	mheap_.sweepgen += 2                  // 增加清扫代数。
	sweep.active.reset()                  // 重置清扫活动状态。
	mheap_.pagesSwept.Store(0)            // 重置已清扫的页面计数。
	mheap_.sweepArenas = mheap_.allArenas // 设置需要清扫的所有区域。
	mheap_.reclaimIndex.Store(0)          // 重置回收索引。
	mheap_.reclaimCredit.Store(0)         // 重置回收信用。
	unlock(&mheap_.lock)                  // 释放堆锁。

	sweep.centralIndex.clear() // 清除中央索引。

	// 特殊情况：同步清扫。
	// _ConcurrentSweep: 垃圾回收器将在清扫阶段尝试并发执行清扫工作，以减少暂停时间
	// gcForceBlockMode: 立即执行停止世界的垃圾回收和清扫（由用户强制触发）
	if !_ConcurrentSweep || mode == gcForceBlockMode {
		// 记录无需按比例清扫。
		lock(&mheap_.lock)           // 获取堆锁。
		mheap_.sweepPagesPerByte = 0 // 设置清扫页面每字节的比例为 0。
		unlock(&mheap_.lock)         // 释放堆锁。

		// 急切地清扫所有 span。
		for sweepone() != ^uintptr(0) {
			sweep.npausesweep++ // 记录清扫次数。
		}

		// 急切地释放工作缓冲区。
		prepareFreeWorkbufs() // 准备释放工作缓冲区。
		for freeSomeWbufs(false) {
		}

		// 所有 "free" 事件已经发生，可以立即提供此标记/清扫周期的配置文件。
		mProf_NextCycle() // 切换到下一个配置文件周期。
		mProf_Flush()     // 刷新配置文件。
		return
	}

	lock(&sweep.lock) // 获取清扫锁。

	// 后台清扫。
	if sweep.parked {
		sweep.parked = false // 设置清扫状态为未停放。
		// sweep.g: 垃圾回收的 goroutine
		ready(sweep.g, 0, true) // 将清扫 goroutine 设置为可运行状态, 因为调度器 p 被 stw 暂停了,所以还不能调度后台清扫 goroutine
	}

	unlock(&sweep.lock) // 释放清扫锁。
}

// 函数在标记阶段（并发或停止世界）开始之前重置全局状态，并重置所有 G 的栈扫描状态。
//
// 此操作可以在不停止世界的情况下安全进行，因为在此期间创建的所有 G 都将处于重置状态。
//
// gcResetMarkState 必须在系统栈上调用，因为它获取了堆锁。有关详细信息，请参阅 mheap。
//
//go:systemstack
func gcResetMarkState() {
	// 锁定 allgs 锁，以确保遍历所有 goroutine 时不发生变化
	forEachG(func(gp *g) {
		// 重置所有 G 的状态
		gp.gcscandone = false // 在 gcphasework 中设置为 true
		gp.gcAssistBytes = 0  // 重置 GC 辅助字节数
	})

	lock(&mheap_.lock)

	// 清除页标记。这仅占每 64GB 堆 1MB，因此此处花费的时间相当微不足道。
	arenas := mheap_.allArenas

	unlock(&mheap_.lock)

	// 遍历所有 arena，清除页标记。
	for _, ai := range arenas {
		ha := mheap_.arenas[ai.l1()][ai.l2()]
		for i := range ha.pageMarks {
			ha.pageMarks[i] = 0 // 清除页标记
		}
	}

	// 重置全局标记状态。

	work.bytesMarked = 0                                // 重置已标记字节数
	work.initialHeapLive = gcController.heapLive.Load() // 保存初始存活堆大小
}

// Hooks for other packages

var poolcleanup func()
var boringCaches []unsafe.Pointer // for crypto/internal/boring

//go:linkname sync_runtime_registerPoolCleanup sync.runtime_registerPoolCleanup
func sync_runtime_registerPoolCleanup(f func()) {
	poolcleanup = f
}

//go:linkname boring_registerCache crypto/internal/boring/bcache.registerCache
func boring_registerCache(p unsafe.Pointer) {
	boringCaches = append(boringCaches, p)
}

// 函数用于清除各种缓存和池，以减少内存占用并准备下一轮垃圾回收。
//
// 这包括同步池（sync.Pool）、boringcrypto 缓存、中央 sudog 缓存以及中央 defer 池。
func clearpools() {
	// 清除 sync.Pool 缓存。
	if poolcleanup != nil {
		poolcleanup() // 调用 poolcleanup 函数清除 sync.Pool 缓存
	}

	// 遍历 boringCaches 中的每个缓存指针，并使用原子操作将其设置为 nil，以清空缓存
	for _, p := range boringCaches {
		atomicstorep(p, nil) // 使用原子操作清空缓存
	}

	// 清除中央 sudog 缓存。
	// 保留每个 P 的缓存不变，因为它们具有严格限定的大小。
	// 在释放缓存列表之前断开连接，以免悬挂引用导致所有条目都被固定。
	lock(&sched.sudoglock)
	var sg, sgnext *sudog
	for sg = sched.sudogcache; sg != nil; sg = sgnext {
		sgnext = sg.next // 保存下一个 sudog
		sg.next = nil    // 断开 sudog 列表的链接
	}
	sched.sudogcache = nil // 清空 sudog 缓存
	unlock(&sched.sudoglock)

	// 清除中央 defer 池。
	// 保留每个 P 的 defer 池不变，因为它们具有严格限定的大小。
	lock(&sched.deferlock)
	// 在释放 defer 池之前断开连接，以免悬挂引用导致所有条目都被固定。
	var d, dlink *_defer
	for d = sched.deferpool; d != nil; d = dlink {
		dlink = d.link // 保存下一个 defer
		d.link = nil   // 断开 defer 列表的链接
	}
	sched.deferpool = nil // 清空 defer 池
	unlock(&sched.deferlock)
}

// Timing

// itoaDiv formats val/(10**dec) into buf.
func itoaDiv(buf []byte, val uint64, dec int) []byte {
	i := len(buf) - 1
	idec := i - dec
	for val >= 10 || i >= idec {
		buf[i] = byte(val%10 + '0')
		i--
		if i == idec {
			buf[i] = '.'
			i--
		}
		val /= 10
	}
	buf[i] = byte(val + '0')
	return buf[i:]
}

// fmtNSAsMS nicely formats ns nanoseconds as milliseconds.
func fmtNSAsMS(buf []byte, ns uint64) []byte {
	if ns >= 10e6 {
		// Format as whole milliseconds.
		return itoaDiv(buf, ns/1e6, 0)
	}
	// Format two digits of precision, with at most three decimal places.
	x := ns / 1e3
	if x == 0 {
		buf[0] = '0'
		return buf[:1]
	}
	dec := 3
	for x >= 100 {
		x /= 10
		dec--
	}
	return itoaDiv(buf, x, dec)
}

// Helpers for testing GC.

// gcTestMoveStackOnNextCall causes the stack to be moved on a call
// immediately following the call to this. It may not work correctly
// if any other work appears after this call (such as returning).
// Typically the following call should be marked go:noinline so it
// performs a stack check.
//
// In rare cases this may not cause the stack to move, specifically if
// there's a preemption between this call and the next.
func gcTestMoveStackOnNextCall() {
	gp := getg()
	gp.stackguard0 = stackForceMove
}

// gcTestIsReachable performs a GC and returns a bit set where bit i
// is set if ptrs[i] is reachable.
func gcTestIsReachable(ptrs ...unsafe.Pointer) (mask uint64) {
	// This takes the pointers as unsafe.Pointers in order to keep
	// them live long enough for us to attach specials. After
	// that, we drop our references to them.

	if len(ptrs) > 64 {
		panic("too many pointers for uint64 mask")
	}

	// Block GC while we attach specials and drop our references
	// to ptrs. Otherwise, if a GC is in progress, it could mark
	// them reachable via this function before we have a chance to
	// drop them.
	semacquire(&gcsema)

	// Create reachability specials for ptrs.
	specials := make([]*specialReachable, len(ptrs))
	for i, p := range ptrs {
		lock(&mheap_.speciallock)
		s := (*specialReachable)(mheap_.specialReachableAlloc.alloc())
		unlock(&mheap_.speciallock)
		s.special.kind = _KindSpecialReachable
		if !addspecial(p, &s.special) {
			throw("already have a reachable special (duplicate pointer?)")
		}
		specials[i] = s
		// Make sure we don't retain ptrs.
		ptrs[i] = nil
	}

	semrelease(&gcsema)

	// Force a full GC and sweep.
	GC()

	// Process specials.
	for i, s := range specials {
		if !s.done {
			printlock()
			println("runtime: object", i, "was not swept")
			throw("IsReachable failed")
		}
		if s.reachable {
			mask |= 1 << i
		}
		lock(&mheap_.speciallock)
		mheap_.specialReachableAlloc.free(unsafe.Pointer(s))
		unlock(&mheap_.speciallock)
	}

	return mask
}

// gcTestPointerClass returns the category of what p points to, one of:
// "heap", "stack", "data", "bss", "other". This is useful for checking
// that a test is doing what it's intended to do.
//
// This is nosplit simply to avoid extra pointer shuffling that may
// complicate a test.
//
//go:nosplit
func gcTestPointerClass(p unsafe.Pointer) string {
	p2 := uintptr(noescape(p))
	gp := getg()
	if gp.stack.lo <= p2 && p2 < gp.stack.hi {
		return "stack"
	}
	if base, _, _ := findObject(p2, 0, 0); base != 0 {
		return "heap"
	}
	for _, datap := range activeModules() {
		if datap.data <= p2 && p2 < datap.edata || datap.noptrdata <= p2 && p2 < datap.enoptrdata {
			return "data"
		}
		if datap.bss <= p2 && p2 < datap.ebss || datap.noptrbss <= p2 && p2 <= datap.enoptrbss {
			return "bss"
		}
	}
	KeepAlive(p)
	return "other"
}
