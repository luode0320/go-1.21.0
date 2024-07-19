// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package context defines the Context type, which carries deadlines,
// cancellation signals, and other request-scoped values across API boundaries
// and between processes.
//
// Incoming requests to a server should create a [Context], and outgoing
// calls to servers should accept a Context. The chain of function
// calls between them must propagate the Context, optionally replacing
// it with a derived Context created using [WithCancel], [WithDeadline],
// [WithTimeout], or [WithValue]. When a Context is canceled, all
// Contexts derived from it are also canceled.
//
// The [WithCancel], [WithDeadline], and [WithTimeout] functions take a
// Context (the parent) and return a derived Context (the child) and a
// [CancelFunc]. Calling the CancelFunc cancels the child and its
// children, removes the parent's reference to the child, and stops
// any associated timers. Failing to call the CancelFunc leaks the
// child and its children until the parent is canceled or the timer
// fires. The go vet tool checks that CancelFuncs are used on all
// control-flow paths.
//
// The [WithCancelCause] function returns a [CancelCauseFunc], which
// takes an error and records it as the cancellation cause. Calling
// [Cause] on the canceled context or any of its children retrieves
// the cause. If no cause is specified, Cause(ctx) returns the same
// value as ctx.Err().
//
// Programs that use Contexts should follow these rules to keep interfaces
// consistent across packages and enable static analysis tools to check context
// propagation:
//
// Do not store Contexts inside a struct type; instead, pass a Context
// explicitly to each function that needs it. The Context should be the first
// parameter, typically named ctx:
//
//	func DoSomething(ctx context.Context, arg Arg) error {
//		// ... use ctx ...
//	}
//
// Do not pass a nil [Context], even if a function permits it. Pass [context.TODO]
// if you are unsure about which Context to use.
//
// Use context Values only for request-scoped data that transits processes and
// APIs, not for passing optional parameters to functions.
//
// The same Context may be passed to functions running in different goroutines;
// Contexts are safe for simultaneous use by multiple goroutines.
//
// See https://blog.golang.org/context for example code for a server that uses
// Contexts.
package context

import (
	"errors"
	"internal/reflectlite"
	"sync"
	"sync/atomic"
	"time"
)

// Context 接口定义了一个上下文，用于在 API 之间传递截止时间、取消信号和其他值。
//
// Context 的方法可以被多个 goroutines 同时调用。
type Context interface {
	// Deadline 返回一个截止时间，如果没有设置会返回false
	// - 通过设置之这个截止时间, 定时任务控制这个上下文通道在哪个时间点定时关闭通道
	Deadline() (deadline time.Time, ok bool)

	// Done 返回一个 channel 通道，可以表示 context 被取消的信号
	// - 当这个 channel 被关闭时，说明 context 被取消了。注意，这是一个只读的channel。
	// - 我们又知道，读一个未关闭的通道会阻塞(配合select可用快速失败非阻塞), 读一个关闭的 channel 会读出相应类型的零值。
	// - 因此在子协程里读这个 channel，除非被关闭，否则会直接快速失败, 读不到任何东西。
	// - 也正是利用了这一点，子协程从 channel 里读出了值（零值）后，就可以做一些收尾工作，尽快退出。
	Done() <-chan struct{}

	// Err 快速检测是否已经关闭通道
	// - 如果通道成功取消, 会返回一个真正的error错误
	Err() error

	// Value 获取之前设置的 key 对应的 value。
	Value(key any) any
}

// Canceled 取消上下文时 [Context.Err] 返回的错误。
var Canceled = errors.New("context canceled")

// DeadlineExceeded is the error returned by [Context.Err] when the context's
// deadline passes.
var DeadlineExceeded error = deadlineExceededError{}

type deadlineExceededError struct{}

func (deadlineExceededError) Error() string   { return "context deadline exceeded" }
func (deadlineExceededError) Timeout() bool   { return true }
func (deadlineExceededError) Temporary() bool { return true }

// emptyCtx 实现了 Context 接口, 但是实现的方法都返回默认值, 没有超时、没有通道、没有错误、没有kv
// 它是 backgroundCtx 和 todoCtx 的基础结构。
type emptyCtx struct{}

func (emptyCtx) Deadline() (deadline time.Time, ok bool) {
	return
}

func (emptyCtx) Done() <-chan struct{} {
	return nil
}

func (emptyCtx) Err() error {
	return nil
}

func (emptyCtx) Value(key any) any {
	return nil
}

// 一个空的context
type backgroundCtx struct{ emptyCtx }

// 一个name
func (backgroundCtx) String() string {
	return "context.Background"
}

// Background 返回一个非 nil 的空 Context。
// 实现了 Context 接口, 但是实现的方法都返回默认值, 没有超时、没有通道、没有错误、没有kv
func Background() Context {
	return backgroundCtx{}
}

// 一个空的context
type todoCtx struct{ emptyCtx }

// 一个name
func (todoCtx) String() string {
	return "context.TODO"
}

// TODO 返回一个非 nil 的空 Context。
// 实现了 Context 接口, 但是实现的方法都返回默认值, 没有超时、没有通道、没有错误、没有kv
// 当不清楚要使用哪个 Context 或者尚未可用时，代码应该使用 context.TODO。
func TODO() Context {
	return todoCtx{}
}

// A CancelFunc tells an operation to abandon its work.
// A CancelFunc does not wait for the work to stop.
// A CancelFunc may be called by multiple goroutines simultaneously.
// After the first call, subsequent calls to a CancelFunc do nothing.
type CancelFunc func()

// WithCancel 返回具有新 Done 完成通道的 parent 的副本。
// 当调用返回的 cancel 回调函数或父上下文的 Done 完成通道被关闭时，返回上下文的 Done 完成通道会被关闭。
func WithCancel(parent Context) (ctx Context, cancel CancelFunc) {
	// 创建一个可关闭通道的上下文 cancelCtx
	c := withCancel(parent)
	// 返回一个ctx上下文, 和一个关闭通道的回调
	return c, func() { c.cancel(true, Canceled, nil) }
}

// A CancelCauseFunc behaves like a [CancelFunc] but additionally sets the cancellation cause.
// This cause can be retrieved by calling [Cause] on the canceled Context or on
// any of its derived Contexts.
//
// If the context has already been canceled, CancelCauseFunc does not set the cause.
// For example, if childContext is derived from parentContext:
//   - if parentContext is canceled with cause1 before childContext is canceled with cause2,
//     then Cause(parentContext) == Cause(childContext) == cause1
//   - if childContext is canceled with cause2 before parentContext is canceled with cause1,
//     then Cause(parentContext) == cause1 and Cause(childContext) == cause2
type CancelCauseFunc func(cause error)

// WithCancelCause behaves like [WithCancel] but returns a [CancelCauseFunc] instead of a [CancelFunc].
// Calling cancel with a non-nil error (the "cause") records that error in ctx;
// it can then be retrieved using Cause(ctx).
// Calling cancel with nil sets the cause to Canceled.
//
// Example use:
//
//	ctx, cancel := context.WithCancelCause(parent)
//	cancel(myError)
//	ctx.Err() // returns context.Canceled
//	context.Cause(ctx) // returns myError
func WithCancelCause(parent Context) (ctx Context, cancel CancelCauseFunc) {
	c := withCancel(parent)
	return c, func(cause error) { c.cancel(true, Canceled, cause) }
}

// 创建一个可关闭通道的上下文 cancelCtx
func withCancel(parent Context) *cancelCtx {
	// 父级上下文不能为 nil
	if parent == nil {
		panic("cannot create context from nil parent")
	}

	// 创建一个新的 cancelCtx 可关闭通道的上下文
	c := &cancelCtx{}
	// 将这个关闭通道的上下文 c , 挂在父级上下文的上下文链上
	c.propagateCancel(parent, c)
	return c
}

// Cause returns a non-nil error explaining why c was canceled.
// The first cancellation of c or one of its parents sets the cause.
// If that cancellation happened via a call to CancelCauseFunc(err),
// then [Cause] returns err.
// Otherwise Cause(c) returns the same value as c.Err().
// Cause returns nil if c has not been canceled yet.
func Cause(c Context) error {
	if cc, ok := c.Value(&cancelCtxKey).(*cancelCtx); ok {
		cc.mu.Lock()
		defer cc.mu.Unlock()
		return cc.cause
	}
	return nil
}

// AfterFunc arranges to call f in its own goroutine after ctx is done
// (cancelled or timed out).
// If ctx is already done, AfterFunc calls f immediately in its own goroutine.
//
// Multiple calls to AfterFunc on a context operate independently;
// one does not replace another.
//
// Calling the returned stop function stops the association of ctx with f.
// It returns true if the call stopped f from being run.
// If stop returns false,
// either the context is done and f has been started in its own goroutine;
// or f was already stopped.
// The stop function does not wait for f to complete before returning.
// If the caller needs to know whether f is completed,
// it must coordinate with f explicitly.
//
// If ctx has a "AfterFunc(func()) func() bool" method,
// AfterFunc will use it to schedule the call.
func AfterFunc(ctx Context, f func()) (stop func() bool) {
	a := &afterFuncCtx{
		f: f,
	}

	// 如果父级上下文此时如果取消了, 子上下文也同时取消
	a.cancelCtx.propagateCancel(ctx, a)
	return func() bool {
		stopped := false
		a.once.Do(func() {
			stopped = true
		})
		if stopped {
			a.cancel(true, Canceled, nil)
		}
		return stopped
	}
}

type afterFuncer interface {
	AfterFunc(func()) func() bool
}

type afterFuncCtx struct {
	cancelCtx
	once sync.Once // either starts running f or stops f from running
	f    func()
}

func (a *afterFuncCtx) cancel(removeFromParent bool, err, cause error) {
	a.cancelCtx.cancel(false, err, cause)
	if removeFromParent {
		removeChild(a.Context, a)
	}
	a.once.Do(func() {
		go a.f()
	})
}

// A stopCtx is used as the parent context of a cancelCtx when
// an AfterFunc has been registered with the parent.
// It holds the stop function used to unregister the AfterFunc.
type stopCtx struct {
	Context
	stop func() bool
}

// 计算曾经创建的goroutines的数量; 用于测试。
var goroutines atomic.Int32

// &cancelCtxKey 是cancelCtx为其返回自身的键
// 这是一个全局变量, 它一定是0, 没有人会修改它, 由它查询key。
var cancelCtxKey int

// 函数断言 parent 上下文是一个 cancelCtx 带取消的上下文类型
// cancelCtx和继承了cancelCtx的timerCtx都属于一个 cancelCtx 带取消的上下文类型
func parentCancelCtx(parent Context) (*cancelCtx, bool) {
	// 获取父 Context 上下文的通道。
	done := parent.Done()

	// 如果 Done 上下文通道是 closedchan（一个已关闭的空通道）或者是 nil，
	// 说明这是一个根 Context 或者无效的 Context 上下文，直接返回
	if done == closedchan || done == nil {
		return nil, false
	}

	// 使用 &cancelCtxKey 作为键查询 value
	// 作用是验证当前 parent 上下文是否是 cancelCtx 类型的, 无其他含义
	// Value(): 返回的也是 parent 本身
	// 感觉应该是想做扩展的, 但是似乎没做, 正常就是一个类型断言
	p, ok := parent.Value(&cancelCtxKey).(*cancelCtx)
	if !ok {
		return nil, false
	}

	// 使用原子操作加载 *cancelCtx 的 done 通道。
	// Load 方法返回通道的引用，如果通道已经被关闭或者不存在，会返回默认值。
	pdone, _ := p.done.Load().(chan struct{})

	// 检查父 Context 的 Done 通道是否与 *cancelCtx 的 done 通道相匹配。
	// 如果不匹配，说明父 Context 被自定义的 Context 实现所包装，不应绕过。
	if pdone != done {
		return nil, false
	}

	return p, true
}

// 从其父级中移除上下文。
func removeChild(parent Context, child canceler) {
	if s, ok := parent.(stopCtx); ok {
		s.stop()
		return
	}

	// 函数断言 parent 上下文是一个 cancelCtx 带取消的上下文类型
	// cancelCtx和继承了cancelCtx的timerCtx都属于一个 cancelCtx 带取消的上下文类型
	p, ok := parentCancelCtx(parent)
	if !ok {
		return
	}

	p.mu.Lock()
	if p.children != nil {
		delete(p.children, child)
	}
	p.mu.Unlock()
}

// canceler 是一个可以直接取消的上下文类型。其实现包括 *cancelCtx 和 *timerCtx。
type canceler interface {
	// 可以用于直接取消上下文。如果 removeFromParent 为 true，会从父上下文中移除当前上下文。
	// err 表示取消的原因，cause 表示导致取消的根本原因。
	cancel(removeFromParent bool, err, cause error)
	// Done 返回一个 channel 通道，可以表示 context 被取消的信号
	Done() <-chan struct{}
}

// 一个可重复使用的已经关闭的通道。
var closedchan = make(chan struct{})

// 将通道初始化未关闭状态
func init() {
	close(closedchan)
}

// 可以被取消。当取消时，它也会取消任何实现了 canceler 接口的子上下文。
type cancelCtx struct {
	Context                        // 继承通用上下文接口, cancelCtx 需要实现通用上下文的接口方法
	mu       sync.Mutex            // 同步锁, 用于保护以下字段
	done     atomic.Value          // 原子操作保存通道, Done()创建 chan struct{} 空通道保存在这里, 用于取消
	children map[canceler]struct{} // 在第一次取消调用时设为 nil
	err      error                 // 在第一次取消调用时设置为非 nil
	cause    error                 // 在第一次取消调用时设置为非 nil
}

// Value 返回与此上下文关联的键的值，如果没有与键关联的值，则返回 nil。
func (c *cancelCtx) Value(key any) any {
	// 如果 key 是 cancelCtxKey，则返回当前 cancelCtx 实例本身
	if key == &cancelCtxKey {
		return c
	}
	// 否则，调用父上下文的 Value 方法继续查找 key 的值并返回
	return value(c.Context, key)
}

// Done 返回一个 channel 通道，可以表示 context 被取消的信号
func (c *cancelCtx) Done() <-chan struct{} {
	// 尝试加载已存在的 done 通道
	d := c.done.Load()
	if d != nil {
		return d.(chan struct{})
	}

	// 以下为处理当 done 通道尚未创建时的逻辑
	c.mu.Lock()
	defer c.mu.Unlock()

	// 再次加载 done 通道，避免竞态条件
	d = c.done.Load()
	if d == nil {
		// 创建一个新的无缓冲的空通道
		d = make(chan struct{})
		// 将新的通道存储到 done 字段中
		c.done.Store(d)
	}

	// 返回 done 通道
	return d.(chan struct{})
}

// Err 返回 ctx 上下文是否被取消, 如果没有取消会返回一个错误
func (c *cancelCtx) Err() error {
	c.mu.Lock()

	// c.err 是在 Context 被取消时设置的
	err := c.err

	c.mu.Unlock()
	return err
}

// 将这个带关闭通道功能的上下文 child, 挂在父级上下文 parent 的上下文链上
// 如果父级上下文此时如果取消了, 子上下文也同时取消
func (c *cancelCtx) propagateCancel(parent Context, child canceler) {
	// 将当前关闭通道的上下文实例的 Context, 赋值为父级引用
	c.Context = parent

	// 获取父级上下文的通道
	// 如果父级通道为 nil, 则不需要
	done := parent.Done()
	if done == nil {
		return
	}

	select {
	case <-done:
		// 父级上下文此时如果取消了, 子上下文也取消
		// removeFromParent == false: 但是不从父节点中移除自己, 因为只要这条链上的前面的断开就可以了
		// 所以, 如果父级取消了, 从链式移除, 剩下的父级的子级都会从整条上下文链上移除
		child.cancel(false, parent.Err(), Cause(parent))
		return
	default:
	}

	// 函数断言 parent 上下文是一个 cancelCtx 带取消的上下文类型
	// cancelCtx和继承了cancelCtx的timerCtx都属于一个 cancelCtx 带取消的上下文类型
	if p, ok := parentCancelCtx(parent); ok { // 断言
		p.mu.Lock()

		// err!=nil: 说明上下文成功取消,关闭通道了
		if p.err != nil {
			// 父级上下文此时如果取消了, 子上下文也取消
			// removeFromParent == false: 但是不从父节点中移除自己, 因为只要这条链上的前面的断开就可以了
			// 所以, 如果父级取消了, 从链式移除, 剩下的父级的子级都会从整条上下文链上移除
			child.cancel(false, p.err, p.cause)
		} else {
			// 上下文没有被取消
			if p.children == nil {
				p.children = make(map[canceler]struct{})
			}
			//  将当前上下文挂在父级上下文的 children 子级集合中
			p.children[child] = struct{}{}
		}

		p.mu.Unlock()
		return
	}

	// 父上下文实现了 AfterFunc 方法。
	// 只有 afterFuncCtx 类型的上下文才实现了这个方法 afterFuncer
	if a, ok := parent.(afterFuncer); ok {
		c.mu.Lock()

		// 在初始化给的一个回调函数执行之后再关闭通道, 取消上下文
		stop := a.AfterFunc(func() {
			child.cancel(false, parent.Err(), Cause(parent))
		})
		c.Context = stopCtx{
			Context: parent,
			stop:    stop,
		}

		c.mu.Unlock()
		return
	}

	// 计算曾经创建的过goroutine的数量; 用于测试, 没用。
	goroutines.Add(1)

	// 开一个协程, 专门检测父上下文是否取消, 如果取消, 它的子上下文也取消
	go func() {
		select {
		case <-parent.Done():
			// 父上下文已经被取消,子上下文也取消
			child.cancel(false, parent.Err(), Cause(parent))
		case <-child.Done():
		}
	}()
}

type stringer interface {
	String() string
}

func contextName(c Context) string {
	if s, ok := c.(stringer); ok {
		return s.String()
	}
	return reflectlite.TypeOf(c).String()
}

func (c *cancelCtx) String() string {
	return contextName(c.Context) + ".WithCancel"
}

// 关闭 channel 通道
// removeFromParent: 表示是否需要从父级取消器的子级列表中移除自己。
// err: 取消错误，通常是一个 context.Canceled 或 context.DeadlineExceeded 错误。
// cause: 取消的直接原因，可以是 nil。
func (c *cancelCtx) cancel(removeFromParent bool, err, cause error) {
	// 必须要传 err,说明关闭通道的原因
	if err == nil {
		panic("context: internal error: missing cancel error")
	}

	// 关闭通道的原因
	if cause == nil {
		cause = err
	}

	c.mu.Lock()

	// 通道已经被关闭, 上下文取消了
	if c.err != nil {
		c.mu.Unlock()
		return
	}

	// 赋值关闭通道的原因, 以便通过 Err() 方法获取
	c.err = err
	c.cause = cause

	// 获取通道
	d, _ := c.done.Load().(chan struct{})
	if d == nil {
		c.done.Store(closedchan) // 一个可重复使用的已经关闭的通道。
	} else {
		// 如果有直接触发关闭通道
		close(d)
	}

	for child := range c.children {
		child.cancel(false, err, cause) // 递归关闭当前上下文的所有子上下文
	}
	// 清空子上下文
	c.children = nil

	c.mu.Unlock()

	if removeFromParent {
		// 从父节点中移除自己
		removeChild(c.Context, c) // 从父级上下文移除当前上下文
	}
}

// WithoutCancel returns a copy of parent that is not canceled when parent is canceled.
// The returned context returns no Deadline or Err, and its Done channel is nil.
// Calling [Cause] on the returned context returns nil.
func WithoutCancel(parent Context) Context {
	if parent == nil {
		panic("cannot create context from nil parent")
	}
	return withoutCancelCtx{parent}
}

type withoutCancelCtx struct {
	c Context
}

func (withoutCancelCtx) Deadline() (deadline time.Time, ok bool) {
	return
}

func (withoutCancelCtx) Done() <-chan struct{} {
	return nil
}

func (withoutCancelCtx) Err() error {
	return nil
}

func (c withoutCancelCtx) Value(key any) any {
	return value(c, key)
}

func (c withoutCancelCtx) String() string {
	return contextName(c.c) + ".WithoutCancel"
}

// WithDeadline 返回指定时间自动取消的上下文,和一个可以手动取消上下文的函数
func WithDeadline(parent Context, d time.Time) (Context, CancelFunc) {
	// parent: 父级上下文
	// time: 未来的一个新时间点
	return WithDeadlineCause(parent, d, nil)
}

// WithDeadlineCause 类似于 WithDeadline，但还在截止时间超过时设置返回的 Context 的原因。
// 参数:
// parent: 父级上下文
// d: 未来的一个新时间点
// cause: 取消上下文的原因日志
func WithDeadlineCause(parent Context, d time.Time, cause error) (Context, CancelFunc) {
	// 初始化的 ctx 不能为 nil
	if parent == nil {
		panic("cannot create context from nil parent")
	}

	// 如果父级上下文有截止时间, 并且父级的截止时间要早于此刻想设置的时间
	// 则可以创建一个不带取消的上下文, 因为如果父级超时了, 会同步将当前这个子上下文也取消的
	if cur, ok := parent.Deadline(); ok && cur.Before(d) {
		return WithCancel(parent) // 返回一个正常的ctx
	}

	// 创建一个带截止时间的上下文, 继承带取消通道的上下文
	c := &timerCtx{
		deadline: d,
	}

	// 将这个带截止时间的上下文 c, 挂在父级上下文 parent 的上下文链上
	// 如果父级上下文此时如果取消了, 子上下文也同时取消
	c.cancelCtx.propagateCancel(parent, c)

	// 计算当前和设置的时间的差距
	dur := time.Until(d)
	// 截止时间已经过期
	if dur <= 0 {
		// 关闭ctx
		c.cancel(true, DeadlineExceeded, cause)
		return c, func() { c.cancel(false, Canceled, nil) }
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// 如果通道还未关闭
	if c.err == nil {
		// 开启一个定时任务, 并设置 dur 后开始执行关闭通道的任务
		c.timer = time.AfterFunc(dur, func() {
			c.cancel(true, DeadlineExceeded, cause)
		})
	}

	return c, func() { c.cancel(true, Canceled, nil) }
}

// 携带一个计时器和一个截止时间。它通过停止其计时器来实现通道关闭, 上下文取消
type timerCtx struct {
	cancelCtx             // 继承可关闭通道的上下文
	timer     *time.Timer // 这是一个定时器, 通道会通过这个定时器执行定时关闭
	deadline  time.Time   // 这是一个真正要执行关闭通道的时间点
}

func (c *timerCtx) Deadline() (deadline time.Time, ok bool) {
	return c.deadline, true
}

func (c *timerCtx) String() string {
	return contextName(c.cancelCtx.Context) + ".WithDeadline(" +
		c.deadline.String() + " [" +
		time.Until(c.deadline).String() + "])"
}

// 方法用于取消 timerCtx 上下文，它接受三个参数：
// removeFromParent: 表示是否需要从父级取消器的子级列表中移除自己。
// err: 取消错误，通常是一个 context.Canceled 或 context.DeadlineExceeded 错误。
// cause: 取消的直接原因，可以是 nil。
func (c *timerCtx) cancel(removeFromParent bool, err, cause error) {
	// 调用底层的 cancelCtx 的 cancel 方法来真正执行取消操作。
	// 这里的 false 参数表示不从父级中移除，因为我们将在后面的 if 专门处理。
	c.cancelCtx.cancel(false, err, cause)

	// 如果需要从父级取消器中移除自己
	if removeFromParent {
		removeChild(c.cancelCtx.Context, c) // 从父级上下文移除当前上下文
	}

	c.mu.Lock()

	// 停止定时器, 定时取消关闭通道
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}

	c.mu.Unlock()
}

// WithTimeout 创建一个带有超时限制的 Context 上下文,和一个可以手动取消上下文的函数
// 实际上就是调用 WithDeadline 指定时间取消的ctx, 将指定的时间控制为我们要设置的超时的那个时间点
func WithTimeout(parent Context, timeout time.Duration) (Context, CancelFunc) {
	// parent: 父级上下文
	// time: 当前时间 + 指定的一个时间值 = 未来的一个新时间点
	return WithDeadline(parent, time.Now().Add(timeout))
}

// WithTimeoutCause behaves like [WithTimeout] but also sets the cause of the
// returned Context when the timeout expires. The returned [CancelFunc] does
// not set the cause.
func WithTimeoutCause(parent Context, timeout time.Duration, cause error) (Context, CancelFunc) {
	return WithDeadlineCause(parent, time.Now().Add(timeout), cause)
}

// WithValue 创建一个子上下文, 包括k-v的键值对。
// 最终的上下文是一个链的形式, 每个子上下文都有一个父级上下文
// 根节点就是 backgroundCtx / todoCtx
func WithValue(parent Context, key, val any) Context {
	// 无法从 nil 父上下文创建上下文
	if parent == nil {
		panic("cannot create context from nil parent")
	}

	// nil 键
	if key == nil {
		panic("nil key")
	}

	// 因为对 key 的要求是可比较，因为之后需要通过 key 取出 context 中的值，可比较是必须的。
	if !reflectlite.TypeOf(key).Comparable() {
		panic("key is not comparable")
	}

	// 传入父上下文,和当前的k-v
	// 最终的value上下文是一个链的形式, 每个子上下文都有一个父级上下文
	// 根节点就是 backgroundCtx / todoCtx
	return &valueCtx{parent, key, val}
}

// 携带一个键值对的上下文。
type valueCtx struct {
	Context      // 继承通用上下文接口
	key, val any // 键值对
}

// stringify tries a bit to stringify v, without using fmt, since we don't
// want context depending on the unicode tables. This is only used by
// *valueCtx.String().
func stringify(v any) string {
	switch s := v.(type) {
	case stringer:
		return s.String()
	case string:
		return s
	}
	return "<not Stringer>"
}

func (c *valueCtx) String() string {
	return contextName(c.Context) + ".WithValue(type " +
		reflectlite.TypeOf(c.key).String() +
		", val " + stringify(c.val) + ")"
}

// Value 返回与此上下文关联的键的值，如果没有与键关联的值，则返回 nil。
func (c *valueCtx) Value(key any) any {
	// 如果传入的 key 与当前 valueCtx 结构体中的 key 相等，则返回对应的值。
	if c.key == key {
		return c.val
	}

	// 一直顺着 context 往前，最终找到根节点（一般是 emptyCtx），直接返回一个 nil。
	return value(c.Context, key)
}

// 一直顺着 context 往前，最终找到根节点（一般是 emptyCtx），直接返回一个 nil。
// c: 父级上下文
// key: 要查询的键
func value(c Context, key any) any {
	// 循环查询
	for {
		switch ctx := c.(type) {
		case *valueCtx:
			// 如果传入的 key 与 valueCtx 结构体中的 key 相等，则返回对应的值。
			if key == ctx.key {
				return ctx.val
			}
			// 否则, 获取上一级上下文继续查询
			c = ctx.Context
		case *cancelCtx:
			// 如果传入的 key 是 &cancelCtxKey，则返回当前的 cancelCtx。
			if key == &cancelCtxKey {
				return c
			}
			// 否则, 获取上一级上下文继续查询
			c = ctx.Context
		case withoutCancelCtx:
			// 如果传入的 key 是 &cancelCtxKey，当 ctx 是使用 WithoutCancel 创建时，表示 Cause(ctx) == nil。
			if key == &cancelCtxKey {
				return nil
			}
			// 否则, 获取上一级上下文继续查询
			c = ctx.c
		case *timerCtx:
			// 如果传入的 key 是 &cancelCtxKey，则返回 timerCtx 内的 cancelCtx。
			if key == &cancelCtxKey {
				return &ctx.cancelCtx
			}
			// 否则, 获取上一级上下文继续查询
			c = ctx.Context
		case backgroundCtx, todoCtx:
			// backgroundCtx 和 todoCtx 不携带任何值，直接返回 nil。
			return nil
		default:
			// 其他类型的 Context，继续调用其 Value 方法查找传入 key 对应的值。
			return c.Value(key)
		}
	}
}
