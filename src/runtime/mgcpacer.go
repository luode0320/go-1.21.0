// Copyright 2021 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/cpu"
	"internal/goexperiment"
	"runtime/internal/atomic"
	_ "unsafe" // for go:linkname
)

const (
	// gcGoalUtilization is the goal CPU utilization for
	// marking as a fraction of GOMAXPROCS.
	//
	// Increasing the goal utilization will shorten GC cycles as the GC
	// has more resources behind it, lessening costs from the write barrier,
	// but comes at the cost of increasing mutator latency.
	gcGoalUtilization = gcBackgroundUtilization

	// gcBackgroundUtilization is the fixed CPU utilization for background
	// marking. It must be <= gcGoalUtilization. The difference between
	// gcGoalUtilization and gcBackgroundUtilization will be made up by
	// mark assists. The scheduler will aim to use within 50% of this
	// goal.
	//
	// As a general rule, there's little reason to set gcBackgroundUtilization
	// < gcGoalUtilization. One reason might be in mostly idle applications,
	// where goroutines are unlikely to assist at all, so the actual
	// utilization will be lower than the goal. But this is moot point
	// because the idle mark workers already soak up idle CPU resources.
	// These two values are still kept separate however because they are
	// distinct conceptually, and in previous iterations of the pacer the
	// distinction was more important.
	gcBackgroundUtilization = 0.25

	// gcCreditSlack is the amount of scan work credit that can
	// accumulate locally before updating gcController.heapScanWork and,
	// optionally, gcController.bgScanCredit. Lower values give a more
	// accurate assist ratio and make it more likely that assists will
	// successfully steal background credit. Higher values reduce memory
	// contention.
	gcCreditSlack = 2000

	// gcAssistTimeSlack is the nanoseconds of mutator assist time that
	// can accumulate on a P before updating gcController.assistTime.
	gcAssistTimeSlack = 5000

	// gcOverAssistWork determines how many extra units of scan work a GC
	// assist does when an assist happens. This amortizes the cost of an
	// assist by pre-paying for this many bytes of future allocations.
	gcOverAssistWork = 64 << 10

	// defaultHeapMinimum 是 heapMinimum 的值 GOGC==100.默认: 4194304 == 4 MB
	defaultHeapMinimum = (goexperiment.HeapMinimum512KiBInt)*(512<<10) +
		(1-goexperiment.HeapMinimum512KiBInt)*(4<<20)

	// maxStackScanSlack is the bytes of stack space allocated or freed
	// that can accumulate on a P before updating gcController.stackSize.
	maxStackScanSlack = 8 << 10

	// memoryLimitMinHeapGoalHeadroom is the minimum amount of headroom the
	// pacer gives to the heap goal when operating in the memory-limited regime.
	// That is, it'll reduce the heap goal by this many extra bytes off of the
	// base calculation, at minimum.
	memoryLimitMinHeapGoalHeadroom = 1 << 20

	// memoryLimitHeapGoalHeadroomPercent is how headroom the memory-limit-based
	// heap goal should have as a percent of the maximum possible heap goal allowed
	// to maintain the memory limit.
	memoryLimitHeapGoalHeadroomPercent = 3
)

// gcController implements the GC pacing controller that determines
// when to trigger concurrent garbage collection and how much marking
// work to do in mutator assists and background marking.
//
// It calculates the ratio between the allocation rate (in terms of CPU
// time) and the GC scan throughput to determine the heap size at which to
// trigger a GC cycle such that no GC assists are required to finish on time.
// This algorithm thus optimizes GC CPU utilization to the dedicated background
// mark utilization of 25% of GOMAXPROCS by minimizing GC assists.
// GOMAXPROCS. The high-level design of this algorithm is documented
// at https://github.com/golang/proposal/blob/master/design/44167-gc-pacer-redesign.md.
// See https://golang.org/s/go15gcpacing for additional historical context.
var gcController gcControllerState

type gcControllerState struct {
	// 从 GOGC 初始化。GOGC=off 表示没有 GC。
	gcPercent atomic.Int32

	// memoryLimit is the soft memory limit in bytes.
	//
	// Initialized from GOMEMLIMIT. GOMEMLIMIT=off is equivalent to MaxInt64
	// which means no soft memory limit in practice.
	//
	// This is an int64 instead of a uint64 to more easily maintain parity with
	// the SetMemoryLimit API, which sets a maximum at MaxInt64. This value
	// should never be negative.
	memoryLimit atomic.Int64

	// heapMinimum is the minimum heap size at which to trigger GC.
	// For small heaps, this overrides the usual GOGC*live set rule.
	//
	// When there is a very small live set but a lot of allocation, simply
	// collecting when the heap reaches GOGC*live results in many GC
	// cycles and high total per-GC overhead. This minimum amortizes this
	// per-GC overhead while keeping the heap reasonably small.
	//
	// During initialization this is set to 4MB*GOGC/100. In the case of
	// GOGC==0, this will set heapMinimum to 0, resulting in constant
	// collection even when the heap size is small, which is useful for
	// debugging.
	heapMinimum uint64

	// runway is the amount of runway in heap bytes allocated by the
	// application that we want to give the GC once it starts.
	//
	// This is computed from consMark during mark termination.
	runway atomic.Uint64

	// consMark is the estimated per-CPU consMark ratio for the application.
	//
	// It represents the ratio between the application's allocation
	// rate, as bytes allocated per CPU-time, and the GC's scan rate,
	// as bytes scanned per CPU-time.
	// The units of this ratio are (B / cpu-ns) / (B / cpu-ns).
	//
	// At a high level, this value is computed as the bytes of memory
	// allocated (cons) per unit of scan work completed (mark) in a GC
	// cycle, divided by the CPU time spent on each activity.
	//
	// Updated at the end of each GC cycle, in endCycle.
	consMark float64

	// lastConsMark is the computed cons/mark value for the previous 4 GC
	// cycles. Note that this is *not* the last value of consMark, but the
	// measured cons/mark value in endCycle.
	lastConsMark [4]float64

	// 是基于 gcPercent 计算得出的下一次垃圾回收结束时的 heapLive 垃圾回收认为还活着的字节数目标。
	//
	// 如果 gcPercent 被禁用，则设置为 ^uint64(0)。
	gcPercentHeapGoal atomic.Uint64

	// 是确保最小清扫距离的最小触发阈值。
	//
	// 这个界限也是特殊的，因为它同时应用于触发阈值 *和* 目标（所有其他触发界限必须基于目标）。
	//
	// 它在 commit 时间预先计算。理论是，在没有参数如 gcPercent 的突然变化的情况下，
	// 触发阈值将被选择为始终给清扫器留有足够的缓冲空间。然而，如果发生了这样的变化，
	// 可能会突然将触发阈值大幅度提前，这时我们需要确保清扫器仍然有足够的缓冲空间。
	sweepDistMinTrigger atomic.Uint64

	// triggered 是当前 GC 周期实际触发的点。
	// 仅在 GC 循环的标记阶段有效，否则设置为 ^uint64（0）。
	//
	// 在世界停止时更新。
	triggered uint64

	// lastHeapGoal is the value of heapGoal at the moment the last GC
	// ended. Note that this is distinct from the last value heapGoal had,
	// because it could change if e.g. gcPercent changes.
	//
	// Read and written with the world stopped or with mheap_.lock held.
	lastHeapGoal uint64

	// 表示垃圾回收认为还活着的字节数。这是指最近一次垃圾回收保留的字节数加上自那以后分配的字节数。
	// heapLive ≤ memstats.totalAlloc - memstats.totalFree，因为
	// heapAlloc 包括了那些还未被标记但在清扫前仍保留的对象（因此随着分配而上升，随着清扫而下降），
	// 而 heapLive 排除了这些对象（因此只在两次垃圾回收之间上升）。
	//
	// 为了减少竞争，这个值仅在从 mcentral 获取 span 时更新，
	// 并且在这个时候它统计了该 span 中所有未分配的槽位（这些槽位将在 mcache 获取另一个 span 之前被分配）。
	// 因此，它稍微高估了“真实的”存活堆大小。
	// 高估比低估好，因为 1) 这导致垃圾回收比必要时更早触发，而不是可能太晚触发，
	// 2) 这导致保守的垃圾回收速率，而不是可能导致过低的垃圾回收速率。
	//
	// 每当更新这个值时，都要调用 traceHeapAlloc() 和
	// 当前 gcControllerState 的 revise() 方法。
	heapLive atomic.Uint64

	// heapScan is the number of bytes of "scannable" heap. This is the
	// live heap (as counted by heapLive), but omitting no-scan objects and
	// no-scan tails of objects.
	//
	// This value is fixed at the start of a GC cycle. It represents the
	// maximum scannable heap.
	heapScan atomic.Uint64

	// lastHeapScan is the number of bytes of heap that were scanned
	// last GC cycle. It is the same as heapMarked, but only
	// includes the "scannable" parts of objects.
	//
	// Updated when the world is stopped.
	lastHeapScan uint64

	// lastStackScan is the number of bytes of stack that were scanned
	// last GC cycle.
	lastStackScan atomic.Uint64

	// maxStackScan is the amount of allocated goroutine stack space in
	// use by goroutines.
	//
	// This number tracks allocated goroutine stack space rather than used
	// goroutine stack space (i.e. what is actually scanned) because used
	// goroutine stack space is much harder to measure cheaply. By using
	// allocated space, we make an overestimate; this is OK, it's better
	// to conservatively overcount than undercount.
	maxStackScan atomic.Uint64

	// globalsScan is the total amount of global variable space
	// that is scannable.
	globalsScan atomic.Uint64

	// 是前一个 GC 标记的字节数
	heapMarked uint64

	// 这些值在本周期内通过原子操作进行更新。更新发生在有限大小的批次中，
	// 因为它们在整个周期中既被写入也被读取。在周期结束时，heapScanWork 表示保留的堆中有多少是可扫描的。
	//
	// 目前这些值是以字节为单位进行度量的。对于大多数用途来说，这是一个不透明的工作单位，
	// 但对于估算来说，定义是很重要的。
	//
	// 注意：stackScanWork 只包括扫描的栈空间，而不包括所有分配的栈空间。
	heapScanWork    atomic.Int64 // 是本周期内执行的堆扫描工作的总量。
	stackScanWork   atomic.Int64 // 是本周期内执行的栈扫描工作的总量。
	globalsScanWork atomic.Int64 // 是本周期内执行的全局变量扫描工作的总量。

	// bgScanCredit is the scan work credit accumulated by the concurrent
	// background scan. This credit is accumulated by the background scan
	// and stolen by mutator assists.  Updates occur in bounded batches,
	// since it is both written and read throughout the cycle.
	bgScanCredit atomic.Int64

	// assistTime is the nanoseconds spent in mutator assists
	// during this cycle. This is updated atomically, and must also
	// be updated atomically even during a STW, because it is read
	// by sysmon. Updates occur in bounded batches, since it is both
	// written and read throughout the cycle.
	assistTime atomic.Int64

	// dedicatedMarkTime is the nanoseconds spent in dedicated mark workers
	// during this cycle. This is updated at the end of the concurrent mark
	// phase.
	dedicatedMarkTime atomic.Int64

	// fractionalMarkTime is the nanoseconds spent in the fractional mark
	// worker during this cycle. This is updated throughout the cycle and
	// will be up-to-date if the fractional mark worker is not currently
	// running.
	fractionalMarkTime atomic.Int64

	// idleMarkTime is the nanoseconds spent in idle marking during this
	// cycle. This is updated throughout the cycle.
	idleMarkTime atomic.Int64

	// markStartTime is the absolute start time in nanoseconds
	// that assists and background mark workers started.
	markStartTime int64

	// dedicatedMarkWorkersNeeded is the number of dedicated mark workers
	// that need to be started. This is computed at the beginning of each
	// cycle and decremented as dedicated mark workers get started.
	dedicatedMarkWorkersNeeded atomic.Int64

	// idleMarkWorkers is two packed int32 values in a single uint64.
	// These two values are always updated simultaneously.
	//
	// The bottom int32 is the current number of idle mark workers executing.
	//
	// The top int32 is the maximum number of idle mark workers allowed to
	// execute concurrently. Normally, this number is just gomaxprocs. However,
	// during periodic GC cycles it is set to 0 because the system is idle
	// anyway; there's no need to go full blast on all of GOMAXPROCS.
	//
	// The maximum number of idle mark workers is used to prevent new workers
	// from starting, but it is not a hard maximum. It is possible (but
	// exceedingly rare) for the current number of idle mark workers to
	// transiently exceed the maximum. This could happen if the maximum changes
	// just after a GC ends, and an M with no P.
	//
	// Note that if we have no dedicated mark workers, we set this value to
	// 1 in this case we only have fractional GC workers which aren't scheduled
	// strictly enough to ensure GC progress. As a result, idle-priority mark
	// workers are vital to GC progress in these situations.
	//
	// For example, consider a situation in which goroutines block on the GC
	// (such as via runtime.GOMAXPROCS) and only fractional mark workers are
	// scheduled (e.g. GOMAXPROCS=1). Without idle-priority mark workers, the
	// last running M might skip scheduling a fractional mark worker if its
	// utilization goal is met, such that once it goes to sleep (because there's
	// nothing to do), there will be nothing else to spin up a new M for the
	// fractional worker in the future, stalling GC progress and causing a
	// deadlock. However, idle-priority workers will *always* run when there is
	// nothing left to do, ensuring the GC makes progress.
	//
	// See github.com/golang/go/issues/44163 for more details.
	idleMarkWorkers atomic.Uint64

	// assistWorkPerByte is the ratio of scan work to allocated
	// bytes that should be performed by mutator assists. This is
	// computed at the beginning of each cycle and updated every
	// time heapScan is updated.
	assistWorkPerByte atomic.Float64

	// assistBytesPerWork is 1/assistWorkPerByte.
	//
	// Note that because this is read and written independently
	// from assistWorkPerByte users may notice a skew between
	// the two values, and such a state should be safe.
	assistBytesPerWork atomic.Float64

	// fractionalUtilizationGoal is the fraction of wall clock
	// time that should be spent in the fractional mark worker on
	// each P that isn't running a dedicated worker.
	//
	// For example, if the utilization goal is 25% and there are
	// no dedicated workers, this will be 0.25. If the goal is
	// 25%, there is one dedicated worker, and GOMAXPROCS is 5,
	// this will be 0.05 to make up the missing 5%.
	//
	// If this is zero, no fractional workers are needed.
	fractionalUtilizationGoal float64

	// These memory stats are effectively duplicates of fields from
	// memstats.heapStats but are updated atomically or with the world
	// stopped and don't provide the same consistency guarantees.
	//
	// Because the runtime is responsible for managing a memory limit, it's
	// useful to couple these stats more tightly to the gcController, which
	// is intimately connected to how that memory limit is maintained.
	heapInUse    sysMemStat    // bytes in mSpanInUse spans
	heapReleased sysMemStat    // bytes released to the OS
	heapFree     sysMemStat    // bytes not in any span, but not released to the OS
	totalAlloc   atomic.Uint64 // total bytes allocated
	totalFree    atomic.Uint64 // total bytes freed
	mappedReady  atomic.Uint64 // total virtual memory in the Ready state (see mem.go).

	// test indicates that this is a test-only copy of gcControllerState.
	test bool

	_ cpu.CacheLinePad
}

// 方法初始化垃圾回收控制器的状态。
// gcPercent 是从环境变量 GOGC 读取的初始垃圾回收百分比。
// memoryLimit 是从环境变量 GOMEMLIMIT 读取的初始最大堆内存限制。
func (c *gcControllerState) init(gcPercent int32, memoryLimit int64) {
	c.heapMinimum = defaultHeapMinimum // 设置初始的最小堆大小。默认: 4194304 == 4 MB
	c.triggered = ^uint64(0)           // 初始化触发阈值为最大值，表示垃圾回收尚未触发。
	c.setGCPercent(gcPercent)          // 设置初始的垃圾回收百分比。
	c.setMemoryLimit(memoryLimit)      // 设置初始的最大堆内存限制。
	c.commit(true)                     // 初始化计算所有步调参数，这些参数用于导出垃圾回收的触发阈值和堆的目标大小。

	// 注意: 不需要调用 traceHeapGoal。在初始化期间，跟踪功能从未启用。
	// 注意: 无需调用 revise；在初始化期间没有启用垃圾回收。
}

// startCycle resets the GC controller's state and computes estimates
// for a new GC cycle. The caller must hold worldsema and the world
// must be stopped.
func (c *gcControllerState) startCycle(markStartTime int64, procs int, trigger gcTrigger) {
	c.heapScanWork.Store(0)
	c.stackScanWork.Store(0)
	c.globalsScanWork.Store(0)
	c.bgScanCredit.Store(0)
	c.assistTime.Store(0)
	c.dedicatedMarkTime.Store(0)
	c.fractionalMarkTime.Store(0)
	c.idleMarkTime.Store(0)
	c.markStartTime = markStartTime
	c.triggered = c.heapLive.Load()

	// Compute the background mark utilization goal. In general,
	// this may not come out exactly. We round the number of
	// dedicated workers so that the utilization is closest to
	// 25%. For small GOMAXPROCS, this would introduce too much
	// error, so we add fractional workers in that case.
	totalUtilizationGoal := float64(procs) * gcBackgroundUtilization
	dedicatedMarkWorkersNeeded := int64(totalUtilizationGoal + 0.5)
	utilError := float64(dedicatedMarkWorkersNeeded)/totalUtilizationGoal - 1
	const maxUtilError = 0.3
	if utilError < -maxUtilError || utilError > maxUtilError {
		// Rounding put us more than 30% off our goal. With
		// gcBackgroundUtilization of 25%, this happens for
		// GOMAXPROCS<=3 or GOMAXPROCS=6. Enable fractional
		// workers to compensate.
		if float64(dedicatedMarkWorkersNeeded) > totalUtilizationGoal {
			// Too many dedicated workers.
			dedicatedMarkWorkersNeeded--
		}
		c.fractionalUtilizationGoal = (totalUtilizationGoal - float64(dedicatedMarkWorkersNeeded)) / float64(procs)
	} else {
		c.fractionalUtilizationGoal = 0
	}

	// In STW mode, we just want dedicated workers.
	if debug.gcstoptheworld > 0 {
		dedicatedMarkWorkersNeeded = int64(procs)
		c.fractionalUtilizationGoal = 0
	}

	// Clear per-P state
	for _, p := range allp {
		p.gcAssistTime = 0
		p.gcFractionalMarkTime = 0
	}

	if trigger.kind == gcTriggerTime {
		// During a periodic GC cycle, reduce the number of idle mark workers
		// required. However, we need at least one dedicated mark worker or
		// idle GC worker to ensure GC progress in some scenarios (see comment
		// on maxIdleMarkWorkers).
		if dedicatedMarkWorkersNeeded > 0 {
			c.setMaxIdleMarkWorkers(0)
		} else {
			// TODO(mknyszek): The fundamental reason why we need this is because
			// we can't count on the fractional mark worker to get scheduled.
			// Fix that by ensuring it gets scheduled according to its quota even
			// if the rest of the application is idle.
			c.setMaxIdleMarkWorkers(1)
		}
	} else {
		// N.B. gomaxprocs and dedicatedMarkWorkersNeeded are guaranteed not to
		// change during a GC cycle.
		c.setMaxIdleMarkWorkers(int32(procs) - int32(dedicatedMarkWorkersNeeded))
	}

	// Compute initial values for controls that are updated
	// throughout the cycle.
	c.dedicatedMarkWorkersNeeded.Store(dedicatedMarkWorkersNeeded)
	c.revise()

	if debug.gcpacertrace > 0 {
		heapGoal := c.heapGoal()
		assistRatio := c.assistWorkPerByte.Load()
		print("pacer: assist ratio=", assistRatio,
			" (scan ", gcController.heapScan.Load()>>20, " MB in ",
			work.initialHeapLive>>20, "->",
			heapGoal>>20, " MB)",
			" workers=", dedicatedMarkWorkersNeeded,
			"+", c.fractionalUtilizationGoal, "\n")
	}
}

// revise updates the assist ratio during the GC cycle to account for
// improved estimates. This should be called whenever gcController.heapScan,
// gcController.heapLive, or if any inputs to gcController.heapGoal are
// updated. It is safe to call concurrently, but it may race with other
// calls to revise.
//
// The result of this race is that the two assist ratio values may not line
// up or may be stale. In practice this is OK because the assist ratio
// moves slowly throughout a GC cycle, and the assist ratio is a best-effort
// heuristic anyway. Furthermore, no part of the heuristic depends on
// the two assist ratio values being exact reciprocals of one another, since
// the two values are used to convert values from different sources.
//
// The worst case result of this raciness is that we may miss a larger shift
// in the ratio (say, if we decide to pace more aggressively against the
// hard heap goal) but even this "hard goal" is best-effort (see #40460).
// The dedicated GC should ensure we don't exceed the hard goal by too much
// in the rare case we do exceed it.
//
// It should only be called when gcBlackenEnabled != 0 (because this
// is when assists are enabled and the necessary statistics are
// available).
func (c *gcControllerState) revise() {
	gcPercent := c.gcPercent.Load()
	if gcPercent < 0 {
		// If GC is disabled but we're running a forced GC,
		// act like GOGC is huge for the below calculations.
		gcPercent = 100000
	}
	live := c.heapLive.Load()
	scan := c.heapScan.Load()
	work := c.heapScanWork.Load() + c.stackScanWork.Load() + c.globalsScanWork.Load()

	// Assume we're under the soft goal. Pace GC to complete at
	// heapGoal assuming the heap is in steady-state.
	heapGoal := int64(c.heapGoal())

	// The expected scan work is computed as the amount of bytes scanned last
	// GC cycle (both heap and stack), plus our estimate of globals work for this cycle.
	scanWorkExpected := int64(c.lastHeapScan + c.lastStackScan.Load() + c.globalsScan.Load())

	// maxScanWork is a worst-case estimate of the amount of scan work that
	// needs to be performed in this GC cycle. Specifically, it represents
	// the case where *all* scannable memory turns out to be live, and
	// *all* allocated stack space is scannable.
	maxStackScan := c.maxStackScan.Load()
	maxScanWork := int64(scan + maxStackScan + c.globalsScan.Load())
	if work > scanWorkExpected {
		// We've already done more scan work than expected. Because our expectation
		// is based on a steady-state scannable heap size, we assume this means our
		// heap is growing. Compute a new heap goal that takes our existing runway
		// computed for scanWorkExpected and extrapolates it to maxScanWork, the worst-case
		// scan work. This keeps our assist ratio stable if the heap continues to grow.
		//
		// The effect of this mechanism is that assists stay flat in the face of heap
		// growths. It's OK to use more memory this cycle to scan all the live heap,
		// because the next GC cycle is inevitably going to use *at least* that much
		// memory anyway.
		extHeapGoal := int64(float64(heapGoal-int64(c.triggered))/float64(scanWorkExpected)*float64(maxScanWork)) + int64(c.triggered)
		scanWorkExpected = maxScanWork

		// hardGoal is a hard limit on the amount that we're willing to push back the
		// heap goal, and that's twice the heap goal (i.e. if GOGC=100 and the heap and/or
		// stacks and/or globals grow to twice their size, this limits the current GC cycle's
		// growth to 4x the original live heap's size).
		//
		// This maintains the invariant that we use no more memory than the next GC cycle
		// will anyway.
		hardGoal := int64((1.0 + float64(gcPercent)/100.0) * float64(heapGoal))
		if extHeapGoal > hardGoal {
			extHeapGoal = hardGoal
		}
		heapGoal = extHeapGoal
	}
	if int64(live) > heapGoal {
		// We're already past our heap goal, even the extrapolated one.
		// Leave ourselves some extra runway, so in the worst case we
		// finish by that point.
		const maxOvershoot = 1.1
		heapGoal = int64(float64(heapGoal) * maxOvershoot)

		// Compute the upper bound on the scan work remaining.
		scanWorkExpected = maxScanWork
	}

	// Compute the remaining scan work estimate.
	//
	// Note that we currently count allocations during GC as both
	// scannable heap (heapScan) and scan work completed
	// (scanWork), so allocation will change this difference
	// slowly in the soft regime and not at all in the hard
	// regime.
	scanWorkRemaining := scanWorkExpected - work
	if scanWorkRemaining < 1000 {
		// We set a somewhat arbitrary lower bound on
		// remaining scan work since if we aim a little high,
		// we can miss by a little.
		//
		// We *do* need to enforce that this is at least 1,
		// since marking is racy and double-scanning objects
		// may legitimately make the remaining scan work
		// negative, even in the hard goal regime.
		scanWorkRemaining = 1000
	}

	// Compute the heap distance remaining.
	heapRemaining := heapGoal - int64(live)
	if heapRemaining <= 0 {
		// This shouldn't happen, but if it does, avoid
		// dividing by zero or setting the assist negative.
		heapRemaining = 1
	}

	// Compute the mutator assist ratio so by the time the mutator
	// allocates the remaining heap bytes up to heapGoal, it will
	// have done (or stolen) the remaining amount of scan work.
	// Note that the assist ratio values are updated atomically
	// but not together. This means there may be some degree of
	// skew between the two values. This is generally OK as the
	// values shift relatively slowly over the course of a GC
	// cycle.
	assistWorkPerByte := float64(scanWorkRemaining) / float64(heapRemaining)
	assistBytesPerWork := float64(heapRemaining) / float64(scanWorkRemaining)
	c.assistWorkPerByte.Store(assistWorkPerByte)
	c.assistBytesPerWork.Store(assistBytesPerWork)
}

// endCycle 用于计算下一次垃圾回收周期的保守标记比率（cons-mark estimate）。
// userForced 表示当前的 GC 周期是否是由应用程序强制触发的。
//
// endCycle 用于计算下一次垃圾回收周期的保守标记比率（cons-mark estimate）。
// userForced 表示当前的 GC 周期是否是由应用程序强制触发的。
func (c *gcControllerState) endCycle(now int64, procs int, userForced bool) {
	// 记录上次垃圾回收的目标堆大小，用于 scavenger。
	// 我们即将更新堆目标。
	gcController.lastHeapGoal = c.heapGoal()

	// 计算协助开启的时间长度。
	assistDuration := now - c.markStartTime

	// 假设后台标记达到了利用率目标。
	utilization := gcBackgroundUtilization
	// 添加协助利用率；避免除以零的情况。
	if assistDuration > 0 {
		utilization += float64(c.assistTime.Load()) / float64(assistDuration*int64(procs))
	}

	if c.heapLive.Load() <= c.triggered {
		// 不应该发生这种情况，但为了安全起见，在 GC 极端短暂的情况下也要考虑。
		// 在这种情况下，唯一合理的值是 c.heapLive - c.triggered 为 0，这意味着 GC 如此短暂以至于无关紧要。
		// 忽略这种情况，不更新任何值。
		return
	}
	idleUtilization := 0.0
	if assistDuration > 0 {
		idleUtilization = float64(c.idleMarkTime.Load()) / float64(assistDuration*int64(procs))
	}

	// 确定保守/标记比率。
	//
	// 我们想要的分子和分母单位都是 B/cpu-ns。
	// 我们可以通过将分配或扫描的字节数除以这些操作所需的时间来获得。
	// 对于分配，CPU 时间是
	//
	//    assistDuration * procs * (1 - utilization)
	//
	// 其中 utilization 包括后台 GC 工作器和协助。它不包括空闲 GC 工作时间，因为在理论上，mutator 可以随时获取空闲时间。
	//
	// 对于扫描，CPU 时间是
	//
	//    assistDuration * procs * (utilization + idleUtilization)
	//
	// 在这种情况下，我们包括空闲利用率，因为这是额外的 CPU 时间，GC 可以使用它。
	//
	// 实际上，空闲 GC 时间在这里有点双倍计算，但它与其他类型的 GC 工作相比非常奇怪，因为它非常灵活。具体来说，mutator 总是可以自由地获取它。
	//
	// 所以这个计算实际上是：
	//     (heapLive - triggered) / (assistDuration * procs * (1 - utilization)) /
	//         (scanWork) / (assistDuration * procs * (utilization + idleUtilization))
	//
	// 注意，因为我们只关心比率，assistDuration 和 procs 会相互抵消。
	scanWork := c.heapScanWork.Load() + c.stackScanWork.Load() + c.globalsScanWork.Load()
	currentConsMark := (float64(c.heapLive.Load()-c.triggered) * (utilization + idleUtilization)) /
		(float64(scanWork) * (1 - utilization))

	// 更新我们的保守/标记估计值。这是我们刚刚计算的值与最后四个测量的保守/标记值中的最大值。
	// 我们取最大值的原因是为了偏向较少的协助，以牺牲额外的 GC 周期（提前开始）来应对嘈杂的保守/标记测量。
	oldConsMark := c.consMark
	c.consMark = currentConsMark
	for i := range c.lastConsMark {
		if c.lastConsMark[i] > c.consMark {
			c.consMark = c.lastConsMark[i]
		}
	}
	copy(c.lastConsMark[:], c.lastConsMark[1:])
	c.lastConsMark[len(c.lastConsMark)-1] = currentConsMark

	if debug.gcpacertrace > 0 {
		printlock()
		goal := gcGoalUtilization * 100
		print("pacer: ", int(utilization*100), "% CPU (", int(goal), " exp.) for ")
		print(c.heapScanWork.Load(), "+", c.stackScanWork.Load(), "+", c.globalsScanWork.Load(), " B work (", c.lastHeapScan+c.lastStackScan.Load()+c.globalsScan.Load(), " B exp.) ")
		live := c.heapLive.Load()
		print("in ", c.triggered, " B -> ", live, " B (∆goal ", int64(live)-int64(c.lastHeapGoal), ", cons/mark ", oldConsMark, ")")
		println()
		printunlock()
	}
}

// enlistWorker encourages another dedicated mark worker to start on
// another P if there are spare worker slots. It is used by putfull
// when more work is made available.
//
//go:nowritebarrier
func (c *gcControllerState) enlistWorker() {
	// If there are idle Ps, wake one so it will run an idle worker.
	// NOTE: This is suspected of causing deadlocks. See golang.org/issue/19112.
	//
	//	if sched.npidle.Load() != 0 && sched.nmspinning.Load() == 0 {
	//		wakep()
	//		return
	//	}

	// There are no idle Ps. If we need more dedicated workers,
	// try to preempt a running P so it will switch to a worker.
	if c.dedicatedMarkWorkersNeeded.Load() <= 0 {
		return
	}
	// Pick a random other P to preempt.
	if gomaxprocs <= 1 {
		return
	}
	gp := getg()
	if gp == nil || gp.m == nil || gp.m.p == 0 {
		return
	}
	myID := gp.m.p.ptr().id
	for tries := 0; tries < 5; tries++ {
		id := int32(fastrandn(uint32(gomaxprocs - 1)))
		if id >= myID {
			id++
		}
		p := allp[id]
		if p.status != _Prunning {
			continue
		}
		if preemptone(p) {
			return
		}
	}
}

// findRunnableGCWorker returns a background mark worker for pp if it
// should be run. This must only be called when gcBlackenEnabled != 0.
func (c *gcControllerState) findRunnableGCWorker(pp *p, now int64) (*g, int64) {
	if gcBlackenEnabled == 0 {
		throw("gcControllerState.findRunnable: blackening not enabled")
	}

	// Since we have the current time, check if the GC CPU limiter
	// hasn't had an update in a while. This check is necessary in
	// case the limiter is on but hasn't been checked in a while and
	// so may have left sufficient headroom to turn off again.
	if now == 0 {
		now = nanotime()
	}
	if gcCPULimiter.needUpdate(now) {
		gcCPULimiter.update(now)
	}

	if !gcMarkWorkAvailable(pp) {
		// No work to be done right now. This can happen at
		// the end of the mark phase when there are still
		// assists tapering off. Don't bother running a worker
		// now because it'll just return immediately.
		return nil, now
	}

	// Grab a worker before we commit to running below.
	node := (*gcBgMarkWorkerNode)(gcBgMarkWorkerPool.pop())
	if node == nil {
		// There is at least one worker per P, so normally there are
		// enough workers to run on all Ps, if necessary. However, once
		// a worker enters gcMarkDone it may park without rejoining the
		// pool, thus freeing a P with no corresponding worker.
		// gcMarkDone never depends on another worker doing work, so it
		// is safe to simply do nothing here.
		//
		// If gcMarkDone bails out without completing the mark phase,
		// it will always do so with queued global work. Thus, that P
		// will be immediately eligible to re-run the worker G it was
		// just using, ensuring work can complete.
		return nil, now
	}

	decIfPositive := func(val *atomic.Int64) bool {
		for {
			v := val.Load()
			if v <= 0 {
				return false
			}

			if val.CompareAndSwap(v, v-1) {
				return true
			}
		}
	}

	if decIfPositive(&c.dedicatedMarkWorkersNeeded) {
		// This P is now dedicated to marking until the end of
		// the concurrent mark phase.
		pp.gcMarkWorkerMode = gcMarkWorkerDedicatedMode
	} else if c.fractionalUtilizationGoal == 0 {
		// No need for fractional workers.
		gcBgMarkWorkerPool.push(&node.node)
		return nil, now
	} else {
		// Is this P behind on the fractional utilization
		// goal?
		//
		// This should be kept in sync with pollFractionalWorkerExit.
		delta := now - c.markStartTime
		if delta > 0 && float64(pp.gcFractionalMarkTime)/float64(delta) > c.fractionalUtilizationGoal {
			// Nope. No need to run a fractional worker.
			gcBgMarkWorkerPool.push(&node.node)
			return nil, now
		}
		// Run a fractional worker.
		pp.gcMarkWorkerMode = gcMarkWorkerFractionalMode
	}

	// Run the background mark worker.
	gp := node.gp.ptr()
	casgstatus(gp, _Gwaiting, _Grunnable)
	if traceEnabled() {
		traceGoUnpark(gp, 0)
	}
	return gp, now
}

// resetLive sets up the controller state for the next mark phase after the end
// of the previous one. Must be called after endCycle and before commit, before
// the world is started.
//
// The world must be stopped.
func (c *gcControllerState) resetLive(bytesMarked uint64) {
	c.heapMarked = bytesMarked
	c.heapLive.Store(bytesMarked)
	c.heapScan.Store(uint64(c.heapScanWork.Load()))
	c.lastHeapScan = uint64(c.heapScanWork.Load())
	c.lastStackScan.Store(uint64(c.stackScanWork.Load()))
	c.triggered = ^uint64(0) // Reset triggered.

	// heapLive was updated, so emit a trace event.
	if traceEnabled() {
		traceHeapAlloc(bytesMarked)
	}
}

// markWorkerStop must be called whenever a mark worker stops executing.
//
// It updates mark work accounting in the controller by a duration of
// work in nanoseconds and other bookkeeping.
//
// Safe to execute at any time.
func (c *gcControllerState) markWorkerStop(mode gcMarkWorkerMode, duration int64) {
	switch mode {
	case gcMarkWorkerDedicatedMode:
		c.dedicatedMarkTime.Add(duration)
		c.dedicatedMarkWorkersNeeded.Add(1)
	case gcMarkWorkerFractionalMode:
		c.fractionalMarkTime.Add(duration)
	case gcMarkWorkerIdleMode:
		c.idleMarkTime.Add(duration)
		c.removeIdleMarkWorker()
	default:
		throw("markWorkerStop: unknown mark worker mode")
	}
}

func (c *gcControllerState) update(dHeapLive, dHeapScan int64) {
	if dHeapLive != 0 {
		live := gcController.heapLive.Add(dHeapLive)
		if traceEnabled() {
			// gcController.heapLive changed.
			traceHeapAlloc(live)
		}
	}
	if gcBlackenEnabled == 0 {
		// Update heapScan when we're not in a current GC. It is fixed
		// at the beginning of a cycle.
		if dHeapScan != 0 {
			gcController.heapScan.Add(dHeapScan)
		}
	} else {
		// gcController.heapLive changed.
		c.revise()
	}
}

func (c *gcControllerState) addScannableStack(pp *p, amount int64) {
	if pp == nil {
		c.maxStackScan.Add(amount)
		return
	}
	pp.maxStackScanDelta += amount
	if pp.maxStackScanDelta >= maxStackScanSlack || pp.maxStackScanDelta <= -maxStackScanSlack {
		c.maxStackScan.Add(pp.maxStackScanDelta)
		pp.maxStackScanDelta = 0
	}
}

func (c *gcControllerState) addGlobals(amount int64) {
	c.globalsScan.Add(amount)
}

// heapGoal 返回当前堆目标。
func (c *gcControllerState) heapGoal() uint64 {
	goal, _ := c.heapGoalInternal()
	return goal
}

// 是 heapGoal 方法的实现，获取堆目标和最小触发阈值
//
// 返回的 minTrigger 始终 <= goal。
func (c *gcControllerState) heapGoalInternal() (goal, minTrigger uint64) {
	// 从为 gcPercent 计算的目标开始。
	// gcPercent 是一个配置参数，用来指定堆大小的百分比作为垃圾回收的目标
	// 由`debug.SetGCPercent` 进行控制。
	goal = c.gcPercentHeapGoal.Load()

	// 检查基于内存限制的目标是否更小，如果是，则选择那个目标。
	if newGoal := c.memoryLimitHeapGoal(); newGoal < goal {
		goal = newGoal // 选择基于内存限制的目标
	} else {
		// 我们不受内存限制目标的限制，因此执行一系列调整，
		// 这些调整可能会在各种情况下将目标向前移动。

		sweepDistTrigger := c.sweepDistMinTrigger.Load()
		if sweepDistTrigger > goal {
			// 设置目标以保持自上次调用 commit 以来的最小清扫距离。
			// 注意：我们永远不想在内存限制模式下这样做，因为它可能会推高目标。
			goal = sweepDistTrigger
		}
		// 由于我们在内存限制模式下忽略清扫距离触发器，
		// 我们需要确保不会将其传播到触发阈值，因为它可能会导致 触发阈值 < 目标 的不变式违反。
		minTrigger = sweepDistTrigger

		// 确保堆目标至少比触发点稍大一些。这可能不是事实，如果垃圾回收开始延迟，
		// 或者将 gcController.heapLive 推过触发阈值的分配很大，或者触发阈值非常接近 GOGC。
		// 协助与这个距离成比例，所以即使这意味着稍微超过 GOGC 目标，也要强制执行最小距离。
		//
		// 如果我们处于内存限制模式下，则忽略这一点：我们宁愿让垃圾回收对离目标有多近作出强烈的反应，
		// 而不是以这种方式回退目标，这可能会导致我们超过内存限制。
		const minRunway = 64 << 10
		if c.triggered != ^uint64(0) && goal < c.triggered+minRunway {
			// 设置 goal = c.triggered + minRunway，确保有足够的跑道距离
			// 这确保了即使目标稍微超过 GOGC 目标，也有足够的空间进行协助
			goal = c.triggered + minRunway
		}
	}
	return
}

// memoryLimitHeapGoal returns a heap goal derived from memoryLimit.
func (c *gcControllerState) memoryLimitHeapGoal() uint64 {
	// Start by pulling out some values we'll need. Be careful about overflow.
	var heapFree, heapAlloc, mappedReady uint64
	for {
		heapFree = c.heapFree.load()                         // Free and unscavenged memory.
		heapAlloc = c.totalAlloc.Load() - c.totalFree.Load() // Heap object bytes in use.
		mappedReady = c.mappedReady.Load()                   // Total unreleased mapped memory.
		if heapFree+heapAlloc <= mappedReady {
			break
		}
		// It is impossible for total unreleased mapped memory to exceed heap memory, but
		// because these stats are updated independently, we may observe a partial update
		// including only some values. Thus, we appear to break the invariant. However,
		// this condition is necessarily transient, so just try again. In the case of a
		// persistent accounting error, we'll deadlock here.
	}

	// Below we compute a goal from memoryLimit. There are a few things to be aware of.
	// Firstly, the memoryLimit does not easily compare to the heap goal: the former
	// is total mapped memory by the runtime that hasn't been released, while the latter is
	// only heap object memory. Intuitively, the way we convert from one to the other is to
	// subtract everything from memoryLimit that both contributes to the memory limit (so,
	// ignore scavenged memory) and doesn't contain heap objects. This isn't quite what
	// lines up with reality, but it's a good starting point.
	//
	// In practice this computation looks like the following:
	//
	//    goal := memoryLimit - ((mappedReady - heapFree - heapAlloc) + max(mappedReady - memoryLimit, 0))
	//                    ^1                                    ^2
	//    goal -= goal / 100 * memoryLimitHeapGoalHeadroomPercent
	//    ^3
	//
	// Let's break this down.
	//
	// The first term (marker 1) is everything that contributes to the memory limit and isn't
	// or couldn't become heap objects. It represents, broadly speaking, non-heap overheads.
	// One oddity you may have noticed is that we also subtract out heapFree, i.e. unscavenged
	// memory that may contain heap objects in the future.
	//
	// Let's take a step back. In an ideal world, this term would look something like just
	// the heap goal. That is, we "reserve" enough space for the heap to grow to the heap
	// goal, and subtract out everything else. This is of course impossible; the definition
	// is circular! However, this impossible definition contains a key insight: the amount
	// we're *going* to use matters just as much as whatever we're currently using.
	//
	// Consider if the heap shrinks to 1/10th its size, leaving behind lots of free and
	// unscavenged memory. mappedReady - heapAlloc will be quite large, because of that free
	// and unscavenged memory, pushing the goal down significantly.
	//
	// heapFree is also safe to exclude from the memory limit because in the steady-state, it's
	// just a pool of memory for future heap allocations, and making new allocations from heapFree
	// memory doesn't increase overall memory use. In transient states, the scavenger and the
	// allocator actively manage the pool of heapFree memory to maintain the memory limit.
	//
	// The second term (marker 2) is the amount of memory we've exceeded the limit by, and is
	// intended to help recover from such a situation. By pushing the heap goal down, we also
	// push the trigger down, triggering and finishing a GC sooner in order to make room for
	// other memory sources. Note that since we're effectively reducing the heap goal by X bytes,
	// we're actually giving more than X bytes of headroom back, because the heap goal is in
	// terms of heap objects, but it takes more than X bytes (e.g. due to fragmentation) to store
	// X bytes worth of objects.
	//
	// The final adjustment (marker 3) reduces the maximum possible memory limit heap goal by
	// memoryLimitHeapGoalPercent. As the name implies, this is to provide additional headroom in
	// the face of pacing inaccuracies, and also to leave a buffer of unscavenged memory so the
	// allocator isn't constantly scavenging. The reduction amount also has a fixed minimum
	// (memoryLimitMinHeapGoalHeadroom, not pictured) because the aforementioned pacing inaccuracies
	// disproportionately affect small heaps: as heaps get smaller, the pacer's inputs get fuzzier.
	// Shorter GC cycles and less GC work means noisy external factors like the OS scheduler have a
	// greater impact.

	memoryLimit := uint64(c.memoryLimit.Load())

	// Compute term 1.
	nonHeapMemory := mappedReady - heapFree - heapAlloc

	// Compute term 2.
	var overage uint64
	if mappedReady > memoryLimit {
		overage = mappedReady - memoryLimit
	}

	if nonHeapMemory+overage >= memoryLimit {
		// We're at a point where non-heap memory exceeds the memory limit on its own.
		// There's honestly not much we can do here but just trigger GCs continuously
		// and let the CPU limiter reign that in. Something has to give at this point.
		// Set it to heapMarked, the lowest possible goal.
		return c.heapMarked
	}

	// Compute the goal.
	goal := memoryLimit - (nonHeapMemory + overage)

	// Apply some headroom to the goal to account for pacing inaccuracies and to reduce
	// the impact of scavenging at allocation time in response to a high allocation rate
	// when GOGC=off. See issue #57069. Also, be careful about small limits.
	headroom := goal / 100 * memoryLimitHeapGoalHeadroomPercent
	if headroom < memoryLimitMinHeapGoalHeadroom {
		// Set a fixed minimum to deal with the particularly large effect pacing inaccuracies
		// have for smaller heaps.
		headroom = memoryLimitMinHeapGoalHeadroom
	}
	if goal < headroom || goal-headroom < headroom {
		goal = headroom
	} else {
		goal = goal - headroom
	}
	// Don't let us go below the live heap. A heap goal below the live heap doesn't make sense.
	if goal < c.heapMarked {
		goal = c.heapMarked
	}
	return goal
}

const (
	// These constants determine the bounds on the GC trigger as a fraction
	// of heap bytes allocated between the start of a GC (heapLive == heapMarked)
	// and the end of a GC (heapLive == heapGoal).
	//
	// The constants are obscured in this way for efficiency. The denominator
	// of the fraction is always a power-of-two for a quick division, so that
	// the numerator is a single constant integer multiplication.
	triggerRatioDen = 64

	// The minimum trigger constant was chosen empirically: given a sufficiently
	// fast/scalable allocator with 48 Ps that could drive the trigger ratio
	// to <0.05, this constant causes applications to retain the same peak
	// RSS compared to not having this allocator.
	minTriggerRatioNum = 45 // ~0.7

	// The maximum trigger constant is chosen somewhat arbitrarily, but the
	// current constant has served us well over the years.
	maxTriggerRatioNum = 61 // ~0.95
)

// 返回当前应触发垃圾回收的阈值以及堆的目标大小。
//
// 返回的阈值可以与 heapLive 进行比较以确定是否应触发垃圾回收。
// 因此，每当堆目标可能发生改变时，都应检查垃圾回收触发条件（但出于效率考虑，可能不会在每次变动时检查）。
// 返回 触发阈值,堆目标
func (c *gcControllerState) trigger() (uint64, uint64) {
	goal, minTrigger := c.heapGoalInternal() // 获取堆目标和最小触发阈值。

	// 不变式：触发阈值必须始终小于堆目标。
	//
	// 注意：内存限制为堆目标设定了硬上限，但存活堆可能会超出这个限制。

	// heapMarked是前一个 GC 标记的字节数
	if c.heapMarked >= goal {
		// 目标不应小于 heapMarked，但作为防御措施，我们设置合理的触发阈值，
		// 使垃圾回收在 heapMarked 时连续进行，但如果目标小于这个值，则尊重目标。
		// 表示垃圾回收应在目标大小时触发
		return goal, goal
	}

	// 在这一点以下，c.heapMarked < goal。

	// heapMarked 是我们的绝对最小值，触发阈值的下限可能低于此值。
	if minTrigger < c.heapMarked {
		minTrigger = c.heapMarked
	}

	// 如果让触发阈值降得太低，那么如果应用程序快速分配内存，我们可能会处于
	// 几乎总是进行垃圾回收的状态，此时正在分配黑色对象。这会导致堆不断增长，
	// 最终导致 RSS（常驻集大小）增加。通过设置触发阈值的下限，我们实际上是在说，
	// 我们愿意在垃圾回收期间使用更多 CPU 来防止这种 RSS 的增长。

	// 计算触发阈值的下限, 确保触发阈值不会过低
	triggerLowerBound := uint64(((goal-c.heapMarked)/triggerRatioDen)*minTriggerRatioNum) + c.heapMarked
	if minTrigger < triggerLowerBound {
		minTrigger = triggerLowerBound
	}

	// 计算最大触发阈值
	// 对于较小的堆，将最大触发阈值设置为目标大小与存活堆之间的 maxTriggerRatio。
	// 对于较大的堆，将最大触发阈值设置为目标大小减去最小堆大小。
	maxTrigger := uint64(((goal-c.heapMarked)/triggerRatioDen)*maxTriggerRatioNum) + c.heapMarked
	if goal > defaultHeapMinimum && goal-defaultHeapMinimum > maxTrigger {
		maxTrigger = goal - defaultHeapMinimum
	}
	if maxTrigger < minTrigger {
		maxTrigger = minTrigger
	}

	// 计算触发阈值
	var trigger uint64
	runway := c.runway.Load()
	if runway > goal {
		trigger = minTrigger
	} else {
		trigger = goal - runway
	}
	if trigger < minTrigger {
		trigger = minTrigger
	}
	if trigger > maxTrigger {
		trigger = maxTrigger
	}
	if trigger > goal {
		print("trigger=", trigger, " heapGoal=", goal, "\n")
		print("minTrigger=", minTrigger, " maxTrigger=", maxTrigger, "\n")
		throw("produced a trigger greater than the heap goal")
	}
	return trigger, goal
}

// 重新计算所有步调参数，这些参数用于导出垃圾回收的触发阈值和堆的目标大小。
//
// 这个方法可以在任何时候被调用。如果垃圾回收正在进行并发阶段，它将调整该阶段的步调。
//
// isSweepDone 应该是调用 isSweepDone() 的结果，除非我们在测试中或我们知道我们正在执行垃圾回收周期。
//
// 这个方法依赖于 gcPercent、gcController.heapMarked 和 gcController.heapLive。这些值必须是最新的。
//
// 如果垃圾回收已启用，调用者必须在调用此方法后调用 gcControllerState.revise。
//
// 必须持有 mheap_.lock 锁或停止世界。
func (c *gcControllerState) commit(isSweepDone bool) {
	if !c.test {
		assertWorldStoppedOrLockHeld(&mheap_.lock)
	}

	if isSweepDone {
		// 清扫已完成，因此无需考虑触发阈值的任何限制。
		c.sweepDistMinTrigger.Store(0)
	} else {
		// 并发清扫发生在从 gcController.heapLive 到触发阈值的堆增长中。
		// 确保清扫器有足够的跑道，如果它没有足够的空间。
		c.sweepDistMinTrigger.Store(c.heapLive.Load() + sweepMinHeapDistance)
	}

	// 计算下一次垃圾回收的目标大小，该目标是在上一个周期开始时的存活堆基础上，
	// 增长了 GOGC/100 的比例，再加上非堆来源的垃圾回收工作所需的额外空间。
	gcPercentHeapGoal := ^uint64(0)

	// 从 gcPercent 中加载当前的垃圾回收百分比设置。
	if gcPercent := c.gcPercent.Load(); gcPercent >= 0 {
		// 计算 gcPercentHeapGoal，即下一次垃圾回收的目标大小。
		// c.heapMarked 表示上次垃圾回收后标记的存活堆大小
		// c.lastStackScan.Load() 和 c.globalsScan.Load() 分别表示上次扫描栈和全局变量所消耗的空间
		gcPercentHeapGoal = c.heapMarked + (c.heapMarked+c.lastStackScan.Load()+c.globalsScan.Load())*uint64(gcPercent)/100
	}
	// 应用最小堆大小。它是基于 gcPercent 定义的，并且只能通过调用 commit 的函数来更新。
	if gcPercentHeapGoal < c.heapMinimum {
		gcPercentHeapGoal = c.heapMinimum
	}
	// 存储计算得到的 gcPercentHeapGoal。
	c.gcPercentHeapGoal.Store(gcPercentHeapGoal)

	// 计算我们希望垃圾回收拥有的跑道量，使用我们对 cons/mark 比率的估计。
	//
	// 思路是采用我们预期的扫描工作，并乘以 cons/mark 比率来确定完成该扫描工作所需的时间，
	// 以字节分配的形式表示。这给了我们垃圾回收的跑道。
	//
	// 但是，cons/mark 比率是以每 CPU 秒的速率比率表示的，而在这里我们关心的是 CPU 资源
	// 在应用程序和垃圾回收之间分配的相对速率。
	//
	// 总结来说，我们拥有 B / cpu-ns，而我们想要的是 B / ns。我们通过乘以我们期望的 CPU 资源分配来获得它。
	// 我们选择以 GOMAXPROCS * fraction 的形式表达 CPU 资源。需要注意的是，因为我们这里处理的是比率，
	// 我们可以省略 CPU 核心的数量，因为它们会在分子和分母中出现并相互抵消。
	// 因此，这基本上只是根据我们期望的资源分配来“加权”cons/mark 比率。
	//
	// 此外，通过设置跑道使得 CPU 资源按照这种方式分配，假设 cons/mark 比率是正确的，
	// 我们就使得这种资源分配成为现实。
	c.runway.Store(uint64((c.consMark * (1 - gcGoalUtilization) / (gcGoalUtilization)) * float64(c.lastHeapScan+c.lastStackScan.Load()+c.globalsScan.Load())))
}

// setGCPercent updates gcPercent. commit must be called after.
// Returns the old value of gcPercent.
//
// The world must be stopped, or mheap_.lock must be held.
func (c *gcControllerState) setGCPercent(in int32) int32 {
	if !c.test {
		assertWorldStoppedOrLockHeld(&mheap_.lock)
	}

	out := c.gcPercent.Load()
	if in < 0 {
		in = -1
	}
	c.heapMinimum = defaultHeapMinimum * uint64(in) / 100
	c.gcPercent.Store(in)

	return out
}

//go:linkname setGCPercent runtime/debug.setGCPercent
func setGCPercent(in int32) (out int32) {
	// Run on the system stack since we grab the heap lock.
	systemstack(func() {
		lock(&mheap_.lock)
		out = gcController.setGCPercent(in)
		gcControllerCommit() // 设置GCPercent
		unlock(&mheap_.lock)
	})

	// If we just disabled GC, wait for any concurrent GC mark to
	// finish so we always return with no GC running.
	if in < 0 {
		gcWaitOnMark(work.cycles.Load())
	}

	return out
}

// 函数读取环境变量 GOGC 的值，并将其转换为 int32 类型返回。
// 使用百分比控制垃圾回收的频率
// 如果 GOGC 的值为 "off"，则返回 -1；如果 GOGC 是一个有效的整数，则返回该整数；
// 否则，返回默认值 100。
func readGOGC() int32 {
	// 获取 GOGC 环境变量的值。
	p := gogetenv("GOGC")

	// 检查 GOGC 是否设置为 "off"。
	if p == "off" {
		// 如果 GOGC 设置为 "off"，则返回 -1，表示关闭垃圾回收百分比。
		return -1
	}

	// 尝试将 GOGC 的值转换为 int32 类型。
	if n, ok := atoi32(p); ok {
		return n
	}

	// 如果 GOGC 的值不是 "off" 也不是有效的整数，则返回默认值 100。
	return 100
}

// setMemoryLimit updates memoryLimit. commit must be called after
// Returns the old value of memoryLimit.
//
// The world must be stopped, or mheap_.lock must be held.
func (c *gcControllerState) setMemoryLimit(in int64) int64 {
	if !c.test {
		assertWorldStoppedOrLockHeld(&mheap_.lock)
	}

	out := c.memoryLimit.Load()
	if in >= 0 {
		c.memoryLimit.Store(in)
	}

	return out
}

//go:linkname setMemoryLimit runtime/debug.setMemoryLimit
func setMemoryLimit(in int64) (out int64) {
	// Run on the system stack since we grab the heap lock.
	systemstack(func() {
		lock(&mheap_.lock)
		out = gcController.setMemoryLimit(in)
		if in < 0 || out == in {
			// If we're just checking the value or not changing
			// it, there's no point in doing the rest.
			unlock(&mheap_.lock)
			return
		}
		gcControllerCommit() // 设置最大内存
		unlock(&mheap_.lock)
	})
	return out
}

// 函数读取环境变量 GOMEMLIMIT 的值，并将其转换为 int64 类型返回。
// 运行时的最大堆内存限制
// 如果 GOMEMLIMIT 的值为空字符串或 "off"，则返回 maxInt64（表示没有限制）；
// 如果 GOMEMLIMIT 是一个有效的字节数量，则返回该字节数量；
// 如果 GOMEMLIMIT 的值格式错误，则抛出异常。
func readGOMEMLIMIT() int64 {
	// 获取 GOMEMLIMIT 环境变量的值。
	p := gogetenv("GOMEMLIMIT")

	// 检查 GOMEMLIMIT 是否为空字符串或设置为 "off"。
	if p == "" || p == "off" {
		// 如果 GOMEMLIMIT 设置为空字符串或 "off"，则返回 maxInt64，表示没有内存限制。
		return maxInt64
	}

	// 尝试将 GOMEMLIMIT 的值转换为字节数量。
	n, ok := parseByteCount(p)
	if !ok {
		print("GOMEMLIMIT=", p, "\n")
		throw("malformed GOMEMLIMIT; see `go doc runtime/debug.SetMemoryLimit`")
	}

	// 如果转换成功，则返回转换后的字节数量。
	return n
}

// addIdleMarkWorker attempts to add a new idle mark worker.
//
// If this returns true, the caller must become an idle mark worker unless
// there's no background mark worker goroutines in the pool. This case is
// harmless because there are already background mark workers running.
// If this returns false, the caller must NOT become an idle mark worker.
//
// nosplit because it may be called without a P.
//
//go:nosplit
func (c *gcControllerState) addIdleMarkWorker() bool {
	for {
		old := c.idleMarkWorkers.Load()
		n, max := int32(old&uint64(^uint32(0))), int32(old>>32)
		if n >= max {
			// See the comment on idleMarkWorkers for why
			// n > max is tolerated.
			return false
		}
		if n < 0 {
			print("n=", n, " max=", max, "\n")
			throw("negative idle mark workers")
		}
		new := uint64(uint32(n+1)) | (uint64(max) << 32)
		if c.idleMarkWorkers.CompareAndSwap(old, new) {
			return true
		}
	}
}

// needIdleMarkWorker is a hint as to whether another idle mark worker is needed.
//
// The caller must still call addIdleMarkWorker to become one. This is mainly
// useful for a quick check before an expensive operation.
//
// nosplit because it may be called without a P.
//
//go:nosplit
func (c *gcControllerState) needIdleMarkWorker() bool {
	p := c.idleMarkWorkers.Load()
	n, max := int32(p&uint64(^uint32(0))), int32(p>>32)
	return n < max
}

// removeIdleMarkWorker must be called when an new idle mark worker stops executing.
func (c *gcControllerState) removeIdleMarkWorker() {
	for {
		old := c.idleMarkWorkers.Load()
		n, max := int32(old&uint64(^uint32(0))), int32(old>>32)
		if n-1 < 0 {
			print("n=", n, " max=", max, "\n")
			throw("negative idle mark workers")
		}
		new := uint64(uint32(n-1)) | (uint64(max) << 32)
		if c.idleMarkWorkers.CompareAndSwap(old, new) {
			return
		}
	}
}

// setMaxIdleMarkWorkers sets the maximum number of idle mark workers allowed.
//
// This method is optimistic in that it does not wait for the number of
// idle mark workers to reduce to max before returning; it assumes the workers
// will deschedule themselves.
func (c *gcControllerState) setMaxIdleMarkWorkers(max int32) {
	for {
		old := c.idleMarkWorkers.Load()
		n := int32(old & uint64(^uint32(0)))
		if n < 0 {
			print("n=", n, " max=", max, "\n")
			throw("negative idle mark workers")
		}
		new := uint64(uint32(n)) | (uint64(max) << 32)
		if c.idleMarkWorkers.CompareAndSwap(old, new) {
			return
		}
	}
}

// gcControllerCommit 是 gcController.commit 的包装函数，它传递来自实时（非测试）数据的参数。
// 它还更新了垃圾回收步调的任何消费者，如清扫步调和背景清扫器。
//
// 调用 gcController.commit。
//
// 必须持有堆锁，因此必须在系统栈上执行此函数。
//
//go:systemstack
func gcControllerCommit() {
	// 断言世界已停止或持有堆锁。
	assertWorldStoppedOrLockHeld(&mheap_.lock)

	gcController.commit(isSweepDone()) // 重新计算所有步调参数，这些参数用于导出垃圾回收的触发阈值和堆的目标大小。

	// 更新标记步调。
	if gcphase != _GCoff {
		gcController.revise()
	}

	// TODO(mknyszek): 这不再非常准确，因为堆目标是动态计算的。
	// 仍然有用作快照，但不如以前有用。
	if traceEnabled() {
		traceHeapGoal()
	}

	// 获取触发阈值和堆目标。
	trigger, heapGoal := gcController.trigger()

	// 根据触发阈值调整清扫步调。
	gcPaceSweeper(trigger)

	// 根据最新的步调参数调整背景清扫器。
	// 1. 当前的内存限制
	// 2. 堆目标
	// 3. 上一次的堆目标
	gcPaceScavenger(gcController.memoryLimit.Load(), heapGoal, gcController.lastHeapGoal)
}
