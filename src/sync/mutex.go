// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package sync provides basic synchronization primitives such as mutual
// exclusion locks. Other than the Once and WaitGroup types, most are intended
// for use by low-level library routines. Higher-level synchronization is
// better done via channels and communication.
//
// Values containing the types defined in this package should not be copied.
package sync

import (
	"internal/race"
	"sync/atomic"
	"unsafe"
)

// Provided by runtime via linkname.
func throw(string)
func fatal(string)

// Mutex 是一个互斥锁。
// Mutex 的零值是一个未上锁的互斥锁实例。
//
// 在首次使用后，Mutex 实例不应被复制。
//
// 根据 Go 语言的内存模型，
// 第 n 次调用 Unlock "发生在" 第 m 次调用 Lock "之前"
// 对于任何 n < m 的情况(加锁的次数永远大于解锁的次数)。
// 成功的 TryLock 调用等同于 Lock 调用。
// 失败的 TryLock 调用不建立任何 "发生在" 的关系。
type Mutex struct {
	// state 是一个 32 位整数，用来存储锁的状态信息。
	// 1.通过状态位来区分锁是否被持有,是否处于饥饿状态, 是否处于等待状态, 是否处于竞争状态
	// 2.持有者的等待队列长度
	state int32

	// sema 是一个 32 位无符号整数，作为信号量使用。
	// 当 Mutex 被解锁时，会将 sema 唤醒，允许等待的 goroutine 获取锁。
	sema uint32
}

// A Locker represents an object that can be locked and unlocked.
type Locker interface {
	Lock()
	Unlock()
}

const (
	// 将 iota 左移 0 位，相当于 1，表示互斥锁被锁定
	mutexLocked = 1 << iota
	// 这个状态表示一个 Goroutine 已经被唤醒，可以继续执行
	mutexWoken
	//这个状态表示一个 Goroutine 处于饥饿状态，即在只读模式下被锁阻塞等待锁
	mutexStarving
	// 表示一个用来保存等待 Goroutine 数量的位数
	mutexWaiterShift = iota

	// Mutex fairness.
	//
	// Mutex can be in 2 modes of operations: normal and starvation.
	// In normal mode waiters are queued in FIFO order, but a woken up waiter
	// does not own the mutex and competes with new arriving goroutines over
	// the ownership. New arriving goroutines have an advantage -- they are
	// already running on CPU and there can be lots of them, so a woken up
	// waiter has good chances of losing. In such case it is queued at front
	// of the wait queue. If a waiter fails to acquire the mutex for more than 1ms,
	// it switches mutex to the starvation mode.
	//
	// In starvation mode ownership of the mutex is directly handed off from
	// the unlocking goroutine to the waiter at the front of the queue.
	// New arriving goroutines don't try to acquire the mutex even if it appears
	// to be unlocked, and don't try to spin. Instead they queue themselves at
	// the tail of the wait queue.
	//
	// If a waiter receives ownership of the mutex and sees that either
	// (1) it is the last waiter in the queue, or (2) it waited for less than 1 ms,
	// it switches mutex back to normal operation mode.
	//
	// Normal mode has considerably better performance as a goroutine can acquire
	// a mutex several times in a row even if there are blocked waiters.
	// Starvation mode is important to prevent pathological cases of tail latency.
	starvationThresholdNs = 1e6
)

// Lock 锁定互斥锁 m。
// 如果锁已经被使用，调用该方法的 goroutine 将会阻塞，直到互斥锁变得可用。
func (m *Mutex) Lock() {
	// 快速路径：尝试获取未锁定的互斥锁。
	// 使用原子操作 CompareAndSwapInt32 来检查并设置 m.state 的值。
	// 如果 m.state 当前为 0（表示未锁定），则将其设置为 mutexLocked=1 常量, 表示互斥锁被锁定
	if atomic.CompareAndSwapInt32(&m.state, 0, mutexLocked) {
		// 如果启用了竞态检测（race detection），那么记录这个互斥锁的获取。
		// 这对于竞态检测工具来说非常重要，因为它可以跟踪锁的获取和释放事件。
		if race.Enabled {
			race.Acquire(unsafe.Pointer(m))
		}
		return
	}

	// 慢速路径：当互斥锁已经被其他 goroutine 占用时触发。
	// 此处将调用 lockSlow 方法，它包含了更复杂的逻辑来处理锁的竞争情况。
	m.lockSlow()
}

// TryLock tries to lock m and reports whether it succeeded.
//
// Note that while correct uses of TryLock do exist, they are rare,
// and use of TryLock is often a sign of a deeper problem
// in a particular use of mutexes.
func (m *Mutex) TryLock() bool {
	old := m.state
	if old&(mutexLocked|mutexStarving) != 0 {
		return false
	}

	// There may be a goroutine waiting for the mutex, but we are
	// running now and can try to grab the mutex before that
	// goroutine wakes up.
	if !atomic.CompareAndSwapInt32(&m.state, old, old|mutexLocked) {
		return false
	}

	if race.Enabled {
		race.Acquire(unsafe.Pointer(m))
	}
	return true
}

// lockSlow 函数是 Mutex.Lock 方法的慢速路径。
// 它在快速路径（CompareAndSwapInt32）无法立即获取锁时被调用，
// 即互斥锁已经被其他 goroutine 占用。
// 此函数处理锁的获取，包括自旋、加入等待队列和处理饥饿模式。
func (m *Mutex) lockSlow() {
	var waitStartTime int64 // 记录等待开始的时间戳，用于检测饥饿模式。
	var starving bool       // 标记当前 goroutine 是否处于饥饿模式。
	var awoke bool          // 标记当前 goroutine 是否从阻塞状态被唤醒。
	var iter int            // 自旋尝试次数，用于控制自旋频率。
	var old int32           // 保存互斥锁的旧状态，用于比较和交换操作。

	for {
		// 1.至少被锁了, 才可以自旋
		// 2.饥饿模式是一定不可以自旋的
		// 3.runtime_canSpin自旋的次数不可以超过active_spin=4次
		// 检查锁是否被占用或者处于非饥饿模式
		if old&(mutexLocked|mutexStarving) == mutexLocked && runtime_canSpin(iter) {
			// 当自旋有意义时，尝试设置 mutexWoken 标志位。
			// 这个标志告诉 Unlock 方法，当前 goroutine 已经被唤醒，不需要再唤醒其他 goroutines。
			if !awoke && old&mutexWoken == 0 && old>>mutexWaiterShift != 0 &&
				atomic.CompareAndSwapInt32(&m.state, old, old|mutexWoken) {
				awoke = true // 标记从睡眠中被唤醒
			}

			runtime_doSpin() // 执行一次自旋操作，等待锁的释放。
			iter++           // 增加自旋次数。
			old = m.state    // 更新旧状态，准备下一次 CAS。
			continue         // 继续循环，尝试获取锁。
		}

		// 准备更新的新状态，初始化为旧状态。
		new := old

		// 如果不是饥饿模式，尝试获取锁。
		// mutexLocked 标志位表示锁被占用。
		if old&mutexStarving == 0 {
			new |= mutexLocked
		}

		// 如果锁被占用或处于饥饿模式，增加等待队列计数, 等待队列数量 + 1。
		// mutexWaiterShift 是一个右移位数，用于计算等待队列的长度。
		if old&(mutexLocked|mutexStarving) != 0 {
			new += 1 << mutexWaiterShift
		}

		// 当前 goroutine 切换互斥锁至饥饿模式，前提是锁当前被占用。
		// 饥饿模式下，锁的所有权直接传递给等待队列中的下一个 goroutine。
		if starving && old&mutexLocked != 0 {
			new |= mutexStarving
		}

		// 如果当前 goroutine 已经被标记为唤醒状态，需要重置 mutexWoken 标志位。
		// mutexWoken 标志位表示一个 goroutine 已经被唤醒。
		if awoke {
			if new&mutexWoken == 0 {
				throw("sync: inconsistent mutex state")
			}
			new &^= mutexWoken
		}

		// 尝试使用 CAS 更新 state 状态，修改成功则表示获取到锁资源
		// 注意: 这个 cas 并不是真正加锁, 它只是为了防止这个if被并发执行
		// 理论上所有想要加锁的 goroutine 最终都会进入这个if, 只不过是谁快谁慢而已
		if atomic.CompareAndSwapInt32(&m.state, old, new) {
			// 如果 old 旧状态是非饥饿模式，并且未获取过锁, 说明旧状态是属于一个无锁的状态
			// 此时执行 cas 成功更新为加锁的状态，可以直接 return
			if old&(mutexLocked|mutexStarving) == 0 {
				break // 成功锁定互斥锁，退出循环。
			}

			// 如果旧状态已经是一个加锁的状态了, 就算更新成功了也没有意义
			// 因为你更新的不过是再阻塞队列里多加了一个 goroutine 而已, 锁依然是被占用的
			// 表示未能上锁成功

			// 如果之前已经在等待过至少一次，保持队列中 LIFO（后进先出）顺序。
			queueLifo := waitStartTime != 0
			if waitStartTime == 0 {
				// 如果还没开始等待，记录开始等待的时间
				waitStartTime = runtime_nanotime()
			}

			// 到这里，表示即将开始上锁, 如果上锁失败, 则阻塞

			// 函数的作用是尝试获取锁，如果锁已经被其他 Goroutine 持有，则当前 Goroutine 会被阻塞，加入等待队列，等待锁的释放
			// 当其他 Goroutine 释放了锁时，队头被阻塞的 Goroutine 会被唤醒
			// queueLife = true, 将会把 goroutine 放到等待队列队头
			// queueLife = false, 将会把 goroutine 放到等待队列队尾
			runtime_SemacquireMutex(&m.sema, queueLifo, 1)

			// 到这里，表示队头的 Goroutine 被唤醒, 继续执行下面代码

			// 检查是否切换到了饥饿模式。
			// 计算是否符合饥饿模式，即等待时间是否超过一定的时间
			starving = starving || runtime_nanotime()-waitStartTime > starvationThresholdNs

			// 更新旧状态，准备下一次 CAS。
			old = m.state

			// 如果上一次是饥饿模式, 此次唤醒队头的 goroutine 一定会获得锁, 加入该 if,调整锁状态 state 为加锁
			if old&mutexStarving != 0 {
				if old&(mutexLocked|mutexWoken) != 0 || old>>mutexWaiterShift == 0 {
					throw("sync: inconsistent mutex state")
				}

				// 调整状态，设置 mutexLocked 加锁状态并减少等待计数。
				// mutexLocked 标志位表示锁被占用，mutexWaiterShift 用于计算等待队列的长度。
				delta := int32(mutexLocked - 1<<mutexWaiterShift)

				// 此次不是饥饿模式又或者下次没有要唤起等待队列的 goroutine 了
				if !starving || old>>mutexWaiterShift == 1 {
					// 退出饥饿模式。
					// 这里很关键，因为饥饿模式效率低下，两个 goroutines 可能无限地步调一致地切换互斥锁到饥饿模式。
					// 一旦 goroutine 成功获取锁，就退出饥饿模式，以防止不必要的性能损耗。
					delta -= mutexStarving
				}

				atomic.AddInt32(&m.state, delta)
				break // 成功获取锁，退出循环。
			}

			// 如果上一次不是饥饿状态, 那么正常重置状态, 准备重新竞争锁
			// 这就会导致, 队列出来的锁会慢与已经正在获取锁的 goroutine
			// 因为, 从队列唤醒出来, 到这一行代码, 再继续回到 for 遍历 cas 判断肯定是慢于新 goroutine 的
			// 所以这个被唤醒的 goroutine 再竞争下, 很可能又会加入到等待队列的前面再次被阻塞
			awoke = true // 标记从睡眠中被唤醒
			iter = 0     // 重置自旋次数
		} else {
			// 如果 CAS 失败，更新旧状态并重试。
			// CAS 失败可能是因为另一个 goroutine 修改了状态，因此需要重新读取状态。
			old = m.state
		}
	}

	// 如果启用了竞态检测，记录互斥锁的获取。
	if race.Enabled {
		race.Acquire(unsafe.Pointer(m))
	}
}

// Unlock 用于解锁。
// 如果在调用 Unlock 时 m 没有被锁住，这会引发运行时错误。
//
// 一个被锁住的 Mutex 并不与特定的 Goroutine 相关联。
// 允许一个 Goroutine 锁定一个 Mutex，然后安排另一个 Goroutine 解锁它。
func (m *Mutex) Unlock() {
	// 如果启用了竞态检测，记录互斥锁的状态，并释放锁的持有。
	// 这有助于在多 goroutine 环境中调试竞态条件。
	if race.Enabled {
		_ = m.state                     // 这一行用于确保编译器不会优化掉对 m.state 的引用。
		race.Release(unsafe.Pointer(m)) // 标记互斥锁被释放。
	}

	// 快速路径：清除锁标志。
	// 使用原子操作 AddInt32 来减少 m.state 的值，实际上相当于清除 mutexLocked 标志位。
	// 注意，这里使用负数（-mutexLocked）是因为 AddInt32 是加法操作，我们需要做的是减法。
	new := atomic.AddInt32(&m.state, -mutexLocked)

	// 如果 new 不为零，说明还有其他的标志位被设置，例如有等待的 goroutines 或者处于饥饿模式。
	// 这种情况下，需要调用慢速路径 unlockSlow 来处理剩余的逻辑。因为需要唤醒等待队列的协程
	if new != 0 {
		// 慢速路径被独立出来，以便快速路径能够被内联，提高性能。
		// 在追踪时，我们跳过额外的一帧来隐藏 unlockSlow 的调用，使追踪信息更加简洁。
		m.unlockSlow(new)
	}
}

// 是 Mutex.Unlock 方法的慢速路径。
//
// 当快速路径（atomic.AddInt32）返回非零值时，即互斥锁还保留有其他标志位时，
// 此方法被调用来处理剩余的解锁逻辑。
func (m *Mutex) unlockSlow(new int32) {
	// 首先检查互斥锁是否实际上已经被解锁。
	// 如果在解锁操作之后，互斥锁的状态不包含 mutexLocked 标志位，这表明锁已被非法解锁。
	if (new+mutexLocked)&mutexLocked == 0 {
		fatal("sync: unlock of unlocked mutex")
	}

	// 处理正常解锁逻辑。
	if new&mutexStarving == 0 {
		// 保存互斥锁的旧状态
		old := new

		for {
			// 检查等待队列是否为空: 不需要唤醒其他goroutine, 直接返回
			// 有其他 goroutine 已经被唤醒或获取了锁, 直接返回
			if old>>mutexWaiterShift == 0 || old&(mutexLocked|mutexWoken|mutexStarving) != 0 {
				return
			}

			// 减少等待队列计数，
			new = (old - 1<<mutexWaiterShift) | mutexWoken

			// 尝试原子性地更新状态为新状态
			if atomic.CompareAndSwapInt32(&m.state, old, new) {
				// 释放信号量，唤醒等待队列的一个的 Goroutine
				// 信号量将按照先进先出（FIFO, First In First Out）的原则释放。
				// 这意味着，如果多个 goroutines 正在等待信号量，那么最早进入等待队列的 goroutine 将被唤醒
				runtime_Semrelease(&m.sema, false, 1)
				return
			}

			// 如果 CAS 失败，更新旧状态并重试。
			old = m.state
		}
	} else {
		// 饥饿模式：将互斥锁所有权交给下一个等待者，并且让出时间片，以便下一个等待者可以立即开始运行
		// 注意：未设置互斥锁标志位，等待者会在唤醒后设置它；但如果饥饿标志位已经设置，则新到来的 Goroutine 不会获取锁

		// 直接将锁的所有权传递给等待队列中的下一个 goroutine
		// 信号量将按照后进先出（LIFO, Last In First Out）的原则释放。
		// 这意味着最近进入等待队列的 goroutine 将被唤醒。
		// 因为饥饿模式下, 同一个 goroutine 一直获取锁, 一直失败
		// 失败后会将这个 goroutine , 加入到最前面, 而不是排在队列最后面
		runtime_Semrelease(&m.sema, true, 1)
	}
}
