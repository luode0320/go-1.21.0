// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/abi"
	"internal/cpu"
	"internal/goarch"
	"runtime/internal/atomic"
	"runtime/internal/sys"
	"unsafe"
)

// set using cmd/go/internal/modload.ModInfoProg
var modinfo string

// Goroutine scheduler
// The scheduler's job is to distribute ready-to-run goroutines over worker threads.
//
// The main concepts are:
// G - goroutine.
// M - worker thread, or machine.
// P - processor, a resource that is required to execute Go code.
//     M must have an associated P to execute Go code, however it can be
//     blocked or in a syscall w/o an associated P.
//
// Design doc at https://golang.org/s/go11sched.

// Worker thread parking/unparking.
// We need to balance between keeping enough running worker threads to utilize
// available hardware parallelism and parking excessive running worker threads
// to conserve CPU resources and power. This is not simple for two reasons:
// (1) scheduler state is intentionally distributed (in particular, per-P work
// queues), so it is not possible to compute global predicates on fast paths;
// (2) for optimal thread management we would need to know the future (don't park
// a worker thread when a new goroutine will be readied in near future).
//
// Three rejected approaches that would work badly:
// 1. Centralize all scheduler state (would inhibit scalability).
// 2. Direct goroutine handoff. That is, when we ready a new goroutine and there
//    is a spare P, unpark a thread and handoff it the thread and the goroutine.
//    This would lead to thread state thrashing, as the thread that readied the
//    goroutine can be out of work the very next moment, we will need to park it.
//    Also, it would destroy locality of computation as we want to preserve
//    dependent goroutines on the same thread; and introduce additional latency.
// 3. Unpark an additional thread whenever we ready a goroutine and there is an
//    idle P, but don't do handoff. This would lead to excessive thread parking/
//    unparking as the additional threads will instantly park without discovering
//    any work to do.
//
// The current approach:
//
// This approach applies to three primary sources of potential work: readying a
// goroutine, new/modified-earlier timers, and idle-priority GC. See below for
// additional details.
//
// We unpark an additional thread when we submit work if (this is wakep()):
// 1. There is an idle P, and
// 2. There are no "spinning" worker threads.
//
// A worker thread is considered spinning if it is out of local work and did
// not find work in the global run queue or netpoller; the spinning state is
// denoted in m.spinning and in sched.nmspinning. Threads unparked this way are
// also considered spinning; we don't do goroutine handoff so such threads are
// out of work initially. Spinning threads spin on looking for work in per-P
// run queues and timer heaps or from the GC before parking. If a spinning
// thread finds work it takes itself out of the spinning state and proceeds to
// execution. If it does not find work it takes itself out of the spinning
// state and then parks.
//
// If there is at least one spinning thread (sched.nmspinning>1), we don't
// unpark new threads when submitting work. To compensate for that, if the last
// spinning thread finds work and stops spinning, it must unpark a new spinning
// thread. This approach smooths out unjustified spikes of thread unparking,
// but at the same time guarantees eventual maximal CPU parallelism
// utilization.
//
// The main implementation complication is that we need to be very careful
// during spinning->non-spinning thread transition. This transition can race
// with submission of new work, and either one part or another needs to unpark
// another worker thread. If they both fail to do that, we can end up with
// semi-persistent CPU underutilization.
//
// The general pattern for submission is:
// 1. Submit work to the local run queue, timer heap, or GC state.
// 2. #StoreLoad-style memory barrier.
// 3. Check sched.nmspinning.
//
// The general pattern for spinning->non-spinning transition is:
// 1. Decrement nmspinning.
// 2. #StoreLoad-style memory barrier.
// 3. Check all per-P work queues and GC for new work.
//
// Note that all this complexity does not apply to global run queue as we are
// not sloppy about thread unparking when submitting to global queue. Also see
// comments for nmspinning manipulation.
//
// How these different sources of work behave varies, though it doesn't affect
// the synchronization approach:
// * Ready goroutine: this is an obvious source of work; the goroutine is
//   immediately ready and must run on some thread eventually.
// * New/modified-earlier timer: The current timer implementation (see time.go)
//   uses netpoll in a thread with no work available to wait for the soonest
//   timer. If there is no thread waiting, we want a new spinning thread to go
//   wait.
// * Idle-priority GC: The GC wakes a stopped idle thread to contribute to
//   background GC work (note: currently disabled per golang.org/issue/19112).
//   Also see golang.org/issue/44313, as this should be extended to all GC
//   workers.

var (
	m0           m
	g0           g
	mcache0      *mcache
	raceprocctx0 uintptr
	raceFiniLock mutex
)

// This slice records the initializing tasks that need to be
// done to start up the runtime. It is built by the linker.
var runtime_inittasks []*initTask

// main_init_done is a signal used by cgocallbackg that initialization
// has been completed. It is made before _cgo_notify_runtime_init_done,
// so all cgo calls can rely on it existing. When main_init is complete,
// it is closed, meaning cgocallbackg can reliably receive from it.
var main_init_done chan bool

//go:linkname main_main main.main
func main_main()

// mainStarted 表示主M已启动。
var mainStarted bool

// runtimeInitTime is the nanotime() at which the runtime started.
var runtimeInitTime int64

// Value to use for signal mask for newly created M's.
var initSigmask sigset

// The main goroutine.
func main() { // 这是主goroutine
	mp := getg().m

	// Racectx of m0->g0 is used only as the parent of the main goroutine.
	// It must not be used for anything else.
	mp.g0.racectx = 0

	// Max stack size is 1 GB on 64-bit, 250 MB on 32-bit.
	// Using decimal instead of binary GB and MB because
	// they look nicer in the stack overflow failure message.
	if goarch.PtrSize == 8 {
		maxstacksize = 1000000000
	} else {
		maxstacksize = 250000000
	}

	// An upper limit for max stack size. Used to avoid random crashes
	// after calling SetMaxStack and trying to allocate a stack that is too big,
	// since stackalloc works with 32-bit sizes.
	maxstackceiling = 2 * maxstacksize

	// Allow newproc to start new Ms.
	mainStarted = true

	// wasm上还没有线程，所以没有sysmon
	if GOARCH != "wasm" {
		systemstack(func() {
			newm(sysmon, nil, -1) // 创建监控线程，该线程独立于调度器，不需要跟 p 关联即可运行
		})
	}

	// Lock the main goroutine onto this, the main OS thread,
	// during initialization. Most programs won't care, but a few
	// do require certain calls to be made by the main thread.
	// Those can arrange for main.main to run in the main thread
	// by calling runtime.LockOSThread during initialization
	// to preserve the lock.
	lockOSThread()

	if mp != &m0 {
		throw("runtime.main not on m0")
	}

	// Record when the world started.
	// Must be before doInit for tracing init.
	runtimeInitTime = nanotime()
	if runtimeInitTime == 0 {
		throw("nanotime returning zero")
	}

	if debug.inittrace != 0 {
		inittrace.id = getg().goid
		inittrace.active = true
	}

	doInit(runtime_inittasks) // Must be before defer.

	// Defer unlock so that runtime.Goexit during init does the unlock too.
	needUnlock := true
	defer func() {
		if needUnlock {
			unlockOSThread()
		}
	}()

	gcenable()

	main_init_done = make(chan bool)
	if iscgo {
		if _cgo_pthread_key_created == nil {
			throw("_cgo_pthread_key_created missing")
		}

		if _cgo_thread_start == nil {
			throw("_cgo_thread_start missing")
		}
		if GOOS != "windows" {
			if _cgo_setenv == nil {
				throw("_cgo_setenv missing")
			}
			if _cgo_unsetenv == nil {
				throw("_cgo_unsetenv missing")
			}
		}
		if _cgo_notify_runtime_init_done == nil {
			throw("_cgo_notify_runtime_init_done missing")
		}

		// Set the x_crosscall2_ptr C function pointer variable point to crosscall2.
		if set_crosscall2 == nil {
			throw("set_crosscall2 missing")
		}
		set_crosscall2()

		// Start the template thread in case we enter Go from
		// a C-created thread and need to create a new thread.
		startTemplateThread()
		cgocall(_cgo_notify_runtime_init_done, nil)
	}

	// Run the initializing tasks. Depending on build mode this
	// list can arrive a few different ways, but it will always
	// contain the init tasks computed by the linker for all the
	// packages in the program (excluding those added at runtime
	// by package plugin).
	for _, m := range activeModules() {
		doInit(m.inittasks)
	}

	// Disable init tracing after main init done to avoid overhead
	// of collecting statistics in malloc and newproc
	inittrace.active = false

	close(main_init_done)

	needUnlock = false
	unlockOSThread()

	if isarchive || islibrary {
		// A program compiled with -buildmode=c-archive or c-shared
		// has a main, but it is not executed.
		return
	}
	fn := main_main // make an indirect call, as the linker doesn't know the address of the main package when laying down the runtime
	fn()
	if raceenabled {
		runExitHooks(0) // run hooks now, since racefini does not return
		racefini()
	}

	// Make racy client program work: if panicking on
	// another goroutine at the same time as main returns,
	// let the other goroutine finish printing the panic trace.
	// Once it does, it will exit. See issues 3934 and 20018.
	if runningPanicDefers.Load() != 0 {
		// Running deferred functions should not take long.
		for c := 0; c < 1000; c++ {
			if runningPanicDefers.Load() == 0 {
				break
			}
			Gosched()
		}
	}
	if panicking.Load() != 0 {
		gopark(nil, nil, waitReasonPanicWait, traceBlockForever, 1)
	}
	runExitHooks(0)

	exit(0)
	for {
		var x *int32
		*x = 0
	}
}

// os_beforeExit is called from os.Exit(0).
//
//go:linkname os_beforeExit os.runtime_beforeExit
func os_beforeExit(exitCode int) {
	runExitHooks(exitCode)
	if exitCode == 0 && raceenabled {
		racefini()
	}
}

// start forcegc helper goroutine
func init() {
	go forcegchelper()
}

func forcegchelper() {
	forcegc.g = getg()
	lockInit(&forcegc.lock, lockRankForcegc)
	for {
		lock(&forcegc.lock)
		if forcegc.idle.Load() {
			throw("forcegc: phase error")
		}
		forcegc.idle.Store(true)
		goparkunlock(&forcegc.lock, waitReasonForceGCIdle, traceBlockSystemGoroutine, 1)
		// this goroutine is explicitly resumed by sysmon
		if debug.gctrace > 0 {
			println("GC forced")
		}
		// Time-triggered, fully concurrent.
		gcStart(gcTrigger{kind: gcTriggerTime, now: nanotime()})
	}
}

// Gosched yields the processor, allowing other goroutines to run. It does not
// suspend the current goroutine, so execution resumes automatically.
//
//go:nosplit
func Gosched() {
	checkTimeouts()
	mcall(gosched_m)
}

// goschedguarded yields the processor like gosched, but also checks
// for forbidden states and opts out of the yield in those cases.
//
//go:nosplit
func goschedguarded() {
	mcall(goschedguarded_m)
}

// goschedIfBusy yields the processor like gosched, but only does so if
// there are no idle Ps or if we're on the only P and there's nothing in
// the run queue. In both cases, there is freely available idle time.
//
//go:nosplit
func goschedIfBusy() {
	gp := getg()
	// Call gosched if gp.preempt is set; we may be in a tight loop that
	// doesn't otherwise yield.
	if !gp.preempt && sched.npidle.Load() > 0 {
		return
	}
	mcall(gosched_m)
}

// Puts the current goroutine into a waiting state and calls unlockf on the
// system stack.
//
// If unlockf returns false, the goroutine is resumed.
//
// unlockf must not access this G's stack, as it may be moved between
// the call to gopark and the call to unlockf.
//
// Note that because unlockf is called after putting the G into a waiting
// state, the G may have already been readied by the time unlockf is called
// unless there is external synchronization preventing the G from being
// readied. If unlockf returns false, it must guarantee that the G cannot be
// externally readied.
//
// Reason explains why the goroutine has been parked. It is displayed in stack
// traces and heap dumps. Reasons should be unique and descriptive. Do not
// re-use reasons, add new ones.
func gopark(unlockf func(*g, unsafe.Pointer) bool, lock unsafe.Pointer, reason waitReason, traceReason traceBlockReason, traceskip int) {
	if reason != waitReasonSleep {
		checkTimeouts() // timeouts may expire while two goroutines keep the scheduler busy
	}
	mp := acquirem()
	gp := mp.curg
	status := readgstatus(gp)
	if status != _Grunning && status != _Gscanrunning {
		throw("gopark: bad g status")
	}
	mp.waitlock = lock
	mp.waitunlockf = unlockf
	gp.waitreason = reason
	mp.waitTraceBlockReason = traceReason
	mp.waitTraceSkip = traceskip
	releasem(mp)
	// can't do anything that might move the G between Ms here.
	mcall(park_m)
}

// Puts the current goroutine into a waiting state and unlocks the lock.
// The goroutine can be made runnable again by calling goready(gp).
func goparkunlock(lock *mutex, reason waitReason, traceReason traceBlockReason, traceskip int) {
	gopark(parkunlock_c, unsafe.Pointer(lock), reason, traceReason, traceskip)
}

func goready(gp *g, traceskip int) {
	systemstack(func() {
		ready(gp, traceskip, true)
	})
}

//go:nosplit
func acquireSudog() *sudog {
	// Delicate dance: the semaphore implementation calls
	// acquireSudog, acquireSudog calls new(sudog),
	// new calls malloc, malloc can call the garbage collector,
	// and the garbage collector calls the semaphore implementation
	// in stopTheWorld.
	// Break the cycle by doing acquirem/releasem around new(sudog).
	// The acquirem/releasem increments m.locks during new(sudog),
	// which keeps the garbage collector from being invoked.
	mp := acquirem()
	pp := mp.p.ptr()
	if len(pp.sudogcache) == 0 {
		lock(&sched.sudoglock)
		// First, try to grab a batch from central cache.
		for len(pp.sudogcache) < cap(pp.sudogcache)/2 && sched.sudogcache != nil {
			s := sched.sudogcache
			sched.sudogcache = s.next
			s.next = nil
			pp.sudogcache = append(pp.sudogcache, s)
		}
		unlock(&sched.sudoglock)
		// If the central cache is empty, allocate a new one.
		if len(pp.sudogcache) == 0 {
			pp.sudogcache = append(pp.sudogcache, new(sudog))
		}
	}
	n := len(pp.sudogcache)
	s := pp.sudogcache[n-1]
	pp.sudogcache[n-1] = nil
	pp.sudogcache = pp.sudogcache[:n-1]
	if s.elem != nil {
		throw("acquireSudog: found s.elem != nil in cache")
	}
	releasem(mp)
	return s
}

//go:nosplit
func releaseSudog(s *sudog) {
	if s.elem != nil {
		throw("runtime: sudog with non-nil elem")
	}
	if s.isSelect {
		throw("runtime: sudog with non-false isSelect")
	}
	if s.next != nil {
		throw("runtime: sudog with non-nil next")
	}
	if s.prev != nil {
		throw("runtime: sudog with non-nil prev")
	}
	if s.waitlink != nil {
		throw("runtime: sudog with non-nil waitlink")
	}
	if s.c != nil {
		throw("runtime: sudog with non-nil c")
	}
	gp := getg()
	if gp.param != nil {
		throw("runtime: releaseSudog with non-nil gp.param")
	}
	mp := acquirem() // avoid rescheduling to another P
	pp := mp.p.ptr()
	if len(pp.sudogcache) == cap(pp.sudogcache) {
		// Transfer half of local cache to the central cache.
		var first, last *sudog
		for len(pp.sudogcache) > cap(pp.sudogcache)/2 {
			n := len(pp.sudogcache)
			p := pp.sudogcache[n-1]
			pp.sudogcache[n-1] = nil
			pp.sudogcache = pp.sudogcache[:n-1]
			if first == nil {
				first = p
			} else {
				last.next = p
			}
			last = p
		}
		lock(&sched.sudoglock)
		last.next = sched.sudogcache
		sched.sudogcache = first
		unlock(&sched.sudoglock)
	}
	pp.sudogcache = append(pp.sudogcache, s)
	releasem(mp)
}

// called from assembly.
func badmcall(fn func(*g)) {
	throw("runtime: mcall called on m->g0 stack")
}

func badmcall2(fn func(*g)) {
	throw("runtime: mcall function returned")
}

func badreflectcall() {
	panic(plainError("arg size to reflect.call more than 1GB"))
}

//go:nosplit
//go:nowritebarrierrec
func badmorestackg0() {
	writeErrStr("fatal: morestack on g0\n")
}

//go:nosplit
//go:nowritebarrierrec
func badmorestackgsignal() {
	writeErrStr("fatal: morestack on gsignal\n")
}

//go:nosplit
func badctxt() {
	throw("ctxt != 0")
}

func lockedOSThread() bool {
	gp := getg()
	return gp.lockedm != 0 && gp.m.lockedg != 0
}

var (
	// allgs 包含了所有曾经创建过的 G（包括已经死亡的 G），因此不会缩小。
	//
	// 通过切片访问受到 allglock 或 stop-the-world 的保护。
	// 不能获取锁的读取者可以（谨慎地！）使用下面的原子变量。
	allglock mutex
	allgs    []*g

	// allglen 和 allgptr 是原子变量，分别包含 len(allgs) 和 &allgs[0]。
	// 适当的顺序取决于全序加载和存储。写操作受 allglock 保护。
	//
	// 在更新 allgptr 之前更新 allglen。读取者应该在读取 allgptr 之前先读取 allglen，
	// 以确保 allglen 始终 <= len(allgptr)。在比赛期间追加新 G 可能会被错过。
	// 为了获得所有 G 的一致视图，必须持有 allglock。
	//
	// allgptr 的复制应始终存储为具体类型或 unsafe.Pointer，而不是 uintptr，以确保 GC
	// 仍然可以到达它，即使它指向了一个过时的数组。
	allglen uintptr
	allgptr **g
)

func allgadd(gp *g) {
	if readgstatus(gp) == _Gidle {
		throw("allgadd: bad status Gidle")
	}

	lock(&allglock)
	allgs = append(allgs, gp)
	if &allgs[0] != allgptr {
		atomicstorep(unsafe.Pointer(&allgptr), unsafe.Pointer(&allgs[0]))
	}
	atomic.Storeuintptr(&allglen, uintptr(len(allgs)))
	unlock(&allglock)
}

// allGsSnapshot returns a snapshot of the slice of all Gs.
//
// The world must be stopped or allglock must be held.
func allGsSnapshot() []*g {
	assertWorldStoppedOrLockHeld(&allglock)

	// Because the world is stopped or allglock is held, allgadd
	// cannot happen concurrently with this. allgs grows
	// monotonically and existing entries never change, so we can
	// simply return a copy of the slice header. For added safety,
	// we trim everything past len because that can still change.
	return allgs[:len(allgs):len(allgs)]
}

// atomicAllG returns &allgs[0] and len(allgs) for use with atomicAllGIndex.
func atomicAllG() (**g, uintptr) {
	length := atomic.Loaduintptr(&allglen)
	ptr := (**g)(atomic.Loadp(unsafe.Pointer(&allgptr)))
	return ptr, length
}

// atomicAllGIndex returns ptr[i] with the allgptr returned from atomicAllG.
func atomicAllGIndex(ptr **g, i uintptr) *g {
	return *(**g)(add(unsafe.Pointer(ptr), i*goarch.PtrSize))
}

// forEachG calls fn on every G from allgs.
//
// forEachG takes a lock to exclude concurrent addition of new Gs.
func forEachG(fn func(gp *g)) {
	lock(&allglock)
	for _, gp := range allgs {
		fn(gp)
	}
	unlock(&allglock)
}

// forEachGRace calls fn on every G from allgs.
//
// forEachGRace avoids locking, but does not exclude addition of new Gs during
// execution, which may be missed.
func forEachGRace(fn func(gp *g)) {
	ptr, length := atomicAllG()
	for i := uintptr(0); i < length; i++ {
		gp := atomicAllGIndex(ptr, i)
		fn(gp)
	}
	return
}

const (
	// Number of goroutine ids to grab from sched.goidgen to local per-P cache at once.
	// 16 seems to provide enough amortization, but other than that it's mostly arbitrary number.
	_GoidCacheBatch = 16
)

// cpuinit sets up CPU feature flags and calls internal/cpu.Initialize. env should be the complete
// value of the GODEBUG environment variable.
func cpuinit(env string) {
	switch GOOS {
	case "aix", "darwin", "ios", "dragonfly", "freebsd", "netbsd", "openbsd", "illumos", "solaris", "linux":
		cpu.DebugOptions = true
	}
	cpu.Initialize(env)

	// Support cpu feature variables are used in code generated by the compiler
	// to guard execution of instructions that can not be assumed to be always supported.
	switch GOARCH {
	case "386", "amd64":
		x86HasPOPCNT = cpu.X86.HasPOPCNT
		x86HasSSE41 = cpu.X86.HasSSE41
		x86HasFMA = cpu.X86.HasFMA

	case "arm":
		armHasVFPv4 = cpu.ARM.HasVFPv4

	case "arm64":
		arm64HasATOMICS = cpu.ARM64.HasATOMICS
	}
}

// getGodebugEarly extracts the environment variable GODEBUG from the environment on
// Unix-like operating systems and returns it. This function exists to extract GODEBUG
// early before much of the runtime is initialized.
func getGodebugEarly() string {
	const prefix = "GODEBUG="
	var env string
	switch GOOS {
	case "aix", "darwin", "ios", "dragonfly", "freebsd", "netbsd", "openbsd", "illumos", "solaris", "linux":
		// Similar to goenv_unix but extracts the environment value for
		// GODEBUG directly.
		// TODO(moehrmann): remove when general goenvs() can be called before cpuinit()
		n := int32(0)
		for argv_index(argv, argc+1+n) != nil {
			n++
		}

		for i := int32(0); i < n; i++ {
			p := argv_index(argv, argc+1+i)
			s := unsafe.String(p, findnull(p))

			if hasPrefix(s, prefix) {
				env = gostring(p)[len(prefix):]
				break
			}
		}
	}
	return env
}

// 启动序列如下：
//
// 调用 runtime.osinit
// 调用 runtime.schedinit
// 创建并排队新的 G
// 调用 runtime.mstart
// 新的 G 调用 runtime.main
func schedinit() {
	// 初始化多个锁，用于运行时内部的同步和互斥。
	// 每个锁都分配了特定的等级，以避免死锁。
	// 这些锁包括但不限于调度器锁、系统监控锁、defer 锁等。
	// 初始化锁的等级是运行时调度和并发控制的关键部分。
	lockInit(&sched.lock, lockRankSched)             // 初始化 sched.lock，调度器锁，锁等级为 lockRankSched。
	lockInit(&sched.sysmonlock, lockRankSysmon)      // 初始化 sched.sysmonlock，系统监控锁，锁等级为 lockRankSysmon。
	lockInit(&sched.deferlock, lockRankDefer)        // 初始化 sched.deferlock，defer 锁，锁等级为 lockRankDefer。
	lockInit(&sched.sudoglock, lockRankSudog)        // 初始化 sched.sudoglock，sudog 锁，锁等级为 lockRankSudog。
	lockInit(&deadlock, lockRankDeadlock)            // 初始化 deadlock，死锁锁，锁等级为 lockRankDeadlock。
	lockInit(&paniclk, lockRankPanic)                // 初始化 paniclk，panic 锁，锁等级为 lockRankPanic。
	lockInit(&allglock, lockRankAllg)                // 初始化 allglock，所有 G 的锁，锁等级为 lockRankAllg。
	lockInit(&allpLock, lockRankAllp)                // 初始化 allpLock，所有 P 的锁，锁等级为 lockRankAllp。
	lockInit(&reflectOffs.lock, lockRankReflectOffs) // 初始化 reflectOffs.lock，反射包 Offs 锁，锁等级为 lockRankReflectOffs。
	lockInit(&finlock, lockRankFin)                  // 初始化 finlock，终结锁，锁等级为 lockRankFin。
	lockInit(&cpuprof.lock, lockRankCpuprof)         // 初始化 cpuprof.lock，CPU 分析锁，锁等级为 lockRankCpuprof。
	// 初始化 allocmLock，M 分配锁，锁等级为 lockRankAllocmR、lockRankAllocmRInternal、lockRankAllocmW。
	allocmLock.init(lockRankAllocmR, lockRankAllocmRInternal, lockRankAllocmW)
	// 初始化 execLock，执行锁，锁等级为 lockRankExecR、lockRankExecRInternal、lockRankExecW。
	execLock.init(lockRankExecR, lockRankExecRInternal, lockRankExecW)
	// 初始化 traceLock。
	traceLockInit()
	// 初始化 memstats.heapStats.noPLock，堆统计锁，锁等级为 lockRankLeafRank，确保它始终是叶子锁。
	// 所有此锁的关键部分都应该非常短。
	lockInit(&memstats.heapStats.noPLock, lockRankLeafRank)

	// 返回当前 G 的指针, 取出g0
	gp := getg()

	// 如果启用了 race 检测器，raceinit 必须是第一个调用的函数。
	// raceinit 在调用 mallocinit 之前完成，避免在初始化期间发生竞争条件。
	if raceenabled {
		gp.racectx, raceprocctx0 = raceinit()
	}

	// 设置最大 m（操作系统线程）数量。
	sched.maxmcount = 10000

	// 世界开始时是停止的。
	worldStopped()

	// 验证模块数据，初始化栈，内存分配器，调试设置等。
	// 这些步骤确保了运行时的基本结构和功能就绪。
	moduledataverify()           // 验证模块数据
	stackinit()                  // 初始化栈
	mallocinit()                 // 初始化内存分配器
	godebug := getGodebugEarly() // 获取早期的 Godebug 设置
	initPageTrace(godebug)       // 初始化页跟踪，必须在 mallocinit 之后但在任何分配发生之前运行。
	cpuinit(godebug)             // 初始化 CPU，必须在 alginit 之前运行。
	alginit()                    // 初始化算法，如哈希映射、随机数生成等，必须在使用之前调用。
	fastrandinit()               // 初始化快速随机数生成器，必须在 mcommoninit 之前运行。
	mcommoninit(gp.m, -1)        // 初始化当前 g0 的 m0 结构体
	modulesinit()                // 初始化模块，提供 activeModules。
	typelinksinit()              // 初始化类型链接，使用 maps 和 activeModules。
	itabsinit()                  // 初始化接口表，使用 activeModules。
	stkobjinit()                 // 初始化栈对象，必须在 GC 开始之前运行。

	sigsave(&gp.m.sigmask)     // 保存信号掩码，用于后续恢复。
	initSigmask = gp.m.sigmask // 记录初始信号掩码。

	goargs()         // 解析命令行参数。
	goenvs()         // 解析环境变量。
	secure()         // 设置安全模式，可能会影响某些运行时行为。
	parsedebugvars() // 解析调试变量，可能会影响运行时的调试和性能配置。
	gcinit()         // 初始化 GC（垃圾回收）。

	// 如果禁用了内存剖析，更新 MemProfileRate 为 0 以关闭内存剖析。
	if disableMemoryProfiling {
		MemProfileRate = 0
	}

	// 加锁，更新调度器状态。
	lock(&sched.lock)
	// 设置上次网络轮询的时间戳，这将用于后续的网络 I/O 调度决策。
	sched.lastpoll.Store(nanotime())

	// ncpu 在 runtime.osinit 时已经获取
	procs := ncpu
	// 如果GOMAXPROCS设置并且合法就将procs的设置为GOMAXPROCS
	if n, ok := atoi32(gogetenv("GOMAXPROCS")); ok && n > 0 {
		procs = n
	}

	// 调整处理器数量，返回包含本地工作列表的 P 的列表，需要由调用者调度, 确保有足够的 P 来处理 goroutine。
	// 如果调整失败，会抛出异常。
	if procresize(procs) != nil {
		throw("unknown runnable goroutine during bootstrap")
	}

	// 解锁，允许其他调度器操作。
	unlock(&sched.lock)

	// 世界现在实际上已经启动，P 可以运行了。
	worldStarted()

	// 确保构建版本和模块信息被记录在二进制文件中。
	if buildVersion == "" {
		// 不应触发此条件，但确保 runtime·buildVersion 被保留。
		buildVersion = "unknown"
	}
	if len(modinfo) == 1 {
		// 同样，不应触发此条件，但确保 runtime·modinfo 被保留。
		modinfo = ""
	}
}

func dumpgstatus(gp *g) {
	thisg := getg()
	print("runtime:   gp: gp=", gp, ", goid=", gp.goid, ", gp->atomicstatus=", readgstatus(gp), "\n")
	print("runtime: getg:  g=", thisg, ", goid=", thisg.goid, ",  g->atomicstatus=", readgstatus(thisg), "\n")
}

// sched.lock must be held.
func checkmcount() {
	assertLockHeld(&sched.lock)

	// Exclude extra M's, which are used for cgocallback from threads
	// created in C.
	//
	// The purpose of the SetMaxThreads limit is to avoid accidental fork
	// bomb from something like millions of goroutines blocking on system
	// calls, causing the runtime to create millions of threads. By
	// definition, this isn't a problem for threads created in C, so we
	// exclude them from the limit. See https://go.dev/issue/60004.
	count := mcount() - int32(extraMInUse.Load()) - int32(extraMLength.Load())
	if count > sched.maxmcount {
		print("runtime: program exceeds ", sched.maxmcount, "-thread limit\n")
		throw("thread exhaustion")
	}
}

// mReserveID returns the next ID to use for a new m. This new m is immediately
// considered 'running' by checkdead.
//
// sched.lock must be held.
func mReserveID() int64 {
	assertLockHeld(&sched.lock)

	if sched.mnext+1 < sched.mnext {
		throw("runtime: thread ID overflow")
	}
	id := sched.mnext
	sched.mnext++
	checkmcount()
	return id
}

// 如果传递了预分配的 ID，则作为 'id' 参数，或者通过传入 -1 来省略它。
func mcommoninit(mp *m, id int64) {
	// 返回当前 G 的指针
	gp := getg()

	// g0 的栈对用户没有意义（且不一定可逆向），所以仅当 gp 不等于其自身的 g0 时才进行调用者追踪。
	if gp != gp.m.g0 {
		callers(1, mp.createstack[:])
	}

	// 获取调度器锁，以确保线程安全。
	lock(&sched.lock)

	// 如果提供了 ID，则直接使用；否则，从全局资源池中预留一个 ID。
	if id >= 0 {
		mp.id = id
	} else {
		// 返回用于新m的下一个ID
		mp.id = mReserveID()
	}

	// 使用散列函数生成两个 32 位的随机数，一个基于 m 的 ID，另一个基于当前时间。
	lo := uint32(int64Hash(uint64(mp.id), fastrandseed))
	hi := uint32(int64Hash(uint64(cputicks()), ^fastrandseed))
	// 如果生成的随机数全为 0，则强制 hi 为 1，以避免全 0 的情况。
	if lo|hi == 0 {
		hi = 1
	}

	// 根据系统字节序（大端或小端）来组合生成的低 32 位和高 32 位随机数，形成 fastrand。
	// 这是为了保证跨平台的一致性。
	if goarch.BigEndian {
		mp.fastrand = uint64(lo)<<32 | uint64(hi)
	} else {
		mp.fastrand = uint64(hi)<<32 | uint64(lo)
	}

	// 调用 mpreinit 来进一步初始化 m 结构体。
	mpreinit(mp)

	// 如果 m 中存在信号处理 goroutine，则更新其 stackguard1 值。
	if mp.gsignal != nil {
		mp.gsignal.stackguard1 = mp.gsignal.stack.lo + stackGuard
	}

	// 将 m 添加到 allm 所有的m构成的一个链表列表中，这样垃圾收集器就不会误删掉与 m 关联的 goroutine。
	// 当 m 仅在寄存器或线程本地存储中时，这是必要的。
	mp.alllink = allm

	// NumCgoCall() 函数会在不持有调度器锁的情况下遍历 allm，
	// 所以我们安全地发布 mp，以确保其他 goroutine 可以看到这个变化。
	atomicstorep(unsafe.Pointer(&allm), unsafe.Pointer(mp))
	unlock(&sched.lock)

	// 如果在 C 代码中调用 Go，或者在 Solaris、Illumos 或 Windows 上运行，
	// 则分配内存以保存 C 调用堆栈的回溯信息，以防 C 代码中的崩溃。
	if iscgo || GOOS == "solaris" || GOOS == "illumos" || GOOS == "windows" {
		mp.cgoCallers = new(cgoCallers)
	}
}

func (mp *m) becomeSpinning() {
	mp.spinning = true
	sched.nmspinning.Add(1)
	sched.needspinning.Store(0)
}

func (mp *m) hasCgoOnStack() bool {
	return mp.ncgo > 0 || mp.isextra
}

var fastrandseed uintptr

func fastrandinit() {
	s := (*[unsafe.Sizeof(fastrandseed)]byte)(unsafe.Pointer(&fastrandseed))[:]
	getRandomData(s)
}

// Mark gp ready to run.
func ready(gp *g, traceskip int, next bool) {
	if traceEnabled() {
		traceGoUnpark(gp, traceskip)
	}

	status := readgstatus(gp)

	// Mark runnable.
	mp := acquirem() // disable preemption because it can be holding p in a local var
	if status&^_Gscan != _Gwaiting {
		dumpgstatus(gp)
		throw("bad g->status in ready")
	}

	// status is Gwaiting or Gscanwaiting, make Grunnable and put on runq
	casgstatus(gp, _Gwaiting, _Grunnable)
	runqput(mp.p.ptr(), gp, next)
	wakep()
	releasem(mp)
}

// freezeStopWait is a large value that freezetheworld sets
// sched.stopwait to in order to request that all Gs permanently stop.
const freezeStopWait = 0x7fffffff

// freezing is set to non-zero if the runtime is trying to freeze the
// world.
var freezing atomic.Bool

// Similar to stopTheWorld but best-effort and can be called several times.
// There is no reverse operation, used during crashing.
// This function must not lock any mutexes.
func freezetheworld() {
	freezing.Store(true)
	if debug.dontfreezetheworld > 0 {
		// Don't prempt Ps to stop goroutines. That will perturb
		// scheduler state, making debugging more difficult. Instead,
		// allow goroutines to continue execution.
		//
		// fatalpanic will tracebackothers to trace all goroutines. It
		// is unsafe to trace a running goroutine, so tracebackothers
		// will skip running goroutines. That is OK and expected, we
		// expect users of dontfreezetheworld to use core files anyway.
		//
		// However, allowing the scheduler to continue running free
		// introduces a race: a goroutine may be stopped when
		// tracebackothers checks its status, and then start running
		// later when we are in the middle of traceback, potentially
		// causing a crash.
		//
		// To mitigate this, when an M naturally enters the scheduler,
		// schedule checks if freezing is set and if so stops
		// execution. This guarantees that while Gs can transition from
		// running to stopped, they can never transition from stopped
		// to running.
		//
		// The sleep here allows racing Ms that missed freezing and are
		// about to run a G to complete the transition to running
		// before we start traceback.
		usleep(1000)
		return
	}

	// stopwait and preemption requests can be lost
	// due to races with concurrently executing threads,
	// so try several times
	for i := 0; i < 5; i++ {
		// this should tell the scheduler to not start any new goroutines
		sched.stopwait = freezeStopWait
		sched.gcwaiting.Store(true)
		// this should stop running goroutines
		if !preemptall() {
			break // no running goroutines
		}
		usleep(1000)
	}
	// to be sure
	usleep(1000)
	preemptall()
	usleep(1000)
}

// All reads and writes of g's status go through readgstatus, casgstatus
// castogscanstatus, casfrom_Gscanstatus.
//
//go:nosplit
func readgstatus(gp *g) uint32 {
	return gp.atomicstatus.Load()
}

// The Gscanstatuses are acting like locks and this releases them.
// If it proves to be a performance hit we should be able to make these
// simple atomic stores but for now we are going to throw if
// we see an inconsistent state.
func casfrom_Gscanstatus(gp *g, oldval, newval uint32) {
	success := false

	// Check that transition is valid.
	switch oldval {
	default:
		print("runtime: casfrom_Gscanstatus bad oldval gp=", gp, ", oldval=", hex(oldval), ", newval=", hex(newval), "\n")
		dumpgstatus(gp)
		throw("casfrom_Gscanstatus:top gp->status is not in scan state")
	case _Gscanrunnable,
		_Gscanwaiting,
		_Gscanrunning,
		_Gscansyscall,
		_Gscanpreempted:
		if newval == oldval&^_Gscan {
			success = gp.atomicstatus.CompareAndSwap(oldval, newval)
		}
	}
	if !success {
		print("runtime: casfrom_Gscanstatus failed gp=", gp, ", oldval=", hex(oldval), ", newval=", hex(newval), "\n")
		dumpgstatus(gp)
		throw("casfrom_Gscanstatus: gp->status is not in scan state")
	}
	releaseLockRank(lockRankGscan)
}

// This will return false if the gp is not in the expected status and the cas fails.
// This acts like a lock acquire while the casfromgstatus acts like a lock release.
func castogscanstatus(gp *g, oldval, newval uint32) bool {
	switch oldval {
	case _Grunnable,
		_Grunning,
		_Gwaiting,
		_Gsyscall:
		if newval == oldval|_Gscan {
			r := gp.atomicstatus.CompareAndSwap(oldval, newval)
			if r {
				acquireLockRank(lockRankGscan)
			}
			return r

		}
	}
	print("runtime: castogscanstatus oldval=", hex(oldval), " newval=", hex(newval), "\n")
	throw("castogscanstatus")
	panic("not reached")
}

// casgstatusAlwaysTrack is a debug flag that causes casgstatus to always track
// various latencies on every transition instead of sampling them.
var casgstatusAlwaysTrack = false

// If asked to move to or from a Gscanstatus this will throw. Use the castogscanstatus
// and casfrom_Gscanstatus instead.
// casgstatus will loop if the g->atomicstatus is in a Gscan status until the routine that
// put it in the Gscan state is finished.
//
//go:nosplit
func casgstatus(gp *g, oldval, newval uint32) {
	if (oldval&_Gscan != 0) || (newval&_Gscan != 0) || oldval == newval {
		systemstack(func() {
			print("runtime: casgstatus: oldval=", hex(oldval), " newval=", hex(newval), "\n")
			throw("casgstatus: bad incoming values")
		})
	}

	acquireLockRank(lockRankGscan)
	releaseLockRank(lockRankGscan)

	// See https://golang.org/cl/21503 for justification of the yield delay.
	const yieldDelay = 5 * 1000
	var nextYield int64

	// loop if gp->atomicstatus is in a scan state giving
	// GC time to finish and change the state to oldval.
	for i := 0; !gp.atomicstatus.CompareAndSwap(oldval, newval); i++ {
		if oldval == _Gwaiting && gp.atomicstatus.Load() == _Grunnable {
			throw("casgstatus: waiting for Gwaiting but is Grunnable")
		}
		if i == 0 {
			nextYield = nanotime() + yieldDelay
		}
		if nanotime() < nextYield {
			for x := 0; x < 10 && gp.atomicstatus.Load() != oldval; x++ {
				procyield(1)
			}
		} else {
			osyield()
			nextYield = nanotime() + yieldDelay/2
		}
	}

	if oldval == _Grunning {
		// Track every gTrackingPeriod time a goroutine transitions out of running.
		if casgstatusAlwaysTrack || gp.trackingSeq%gTrackingPeriod == 0 {
			gp.tracking = true
		}
		gp.trackingSeq++
	}
	if !gp.tracking {
		return
	}

	// Handle various kinds of tracking.
	//
	// Currently:
	// - Time spent in runnable.
	// - Time spent blocked on a sync.Mutex or sync.RWMutex.
	switch oldval {
	case _Grunnable:
		// We transitioned out of runnable, so measure how much
		// time we spent in this state and add it to
		// runnableTime.
		now := nanotime()
		gp.runnableTime += now - gp.trackingStamp
		gp.trackingStamp = 0
	case _Gwaiting:
		if !gp.waitreason.isMutexWait() {
			// Not blocking on a lock.
			break
		}
		// Blocking on a lock, measure it. Note that because we're
		// sampling, we have to multiply by our sampling period to get
		// a more representative estimate of the absolute value.
		// gTrackingPeriod also represents an accurate sampling period
		// because we can only enter this state from _Grunning.
		now := nanotime()
		sched.totalMutexWaitTime.Add((now - gp.trackingStamp) * gTrackingPeriod)
		gp.trackingStamp = 0
	}
	switch newval {
	case _Gwaiting:
		if !gp.waitreason.isMutexWait() {
			// Not blocking on a lock.
			break
		}
		// Blocking on a lock. Write down the timestamp.
		now := nanotime()
		gp.trackingStamp = now
	case _Grunnable:
		// We just transitioned into runnable, so record what
		// time that happened.
		now := nanotime()
		gp.trackingStamp = now
	case _Grunning:
		// We're transitioning into running, so turn off
		// tracking and record how much time we spent in
		// runnable.
		gp.tracking = false
		sched.timeToRun.record(gp.runnableTime)
		gp.runnableTime = 0
	}
}

// casGToWaiting transitions gp from old to _Gwaiting, and sets the wait reason.
//
// Use this over casgstatus when possible to ensure that a waitreason is set.
func casGToWaiting(gp *g, old uint32, reason waitReason) {
	// Set the wait reason before calling casgstatus, because casgstatus will use it.
	gp.waitreason = reason
	casgstatus(gp, old, _Gwaiting)
}

// casgstatus(gp, oldstatus, Gcopystack), assuming oldstatus is Gwaiting or Grunnable.
// Returns old status. Cannot call casgstatus directly, because we are racing with an
// async wakeup that might come in from netpoll. If we see Gwaiting from the readgstatus,
// it might have become Grunnable by the time we get to the cas. If we called casgstatus,
// it would loop waiting for the status to go back to Gwaiting, which it never will.
//
//go:nosplit
func casgcopystack(gp *g) uint32 {
	for {
		oldstatus := readgstatus(gp) &^ _Gscan
		if oldstatus != _Gwaiting && oldstatus != _Grunnable {
			throw("copystack: bad status, not Gwaiting or Grunnable")
		}
		if gp.atomicstatus.CompareAndSwap(oldstatus, _Gcopystack) {
			return oldstatus
		}
	}
}

// casGToPreemptScan transitions gp from _Grunning to _Gscan|_Gpreempted.
//
// TODO(austin): This is the only status operation that both changes
// the status and locks the _Gscan bit. Rethink this.
func casGToPreemptScan(gp *g, old, new uint32) {
	if old != _Grunning || new != _Gscan|_Gpreempted {
		throw("bad g transition")
	}
	acquireLockRank(lockRankGscan)
	for !gp.atomicstatus.CompareAndSwap(_Grunning, _Gscan|_Gpreempted) {
	}
}

// casGFromPreempted attempts to transition gp from _Gpreempted to
// _Gwaiting. If successful, the caller is responsible for
// re-scheduling gp.
func casGFromPreempted(gp *g, old, new uint32) bool {
	if old != _Gpreempted || new != _Gwaiting {
		throw("bad g transition")
	}
	gp.waitreason = waitReasonPreempted
	return gp.atomicstatus.CompareAndSwap(_Gpreempted, _Gwaiting)
}

// stwReason is an enumeration of reasons the world is stopping.
type stwReason uint8

// Reasons to stop-the-world.
//
// Avoid reusing reasons and add new ones instead.
const (
	stwUnknown                     stwReason = iota // "unknown"
	stwGCMarkTerm                                   // "GC mark termination"
	stwGCSweepTerm                                  // "GC sweep termination"
	stwWriteHeapDump                                // "write heap dump"
	stwGoroutineProfile                             // "goroutine profile"
	stwGoroutineProfileCleanup                      // "goroutine profile cleanup"
	stwAllGoroutinesStack                           // "all goroutines stack trace"
	stwReadMemStats                                 // "read mem stats"
	stwAllThreadsSyscall                            // "AllThreadsSyscall"
	stwGOMAXPROCS                                   // "GOMAXPROCS"
	stwStartTrace                                   // "start trace"
	stwStopTrace                                    // "stop trace"
	stwForTestCountPagesInUse                       // "CountPagesInUse (test)"
	stwForTestReadMetricsSlow                       // "ReadMetricsSlow (test)"
	stwForTestReadMemStatsSlow                      // "ReadMemStatsSlow (test)"
	stwForTestPageCachePagesLeaked                  // "PageCachePagesLeaked (test)"
	stwForTestResetDebugLog                         // "ResetDebugLog (test)"
)

func (r stwReason) String() string {
	return stwReasonStrings[r]
}

// If you add to this list, also add it to src/internal/trace/parser.go.
// If you change the values of any of the stw* constants, bump the trace
// version number and make a copy of this.
var stwReasonStrings = [...]string{
	stwUnknown:                     "unknown",
	stwGCMarkTerm:                  "GC mark termination",
	stwGCSweepTerm:                 "GC sweep termination",
	stwWriteHeapDump:               "write heap dump",
	stwGoroutineProfile:            "goroutine profile",
	stwGoroutineProfileCleanup:     "goroutine profile cleanup",
	stwAllGoroutinesStack:          "all goroutines stack trace",
	stwReadMemStats:                "read mem stats",
	stwAllThreadsSyscall:           "AllThreadsSyscall",
	stwGOMAXPROCS:                  "GOMAXPROCS",
	stwStartTrace:                  "start trace",
	stwStopTrace:                   "stop trace",
	stwForTestCountPagesInUse:      "CountPagesInUse (test)",
	stwForTestReadMetricsSlow:      "ReadMetricsSlow (test)",
	stwForTestReadMemStatsSlow:     "ReadMemStatsSlow (test)",
	stwForTestPageCachePagesLeaked: "PageCachePagesLeaked (test)",
	stwForTestResetDebugLog:        "ResetDebugLog (test)",
}

// stopTheWorld stops all P's from executing goroutines, interrupting
// all goroutines at GC safe points and records reason as the reason
// for the stop. On return, only the current goroutine's P is running.
// stopTheWorld must not be called from a system stack and the caller
// must not hold worldsema. The caller must call startTheWorld when
// other P's should resume execution.
//
// stopTheWorld is safe for multiple goroutines to call at the
// same time. Each will execute its own stop, and the stops will
// be serialized.
//
// This is also used by routines that do stack dumps. If the system is
// in panic or being exited, this may not reliably stop all
// goroutines.
func stopTheWorld(reason stwReason) {
	semacquire(&worldsema)
	gp := getg()
	gp.m.preemptoff = reason.String()
	systemstack(func() {
		// Mark the goroutine which called stopTheWorld preemptible so its
		// stack may be scanned.
		// This lets a mark worker scan us while we try to stop the world
		// since otherwise we could get in a mutual preemption deadlock.
		// We must not modify anything on the G stack because a stack shrink
		// may occur. A stack shrink is otherwise OK though because in order
		// to return from this function (and to leave the system stack) we
		// must have preempted all goroutines, including any attempting
		// to scan our stack, in which case, any stack shrinking will
		// have already completed by the time we exit.
		// Don't provide a wait reason because we're still executing.
		casGToWaiting(gp, _Grunning, waitReasonStoppingTheWorld)
		stopTheWorldWithSema(reason)
		casgstatus(gp, _Gwaiting, _Grunning)
	})
}

// startTheWorld undoes the effects of stopTheWorld.
func startTheWorld() {
	systemstack(func() { startTheWorldWithSema() })

	// worldsema must be held over startTheWorldWithSema to ensure
	// gomaxprocs cannot change while worldsema is held.
	//
	// Release worldsema with direct handoff to the next waiter, but
	// acquirem so that semrelease1 doesn't try to yield our time.
	//
	// Otherwise if e.g. ReadMemStats is being called in a loop,
	// it might stomp on other attempts to stop the world, such as
	// for starting or ending GC. The operation this blocks is
	// so heavy-weight that we should just try to be as fair as
	// possible here.
	//
	// We don't want to just allow us to get preempted between now
	// and releasing the semaphore because then we keep everyone
	// (including, for example, GCs) waiting longer.
	mp := acquirem()
	mp.preemptoff = ""
	semrelease1(&worldsema, true, 0)
	releasem(mp)
}

// stopTheWorldGC has the same effect as stopTheWorld, but blocks
// until the GC is not running. It also blocks a GC from starting
// until startTheWorldGC is called.
func stopTheWorldGC(reason stwReason) {
	semacquire(&gcsema)
	stopTheWorld(reason)
}

// startTheWorldGC undoes the effects of stopTheWorldGC.
func startTheWorldGC() {
	startTheWorld()
	semrelease(&gcsema)
}

// 持有worldsema赋予了M试图阻止世界的权利。
var worldsema uint32 = 1

// Holding gcsema grants the M the right to block a GC, and blocks
// until the current GC is done. In particular, it prevents gomaxprocs
// from changing concurrently.
//
// TODO(mknyszek): Once gomaxprocs and the execution tracer can handle
// being changed/enabled during a GC, remove this.
var gcsema uint32 = 1

// stopTheWorldWithSema 是 Stop-The-World 停止世界机制的核心实现。
// 调用者负责先获取 worldsema 互斥锁并禁用抢占，然后调用 stopTheWorldWithSema：
//
//	semacquire(&worldsema, 0)
//	m.preemptoff = "原因"
//	systemstack(stopTheWorldWithSema)
//
// 完成后，调用者必须要么调用 startTheWorld 或者分别撤销这三个操作：
//
//	m.preemptoff = ""
//	systemstack(startTheWorldWithSema)
//	semrelease(&worldsema)
//
// 允许获取 worldsema 一次，然后执行多个 startTheWorldWithSema/stopTheWorldWithSema 对。
// 其他 P 可以在连续的 startTheWorldWithSema 和 stopTheWorldWithSema 调用之间执行。
// 持有 worldsema 会导致任何其他试图执行 stopTheWorld 的 goroutine 阻塞
func stopTheWorldWithSema(reason stwReason) {
	// 开始 STW 追踪。
	if traceEnabled() {
		traceSTWStart(reason)
	}
	gp := getg()

	// 如果持有锁，则无法阻止另一个被阻塞在获取锁上的 M。
	if gp.m.locks > 0 {
		// 抛出错误，因为持有锁时不应该调用 stopTheWorld。
		throw("stopTheWorld: holding locks")
	}

	lock(&sched.lock)              // 获取调度器锁，以确保在 STW 期间调度器状态的一致性。
	sched.stopwait = gomaxprocs    // 设置 stopwait 计数器为 gomaxprocs，表示需要停止的 P 的数量
	sched.gcwaiting.Store(true)    // 设置 gcwaiting 标志，表示正在进行垃圾回收
	preemptall()                   // 强制所有 P 预抢占，确保它们都停止
	gp.m.p.ptr().status = _Pgcstop // 将当前 P 的状态设为 _Pgcstop，表示它已停止
	sched.stopwait--

	// 尝试停止所有处于 Psyscall 状态的 P。
	for _, pp := range allp {
		s := pp.status
		// 遍历所有 P，如果一个 P 的状态为 _Psyscall（表示在系统调用中），则将其状态改为 _Pgcstop 已停止
		if s == _Psyscall && atomic.Cas(&pp.status, s, _Pgcstop) {
			if traceEnabled() {
				// 追踪系统调用阻塞。
				traceGoSysBlock(pp)
				// 追踪进程停止。
				traceProcStop(pp)
			}

			// 更新 stopwait 计数器
			pp.syscalltick++
			sched.stopwait--
		}
	}

	// 停止空闲的 P。
	now := nanotime()
	for {
		// 获取空闲的 P，并将其状态设为 _Pgcstop 已停止
		pp, _ := pidleget(now)
		if pp == nil {
			break
		}
		pp.status = _Pgcstop
		sched.stopwait--
	}

	// 如果 stopwait 大于 0，则表示还有 P 需要停止
	wait := sched.stopwait > 0
	unlock(&sched.lock)

	// 如果还需要等待，则循环等待，直到所有 P 都停止
	if wait {
		for {
			// 等待 100 微秒，然后尝试重新抢占以防止竞态条件。
			if notetsleep(&sched.stopnote, 100*1000) {
				noteclear(&sched.stopnote)
				break
			}
			preemptall() // 强制所有 P 预抢占，确保它们都停止
		}
	}

	// 如果 stopwait 不等于 0 或者有任何 P 的状态不是 _Pgcstop，则抛出错误
	bad := ""
	if sched.stopwait != 0 {
		bad = "stopTheWorld: not stopped (stopwait != 0)"
	} else {
		for _, pp := range allp {
			if pp.status != _Pgcstop {
				bad = "stopTheWorld: not stopped (status != _Pgcstop)"
			}
		}
	}

	// 如果 freezing 标志为真，则表示有线程正在 panic，此时锁定 deadlock 来阻止当前线程
	if freezing.Load() {
		lock(&deadlock)
		lock(&deadlock)
	}
	if bad != "" {
		// 如果检查失败，抛出错误。
		throw(bad)
	}

	// 调用 worldStopped，这通常是垃圾回收或其他需要 STW 的操作。
	worldStopped()
}

func startTheWorldWithSema() int64 {
	assertWorldStopped()

	mp := acquirem() // disable preemption because it can be holding p in a local var
	if netpollinited() {
		list := netpoll(0) // non-blocking
		injectglist(&list)
	}
	lock(&sched.lock)

	procs := gomaxprocs
	if newprocs != 0 {
		procs = newprocs
		newprocs = 0
	}
	p1 := procresize(procs)
	sched.gcwaiting.Store(false)
	if sched.sysmonwait.Load() {
		sched.sysmonwait.Store(false)
		notewakeup(&sched.sysmonnote)
	}
	unlock(&sched.lock)

	worldStarted()

	for p1 != nil {
		p := p1
		p1 = p1.link.ptr()
		if p.m != 0 {
			mp := p.m.ptr()
			p.m = 0
			if mp.nextp != 0 {
				throw("startTheWorld: inconsistent mp->nextp")
			}
			mp.nextp.set(p)
			notewakeup(&mp.park)
		} else {
			// Start M to run P.  Do not start another M below.
			newm(nil, p, -1)
		}
	}

	// Capture start-the-world time before doing clean-up tasks.
	startTime := nanotime()
	if traceEnabled() {
		traceSTWDone()
	}

	// Wakeup an additional proc in case we have excessive runnable goroutines
	// in local queues or in the global queue. If we don't, the proc will park itself.
	// If we have lots of excessive work, resetspinning will unpark additional procs as necessary.
	wakep()

	releasem(mp)

	return startTime
}

// usesLibcall indicates whether this runtime performs system calls
// via libcall.
func usesLibcall() bool {
	switch GOOS {
	case "aix", "darwin", "illumos", "ios", "solaris", "windows":
		return true
	case "openbsd":
		return GOARCH == "386" || GOARCH == "amd64" || GOARCH == "arm" || GOARCH == "arm64"
	}
	return false
}

// mStackIsSystemAllocated indicates whether this runtime starts on a
// system-allocated stack.
func mStackIsSystemAllocated() bool {
	switch GOOS {
	case "aix", "darwin", "plan9", "illumos", "ios", "solaris", "windows":
		return true
	case "openbsd":
		switch GOARCH {
		case "386", "amd64", "arm", "arm64":
			return true
		}
	}
	return false
}

// 它是用程序集编写的，使用ABI0，标记为TOPFRAME，并调用mstart0。
// 这个函数就是调度循环，除非程序退出否则永远阻塞住
func mstart()

// 是 Go 运行时中新创建的 M（机器线程）的入口点。
// 这个函数不能分割栈，因为我们可能还没有设置好栈边界。
//
// 可能在停止世界(STW, Stop-The-World)期间运行（因为它没有分配给它一个 P），所以不允许使用写屏障。
//
//go:nosplit  // 表示编译器不应在此函数中插入栈检查点
//go:nowritebarrierrec  // 表示在这个函数中不应该记录写入的屏障
func mstart0() {
	// 获取当前正在运行的 g 结构，即 goroutine。
	gp := getg()

	// 判断是否是操作系统分配的栈
	osStack := gp.stack.lo == 0
	if osStack {
		// 从系统栈初始化栈边界。
		// Cgo 可能已经在 stack.hi 中留下了栈大小。
		// minit 可能会更新栈边界。
		//
		// 注意：这些边界可能不是非常准确。
		// 我们把 hi 设置为 &size，但是上面还有其他东西。
		// 1024 是为了补偿这个，但有些随意。
		//
		// 获取栈大小
		size := gp.stack.hi
		if size == 0 {
			// 如果没有栈大小信息，初始化为默认大小
			size = 16384 * sys.StackGuardMultiplier
		}
		// 设置栈的高边界为 size 的地址
		gp.stack.hi = uintptr(noescape(unsafe.Pointer(&size)))
		// 设置栈的低边界为高边界减去栈大小加上一定的偏移量
		gp.stack.lo = gp.stack.hi - size + 1024
	}

	// 初始化栈保护区，这样我们就可以开始调用常规的 Go 代码。
	gp.stackguard0 = gp.stack.lo + stackGuard
	// 这是 g0，所以我们也可以调用 go:systemstack 函数，
	// 这些函数会检查 stackguard1。
	gp.stackguard1 = gp.stackguard0

	// 调用 mstart1 继续 M 的初始化过程。
	// 这个函数也就是调度循环，除非线程退出否则永远阻塞住
	mstart1() // 开始 m0 开始运行

	print("释放资源和结束线程 \n")
	// 退出这个线程。
	// 判断栈是否是由系统分配的
	if mStackIsSystemAllocated() {
		// Windows, Solaris, illumos, Darwin, AIX 和 Plan 9 总是系统分配栈，
		// 但是在 mstart 之前就已经放入了 gp.stack，
		// 所以上面的逻辑还没有设置 osStack。
		osStack = true
	}
	// 调用 mexit 来释放资源和结束线程，参数指示栈是否由系统分配。
	mexit(osStack)
}

// go:noinline 修饰符确保了 getcallerpc 和 getcallersp 的调用是安全的，
// 这样我们就可以设置 g0.sched 以返回到 mstart1 在 mstart0 中的调用之上。
//
//go:noinline
func mstart1() {
	// 获取当前正在运行的 g 结构，即 goroutine。
	gp := getg()

	// 如果 gp 不等于 gp.m.g0，抛出错误，这表示 g0 没有正确设置。
	if gp != gp.m.g0 {
		throw("bad runtime·mstart")
	}

	// 设置 m.g0.sched 为一个标记，返回到 mstart0 中 mstart1 调用的紧接后方， 供 goexit0 和 mcall 使用。
	// 我们在调用 schedule 之后永远不会回到 mstart1， 所以其他调用可以重用当前的栈帧。
	// goexit0 使用 gogo 语句需要从 mstart1 返回， 并允许 mstart0 退出线程。
	gp.sched.g = guintptr(unsafe.Pointer(gp)) // 设置 g0 的 g 指向自身。
	gp.sched.pc = getcallerpc()               // 设置 g0 的 pc 为调用者（mstart1 的调用者）的程序计数器。
	gp.sched.sp = getcallersp()               // 设置 g0 的 sp 为调用者（mstart1 的调用者）的栈指针。

	// 装载汇编初始化函数，进行平台特定的初始化。
	asminit()
	// 函数确保了新创建的 M 有正确的线程句柄、进程 ID、高分辨率定时器（如果可用），以及设置了正确的栈边界，这些都是初始化 M 和其 g0 必需的
	minit()

	// 安装信号处理器；在 minit 之后安装，以便 minit 可以准备线程来处理信号。
	if gp.m == &m0 {
		// mstartm0 确保了在 m0 上正确地初始化了与外部线程和信号处理相关的关键组件，为后续的并发和异常处理打下了基础
		mstartm0()
	}

	// 调用 mstartfn，如果定义的话。
	if fn := gp.m.mstartfn; fn != nil {
		fn() // 执行用户定义的 M 开始函数。
	}

	// 一个机器线程（M）通常绑定到一个运行时调度器的处理器（P）
	// 对于非 m0 的 M，从 nextp 列表中获取一个 P(调度器)，准备执行 goroutine
	if gp.m != &m0 {
		acquirep(gp.m.nextp.ptr()) // 获取一个 P，从 gp.m.nextp 指定的 P 列表中。
		gp.m.nextp = 0             // 清除 gp.m.nextp，表示 M 已经获取了一个 P。
	}

	// 调度器的一个循环：找到一个可运行的 goroutine 并执行它
	schedule() // 启动调度器
}

// mstartm0 确保了在 m0 上正确地初始化了与外部线程和信号处理相关的关键组件，为后续的并发和异常处理打下了基础
//
// 允许写屏障在这里使用，因为我们知道 GC（垃圾回收）此时还不会运行，
// 所以写屏障将会是空操作。
// 创建额外的 M：
//
//	1.检查当前环境是否使用 CGO（C 与 Go 交互）或者是否运行在 Windows 操作系统上。
//	如果满足上述条件，并且 cgoHasExtraM 标志未被设置过，则设置 cgoHasExtraM 为 true，并调用 newextram() 函数创建一个额外的 M。
//	这个额外的 M 主要用于处理非 Go 创建的线程上的回调，例如在 Windows 上由 syscall.NewCallback 创建的回调，因为这些回调可能不在 Go 的控制之下，但仍然需要进行适当的调度和资源管理。
//
//	2.初始化信号处理：
//	调用 initsig(false) 函数来初始化信号处理机制。
//	参数 false 表明这是在初始化阶段，此时不希望捕获任何信号，因为系统还未完全准备好处理信号事件。
//
//go:yeswritebarrierrec
func mstartm0() {
	// 如果当前环境使用 CGO 或者是 Windows 操作系统，
	// 并且 cgoHasExtraM 未被设置过，
	// 则创建一个额外的 M（机器线程）用于处理非 Go 语言创建的线程上的回调。
	// Windows 上的额外 M 也是必需的，用于处理由 syscall.NewCallback 创建的回调。
	// 详情请参阅 issue #6751。
	if (iscgo || GOOS == "windows") && !cgoHasExtraM {
		cgoHasExtraM = true
		newextram()
	}
	// 初始化信号处理。
	// 参数 false 表明这是初始化阶段，不希望捕获任何信号。
	initsig(false)
}

// mPark causes a thread to park itself, returning once woken.
//
//go:nosplit
func mPark() {
	gp := getg()
	notesleep(&gp.m.park)
	noteclear(&gp.m.park)
}

// mexit tears down and exits the current thread.
//
// Don't call this directly to exit the thread, since it must run at
// the top of the thread stack. Instead, use gogo(&gp.m.g0.sched) to
// unwind the stack to the point that exits the thread.
//
// It is entered with m.p != nil, so write barriers are allowed. It
// will release the P before exiting.
//
//go:yeswritebarrierrec
func mexit(osStack bool) {
	mp := getg().m

	if mp == &m0 {
		// This is the main thread. Just wedge it.
		//
		// On Linux, exiting the main thread puts the process
		// into a non-waitable zombie state. On Plan 9,
		// exiting the main thread unblocks wait even though
		// other threads are still running. On Solaris we can
		// neither exitThread nor return from mstart. Other
		// bad things probably happen on other platforms.
		//
		// We could try to clean up this M more before wedging
		// it, but that complicates signal handling.
		handoffp(releasep())
		lock(&sched.lock)
		sched.nmfreed++
		checkdead()
		unlock(&sched.lock)
		mPark()
		throw("locked m0 woke up")
	}

	sigblock(true)
	unminit()

	// Free the gsignal stack.
	if mp.gsignal != nil {
		stackfree(mp.gsignal.stack)
		// On some platforms, when calling into VDSO (e.g. nanotime)
		// we store our g on the gsignal stack, if there is one.
		// Now the stack is freed, unlink it from the m, so we
		// won't write to it when calling VDSO code.
		mp.gsignal = nil
	}

	// Remove m from allm.
	lock(&sched.lock)
	for pprev := &allm; *pprev != nil; pprev = &(*pprev).alllink {
		if *pprev == mp {
			*pprev = mp.alllink
			goto found
		}
	}
	throw("m not found in allm")
found:
	// Delay reaping m until it's done with the stack.
	//
	// Put mp on the free list, though it will not be reaped while freeWait
	// is freeMWait. mp is no longer reachable via allm, so even if it is
	// on an OS stack, we must keep a reference to mp alive so that the GC
	// doesn't free mp while we are still using it.
	//
	// Note that the free list must not be linked through alllink because
	// some functions walk allm without locking, so may be using alllink.
	mp.freeWait.Store(freeMWait)
	mp.freelink = sched.freem
	sched.freem = mp
	unlock(&sched.lock)

	atomic.Xadd64(&ncgocall, int64(mp.ncgocall))

	// Release the P.
	handoffp(releasep())
	// After this point we must not have write barriers.

	// Invoke the deadlock detector. This must happen after
	// handoffp because it may have started a new M to take our
	// P's work.
	lock(&sched.lock)
	sched.nmfreed++
	checkdead()
	unlock(&sched.lock)

	if GOOS == "darwin" || GOOS == "ios" {
		// Make sure pendingPreemptSignals is correct when an M exits.
		// For #41702.
		if mp.signalPending.Load() != 0 {
			pendingPreemptSignals.Add(-1)
		}
	}

	// Destroy all allocated resources. After this is called, we may no
	// longer take any locks.
	mdestroy(mp)

	if osStack {
		// No more uses of mp, so it is safe to drop the reference.
		mp.freeWait.Store(freeMRef)

		// Return from mstart and let the system thread
		// library free the g0 stack and terminate the thread.
		return
	}

	// mstart is the thread's entry point, so there's nothing to
	// return to. Exit the thread directly. exitThread will clear
	// m.freeWait when it's done with the stack and the m can be
	// reaped.
	exitThread(&mp.freeWait)
}

// forEachP calls fn(p) for every P p when p reaches a GC safe point.
// If a P is currently executing code, this will bring the P to a GC
// safe point and execute fn on that P. If the P is not executing code
// (it is idle or in a syscall), this will call fn(p) directly while
// preventing the P from exiting its state. This does not ensure that
// fn will run on every CPU executing Go code, but it acts as a global
// memory barrier. GC uses this as a "ragged barrier."
//
// The caller must hold worldsema.
//
//go:systemstack
func forEachP(fn func(*p)) {
	mp := acquirem()
	pp := getg().m.p.ptr()

	lock(&sched.lock)
	if sched.safePointWait != 0 {
		throw("forEachP: sched.safePointWait != 0")
	}
	sched.safePointWait = gomaxprocs - 1
	sched.safePointFn = fn

	// Ask all Ps to run the safe point function.
	for _, p2 := range allp {
		if p2 != pp {
			atomic.Store(&p2.runSafePointFn, 1)
		}
	}
	preemptall()

	// Any P entering _Pidle or _Psyscall from now on will observe
	// p.runSafePointFn == 1 and will call runSafePointFn when
	// changing its status to _Pidle/_Psyscall.

	// Run safe point function for all idle Ps. sched.pidle will
	// not change because we hold sched.lock.
	for p := sched.pidle.ptr(); p != nil; p = p.link.ptr() {
		if atomic.Cas(&p.runSafePointFn, 1, 0) {
			fn(p)
			sched.safePointWait--
		}
	}

	wait := sched.safePointWait > 0
	unlock(&sched.lock)

	// Run fn for the current P.
	fn(pp)

	// Force Ps currently in _Psyscall into _Pidle and hand them
	// off to induce safe point function execution.
	for _, p2 := range allp {
		s := p2.status
		if s == _Psyscall && p2.runSafePointFn == 1 && atomic.Cas(&p2.status, s, _Pidle) {
			if traceEnabled() {
				traceGoSysBlock(p2)
				traceProcStop(p2)
			}
			p2.syscalltick++
			handoffp(p2)
		}
	}

	// Wait for remaining Ps to run fn.
	if wait {
		for {
			// Wait for 100us, then try to re-preempt in
			// case of any races.
			//
			// Requires system stack.
			if notetsleep(&sched.safePointNote, 100*1000) {
				noteclear(&sched.safePointNote)
				break
			}
			preemptall()
		}
	}
	if sched.safePointWait != 0 {
		throw("forEachP: not done")
	}
	for _, p2 := range allp {
		if p2.runSafePointFn != 0 {
			throw("forEachP: P did not run fn")
		}
	}

	lock(&sched.lock)
	sched.safePointFn = nil
	unlock(&sched.lock)
	releasem(mp)
}

// runSafePointFn runs the safe point function, if any, for this P.
// This should be called like
//
//	if getg().m.p.runSafePointFn != 0 {
//	    runSafePointFn()
//	}
//
// runSafePointFn must be checked on any transition in to _Pidle or
// _Psyscall to avoid a race where forEachP sees that the P is running
// just before the P goes into _Pidle/_Psyscall and neither forEachP
// nor the P run the safe-point function.
func runSafePointFn() {
	p := getg().m.p.ptr()
	// Resolve the race between forEachP running the safe-point
	// function on this P's behalf and this P running the
	// safe-point function directly.
	if !atomic.Cas(&p.runSafePointFn, 1, 0) {
		return
	}
	sched.safePointFn(p)
	lock(&sched.lock)
	sched.safePointWait--
	if sched.safePointWait == 0 {
		notewakeup(&sched.safePointNote)
	}
	unlock(&sched.lock)
}

// When running with cgo, we call _cgo_thread_start
// to start threads for us so that we can play nicely with
// foreign code.
var cgoThreadStart unsafe.Pointer

type cgothreadstart struct {
	g   guintptr
	tls *uint64
	fn  unsafe.Pointer
}

// Allocate a new m unassociated with any thread.
// Can use p for allocation context if needed.
// fn is recorded as the new m's m.mstartfn.
// id is optional pre-allocated m ID. Omit by passing -1.
//
// This function is allowed to have write barriers even if the caller
// isn't because it borrows pp.
//
//go:yeswritebarrierrec
func allocm(pp *p, fn func(), id int64) *m {
	allocmLock.rlock()

	// The caller owns pp, but we may borrow (i.e., acquirep) it. We must
	// disable preemption to ensure it is not stolen, which would make the
	// caller lose ownership.
	acquirem()

	gp := getg()
	if gp.m.p == 0 {
		acquirep(pp) // temporarily borrow p for mallocs in this function
	}

	// Release the free M list. We need to do this somewhere and
	// this may free up a stack we can use.
	if sched.freem != nil {
		lock(&sched.lock)
		var newList *m
		for freem := sched.freem; freem != nil; {
			wait := freem.freeWait.Load()
			if wait == freeMWait {
				next := freem.freelink
				freem.freelink = newList
				newList = freem
				freem = next
				continue
			}
			// Free the stack if needed. For freeMRef, there is
			// nothing to do except drop freem from the sched.freem
			// list.
			if wait == freeMStack {
				// stackfree must be on the system stack, but allocm is
				// reachable off the system stack transitively from
				// startm.
				systemstack(func() {
					stackfree(freem.g0.stack)
				})
			}
			freem = freem.freelink
		}
		sched.freem = newList
		unlock(&sched.lock)
	}

	mp := new(m)
	mp.mstartfn = fn
	mcommoninit(mp, id)

	// In case of cgo or Solaris or illumos or Darwin, pthread_create will make us a stack.
	// Windows and Plan 9 will layout sched stack on OS stack.
	if iscgo || mStackIsSystemAllocated() {
		mp.g0 = malg(-1)
	} else {
		mp.g0 = malg(16384 * sys.StackGuardMultiplier)
	}
	mp.g0.m = mp

	if pp == gp.m.p.ptr() {
		releasep()
	}

	releasem(gp.m)
	allocmLock.runlock()
	return mp
}

// needm is called when a cgo callback happens on a
// thread without an m (a thread not created by Go).
// In this case, needm is expected to find an m to use
// and return with m, g initialized correctly.
// Since m and g are not set now (likely nil, but see below)
// needm is limited in what routines it can call. In particular
// it can only call nosplit functions (textflag 7) and cannot
// do any scheduling that requires an m.
//
// In order to avoid needing heavy lifting here, we adopt
// the following strategy: there is a stack of available m's
// that can be stolen. Using compare-and-swap
// to pop from the stack has ABA races, so we simulate
// a lock by doing an exchange (via Casuintptr) to steal the stack
// head and replace the top pointer with MLOCKED (1).
// This serves as a simple spin lock that we can use even
// without an m. The thread that locks the stack in this way
// unlocks the stack by storing a valid stack head pointer.
//
// In order to make sure that there is always an m structure
// available to be stolen, we maintain the invariant that there
// is always one more than needed. At the beginning of the
// program (if cgo is in use) the list is seeded with a single m.
// If needm finds that it has taken the last m off the list, its job
// is - once it has installed its own m so that it can do things like
// allocate memory - to create a spare m and put it on the list.
//
// Each of these extra m's also has a g0 and a curg that are
// pressed into service as the scheduling stack and current
// goroutine for the duration of the cgo callback.
//
// It calls dropm to put the m back on the list,
// 1. when the callback is done with the m in non-pthread platforms,
// 2. or when the C thread exiting on pthread platforms.
//
// The signal argument indicates whether we're called from a signal
// handler.
//
//go:nosplit
func needm(signal bool) {
	if (iscgo || GOOS == "windows") && !cgoHasExtraM {
		// Can happen if C/C++ code calls Go from a global ctor.
		// Can also happen on Windows if a global ctor uses a
		// callback created by syscall.NewCallback. See issue #6751
		// for details.
		//
		// Can not throw, because scheduler is not initialized yet.
		writeErrStr("fatal error: cgo callback before cgo call\n")
		exit(1)
	}

	// Save and block signals before getting an M.
	// The signal handler may call needm itself,
	// and we must avoid a deadlock. Also, once g is installed,
	// any incoming signals will try to execute,
	// but we won't have the sigaltstack settings and other data
	// set up appropriately until the end of minit, which will
	// unblock the signals. This is the same dance as when
	// starting a new m to run Go code via newosproc.
	var sigmask sigset
	sigsave(&sigmask)
	sigblock(false)

	// getExtraM is safe here because of the invariant above,
	// that the extra list always contains or will soon contain
	// at least one m.
	mp, last := getExtraM()

	// Set needextram when we've just emptied the list,
	// so that the eventual call into cgocallbackg will
	// allocate a new m for the extra list. We delay the
	// allocation until then so that it can be done
	// after exitsyscall makes sure it is okay to be
	// running at all (that is, there's no garbage collection
	// running right now).
	mp.needextram = last

	// Store the original signal mask for use by minit.
	mp.sigmask = sigmask

	// Install TLS on some platforms (previously setg
	// would do this if necessary).
	osSetupTLS(mp)

	// Install g (= m->g0) and set the stack bounds
	// to match the current stack.
	setg(mp.g0)
	sp := getcallersp()
	callbackUpdateSystemStack(mp, sp, signal)

	// Should mark we are already in Go now.
	// Otherwise, we may call needm again when we get a signal, before cgocallbackg1,
	// which means the extram list may be empty, that will cause a deadlock.
	mp.isExtraInC = false

	// Initialize this thread to use the m.
	asminit()
	minit()

	// mp.curg is now a real goroutine.
	casgstatus(mp.curg, _Gdead, _Gsyscall)
	sched.ngsys.Add(-1)
}

// Acquire an extra m and bind it to the C thread when a pthread key has been created.
//
//go:nosplit
func needAndBindM() {
	needm(false)

	if _cgo_pthread_key_created != nil && *(*uintptr)(_cgo_pthread_key_created) != 0 {
		cgoBindM()
	}
}

// newextram allocates m's and puts them on the extra list.
// It is called with a working local m, so that it can do things
// like call schedlock and allocate.
func newextram() {
	c := extraMWaiters.Swap(0)
	if c > 0 {
		for i := uint32(0); i < c; i++ {
			oneNewExtraM()
		}
	} else if extraMLength.Load() == 0 {
		// Make sure there is at least one extra M.
		oneNewExtraM()
	}
}

// oneNewExtraM allocates an m and puts it on the extra list.
func oneNewExtraM() {
	// Create extra goroutine locked to extra m.
	// The goroutine is the context in which the cgo callback will run.
	// The sched.pc will never be returned to, but setting it to
	// goexit makes clear to the traceback routines where
	// the goroutine stack ends.
	mp := allocm(nil, nil, -1)
	gp := malg(4096)
	gp.sched.pc = abi.FuncPCABI0(goexit) + sys.PCQuantum
	gp.sched.sp = gp.stack.hi
	gp.sched.sp -= 4 * goarch.PtrSize // extra space in case of reads slightly beyond frame
	gp.sched.lr = 0
	gp.sched.g = guintptr(unsafe.Pointer(gp))
	gp.syscallpc = gp.sched.pc
	gp.syscallsp = gp.sched.sp
	gp.stktopsp = gp.sched.sp
	// malg returns status as _Gidle. Change to _Gdead before
	// adding to allg where GC can see it. We use _Gdead to hide
	// this from tracebacks and stack scans since it isn't a
	// "real" goroutine until needm grabs it.
	casgstatus(gp, _Gidle, _Gdead)
	gp.m = mp
	mp.curg = gp
	mp.isextra = true
	// mark we are in C by default.
	mp.isExtraInC = true
	mp.lockedInt++
	mp.lockedg.set(gp)
	gp.lockedm.set(mp)
	gp.goid = sched.goidgen.Add(1)
	if raceenabled {
		gp.racectx = racegostart(abi.FuncPCABIInternal(newextram) + sys.PCQuantum)
	}
	if traceEnabled() {
		traceOneNewExtraM(gp)
	}
	// put on allg for garbage collector
	allgadd(gp)

	// gp is now on the allg list, but we don't want it to be
	// counted by gcount. It would be more "proper" to increment
	// sched.ngfree, but that requires locking. Incrementing ngsys
	// has the same effect.
	sched.ngsys.Add(1)

	// Add m to the extra list.
	addExtraM(mp)
}

// dropm puts the current m back onto the extra list.
//
// 1. On systems without pthreads, like Windows
// dropm is called when a cgo callback has called needm but is now
// done with the callback and returning back into the non-Go thread.
//
// The main expense here is the call to signalstack to release the
// m's signal stack, and then the call to needm on the next callback
// from this thread. It is tempting to try to save the m for next time,
// which would eliminate both these costs, but there might not be
// a next time: the current thread (which Go does not control) might exit.
// If we saved the m for that thread, there would be an m leak each time
// such a thread exited. Instead, we acquire and release an m on each
// call. These should typically not be scheduling operations, just a few
// atomics, so the cost should be small.
//
// 2. On systems with pthreads
// dropm is called while a non-Go thread is exiting.
// We allocate a pthread per-thread variable using pthread_key_create,
// to register a thread-exit-time destructor.
// And store the g into a thread-specific value associated with the pthread key,
// when first return back to C.
// So that the destructor would invoke dropm while the non-Go thread is exiting.
// This is much faster since it avoids expensive signal-related syscalls.
//
// This always runs without a P, so //go:nowritebarrierrec is required.
//
// This may run with a different stack than was recorded in g0 (there is no
// call to callbackUpdateSystemStack prior to dropm), so this must be
// //go:nosplit to avoid the stack bounds check.
//
//go:nowritebarrierrec
//go:nosplit
func dropm() {
	// Clear m and g, and return m to the extra list.
	// After the call to setg we can only call nosplit functions
	// with no pointer manipulation.
	mp := getg().m

	// Return mp.curg to dead state.
	casgstatus(mp.curg, _Gsyscall, _Gdead)
	mp.curg.preemptStop = false
	sched.ngsys.Add(1)

	// Block signals before unminit.
	// Unminit unregisters the signal handling stack (but needs g on some systems).
	// Setg(nil) clears g, which is the signal handler's cue not to run Go handlers.
	// It's important not to try to handle a signal between those two steps.
	sigmask := mp.sigmask
	sigblock(false)
	unminit()

	setg(nil)

	// Clear g0 stack bounds to ensure that needm always refreshes the
	// bounds when reusing this M.
	g0 := mp.g0
	g0.stack.hi = 0
	g0.stack.lo = 0
	g0.stackguard0 = 0
	g0.stackguard1 = 0

	putExtraM(mp)

	msigrestore(sigmask)
}

// bindm store the g0 of the current m into a thread-specific value.
//
// We allocate a pthread per-thread variable using pthread_key_create,
// to register a thread-exit-time destructor.
// We are here setting the thread-specific value of the pthread key, to enable the destructor.
// So that the pthread_key_destructor would dropm while the C thread is exiting.
//
// And the saved g will be used in pthread_key_destructor,
// since the g stored in the TLS by Go might be cleared in some platforms,
// before the destructor invoked, so, we restore g by the stored g, before dropm.
//
// We store g0 instead of m, to make the assembly code simpler,
// since we need to restore g0 in runtime.cgocallback.
//
// On systems without pthreads, like Windows, bindm shouldn't be used.
//
// NOTE: this always runs without a P, so, nowritebarrierrec required.
//
//go:nosplit
//go:nowritebarrierrec
func cgoBindM() {
	if GOOS == "windows" || GOOS == "plan9" {
		fatal("bindm in unexpected GOOS")
	}
	g := getg()
	if g.m.g0 != g {
		fatal("the current g is not g0")
	}
	if _cgo_bindm != nil {
		asmcgocall(_cgo_bindm, unsafe.Pointer(g))
	}
}

// A helper function for EnsureDropM.
func getm() uintptr {
	return uintptr(unsafe.Pointer(getg().m))
}

var (
	// Locking linked list of extra M's, via mp.schedlink. Must be accessed
	// only via lockextra/unlockextra.
	//
	// Can't be atomic.Pointer[m] because we use an invalid pointer as a
	// "locked" sentinel value. M's on this list remain visible to the GC
	// because their mp.curg is on allgs.
	extraM atomic.Uintptr
	// Number of M's in the extraM list.
	extraMLength atomic.Uint32
	// Number of waiters in lockextra.
	extraMWaiters atomic.Uint32

	// Number of extra M's in use by threads.
	extraMInUse atomic.Uint32
)

// lockextra locks the extra list and returns the list head.
// The caller must unlock the list by storing a new list head
// to extram. If nilokay is true, then lockextra will
// return a nil list head if that's what it finds. If nilokay is false,
// lockextra will keep waiting until the list head is no longer nil.
//
//go:nosplit
func lockextra(nilokay bool) *m {
	const locked = 1

	incr := false
	for {
		old := extraM.Load()
		if old == locked {
			osyield_no_g()
			continue
		}
		if old == 0 && !nilokay {
			if !incr {
				// Add 1 to the number of threads
				// waiting for an M.
				// This is cleared by newextram.
				extraMWaiters.Add(1)
				incr = true
			}
			usleep_no_g(1)
			continue
		}
		if extraM.CompareAndSwap(old, locked) {
			return (*m)(unsafe.Pointer(old))
		}
		osyield_no_g()
		continue
	}
}

//go:nosplit
func unlockextra(mp *m, delta int32) {
	extraMLength.Add(delta)
	extraM.Store(uintptr(unsafe.Pointer(mp)))
}

// Return an M from the extra M list. Returns last == true if the list becomes
// empty because of this call.
//
// Spins waiting for an extra M, so caller must ensure that the list always
// contains or will soon contain at least one M.
//
//go:nosplit
func getExtraM() (mp *m, last bool) {
	mp = lockextra(false)
	extraMInUse.Add(1)
	unlockextra(mp.schedlink.ptr(), -1)
	return mp, mp.schedlink.ptr() == nil
}

// Returns an extra M back to the list. mp must be from getExtraM. Newly
// allocated M's should use addExtraM.
//
//go:nosplit
func putExtraM(mp *m) {
	extraMInUse.Add(-1)
	addExtraM(mp)
}

// Adds a newly allocated M to the extra M list.
//
//go:nosplit
func addExtraM(mp *m) {
	mnext := lockextra(true)
	mp.schedlink.set(mnext)
	unlockextra(mp, 1)
}

var (
	// allocmLock is locked for read when creating new Ms in allocm and their
	// addition to allm. Thus acquiring this lock for write blocks the
	// creation of new Ms.
	allocmLock rwmutex

	// execLock serializes exec and clone to avoid bugs or unspecified
	// behaviour around exec'ing while creating/destroying threads. See
	// issue #19546.
	execLock rwmutex
)

// These errors are reported (via writeErrStr) by some OS-specific
// versions of newosproc and newosproc0.
const (
	failthreadcreate  = "runtime: failed to create new OS thread\n"
	failallocatestack = "runtime: failed to allocate stack for the new OS thread\n"
)

// newmHandoff contains a list of m structures that need new OS threads.
// This is used by newm in situations where newm itself can't safely
// start an OS thread.
var newmHandoff struct {
	lock mutex

	// newm points to a list of M structures that need new OS
	// threads. The list is linked through m.schedlink.
	newm muintptr

	// waiting indicates that wake needs to be notified when an m
	// is put on the list.
	waiting bool
	wake    note

	// haveTemplateThread indicates that the templateThread has
	// been started. This is not protected by lock. Use cas to set
	// to 1.
	haveTemplateThread uint32
}

// 创建一个新的 M。它将从调用 fn 函数或调度器开始。
// fn 必须是静态的，不能是堆上分配的闭包。
// id 是可选的预先分配的 M ID。如果不指定则传递 -1。
//
// 可能在 m.p 为 nil 的情况下运行，所以不允许写屏障。
//
//go:nowritebarrierrec
func newm(fn func(), pp *p, id int64) {
	// allocm 函数会向 allm 添加一个新的 M，但它们直到由 OS 在 newm1 或模板线程中创建才开始运行。
	// doAllThreadsSyscall 要求 allm 中的每个 M 最终都会开始并且可以被信号中断，即使在全局停止世界（STW）期间也是如此。
	//
	// 在这里禁用抢占，直到我们启动线程以确保 newm 不会在 allocm 和启动新线程之间被抢占，
	// 确保任何添加到 allm 的东西都保证最终会开始。
	acquirem()

	mp := allocm(pp, fn, id) // 为 M 分配内存并初始化，将其赋值给 mp
	mp.nextp.set(pp)         // 配置下一个 P
	mp.sigmask = initSigmask // 设置初始信号掩码

	if gp := getg(); gp != nil && gp.m != nil && (gp.m.lockedExt != 0 || gp.m.incgo) && GOOS != "plan9" {
		// 我们正在一个锁定的 M 上或一个可能由 C 启动的线程上。
		// 内核线程的状态可能很奇怪（用户可能为了这个目的锁定它）。
		// 我们不想把这个状态克隆到另一个线程中。
		// 相反，让一个已知良好的线程为我们创建线程。
		//
		// 在 Plan 9 上禁用这个特性。参见 golang.org/issue/22227。
		//
		// TODO: 这个特性在 Windows 上可能是不必要的，Windows 的线程创建并不基于 fork。
		lock(&newmHandoff.lock)
		if newmHandoff.haveTemplateThread == 0 {
			throw("在一个被锁的线程上，但没有模板线程")
		}
		mp.schedlink = newmHandoff.newm
		newmHandoff.newm.set(mp)
		if newmHandoff.waiting {
			newmHandoff.waiting = false
			// 唤醒模板线程
			notewakeup(&newmHandoff.wake)
		}
		unlock(&newmHandoff.lock)
		// M 尚未开始，但模板线程不参与 STW，所以它总是处理排队的 Ms，
		// 所以释放 m 是安全的。
		releasem(getg().m)
		return
	}
	newm1(mp) // 实际启动 M
	releasem(getg().m)
}

// 实际上启动一个新创建的 m 结构体，使其成为一个运行中的线程。
func newm1(mp *m) {
	// 检查是否启用了 Cgo 支持。
	if iscgo {
		// 初始化 cgothreadstart 类型的变量 ts。
		var ts cgothreadstart
		// 检查是否定义了 Cgo 的线程启动函数。
		if _cgo_thread_start == nil {
			throw("_cgo_thread_start missing")
		}
		// 设置 cgo 线程启动函数的参数 g 为 mp.g0
		ts.g.set(mp.g0)
		// 设置线程本地存储的指针为 &mp.tls[0]
		ts.tls = (*uint64)(unsafe.Pointer(&mp.tls[0]))
		// 设置线程启动函数为 mstart 的函数指针
		ts.fn = unsafe.Pointer(abi.FuncPCABI0(mstart))

		// 写入 msan 监测
		if msanenabled {
			msanwrite(unsafe.Pointer(&ts), unsafe.Sizeof(ts))
		}

		// 写入 asan 监测
		if asanenabled {
			asanwrite(unsafe.Pointer(&ts), unsafe.Sizeof(ts))
		}

		// 防止进程克隆
		execLock.rlock()
		asmcgocall(_cgo_thread_start, unsafe.Pointer(&ts)) // 调用 _cgo_thread_start 函数启动 cgo 线程
		execLock.runlock()
		return
	}

	// 如果没有启用 Cgo，直接启动一个新的操作系统线程。

	// 防止进程克隆
	execLock.rlock()
	newosproc(mp) // 调用 newosproc 来启动一个新的操作系统线程。
	execLock.runlock()
}

// startTemplateThread starts the template thread if it is not already
// running.
//
// The calling thread must itself be in a known-good state.
func startTemplateThread() {
	if GOARCH == "wasm" { // no threads on wasm yet
		return
	}

	// Disable preemption to guarantee that the template thread will be
	// created before a park once haveTemplateThread is set.
	mp := acquirem()
	if !atomic.Cas(&newmHandoff.haveTemplateThread, 0, 1) {
		releasem(mp)
		return
	}
	newm(templateThread, nil, -1)
	releasem(mp)
}

// templateThread is a thread in a known-good state that exists solely
// to start new threads in known-good states when the calling thread
// may not be in a good state.
//
// Many programs never need this, so templateThread is started lazily
// when we first enter a state that might lead to running on a thread
// in an unknown state.
//
// templateThread runs on an M without a P, so it must not have write
// barriers.
//
//go:nowritebarrierrec
func templateThread() {
	lock(&sched.lock)
	sched.nmsys++
	checkdead()
	unlock(&sched.lock)

	for {
		lock(&newmHandoff.lock)
		for newmHandoff.newm != 0 {
			newm := newmHandoff.newm.ptr()
			newmHandoff.newm = 0
			unlock(&newmHandoff.lock)
			for newm != nil {
				next := newm.schedlink.ptr()
				newm.schedlink = 0
				newm1(newm)
				newm = next
			}
			lock(&newmHandoff.lock)
		}
		newmHandoff.waiting = true
		noteclear(&newmHandoff.wake)
		unlock(&newmHandoff.lock)
		notesleep(&newmHandoff.wake)
	}
}

// Stops execution of the current m until new work is available.
// Returns with acquired P.
func stopm() {
	gp := getg()

	if gp.m.locks != 0 {
		throw("stopm holding locks")
	}
	if gp.m.p != 0 {
		throw("stopm holding p")
	}
	if gp.m.spinning {
		throw("stopm spinning")
	}

	lock(&sched.lock)
	mput(gp.m)
	unlock(&sched.lock)
	mPark()
	acquirep(gp.m.nextp.ptr())
	gp.m.nextp = 0
}

func mspinning() {
	// startm的呼叫者增加了nmspinning。设置新的M的旋转。
	getg().m.spinning = true
}

// 函数调度一个 M 来运行 P（如果有必要，创建一个新的 M）。
// 如果 p 为 nil，则尝试获取一个空闲的 P，如果没有空闲的 P，则什么也不做。
// 可能在没有 m.p 的情况下运行，所以不允许写屏障。
// 如果 spinning 设置为真，调用者已经增加了 nmspinning 并且必须提供一个 P。startm 将会在新启动的 M 中设置 m.spinning。
//
// 传递非 nil P 的调用者必须从不可抢占的上下文中调用。参见 acquirem 注释。
//
// lockheld 参数指示调用者是否已经获取了调度器锁。持有锁的调用者在调用时必须传入 true。锁可能会暂时被释放，但在返回前会重新获取。
//
// 必须没有写屏障，因为这可能在没有 P 的情况下被调用。
//
//go:nowritebarrierrec
func startm(pp *p, spinning, lockheld bool) {
	// 禁止抢占。
	//
	// 每个拥有的 P 必须有一个最终会停止它的所有者，以防 GC 停止请求。
	// startm 临时拥有 P（来自参数或 pidleget），并将所有权转移给启动的 M，后者将负责执行停止。
	//
	// 在这个临时所有权期间，必须禁用抢占，否则当前运行的 P 可能在仍持有临时 P 的情况下进入 GC 停止，导致 P 悬挂和 STW 死锁。
	//
	// 传递非 nil P 的调用者必须已经在不可抢占的上下文中，否则这样的抢占可能在 startm 函数入口处发生。
	// 传递 nil P 的调用者可能是可抢占的，所以我们必须在下面从 pidleget 获取 P 之前禁用抢占。

	// 获取当前线程 m
	mp := acquirem()
	if !lockheld {
		lock(&sched.lock)
	}

	// 尝试从空闲 P 队列中获取一个 P
	if pp == nil {
		if spinning {
			// TODO(prattmic): 对该函数的所有剩余调用
			// 用 _p_ = = nil可以清理找到一个p
			// 在调用startm之前。
			throw("startm: P required for spinning=true")
		}

		// 没有指定 p 则需要从全局空闲队列中获取一个 p
		pp, _ = pidleget(0)
		// 没有找到空闲 P，释放锁并返回
		if pp == nil {
			if !lockheld {
				unlock(&sched.lock)
			}
			releasem(mp)
			return
		}
	}

	// 尝试从空闲 M 队列中获取一个 M
	nmp := mget()

	// 如果没有可用的 M，预分配一个新的 M 的 ID，释放锁，然后调用 newm 创建一个新的 M
	if nmp == nil {
		id := mReserveID() // 配一个新的 M 的 ID
		// 释放锁
		unlock(&sched.lock)

		var fn func()
		if spinning {
			// 调用者递增nmspinning，因此在新的m中设置M.spinning。
			fn = mspinning
		}

		newm(fn, pp, id) // 创建一个新的 M

		if lockheld {
			lock(&sched.lock)
		}
		// 所有权转移在 newm 的 start 中完成。现在抢占是安全的。
		releasem(mp)
		return
	}

	if !lockheld {
		unlock(&sched.lock)
	}

	// 如果获取的 M 已经在自旋，抛出错误
	if nmp.spinning {
		throw("startm: m is spinning")
	}
	// 如果 M 已经拥有一个 P，抛出错误
	if nmp.nextp != 0 {
		throw("startm: m has p")
	}
	// 如果 spinning 为真且 P 的运行队列不为空，抛出错误
	if spinning && !runqempty(pp) {
		throw("startm: p has runnable gs")
	}

	nmp.spinning = spinning // 新 M 中设置 m.spinning。
	nmp.nextp.set(pp)       // 设置 m 马上要分配的 p
	notewakeup(&nmp.park)   // 函数唤醒等待在 note 上的一个线程。
	// 所有权转移在 wakeup 中完成。现在抢占是安全的。
	releasem(mp)
}

// 函数用于从系统调用或锁定状态中释放 P。将 P 的控制权交给另一个 M。
// 此函数总是在无 P 的上下文中运行，因此不允许写屏障。
//
//go:nowritebarrierrec
func handoffp(pp *p) {
	// handoffp 必须在任何 findrunnable 查找 G 可能返回要在 pp 上运行的 G 的情况下启动 M。

	// 如果有本地任务或者全局队列有任务，直接启动 M。
	if !runqempty(pp) || sched.runqsize != 0 {
		startm(pp, false, false) // 调度一个 M 来运行 P（如果有必要，创建一个新的 M）
		return
	}

	// 如果有跟踪工作要做，直接启动 M。
	if (traceEnabled() || traceShuttingDown()) && traceReaderAvailable() != nil {
		// 调度一个 M（机器线程）来运行 P（处理器），如果必要的话，会创建一个新的 M
		startm(pp, false, false)
		return
	}

	// 如果有 GC 工作，直接启动 M。
	if gcBlackenEnabled != 0 && gcMarkWorkAvailable(pp) {
		// 调度一个 M（机器线程）来运行 P（处理器），如果必要的话，会创建一个新的 M
		startm(pp, false, false)
		return
	}

	// 如果没有旋转或空闲的 M，直接启动 M 来处理可能的工作
	if sched.nmspinning.Load()+sched.npidle.Load() == 0 && sched.nmspinning.CompareAndSwap(0, 1) { // TODO: fast atomic
		sched.needspinning.Store(0)
		// 调度一个 M（机器线程）来运行 P（处理器），如果必要的话，会创建一个新的 M
		startm(pp, true, false)
		return
	}

	// 获取调度器锁。
	lock(&sched.lock)

	// 如果 GC 等待中，设置 P 状态为 _Pgcstop，减少 stopwait 计数。
	if sched.gcwaiting.Load() {
		pp.status = _Pgcstop
		sched.stopwait--
		if sched.stopwait == 0 {
			notewakeup(&sched.stopnote)
		}
		unlock(&sched.lock)
		return
	}

	// 如果有安全点函数需要运行，执行它。
	if pp.runSafePointFn != 0 && atomic.Cas(&pp.runSafePointFn, 1, 0) {
		sched.safePointFn(pp)
		sched.safePointWait--
		if sched.safePointWait == 0 {
			notewakeup(&sched.safePointNote)
		}
	}

	// 如果全局运行队列中有工作，解锁调度器锁并启动 M。
	if sched.runqsize != 0 {
		unlock(&sched.lock)
		// 调度一个 M（机器线程）来运行 P（处理器），如果必要的话，会创建一个新的 M
		startm(pp, false, false)
		return
	}

	// 如果这是最后一个运行的 P，且没有人正在轮询网络，直接启动 M。
	if sched.npidle.Load() == gomaxprocs-1 && sched.lastpoll.Load() != 0 {
		unlock(&sched.lock)
		// 调度一个 M（机器线程）来运行 P（处理器），如果必要的话，会创建一个新的 M
		startm(pp, false, false)
		return
	}

	// 调度器锁不能在下面调用 wakeNetPoller 时持有，
	// 因为 wakeNetPoller 可能调用 wakep，进而可能调用 startm。
	when := nobarrierWakeTime(pp)
	pidleput(pp, 0) // 没有工作要处理，把 p 放入全局空闲队列
	unlock(&sched.lock)

	// 如果有唤醒时间，唤醒网络轮询器。
	if when != 0 {
		wakeNetPoller(when)
	}
}

// Tries to add one more P to execute G's.
// Called when a G is made runnable (newproc, ready).
// Must be called with a P.
func wakep() {
	// Be conservative about spinning threads, only start one if none exist
	// already.
	if sched.nmspinning.Load() != 0 || !sched.nmspinning.CompareAndSwap(0, 1) {
		return
	}

	// Disable preemption until ownership of pp transfers to the next M in
	// startm. Otherwise preemption here would leave pp stuck waiting to
	// enter _Pgcstop.
	//
	// See preemption comment on acquirem in startm for more details.
	mp := acquirem()

	var pp *p
	lock(&sched.lock)
	pp, _ = pidlegetSpinning(0)
	if pp == nil {
		if sched.nmspinning.Add(-1) < 0 {
			throw("wakep: negative nmspinning")
		}
		unlock(&sched.lock)
		releasem(mp)
		return
	}
	// Since we always have a P, the race in the "No M is available"
	// comment in startm doesn't apply during the small window between the
	// unlock here and lock in startm. A checkdead in between will always
	// see at least one running M (ours).
	unlock(&sched.lock)

	startm(pp, true, false)

	releasem(mp)
}

// Stops execution of the current m that is locked to a g until the g is runnable again.
// Returns with acquired P.
func stoplockedm() {
	gp := getg()

	if gp.m.lockedg == 0 || gp.m.lockedg.ptr().lockedm.ptr() != gp.m {
		throw("stoplockedm: inconsistent locking")
	}
	if gp.m.p != 0 {
		// Schedule another M to run this p.
		pp := releasep()
		handoffp(pp)
	}
	incidlelocked(1)
	// Wait until another thread schedules lockedg again.
	mPark()
	status := readgstatus(gp.m.lockedg.ptr())
	if status&^_Gscan != _Grunnable {
		print("runtime:stoplockedm: lockedg (atomicstatus=", status, ") is not Grunnable or Gscanrunnable\n")
		dumpgstatus(gp.m.lockedg.ptr())
		throw("stoplockedm: not runnable")
	}
	acquirep(gp.m.nextp.ptr())
	gp.m.nextp = 0
}

// Schedules the locked m to run the locked gp.
// May run during STW, so write barriers are not allowed.
//
//go:nowritebarrierrec
func startlockedm(gp *g) {
	mp := gp.lockedm.ptr()
	if mp == getg().m {
		throw("startlockedm: locked to me")
	}
	if mp.nextp != 0 {
		throw("startlockedm: m has p")
	}
	// directly handoff current P to the locked m
	incidlelocked(-1)
	pp := releasep()
	mp.nextp.set(pp)
	notewakeup(&mp.park)
	stopm()
}

// Stops the current m for stopTheWorld.
// Returns when the world is restarted.
func gcstopm() {
	gp := getg()

	if !sched.gcwaiting.Load() {
		throw("gcstopm: not waiting for gc")
	}
	if gp.m.spinning {
		gp.m.spinning = false
		// OK to just drop nmspinning here,
		// startTheWorld will unpark threads as necessary.
		if sched.nmspinning.Add(-1) < 0 {
			throw("gcstopm: negative nmspinning")
		}
	}
	pp := releasep()
	lock(&sched.lock)
	pp.status = _Pgcstop
	sched.stopwait--
	if sched.stopwait == 0 {
		notewakeup(&sched.stopnote)
	}
	unlock(&sched.lock)
	stopm()
}

// 将 gp 调度到当前 M 上运行。
// 如果 inheritTime 为真，gp 继承当前时间片剩余的时间。
// 否则，gp 开始一个新的时间片。
// 该函数永远不会返回。
//
// 允许写屏障，因为这是在多个地方获取 P 后立即调用的。
//
//go:yeswritebarrierrec
func execute(gp *g, inheritTime bool) {
	// 获取当前线程的机器线程（M）
	mp := getg().m

	// 如果 goroutine 配置文件功能已激活，确保 gp 的栈信息被记录到配置文件中
	if goroutineProfile.active {
		tryRecordGoroutineProfile(gp, osyield)
	}

	// 将当前 M 分配给 gp，并更新 gp 的状态为 _Grunning，表示它即将运行
	mp.curg = gp
	gp.m = mp

	// 重置 gp 的等待时间、抢占标志和栈保护指针
	casgstatus(gp, _Grunnable, _Grunning)
	gp.waitsince = 0
	gp.preempt = false
	gp.stackguard0 = gp.stack.lo + stackGuard

	// 如果不继承时间片，增加当前 P 的调度计数，用于时间片轮换
	if !inheritTime {
		mp.p.ptr().schedtick++
	}

	// 检查是否需要开启或关闭 CPU 分析器，并进行相应的设置
	hz := sched.profilehz
	if mp.profilehz != hz {
		setThreadCPUProfiler(hz)
	}

	// 如果跟踪功能已启用，记录 goroutine 的退出和启动事件
	if traceEnabled() {
		if gp.syscallsp != 0 {
			traceGoSysExit()
		}
		traceGoStart()
	}

	gogo(&gp.sched) // 将控制权交给 gp 的调度状态，开始执行 gp, 实际运行 goroutine。
}

// 查找一个可运行的 goroutine 来执行。
// 它尝试从其他 P（处理器）偷取 goroutine，从本地或全局队列中获取 goroutine，或者轮询网络。
// tryWakeP 表示返回的 goroutine 不是普通的（例如 GC worker 或 trace reader），因此调用者应该尝试唤醒一个 P。
func findRunnable() (gp *g, inheritTime, tryWakeP bool) {
	// 从当前运行的 goroutine (getg()) 中获取其关联的 M（机器线程）
	mp := getg().m

	// 这里的条件和 handoffp 中的条件必须一致：
	// 如果 findRunnable 会返回一个 goroutine 来运行，handoffp 必须启动一个 M。

top: // 标记位置，用于循环重试。
	pp := mp.p.ptr() // 获取当前 M 关联的 P。

	// 如果垃圾回收（GC）等待队列中有 goroutine，暂停当前 M。
	if sched.gcwaiting.Load() {
		gcstopm()
		goto top // 循环重试。
	}

	// 如果有安全点函数需要运行，执行它。
	if pp.runSafePointFn != 0 {
		runSafePointFn()
	}

	// 检查定时器，获取当前时间和下一个定时器到期时间。
	// 这些值用于稍后的任务窃取，确保数据的相关性。
	now, pollUntil, _ := checkTimers(pp, 0)

	// 尝试调度 trace reader 跟踪阅读器。
	if traceEnabled() || traceShuttingDown() {
		// 返回应唤醒的跟踪读取器 (如果有)
		gp := traceReader()
		if gp != nil {
			// 将 trace reader 状态改为可运行。
			casgstatus(gp, _Gwaiting, _Grunnable)
			traceGoUnpark(gp, 0)
			// 返回找到的 goroutine 和标志。
			return gp, false, true
		}
	}

	// 尝试调度 GC worker。
	if gcBlackenEnabled != 0 {
		gp, tnow := gcController.findRunnableGCWorker(pp, now)
		if gp != nil {
			// 返回找到的 GC worker。
			return gp, false, true
		}
		// 更新当前时间。
		now = tnow
	}

	// 检查全局可运行队列，确保公平性。当一个 Goroutine在某个 P 上执行时，如果因为某些原因需要放弃执行权或者被抢占
	// 那么这个 Goroutine会被放入全局可运行队列中等待被再次调度执行
	// 1.Goroutine 主动让出执行权，例如调用 runtime.Gosched() 方法；
	// 2.Goroutine 执行的时间片用完，会被放入全局队列以便重新调度；
	// 3.某些系统调用（比如阻塞的网络操作）会导致 Goroutine暂时进入全局队列。
	//
	// schedtick 刚好等于61的倍数 ,每次调度器调用时递增;
	// runqsize > 0 全局可运行队列的大小;
	if pp.schedtick%61 == 0 && sched.runqsize > 0 {
		lock(&sched.lock)
		// 尝试从全局可运行队列中获取一批G
		gp := globrunqget(pp, 1)
		unlock(&sched.lock)
		if gp != nil {
			// 返回从全局队列找到的 goroutine。
			return gp, false, false
		}
	}

	// 唤醒终结器 goroutine。
	if fingStatus.Load()&(fingWait|fingWake) == fingWait|fingWake {
		if gp := wakefing(); gp != nil {
			ready(gp, 0, true)
		}
	}

	// 如果定义了 cgo_yield 函数，调用它。
	if *cgo_yield != nil {
		asmcgocall(*cgo_yield, nil)
	}

	// 尝试从本地队列获取 goroutine。
	if gp, inheritTime := runqget(pp); gp != nil {
		// 返回从本地队列找到的 goroutine。
		return gp, inheritTime, false
	}

	// 如果全局队列不为空，尝试从中获取 goroutine。
	// 1.Goroutine 主动让出执行权，例如调用 runtime.Gosched() 方法；
	// 2.Goroutine 执行的时间片用完，会被放入全局队列以便重新调度；
	// 3.某些系统调用（比如阻塞的网络操作）会导致 Goroutine暂时进入全局队列。
	//
	// runqsize > 0 全局可运行队列的大小;
	if sched.runqsize != 0 {
		lock(&sched.lock)
		// 尝试从全局可运行队列中获取一批G
		gp := globrunqget(pp, 0)
		unlock(&sched.lock)
		if gp != nil {
			return gp, false, false
		}
	}

	// 检测网络轮询器是否已经初始化，并且当前有等待的网络轮询事件，并且上一次轮询时间不为零。
	// 这是一个优化操作，避免直接进行任务窃取。
	// 当某个 Goroutine 因为在网络操作中被阻塞而无法继续执行时，它会被放入等待网络事件的队列中，等待网络事件就绪后再次被调度执行。
	if netpollinited() && netpollWaiters.Load() > 0 && sched.lastpoll.Load() != 0 {
		// 函数用于检查就绪的网络连接。运行时网络 I/O 的关键部分，
		// 它利用平台的 IO 完成端口机制来高效地检测就绪的网络连接，并准备好相应的 goroutine 进行后续的网络操作
		// 返回一个 goroutine 列表，表示这些 goroutine 的网络阻塞已经停止, 可以开始调度运行。
		if list := netpoll(0); !list.empty() {
			gp := list.pop()                      // 从事件列表中取出一个 Goroutine 并标记为可运行状态。
			injectglist(&list)                    // 将需要运行的 goroutine 列表注入到全局队列中，准备执行。
			casgstatus(gp, _Gwaiting, _Grunnable) // 原子更新 Goroutine 的状态为可运行状态。

			// 如果追踪功能开启，则记录 Goroutine 的解除阻塞事件。
			if traceEnabled() {
				traceGoUnpark(gp, 0)
			}
			// 返回从网络轮询中找到的 Goroutine。
			return gp, false, false
		}
	}

	// 尝试从任何 P 上窃取可运行的 goroutine。
	// 限制自旋 M 的数量，以防止在高 GOMAXPROCS 下程序并行度低时过度消耗 CPU。
	// 如果当前 M 正在自旋或者当前可自旋 M 数量的两倍小于 GOMAXPROCS 减去空闲的 P 数量，
	// 则进入窃取工作的逻辑。
	if mp.spinning || 2*sched.nmspinning.Load() < gomaxprocs-sched.npidle.Load() {
		if !mp.spinning {
			// 如果当前 M 不处于自旋状态，则将其转换为自旋状态。
			mp.becomeSpinning()
		}

		// 尝试从任何 P 上窃取可运行的 goroutine。
		// gp 为窃取到的 Goroutine，inheritTime 表示是否继承 Goroutine 的执行时间片，
		// tnow 为当前时间，w 表示新的待处理工作的绝对时间，newWork 表示是否有新的工作生成。
		gp, inheritTime, tnow, w, newWork := stealWork(now)
		if gp != nil {
			// 如果成功窃取到工作，返回 Goroutine gp、是否继承执行时间片 inheritTime 以及不需抢占标记 false。
			return gp, inheritTime, false
		}
		if newWork {
			// 如果可能存在新的定时器或 GC 工作，则重新开始窃取以查找新的工作。
			goto top
		}

		// 更新当前时间。
		now = tnow
		if w != 0 && (pollUntil == 0 || w < pollUntil) {
			// 如果发现更早的定时器到期时间，则更新 pollUntil。
			pollUntil = w
		}
	}

	// 我们当前没有任何工作可做。
	//
	// 如果 GC 标记阶段正在进行，并且有标记工作可用，尝试运行空闲时间标记，以利用当前的 P 而不是放弃它
	if gcBlackenEnabled != 0 && gcMarkWorkAvailable(pp) && gcController.addIdleMarkWorker() {
		// 从 GC 背景标记工作者池中获取一个节点。
		node := (*gcBgMarkWorkerNode)(gcBgMarkWorkerPool.pop())
		if node != nil {
			// 设置当前 P 的 GC 标记工作者模式为闲置模式。
			pp.gcMarkWorkerMode = gcMarkWorkerIdleMode

			// 从节点中获取 goroutine。
			gp := node.gp.ptr()

			// 将 goroutine 的状态从等待状态 (_Gwaiting) 改为可运行状态 (_Grunnable)。
			casgstatus(gp, _Gwaiting, _Grunnable)

			// 如果 trace 功能已启用，记录 goroutine 的唤醒。
			if traceEnabled() {
				traceGoUnpark(gp, 0)
			}
			// 返回找到的 goroutine 和相应的标志。
			return gp, false, false
		}
		// 如果没有找到节点，从 GC 控制器中移除闲置的标记工作者。
		gcController.removeIdleMarkWorker()
	}

	// WebAssembly 特殊处理：
	// 如果回调返回并且没有其他 goroutine 处于活跃状态，
	// 则唤醒事件处理器 goroutine，该 goroutine 会暂停执行直到触发回调。
	// 这个 beforeIdle 方法目前永远返回 nil,false
	gp, otherReady := beforeIdle(now, pollUntil)
	if gp != nil {
		// 将 goroutine 的状态从等待状态 (_Gwaiting) 改为可运行状态 (_Grunnable)。
		casgstatus(gp, _Gwaiting, _Grunnable)

		// 如果 trace 功能已启用，记录 goroutine 的唤醒。
		if traceEnabled() {
			traceGoUnpark(gp, 0)
		}

		// 返回找到的 goroutine 和相应的标志。
		return gp, false, false
	}

	// 如果有其他 goroutine 准备好，重新开始寻找工作。
	if otherReady {
		goto top
	}

	// 在我们放弃当前 P 之前，对 allp 切片做一个快照。
	// allp: 所有 p（逻辑处理器）的数组，长度等于 gomaxprocs
	//
	// allp 切片可能会在我们不再阻塞安全点（safe-point）时改变，
	// 因此我们需要快照来保持一致。我们不需要快照切片的内容，
	// 因为 allp 切片的前 cap(allp) 元素是不可变的。
	allpSnapshot := allp
	// 同样，对掩码（mask）也进行快照。值的变化是可以接受的，
	// 但是我们不能允许长度在我们使用过程中发生变化。
	idlepMaskSnapshot := idlepMask
	timerpMaskSnapshot := timerpMask

	// 返回 P 并进入阻塞状态。
	lock(&sched.lock)

	// 检查是否需要等待 GC 或执行安全点函数。
	if sched.gcwaiting.Load() || pp.runSafePointFn != 0 {
		// 如果有 GC 等待或有安全点函数要执行，则解锁并重新开始循环。
		unlock(&sched.lock)
		goto top
	}

	// 检查全局运行队列。
	if sched.runqsize != 0 {
		// 如果全局运行队列不为空，尝试从中获取一个 goroutine。
		gp := globrunqget(pp, 0)
		unlock(&sched.lock)
		// 返回找到的 goroutine。
		return gp, false, false
	}

	// 检查是否需要切换到自旋状态。
	if !mp.spinning && sched.needspinning.Load() == 1 {
		// See "Delicate dance" comment below.
		mp.becomeSpinning()
		unlock(&sched.lock)
		goto top
	}

	// 确认释放的 P 是否正确。
	if releasep() != pp {
		throw("findrunnable: wrong p")
	}

	// 将当前 P 设置为闲置状态。
	now = pidleput(pp, now)
	unlock(&sched.lock)

	// 线程从自旋状态转换到非自旋状态，
	// 这可能与新工作提交并发发生。我们必须首先减少自旋线程计数，
	// 然后重新检查所有工作来源（在 StoreLoad 内存屏障之间）。
	// 如果我们反向操作，另一个线程可能在我们检查完所有来源之后
	// 但还没减少 nmspinning 之前提交工作；结果将不会有任何线程被唤醒去执行工作。
	//
	// 这适用于以下工作来源：
	// * 添加到每个 P 运行队列的 goroutine。
	// * 每个 P 定时器堆上的新或早先修改的定时器。
	// * 闲置优先级的 GC 工作（除非有 golang.org/issue/19112）。
	//
	// 如果我们发现新工作，我们需要恢复 m.spinning 状态作为信号，
	// 以便 resetspinning 可以唤醒一个新的工作线程（因为可能有多个饥饿的 goroutine）。
	//
	// 但是，如果我们发现新工作后也观察到没有闲置的 P，
	// 我们就遇到了问题。我们可能正在与非自旋状态的 M 竞争，
	// 它已经找不到工作正准备释放它的 P 并停车。让那个 P 变成闲置状态会导致
	// 工作保护的损失（有可运行工作时闲置的 P）。这在不太可能发生的情况下
	// （即我们正与所有其他 P 停车竞争时恰好发现来自 netpoll 的新工作）
	// 可能导致完全死锁。
	//
	// 我们使用 sched.needspinning 来与即将闲置的非自旋状态 Ms 同步。
	// 如果它们即将放弃 P 时 needspinning 被设置，它们将取消放弃并代替
	// 成为我们服务的新自旋 M。如果我们没有竞争并且系统确实满负荷，
	// 那么不需要自旋线程，下一个自然变成自旋状态的线程将清除标志。
	//
	// 参见文件顶部的“工作线程停车/唤醒”注释。
	wasSpinning := mp.spinning

	// 记录当前 M 是否处于自旋状态。
	if mp.spinning {
		mp.spinning = false
		// 将当前 M 状态从自旋改为非自旋，并更新自旋 M 数量。
		if sched.nmspinning.Add(-1) < 0 {
			throw("findrunnable: negative nmspinning")
		}

		// 请注意：为了正确性，只有从自旋状态切换到非自旋状态的最后一个 M 必须执行以下重新检查，
		// 以确保没有遗漏的工作。然而，运行时在一些情况下会有瞬时增加 nmspinning 而不经过此路径减少，
		// 因此我们必须保守地在所有自旋的 M 上执行检查。
		//
		// 参考：https://go.dev/issue/43997。

		// 再次检查所有运行队列。
		// 在所有 P 上检查运行队列，获取工作。
		pp := checkRunqsNoP(allpSnapshot, idlepMaskSnapshot)
		if pp != nil {
			// 如果获取到了新的工作 P，则将 M 关联到该 P 上。
			acquirep(pp)
			mp.becomeSpinning()
			goto top
		}

		// 再次检查是否存在空闲的 GC 工作。
		// 在所有 P 上检查是否存在空闲的 GC 工作。
		pp, gp := checkIdleGCNoP()
		if pp != nil {
			// 如果存在空闲的 GC 工作，则将 M 关联到该 P 上，并执行 GC 相关操作。
			acquirep(pp)
			mp.becomeSpinning()

			// 运行闲置 worker。
			pp.gcMarkWorkerMode = gcMarkWorkerIdleMode
			casgstatus(gp, _Gwaiting, _Grunnable)
			if traceEnabled() {
				traceGoUnpark(gp, 0)
			}
			// 返回执行的 Goroutine 及相关信息。
			return gp, false, false
		}

		// 最后，检查定时器是否存在新的定时器或定时器到期事件。
		// 在所有 P 上检查定时器，更新 pollUntil 变量。
		pollUntil = checkTimersNoP(allpSnapshot, timerpMaskSnapshot, pollUntil)
	}

	// 轮询网络直到下一个定时器。
	// 如果网络轮询已初始化，并且存在网络轮询等待者或存在下一个定时器时间，且上次轮询成功标记不为0，则进行网络轮询。
	if netpollinited() && (netpollWaiters.Load() > 0 || pollUntil != 0) && sched.lastpoll.Swap(0) != 0 {
		// 更新调度器的轮询时间。
		sched.pollUntil.Store(pollUntil)

		// 如果当前 M 已绑定 P，则抛出错误。
		if mp.p != 0 {
			throw("findrunnable: netpoll with p")
		}

		// 如果当前 M 处于自旋状态，则抛出错误。
		if mp.spinning {
			throw("findrunnable: netpoll with spinning")
		}

		delay := int64(-1)
		if pollUntil != 0 {
			if now == 0 {
				now = nanotime()
			}
			// 计算需要延迟的时间。
			delay = pollUntil - now
			if delay < 0 {
				delay = 0
			}
		}

		// 如果使用假时间，则只进行轮询而不等待。
		if faketime != 0 {
			// 使用假时间时，只进行轮询。
			delay = 0
		}

		// 阻塞直到有新工作可用。
		// 进行网络轮询操作，延迟指定时间后返回就绪 Goroutine 列表。
		list := netpoll(delay)
		// 完成阻塞后刷新当前时间。
		now = nanotime()
		sched.pollUntil.Store(0)
		sched.lastpoll.Store(now)
		if faketime != 0 && list.empty() {
			// 如果使用假时间且没有准备好任何工作，则停止当前 M。
			// 当所有 M 停止时，checkdead 将调用 timejump 来调整时钟。
			stopm()
			goto top
		}
		lock(&sched.lock)
		pp, _ := pidleget(now)
		unlock(&sched.lock)

		// 如果没有空闲 P，则将就绪列表注入调度器中。
		if pp == nil {
			injectglist(&list)
		} else {
			// 如果有空闲 P，则将 M 绑定到该 P 上。
			acquirep(pp)
			if !list.empty() {
				// 如果就绪列表不为空，则将就绪 Goroutine 取出并运行。
				gp := list.pop()
				injectglist(&list)
				casgstatus(gp, _Gwaiting, _Grunnable)
				if traceEnabled() {
					traceGoUnpark(gp, 0)
				}
				// 返回执行的 Goroutine 及相关信息。
				return gp, false, false
			}

			// 如果之前该 M 处于自旋状态，则恢复自旋状态。
			if wasSpinning {
				mp.becomeSpinning()
			}
			goto top
		}
	} else if pollUntil != 0 && netpollinited() {
		// 如果存在下一个定时器且网络轮询已初始化，则继续判断是否需要调用 netpollBreak。
		pollerPollUntil := sched.pollUntil.Load()
		if pollerPollUntil == 0 || pollerPollUntil > pollUntil {
			// 如果当前定时器需要更新或者定时器时间变更，则调用 netpollBreak。
			netpollBreak()
		}
	}

	// 停止当前m的执行，直到新的工作可用
	stopm()
	goto top
}

// pollWork reports whether there is non-background work this P could
// be doing. This is a fairly lightweight check to be used for
// background work loops, like idle GC. It checks a subset of the
// conditions checked by the actual scheduler.
func pollWork() bool {
	if sched.runqsize != 0 {
		return true
	}
	p := getg().m.p.ptr()
	if !runqempty(p) {
		return true
	}
	if netpollinited() && netpollWaiters.Load() > 0 && sched.lastpoll.Load() != 0 {
		if list := netpoll(0); !list.empty() {
			injectglist(&list)
			return true
		}
	}
	return false
}

// 尝试从任何 P 上窃取可运行的 goroutine 或定时器。
// 从所有 p 调度器中取出一个可执行的任务, 一共取4次, 最多有 4*len(p) 个任务
//
// 如果 newWork 为真，可能有新工作已被准备就绪。
//
// 如果 now 不为 0，则它是当前时间。stealWork 函数返回传递的时间或
// 如果 now 为 0 时的当前时间。
func stealWork(now int64) (gp *g, inheritTime bool, rnow, pollUntil int64, newWork bool) {
	// 获取当前协程关联的 P。
	pp := getg().m.p.ptr()

	// 标记是否已运行定时器。
	ranTimer := false

	const stealTries = 4
	// 从所有 p 调度器中取出一个可执行的任务, 一共取4次, 最多有 4*len(p) 个任务
	for i := 0; i < stealTries; i++ {
		stealTimersOrRunNextG := i == stealTries-1

		// 遍历所有 P 的顺序, 以一个随机数开始。
		for enum := stealOrder.start(fastrand()); !enum.done(); enum.next() {
			// GC 工作可能可用。
			if sched.gcwaiting.Load() {
				return nil, false, now, pollUntil, true
			}

			// allp 所有 p（逻辑处理器）切片获取索引位置的 p
			p2 := allp[enum.position()]
			if pp == p2 {
				// 如果是当前 P 则跳过。
				continue
			}

			// 从 p2 窃取定时器。
			// 在最后一轮之前唯一可能持有不同 P 的定时器锁的地方。
			if stealTimersOrRunNextG && timerpMask.read(enum.position()) {
				tnow, w, ran := checkTimers(p2, now)
				now = tnow
				if w != 0 && (pollUntil == 0 || w < pollUntil) {
					pollUntil = w
				}
				if ran {
					// 运行定时器可能已经使任意数量的 G 变为可运行状态。
					if gp, inheritTime := runqget(pp); gp != nil {
						// 尝试从当前 P 的本地运行队列获取 Goroutine，并返回。
						return gp, inheritTime, now, pollUntil, ranTimer
					}
					ranTimer = true
				}
			}

			// 如果 p2 是闲置的，则不要费力尝试窃取。
			if !idlepMask.read(enum.position()) {
				if gp := runqsteal(pp, p2, stealTimersOrRunNextG); gp != nil {
					// 尝试从其他 P 窃取可运行的 Goroutine 并返回。
					return gp, false, now, pollUntil, ranTimer
				}
			}
		}
	}

	// 没有找到可以窃取的 goroutine。不过，运行定时器可能已经
	// 使我们错过的某些 goroutine 变为可运行状态。指示等待的下一个定时器。
	return nil, false, now, pollUntil, ranTimer
}

// Check all Ps for a runnable G to steal.
//
// On entry we have no P. If a G is available to steal and a P is available,
// the P is returned which the caller should acquire and attempt to steal the
// work to.
func checkRunqsNoP(allpSnapshot []*p, idlepMaskSnapshot pMask) *p {
	for id, p2 := range allpSnapshot {
		if !idlepMaskSnapshot.read(uint32(id)) && !runqempty(p2) {
			lock(&sched.lock)
			pp, _ := pidlegetSpinning(0)
			if pp == nil {
				// Can't get a P, don't bother checking remaining Ps.
				unlock(&sched.lock)
				return nil
			}
			unlock(&sched.lock)
			return pp
		}
	}

	// No work available.
	return nil
}

// Check all Ps for a timer expiring sooner than pollUntil.
//
// Returns updated pollUntil value.
func checkTimersNoP(allpSnapshot []*p, timerpMaskSnapshot pMask, pollUntil int64) int64 {
	for id, p2 := range allpSnapshot {
		if timerpMaskSnapshot.read(uint32(id)) {
			w := nobarrierWakeTime(p2)
			if w != 0 && (pollUntil == 0 || w < pollUntil) {
				pollUntil = w
			}
		}
	}

	return pollUntil
}

// Check for idle-priority GC, without a P on entry.
//
// If some GC work, a P, and a worker G are all available, the P and G will be
// returned. The returned P has not been wired yet.
func checkIdleGCNoP() (*p, *g) {
	// N.B. Since we have no P, gcBlackenEnabled may change at any time; we
	// must check again after acquiring a P. As an optimization, we also check
	// if an idle mark worker is needed at all. This is OK here, because if we
	// observe that one isn't needed, at least one is currently running. Even if
	// it stops running, its own journey into the scheduler should schedule it
	// again, if need be (at which point, this check will pass, if relevant).
	if atomic.Load(&gcBlackenEnabled) == 0 || !gcController.needIdleMarkWorker() {
		return nil, nil
	}
	if !gcMarkWorkAvailable(nil) {
		return nil, nil
	}

	// Work is available; we can start an idle GC worker only if there is
	// an available P and available worker G.
	//
	// We can attempt to acquire these in either order, though both have
	// synchronization concerns (see below). Workers are almost always
	// available (see comment in findRunnableGCWorker for the one case
	// there may be none). Since we're slightly less likely to find a P,
	// check for that first.
	//
	// Synchronization: note that we must hold sched.lock until we are
	// committed to keeping it. Otherwise we cannot put the unnecessary P
	// back in sched.pidle without performing the full set of idle
	// transition checks.
	//
	// If we were to check gcBgMarkWorkerPool first, we must somehow handle
	// the assumption in gcControllerState.findRunnableGCWorker that an
	// empty gcBgMarkWorkerPool is only possible if gcMarkDone is running.
	lock(&sched.lock)
	pp, now := pidlegetSpinning(0)
	if pp == nil {
		unlock(&sched.lock)
		return nil, nil
	}

	// Now that we own a P, gcBlackenEnabled can't change (as it requires STW).
	if gcBlackenEnabled == 0 || !gcController.addIdleMarkWorker() {
		pidleput(pp, now)
		unlock(&sched.lock)
		return nil, nil
	}

	node := (*gcBgMarkWorkerNode)(gcBgMarkWorkerPool.pop())
	if node == nil {
		pidleput(pp, now)
		unlock(&sched.lock)
		gcController.removeIdleMarkWorker()
		return nil, nil
	}

	unlock(&sched.lock)

	return pp, node.gp.ptr()
}

// wakeNetPoller wakes up the thread sleeping in the network poller if it isn't
// going to wake up before the when argument; or it wakes an idle P to service
// timers and the network poller if there isn't one already.
func wakeNetPoller(when int64) {
	if sched.lastpoll.Load() == 0 {
		// In findrunnable we ensure that when polling the pollUntil
		// field is either zero or the time to which the current
		// poll is expected to run. This can have a spurious wakeup
		// but should never miss a wakeup.
		pollerPollUntil := sched.pollUntil.Load()
		if pollerPollUntil == 0 || pollerPollUntil > when {
			netpollBreak()
		}
	} else {
		// There are no threads in the network poller, try to get
		// one there so it can handle new timers.
		if GOOS != "plan9" { // Temporary workaround - see issue #42303.
			wakep()
		}
	}
}

func resetspinning() {
	gp := getg()
	if !gp.m.spinning {
		throw("resetspinning: not a spinning m")
	}
	gp.m.spinning = false
	nmspinning := sched.nmspinning.Add(-1)
	if nmspinning < 0 {
		throw("findrunnable: negative nmspinning")
	}
	// M wakeup policy is deliberately somewhat conservative, so check if we
	// need to wakeup another P here. See "Worker thread parking/unparking"
	// comment at the top of the file for details.
	wakep()
}

// 函数将 glist 中的所有可运行 G 添加到某个运行队列，
// 并清空 glist。如果没有当前的 P，它们会被添加到全局队列，
// 并且最多 npidle 个 M 会被启动来运行它们。
// 否则，对于每个空闲的 P，会将一个 G 添加到全局队列并启动一个 M。
// 剩余的 G 会被添加到当前 P 的本地运行队列。
// 此函数可能暂时获取 sched.lock。
// 可以与 GC 并行运行。
func injectglist(glist *gList) {
	// 如果 glist 为空，直接返回。
	if glist.empty() {
		return
	}

	// 记录 Goroutine 的 Unpark 事件。
	if traceEnabled() {
		for gp := glist.head.ptr(); gp != nil; gp = gp.schedlink.ptr() {
			traceGoUnpark(gp, 0)
		}
	}

	// 将所有 Goroutine 标记为可运行状态，然后再将它们放入运行队列。
	head := glist.head.ptr()
	var tail *g
	qsize := 0
	for gp := head; gp != nil; gp = gp.schedlink.ptr() {
		tail = gp
		qsize++
		// 将 Goroutine 状态从等待改为可运行。
		casgstatus(gp, _Gwaiting, _Grunnable)
	}

	// 将 gList 转换为 gQueue。
	var q gQueue
	q.head.set(head)
	q.tail.set(tail)
	*glist = gList{} // 清空 glist。

	// 启动空闲的 M 来处理等待的 Goroutine。
	startIdle := func(n int) {
		for i := 0; i < n; i++ {
			mp := acquirem() // 获取一个 M。
			lock(&sched.lock)

			pp, _ := pidlegetSpinning(0)
			if pp == nil {
				unlock(&sched.lock)
				releasem(mp)
				break
			}

			// 启动 M 在 P 上运行 Goroutine。
			startm(pp, false, true)
			unlock(&sched.lock)
			releasem(mp)
		}
	}

	pp := getg().m.p.ptr()
	// 如果当前 m 线程没有 P 调度器。
	if pp == nil {
		lock(&sched.lock)
		globrunqputbatch(&q, int32(qsize)) // 将 Goroutine 批量添加到全局队列。
		unlock(&sched.lock)
		startIdle(qsize) // 启动 M 来处理所有等待的 Goroutine。
		return
	}

	// 计算空闲的 P 的数量，并尝试将 Goroutine 分配给它们。
	npidle := int(sched.npidle.Load())
	var globq gQueue
	var n int
	for n = 0; n < npidle && !q.empty(); n++ {
		g := q.pop()      // 从队列中取出一个 Goroutine。
		globq.pushBack(g) // 将 Goroutine 放入临时队列。
	}

	if n > 0 {
		lock(&sched.lock)
		globrunqputbatch(&globq, int32(n)) // 将 Goroutine 批量添加到全局队列。
		unlock(&sched.lock)
		startIdle(n) // 启动 M 来处理分配给全局队列的 Goroutine。
		qsize -= n
	}

	// 如果还有剩余的 Goroutine，将它们添加到当前 P 的本地队列。
	if !q.empty() {
		runqputbatch(pp, &q, qsize)
	}
}

// 调度器的一个循环：找到一个可运行的 goroutine 并执行它，同时也处理了各种边界情况
// 如锁持有、自旋状态、冻结世界、调度禁用等，以维护系统的稳定性和一致性
// 该函数永不返回。
func schedule() {
	mp := getg().m

	// 如果当前 M 持有锁，则抛出错误，因为调度器不应该在持有锁的情况下运行
	if mp.locks != 0 {
		throw("schedule: holding locks")
	}
	// 如果 M 正在锁定一个 goroutine 准备执行，停止执行当前 M 并执行那个锁定的 goroutine
	if mp.lockedg != 0 {
		stoplockedm()
		// 执行锁定的 goroutine，永不返回。
		execute(mp.lockedg.ptr(), false)
	}

	// 如果正在执行 cgo 调用，抛出错误，因为 cgo 调用使用的是 M 的 g0 栈，此时不应该切换 goroutine
	if mp.incgo {
		throw("schedule: in cgo")
	}

top:
	// 获取当前 M 的 P，并清除抢占标志
	pp := mp.p.ptr()
	pp.preempt = false

	// 安全检查：如果 M 正在自旋，可运行队列应该是空的。可运行队列不为空不可能自旋, 因为至少会有一个任务执行
	// 在调用 checkTimers 之前检查，因为 checkTimers 可能会调用 goready 把就绪的 goroutine 放入本地可运行队列。
	if mp.spinning && (pp.runnext != 0 || pp.runqhead != pp.runqtail) {
		throw("schedule: spinning with local work")
	}

	// 调用 findRunnable 函数寻找可运行的 goroutine，如果找不到则阻塞等待
	// 它尝试从其他 P（处理器）偷取 goroutine，从本地或全局队列中获取 goroutine，或者轮询网络。
	gp, inheritTime, tryWakeP := findRunnable()

	// 如果禁止冻结世界并且正在冻结，造成死锁以阻止新的 goroutine 运行
	// 目的是不让新的 goroutine 运行，避免干扰冻结世界的行为。
	if debug.dontfreezetheworld > 0 && freezing.Load() {
		lock(&deadlock)
		lock(&deadlock)
	}

	// 如果 M 正在自旋，重置自旋状态，并可能启动新的自旋 M
	// 所以如果它被标记为自旋状态，现在需要重置，并可能启动一个新的自旋 M。
	if mp.spinning {
		resetspinning()
	}

	// 如果调度被禁用，将 goroutine 放入待运行列表,等到重新启用用户调度时再查看。
	if sched.disable.user && !schedEnabled(gp) {
		lock(&sched.lock)
		if schedEnabled(gp) {
			unlock(&sched.lock)
		} else {
			sched.disable.runnable.pushBack(gp)
			sched.disable.n++
			unlock(&sched.lock)
			goto top // 循环调度
		}
	}

	// 如果需要，唤醒一个 P，用于非普通 goroutine （比如 GC worker 或 tracereader）的调度
	if tryWakeP {
		wakep()
	}

	// 如果 gp 锁定了另一个 M，将自己的 P 交给那个 M，然后等待一个新的 P。
	if gp.lockedm != 0 {
		startlockedm(gp)
		goto top // 循环调度
	}

	// execute 函数是永远不会返回的，因为它通过 gogo(&gp.sched) 直接跳转到了 goroutine 的执行代码中。
	// 但是，当 goroutine 结束后，控制权回到了调度器，这通常发生在 goroutine 的执行代码中调用了 goexit 或者当 goroutine 的主函数执行完毕时。
	// 因此，尽管 execute 不会返回到 schedule，但 schedule 会不断地被调用来寻找下一个可运行的 goroutine。
	// 这就是为什么 schedule 函数看起来像是在循环，因为它会一直运行，直到程序结束或所有 goroutine 都已完成。
	// 原理是执行完成一个goroutine之后并不是直接调用销毁,而是底层继续调用goexit0退出方法, 这个退出方法会重新调用到这个 schedule 函数
	execute(gp, inheritTime) // 执行找到的 goroutine。
}

// dropg removes the association between m and the current goroutine m->curg (gp for short).
// Typically a caller sets gp's status away from Grunning and then
// immediately calls dropg to finish the job. The caller is also responsible
// for arranging that gp will be restarted using ready at an
// appropriate time. After calling dropg and arranging for gp to be
// readied later, the caller can do other work but eventually should
// call schedule to restart the scheduling of goroutines on this m.
func dropg() {
	gp := getg()

	setMNoWB(&gp.m.curg.m, nil)
	setGNoWB(&gp.m.curg, nil)
}

// checkTimers runs any timers for the P that are ready.
// If now is not 0 it is the current time.
// It returns the passed time or the current time if now was passed as 0.
// and the time when the next timer should run or 0 if there is no next timer,
// and reports whether it ran any timers.
// If the time when the next timer should run is not 0,
// it is always larger than the returned time.
// We pass now in and out to avoid extra calls of nanotime.
//
//go:yeswritebarrierrec
func checkTimers(pp *p, now int64) (rnow, pollUntil int64, ran bool) {
	// If it's not yet time for the first timer, or the first adjusted
	// timer, then there is nothing to do.
	next := pp.timer0When.Load()
	nextAdj := pp.timerModifiedEarliest.Load()
	if next == 0 || (nextAdj != 0 && nextAdj < next) {
		next = nextAdj
	}

	if next == 0 {
		// No timers to run or adjust.
		return now, 0, false
	}

	if now == 0 {
		now = nanotime()
	}
	if now < next {
		// Next timer is not ready to run, but keep going
		// if we would clear deleted timers.
		// This corresponds to the condition below where
		// we decide whether to call clearDeletedTimers.
		if pp != getg().m.p.ptr() || int(pp.deletedTimers.Load()) <= int(pp.numTimers.Load()/4) {
			return now, next, false
		}
	}

	lock(&pp.timersLock)

	if len(pp.timers) > 0 {
		adjusttimers(pp, now)
		for len(pp.timers) > 0 {
			// Note that runtimer may temporarily unlock
			// pp.timersLock.
			if tw := runtimer(pp, now); tw != 0 {
				if tw > 0 {
					pollUntil = tw
				}
				break
			}
			ran = true
		}
	}

	// If this is the local P, and there are a lot of deleted timers,
	// clear them out. We only do this for the local P to reduce
	// lock contention on timersLock.
	if pp == getg().m.p.ptr() && int(pp.deletedTimers.Load()) > len(pp.timers)/4 {
		clearDeletedTimers(pp)
	}

	unlock(&pp.timersLock)

	return now, pollUntil, ran
}

func parkunlock_c(gp *g, lock unsafe.Pointer) bool {
	unlock((*mutex)(lock))
	return true
}

// park continuation on g0.
func park_m(gp *g) {
	mp := getg().m

	if traceEnabled() {
		traceGoPark(mp.waitTraceBlockReason, mp.waitTraceSkip)
	}

	// N.B. Not using casGToWaiting here because the waitreason is
	// set by park_m's caller.
	casgstatus(gp, _Grunning, _Gwaiting)
	dropg()

	if fn := mp.waitunlockf; fn != nil {
		ok := fn(gp, mp.waitlock)
		mp.waitunlockf = nil
		mp.waitlock = nil
		if !ok {
			if traceEnabled() {
				traceGoUnpark(gp, 2)
			}
			casgstatus(gp, _Gwaiting, _Grunnable)
			execute(gp, true) // Schedule it back, never returns.
		}
	}
	schedule()
}

func goschedImpl(gp *g) {
	status := readgstatus(gp)
	if status&^_Gscan != _Grunning {
		dumpgstatus(gp)
		throw("bad g status")
	}
	casgstatus(gp, _Grunning, _Grunnable)
	dropg()
	lock(&sched.lock)
	globrunqput(gp)
	unlock(&sched.lock)

	schedule()
}

// Gosched continuation on g0.
func gosched_m(gp *g) {
	if traceEnabled() {
		traceGoSched()
	}
	goschedImpl(gp)
}

// goschedguarded is a forbidden-states-avoided version of gosched_m.
func goschedguarded_m(gp *g) {

	if !canPreemptM(gp.m) {
		gogo(&gp.sched) // never return
	}

	if traceEnabled() {
		traceGoSched()
	}
	goschedImpl(gp)
}

func gopreempt_m(gp *g) {
	if traceEnabled() {
		traceGoPreempt()
	}
	goschedImpl(gp)
}

// preemptPark parks gp and puts it in _Gpreempted.
//
//go:systemstack
func preemptPark(gp *g) {
	if traceEnabled() {
		traceGoPark(traceBlockPreempted, 0)
	}
	status := readgstatus(gp)
	if status&^_Gscan != _Grunning {
		dumpgstatus(gp)
		throw("bad g status")
	}

	if gp.asyncSafePoint {
		// Double-check that async preemption does not
		// happen in SPWRITE assembly functions.
		// isAsyncSafePoint must exclude this case.
		f := findfunc(gp.sched.pc)
		if !f.valid() {
			throw("preempt at unknown pc")
		}
		if f.flag&abi.FuncFlagSPWrite != 0 {
			println("runtime: unexpected SPWRITE function", funcname(f), "in async preempt")
			throw("preempt SPWRITE")
		}
	}

	// Transition from _Grunning to _Gscan|_Gpreempted. We can't
	// be in _Grunning when we dropg because then we'd be running
	// without an M, but the moment we're in _Gpreempted,
	// something could claim this G before we've fully cleaned it
	// up. Hence, we set the scan bit to lock down further
	// transitions until we can dropg.
	casGToPreemptScan(gp, _Grunning, _Gscan|_Gpreempted)
	dropg()
	casfrom_Gscanstatus(gp, _Gscan|_Gpreempted, _Gpreempted)
	schedule()
}

// goyield is like Gosched, but it:
// - emits a GoPreempt trace event instead of a GoSched trace event
// - puts the current G on the runq of the current P instead of the globrunq
func goyield() {
	checkTimeouts()
	mcall(goyield_m)
}

func goyield_m(gp *g) {
	if traceEnabled() {
		traceGoPreempt()
	}
	pp := gp.m.p.ptr()
	casgstatus(gp, _Grunning, _Grunnable)
	dropg()
	runqput(pp, gp, false)
	schedule()
}

// 完成当前 goroutine 的执行。
// 这个函数确保在 goroutine 结束前执行一些清理工作，
// 如 race detector 和 trace 的结束通知，然后调用 mcall(goexit0) 实际完成退出。
func goexit1() {
	// 如果 race detector 已启用，则通知 race detector 当前 goroutine 即将结束。
	if raceenabled {
		racegoend()
	}

	// 如果 trace 功能已启用，则通知 trace 当前 goroutine 即将结束。
	if traceEnabled() {
		traceGoEnd()
	}

	// 调用 mcall(goexit0)，这是一个低级别的调用，直接调用到汇编语言代码，
	// 这里的 goexit0 是最终完成 goroutine 退出的函数。
	// mcall 通常用于调用不需要 GC（垃圾回收）保护的函数。
	mcall(goexit0)
}

// goexit0 函数在 g0 上继续执行 goexit 流程。
func goexit0(gp *g) {
	// 获取当前 goroutine 所在的 m 对象。
	mp := getg().m
	// 获取 m 对象关联的 p 对象。
	pp := mp.p.ptr()

	// 将 gp 的状态从运行中 (_Grunning) 改为已死亡 (_Gdead)。
	casgstatus(gp, _Grunning, _Gdead)
	// 计算并添加 gp 的可扫描栈大小到 pp 的扫描控制器中，用于 GC。
	gcController.addScannableStack(pp, -int64(gp.stack.hi-gp.stack.lo))

	// 如果 gp 是系统 goroutine，则减少系统 goroutine 的计数。
	if isSystemGoroutine(gp, false) {
		sched.ngsys.Add(-1)
	}

	gp.m = nil                     // 清理 gp 的 m 字段，避免资源泄漏。
	locked := gp.lockedm != 0      // 记录 gp 是否锁定过线程。
	gp.lockedm = 0                 // 清除 gp 的 lockedm 标志。
	mp.lockedg = 0                 // 清除 m 的 lockedg 标志。
	gp.preemptStop = false         // 清除 gp 的 preemptStop 标志。
	gp.paniconfault = false        // 清除 gp 的 paniconfault 标志。
	gp._defer = nil                // 清除 gp 的 _defer 字段。
	gp._panic = nil                // 清除 gp 的 _panic 字段。
	gp.writebuf = nil              // 清除 gp 的 writebuf 字段。
	gp.waitreason = waitReasonZero // 设置 gp 的 waitreason 字段为零值。
	gp.param = nil                 // 清除 gp 的 param 字段。
	gp.labels = nil                // 清除 gp 的 labels 字段。
	gp.timer = nil                 // 清除 gp 的 timer 字段。

	// 如果启用了 GC 黑化，并且 gp 的 gcAssistBytes 大于零，则将协助信用冲销到全局池。
	if gcBlackenEnabled != 0 && gp.gcAssistBytes > 0 {
		assistWorkPerByte := gcController.assistWorkPerByte.Load()
		scanCredit := int64(assistWorkPerByte * float64(gp.gcAssistBytes))
		gcController.bgScanCredit.Add(scanCredit)
		gp.gcAssistBytes = 0
	}

	// 调用 dropg 函数，从当前 m 的 goroutine 列表中删除 gp。
	dropg()

	// 如果架构是 wasm，则没有线程，直接将 gp 放入 p 的自由列表并调度。
	if GOARCH == "wasm" {
		gfput(pp, gp)
		schedule() // 调用goexit0退出, 继续调用调度器
	}

	// 检查 m 是否锁定内部资源。
	if mp.lockedInt != 0 {
		print("invalid m->lockedInt = ", mp.lockedInt, "\n")
		throw("internal lockOSThread error")
	}
	// 将 gp 放入 p 的自由列表。
	gfput(pp, gp)
	// 如果 gp 锁定了线程，则需要特殊处理。
	if locked {
		if GOOS != "plan9" {
			gogo(&mp.g0.sched) // 如果不是 Plan9 系统，返回到 mstart，释放 P 并退出线程。
		} else {
			// 对于 Plan9，清除 lockedExt 标志，可能重新使用此线程。
			mp.lockedExt = 0
		}
	}
	schedule() // 调用goexit0退出, 继续调用调度器
}

// save updates getg().sched to refer to pc and sp so that a following
// gogo will restore pc and sp.
//
// save must not have write barriers because invoking a write barrier
// can clobber getg().sched.
//
//go:nosplit
//go:nowritebarrierrec
func save(pc, sp uintptr) {
	gp := getg()

	if gp == gp.m.g0 || gp == gp.m.gsignal {
		// m.g0.sched is special and must describe the context
		// for exiting the thread. mstart1 writes to it directly.
		// m.gsignal.sched should not be used at all.
		// This check makes sure save calls do not accidentally
		// run in contexts where they'd write to system g's.
		throw("save on system g not allowed")
	}

	gp.sched.pc = pc
	gp.sched.sp = sp
	gp.sched.lr = 0
	gp.sched.ret = 0
	// We need to ensure ctxt is zero, but can't have a write
	// barrier here. However, it should always already be zero.
	// Assert that.
	if gp.sched.ctxt != nil {
		badctxt()
	}
}

// The goroutine g is about to enter a system call.
// Record that it's not using the cpu anymore.
// This is called only from the go syscall library and cgocall,
// not from the low-level system calls used by the runtime.
//
// Entersyscall cannot split the stack: the save must
// make g->sched refer to the caller's stack segment, because
// entersyscall is going to return immediately after.
//
// Nothing entersyscall calls can split the stack either.
// We cannot safely move the stack during an active call to syscall,
// because we do not know which of the uintptr arguments are
// really pointers (back into the stack).
// In practice, this means that we make the fast path run through
// entersyscall doing no-split things, and the slow path has to use systemstack
// to run bigger things on the system stack.
//
// reentersyscall is the entry point used by cgo callbacks, where explicitly
// saved SP and PC are restored. This is needed when exitsyscall will be called
// from a function further up in the call stack than the parent, as g->syscallsp
// must always point to a valid stack frame. entersyscall below is the normal
// entry point for syscalls, which obtains the SP and PC from the caller.
//
// Syscall tracing:
// At the start of a syscall we emit traceGoSysCall to capture the stack trace.
// If the syscall does not block, that is it, we do not emit any other events.
// If the syscall blocks (that is, P is retaken), retaker emits traceGoSysBlock;
// when syscall returns we emit traceGoSysExit and when the goroutine starts running
// (potentially instantly, if exitsyscallfast returns true) we emit traceGoStart.
// To ensure that traceGoSysExit is emitted strictly after traceGoSysBlock,
// we remember current value of syscalltick in m (gp.m.syscalltick = gp.m.p.ptr().syscalltick),
// whoever emits traceGoSysBlock increments p.syscalltick afterwards;
// and we wait for the increment before emitting traceGoSysExit.
// Note that the increment is done even if tracing is not enabled,
// because tracing can be enabled in the middle of syscall. We don't want the wait to hang.
//
//go:nosplit
func reentersyscall(pc, sp uintptr) {
	gp := getg()

	// Disable preemption because during this function g is in Gsyscall status,
	// but can have inconsistent g->sched, do not let GC observe it.
	gp.m.locks++

	// Entersyscall must not call any function that might split/grow the stack.
	// (See details in comment above.)
	// Catch calls that might, by replacing the stack guard with something that
	// will trip any stack check and leaving a flag to tell newstack to die.
	gp.stackguard0 = stackPreempt
	gp.throwsplit = true

	// Leave SP around for GC and traceback.
	save(pc, sp)
	gp.syscallsp = sp
	gp.syscallpc = pc
	casgstatus(gp, _Grunning, _Gsyscall)
	if staticLockRanking {
		// When doing static lock ranking casgstatus can call
		// systemstack which clobbers g.sched.
		save(pc, sp)
	}
	if gp.syscallsp < gp.stack.lo || gp.stack.hi < gp.syscallsp {
		systemstack(func() {
			print("entersyscall inconsistent ", hex(gp.syscallsp), " [", hex(gp.stack.lo), ",", hex(gp.stack.hi), "]\n")
			throw("entersyscall")
		})
	}

	if traceEnabled() {
		systemstack(traceGoSysCall)
		// systemstack itself clobbers g.sched.{pc,sp} and we might
		// need them later when the G is genuinely blocked in a
		// syscall
		save(pc, sp)
	}

	if sched.sysmonwait.Load() {
		systemstack(entersyscall_sysmon)
		save(pc, sp)
	}

	if gp.m.p.ptr().runSafePointFn != 0 {
		// runSafePointFn may stack split if run on this stack
		systemstack(runSafePointFn)
		save(pc, sp)
	}

	gp.m.syscalltick = gp.m.p.ptr().syscalltick
	pp := gp.m.p.ptr()
	pp.m = 0
	gp.m.oldp.set(pp)
	gp.m.p = 0
	atomic.Store(&pp.status, _Psyscall)
	if sched.gcwaiting.Load() {
		systemstack(entersyscall_gcwait)
		save(pc, sp)
	}

	gp.m.locks--
}

// Standard syscall entry used by the go syscall library and normal cgo calls.
//
// This is exported via linkname to assembly in the syscall package and x/sys.
//
//go:nosplit
//go:linkname entersyscall
func entersyscall() {
	reentersyscall(getcallerpc(), getcallersp())
}

func entersyscall_sysmon() {
	lock(&sched.lock)
	if sched.sysmonwait.Load() {
		sched.sysmonwait.Store(false)
		notewakeup(&sched.sysmonnote)
	}
	unlock(&sched.lock)
}

func entersyscall_gcwait() {
	gp := getg()
	pp := gp.m.oldp.ptr()

	lock(&sched.lock)
	if sched.stopwait > 0 && atomic.Cas(&pp.status, _Psyscall, _Pgcstop) {
		if traceEnabled() {
			traceGoSysBlock(pp)
			traceProcStop(pp)
		}
		pp.syscalltick++
		if sched.stopwait--; sched.stopwait == 0 {
			notewakeup(&sched.stopnote)
		}
	}
	unlock(&sched.lock)
}

// The same as entersyscall(), but with a hint that the syscall is blocking.
//
//go:nosplit
func entersyscallblock() {
	gp := getg()

	gp.m.locks++ // see comment in entersyscall
	gp.throwsplit = true
	gp.stackguard0 = stackPreempt // see comment in entersyscall
	gp.m.syscalltick = gp.m.p.ptr().syscalltick
	gp.m.p.ptr().syscalltick++

	// Leave SP around for GC and traceback.
	pc := getcallerpc()
	sp := getcallersp()
	save(pc, sp)
	gp.syscallsp = gp.sched.sp
	gp.syscallpc = gp.sched.pc
	if gp.syscallsp < gp.stack.lo || gp.stack.hi < gp.syscallsp {
		sp1 := sp
		sp2 := gp.sched.sp
		sp3 := gp.syscallsp
		systemstack(func() {
			print("entersyscallblock inconsistent ", hex(sp1), " ", hex(sp2), " ", hex(sp3), " [", hex(gp.stack.lo), ",", hex(gp.stack.hi), "]\n")
			throw("entersyscallblock")
		})
	}
	casgstatus(gp, _Grunning, _Gsyscall)
	if gp.syscallsp < gp.stack.lo || gp.stack.hi < gp.syscallsp {
		systemstack(func() {
			print("entersyscallblock inconsistent ", hex(sp), " ", hex(gp.sched.sp), " ", hex(gp.syscallsp), " [", hex(gp.stack.lo), ",", hex(gp.stack.hi), "]\n")
			throw("entersyscallblock")
		})
	}

	systemstack(entersyscallblock_handoff)

	// Resave for traceback during blocked call.
	save(getcallerpc(), getcallersp())

	gp.m.locks--
}

func entersyscallblock_handoff() {
	if traceEnabled() {
		traceGoSysCall()
		traceGoSysBlock(getg().m.p.ptr())
	}
	handoffp(releasep())
}

// The goroutine g exited its system call.
// Arrange for it to run on a cpu again.
// This is called only from the go syscall library, not
// from the low-level system calls used by the runtime.
//
// Write barriers are not allowed because our P may have been stolen.
//
// This is exported via linkname to assembly in the syscall package.
//
//go:nosplit
//go:nowritebarrierrec
//go:linkname exitsyscall
func exitsyscall() {
	gp := getg()

	gp.m.locks++ // see comment in entersyscall
	if getcallersp() > gp.syscallsp {
		throw("exitsyscall: syscall frame is no longer valid")
	}

	gp.waitsince = 0
	oldp := gp.m.oldp.ptr()
	gp.m.oldp = 0
	if exitsyscallfast(oldp) {
		// When exitsyscallfast returns success, we have a P so can now use
		// write barriers
		if goroutineProfile.active {
			// Make sure that gp has had its stack written out to the goroutine
			// profile, exactly as it was when the goroutine profiler first
			// stopped the world.
			systemstack(func() {
				tryRecordGoroutineProfileWB(gp)
			})
		}
		if traceEnabled() {
			if oldp != gp.m.p.ptr() || gp.m.syscalltick != gp.m.p.ptr().syscalltick {
				systemstack(traceGoStart)
			}
		}
		// There's a cpu for us, so we can run.
		gp.m.p.ptr().syscalltick++
		// We need to cas the status and scan before resuming...
		casgstatus(gp, _Gsyscall, _Grunning)

		// Garbage collector isn't running (since we are),
		// so okay to clear syscallsp.
		gp.syscallsp = 0
		gp.m.locks--
		if gp.preempt {
			// restore the preemption request in case we've cleared it in newstack
			gp.stackguard0 = stackPreempt
		} else {
			// otherwise restore the real stackGuard, we've spoiled it in entersyscall/entersyscallblock
			gp.stackguard0 = gp.stack.lo + stackGuard
		}
		gp.throwsplit = false

		if sched.disable.user && !schedEnabled(gp) {
			// Scheduling of this goroutine is disabled.
			Gosched()
		}

		return
	}

	if traceEnabled() {
		// Wait till traceGoSysBlock event is emitted.
		// This ensures consistency of the trace (the goroutine is started after it is blocked).
		for oldp != nil && oldp.syscalltick == gp.m.syscalltick {
			osyield()
		}
		// We can't trace syscall exit right now because we don't have a P.
		// Tracing code can invoke write barriers that cannot run without a P.
		// So instead we remember the syscall exit time and emit the event
		// in execute when we have a P.
		gp.trace.sysExitTime = traceClockNow()
	}

	gp.m.locks--

	// Call the scheduler.
	mcall(exitsyscall0)

	// Scheduler returned, so we're allowed to run now.
	// Delete the syscallsp information that we left for
	// the garbage collector during the system call.
	// Must wait until now because until gosched returns
	// we don't know for sure that the garbage collector
	// is not running.
	gp.syscallsp = 0
	gp.m.p.ptr().syscalltick++
	gp.throwsplit = false
}

//go:nosplit
func exitsyscallfast(oldp *p) bool {
	gp := getg()

	// Freezetheworld sets stopwait but does not retake P's.
	if sched.stopwait == freezeStopWait {
		return false
	}

	// Try to re-acquire the last P.
	if oldp != nil && oldp.status == _Psyscall && atomic.Cas(&oldp.status, _Psyscall, _Pidle) {
		// There's a cpu for us, so we can run.
		wirep(oldp)
		exitsyscallfast_reacquired()
		return true
	}

	// Try to get any other idle P.
	if sched.pidle != 0 {
		var ok bool
		systemstack(func() {
			ok = exitsyscallfast_pidle()
			if ok && traceEnabled() {
				if oldp != nil {
					// Wait till traceGoSysBlock event is emitted.
					// This ensures consistency of the trace (the goroutine is started after it is blocked).
					for oldp.syscalltick == gp.m.syscalltick {
						osyield()
					}
				}
				traceGoSysExit()
			}
		})
		if ok {
			return true
		}
	}
	return false
}

// exitsyscallfast_reacquired is the exitsyscall path on which this G
// has successfully reacquired the P it was running on before the
// syscall.
//
//go:nosplit
func exitsyscallfast_reacquired() {
	gp := getg()
	if gp.m.syscalltick != gp.m.p.ptr().syscalltick {
		if traceEnabled() {
			// The p was retaken and then enter into syscall again (since gp.m.syscalltick has changed).
			// traceGoSysBlock for this syscall was already emitted,
			// but here we effectively retake the p from the new syscall running on the same p.
			systemstack(func() {
				// Denote blocking of the new syscall.
				traceGoSysBlock(gp.m.p.ptr())
				// Denote completion of the current syscall.
				traceGoSysExit()
			})
		}
		gp.m.p.ptr().syscalltick++
	}
}

func exitsyscallfast_pidle() bool {
	lock(&sched.lock)
	pp, _ := pidleget(0)
	if pp != nil && sched.sysmonwait.Load() {
		sched.sysmonwait.Store(false)
		notewakeup(&sched.sysmonnote)
	}
	unlock(&sched.lock)
	if pp != nil {
		acquirep(pp)
		return true
	}
	return false
}

// exitsyscall slow path on g0.
// Failed to acquire P, enqueue gp as runnable.
//
// Called via mcall, so gp is the calling g from this M.
//
//go:nowritebarrierrec
func exitsyscall0(gp *g) {
	casgstatus(gp, _Gsyscall, _Grunnable)
	dropg()
	lock(&sched.lock)
	var pp *p
	if schedEnabled(gp) {
		pp, _ = pidleget(0)
	}
	var locked bool
	if pp == nil {
		globrunqput(gp)

		// Below, we stoplockedm if gp is locked. globrunqput releases
		// ownership of gp, so we must check if gp is locked prior to
		// committing the release by unlocking sched.lock, otherwise we
		// could race with another M transitioning gp from unlocked to
		// locked.
		locked = gp.lockedm != 0
	} else if sched.sysmonwait.Load() {
		sched.sysmonwait.Store(false)
		notewakeup(&sched.sysmonnote)
	}
	unlock(&sched.lock)
	if pp != nil {
		acquirep(pp)
		execute(gp, false) // Never returns.
	}
	if locked {
		// Wait until another thread schedules gp and so m again.
		//
		// N.B. lockedm must be this M, as this g was running on this M
		// before entersyscall.
		stoplockedm()
		execute(gp, false) // Never returns.
	}
	stopm()
	schedule() // Never returns.
}

// Called from syscall package before fork.
//
//go:linkname syscall_runtime_BeforeFork syscall.runtime_BeforeFork
//go:nosplit
func syscall_runtime_BeforeFork() {
	gp := getg().m.curg

	// Block signals during a fork, so that the child does not run
	// a signal handler before exec if a signal is sent to the process
	// group. See issue #18600.
	gp.m.locks++
	sigsave(&gp.m.sigmask)
	sigblock(false)

	// This function is called before fork in syscall package.
	// Code between fork and exec must not allocate memory nor even try to grow stack.
	// Here we spoil g.stackguard0 to reliably detect any attempts to grow stack.
	// runtime_AfterFork will undo this in parent process, but not in child.
	gp.stackguard0 = stackFork
}

// Called from syscall package after fork in parent.
//
//go:linkname syscall_runtime_AfterFork syscall.runtime_AfterFork
//go:nosplit
func syscall_runtime_AfterFork() {
	gp := getg().m.curg

	// See the comments in beforefork.
	gp.stackguard0 = gp.stack.lo + stackGuard

	msigrestore(gp.m.sigmask)

	gp.m.locks--
}

// inForkedChild is true while manipulating signals in the child process.
// This is used to avoid calling libc functions in case we are using vfork.
var inForkedChild bool

// Called from syscall package after fork in child.
// It resets non-sigignored signals to the default handler, and
// restores the signal mask in preparation for the exec.
//
// Because this might be called during a vfork, and therefore may be
// temporarily sharing address space with the parent process, this must
// not change any global variables or calling into C code that may do so.
//
//go:linkname syscall_runtime_AfterForkInChild syscall.runtime_AfterForkInChild
//go:nosplit
//go:nowritebarrierrec
func syscall_runtime_AfterForkInChild() {
	// It's OK to change the global variable inForkedChild here
	// because we are going to change it back. There is no race here,
	// because if we are sharing address space with the parent process,
	// then the parent process can not be running concurrently.
	inForkedChild = true

	clearSignalHandlers()

	// When we are the child we are the only thread running,
	// so we know that nothing else has changed gp.m.sigmask.
	msigrestore(getg().m.sigmask)

	inForkedChild = false
}

// pendingPreemptSignals is the number of preemption signals
// that have been sent but not received. This is only used on Darwin.
// For #41702.
var pendingPreemptSignals atomic.Int32

// Called from syscall package before Exec.
//
//go:linkname syscall_runtime_BeforeExec syscall.runtime_BeforeExec
func syscall_runtime_BeforeExec() {
	// Prevent thread creation during exec.
	execLock.lock()

	// On Darwin, wait for all pending preemption signals to
	// be received. See issue #41702.
	if GOOS == "darwin" || GOOS == "ios" {
		for pendingPreemptSignals.Load() > 0 {
			osyield()
		}
	}
}

// Called from syscall package after Exec.
//
//go:linkname syscall_runtime_AfterExec syscall.runtime_AfterExec
func syscall_runtime_AfterExec() {
	execLock.unlock()
}

// Allocate a new g, with a stack big enough for stacksize bytes.
func malg(stacksize int32) *g {
	newg := new(g)
	if stacksize >= 0 {
		stacksize = round2(stackSystem + stacksize)
		systemstack(func() {
			newg.stack = stackalloc(uint32(stacksize))
		})
		newg.stackguard0 = newg.stack.lo + stackGuard
		newg.stackguard1 = ^uintptr(0)
		// Clear the bottom word of the stack. We record g
		// there on gsignal stack during VDSO on ARM and ARM64.
		*(*uintptr)(unsafe.Pointer(newg.stack.lo)) = 0
	}
	return newg
}

// 创建一个新的 goroutine 来运行 fn 函数。
// 将这个 goroutine 放入等待运行的 goroutine 队列中。
// 编译器将 go 语句转换为对这个函数的调用。
func newproc(fn *funcval) {
	// 获取当前正在运行的 goroutine。
	gp := getg()
	// 获取调用者（即创建新的 goroutine 的位置）的程序计数器（PC），用于后续的调用者信息记录。
	pc := getcallerpc()

	// 函数在一个更大的系统栈上执行接下来的操作。
	// 这是因为创建 goroutine 可能涉及大量的栈空间消耗，尤其是当递归创建 goroutine 时。
	systemstack(func() {
		// 实际创建新的 goroutine，传入要运行的函数 fn、当前 goroutine gp 和调用者 PC
		newg := newproc1(fn, gp, pc)

		//  获取当前正在运行的 goroutine 所关联的处理器（P），这是为了将新创建的 goroutine 放入正确的运行队列中。
		pp := getg().m.p.ptr()

		// 将新创建的 goroutine 放入 P 的运行队列中，准备运行。
		runqput(pp, newg, true)

		// 如果 main 函数已经开始运行，调用 wakep 函数来唤醒一个等待的处理器（P）
		// 这有助于确保新创建的 goroutine 能够尽快获得执行的机会。
		if mainStarted {
			wakep()
		}
	})
}

// 创建一个新的 goroutine，状态为 _Grunnable，从 fn 函数开始执行。
// (表示此 goroutine 在运行队列上。它当前没有执行用户代码。栈没有所有权)
//
// callerpc 是创建这个 goroutine 的 go 语句的地址。
// 调用者负责将新创建的 goroutine 添加到调度器中。
func newproc1(fn *funcval, callergp *g, callerpc uintptr) *g {
	// 如果 fn 为 nil，致命错误
	if fn == nil {
		fatal("go of nil func value")
	}

	// 获取一个 M，禁用抢占，因为我们持有 M 和 P 的局部变量
	mp := acquirem()
	// 获取当前 M 所关联的 P
	pp := mp.p.ptr()

	// 尝试从 P 的空闲 goroutine 队列中获取一个 goroutine，如果没有，则分配一个新的 goroutine。

	// 尝试从 P 的空闲 goroutine 队列中获取一个 goroutine
	newg := gfget(pp)
	// 如果没有找到空闲的 goroutine
	if newg == nil {
		// 分配一个新的 goroutine, stackMin=2048, 创建一个goroutine最小为2k
		newg = malg(stackMin)
		// 将状态从 _Gidle=尚未初始化 变更为 _Gdead=目前未使用
		casgstatus(newg, _Gidle, _Gdead)
		// 将 newg 添加到所有 goroutine 的列表中
		allgadd(newg)
	}

	// 如果 newg 没有栈，抛出错误
	if newg.stack.hi == 0 {
		throw("newproc1: newg missing stack")
	}
	// 如果 newg 的状态不是 _Gdead=目前未使用，抛出错误
	if readgstatus(newg) != _Gdead {
		throw("newproc1: new g is not Gdead")
	}

	// 计算额外的空间大小
	totalSize := uintptr(4*goarch.PtrSize + sys.MinFrameSize)
	// 栈对齐
	totalSize = alignUp(totalSize, sys.StackAlign)
	// 计算栈指针位置
	sp := newg.stack.hi - totalSize
	spArg := sp
	if usesLR {
		// 设置 caller's LR
		*(*uintptr)(unsafe.Pointer(sp)) = 0
		prepGoExitFrame(sp) // 准备退出帧
		spArg += sys.MinFrameSize
	}

	// 清零调度信息，设置栈指针、PC 寄存器、指向自身 goroutine 的指针等

	// 清零 newg 的调度信息
	memclrNoHeapPointers(unsafe.Pointer(&newg.sched), unsafe.Sizeof(newg.sched))
	// 设置栈指针
	newg.sched.sp = sp
	// 设置栈顶指针
	newg.stktopsp = sp
	// 设置 PC 寄存器
	newg.sched.pc = abi.FuncPCABI0(goexit) + sys.PCQuantum
	// 设置指向自身 goroutine 的指针
	newg.sched.g = guintptr(unsafe.Pointer(newg))

	// 准备调用 fn 函数, 使其看起来像执行了一次对 fn 的调用，然后在 fn 的第一条指令前停止。
	gostartcallfn(&newg.sched, fn)

	// 设置父 goroutine ID、创建时的 PC、祖先信息、起始 PC 等

	// 设置父 goroutine 的 ID
	newg.parentGoid = callergp.goid
	// 设置创建时的 PC
	newg.gopc = callerpc
	// 保存祖先 goroutine 信息
	newg.ancestors = saveAncestors(callergp)
	// 设置起始 PC
	newg.startpc = fn.fn

	// 根据 goroutine 类型更新计数器，继承 pprof 标签，设置不需要被剖析的状态
	if isSystemGoroutine(newg, false) {
		sched.ngsys.Add(1)
	} else {
		if mp.curg != nil {
			newg.labels = mp.curg.labels // 只有用户goroutines继承ppprof标签。
		}
		if goroutineProfile.active {
			// 设置 goroutine 不需要被剖析
			newg.goroutineProfiled.Store(goroutineProfileSatisfied)
		}
	}

	// 跟踪初始状态？
	newg.trackingSeq = uint8(fastrand())
	if newg.trackingSeq%gTrackingPeriod == 0 {
		newg.tracking = true
	}

	// 使用 CAS 设置 goroutine 的状态为 _Grunnable。
	// 设置newg状态为_Grunnable, 到这里 newg 就可以运行了
	casgstatus(newg, _Gdead, _Grunnable)
	// 更新 GC 控制器的可扫描栈
	gcController.addScannableStack(pp, int64(newg.stack.hi-newg.stack.lo))

	// 从 P 的缓存中分配一个 goroutine ID
	if pp.goidcache == pp.goidcacheend {
		pp.goidcache = sched.goidgen.Add(_GoidCacheBatch)
		pp.goidcache -= _GoidCacheBatch - 1
		pp.goidcacheend = pp.goidcache + _GoidCacheBatch
	}
	newg.goid = pp.goidcache
	pp.goidcache++

	// 如果 race 检测器启用
	if raceenabled {
		newg.racectx = racegostart(callerpc)
		newg.raceignore = 0
		if newg.labels != nil {
			// See note in proflabel.go on labelSync's role in synchronizing
			// with the reads in the signal handler.
			racereleasemergeg(newg, unsafe.Pointer(&labelSync))
		}
	}

	// 如果跟踪启用
	if traceEnabled() {
		traceGoCreate(newg, newg.startpc)
	}
	// 释放 M
	releasem(mp)
	// 返回新创建的 goroutine
	return newg
}

// saveAncestors copies previous ancestors of the given caller g and
// includes info for the current caller into a new set of tracebacks for
// a g being created.
func saveAncestors(callergp *g) *[]ancestorInfo {
	// Copy all prior info, except for the root goroutine (goid 0).
	if debug.tracebackancestors <= 0 || callergp.goid == 0 {
		return nil
	}
	var callerAncestors []ancestorInfo
	if callergp.ancestors != nil {
		callerAncestors = *callergp.ancestors
	}
	n := int32(len(callerAncestors)) + 1
	if n > debug.tracebackancestors {
		n = debug.tracebackancestors
	}
	ancestors := make([]ancestorInfo, n)
	copy(ancestors[1:], callerAncestors)

	var pcs [tracebackInnerFrames]uintptr
	npcs := gcallers(callergp, 0, pcs[:])
	ipcs := make([]uintptr, npcs)
	copy(ipcs, pcs[:])
	ancestors[0] = ancestorInfo{
		pcs:  ipcs,
		goid: callergp.goid,
		gopc: callergp.gopc,
	}

	ancestorsp := new([]ancestorInfo)
	*ancestorsp = ancestors
	return ancestorsp
}

// Put on gfree list.
// If local list is too long, transfer a batch to the global list.
func gfput(pp *p, gp *g) {
	if readgstatus(gp) != _Gdead {
		throw("gfput: bad status (not Gdead)")
	}

	stksize := gp.stack.hi - gp.stack.lo

	if stksize != uintptr(startingStackSize) {
		// non-standard stack size - free it.
		stackfree(gp.stack)
		gp.stack.lo = 0
		gp.stack.hi = 0
		gp.stackguard0 = 0
	}

	pp.gFree.push(gp)
	pp.gFree.n++
	if pp.gFree.n >= 64 {
		var (
			inc      int32
			stackQ   gQueue
			noStackQ gQueue
		)
		for pp.gFree.n >= 32 {
			gp := pp.gFree.pop()
			pp.gFree.n--
			if gp.stack.lo == 0 {
				noStackQ.push(gp)
			} else {
				stackQ.push(gp)
			}
			inc++
		}
		lock(&sched.gFree.lock)
		sched.gFree.noStack.pushAll(noStackQ)
		sched.gFree.stack.pushAll(stackQ)
		sched.gFree.n += inc
		unlock(&sched.gFree.lock)
	}
}

// Get from gfree list.
// If local list is empty, grab a batch from global list.
func gfget(pp *p) *g {
retry:
	if pp.gFree.empty() && (!sched.gFree.stack.empty() || !sched.gFree.noStack.empty()) {
		lock(&sched.gFree.lock)
		// Move a batch of free Gs to the P.
		for pp.gFree.n < 32 {
			// Prefer Gs with stacks.
			gp := sched.gFree.stack.pop()
			if gp == nil {
				gp = sched.gFree.noStack.pop()
				if gp == nil {
					break
				}
			}
			sched.gFree.n--
			pp.gFree.push(gp)
			pp.gFree.n++
		}
		unlock(&sched.gFree.lock)
		goto retry
	}
	gp := pp.gFree.pop()
	if gp == nil {
		return nil
	}
	pp.gFree.n--
	if gp.stack.lo != 0 && gp.stack.hi-gp.stack.lo != uintptr(startingStackSize) {
		// Deallocate old stack. We kept it in gfput because it was the
		// right size when the goroutine was put on the free list, but
		// the right size has changed since then.
		systemstack(func() {
			stackfree(gp.stack)
			gp.stack.lo = 0
			gp.stack.hi = 0
			gp.stackguard0 = 0
		})
	}
	if gp.stack.lo == 0 {
		// Stack was deallocated in gfput or just above. Allocate a new one.
		systemstack(func() {
			gp.stack = stackalloc(startingStackSize)
		})
		gp.stackguard0 = gp.stack.lo + stackGuard
	} else {
		if raceenabled {
			racemalloc(unsafe.Pointer(gp.stack.lo), gp.stack.hi-gp.stack.lo)
		}
		if msanenabled {
			msanmalloc(unsafe.Pointer(gp.stack.lo), gp.stack.hi-gp.stack.lo)
		}
		if asanenabled {
			asanunpoison(unsafe.Pointer(gp.stack.lo), gp.stack.hi-gp.stack.lo)
		}
	}
	return gp
}

// Purge all cached G's from gfree list to the global list.
func gfpurge(pp *p) {
	var (
		inc      int32
		stackQ   gQueue
		noStackQ gQueue
	)
	for !pp.gFree.empty() {
		gp := pp.gFree.pop()
		pp.gFree.n--
		if gp.stack.lo == 0 {
			noStackQ.push(gp)
		} else {
			stackQ.push(gp)
		}
		inc++
	}
	lock(&sched.gFree.lock)
	sched.gFree.noStack.pushAll(noStackQ)
	sched.gFree.stack.pushAll(stackQ)
	sched.gFree.n += inc
	unlock(&sched.gFree.lock)
}

// Breakpoint executes a breakpoint trap.
func Breakpoint() {
	breakpoint()
}

// dolockOSThread is called by LockOSThread and lockOSThread below
// after they modify m.locked. Do not allow preemption during this call,
// or else the m might be different in this function than in the caller.
//
//go:nosplit
func dolockOSThread() {
	if GOARCH == "wasm" {
		return // no threads on wasm yet
	}
	gp := getg()
	gp.m.lockedg.set(gp)
	gp.lockedm.set(gp.m)
}

// LockOSThread wires the calling goroutine to its current operating system thread.
// The calling goroutine will always execute in that thread,
// and no other goroutine will execute in it,
// until the calling goroutine has made as many calls to
// UnlockOSThread as to LockOSThread.
// If the calling goroutine exits without unlocking the thread,
// the thread will be terminated.
//
// All init functions are run on the startup thread. Calling LockOSThread
// from an init function will cause the main function to be invoked on
// that thread.
//
// A goroutine should call LockOSThread before calling OS services or
// non-Go library functions that depend on per-thread state.
//
//go:nosplit
func LockOSThread() {
	if atomic.Load(&newmHandoff.haveTemplateThread) == 0 && GOOS != "plan9" {
		// If we need to start a new thread from the locked
		// thread, we need the template thread. Start it now
		// while we're in a known-good state.
		startTemplateThread()
	}
	gp := getg()
	gp.m.lockedExt++
	if gp.m.lockedExt == 0 {
		gp.m.lockedExt--
		panic("LockOSThread nesting overflow")
	}
	dolockOSThread()
}

//go:nosplit
func lockOSThread() {
	getg().m.lockedInt++
	dolockOSThread()
}

// dounlockOSThread is called by UnlockOSThread and unlockOSThread below
// after they update m->locked. Do not allow preemption during this call,
// or else the m might be in different in this function than in the caller.
//
//go:nosplit
func dounlockOSThread() {
	if GOARCH == "wasm" {
		return // no threads on wasm yet
	}
	gp := getg()
	if gp.m.lockedInt != 0 || gp.m.lockedExt != 0 {
		return
	}
	gp.m.lockedg = 0
	gp.lockedm = 0
}

// UnlockOSThread undoes an earlier call to LockOSThread.
// If this drops the number of active LockOSThread calls on the
// calling goroutine to zero, it unwires the calling goroutine from
// its fixed operating system thread.
// If there are no active LockOSThread calls, this is a no-op.
//
// Before calling UnlockOSThread, the caller must ensure that the OS
// thread is suitable for running other goroutines. If the caller made
// any permanent changes to the state of the thread that would affect
// other goroutines, it should not call this function and thus leave
// the goroutine locked to the OS thread until the goroutine (and
// hence the thread) exits.
//
//go:nosplit
func UnlockOSThread() {
	gp := getg()
	if gp.m.lockedExt == 0 {
		return
	}
	gp.m.lockedExt--
	dounlockOSThread()
}

//go:nosplit
func unlockOSThread() {
	gp := getg()
	if gp.m.lockedInt == 0 {
		systemstack(badunlockosthread)
	}
	gp.m.lockedInt--
	dounlockOSThread()
}

func badunlockosthread() {
	throw("runtime: internal error: misuse of lockOSThread/unlockOSThread")
}

func gcount() int32 {
	n := int32(atomic.Loaduintptr(&allglen)) - sched.gFree.n - sched.ngsys.Load()
	for _, pp := range allp {
		n -= pp.gFree.n
	}

	// All these variables can be changed concurrently, so the result can be inconsistent.
	// But at least the current goroutine is running.
	if n < 1 {
		n = 1
	}
	return n
}

func mcount() int32 {
	return int32(sched.mnext - sched.nmfreed)
}

var prof struct {
	signalLock atomic.Uint32

	// Must hold signalLock to write. Reads may be lock-free, but
	// signalLock should be taken to synchronize with changes.
	hz atomic.Int32
}

func _System()                    { _System() }
func _ExternalCode()              { _ExternalCode() }
func _LostExternalCode()          { _LostExternalCode() }
func _GC()                        { _GC() }
func _LostSIGPROFDuringAtomic64() { _LostSIGPROFDuringAtomic64() }
func _VDSO()                      { _VDSO() }

// Called if we receive a SIGPROF signal.
// Called by the signal handler, may run during STW.
//
//go:nowritebarrierrec
func sigprof(pc, sp, lr uintptr, gp *g, mp *m) {
	if prof.hz.Load() == 0 {
		return
	}

	// If mp.profilehz is 0, then profiling is not enabled for this thread.
	// We must check this to avoid a deadlock between setcpuprofilerate
	// and the call to cpuprof.add, below.
	if mp != nil && mp.profilehz == 0 {
		return
	}

	// On mips{,le}/arm, 64bit atomics are emulated with spinlocks, in
	// runtime/internal/atomic. If SIGPROF arrives while the program is inside
	// the critical section, it creates a deadlock (when writing the sample).
	// As a workaround, create a counter of SIGPROFs while in critical section
	// to store the count, and pass it to sigprof.add() later when SIGPROF is
	// received from somewhere else (with _LostSIGPROFDuringAtomic64 as pc).
	if GOARCH == "mips" || GOARCH == "mipsle" || GOARCH == "arm" {
		if f := findfunc(pc); f.valid() {
			if hasPrefix(funcname(f), "runtime/internal/atomic") {
				cpuprof.lostAtomic++
				return
			}
		}
		if GOARCH == "arm" && goarm < 7 && GOOS == "linux" && pc&0xffff0000 == 0xffff0000 {
			// runtime/internal/atomic functions call into kernel
			// helpers on arm < 7. See
			// runtime/internal/atomic/sys_linux_arm.s.
			cpuprof.lostAtomic++
			return
		}
	}

	// Profiling runs concurrently with GC, so it must not allocate.
	// Set a trap in case the code does allocate.
	// Note that on windows, one thread takes profiles of all the
	// other threads, so mp is usually not getg().m.
	// In fact mp may not even be stopped.
	// See golang.org/issue/17165.
	getg().m.mallocing++

	var u unwinder
	var stk [maxCPUProfStack]uintptr
	n := 0
	if mp.ncgo > 0 && mp.curg != nil && mp.curg.syscallpc != 0 && mp.curg.syscallsp != 0 {
		cgoOff := 0
		// Check cgoCallersUse to make sure that we are not
		// interrupting other code that is fiddling with
		// cgoCallers.  We are running in a signal handler
		// with all signals blocked, so we don't have to worry
		// about any other code interrupting us.
		if mp.cgoCallersUse.Load() == 0 && mp.cgoCallers != nil && mp.cgoCallers[0] != 0 {
			for cgoOff < len(mp.cgoCallers) && mp.cgoCallers[cgoOff] != 0 {
				cgoOff++
			}
			n += copy(stk[:], mp.cgoCallers[:cgoOff])
			mp.cgoCallers[0] = 0
		}

		// Collect Go stack that leads to the cgo call.
		u.initAt(mp.curg.syscallpc, mp.curg.syscallsp, 0, mp.curg, unwindSilentErrors)
	} else if usesLibcall() && mp.libcallg != 0 && mp.libcallpc != 0 && mp.libcallsp != 0 {
		// Libcall, i.e. runtime syscall on windows.
		// Collect Go stack that leads to the call.
		u.initAt(mp.libcallpc, mp.libcallsp, 0, mp.libcallg.ptr(), unwindSilentErrors)
	} else if mp != nil && mp.vdsoSP != 0 {
		// VDSO call, e.g. nanotime1 on Linux.
		// Collect Go stack that leads to the call.
		u.initAt(mp.vdsoPC, mp.vdsoSP, 0, gp, unwindSilentErrors|unwindJumpStack)
	} else {
		u.initAt(pc, sp, lr, gp, unwindSilentErrors|unwindTrap|unwindJumpStack)
	}
	n += tracebackPCs(&u, 0, stk[n:])

	if n <= 0 {
		// Normal traceback is impossible or has failed.
		// Account it against abstract "System" or "GC".
		n = 2
		if inVDSOPage(pc) {
			pc = abi.FuncPCABIInternal(_VDSO) + sys.PCQuantum
		} else if pc > firstmoduledata.etext {
			// "ExternalCode" is better than "etext".
			pc = abi.FuncPCABIInternal(_ExternalCode) + sys.PCQuantum
		}
		stk[0] = pc
		if mp.preemptoff != "" {
			stk[1] = abi.FuncPCABIInternal(_GC) + sys.PCQuantum
		} else {
			stk[1] = abi.FuncPCABIInternal(_System) + sys.PCQuantum
		}
	}

	if prof.hz.Load() != 0 {
		// Note: it can happen on Windows that we interrupted a system thread
		// with no g, so gp could nil. The other nil checks are done out of
		// caution, but not expected to be nil in practice.
		var tagPtr *unsafe.Pointer
		if gp != nil && gp.m != nil && gp.m.curg != nil {
			tagPtr = &gp.m.curg.labels
		}
		cpuprof.add(tagPtr, stk[:n])

		gprof := gp
		var pp *p
		if gp != nil && gp.m != nil {
			if gp.m.curg != nil {
				gprof = gp.m.curg
			}
			pp = gp.m.p.ptr()
		}
		traceCPUSample(gprof, pp, stk[:n])
	}
	getg().m.mallocing--
}

// setcpuprofilerate sets the CPU profiling rate to hz times per second.
// If hz <= 0, setcpuprofilerate turns off CPU profiling.
func setcpuprofilerate(hz int32) {
	// Force sane arguments.
	if hz < 0 {
		hz = 0
	}

	// Disable preemption, otherwise we can be rescheduled to another thread
	// that has profiling enabled.
	gp := getg()
	gp.m.locks++

	// Stop profiler on this thread so that it is safe to lock prof.
	// if a profiling signal came in while we had prof locked,
	// it would deadlock.
	setThreadCPUProfiler(0)

	for !prof.signalLock.CompareAndSwap(0, 1) {
		osyield()
	}
	if prof.hz.Load() != hz {
		setProcessCPUProfiler(hz)
		prof.hz.Store(hz)
	}
	prof.signalLock.Store(0)

	lock(&sched.lock)
	sched.profilehz = hz
	unlock(&sched.lock)

	if hz != 0 {
		setThreadCPUProfiler(hz)
	}

	gp.m.locks--
}

// init initializes pp, which may be a freshly allocated p or a
// previously destroyed p, and transitions it to status _Pgcstop.
func (pp *p) init(id int32) {
	pp.id = id
	pp.status = _Pgcstop
	pp.sudogcache = pp.sudogbuf[:0]
	pp.deferpool = pp.deferpoolbuf[:0]
	pp.wbBuf.reset()
	if pp.mcache == nil {
		if id == 0 {
			if mcache0 == nil {
				throw("missing mcache?")
			}
			// Use the bootstrap mcache0. Only one P will get
			// mcache0: the one with ID 0.
			pp.mcache = mcache0
		} else {
			pp.mcache = allocmcache()
		}
	}
	if raceenabled && pp.raceprocctx == 0 {
		if id == 0 {
			pp.raceprocctx = raceprocctx0
			raceprocctx0 = 0 // bootstrap
		} else {
			pp.raceprocctx = raceproccreate()
		}
	}
	lockInit(&pp.timersLock, lockRankTimers)

	// This P may get timers when it starts running. Set the mask here
	// since the P may not go through pidleget (notably P 0 on startup).
	timerpMask.set(id)
	// Similarly, we may not go through pidleget before this P starts
	// running if it is P 0 on startup.
	idlepMask.clear(id)
}

// destroy releases all of the resources associated with pp and
// transitions it to status _Pdead.
//
// sched.lock must be held and the world must be stopped.
func (pp *p) destroy() {
	assertLockHeld(&sched.lock)
	assertWorldStopped()

	// Move all runnable goroutines to the global queue
	for pp.runqhead != pp.runqtail {
		// Pop from tail of local queue
		pp.runqtail--
		gp := pp.runq[pp.runqtail%uint32(len(pp.runq))].ptr()
		// Push onto head of global queue
		globrunqputhead(gp)
	}
	if pp.runnext != 0 {
		globrunqputhead(pp.runnext.ptr())
		pp.runnext = 0
	}
	if len(pp.timers) > 0 {
		plocal := getg().m.p.ptr()
		// The world is stopped, but we acquire timersLock to
		// protect against sysmon calling timeSleepUntil.
		// This is the only case where we hold the timersLock of
		// more than one P, so there are no deadlock concerns.
		lock(&plocal.timersLock)
		lock(&pp.timersLock)
		moveTimers(plocal, pp.timers)
		pp.timers = nil
		pp.numTimers.Store(0)
		pp.deletedTimers.Store(0)
		pp.timer0When.Store(0)
		unlock(&pp.timersLock)
		unlock(&plocal.timersLock)
	}
	// Flush p's write barrier buffer.
	if gcphase != _GCoff {
		wbBufFlush1(pp)
		pp.gcw.dispose()
	}
	for i := range pp.sudogbuf {
		pp.sudogbuf[i] = nil
	}
	pp.sudogcache = pp.sudogbuf[:0]
	pp.pinnerCache = nil
	for j := range pp.deferpoolbuf {
		pp.deferpoolbuf[j] = nil
	}
	pp.deferpool = pp.deferpoolbuf[:0]
	systemstack(func() {
		for i := 0; i < pp.mspancache.len; i++ {
			// Safe to call since the world is stopped.
			mheap_.spanalloc.free(unsafe.Pointer(pp.mspancache.buf[i]))
		}
		pp.mspancache.len = 0
		lock(&mheap_.lock)
		pp.pcache.flush(&mheap_.pages)
		unlock(&mheap_.lock)
	})
	freemcache(pp.mcache)
	pp.mcache = nil
	gfpurge(pp)
	traceProcFree(pp)
	if raceenabled {
		if pp.timerRaceCtx != 0 {
			// The race detector code uses a callback to fetch
			// the proc context, so arrange for that callback
			// to see the right thing.
			// This hack only works because we are the only
			// thread running.
			mp := getg().m
			phold := mp.p.ptr()
			mp.p.set(pp)

			racectxend(pp.timerRaceCtx)
			pp.timerRaceCtx = 0

			mp.p.set(phold)
		}
		raceprocdestroy(pp.raceprocctx)
		pp.raceprocctx = 0
	}
	pp.gcAssistTime = 0
	pp.status = _Pdead
}

// 调整处理器数量。
//
// 必须持有 sched.lock 锁，并且世界必须停止。
//
// gcworkbufs 必须不会被 GC 或写屏障代码修改，
// 因此如果处理器数量实际上发生变化，GC 不应运行。
//
// 返回包含本地工作列表的 P 的列表，需要由调用者调度。
func procresize(nprocs int32) *p {
	assertLockHeld(&sched.lock) // 断言锁被持有
	assertWorldStopped()        // 断言世界已停止

	// 获取旧的处理器数量
	old := gomaxprocs
	// 如果参数无效，抛出错误
	if old < 0 || nprocs <= 0 {
		throw("procresize: invalid arg")
	}
	// 如果跟踪启用，记录新的处理器数量
	if traceEnabled() {
		traceGomaxprocs(nprocs)
	}

	// 更新统计处理器信息

	// 获取当前时间
	now := nanotime()
	// 计算并更新总时间
	if sched.procresizetime != 0 {
		sched.totaltime += int64(old) * (now - sched.procresizetime)
	}
	// 更新上一次调整时间
	sched.procresizetime = now

	// 计算掩码单词数量
	maskWords := (nprocs + 31) / 32

	// 如果需要更多处理器，动态扩展 allp、idlepMask 和 timerpMask 列表。
	if nprocs > int32(len(allp)) {
		// 加锁，防止 retake 并发运行
		lock(&allpLock)

		// 切片至新大小
		if nprocs <= int32(cap(allp)) {
			allp = allp[:nprocs]
		} else {
			// 新建切片
			nallp := make([]*p, nprocs)
			// 复制旧元素
			copy(nallp, allp[:cap(allp)])
			// 替换切片
			allp = nallp
		}

		// 扩容 idlepMask 和 timerpMask
		if maskWords <= int32(cap(idlepMask)) {
			idlepMask = idlepMask[:maskWords]
			timerpMask = timerpMask[:maskWords]
		} else {
			nidlepMask := make([]uint32, maskWords)
			// 不需要复制超出len，老Ps是无关紧要的。
			copy(nidlepMask, idlepMask)
			idlepMask = nidlepMask

			ntimerpMask := make([]uint32, maskWords)
			copy(ntimerpMask, timerpMask)
			timerpMask = ntimerpMask
		}

		// 解锁
		unlock(&allpLock)
	}

	// 初始化新增加的处理器 P，设置其状态并存储。
	for i := old; i < nprocs; i++ {
		pp := allp[i]
		if pp == nil {
			// 创建新的 P 结构体
			pp = new(p)
		}

		// 初始化 P
		pp.init(i)
		// 原子存储 P
		atomicstorep(unsafe.Pointer(&allp[i]), unsafe.Pointer(pp))
	}

	// 根据需要，切换当前处理器至空闲状态或获取新的处理器。

	// 获取当前 goroutine
	gp := getg()
	if gp.m.p != 0 && gp.m.p.ptr().id < nprocs {
		// 继续使用当前的 P
		gp.m.p.ptr().status = _Prunning
		gp.m.p.ptr().mcache.prepareForSweep()
	} else {
		// 释放当前 P 并获取 allp[0]
		if gp.m.p != 0 {
			if traceEnabled() {
				// 跟踪当前 P 的状态改变
				traceGoSched()
				traceProcStop(gp.m.p.ptr())
			}
			gp.m.p.ptr().m = 0 // 清除 M 引用
		}

		gp.m.p = 0         // 清除 P 引用
		pp := allp[0]      // 获取第一个 P
		pp.m = 0           // 清除 M 引用
		pp.status = _Pidle // 设置 P 状态为空闲
		acquirep(pp)       // 把allp[0]和m0关联起来

		// 开始跟踪
		if traceEnabled() {
			traceGoStart()
		}
	}

	// g.m.p 现在设置，所以我们不再需要 mcache0 进行引导。
	mcache0 = nil

	// 销毁不再需要的处理器 P 的资源。
	for i := nprocs; i < old; i++ {
		pp := allp[i]
		pp.destroy() // 销毁 P
	}

	// 根据新的处理器数量，裁剪 allp、idlepMask 和 timerpMask 列表。
	if int32(len(allp)) != nprocs {
		lock(&allpLock)
		allp = allp[:nprocs]
		idlepMask = idlepMask[:maskWords]
		timerpMask = timerpMask[:maskWords]
		unlock(&allpLock)
	}

	// 根据 P 的状态将其放入空闲队列或可运行队列。

	// 初始化可运行 P 的指针
	var runnablePs *p
	for i := nprocs - 1; i >= 0; i-- {
		pp := allp[i]
		// 如果是当前 P，跳过
		if gp.m.p.ptr() == pp {
			continue
		}

		// 设置 P 状态为空闲
		pp.status = _Pidle
		if runqempty(pp) {
			// 将 P 放入空闲队列
			pidleput(pp, now)
		} else {
			// 获取 M
			pp.m.set(mget())
			// 链接可运行 P
			pp.link.set(runnablePs)
			// 更新可运行 P 的指针
			runnablePs = pp
		}
	}

	// 原子更新全局处理器数量，并通知 GC 限速器。

	// 重置窃取顺序
	stealOrder.reset(uint32(nprocs))
	// 指向 gomaxprocs 的指针
	var int32p *int32 = &gomaxprocs
	// 原子更新 gomaxprocs
	atomic.Store((*uint32)(unsafe.Pointer(int32p)), uint32(nprocs))
	// 通知限速器处理器数量的变化
	if old != nprocs {
		gcCPULimiter.resetCapacity(now, nprocs)
	}

	// 返回那些具有本地工作队列的处理器，它们需要被调度。
	return runnablePs
}

// Associate p and the current m.
//
// This function is allowed to have write barriers even if the caller
// isn't because it immediately acquires pp.
//
//go:yeswritebarrierrec
func acquirep(pp *p) {
	// Do the part that isn't allowed to have write barriers.
	wirep(pp)

	// Have p; write barriers now allowed.

	// Perform deferred mcache flush before this P can allocate
	// from a potentially stale mcache.
	pp.mcache.prepareForSweep()

	if traceEnabled() {
		traceProcStart()
	}
}

// wirep is the first step of acquirep, which actually associates the
// current M to pp. This is broken out so we can disallow write
// barriers for this part, since we don't yet have a P.
//
//go:nowritebarrierrec
//go:nosplit
func wirep(pp *p) {
	gp := getg()

	if gp.m.p != 0 {
		throw("wirep: already in go")
	}
	if pp.m != 0 || pp.status != _Pidle {
		id := int64(0)
		if pp.m != 0 {
			id = pp.m.ptr().id
		}
		print("wirep: p->m=", pp.m, "(", id, ") p->status=", pp.status, "\n")
		throw("wirep: invalid p state")
	}
	gp.m.p.set(pp)
	pp.m.set(gp.m)
	pp.status = _Prunning
}

// Disassociate p and the current m.
func releasep() *p {
	gp := getg()

	if gp.m.p == 0 {
		throw("releasep: invalid arg")
	}
	pp := gp.m.p.ptr()
	if pp.m.ptr() != gp.m || pp.status != _Prunning {
		print("releasep: m=", gp.m, " m->p=", gp.m.p.ptr(), " p->m=", hex(pp.m), " p->status=", pp.status, "\n")
		throw("releasep: invalid p state")
	}
	if traceEnabled() {
		traceProcStop(gp.m.p.ptr())
	}
	gp.m.p = 0
	pp.m = 0
	pp.status = _Pidle
	return pp
}

func incidlelocked(v int32) {
	lock(&sched.lock)
	sched.nmidlelocked += v
	if v > 0 {
		checkdead()
	}
	unlock(&sched.lock)
}

// Check for deadlock situation.
// The check is based on number of running M's, if 0 -> deadlock.
// sched.lock must be held.
func checkdead() {
	assertLockHeld(&sched.lock)

	// For -buildmode=c-shared or -buildmode=c-archive it's OK if
	// there are no running goroutines. The calling program is
	// assumed to be running.
	if islibrary || isarchive {
		return
	}

	// If we are dying because of a signal caught on an already idle thread,
	// freezetheworld will cause all running threads to block.
	// And runtime will essentially enter into deadlock state,
	// except that there is a thread that will call exit soon.
	if panicking.Load() > 0 {
		return
	}

	// If we are not running under cgo, but we have an extra M then account
	// for it. (It is possible to have an extra M on Windows without cgo to
	// accommodate callbacks created by syscall.NewCallback. See issue #6751
	// for details.)
	var run0 int32
	if !iscgo && cgoHasExtraM && extraMLength.Load() > 0 {
		run0 = 1
	}

	run := mcount() - sched.nmidle - sched.nmidlelocked - sched.nmsys
	if run > run0 {
		return
	}
	if run < 0 {
		print("runtime: checkdead: nmidle=", sched.nmidle, " nmidlelocked=", sched.nmidlelocked, " mcount=", mcount(), " nmsys=", sched.nmsys, "\n")
		unlock(&sched.lock)
		throw("checkdead: inconsistent counts")
	}

	grunning := 0
	forEachG(func(gp *g) {
		if isSystemGoroutine(gp, false) {
			return
		}
		s := readgstatus(gp)
		switch s &^ _Gscan {
		case _Gwaiting,
			_Gpreempted:
			grunning++
		case _Grunnable,
			_Grunning,
			_Gsyscall:
			print("runtime: checkdead: find g ", gp.goid, " in status ", s, "\n")
			unlock(&sched.lock)
			throw("checkdead: runnable g")
		}
	})
	if grunning == 0 { // possible if main goroutine calls runtime·Goexit()
		unlock(&sched.lock) // unlock so that GODEBUG=scheddetail=1 doesn't hang
		fatal("no goroutines (main called runtime.Goexit) - deadlock!")
	}

	// Maybe jump time forward for playground.
	if faketime != 0 {
		if when := timeSleepUntil(); when < maxWhen {
			faketime = when

			// Start an M to steal the timer.
			pp, _ := pidleget(faketime)
			if pp == nil {
				// There should always be a free P since
				// nothing is running.
				unlock(&sched.lock)
				throw("checkdead: no p for timer")
			}
			mp := mget()
			if mp == nil {
				// There should always be a free M since
				// nothing is running.
				unlock(&sched.lock)
				throw("checkdead: no m for timer")
			}
			// M must be spinning to steal. We set this to be
			// explicit, but since this is the only M it would
			// become spinning on its own anyways.
			sched.nmspinning.Add(1)
			mp.spinning = true
			mp.nextp.set(pp)
			notewakeup(&mp.park)
			return
		}
	}

	// There are no goroutines running, so we can look at the P's.
	for _, pp := range allp {
		if len(pp.timers) > 0 {
			return
		}
	}

	unlock(&sched.lock) // unlock so that GODEBUG=scheddetail=1 doesn't hang
	fatal("all goroutines are asleep - deadlock!")
}

// forcegcperiod is the maximum time in nanoseconds between garbage
// collections. If we go this long without a garbage collection, one
// is forced to run.
//
// This is a variable for testing purposes. It normally doesn't change.
var forcegcperiod int64 = 2 * 60 * 1e9

// needSysmonWorkaround is true if the workaround for
// golang.org/issue/42515 is needed on NetBSD.
var needSysmonWorkaround bool = false

// 负责监控和维护整个运行时系统的健康状态;
// 在无需 P 的情况下运行，因此不允许写屏障。
//
//go:nowritebarrierrec
func sysmon() {
	lock(&sched.lock)   // 锁定调度器锁。
	sched.nmsys++       // 增加系统监控线程计数。
	checkdead()         // 检查是否有死掉的 goroutine。
	unlock(&sched.lock) // 解锁调度器锁。

	lasttrace := int64(0) // 上一次记录调度跟踪的时间戳。
	idle := 0             // 记录连续没有唤醒任何 goroutine 的循环次数。
	delay := uint32(0)    // 睡眠延迟，初始为 0。

	// 死循环, 并设置一定的休眠, 避免 cpu 过高
	for {
		// 根据 idle 的值调整延迟。
		if idle == 0 {
			delay = 20 // 初始延迟为 20 微秒。
		} else if idle > 50 {
			delay *= 2 // 如果 idle 大于 50 微秒，延迟翻倍。
		}
		if delay > 10*1000 {
			delay = 10 * 1000 // 如果 idle 大于 10 毫秒，最大延迟为 10 毫秒。
		}

		// 以微秒为单位的睡眠函数，实现功能为线程在指定的微秒时间内进入睡眠状态
		usleep(delay)

		// sysmon 线程不应在启用调度追踪 (schedtrace) 或存在活跃 P 的情况下进入深度睡眠，
		// 这样它可以在适当的时间打印调度追踪信息，重新获取在系统调用中被阻塞的 P，
		// 预先抢占长时间运行的 Goroutine (G)，并在网络繁忙时进行轮询。
		now := nanotime() // 获取当前时间。
		if debug.schedtrace <= 0 && (sched.gcwaiting.Load() || sched.npidle.Load() == gomaxprocs) {
			// 深度睡眠的准备工作。

			// 锁定调度器锁。
			lock(&sched.lock)

			// 再次检查条件是否仍然满足，因为在上锁后条件可能已改变。
			if sched.gcwaiting.Load() || sched.npidle.Load() == gomaxprocs {
				syscallWake := false     // 是否由于系统调用而唤醒。
				next := timeSleepUntil() // 下一次需要唤醒的时间点。

				// 如果下一个唤醒时间点大于当前时间，即还有时间可以深度睡眠。
				if next > now {
					sched.sysmonwait.Store(true) // 标记 sysmon 正在等待。
					unlock(&sched.lock)          // 解锁调度器锁。

					// 计算睡眠时间，确保足够小以保持采样的准确性。
					sleep := forcegcperiod / 2 // 默认睡眠时间为强制 GC 周期的一半。
					if next-now < sleep {
						sleep = next - now // 如果唤醒时间点更近，则使用这个时间差。
					}

					// 检查是否应降低系统负载。
					shouldRelax := sleep >= osRelaxMinNS // 如果睡眠时间足够长，则应降低系统负载。
					if shouldRelax {
						osRelax(true) // 调用 osRelax 减小系统负载。
					}

					// 进行非抢占式的睡眠，等待唤醒事件。
					syscallWake = notetsleep(&sched.sysmonnote, sleep)

					// 如果之前降低了系统负载，则现在恢复。
					if shouldRelax {
						osRelax(false) // 恢复正常系统负载。
					}

					lock(&sched.lock)             // 再次锁定调度器锁。
					sched.sysmonwait.Store(false) // 清除 sysmon 正在等待的标记。
					noteclear(&sched.sysmonnote)  // 清除唤醒通知。
				}

				// 如果由于系统调用而唤醒，则重置 idle 和 delay。
				if syscallWake {
					idle = 0
					delay = 20
				}
			}
			unlock(&sched.lock)
		}

		// 锁定 sysmon 专用锁。
		lock(&sched.sysmonlock)

		// 更新 now，以防在 sysmonnote 或 schedlock/sysmonlock 上阻塞了很长时间。
		now = nanotime()

		// 如果需要，触发 libc 拦截器
		if *cgo_yield != nil {
			asmcgocall(*cgo_yield, nil)
		}

		// 如果超过 10 毫秒没有进行网络轮询，则进行网络轮询
		lastpoll := sched.lastpoll.Load() // lastpoll 上一次网络轮询的时间戳，如果当前正在进行轮询，则为 0。
		// 确保网络轮询已初始化、lastpoll 不为0且距离上次网络轮询已经超过 10 毫秒。
		if netpollinited() && lastpoll != 0 && lastpoll+10*1000*1000 < now {
			// 使用原子比较和交换操作更新 lastpoll 的值为当前时间 now, 表示此次网络轮询的时间戳
			sched.lastpoll.CompareAndSwap(lastpoll, now)
			// 以非阻塞模式进行网络轮询，返回一个列表 list。
			// 函数用于检查就绪的网络连接。运行时网络 I/O 的关键部分，
			// 它利用平台的 IO 完成端口机制来高效地检测就绪的网络连接，并准备好相应的 goroutine 进行后续的网络操作
			// 返回一个 goroutine 列表，表示这些 goroutine 的网络阻塞已经停止, 可以开始调度运行。
			list := netpoll(0)
			// 如果不为空
			if !list.empty() {
				incidlelocked(-1)  // 减少锁定的空闲 M 的计数，表示有 M 被占用
				injectglist(&list) // 将需要运行的 goroutine 列表注入到全局队列中，准备执行。
				incidlelocked(1)   // 增加锁定的空闲 M 的计数，表示空闲 M 的数量增加。
			}
		}

		// 特殊处理 NetBSD 上的定时器问题。
		if GOOS == "netbsd" && needSysmonWorkaround {
			// netpoll 负责等待计时器到期，因此通常无需担心启动一个 M 来处理计时器。
			// （需要注意，上面 sleep for timeSleepUntil 仅确保 sysmon 在可能导致 Go 代码再次运行的计时器到期时重新开始运行）。
			//
			// 然而，netbsd 存在一个内核错误，有时会错过 netpollBreak 的唤醒，可能会导致服务计时器的延迟。
			// 如果检测到这种情况，就启动一个 M 来处理计时器。
			//
			// 参见 issue 42515 和 https://gnats.netbsd.org/cgi-bin/query-pr-single.pl?number=50094。
			if next := timeSleepUntil(); next < now {
				startm(nil, false, false) // 调度一个 M 来运行 P（如果有必要，创建一个新的 M）。
			}
		}

		// 如果 scavenger 请求唤醒，则唤醒 scavenger。
		if scavenger.sysmonWake.Load() != 0 {
			// 如果有人请求，则唤醒清扫程序。
			scavenger.wake()
		}

		// 函数尝试重新获取因系统调用而阻塞的处理器（P），
		// 这样可以确保运行时能够有效地管理资源和调度 Goroutine。
		if retake(now) != 0 {
			idle = 0 // 如果成功重新获取 P 或抢占 G，则重置 idle。
		} else {
			idle++ // 否则增加 idle 计数。
		}

		// 检查是否需要强制执行 GC
		if t := (gcTrigger{kind: gcTriggerTime, now: now}); t.test() && forcegc.idle.Load() {
			lock(&forcegc.lock)
			forcegc.idle.Store(false)
			var list gList
			list.push(forcegc.g)
			injectglist(&list)
			unlock(&forcegc.lock)
		}

		// 记录调度跟踪信息。
		if debug.schedtrace > 0 && lasttrace+int64(debug.schedtrace)*1000000 <= now {
			lasttrace = now
			schedtrace(debug.scheddetail > 0)
		}

		// 解锁 sysmon 专用锁。
		unlock(&sched.sysmonlock)
	}
}

type sysmontick struct {
	schedtick   uint32
	schedwhen   int64
	syscalltick uint32
	syscallwhen int64
}

// forcePreemptNS is the time slice given to a G before it is
// preempted.
const forcePreemptNS = 10 * 1000 * 1000 // 10ms

// 函数尝试重新获取因系统调用而阻塞的处理器（P），
// 这样可以确保运行时能够有效地管理资源和调度 Goroutine。
// 1.如果 Goroutine 运行时间过长，尝试抢占。
// 2.如果 P 正在系统调用中, 尝试将 P 的控制权交给另一个 M。
func retake(now int64) uint32 {
	// 初始化 n 为 0，用于计数重新获取的 P。
	n := 0

	// 防止 allp 切片在遍历过程中被修改。这个锁通常不会竞争，
	// 除非我们已经在停止世界（stop-the-world）操作中。
	lock(&allpLock)

	// 不能使用 for-range 循环遍历 allp，因为可能会暂时释放 allpLock。
	// 因此，每次循环都需要重新获取 allp。
	for i := 0; i < len(allp); i++ {
		pp := allp[i]

		// 这种情况发生在 procresize 增大了 allp 的大小，
		// 但是还没有创建新的 P。
		if pp == nil {
			continue
		}

		pd := &pp.sysmontick
		s := pp.status

		// 标志表示是否需要重新获取 P（仅在系统调用场景下）。
		sysretake := false

		// 如果 P 正在运行或在系统调用中，检查是否需要抢占当前运行的 Goroutine。
		if s == _Prunning || s == _Psyscall {
			t := int64(pp.schedtick)

			// 更新 P 的调度计数和时间戳。
			if int64(pd.schedtick) != t {
				pd.schedtick = uint32(t)
				pd.schedwhen = now
			} else if pd.schedwhen+forcePreemptNS <= now {
				preemptone(pp) // 如果 Goroutine 运行时间过长，尝试抢占。
				// 对于系统调用，preemptone 可能无效，因为此时没有 M 与 P 相关联。
				sysretake = true
			}
		}

		// 如果 P 正在系统调用中，检查是否需要重新获取 P。
		if s == _Psyscall {
			// 如果超过1个sysmon滴答 (至少20us)，则从syscall重新获取P。
			t := int64(pp.syscalltick)

			// 更新 P 的系统调用计数和时间戳。
			if !sysretake && int64(pd.syscalltick) != t {
				pd.syscalltick = uint32(t)
				pd.syscallwhen = now
				continue
			}

			// 只要满足下面三个条件之一，则抢占该 p，否则不抢占
			// 1. p 的运行队列里面有等待运行的 goroutine
			// 2. 所在的 M 线程正在空转或空闲
			// 3. 从上一次监控线程观察到 p 对应的 m 处于系统调用之中到现在已经超过 10 毫秒
			if runqempty(pp) && sched.nmspinning.Load()+sched.npidle.Load() > 0 && pd.syscallwhen+10*1000*1000 > now {
				continue
			}

			// 释放 allpLock，以便可以获取 sched.lock。
			unlock(&allpLock)

			// 减少空闲锁定 M 的数量（假装又有一个 M 正在运行），
			// 这样在 CAS 操作之前可以避免死锁报告。
			incidlelocked(-1)

			// 尝试原子更新 P 的状态，将其从系统调用状态变为空闲状态。
			if atomic.Cas(&pp.status, s, _Pidle) {
				// 记录系统调用阻塞和进程停止的跟踪事件。
				if traceEnabled() {
					traceGoSysBlock(pp)
					traceProcStop(pp)
				}
				n++
				pp.syscalltick++
				handoffp(pp) // 将 P 的控制权交给另一个 M。
			}

			// 恢复空闲锁定 M 的数量。
			incidlelocked(1)
			// 重新获取 allpLock。
			lock(&allpLock)
		}
	}
	// 释放 allpLock。
	unlock(&allpLock)

	// 返回重新获取的 P 的数量。
	return uint32(n)
}

// Tell all goroutines that they have been preempted and they should stop.
// This function is purely best-effort. It can fail to inform a goroutine if a
// processor just started running it.
// No locks need to be held.
// Returns true if preemption request was issued to at least one goroutine.
func preemptall() bool {
	res := false
	for _, pp := range allp {
		if pp.status != _Prunning {
			continue
		}
		if preemptone(pp) {
			res = true
		}
	}
	return res
}

// 函数尝试请求在处理器 P 上运行的 Goroutine 停止。
// 此函数仅尽力而为。它可能无法正确通知 Goroutine，也可能通知错误的 Goroutine。
// 即使通知了正确的 Goroutine，如果 Goroutine 正在同时执行 newstack，它也可能会忽略请求。
// 不需要持有任何锁。
// 如果成功发出抢占请求则返回 true。
// 实际的抢占将在未来某个时刻发生，并通过 gp->status 不再是 Grunning 来指示。
func preemptone(pp *p) bool {
	// 获取 P 当前绑定的 M（机器线程）。
	mp := pp.m.ptr()
	// 如果 M 不存在或 M 与当前调用者的 M 相同，则无法抢占。
	if mp == nil || mp == getg().m {
		return false
	}

	// 获取 M 当前正在运行的 Goroutine。
	// 被抢占的 goroutine
	gp := mp.curg
	// 如果当前没有运行 Goroutine 或运行的是 M 的初始 Goroutine，则无法抢占。
	if gp == nil || gp == mp.g0 {
		return false
	}

	// 设置 Goroutine 的 preempt 标志为 true。
	// 这种标志可以被看作是 Goroutine 的一种外部状态标识，指示着 Goroutine 被强制中断执行
	// 这意味着当前正在执行的 Goroutine 会被暂停，让出 CPU，切换到其他 Goroutine 执行
	// 被标识为外部状态的 Goroutine 在调度器重新选择它作为下一个要执行的 Goroutine时，会被恢复执行
	gp.preempt = true // 抢占

	// 将 Goroutine 的 stackguard0 设置为 stackPreempt。
	// 在 goroutine 内部的每次调用都会比较栈顶指针和 g.stackguard0，
	// 来判断是否发生了栈溢出。stackPreempt 非常大的一个数，比任何栈都大
	// stackPreempt = 0xfffffade
	gp.stackguard0 = stackPreempt

	// 请求异步抢占此 P。
	// 如果支持抢占并且异步抢占未被禁用，则设置 P 的 preempt 标志为 true 并调用 preemptM。
	if preemptMSupported && debug.asyncpreemptoff == 0 {
		pp.preempt = true
		// 请求外部操作系统来暂停并抢占一个特定的 M（机器线程），以便实现 Goroutine 的抢占式调度
		preemptM(mp) // 抢占 G
	}

	// 返回 true 表示抢占请求已发出。
	return true
}

var starttime int64

func schedtrace(detailed bool) {
	now := nanotime()
	if starttime == 0 {
		starttime = now
	}

	lock(&sched.lock)
	print("SCHED ", (now-starttime)/1e6, "ms: gomaxprocs=", gomaxprocs, " idleprocs=", sched.npidle.Load(), " threads=", mcount(), " spinningthreads=", sched.nmspinning.Load(), " needspinning=", sched.needspinning.Load(), " idlethreads=", sched.nmidle, " runqueue=", sched.runqsize)
	if detailed {
		print(" gcwaiting=", sched.gcwaiting.Load(), " nmidlelocked=", sched.nmidlelocked, " stopwait=", sched.stopwait, " sysmonwait=", sched.sysmonwait.Load(), "\n")
	}
	// We must be careful while reading data from P's, M's and G's.
	// Even if we hold schedlock, most data can be changed concurrently.
	// E.g. (p->m ? p->m->id : -1) can crash if p->m changes from non-nil to nil.
	for i, pp := range allp {
		mp := pp.m.ptr()
		h := atomic.Load(&pp.runqhead)
		t := atomic.Load(&pp.runqtail)
		if detailed {
			print("  P", i, ": status=", pp.status, " schedtick=", pp.schedtick, " syscalltick=", pp.syscalltick, " m=")
			if mp != nil {
				print(mp.id)
			} else {
				print("nil")
			}
			print(" runqsize=", t-h, " gfreecnt=", pp.gFree.n, " timerslen=", len(pp.timers), "\n")
		} else {
			// In non-detailed mode format lengths of per-P run queues as:
			// [len1 len2 len3 len4]
			print(" ")
			if i == 0 {
				print("[")
			}
			print(t - h)
			if i == len(allp)-1 {
				print("]\n")
			}
		}
	}

	if !detailed {
		unlock(&sched.lock)
		return
	}

	for mp := allm; mp != nil; mp = mp.alllink {
		pp := mp.p.ptr()
		print("  M", mp.id, ": p=")
		if pp != nil {
			print(pp.id)
		} else {
			print("nil")
		}
		print(" curg=")
		if mp.curg != nil {
			print(mp.curg.goid)
		} else {
			print("nil")
		}
		print(" mallocing=", mp.mallocing, " throwing=", mp.throwing, " preemptoff=", mp.preemptoff, " locks=", mp.locks, " dying=", mp.dying, " spinning=", mp.spinning, " blocked=", mp.blocked, " lockedg=")
		if lockedg := mp.lockedg.ptr(); lockedg != nil {
			print(lockedg.goid)
		} else {
			print("nil")
		}
		print("\n")
	}

	forEachG(func(gp *g) {
		print("  G", gp.goid, ": status=", readgstatus(gp), "(", gp.waitreason.String(), ") m=")
		if gp.m != nil {
			print(gp.m.id)
		} else {
			print("nil")
		}
		print(" lockedm=")
		if lockedm := gp.lockedm.ptr(); lockedm != nil {
			print(lockedm.id)
		} else {
			print("nil")
		}
		print("\n")
	})
	unlock(&sched.lock)
}

// schedEnableUser enables or disables the scheduling of user
// goroutines.
//
// This does not stop already running user goroutines, so the caller
// should first stop the world when disabling user goroutines.
func schedEnableUser(enable bool) {
	lock(&sched.lock)
	if sched.disable.user == !enable {
		unlock(&sched.lock)
		return
	}
	sched.disable.user = !enable
	if enable {
		n := sched.disable.n
		sched.disable.n = 0
		globrunqputbatch(&sched.disable.runnable, n)
		unlock(&sched.lock)
		for ; n != 0 && sched.npidle.Load() != 0; n-- {
			startm(nil, false, false)
		}
	} else {
		unlock(&sched.lock)
	}
}

// schedEnabled reports whether gp should be scheduled. It returns
// false is scheduling of gp is disabled.
//
// sched.lock must be held.
func schedEnabled(gp *g) bool {
	assertLockHeld(&sched.lock)

	if sched.disable.user {
		return isSystemGoroutine(gp, true)
	}
	return true
}

// Put mp on midle list.
// sched.lock must be held.
// May run during STW, so write barriers are not allowed.
//
//go:nowritebarrierrec
func mput(mp *m) {
	assertLockHeld(&sched.lock)

	mp.schedlink = sched.midle
	sched.midle.set(mp)
	sched.nmidle++
	checkdead()
}

// 从 midle（空闲 M 链表）中尝试获取一个 M。
// 调用该函数时必须持有 sched.lock。
// 可能在停止-世界（STW）期间运行，因此不允许写屏障。
//
//go:nowritebarrierrec
func mget() *m {
	assertLockHeld(&sched.lock) // 断言当前持有 sched.lock，确保线程安全性

	// 从 midle 中取出一个空闲 M，并将其作为返回值
	mp := sched.midle.ptr() // 获取 midle 链表头部的 M 指针
	if mp != nil {          // 如果获取到了一个空闲的 M
		// 更新 midle 链表头部为下一个 M，并减少 midle 中空闲 M 的数量
		sched.midle = mp.schedlink // 将 midle 链表头部指向下一个空闲的 M
		sched.nmidle--             // 减少 midle 中空闲 M 的数量
	}
	return mp // 返回获取到的 M，如果没有空闲 M 则返回 nil
}

// Put gp on the global runnable queue.
// sched.lock must be held.
// May run during STW, so write barriers are not allowed.
//
//go:nowritebarrierrec
func globrunqput(gp *g) {
	assertLockHeld(&sched.lock)

	sched.runq.pushBack(gp)
	sched.runqsize++
}

// Put gp at the head of the global runnable queue.
// sched.lock must be held.
// May run during STW, so write barriers are not allowed.
//
//go:nowritebarrierrec
func globrunqputhead(gp *g) {
	assertLockHeld(&sched.lock)

	sched.runq.push(gp)
	sched.runqsize++
}

// Put a batch of runnable goroutines on the global runnable queue.
// This clears *batch.
// sched.lock must be held.
// May run during STW, so write barriers are not allowed.
//
//go:nowritebarrierrec
func globrunqputbatch(batch *gQueue, n int32) {
	assertLockHeld(&sched.lock)

	sched.runq.pushBackAll(*batch)
	sched.runqsize += n
	*batch = gQueue{}
}

// Try get a batch of G's from the global runnable queue.
// sched.lock must be held.
func globrunqget(pp *p, max int32) *g {
	assertLockHeld(&sched.lock)

	if sched.runqsize == 0 {
		return nil
	}

	n := sched.runqsize/gomaxprocs + 1
	if n > sched.runqsize {
		n = sched.runqsize
	}
	if max > 0 && n > max {
		n = max
	}
	if n > int32(len(pp.runq))/2 {
		n = int32(len(pp.runq)) / 2
	}

	sched.runqsize -= n

	gp := sched.runq.pop()
	n--
	for ; n > 0; n-- {
		gp1 := sched.runq.pop()
		runqput(pp, gp1, false)
	}
	return gp
}

// pMask is an atomic bitstring with one bit per P.
type pMask []uint32

// read returns true if P id's bit is set.
func (p pMask) read(id uint32) bool {
	word := id / 32
	mask := uint32(1) << (id % 32)
	return (atomic.Load(&p[word]) & mask) != 0
}

// set sets P id's bit.
func (p pMask) set(id int32) {
	word := id / 32
	mask := uint32(1) << (id % 32)
	atomic.Or(&p[word], mask)
}

// clear clears P id's bit.
func (p pMask) clear(id int32) {
	word := id / 32
	mask := uint32(1) << (id % 32)
	atomic.And(&p[word], ^mask)
}

// updateTimerPMask clears pp's timer mask if it has no timers on its heap.
//
// Ideally, the timer mask would be kept immediately consistent on any timer
// operations. Unfortunately, updating a shared global data structure in the
// timer hot path adds too much overhead in applications frequently switching
// between no timers and some timers.
//
// As a compromise, the timer mask is updated only on pidleget / pidleput. A
// running P (returned by pidleget) may add a timer at any time, so its mask
// must be set. An idle P (passed to pidleput) cannot add new timers while
// idle, so if it has no timers at that time, its mask may be cleared.
//
// Thus, we get the following effects on timer-stealing in findrunnable:
//
//   - Idle Ps with no timers when they go idle are never checked in findrunnable
//     (for work- or timer-stealing; this is the ideal case).
//   - Running Ps must always be checked.
//   - Idle Ps whose timers are stolen must continue to be checked until they run
//     again, even after timer expiration.
//
// When the P starts running again, the mask should be set, as a timer may be
// added at any time.
//
// TODO(prattmic): Additional targeted updates may improve the above cases.
// e.g., updating the mask when stealing a timer.
func updateTimerPMask(pp *p) {
	if pp.numTimers.Load() > 0 {
		return
	}

	// Looks like there are no timers, however another P may transiently
	// decrement numTimers when handling a timerModified timer in
	// checkTimers. We must take timersLock to serialize with these changes.
	lock(&pp.timersLock)
	if pp.numTimers.Load() == 0 {
		timerpMask.clear(pp.id)
	}
	unlock(&pp.timersLock)
}

// pidleput puts p on the _Pidle list. now must be a relatively recent call
// to nanotime or zero. Returns now or the current time if now was zero.
//
// This releases ownership of p. Once sched.lock is released it is no longer
// safe to use p.
//
// sched.lock must be held.
//
// May run during STW, so write barriers are not allowed.
//
//go:nowritebarrierrec
func pidleput(pp *p, now int64) int64 {
	assertLockHeld(&sched.lock)

	if !runqempty(pp) {
		throw("pidleput: P has non-empty run queue")
	}
	if now == 0 {
		now = nanotime()
	}
	updateTimerPMask(pp) // clear if there are no timers.
	idlepMask.set(pp.id)
	pp.link = sched.pidle
	sched.pidle.set(pp)
	sched.npidle.Add(1)
	if !pp.limiterEvent.start(limiterEventIdle, now) {
		throw("must be able to track idle limiter event")
	}
	return now
}

// pidleget tries to get a p from the _Pidle list, acquiring ownership.
//
// sched.lock must be held.
//
// May run during STW, so write barriers are not allowed.
//
//go:nowritebarrierrec
func pidleget(now int64) (*p, int64) {
	assertLockHeld(&sched.lock)

	pp := sched.pidle.ptr()
	if pp != nil {
		// Timer may get added at any time now.
		if now == 0 {
			now = nanotime()
		}
		timerpMask.set(pp.id)
		idlepMask.clear(pp.id)
		sched.pidle = pp.link
		sched.npidle.Add(-1)
		pp.limiterEvent.stop(limiterEventIdle, now)
	}
	return pp, now
}

// pidlegetSpinning tries to get a p from the _Pidle list, acquiring ownership.
// This is called by spinning Ms (or callers than need a spinning M) that have
// found work. If no P is available, this must synchronized with non-spinning
// Ms that may be preparing to drop their P without discovering this work.
//
// sched.lock must be held.
//
// May run during STW, so write barriers are not allowed.
//
//go:nowritebarrierrec
func pidlegetSpinning(now int64) (*p, int64) {
	assertLockHeld(&sched.lock)

	pp, now := pidleget(now)
	if pp == nil {
		// See "Delicate dance" comment in findrunnable. We found work
		// that we cannot take, we must synchronize with non-spinning
		// Ms that may be preparing to drop their P.
		sched.needspinning.Store(1)
		return nil, now
	}

	return pp, now
}

// runqempty reports whether pp has no Gs on its local run queue.
// It never returns true spuriously.
func runqempty(pp *p) bool {
	// Defend against a race where 1) pp has G1 in runqnext but runqhead == runqtail,
	// 2) runqput on pp kicks G1 to the runq, 3) runqget on pp empties runqnext.
	// Simply observing that runqhead == runqtail and then observing that runqnext == nil
	// does not mean the queue is empty.
	for {
		head := atomic.Load(&pp.runqhead)
		tail := atomic.Load(&pp.runqtail)
		runnext := atomic.Loaduintptr((*uintptr)(unsafe.Pointer(&pp.runnext)))
		if tail == atomic.Load(&pp.runqtail) {
			return head == tail && runnext == 0
		}
	}
}

// To shake out latent assumptions about scheduling order,
// we introduce some randomness into scheduling decisions
// when running with the race detector.
// The need for this was made obvious by changing the
// (deterministic) scheduling order in Go 1.5 and breaking
// many poorly-written tests.
// With the randomness here, as long as the tests pass
// consistently with -race, they shouldn't have latent scheduling
// assumptions.
const randomizeScheduler = raceenabled

// 尝试将 goroutine gp 放入本地可运行队列。
// 如果 next 为 false (不竞争)，则将 gp 添加到可运行队列的尾部。
// 如果 next 为 true (立即竞争)，则将 gp 放入 pp.runnext 槽。
// 如果本地队列满了，runqput 将 gp 放到全局队列上。
// 仅由拥有 P 的线程执行。
func runqput(pp *p, gp *g, next bool) {
	// 如果启用了调度器随机化，且 next 为 true，且随机数生成器生成的数字为 0，
	// 则将 next 设置为 false。这是为了增加调度的随机性。
	if randomizeScheduler && next && fastrandn(2) == 0 {
		next = false
	}

	if next {
	retryNext: // 重试标签
		// 获取当前的 runnext 指针。指向下一个应该运行的 goroutine 的指针，用于优化通信和等待模式的调度
		oldnext := pp.runnext
		// 使用 compare-and-swap (CAS) 操作尝试将 gp 设置为新的 runnext。
		if !pp.runnext.cas(oldnext, guintptr(unsafe.Pointer(gp))) {
			// 如果 CAS 失败，意味着另一个 goroutine 同时也在尝试修改 runnext，
			// 因此需要重新尝试直到成功。
			goto retryNext
		}

		// 检查旧的 runnext 是否为零。
		if oldnext == 0 {
			// 如果旧的 runnext 为零，这意味着我们是第一个设置 runnext 的，
			// 所以我们可以直接返回。
			return
		}

		// 注意，这里我们将 gp 的指针设置为旧的 runnext 的值，以便在下一次循环中将其添加到队列。
		// 将原来的 runnext 踢出到常规可运行队列。
		gp = oldnext.ptr()
	}

retry: // 重试标签
	// 加载头部，使用 load-acquire 原子操作来读取 runqhead，
	// 这样可以保证从其他处理器看到的值是最新的。
	h := atomic.LoadAcq(&pp.runqhead)
	t := pp.runqtail
	// 检查队列是否已满，通过比较尾部与头部之间的距离是否小于队列的长度。
	if t-h < uint32(len(pp.runq)) {
		// 计算队列中下一个可用槽的位置。
		// 将 gp 设置到队列的下一个可用位置。
		pp.runq[t%uint32(len(pp.runq))].set(gp)
		// 更新队列的尾部指针，使用 store-release 原子操作来写入 runqtail，
		// 这样可以保证其他处理器能够看到最新的尾部指针。
		atomic.StoreRel(&pp.runqtail, t+1)
		// 成功添加后，返回。
		return
	}

	// 当队列满了时，调用 runqputslow 来处理，这通常会将 gp 移动到全局可运行队列。
	if runqputslow(pp, gp, h, t) {
		return
	}

	// 如果 runqputslow 没有处理 gp 或者队列实际上并不满，
	// 则继续尝试将 gp 添加到队列中。
	// 使用 goto 语句跳回 retry 标签处，重新加载头部并检查队列的状态。
	goto retry
}

// Put g and a batch of work from local runnable queue on global queue.
// Executed only by the owner P.
func runqputslow(pp *p, gp *g, h, t uint32) bool {
	var batch [len(pp.runq)/2 + 1]*g

	// First, grab a batch from local queue.
	n := t - h
	n = n / 2
	if n != uint32(len(pp.runq)/2) {
		throw("runqputslow: queue is not full")
	}
	for i := uint32(0); i < n; i++ {
		batch[i] = pp.runq[(h+i)%uint32(len(pp.runq))].ptr()
	}
	if !atomic.CasRel(&pp.runqhead, h, h+n) { // cas-release, commits consume
		return false
	}
	batch[n] = gp

	if randomizeScheduler {
		for i := uint32(1); i <= n; i++ {
			j := fastrandn(i + 1)
			batch[i], batch[j] = batch[j], batch[i]
		}
	}

	// Link the goroutines.
	for i := uint32(0); i < n; i++ {
		batch[i].schedlink.set(batch[i+1])
	}
	var q gQueue
	q.head.set(batch[0])
	q.tail.set(batch[n])

	// Now put the batch on global queue.
	lock(&sched.lock)
	globrunqputbatch(&q, int32(n+1))
	unlock(&sched.lock)
	return true
}

// runqputbatch tries to put all the G's on q on the local runnable queue.
// If the queue is full, they are put on the global queue; in that case
// this will temporarily acquire the scheduler lock.
// Executed only by the owner P.
func runqputbatch(pp *p, q *gQueue, qsize int) {
	h := atomic.LoadAcq(&pp.runqhead)
	t := pp.runqtail
	n := uint32(0)
	for !q.empty() && t-h < uint32(len(pp.runq)) {
		gp := q.pop()
		pp.runq[t%uint32(len(pp.runq))].set(gp)
		t++
		n++
	}
	qsize -= int(n)

	if randomizeScheduler {
		off := func(o uint32) uint32 {
			return (pp.runqtail + o) % uint32(len(pp.runq))
		}
		for i := uint32(1); i < n; i++ {
			j := fastrandn(i + 1)
			pp.runq[off(i)], pp.runq[off(j)] = pp.runq[off(j)], pp.runq[off(i)]
		}
	}

	atomic.StoreRel(&pp.runqtail, t)
	if !q.empty() {
		lock(&sched.lock)
		globrunqputbatch(q, int32(qsize))
		unlock(&sched.lock)
	}
}

// 从本地可运行队列获取一个 Goroutine。
// 如果 inheritTime 为 true，则 gp 应继承当前时间片的剩余时间。否则，它应启动一个新的时间片。
// 仅由所有者 P 执行。
func runqget(pp *p) (gp *g, inheritTime bool) {
	// 如果存在 runnext，则运行下一个 G。
	next := pp.runnext
	// 如果 runnext 非 0 且 CAS 操作失败，那么只能是被其他 P 窃取，
	// 因为其他 P 可以竞争将 runnext 设置为 0，但只有当前 P 可以将其设置为非 0。
	// 因此，如果 CAS 操作失败，没有必要重试这个 CAS 操作。
	if next != 0 && pp.runnext.cas(next, 0) {
		return next.ptr(), true
	}

	for {
		// 加载-获取，在与其他消费者同步
		h := atomic.LoadAcq(&pp.runqhead)
		t := pp.runqtail
		if t == h {
			// 如果队列为空，返回 nil 和 inheritTime 为 false。
			return nil, false
		}
		gp := pp.runq[h%uint32(len(pp.runq))].ptr()
		// 比较并交换-释放，提交消费
		if atomic.CasRel(&pp.runqhead, h, h+1) {
			// 成功从队列中取出一个 Goroutine，inheritTime 为 false。
			return gp, false
		}
	}
}

// runqdrain drains the local runnable queue of pp and returns all goroutines in it.
// Executed only by the owner P.
func runqdrain(pp *p) (drainQ gQueue, n uint32) {
	oldNext := pp.runnext
	if oldNext != 0 && pp.runnext.cas(oldNext, 0) {
		drainQ.pushBack(oldNext.ptr())
		n++
	}

retry:
	h := atomic.LoadAcq(&pp.runqhead) // load-acquire, synchronize with other consumers
	t := pp.runqtail
	qn := t - h
	if qn == 0 {
		return
	}
	if qn > uint32(len(pp.runq)) { // read inconsistent h and t
		goto retry
	}

	if !atomic.CasRel(&pp.runqhead, h, h+qn) { // cas-release, commits consume
		goto retry
	}

	// We've inverted the order in which it gets G's from the local P's runnable queue
	// and then advances the head pointer because we don't want to mess up the statuses of G's
	// while runqdrain() and runqsteal() are running in parallel.
	// Thus we should advance the head pointer before draining the local P into a gQueue,
	// so that we can update any gp.schedlink only after we take the full ownership of G,
	// meanwhile, other P's can't access to all G's in local P's runnable queue and steal them.
	// See https://groups.google.com/g/golang-dev/c/0pTKxEKhHSc/m/6Q85QjdVBQAJ for more details.
	for i := uint32(0); i < qn; i++ {
		gp := pp.runq[(h+i)%uint32(len(pp.runq))].ptr()
		drainQ.pushBack(gp)
		n++
	}
	return
}

// Grabs a batch of goroutines from pp's runnable queue into batch.
// Batch is a ring buffer starting at batchHead.
// Returns number of grabbed goroutines.
// Can be executed by any P.
func runqgrab(pp *p, batch *[256]guintptr, batchHead uint32, stealRunNextG bool) uint32 {
	for {
		h := atomic.LoadAcq(&pp.runqhead) // load-acquire, synchronize with other consumers
		t := atomic.LoadAcq(&pp.runqtail) // load-acquire, synchronize with the producer
		n := t - h
		n = n - n/2
		if n == 0 {
			if stealRunNextG {
				// Try to steal from pp.runnext.
				if next := pp.runnext; next != 0 {
					if pp.status == _Prunning {
						// Sleep to ensure that pp isn't about to run the g
						// we are about to steal.
						// The important use case here is when the g running
						// on pp ready()s another g and then almost
						// immediately blocks. Instead of stealing runnext
						// in this window, back off to give pp a chance to
						// schedule runnext. This will avoid thrashing gs
						// between different Ps.
						// A sync chan send/recv takes ~50ns as of time of
						// writing, so 3us gives ~50x overshoot.
						if GOOS != "windows" && GOOS != "openbsd" && GOOS != "netbsd" {
							usleep(3)
						} else {
							// On some platforms system timer granularity is
							// 1-15ms, which is way too much for this
							// optimization. So just yield.
							osyield()
						}
					}
					if !pp.runnext.cas(next, 0) {
						continue
					}
					batch[batchHead%uint32(len(batch))] = next
					return 1
				}
			}
			return 0
		}
		if n > uint32(len(pp.runq)/2) { // read inconsistent h and t
			continue
		}
		for i := uint32(0); i < n; i++ {
			g := pp.runq[(h+i)%uint32(len(pp.runq))]
			batch[(batchHead+i)%uint32(len(batch))] = g
		}
		if atomic.CasRel(&pp.runqhead, h, h+n) { // cas-release, commits consume
			return n
		}
	}
}

// Steal half of elements from local runnable queue of p2
// and put onto local runnable queue of p.
// Returns one of the stolen elements (or nil if failed).
func runqsteal(pp, p2 *p, stealRunNextG bool) *g {
	t := pp.runqtail
	n := runqgrab(p2, &pp.runq, t, stealRunNextG)
	if n == 0 {
		return nil
	}
	n--
	gp := pp.runq[(t+n)%uint32(len(pp.runq))].ptr()
	if n == 0 {
		return gp
	}
	h := atomic.LoadAcq(&pp.runqhead) // load-acquire, synchronize with consumers
	if t-h+n >= uint32(len(pp.runq)) {
		throw("runqsteal: runq overflow")
	}
	atomic.StoreRel(&pp.runqtail, t+n) // store-release, makes the item available for consumption
	return gp
}

// A gQueue is a dequeue of Gs linked through g.schedlink. A G can only
// be on one gQueue or gList at a time.
type gQueue struct {
	head guintptr
	tail guintptr
}

// empty reports whether q is empty.
func (q *gQueue) empty() bool {
	return q.head == 0
}

// push adds gp to the head of q.
func (q *gQueue) push(gp *g) {
	gp.schedlink = q.head
	q.head.set(gp)
	if q.tail == 0 {
		q.tail.set(gp)
	}
}

// pushBack adds gp to the tail of q.
func (q *gQueue) pushBack(gp *g) {
	gp.schedlink = 0
	if q.tail != 0 {
		q.tail.ptr().schedlink.set(gp)
	} else {
		q.head.set(gp)
	}
	q.tail.set(gp)
}

// pushBackAll adds all Gs in q2 to the tail of q. After this q2 must
// not be used.
func (q *gQueue) pushBackAll(q2 gQueue) {
	if q2.tail == 0 {
		return
	}
	q2.tail.ptr().schedlink = 0
	if q.tail != 0 {
		q.tail.ptr().schedlink = q2.head
	} else {
		q.head = q2.head
	}
	q.tail = q2.tail
}

// pop removes and returns the head of queue q. It returns nil if
// q is empty.
func (q *gQueue) pop() *g {
	gp := q.head.ptr()
	if gp != nil {
		q.head = gp.schedlink
		if q.head == 0 {
			q.tail = 0
		}
	}
	return gp
}

// popList takes all Gs in q and returns them as a gList.
func (q *gQueue) popList() gList {
	stack := gList{q.head}
	*q = gQueue{}
	return stack
}

// A gList is a list of Gs linked through g.schedlink. A G can only be
// on one gQueue or gList at a time.
type gList struct {
	head guintptr
}

// empty reports whether l is empty.
func (l *gList) empty() bool {
	return l.head == 0
}

// push adds gp to the head of l.
func (l *gList) push(gp *g) {
	gp.schedlink = l.head
	l.head.set(gp)
}

// pushAll prepends all Gs in q to l.
func (l *gList) pushAll(q gQueue) {
	if !q.empty() {
		q.tail.ptr().schedlink = l.head
		l.head = q.head
	}
}

// pop removes and returns the head of l. If l is empty, it returns nil.
func (l *gList) pop() *g {
	gp := l.head.ptr()
	if gp != nil {
		l.head = gp.schedlink
	}
	return gp
}

//go:linkname setMaxThreads runtime/debug.setMaxThreads
func setMaxThreads(in int) (out int) {
	lock(&sched.lock)
	out = int(sched.maxmcount)
	if in > 0x7fffffff { // MaxInt32
		sched.maxmcount = 0x7fffffff
	} else {
		sched.maxmcount = int32(in)
	}
	checkmcount()
	unlock(&sched.lock)
	return
}

//go:nosplit
func procPin() int {
	gp := getg()
	mp := gp.m

	mp.locks++
	return int(mp.p.ptr().id)
}

//go:nosplit
func procUnpin() {
	gp := getg()
	gp.m.locks--
}

//go:linkname sync_runtime_procPin sync.runtime_procPin
//go:nosplit
func sync_runtime_procPin() int {
	return procPin()
}

//go:linkname sync_runtime_procUnpin sync.runtime_procUnpin
//go:nosplit
func sync_runtime_procUnpin() {
	procUnpin()
}

//go:linkname sync_atomic_runtime_procPin sync/atomic.runtime_procPin
//go:nosplit
func sync_atomic_runtime_procPin() int {
	return procPin()
}

//go:linkname sync_atomic_runtime_procUnpin sync/atomic.runtime_procUnpin
//go:nosplit
func sync_atomic_runtime_procUnpin() {
	procUnpin()
}

// Active spinning for sync.Mutex.
//
//go:linkname sync_runtime_canSpin sync.runtime_canSpin
//go:nosplit
func sync_runtime_canSpin(i int) bool {
	// sync.Mutex is cooperative, so we are conservative with spinning.
	// Spin only few times and only if running on a multicore machine and
	// GOMAXPROCS>1 and there is at least one other running P and local runq is empty.
	// As opposed to runtime mutex we don't do passive spinning here,
	// because there can be work on global runq or on other Ps.
	if i >= active_spin || ncpu <= 1 || gomaxprocs <= sched.npidle.Load()+sched.nmspinning.Load()+1 {
		return false
	}
	if p := getg().m.p.ptr(); !runqempty(p) {
		return false
	}
	return true
}

//go:linkname sync_runtime_doSpin sync.runtime_doSpin
//go:nosplit
func sync_runtime_doSpin() {
	procyield(active_spin_cnt)
}

var stealOrder randomOrder

// randomOrder/randomEnum are helper types for randomized work stealing.
// They allow to enumerate all Ps in different pseudo-random orders without repetitions.
// The algorithm is based on the fact that if we have X such that X and GOMAXPROCS
// are coprime, then a sequences of (i + X) % GOMAXPROCS gives the required enumeration.
type randomOrder struct {
	count    uint32
	coprimes []uint32
}

type randomEnum struct {
	i     uint32
	count uint32
	pos   uint32
	inc   uint32
}

func (ord *randomOrder) reset(count uint32) {
	ord.count = count
	ord.coprimes = ord.coprimes[:0]
	for i := uint32(1); i <= count; i++ {
		if gcd(i, count) == 1 {
			ord.coprimes = append(ord.coprimes, i)
		}
	}
}

func (ord *randomOrder) start(i uint32) randomEnum {
	return randomEnum{
		count: ord.count,
		pos:   i % ord.count,
		inc:   ord.coprimes[i/ord.count%uint32(len(ord.coprimes))],
	}
}

func (enum *randomEnum) done() bool {
	return enum.i == enum.count
}

func (enum *randomEnum) next() {
	enum.i++
	enum.pos = (enum.pos + enum.inc) % enum.count
}

func (enum *randomEnum) position() uint32 {
	return enum.pos
}

func gcd(a, b uint32) uint32 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// An initTask represents the set of initializations that need to be done for a package.
// Keep in sync with ../../test/noinit.go:initTask
type initTask struct {
	state uint32 // 0 = uninitialized, 1 = in progress, 2 = done
	nfns  uint32
	// followed by nfns pcs, uintptr sized, one per init function to run
}

// inittrace stores statistics for init functions which are
// updated by malloc and newproc when active is true.
var inittrace tracestat

type tracestat struct {
	active bool   // init tracing activation status
	id     uint64 // init goroutine id
	allocs uint64 // heap allocations
	bytes  uint64 // heap allocated bytes
}

func doInit(ts []*initTask) {
	for _, t := range ts {
		doInit1(t)
	}
}

func doInit1(t *initTask) {
	switch t.state {
	case 2: // fully initialized
		return
	case 1: // initialization in progress
		throw("recursive call during initialization - linker skew")
	default: // not initialized yet
		t.state = 1 // initialization in progress

		var (
			start  int64
			before tracestat
		)

		if inittrace.active {
			start = nanotime()
			// Load stats non-atomically since tracinit is updated only by this init goroutine.
			before = inittrace
		}

		if t.nfns == 0 {
			// We should have pruned all of these in the linker.
			throw("inittask with no functions")
		}

		firstFunc := add(unsafe.Pointer(t), 8)
		for i := uint32(0); i < t.nfns; i++ {
			p := add(firstFunc, uintptr(i)*goarch.PtrSize)
			f := *(*func())(unsafe.Pointer(&p))
			f()
		}

		if inittrace.active {
			end := nanotime()
			// Load stats non-atomically since tracinit is updated only by this init goroutine.
			after := inittrace

			f := *(*func())(unsafe.Pointer(&firstFunc))
			pkg := funcpkgpath(findfunc(abi.FuncPCABIInternal(f)))

			var sbuf [24]byte
			print("init ", pkg, " @")
			print(string(fmtNSAsMS(sbuf[:], uint64(start-runtimeInitTime))), " ms, ")
			print(string(fmtNSAsMS(sbuf[:], uint64(end-start))), " ms clock, ")
			print(string(itoa(sbuf[:], after.bytes-before.bytes)), " bytes, ")
			print(string(itoa(sbuf[:], after.allocs-before.allocs)), " allocs")
			print("\n")
		}

		t.state = 2 // initialization done
	}
}
