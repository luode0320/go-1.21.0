// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"sync/atomic"
	"unsafe"
)

// Cond 实现了条件变量，是 Goroutine 在等待事件发生或宣布事件发生时的会合点。
//
// 每个 Cond 都有一个关联的锁 L（通常是 *Mutex 或 *RWMutex），
// 在改变条件和调用 Wait 方法时必须持有该锁。
//
// 在首次使用后，Cond 不得被复制。
//
// 在 Go 内存模型的术语中，Cond 会安排 Broadcast 或 Signal 的调用“与任何它解除阻塞的 Wait 调用同步”。
//
// 对于许多简单的用例，用户使用通道比使用 Cond 更好（Broadcast 对应于关闭通道，Signal 对应于在通道上发送）。
//
// 有关替代 sync.Cond 的更多信息，请参见 [Roberto Clapis 关于高级并发模式的系列文章]，以及 [Bryan Mills 关于并发模式的讲座]。
//
// [Roberto Clapis 关于高级并发模式的系列文章]: https://blogtitle.github.io/categories/concurrency/
// [Bryan Mills 关于并发模式的讲座]: https://drive.google.com/file/d/1nPdvhB0PutEJzdCq5ms6UI58dp50fcAN/view
type Cond struct {
	noCopy  noCopy      // noCopy 用于确保 Cond 不被复制
	L       Locker      // 表示可以锁定和解锁的对象（通常是 *Mutex 或 *RWMutex）。
	notify  notifyList  // notify 用于通知列表
	checker copyChecker // checker 用于检查复制
}

// NewCond 返回一个带有（通常是 *Mutex 或 *RWMutex）的新 Cond。
func NewCond(l Locker) *Cond {
	return &Cond{L: l}
}

// Wait 在原子性地解锁 c.L 并挂起调用 Goroutine 的执行。在稍后恢复执行后，
// Wait 在返回之前会锁定 c.L。不同于其他系统，在没有被 Broadcast 或 Signal 唤醒的情况下，Wait 无法返回。
//
// 因为在 Wait 等待时 c.L 没有被锁定，调用者通常不能假设条件在 Wait 返回时就为真。
// 在使用 Wait 之前, 你必须加锁 c.L.Lock():
//
//	c.L.Lock()
//	for !condition() {
//	    c.Wait()
//	}
//	... 利用条件 ...
//	c.L.Unlock()
func (c *Cond) Wait() {
	// 检查是否存在复制
	c.checker.check()
	// 此 goroutine 添加到通知列表
	t := runtime_notifyListAdd(&c.notify)
	// 解锁 c.L
	c.L.Unlock()
	// 等待通知列表通知, 当前 goroutine 此时被阻塞
	runtime_notifyListWait(&c.notify, t)

	// 唤醒之后立开始竞争锁, 只有竞争到锁的才会结束 Wait 方法
	// 保证共享数据的并发安全性
	c.L.Lock()
}

// Signal 唤醒在 c 上等待的一个 Goroutine（如果有的话）。
func (c *Cond) Signal() {
	// 检查是否存在复制
	c.checker.check()
	// 通知列表唤醒一个 Goroutine
	runtime_notifyListNotifyOne(&c.notify)
}

// Broadcast 唤醒所有在 c 上等待的 Goroutine。
func (c *Cond) Broadcast() {
	c.checker.check()                      // 检查是否存在复制
	runtime_notifyListNotifyAll(&c.notify) // 通知列表唤醒所有 Goroutine
}

// copyChecker holds back pointer to itself to detect object copying.
type copyChecker uintptr

func (c *copyChecker) check() {
	if uintptr(*c) != uintptr(unsafe.Pointer(c)) &&
		!atomic.CompareAndSwapUintptr((*uintptr)(c), 0, uintptr(unsafe.Pointer(c))) &&
		uintptr(*c) != uintptr(unsafe.Pointer(c)) {
		panic("sync.Cond is copied")
	}
}

// noCopy may be added to structs which must not be copied
// after the first use.
//
// See https://golang.org/issues/8005#issuecomment-190753527
// for details.
//
// Note that it must not be embedded, due to the Lock and Unlock methods.
type noCopy struct{}

// Lock is a no-op used by -copylocks checker from `go vet`.
func (*noCopy) Lock()   {}
func (*noCopy) Unlock() {}
