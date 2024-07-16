// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/abi"
	"internal/goarch"
	"runtime/internal/atomic"
	"runtime/internal/sys"
	"unsafe"
)

// defined constants
const (
	// G 状态
	//
	// 除了表示 G 的一般状态外，G 状态也像是对该 goroutine 的栈（从而也是其执行用户代码的能力）的锁。
	//
	// 如果您要添加到此列表，请在 mgcmark.go 中的“垃圾回收期间允许”的状态列表中添加。
	//
	// TODO（austin）：_Gscan 位可以设计得更轻量级。
	// 例如，我们可能选择不运行在运行队列中找到的 _Gscanrunnable goroutines，而是直到它们变为 _Grunnable 为止，而不是进行 CAS 循环。
	// 转换如 _Gscanwaiting -> _Gscanrunnable 实际上是可以接受的，因为它们不影响栈的所有权。

	// _Gidle 表示此 goroutine 刚刚分配并尚未初始化。
	_Gidle = iota // 0

	// _Grunnable 表示此 goroutine 在运行队列上。它当前没有执行用户代码。栈没有所有权。
	_Grunnable // 1

	// _Grunning 表示此 goroutine 可能执行用户代码。栈由此 goroutine 拥有。它不在运行队列上。
	// 它分配了一个 M 和一个 P（g.m 和 g.m.p 有效）。
	_Grunning // 2

	// _Gsyscall 表示此 goroutine 正在执行系统调用。它没有执行用户代码。栈由此 goroutine 拥有。
	// 它没有在运行队列上。它被分配了一个 M。
	_Gsyscall // 3

	// _Gwaiting 表示此 goroutine 在运行时中被阻塞。它没有在执行用户代码。它不在运行队列上，
	// 但应该在某个地方记录下来（例如，通道等待队列），以便在必要时可以 ready() 它。
	// 栈不属于它自己，除非在适当的通道锁定下，通道操作可能读取或写入栈的部分。
	// 否则，在 goroutine 进入 _Gwaiting 后，访问栈不安全（例如，它可能会被移动）。
	_Gwaiting // 4

	// _Gmoribund_unused 目前未使用，但在 gdb 脚本中硬编码。
	_Gmoribund_unused // 5

	// _Gdead 表示此 goroutine 目前未使用。它可能刚刚退出，位于空闲列表上，或者刚刚初始化。
	// 它没有执行用户代码。它可能有可能没有分配栈。G 及其栈（如果有）属于正在退出该 G 的 M 或从空闲列表获取该 G 的 M 所拥有。
	_Gdead // 6

	// _Genqueue_unused 目前未使用。
	_Genqueue_unused // 7

	// _Gcopystack 表示此 goroutine 的栈正在移动。它没有执行用户代码，也不在运行队列上。
	// 栈由将其置于 _Gcopystack 中的 goroutine 拥有。
	_Gcopystack // 8

	// _Gpreempted 表示此 goroutine 由于暂停 G 的抢占而停止自身。
	// 它类似于 _Gwaiting，但尚无任何东西负责 ready() 该 goroutine。
	// 某些暂停 G 必须将状态 CAS 为 _Gwaiting，以负责 ready() 此 G。
	_Gpreempted // 9

	// _Gscan combined with one of the above states other than
	// _Grunning indicates that GC is scanning the stack. The
	// goroutine is not executing user code and the stack is owned
	// by the goroutine that set the _Gscan bit.
	//
	// _Gscanrunning is different: it is used to briefly block
	// state transitions while GC signals the G to scan its own
	// stack. This is otherwise like _Grunning.
	//
	// atomicstatus&~Gscan gives the state the goroutine will
	// return to when the scan completes.
	_Gscan          = 0x1000
	_Gscanrunnable  = _Gscan + _Grunnable  // 0x1001
	_Gscanrunning   = _Gscan + _Grunning   // 0x1002
	_Gscansyscall   = _Gscan + _Gsyscall   // 0x1003
	_Gscanwaiting   = _Gscan + _Gwaiting   // 0x1004
	_Gscanpreempted = _Gscan + _Gpreempted // 0x1009
)

const (
	// P status

	// _Pidle means a P is not being used to run user code or the
	// scheduler. Typically, it's on the idle P list and available
	// to the scheduler, but it may just be transitioning between
	// other states.
	//
	// The P is owned by the idle list or by whatever is
	// transitioning its state. Its run queue is empty.
	_Pidle = iota

	// _Prunning means a P is owned by an M and is being used to
	// run user code or the scheduler. Only the M that owns this P
	// is allowed to change the P's status from _Prunning. The M
	// may transition the P to _Pidle (if it has no more work to
	// do), _Psyscall (when entering a syscall), or _Pgcstop (to
	// halt for the GC). The M may also hand ownership of the P
	// off directly to another M (e.g., to schedule a locked G).
	_Prunning

	// _Psyscall means a P is not running user code. It has
	// affinity to an M in a syscall but is not owned by it and
	// may be stolen by another M. This is similar to _Pidle but
	// uses lightweight transitions and maintains M affinity.
	//
	// Leaving _Psyscall must be done with a CAS, either to steal
	// or retake the P. Note that there's an ABA hazard: even if
	// an M successfully CASes its original P back to _Prunning
	// after a syscall, it must understand the P may have been
	// used by another M in the interim.
	_Psyscall

	// _Pgcstop means a P is halted for STW and owned by the M
	// that stopped the world. The M that stopped the world
	// continues to use its P, even in _Pgcstop. Transitioning
	// from _Prunning to _Pgcstop causes an M to release its P and
	// park.
	//
	// The P retains its run queue and startTheWorld will restart
	// the scheduler on Ps with non-empty run queues.
	_Pgcstop

	// _Pdead means a P is no longer used (GOMAXPROCS shrank). We
	// reuse Ps if GOMAXPROCS increases. A dead P is mostly
	// stripped of its resources, though a few things remain
	// (e.g., trace buffers).
	_Pdead
)

// Mutual exclusion locks.  In the uncontended case,
// as fast as spin locks (just a few user-level instructions),
// but on the contention path they sleep in the kernel.
// A zeroed Mutex is unlocked (no need to initialize each lock).
// Initialization is helpful for static lock ranking, but not required.
type mutex struct {
	// Empty struct if lock ranking is disabled, otherwise includes the lock rank
	lockRankStruct
	// Futex-based impl treats it as uint32 key,
	// while sema-based impl as M* waitm.
	// Used to be a union, but unions break precise GC.
	key uintptr
}

// sleep and wakeup on one-time events.
// before any calls to notesleep or notewakeup,
// must call noteclear to initialize the Note.
// then, exactly one thread can call notesleep
// and exactly one thread can call notewakeup (once).
// once notewakeup has been called, the notesleep
// will return.  future notesleep will return immediately.
// subsequent noteclear must be called only after
// previous notesleep has returned, e.g. it's disallowed
// to call noteclear straight after notewakeup.
//
// notetsleep is like notesleep but wakes up after
// a given number of nanoseconds even if the event
// has not yet happened.  if a goroutine uses notetsleep to
// wake up early, it must wait to call noteclear until it
// can be sure that no other goroutine is calling
// notewakeup.
//
// notesleep/notetsleep are generally called on g0,
// notetsleepg is similar to notetsleep but is called on user g.
type note struct {
	// Futex-based impl treats it as uint32 key,
	// while sema-based impl as M* waitm.
	// Used to be a union, but unions break precise GC.
	key uintptr
}

type funcval struct {
	fn uintptr
	// variable-size, fn-specific data here
}

// 定义一个 iface 结构体，用于表示接口值
type iface struct {
	tab  *itab          // 指向 itab 结构体的指针
	data unsafe.Pointer // 存储具体值的指针,一般而言是一个指向堆内存的指针
}

// eface 是空接口的内部表示
type eface struct {
	_type *_type         // 存储类型信息的指针
	data  unsafe.Pointer // 存储实际数据的指针
}

func efaceOf(ep *any) *eface {
	return (*eface)(unsafe.Pointer(ep))
}

// The guintptr, muintptr, and puintptr are all used to bypass write barriers.
// It is particularly important to avoid write barriers when the current P has
// been released, because the GC thinks the world is stopped, and an
// unexpected write barrier would not be synchronized with the GC,
// which can lead to a half-executed write barrier that has marked the object
// but not queued it. If the GC skips the object and completes before the
// queuing can occur, it will incorrectly free the object.
//
// We tried using special assignment functions invoked only when not
// holding a running P, but then some updates to a particular memory
// word went through write barriers and some did not. This breaks the
// write barrier shadow checking mode, and it is also scary: better to have
// a word that is completely ignored by the GC than to have one for which
// only a few updates are ignored.
//
// Gs and Ps are always reachable via true pointers in the
// allgs and allp lists or (during allocation before they reach those lists)
// from stack variables.
//
// Ms are always reachable via true pointers either from allm or
// freem. Unlike Gs and Ps we do free Ms, so it's important that
// nothing ever hold an muintptr across a safe point.

// A guintptr holds a goroutine pointer, but typed as a uintptr
// to bypass write barriers. It is used in the Gobuf goroutine state
// and in scheduling lists that are manipulated without a P.
//
// The Gobuf.g goroutine pointer is almost always updated by assembly code.
// In one of the few places it is updated by Go code - func save - it must be
// treated as a uintptr to avoid a write barrier being emitted at a bad time.
// Instead of figuring out how to emit the write barriers missing in the
// assembly manipulation, we change the type of the field to uintptr,
// so that it does not require write barriers at all.
//
// Goroutine structs are published in the allg list and never freed.
// That will keep the goroutine structs from being collected.
// There is never a time that Gobuf.g's contain the only references
// to a goroutine: the publishing of the goroutine in allg comes first.
// Goroutine pointers are also kept in non-GC-visible places like TLS,
// so I can't see them ever moving. If we did want to start moving data
// in the GC, we'd need to allocate the goroutine structs from an
// alternate arena. Using guintptr doesn't make that problem any worse.
// Note that pollDesc.rg, pollDesc.wg also store g in uintptr form,
// so they would need to be updated too if g's start moving.
type guintptr uintptr

//go:nosplit
func (gp guintptr) ptr() *g { return (*g)(unsafe.Pointer(gp)) }

//go:nosplit
func (gp *guintptr) set(g *g) { *gp = guintptr(unsafe.Pointer(g)) }

//go:nosplit
func (gp *guintptr) cas(old, new guintptr) bool {
	return atomic.Casuintptr((*uintptr)(unsafe.Pointer(gp)), uintptr(old), uintptr(new))
}

//go:nosplit
func (gp *g) guintptr() guintptr {
	return guintptr(unsafe.Pointer(gp))
}

// setGNoWB performs *gp = new without a write barrier.
// For times when it's impractical to use a guintptr.
//
//go:nosplit
//go:nowritebarrier
func setGNoWB(gp **g, new *g) {
	(*guintptr)(unsafe.Pointer(gp)).set(new)
}

type puintptr uintptr

//go:nosplit
func (pp puintptr) ptr() *p { return (*p)(unsafe.Pointer(pp)) }

//go:nosplit
func (pp *puintptr) set(p *p) { *pp = puintptr(unsafe.Pointer(p)) }

// muintptr is a *m that is not tracked by the garbage collector.
//
// Because we do free Ms, there are some additional constrains on
// muintptrs:
//
//  1. Never hold an muintptr locally across a safe point.
//
//  2. Any muintptr in the heap must be owned by the M itself so it can
//     ensure it is not in use when the last true *m is released.
type muintptr uintptr

//go:nosplit
func (mp muintptr) ptr() *m { return (*m)(unsafe.Pointer(mp)) }

//go:nosplit
func (mp *muintptr) set(m *m) { *mp = muintptr(unsafe.Pointer(m)) }

// setMNoWB performs *mp = new without a write barrier.
// For times when it's impractical to use an muintptr.
//
//go:nosplit
//go:nowritebarrier
func setMNoWB(mp **m, new *m) {
	(*muintptr)(unsafe.Pointer(mp)).set(new)
}

// goroutine 的调度信息
type gobuf struct {
	sp uintptr  // 存储当前的栈指针，用于上下文切换时保存和恢复。
	pc uintptr  // 存储当前的程序计数器，即执行指令的地址。
	g  guintptr // 是指向当前 goroutine 的指针，用于识别和调度 goroutine。

	// 是函数上下文，可能指向一个在堆上分配的函数值。
	// 这是一个保存的、活动的寄存器，它在真实寄存器和 gobuf 之间交换。
	// 在栈扫描中被视为根对象，以避免在汇编代码中设置写屏障。
	ctxt unsafe.Pointer

	ret uintptr // 存储返回地址，用于上下文恢复时跳转到正确的指令。
	lr  uintptr // 在 ARM 架构中，保存了调用者提供的返回地址，用于函数调用的上下文恢复

	// 存储基址寄存器的值，用于启用了帧指针的架构（如 x86_64）中的上下文切换。
	// 用于构建函数调用栈的帧结构。
	bp uintptr
}

// sudog represents a g in a wait list, such as for sending/receiving
// on a channel.
//
// sudog is necessary because the g ↔ synchronization object relation
// is many-to-many. A g can be on many wait lists, so there may be
// many sudogs for one g; and many gs may be waiting on the same
// synchronization object, so there may be many sudogs for one object.
//
// sudogs are allocated from a special pool. Use acquireSudog and
// releaseSudog to allocate and free them.
type sudog struct {
	// The following fields are protected by the hchan.lock of the
	// channel this sudog is blocking on. shrinkstack depends on
	// this for sudogs involved in channel ops.

	g *g

	next *sudog
	prev *sudog
	elem unsafe.Pointer // data element (may point to stack)

	// The following fields are never accessed concurrently.
	// For channels, waitlink is only accessed by g.
	// For semaphores, all fields (including the ones above)
	// are only accessed when holding a semaRoot lock.

	acquiretime int64
	releasetime int64
	ticket      uint32

	// isSelect indicates g is participating in a select, so
	// g.selectDone must be CAS'd to win the wake-up race.
	isSelect bool

	// success indicates whether communication over channel c
	// succeeded. It is true if the goroutine was awoken because a
	// value was delivered over channel c, and false if awoken
	// because c was closed.
	success bool

	parent   *sudog // semaRoot binary tree
	waitlink *sudog // g.waiting list or semaRoot
	waittail *sudog // semaRoot
	c        *hchan // channel
}

type libcall struct {
	fn   uintptr
	n    uintptr // number of parameters
	args uintptr // parameters
	r1   uintptr // return values
	r2   uintptr
	err  uintptr // error number
}

// Stack describes a Go execution stack.
// The bounds of the stack are exactly [lo, hi),
// with no implicit data structures on either side.
type stack struct {
	lo uintptr
	hi uintptr
}

// heldLockInfo gives info on a held lock and the rank of that lock
type heldLockInfo struct {
	lockAddr uintptr
	rank     lockRank
}

// goroutine 的数据结构
type g struct {
	stack       stack   // 堆栈参数.描述实际的栈内存区间：[stack.lo, stack.hi)。
	stackguard0 uintptr // 用于抢占的，一般情况值为stack.lo + StackGuard
	stackguard1 uintptr // 用于C语言的抢占

	_panic    *_panic // 内部最深层的 panic。
	_defer    *_defer // 内部最深层的 defer。
	m         *m      // 当前的 m 结构，表示当前的 OS 线程。
	sched     gobuf   // 调度信息。
	syscallsp uintptr // 如果状态为 Gsyscall，则 syscallsp = sched.sp 用于 GC。
	syscallpc uintptr // 如果状态为 Gsyscall，则 syscallpc = sched.pc 用于 GC。
	stktopsp  uintptr // 期望的栈顶指针，用于在回溯中检查。

	// param 是一个通用的指针参数字段，用于在特定上下文中传递值，
	// 在其他存储参数的地方难以找到的情况下使用。它目前有三种用途：
	// 1. 当一个通道操作唤醒一个阻塞的 goroutine 时，它将 param 设置为已完成阻塞操作的 sudog。
	// 2. 由 gcAssistAlloc1 使用，向调用者信号表明 goroutine 已完成 GC 周期。
	//    以任何其他方式这样做都是不安全的，因为 goroutine 的栈可能在此期间移动。
	// 3. 由 debugCallWrap 使用，向新 goroutine 传递参数，因为在运行时中分配闭包是禁止的。
	param        unsafe.Pointer
	atomicstatus atomic.Uint32 // 用于原子操作的状态字段。
	stackLock    uint32        // sigprof/scang 锁；TODO: 合并进 atomicstatus。
	goid         uint64        // goroutine 的唯一标识符。
	schedlink    guintptr      // 用于链接 goroutine 的调度链。
	waitsince    int64         // goroutine 大约阻塞的时间。
	waitreason   waitReason    // 如果状态为 Gwaiting，阻塞的原因。

	preempt       bool // 抢占标志，如果需要抢占就将preempt设置为true
	preemptStop   bool // 表示在抢占时是否应过渡到 _Gpreempted 状态；否则，仅取消调度。
	preemptShrink bool // 表示是否应在同步安全点缩减栈。

	// 表示 g 是否停止在一个异步安全点。
	// 这意味着栈上有没有精确指针信息的帧。
	asyncSafePoint bool

	paniconfault bool // 表示是否应在遇到意外故障地址时引发 panic（而不是崩溃）。
	gcscandone   bool // 表示 g 已扫描栈；受 _Gscan 状态位保护。
	throwsplit   bool // 表示是否必须不拆分栈。
	// 表示是否有未锁定的通道指向此 goroutine 的栈。
	// 如果为真，则复制栈时需要获取通道锁以保护这些栈区域。
	activeStackChans bool

	// 表示 goroutine 即将停在 chansend 或 chanrecv 上。
	// 用于指示堆栈缩减的不安全点。
	parkingOnChan atomic.Bool

	raceignore    int8  // 表示是否忽略 race 检测事件。
	tracking      bool  // 表示是否正在为此 G 跟踪调度延迟统计信息。
	trackingSeq   uint8 // 用于决定是否跟踪此 G。
	trackingStamp int64 // 记录了 G 开始被跟踪的时间戳。
	runnableTime  int64 // 花费在可运行状态下的时间，当处于运行状态时清除，仅在跟踪时使用。

	lockedm    muintptr        // 表示 goroutine 正在哪个 m 上运行。
	sig        uint32          // 表示 goroutine 正在等待的信号。
	writebuf   []byte          // 是 goroutine 正在写入的缓冲区。
	sigcode0   uintptr         // 是信号的附加信息。
	sigcode1   uintptr         // 是信号的附加信息。
	sigpc      uintptr         // 是触发信号的 PC。
	parentGoid uint64          // 是创建此 goroutine 的 goroutine 的 goid。
	gopc       uintptr         // 是创建此 goroutine 的 go 语句的 PC。
	ancestors  *[]ancestorInfo // 是创建此 goroutine 的 goroutine 的祖先信息。
	startpc    uintptr         // 是 goroutine 函数的 PC。
	racectx    uintptr         // 用于 race 检测的上下文。
	waiting    *sudog          // 是此 g 正在等待的 sudog 结构（具有有效的 elem 指针）；按锁顺序排列。
	cgoCtxt    []uintptr       // 是 Cgo 调用栈回溯上下文。
	labels     unsafe.Pointer  // 是用于配置文件的标签。
	timer      *timer          // 是 time.Sleep 缓存的计时器。
	selectDone atomic.Uint32   // 表示我们是否参与了 select 并且有人赢得了竞争。

	// 表示此 goroutine 的栈在当前正在进行的 goroutine 配置文件中的状态。
	goroutineProfiled goroutineProfileStateHolder

	// 每个 G 的追踪器状态。
	trace gTraceState

	// 每个 G 的 GC 状态

	//  是此 G 的 GC 辅助信用，以分配的字节数表示。
	// 如果为正，则 G 有信用分配 gcAssistBytes 字节而不进行辅助。
	// 如果为负，则 G 必须通过执行扫描工作来纠正这一点。
	// 我们以字节跟踪这个，以便在分配热点路径中快速更新和检查债务。
	// 辅助比率确定这对应于多少扫描工作债务。
	gcAssistBytes int64
}

// gTrackingPeriod is the number of transitions out of _Grunning between
// latency tracking runs.
const gTrackingPeriod = 8

const (
	// tlsSlots is the number of pointer-sized slots reserved for TLS on some platforms,
	// like Windows.
	tlsSlots = 6
	tlsSize  = tlsSlots * goarch.PtrSize
)

// Values for m.freeWait.
const (
	freeMStack = 0 // M done, free stack and reference.
	freeMRef   = 1 // M done, free reference.
	freeMWait  = 2 // M still in use.
)

// 代表 OS 线程的重要数据结构
type m struct {
	g0      *g     // g0 帮 M 处理大小事务的goroutine，他是 m 中的第一个 goroutine
	morebuf gobuf  // 当 goroutine 需要更多栈空间时，此 gobuf 结构体用于传递参数给 morestack 函数。
	divmod  uint32 // ARM 架构下除法和模运算的分母，liblink 了解此字段。`
	_       uint32 // 用于对齐下一个字段至 8 字节

	procid     uint64            // 调试器使用的进程 ID，但偏移量不是硬编码的，便于跨平台兼容。
	gsignal    *g                // 信号处理 goroutine
	goSigStack gsignalStack      // Go 分配的信号处理栈
	sigmask    sigset            // 存储保存的信号掩码
	tls        [tlsSlots]uintptr // 线程私有空间
	mstartfn   func()            // m 启动函数

	curg      *g       // 当前运行的 goroutine
	caughtsig guintptr // 被致命信号捕获时运行的 goroutine。

	p     puintptr // 当前正在运行的p(处理器), 用于执行 Go 代码。
	nextp puintptr // 暂存的p
	oldp  puintptr // 执行系统调用之前的p

	id int64 // m 的唯一标识符。

	mallocing  int32     // 分配内存的计数器。
	throwing   throwType // 抛出异常的类型。
	preemptoff string    // 如果 if != "" 非空字符串，保持 curg 在此 m 上运行。
	locks      int32     // 锁计数器。
	dying      int32     // 死亡标志。
	profilehz  int32     // 用于 CPU profiling 的采样频率。

	spinning    bool          // 表示当前m没有goroutine了，正在从其他m偷取goroutine
	blocked     bool          // 表示 m 是否被阻塞在等待通知（note）上。
	newSigstack bool          // 如果为真，表示 m 的初始化函数 minit 已经在 C 线程上调用了 sigaltstack，用于设置信号处理栈。
	printlock   int8          // 打印锁。
	incgo       bool          // m 是否正在执行 cgo 调用。
	isextra     bool          // m 是否是额外的 m。
	isExtraInC  bool          // m 是否是额外的 m 且未执行 Go 代码。
	freeWait    atomic.Uint32 // 原子标记，表示是否安全删除 g0 和 m。

	fastrand      uint64        // 快速随机数生成器的内部状态。
	needextram    bool          // 是否需要额外的 m。
	traceback     uint8         // traceback 级别。
	ncgocall      uint64        // 总共的 cgo 调用次数。
	ncgo          int32         // 当前正在进行的 cgo 调用次数。
	cgoCallersUse atomic.Uint32 // 如果非零，cgoCallers 正在临时使用中。
	cgoCallers    *cgoCallers   // cgo 调用的 traceback。
	park          note          // 一个 note 结构，用于 m 的阻塞和唤醒。
	alllink       *m            // 所有m的链表
	schedlink     muintptr      // 在调度器上的链接。
	lockedg       guintptr      // 被锁定的 goroutine。
	createstack   [32]uintptr   // 创建线程的栈。
	lockedExt     uint32        // 外部 LockOSThread 的跟踪。
	lockedInt     uint32        // 内部 lockOSThread 的跟踪。
	nextwaitm     muintptr      // 下一个等待锁的 m。

	// 从 gopark 传入 park_m 的参数。
	waitunlockf          func(*g, unsafe.Pointer) bool
	waitlock             unsafe.Pointer
	waitTraceBlockReason traceBlockReason
	waitTraceSkip        int

	syscalltick uint32      // 用于系统调用的 tick 计数。
	freelink    *m          // 在 sched.freem 列表上的链接。
	trace       mTraceState // m 的追踪状态，用于性能分析。

	// 低级别 NOSPLIT 函数的 libcall 数据。
	libcall   libcall
	libcallpc uintptr // for cpu profiler
	libcallsp uintptr
	libcallg  guintptr
	syscall   libcall // 用于存储 Windows 上系统调用参数的 libcall。

	// 在 VDSO 调用期间用于 traceback 的栈指针和程序计数器。
	vdsoSP uintptr // 在VDSO调用时用于回溯的SP (如果不在通话中，则为0)
	vdsoPC uintptr // 在VDSO调用中用于追溯的PC

	preemptGen    atomic.Uint32 // 预抢占信号完成计数。
	signalPending atomic.Uint32 // 是否有未决的预抢占信号。

	dlogPerM // 日志记录每 m 的数据。

	mOS // OS 特定的 m 数据。

	// m 持有的锁信息，用于锁排名算法，最多可以记录 10 个锁。
	locksHeldLen int
	locksHeld    [10]heldLockInfo
}

// 一个调度 m 线程执行 goroutine 的处理器
type p struct {
	id          int32      // P 的唯一标识符，用于区分不同的 P 实例。
	status      uint32     // P 的当前状态，例如 pidle 表示 P 处于空闲状态，prunning 表示 P 正在运行。
	link        puintptr   // 用于连接 P 到其他 P 的双向链表节点。
	schedtick   uint32     // 每次调度器调用时递增。
	syscalltick uint32     // 每次系统调用时递增。
	sysmontick  sysmontick // 最后由 sysmon 观察到的 tick 值。
	m           muintptr   // 指向与 P 关联的 m 结构体的指针，m 是操作系统线程的抽象。
	mcache      *mcache    // mcache 结构，用于缓存分配器的内存。
	pcache      pageCache  // 页缓存，用于管理内存页的分配和回收。
	raceprocctx uintptr    // race 检测相关的上下文。

	// 可用的 defer 结构池，用于延迟函数调用。
	deferpool    []*_defer
	deferpoolbuf [32]*_defer

	// goroutine ID 缓存，用于加速访问 runtime·sched.goidgen。
	goidcache    uint64
	goidcacheend uint64

	// 定义了可运行 goroutine 的队列，用于存储等待执行的 goroutine。
	runqhead uint32        // p本地goroutine队列的头
	runqtail uint32        // p本地goroutine队列的尾
	runq     [256]guintptr // 队列指针，和sync.pool中数据结构一样也是循环队列
	runnext  guintptr      // 指向下一个应该运行的 goroutine 的指针，用于优化通信和等待模式的调度。

	// G 死亡状态（Gdead）的 goroutine 的缓存列表。
	gFree struct {
		gList
		n int32
	}

	// sudog 缓存，系统调用相关的 goroutine 缓存
	sudogcache []*sudog    // sudog缓存，channel用的
	sudogbuf   [128]*sudog // 也是防止false sharing

	// mspan 对象缓存，用于更高效的内存管理。
	mspancache struct {
		len int
		buf [128]*mspan
	}

	pinnerCache *pinner // pinner 对象缓存，减少重复创建的开销。

	trace pTraceState // 跟踪状态。

	palloc persistentAlloc // 持久化分配器，用于避免互斥锁。

	// 定时器堆的第一个条目的 when 字段值。
	// 如果定时器堆为空，则为 0。
	timer0When atomic.Int64

	// 最早已知的具有 timerModifiedEarlier 状态的定时器的 nextwhen 字段值。
	// 如果没有这样的定时器则为 0。
	timerModifiedEarliest atomic.Int64

	// GC 协助时间和分段标记阶段所花费的时间统计。
	gcAssistTime         int64 // 协助分配器花费的时间
	gcFractionalMarkTime int64 // 分段标记阶段花费的时间

	limiterEvent limiterEvent // GC CPU 限制器事件跟踪。

	gcMarkWorkerMode      gcMarkWorkerMode // GC 标记工作者模式。
	gcMarkWorkerStartTime int64            // 标记工作者开始时间

	// GC 工作缓冲区和写屏障缓冲区，用于优化垃圾回收过程。
	gcw   gcWork // GC 工作缓冲区缓存。
	wbBuf wbBuf  // GC 写屏障缓冲区。

	runSafePointFn uint32 // 如果为 1，在下一个安全点运行 sched.safePointFn。

	statsSeq atomic.Uint32 // 统计序列计数器。

	// 定时器相关的锁、列表、计数器，用于实现 time 包的功能。
	timersLock    mutex         // 定时器锁。
	timers        []*timer      // 一些时间点的动作，用于实现标准库中的 time 包。
	numTimers     atomic.Uint32 // 定时器堆中的定时器数量。
	deletedTimers atomic.Uint32 // 定时器堆中的删除定时器数量。
	timerRaceCtx  uintptr       // 执行定时器函数时使用的 race 上下文。

	maxStackScanDelta int64 // 积累的堆栈扫描增量，表示活动 goroutine 的堆栈空间。

	// 当前 goroutine 在 GC 时间的统计信息。
	scannedStackSize uint64 // 通过此 P 扫描的堆栈大小
	scannedStacks    uint64 // 通过此 P 扫描的堆栈数量

	preempt bool // 如果设置，表示此 P 应尽快进入调度器。

	pageTraceBuf pageTraceBuf // 页面跟踪缓冲区，仅在启用了页面跟踪实验功能时使用。
}

// schedt 时间表是 Go 运行时中负责全局调度策略的关键数据结构。
// 用来保存P的状态信息和goroutine的全局运行队列
type schedt struct {
	goidgen atomic.Uint64 // 生成 goroutine ID 的原子计数器。

	// 用于控制网络 I/O 轮询的定时器。
	lastpoll  atomic.Int64 // 上一次网络轮询的时间戳，如果当前正在进行轮询，则为 0。
	pollUntil atomic.Int64 // 当前轮询应持续到的时间戳。

	lock mutex // 全局调度器锁，用于保护数据结构免受并发修改。

	// 维护空闲的M
	midle        muintptr     // 等待工作的空闲 m 的链表头。
	nmidle       int32        // 等待工作的空闲 m 的数量。
	nmidlelocked int32        // 锁定并等待工作的 m 的数量。
	mnext        int64        // 已创建的 m 的数量，以及下一个 M ID。
	maxmcount    int32        // 最多创建多少个M(10000)
	nmsys        int32        // 系统 m 的数量，不计入死锁检查。
	nmfreed      int64        // 累计已释放的 m 的数量。
	ngsys        atomic.Int32 // 系统级 goroutine 的数量，不受调度器的正常调度规则影响。

	// 维护空闲的P
	pidle        puintptr      // 空闲 p 的链表头。
	npidle       atomic.Int32  // 空闲 p 的数量。
	nmspinning   atomic.Int32  // 正在寻找工作的 m 的数量。
	needspinning atomic.Uint32 // 需要查找工作的标志，布尔值。必须持有 sched.lock 才能设置为 1。

	// 全局可运行队列和其大小，用于存储准备运行的 goroutine。
	// 1.Goroutine 主动让出执行权，例如调用 runtime.Gosched() 方法；
	// 2.Goroutine 执行的时间片用完，会被放入全局队列以便重新调度；
	// 3.某些系统调用（比如阻塞的网络操作）会导致 Goroutine暂时进入全局队列。
	runq     gQueue // 全局可运行队列。
	runqsize int32  // 全局可运行队列的大小。

	// 控制调度器的选择性禁用，允许暂停用户 goroutine 的调度。
	disable struct {
		user     bool   // 用户禁用用户 goroutine 的调度。
		runnable gQueue // 待运行的 Gs 队列。
		n        int32  // runnable 的长度。
	}

	// 全局缓存已经退出的goroutine链表，下次再创建的时候直接用
	// 全局死亡 G 的缓存，用于管理已死的 goroutine 的生命周期。
	gFree struct {
		lock    mutex // 保护锁。
		stack   gList // 有栈的 Gs。
		noStack gList // 无栈的 Gs。
		n       int32 // 缓存中的 G 的数量。
	}

	// 系统调用相关的 sudog 缓存和 defer 结构体池。
	// 中央 sudog 结构体缓存。
	sudoglock  mutex
	sudogcache *sudog
	// 中央可用 defer 结构体池。
	deferlock mutex
	deferpool *_defer

	freem *m // 是等待被释放的 m 的列表，当 m.exited 设置时。通过 m.freelink 链接。

	// GC、停止信号、安全点函数、CPU profiling 等相关状态和控制。
	gcwaiting     atomic.Bool // 标记 gc 是否正在等待运行。
	stopwait      int32       // 控制停止信号的等待计数。
	stopnote      note        // 停止信号的 note。
	sysmonwait    atomic.Bool // 标记 sysmon 是否正在等待。
	sysmonnote    note        // sysmon 的 note。
	safePointFn   func(*p)    // 在每个逻辑处理器（P）上的下一个 GC 安全点调用的函数，如果 p.runSafePointFn 被设置。
	safePointWait int32       // 表示等待安全点的计数器。
	safePointNote note        // 表示关于 GC 安全点的说明。
	profilehz     int32       // CPU profiling 速率。

	// 监控和统计信息，包括系统配置变化、调度延迟、互斥锁等待时间等。
	procresizetime     int64         // 上次更改 gomaxprocs 的时间。
	totaltime          int64         // 截至 procresizetime 的 gomaxprocs 的积分。
	sysmonlock         mutex         // 获取并保持此互斥锁以阻止 sysmon 与运行时的其余部分交互。
	timeToRun          timeHistogram // 是调度延迟的分布，G 在从 _Grunnable 状态转换到 _Grunning 状态前花费的时间
	idleTime           atomic.Int64  // 每个 GC 周期重置。已“花费”的总 CPU 时间，处于空闲状态。
	totalMutexWaitTime atomic.Int64  // 是 goroutines 在 _Gwaiting 状态下等待互斥锁的时间总和。
}

// Values for the flags field of a sigTabT.
const (
	_SigNotify   = 1 << iota // let signal.Notify have signal, even if from kernel
	_SigKill                 // if signal.Notify doesn't take it, exit quietly
	_SigThrow                // if signal.Notify doesn't take it, exit loudly
	_SigPanic                // if the signal is from the kernel, panic
	_SigDefault              // if the signal isn't explicitly requested, don't monitor it
	_SigGoExit               // cause all runtime procs to exit (only used on Plan 9).
	_SigSetStack             // Don't explicitly install handler, but add SA_ONSTACK to existing libc handler
	_SigUnblock              // always unblock; see blockableSig
	_SigIgn                  // _SIG_DFL action is to ignore the signal
)

// Layout of in-memory per-function information prepared by linker
// See https://golang.org/s/go12symtab.
// Keep in sync with linker (../cmd/link/internal/ld/pcln.go:/pclntab)
// and with package debug/gosym and with symtab.go in package runtime.
type _func struct {
	sys.NotInHeap // Only in static data

	entryOff uint32 // start pc, as offset from moduledata.text/pcHeader.textStart
	nameOff  int32  // function name, as index into moduledata.funcnametab.

	args        int32  // in/out args size
	deferreturn uint32 // offset of start of a deferreturn call instruction from entry, if any.

	pcsp      uint32
	pcfile    uint32
	pcln      uint32
	npcdata   uint32
	cuOffset  uint32     // runtime.cutab offset of this function's CU
	startLine int32      // line number of start of function (func keyword/TEXT directive)
	funcID    abi.FuncID // set for certain special runtime functions
	flag      abi.FuncFlag
	_         [1]byte // pad
	nfuncdata uint8   // must be last, must end on a uint32-aligned boundary

	// The end of the struct is followed immediately by two variable-length
	// arrays that reference the pcdata and funcdata locations for this
	// function.

	// pcdata contains the offset into moduledata.pctab for the start of
	// that index's table. e.g.,
	// &moduledata.pctab[_func.pcdata[_PCDATA_UnsafePoint]] is the start of
	// the unsafe point table.
	//
	// An offset of 0 indicates that there is no table.
	//
	// pcdata [npcdata]uint32

	// funcdata contains the offset past moduledata.gofunc which contains a
	// pointer to that index's funcdata. e.g.,
	// *(moduledata.gofunc +  _func.funcdata[_FUNCDATA_ArgsPointerMaps]) is
	// the argument pointer map.
	//
	// An offset of ^uint32(0) indicates that there is no entry.
	//
	// funcdata [nfuncdata]uint32
}

// Pseudo-Func that is returned for PCs that occur in inlined code.
// A *Func can be either a *_func or a *funcinl, and they are distinguished
// by the first uintptr.
//
// TODO(austin): Can we merge this with inlinedCall?
type funcinl struct {
	ones      uint32  // set to ^0 to distinguish from _func
	entry     uintptr // entry of the real (the "outermost") frame
	name      string
	file      string
	line      int32
	startLine int32
}

// Itab 的布局已知于编译器
// 在非垃圾回收内存中分配
// 需要与 ../cmd/compile/internal/reflectdata/reflect.go 文件中的 func.WriteTabs 函数保持同步
type itab struct {
	inter *interfacetype // 接口类型描述
	_type *_type         // 具体类型描述
	hash  uint32         // _type.hash 的副本，用于类型切换
	_     [4]byte        // 未使用的字节
	fun   [1]uintptr     // 变长字段。fun[0]==0 表示 _type 未实现接口
}

// Lock-free stack node.
// Also known to export_test.go.
type lfnode struct {
	next    uint64
	pushcnt uintptr
}

type forcegcstate struct {
	lock mutex
	g    *g
	idle atomic.Bool
}

// extendRandom extends the random numbers in r[:n] to the whole slice r.
// Treats n<0 as n==0.
func extendRandom(r []byte, n int) {
	if n < 0 {
		n = 0
	}
	for n < len(r) {
		// Extend random bits using hash function & time seed
		w := n
		if w > 16 {
			w = 16
		}
		h := memhash(unsafe.Pointer(&r[n-w]), uintptr(nanotime()), uintptr(w))
		for i := 0; i < goarch.PtrSize && n < len(r); i++ {
			r[n] = byte(h)
			n++
			h >>= 8
		}
	}
}

// A _defer holds an entry on the list of deferred calls.
// If you add a field here, add code to clear it in deferProcStack.
// This struct must match the code in cmd/compile/internal/ssagen/ssa.go:deferstruct
// and cmd/compile/internal/ssagen/ssa.go:(*state).call.
// Some defers will be allocated on the stack and some on the heap.
// All defers are logically part of the stack, so write barriers to
// initialize them are not required. All defers must be manually scanned,
// and for heap defers, marked.
type _defer struct {
	started bool
	heap    bool
	// openDefer indicates that this _defer is for a frame with open-coded
	// defers. We have only one defer record for the entire frame (which may
	// currently have 0, 1, or more defers active).
	openDefer bool
	sp        uintptr // sp at time of defer
	pc        uintptr // pc at time of defer
	fn        func()  // can be nil for open-coded defers
	_panic    *_panic // panic that is running defer
	link      *_defer // next defer on G; can point to either heap or stack!

	// If openDefer is true, the fields below record values about the stack
	// frame and associated function that has the open-coded defer(s). sp
	// above will be the sp for the frame, and pc will be address of the
	// deferreturn call in the function.
	fd   unsafe.Pointer // funcdata for the function associated with the frame
	varp uintptr        // value of varp for the stack frame
	// framepc is the current pc associated with the stack frame. Together,
	// with sp above (which is the sp associated with the stack frame),
	// framepc/sp can be used as pc/sp pair to continue a stack trace via
	// gentraceback().
	framepc uintptr
}

// A _panic holds information about an active panic.
//
// A _panic value must only ever live on the stack.
//
// The argp and link fields are stack pointers, but don't need special
// handling during stack growth: because they are pointer-typed and
// _panic values only live on the stack, regular stack pointer
// adjustment takes care of them.
type _panic struct {
	argp      unsafe.Pointer // pointer to arguments of deferred call run during panic; cannot move - known to liblink
	arg       any            // argument to panic
	link      *_panic        // link to earlier panic
	pc        uintptr        // where to return to in runtime if this panic is bypassed
	sp        unsafe.Pointer // where to return to in runtime if this panic is bypassed
	recovered bool           // whether this panic is over
	aborted   bool           // the panic was aborted
	goexit    bool
}

// ancestorInfo records details of where a goroutine was started.
type ancestorInfo struct {
	pcs  []uintptr // pcs from the stack of this goroutine
	goid uint64    // goroutine id of this goroutine; original goroutine possibly dead
	gopc uintptr   // pc of go statement that created this goroutine
}

// A waitReason explains why a goroutine has been stopped.
// See gopark. Do not re-use waitReasons, add new ones.
type waitReason uint8

const (
	waitReasonZero                  waitReason = iota // ""
	waitReasonGCAssistMarking                         // "GC assist marking"
	waitReasonIOWait                                  // "IO wait"
	waitReasonChanReceiveNilChan                      // "chan receive (nil chan)"
	waitReasonChanSendNilChan                         // "chan send (nil chan)"
	waitReasonDumpingHeap                             // "dumping heap"
	waitReasonGarbageCollection                       // "garbage collection"
	waitReasonGarbageCollectionScan                   // "garbage collection scan"
	waitReasonPanicWait                               // "panicwait"
	waitReasonSelect                                  // "select"
	waitReasonSelectNoCases                           // "select (no cases)"
	waitReasonGCAssistWait                            // "GC assist wait"
	waitReasonGCSweepWait                             // "GC sweep wait"
	waitReasonGCScavengeWait                          // "GC scavenge wait"
	waitReasonChanReceive                             // "chan receive"
	waitReasonChanSend                                // "chan send"
	waitReasonFinalizerWait                           // "finalizer wait"
	waitReasonForceGCIdle                             // "force gc (idle)"
	waitReasonSemacquire                              // "semacquire"
	waitReasonSleep                                   // "sleep"
	waitReasonSyncCondWait                            // "sync.Cond.Wait"
	waitReasonSyncMutexLock                           // "sync.Mutex.Lock"
	waitReasonSyncRWMutexRLock                        // "sync.RWMutex.RLock"
	waitReasonSyncRWMutexLock                         // "sync.RWMutex.Lock"
	waitReasonTraceReaderBlocked                      // "trace reader (blocked)"
	waitReasonWaitForGCCycle                          // "wait for GC cycle"
	waitReasonGCWorkerIdle                            // "GC worker (idle)"
	waitReasonGCWorkerActive                          // "GC worker (active)"
	waitReasonPreempted                               // "preempted"
	waitReasonDebugCall                               // "debug call"
	waitReasonGCMarkTermination                       // "GC mark termination"
	waitReasonStoppingTheWorld                        // "stopping the world"
)

var waitReasonStrings = [...]string{
	waitReasonZero:                  "",
	waitReasonGCAssistMarking:       "GC assist marking",
	waitReasonIOWait:                "IO wait",
	waitReasonChanReceiveNilChan:    "chan receive (nil chan)",
	waitReasonChanSendNilChan:       "chan send (nil chan)",
	waitReasonDumpingHeap:           "dumping heap",
	waitReasonGarbageCollection:     "garbage collection",
	waitReasonGarbageCollectionScan: "garbage collection scan",
	waitReasonPanicWait:             "panicwait",
	waitReasonSelect:                "select",
	waitReasonSelectNoCases:         "select (no cases)",
	waitReasonGCAssistWait:          "GC assist wait",
	waitReasonGCSweepWait:           "GC sweep wait",
	waitReasonGCScavengeWait:        "GC scavenge wait",
	waitReasonChanReceive:           "chan receive",
	waitReasonChanSend:              "chan send",
	waitReasonFinalizerWait:         "finalizer wait",
	waitReasonForceGCIdle:           "force gc (idle)",
	waitReasonSemacquire:            "semacquire",
	waitReasonSleep:                 "sleep",
	waitReasonSyncCondWait:          "sync.Cond.Wait",
	waitReasonSyncMutexLock:         "sync.Mutex.Lock",
	waitReasonSyncRWMutexRLock:      "sync.RWMutex.RLock",
	waitReasonSyncRWMutexLock:       "sync.RWMutex.Lock",
	waitReasonTraceReaderBlocked:    "trace reader (blocked)",
	waitReasonWaitForGCCycle:        "wait for GC cycle",
	waitReasonGCWorkerIdle:          "GC worker (idle)",
	waitReasonGCWorkerActive:        "GC worker (active)",
	waitReasonPreempted:             "preempted",
	waitReasonDebugCall:             "debug call",
	waitReasonGCMarkTermination:     "GC mark termination",
	waitReasonStoppingTheWorld:      "stopping the world",
}

func (w waitReason) String() string {
	if w < 0 || w >= waitReason(len(waitReasonStrings)) {
		return "unknown wait reason"
	}
	return waitReasonStrings[w]
}

func (w waitReason) isMutexWait() bool {
	return w == waitReasonSyncMutexLock ||
		w == waitReasonSyncRWMutexRLock ||
		w == waitReasonSyncRWMutexLock
}

var (
	allm       *m           // 所有 m（操作系统线程）的链表。所有的m构成的一个链表
	gomaxprocs int32        // p的最大值，默认等于ncpu，但可以通过 runtime.GOMAXPROCS 修改
	ncpu       int32        // 系统中cpu核的数量，程序启动时由 runtime.osinit 代码初始化
	forcegc    forcegcstate // 强制 GC 状态，用于控制强制触发垃圾回收。
	sched      schedt       // 调度器数据结构，负责全局调度策略。
	newprocs   int32        // 新建的处理器数量，用于动态调整 gomaxprocs。

	allpLock mutex // 保护对 all 的 P 少操作和大小变更。也保护对 allp 的所有写入操作。
	allp     []*p  // 所有 p（逻辑处理器）的数组，长度等于 gomaxprocs

	// 空闲 p 的位图，每一位对应一个 P。读写必须是原子的。
	// 每个 P 必须只更新自己的位。为了保持一致性，一个 P 变为空闲时，
	// 必须同时更新 idlepMask 和 idle P 列表（在 sched.lock 下），否则，
	// 并发的 pidleget 可能在 pidleput 设置 mask 前清除 mask，破坏位图。
	// 注意，procresize 在 stopTheWorldWithSema 中接管所有 P 的所有权。
	idlepMask pMask

	// 可能有定时器的 P 的位图，每一位对应一个 P。读写必须是原子的。
	// 长度可能在安全点处变更。
	timerpMask pMask

	// GC 背景标记工作者的池，条目类型为 *gcBgMarkWorkerNode。
	gcBgMarkWorkerPool lfstack

	// goroutines 的总数。由 worldsema 保护。
	gcBgMarkWorkerCount int32

	// 关于可用 CPU 特性的信息。
	// 运行时之外的包不应使用这些，因为它们不属于外部 API。
	// 在启动时由 asm_{386,amd64}.s 设置。
	processorVersionInfo uint32
	isIntel              bool

	goarm uint8 // ARM 系统上由 cmd/link 设置的目标架构。
)

// Set by the linker so the runtime can determine the buildmode.
var (
	islibrary bool // -buildmode=c-shared
	isarchive bool // -buildmode=c-archive
)

// Must agree with internal/buildcfg.FramePointerEnabled.
const framepointer_enabled = GOARCH == "amd64" || GOARCH == "arm64"
