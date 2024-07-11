// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

// This file contains the implementation of Go channels.

// Invariants:
//  At least one of c.sendq and c.recvq is empty,
//  except for the case of an unbuffered channel with a single goroutine
//  blocked on it for both sending and receiving using a select statement,
//  in which case the length of c.sendq and c.recvq is limited only by the
//  size of the select statement.
//
// For buffered channels, also:
//  c.qcount > 0 implies that c.recvq is empty.
//  c.qcount < c.dataqsiz implies that c.sendq is empty.

import (
	"internal/abi"
	"runtime/internal/atomic"
	"runtime/internal/math"
	"unsafe"
)

const (
	maxAlign  = 8
	hchanSize = unsafe.Sizeof(hchan{}) + uintptr(-int(unsafe.Sizeof(hchan{}))&(maxAlign-1))
	debugChan = false
)

// 表示通道的内部数据结构。结构体中包含了用于管理通道的各种字段
// 如队列中的数据量、队列大小、缓冲区、元素大小、关闭标志、元素类型等。
// 同时还包含了用于管理 发送 和 接收 等待者的等待队列，以及一个互斥锁来保护通道的所有字段。
type hchan struct {
	qcount   uint           // 队列中的总数据量
	dataqsiz uint           // 循环队列的大小
	buf      unsafe.Pointer // 指向包含 dataqsiz 个元素的数组
	elemsize uint16         // 元素大小
	closed   uint32         // 关闭标志
	elemtype *_type         // 元素类型
	sendx    uint           // 发送索引
	recvx    uint           // 接收索引
	recvq    waitq          // 接收等待队列, 被阻塞的 goroutine
	sendq    waitq          // 发送等待队列, 被阻塞的 goroutine

	// lock 用于保护 hchan 中的所有字段，
	// 以及阻塞在此通道上的若干 sudogs 中的几个字段。
	//
	// 在持有此锁时，请勿更改另一个 G 的状态
	// （特别是不要让一个 G 可运行），因为这可能会导致堆栈收缩死锁。
	lock mutex
}

// 表示等待队列的结构。该结构体包含了两个字段，分别指向队列中第一个 sudog 和最后一个 sudog。
// sudog 是表示与 goroutine 相关的结构体，在这里用来管理等待通道操作的 goroutine。
type waitq struct {
	first *sudog // 队列的第一个 sudog, sudog 实际上是对 goroutine 的一个封装
	last  *sudog // 队列的最后一个 sudog, sudog 实际上是对 goroutine 的一个封装
}

//go:linkname reflect_makechan reflect.makechan
func reflect_makechan(t *chantype, size int) *hchan {
	return makechan(t, size)
}

func makechan64(t *chantype, size int64) *hchan {
	if int64(int(size)) != size {
		panic(plainError("makechan: size out of range"))
	}

	return makechan(t, int(size))
}

// 用于创建一个通道并返回指向该通道的指针。
// 在函数中进行了元素大小、内存分配、对齐等方面的检查。
// 根据元素是否包含指针，选择不同的分配方式，并且初始化相应的字段，最后返回创建好的通道指针。
func makechan(t *chantype, size int) *hchan {
	// 获取通道元素类型
	elem := t.Elem

	// 编译器会检查这一点，但还是保险起见。
	if elem.Size_ >= 1<<16 {
		throw("makechan: 无效的通道元素类型")
	}
	if hchanSize%maxAlign != 0 || elem.Align_ > maxAlign {
		throw("makechan: 错误对齐")
	}

	// 检查通道元素总大小是否超出范围，防止溢出或负数大小。
	mem, overflow := math.MulUintptr(elem.Size_, uintptr(size))
	if overflow || mem > maxAlloc-hchanSize || size < 0 {
		panic(plainError("makechan: 大小超出范围"))
	}

	// 初始化 hchan 结构体，根据不同的情况分配内存。
	// Hchan 不包含 GC 需要跟踪的指针，除非存储在 buf 中的元素包含指针。
	// buf 指向同一分配区域，elemtype 是持久的。
	// SudoG 被其拥有线程引用，所以不会被收集。
	// TODO(dvyukov,rlh): 当收集器可以移动已分配的对象时重新考虑。
	var c *hchan
	switch {
	case mem == 0:
		// 队列或元素大小为零。分配 hchan 的内存
		c = (*hchan)(mallocgc(hchanSize, nil, true))
		// Race detector 使用该位置进行同步。
		// 设置 buf 指向 hchan 的 raceaddr 字段
		c.buf = c.raceaddr()
	case elem.PtrBytes == 0:
		// 元素不包含指针。
		// 在一次调用中分配 hchan 和 buf。
		// 分配 hchan 和 buf 的连续内存
		c = (*hchan)(mallocgc(hchanSize+mem, nil, true))
		// 设置 buf 指向 hchan 后面的内存区域
		c.buf = add(unsafe.Pointer(c), hchanSize)
	default:
		// 元素包含指针。
		// 动态创建 hchan 结构体
		c = new(hchan)
		// 单独分配 buf 的内存
		c.buf = mallocgc(mem, elem, true)
	}

	// 初始化 hchan 的其他字段。
	c.elemsize = uint16(elem.Size_)  // 设置每个元素的大小
	c.elemtype = elem                // 设置元素类型
	c.dataqsiz = uint(size)          // 设置循环队列的大小大小
	lockInit(&c.lock, lockRankHchan) // 初始化锁

	// 如果启用了 debug 通道，打印调试信息。
	if debugChan {
		print("makechan: chan=", c, "; elemsize=", elem.Size_, "; dataqsiz=", size, "\n")
	}
	// 返回初始化后的通道
	return c
}

// chanbuf(c, i) 是指向缓冲区中第i个插槽的指针。
func chanbuf(c *hchan, i uint) unsafe.Pointer {
	return add(c.buf, uintptr(i)*uintptr(c.elemsize))
}

// 判断在通道 c 上发送是否会阻塞（即通道已满）。
// 它使用一个字长大小的可变状态读取，因此尽管返回值立即为真，但正确答案可能在调用函数接收返回值之前发生变化。
func full(c *hchan) bool {
	// c.dataqsiz 是不可变的（在通道创建后从不写入）
	// 所以在通道操作期间的任何时候读取都是安全的。
	if c.dataqsiz == 0 {
		// 假设指针读取是松散原子的。
		// 对于无缓冲通道，检查是否有等待接收的 goroutine。
		return c.recvq.first == nil
	}
	// 假设无符号整数读取是松散原子的。
	// 对于有缓冲通道，检查缓冲区中的元素数量是否等于缓冲区大小。
	return c.qcount == c.dataqsiz
}

// c <- x从编译代码的入口点。
//
//go:nosplit
func chansend1(c *hchan, elem unsafe.Pointer) {
	chansend(c, elem, true, getcallerpc())
}

/*
 * 泛型单通道发送/接收
 *
 * @param c 通道指针
 * @param ep 要发送/接收的元素指针
 * @param block 是否阻塞发送/接收操作
 * @param callerpc 调用者的程序计数器
 *
 * @return 是否成功发送/接收
 */
func chansend(c *hchan, ep unsafe.Pointer, block bool, callerpc uintptr) bool {
	// 如果 channel 是 nil
	if c == nil {
		// 不能阻塞，直接返回 false，表示未发送成功
		if !block {
			return false
		}
		// 当前 goroutine 被挂起
		gopark(nil, nil, waitReasonChanSendNilChan, traceBlockForever, 2)
		throw("unreachable")
	}

	if debugChan {
		print("chansend: chan=", c, "\n")
	}

	if raceenabled {
		racereadpc(c.raceaddr(), callerpc, abi.FuncPCABIInternal(chansend))
	}

	// 对于不阻塞的 send，快速检测失败场景
	// full 判断在通道 c 上发送是否会阻塞（即通道已满）。
	//
	// 如果 channel 未关闭且 channel 没有多余的缓冲空间。这可能是：
	// 1. channel 是非缓冲型的，且等待接收队列里没有 goroutine, 快速检测失败
	// 2. channel 是缓冲型的，但循环数组已经装满了元素. 快速检测失败
	if !block && c.closed == 0 && full(c) {
		return false
	}

	var t0 int64
	if blockprofilerate > 0 {
		t0 = cputicks()
	}

	// 锁住 channel，并发安全
	lock(&c.lock)

	// 如果 channel 关闭了
	if c.closed != 0 {
		// 解锁
		unlock(&c.lock)
		// 直接 panic
		panic(plainError("在 closed 关闭的 channel 通道上发送"))
	}

	// 如果接收队列里有 goroutine，直接将要发送的数据拷贝到接收 goroutine
	if sg := c.recvq.dequeue(); sg != nil {
		// 发现了等待的接收者。我们直接将要发送的值传递给接收者，绕过通道的缓冲区（如果有）。
		send(c, sg, ep, func() { unlock(&c.lock) }, 3)
		return true
	}

	// 对于缓冲型的 channel，如果还有缓冲空间
	if c.qcount < c.dataqsiz {
		// 通道缓冲区中有可用的空间。将要发送的元素入队。
		// qp 指向 buf 的 sendx 发送索引位置
		qp := chanbuf(c, c.sendx)
		if raceenabled {
			racenotify(c, c.sendx, nil)
		}

		// 将数据从 ep 要发送/接收的元素指针处拷贝到 qp
		// 完成发送数据入队
		typedmemmove(c.elemtype, qp, ep)
		// 发送游标值加 1
		c.sendx++
		// 如果发送游标值等于容量值，游标值归 0
		if c.sendx == c.dataqsiz {
			c.sendx = 0
		}
		// 缓冲区的元素数量加一
		c.qcount++
		// 解锁
		unlock(&c.lock)
		return true
	}

	// 如果不需要阻塞，则直接返回错误
	if !block {
		unlock(&c.lock)
		return false
	}

	// channel 满了，发送方会被阻塞。接下来会构造一个 sudog

	// 获取当前 goroutine 的指针
	gp := getg()
	// 申请一个 sudog，用于跟踪当前 goroutine 的等待状态
	mysg := acquireSudog()
	mysg.releasetime = 0
	if t0 != 0 {
		// 如果启用了阻塞时间记录，设置 releasetime 为 -1，表示开始计时
		mysg.releasetime = -1
	}

	mysg.elem = ep        // 将发送数据赋值给 sudog 的 elem 字段，以便后续处理
	mysg.waitlink = nil   // 初始化 sudog 的 waitlink 字段
	mysg.g = gp           // 将 sudog 与当前 goroutine 绑定
	mysg.isSelect = false // 标记这不是由 select 语句引起的阻塞
	mysg.c = c            // 绑定 sudog 到通道 c
	gp.waiting = mysg     // 将 sudog 添加到当前 goroutine 的 waiting 列表
	gp.param = nil        // 清除参数，用于后续可能的唤醒

	// 当前 goroutine 进入发送等待队列，等待接收方来唤醒
	c.sendq.enqueue(mysg)

	// 当前 goroutine 被挂起，等待通道 c 的锁被释放
	gp.parkingOnChan.Store(true) // 标记当前 goroutine 正在等待通道
	gopark(chanparkcommit, unsafe.Pointer(&c.lock), waitReasonChanSend, traceBlockChanSend, 2)

	// 确保发送的值一直保持活动，直到接收者复制出来。
	KeepAlive(ep)

	// 从这里开始，当前 goroutine 被唤醒了，意味着数据已经被发送到通道了, 才会释放这个 goroutine

	// 有人唤醒了我们, 说明有一个接收着从队列里面读取了我们发送者阻塞的 goroutine
	// 并将发送的数据从 我们此发送者的 goroutine 写入到了 它接收者的 goroutine。

	// 验证 sudog 是否仍然在当前 goroutine 的 waiting 列表中
	if mysg != gp.waiting {
		throw("G waiting list is corrupted") // 如果 sudog 不在列表中，说明列表被意外修改
	}

	gp.waiting = nil            // 清除 sudog 从当前 goroutine 的 waiting 列表
	gp.activeStackChans = false // 标记当前 goroutine 不再等待通道
	closed := !mysg.success     // 检查 sudog 的 success 字段，确定通道是否已关闭
	gp.param = nil              // 清除参数，以防万一

	// 如果有记录阻塞时间，计算并记录阻塞持续时间
	if mysg.releasetime > 0 {
		blockevent(mysg.releasetime-t0, 2)
	}

	mysg.c = nil       // 去掉 mysg 上绑定的 channel
	releaseSudog(mysg) // 释放 sudog，因为它不再需要

	// 如果通道在发送方等待期间被关闭，引发 panic
	if closed {
		if c.closed == 0 {
			throw("chansend: spurious wakeup") // 如果通道没有真正关闭，这是一次虚假唤醒
		}
		// 被唤醒后发现通道关闭了，引发 panic
		panic(plainError("send on closed channel"))
	}
	return true
}

// send 函数处理向一个空的 channel 发送操作
// ep 指向被发送的元素，会被直接拷贝到接收的 goroutine
// 之后，接收的 goroutine 会被唤醒
//
// c 必须是空的（因为等待队列里有 goroutine，肯定是空的, 否则不可能出现等待）
// c 必须被上锁，发送操作执行完后，会使用 unlockf 函数解锁
// sg 必须已经从等待队列里取出来了
// ep 必须是非空，并且它指向堆或调用者的栈
func send(c *hchan, sg *sudog, ep unsafe.Pointer, unlockf func(), skip int) {
	// 如果启用了竞争检测
	if raceenabled {
		if c.dataqsiz == 0 {
			racesync(c, sg) // 无缓冲通道的竞争检测同步
		} else {
			// 假设我们通过缓冲区进行，即使我们直接复制。需要注意只有在 raceenabled 时我们才需要增加头尾位置。
			racenotify(c, c.recvx, nil) // 通知竞争检测关于接收位置的变更
			racenotify(c, c.recvx, sg)  // 通知竞争检测关于 sg 的变更
			c.recvx++                   // 更新接收位置

			// 如果到达末尾，重置位置
			if c.recvx == c.dataqsiz {
				c.recvx = 0
			}

			// 将发送位置同步到接收位置
			c.sendx = c.recvx // c.sendx = (c.sendx+1) % c.dataqsiz
		}
	}

	// 如果 sg 中的 elem 不为 nil，这表示 sg 已经准备好接收数据
	if sg.elem != nil {
		// 直接将 sg 中的值发送到通道 c 中
		sendDirect(c.elemtype, sg, ep)
		// 清理 sg 中的数据，避免重复发送
		sg.elem = nil
	}

	// 获取此 goroutine
	gp := sg.g
	// 使用 unlockf 解锁通道 c
	unlockf()
	// 将 gp 的参数设置为 sg，这样 gp 在恢复执行时可以访问到 sg
	gp.param = unsafe.Pointer(sg)
	// 标记 sg 成功接收了数据
	sg.success = true

	// 如果 sg 的释放时间不为 0，则更新为当前时间
	if sg.releasetime != 0 {
		// 更新 sg 的释放时间戳
		sg.releasetime = cputicks()
	}

	// 将 gp 标记为可运行状态，使其可以被调度器选择执行
	// 唤醒接收的 goroutine
	// skip和打印栈相关，暂时不理会
	goready(gp, skip+1)
}

// 发送者的数据写入到接收者
func sendDirect(t *_type, sg *sudog, src unsafe.Pointer) {
	// src 位于接收者, dst 位于发送者
	dst := sg.elem

	// 使用类型相关的写屏障处理内存复制
	// 这个函数将确保在内存复制之前，任何可能的引用都被正确地更新
	typeBitsBulkBarrier(t, uintptr(dst), uintptr(src), t.Size_)

	// 由于 dst 始终是 Go 内存，因此不需要进行 cgo 写屏障检查。
	// 对内存执行内存复制操作，大小为 t 的大小
	// 这里 memmove 实际上是一个 Go 的内存复制函数，不同于 C 的 memmove
	memmove(dst, src, t.Size_)
}

// 接收发送者的数据到接收者
func recvDirect(t *_type, sg *sudog, dst unsafe.Pointer) {
	// src 位于发送者, dst 位于接收者
	src := sg.elem

	// 使用类型相关的写屏障处理内存复制
	// 这个函数将确保在内存复制之前，任何可能的引用都被正确地更新
	typeBitsBulkBarrier(t, uintptr(dst), uintptr(src), t.Size_)
	// 由于 src 始终是 Go 内存，因此不需要进行 cgo 写屏障检查。
	// 对内存执行内存复制操作，大小为 t 的大小
	// 这里 memmove 实际上是一个 Go 的内存复制函数，不同于 C 的 memmove
	memmove(dst, src, t.Size_)
}

// 关闭一个通道。
// 如果通道为 nil，将触发 panic。
// 如果通道已经被关闭，再次调用此函数也将触发 panic。
func closechan(c *hchan) {
	// 检查通道是否为 nil
	if c == nil {
		panic(plainError("close of nil channel"))
	}

	// 加锁通道，防止其他 goroutines 修改通道状态
	lock(&c.lock)

	// 如果通道已经关闭，解锁并触发 panic
	if c.closed != 0 {
		unlock(&c.lock)
		panic(plainError("close of closed channel"))
	}

	// 如果启用了竞争检测
	if raceenabled {
		callerpc := getcallerpc()                                             // 获取调用者 PC
		racewritepc(c.raceaddr(), callerpc, abi.FuncPCABIInternal(closechan)) // 记录对通道的写操作
		racerelease(c.raceaddr())                                             // 释放竞争检测锁
	}

	// 标记通道为已关闭
	c.closed = 1

	// 创建一个列表，用于保存等待的 goroutines
	var glist gList

	// 释放所有接收者
	for {
		// 从接收队列中移除一个 sudog
		sg := c.recvq.dequeue()
		// 如果队列为空，退出循环
		if sg == nil {
			break
		}
		// 如果 sudog 持有数据
		if sg.elem != nil {
			typedmemclr(c.elemtype, sg.elem) // 清除数据
			sg.elem = nil                    // 清空 sudog 的 elem 字段
		}
		// 如果设置了释放时间
		if sg.releasetime != 0 {
			sg.releasetime = cputicks() // 更新释放时间
		}

		gp := sg.g                    // 获取 sudog 所属的 goroutine
		gp.param = unsafe.Pointer(sg) // 设置 goroutine 参数为 sudog
		sg.success = false            // 标记接收操作失败

		// 如果启用了竞争检测
		if raceenabled {
			raceacquireg(gp, c.raceaddr()) // 通知竞争检测器 goroutine 访问通道
		}

		// 将接收者 goroutine 添加到列表中
		glist.push(gp)
	}

	// 释放所有写者（他们将会 panic, 发送者本身如果发现通道被关闭会触发 panic）
	for {
		// 从发送队列中移除一个 sudog
		sg := c.sendq.dequeue()
		if sg == nil {
			break // 如果队列为空，退出循环
		}

		sg.elem = nil // 清空 sudog 的 elem 字段
		// 如果设置了释放时间
		if sg.releasetime != 0 {
			sg.releasetime = cputicks() // 更新释放时间
		}

		gp := sg.g                    // 获取 sudog 所属的 goroutine
		gp.param = unsafe.Pointer(sg) // 设置 goroutine 参数为 sudog
		sg.success = false            // 标记发送操作失败

		// 如果启用了竞争检测
		if raceenabled {
			raceacquireg(gp, c.raceaddr()) // 通知竞争检测器 goroutine 访问通道
		}
		// 将 goroutine 添加到列表中
		glist.push(gp)
	}

	// 解锁通道
	unlock(&c.lock)

	// 当我们已经释放了通道锁，现在准备好所有 Gs。

	// 遍历列表中的所有 goroutines
	for !glist.empty() {
		gp := glist.pop() // 移除列表中的一个 goroutine
		gp.schedlink = 0  // 清除调度链接
		goready(gp, 3)    // 将 goroutine 设置为可运行状态
	}
}

// empty reports whether a read from c would block (that is, the channel is
// empty).  It uses a single atomic read of mutable state.
func empty(c *hchan) bool {
	// c.dataqsiz is immutable.
	if c.dataqsiz == 0 {
		return atomic.Loadp(unsafe.Pointer(&c.sendq.first)) == nil
	}
	return atomic.Loaduint(&c.qcount) == 0
}

// entry 从编译的代码指向 <- c。
//
//go:nosplit
func chanrecv1(c *hchan, elem unsafe.Pointer) {
	chanrecv(c, elem, true)
}

//go:nosplit
func chanrecv2(c *hchan, elem unsafe.Pointer) (received bool) {
	_, received = chanrecv(c, elem, true)
	return
}

// 在通道 c 上接收数据，并将接收到的数据写入 ep。
// ep 可以为 nil，在这种情况下，接收到的数据会被忽略。
// 如果 block 为 false 且没有元素可用，则返回 (false, false)。
// 否则，如果 c 已关闭，则将 *ep 置零并返回 (true, false)。
// 否则，填充 *ep 并返回 (true, true)。
// 非 nil 的 ep 必须指向堆或调用者的堆栈。
func chanrecv(c *hchan, ep unsafe.Pointer, block bool) (selected, received bool) {
	// raceenabled: 不需要检查 ep，因为它总是位于堆栈上
	// 或是由 reflect 分配的新内存。

	if debugChan {
		print("chanrecv: chan=", c, "\n")
	}

	if c == nil {
		if !block {
			return
		}
		gopark(nil, nil, waitReasonChanReceiveNilChan, traceBlockForever, 2)
		throw("unreachable")
	}

	// 快速路径：在不获取锁的情况下，检查失败的非阻塞操作。
	if !block && empty(c) {
		// 观察到通道不适合接收后，检查通道是否关闭。
		// 重新排序这些检查可能导致与关闭竞赛时的不正确行为。
		if atomic.Load(&c.closed) == 0 {
			// 因为通道不能重新打开，所以后来观察到通道没有关闭意味着它在第一次观察时也没有关闭。
			// 我们的行为就像我们在那一刻观察到的通道，并报告接收无法进行。
			return
		}
		// 通道不可逆地关闭。重新检查通道是否接收任何待接收数据，
		// 这些数据可能出现在空和关闭检查之间的时刻。
		if empty(c) {
			// 通道不可逆地关闭且为空。
			if raceenabled {
				raceacquire(c.raceaddr())
			}
			if ep != nil {
				typedmemclr(c.elemtype, ep)
			}
			return true, false
		}
	}

	var t0 int64
	if blockprofilerate > 0 {
		t0 = cputicks()
	}

	lock(&c.lock)

	if c.closed != 0 {
		if c.qcount == 0 {
			if raceenabled {
				raceacquire(c.raceaddr())
			}
			unlock(&c.lock)
			if ep != nil {
				typedmemclr(c.elemtype, ep)
			}
			return true, false
		}
		// 通道已关闭，但通道的缓冲中有数据, 不必理会, 就算关闭了, 也可以继续读取。
	} else {
		// 找到未关闭的等待发送者。
		if sg := c.sendq.dequeue(); sg != nil {
			// 找到等待的发送者。如果缓冲区大小为 0，直接从发送者接收值。
			// 否则，从队列头部接收并添加发送者的值到队列尾部。
			recv(c, sg, ep, func() { unlock(&c.lock) }, 3)
			return true, true
		}
	}

	if c.qcount > 0 {
		// 直接从队列接收。
		// 计算队列中数据的位置
		qp := chanbuf(c, c.recvx)

		// 如果启用了竞争检测
		if raceenabled {
			// 通知竞争检测器当前正在访问的缓冲区位置
			racenotify(c, c.recvx, nil)
		}

		// 如果 ep 不为 nil，意味着接收端希望接收数据
		if ep != nil {
			// 将通道队列中的数据复制到接收端提供的位置
			typedmemmove(c.elemtype, ep, qp)
		}

		// 清除队列中的数据，以便下次发送或接收可以使用这个位置
		typedmemclr(c.elemtype, qp)

		c.recvx++ // 更新接收位置，准备下一次接收

		// 如果接收位置到达缓冲区的末尾，重置为缓冲区的起始位置
		if c.recvx == c.dataqsiz {
			c.recvx = 0
		}

		c.qcount--        // 减少队列中的数据计数，表示已取出一个数据
		unlock(&c.lock)   // 释放通道的锁，因为接收操作已完成
		return true, true // 返回 true 表示已成功从通道接收数据，且通道有数据
	}

	if !block {
		// 如果是非阻塞模式，没有数据立即可用，则立即返回，表示没有接收成功
		unlock(&c.lock)
		return false, false
	}

	// 没有可用的发送者：在通道上阻塞。

	gp := getg()           // 获取当前 goroutine 的信息
	mysg := acquireSudog() // 申请一个 sudog，用于跟踪 goroutine 的等待状态
	mysg.releasetime = 0

	// 如果正在收集阻塞时间信息，设置 releasetime 为 -1
	if t0 != 0 {
		mysg.releasetime = -1
	}

	// 不允许在分配 elem 和将 mysg 添加到 gp.waiting 之间分割堆栈，
	// 因为 copystack 可以找到它，这确保了在等待过程中堆栈状态的连续性
	mysg.elem = ep
	mysg.waitlink = nil
	gp.waiting = mysg
	mysg.g = gp
	mysg.isSelect = false
	mysg.c = c
	gp.param = nil

	// 将 sudog 添加到通道的接收等待队列
	c.recvq.enqueue(mysg)

	// 发出信号，通知任何试图缩小我们堆栈的人，我们即将在通道上挂起。
	// 在这个 G 的状态改变和我们设置 gp.activeStackChans 之间的窗口期
	// 不适合进行堆栈缩小，因为我们正准备挂起等待通道。
	gp.parkingOnChan.Store(true)

	// 挂起当前 goroutine，等待通道上的数据
	gopark(chanparkcommit, unsafe.Pointer(&c.lock), waitReasonChanReceive, traceBlockChanRecv, 2)

	// 有人唤醒了我们, 说明有一个发送着从队列里面读取了我们接收者阻塞的 goroutine
	// 并将发送的数据从 发送者的 goroutine 写入到了 我们此接收者的 goroutine。

	// 检查 sudog 是否仍然在等待列表中，以验证等待列表的完整性
	if mysg != gp.waiting {
		throw("G waiting list is corrupted")
	}
	gp.waiting = nil
	gp.activeStackChans = false

	// 如果设置了 releasetime，记录阻塞事件的持续时间
	if mysg.releasetime > 0 {
		blockevent(mysg.releasetime-t0, 2)
	}

	// 检查 sudog 的 success 字段，以确定是否成功接收数据
	success := mysg.success
	gp.param = nil
	mysg.c = nil

	// 释放 sudog，因为等待已经结束
	releaseSudog(mysg)
	// 返回 true 表示已经选择了通道，success 表示是否成功接收数据
	return true, success
}

// 处理在满通道 c 上的接收操作。
// 包含两个部分：
//  1. 发送者 sg 发送的值被放入通道中，
//     并唤醒发送者继续执行。
//  2. 接收者（当前的 G）接收到的值被写入 ep。
//
// 对于同步通道，两个值相同。
// 对于异步通道，接收者从通道缓冲区获取数据，
// 而发送者的数据被放入通道缓冲区。
// 通道 c 必须是满的且已锁定。recv 使用 unlockf 解锁 c。
// sg 必须已经被从 c 中出队。
// 非 nil 的 ep 必须指向堆或调用者的堆栈。
func recv(c *hchan, sg *sudog, ep unsafe.Pointer, unlockf func(), skip int) {
	// 如果是非缓冲型的 channel
	if c.dataqsiz == 0 {
		// 无缓冲通道的处理
		if raceenabled {
			racesync(c, sg) // 竞争检测同步
		}
		if ep != nil {
			// 从发送者复制数据
			recvDirect(c.elemtype, sg, ep)
		}
	} else {
		// 缓冲型的 channel，但 buf 已满。
		// 将循环数组 buf 队首的元素拷贝到接收数据的地址, 将发送者的数据入队。
		// 实际就是如果缓冲区满, 接收者取出数据, 并把新的发送者的数据填充进去

		// 找到接收者的游标位置
		qp := chanbuf(c, c.recvx)
		if raceenabled {
			racenotify(c, c.recvx, nil) // 竞争检测通知
			racenotify(c, c.recvx, sg)
		}

		// 将接收游标处的数据拷贝给接收者
		if ep != nil {
			typedmemmove(c.elemtype, ep, qp)
		}
		// 将发送者数据拷贝到 buf
		typedmemmove(c.elemtype, qp, sg.elem)
		c.recvx++
		if c.recvx == c.dataqsiz {
			c.recvx = 0 // 循环缓冲区
		}
		// 更新发送位置
		c.sendx = c.recvx // c.sendx = (c.sendx+1) % c.dataqsiz
	}
	sg.elem = nil                 // 清除发送者数据
	gp := sg.g                    // 获取发送者 goroutine
	unlockf()                     // 解锁通道
	gp.param = unsafe.Pointer(sg) // 设置 goroutine 参数
	sg.success = true             // 标记为成功

	// 更新释放时间
	if sg.releasetime != 0 {
		sg.releasetime = cputicks()
	}
	// 将发送者 goroutine 设置为可运行状态
	goready(gp, skip+1)
}

func chanparkcommit(gp *g, chanLock unsafe.Pointer) bool {
	// There are unlocked sudogs that point into gp's stack. Stack
	// copying must lock the channels of those sudogs.
	// Set activeStackChans here instead of before we try parking
	// because we could self-deadlock in stack growth on the
	// channel lock.
	gp.activeStackChans = true
	// Mark that it's safe for stack shrinking to occur now,
	// because any thread acquiring this G's stack for shrinking
	// is guaranteed to observe activeStackChans after this store.
	gp.parkingOnChan.Store(false)
	// Make sure we unlock after setting activeStackChans and
	// unsetting parkingOnChan. The moment we unlock chanLock
	// we risk gp getting readied by a channel operation and
	// so gp could continue running before everything before
	// the unlock is visible (even to gp itself).
	unlock((*mutex)(chanLock))
	return true
}

// compiler implements
//
//	select {
//	case c <- v:
//		... foo
//	default:
//		... bar
//	}
//
// as
//
//	if selectnbsend(c, v) {
//		... foo
//	} else {
//		... bar
//	}
func selectnbsend(c *hchan, elem unsafe.Pointer) (selected bool) {
	return chansend(c, elem, false, getcallerpc())
}

// compiler implements
//
//	select {
//	case v, ok = <-c:
//		... foo
//	default:
//		... bar
//	}
//
// as
//
//	if selected, ok = selectnbrecv(&v, c); selected {
//		... foo
//	} else {
//		... bar
//	}
func selectnbrecv(elem unsafe.Pointer, c *hchan) (selected, received bool) {
	return chanrecv(c, elem, false)
}

//go:linkname reflect_chansend reflect.chansend0
func reflect_chansend(c *hchan, elem unsafe.Pointer, nb bool) (selected bool) {
	return chansend(c, elem, !nb, getcallerpc())
}

//go:linkname reflect_chanrecv reflect.chanrecv
func reflect_chanrecv(c *hchan, nb bool, elem unsafe.Pointer) (selected bool, received bool) {
	return chanrecv(c, elem, !nb)
}

//go:linkname reflect_chanlen reflect.chanlen
func reflect_chanlen(c *hchan) int {
	if c == nil {
		return 0
	}
	return int(c.qcount)
}

//go:linkname reflectlite_chanlen internal/reflectlite.chanlen
func reflectlite_chanlen(c *hchan) int {
	if c == nil {
		return 0
	}
	return int(c.qcount)
}

//go:linkname reflect_chancap reflect.chancap
func reflect_chancap(c *hchan) int {
	if c == nil {
		return 0
	}
	return int(c.dataqsiz)
}

//go:linkname reflect_chanclose reflect.chanclose
func reflect_chanclose(c *hchan) {
	closechan(c)
}

func (q *waitq) enqueue(sgp *sudog) {
	// 将 sudog 结构体 sgp 添加到等待队列 q 中
	sgp.next = nil
	x := q.last
	if x == nil {
		// 如果队列为空，直接将 sgp 设置为第一个元素并更新 first 和 last 指针
		sgp.prev = nil
		q.first = sgp
		q.last = sgp
		return
	}
	// 队列不为空时，在队尾添加新的元素 sgp
	sgp.prev = x
	x.next = sgp
	q.last = sgp
}

func (q *waitq) dequeue() *sudog {
	// 从等待队列 q 中移除并返回一个 sudog 结构体
	for {
		// 获取队列头部的 sudog
		sgp := q.first

		// 队列为空，直接返回 nil
		if sgp == nil {
			return nil
		}

		y := sgp.next
		// 队列中只有一个元素
		if y == nil {
			q.first = nil
			q.last = nil
		} else {
			y.prev = nil
			q.first = y
			sgp.next = nil // 标记为已移除 (参见 dequeueSudoG)
		}

		// 如果一个 goroutine 被放在该队列中是因为 select 语句，
		// 那么在该 goroutine 被另一个 case 唤醒并获取通道锁之间存在一小段时间窗口。
		// 一旦获取锁后，它会将自身从队列中移除，因此之后我们就看不到它了。
		// 我们在 G 结构体中使用一个标志来告诉我们，有其他人已经赢得了唤醒这个 goroutine 的竞争，
		// 但是这个 goroutine 还没有从队列中移除自己。
		if sgp.isSelect && !sgp.g.selectDone.CompareAndSwap(0, 1) {
			continue
		}

		return sgp
	}
}

func (c *hchan) raceaddr() unsafe.Pointer {
	// Treat read-like and write-like operations on the channel to
	// happen at this address. Avoid using the address of qcount
	// or dataqsiz, because the len() and cap() builtins read
	// those addresses, and we don't want them racing with
	// operations like close().
	return unsafe.Pointer(&c.buf)
}

func racesync(c *hchan, sg *sudog) {
	racerelease(chanbuf(c, 0))
	raceacquireg(sg.g, chanbuf(c, 0))
	racereleaseg(sg.g, chanbuf(c, 0))
	raceacquire(chanbuf(c, 0))
}

// Notify the race detector of a send or receive involving buffer entry idx
// and a channel c or its communicating partner sg.
// This function handles the special case of c.elemsize==0.
func racenotify(c *hchan, idx uint, sg *sudog) {
	// We could have passed the unsafe.Pointer corresponding to entry idx
	// instead of idx itself.  However, in a future version of this function,
	// we can use idx to better handle the case of elemsize==0.
	// A future improvement to the detector is to call TSan with c and idx:
	// this way, Go will continue to not allocating buffer entries for channels
	// of elemsize==0, yet the race detector can be made to handle multiple
	// sync objects underneath the hood (one sync object per idx)
	qp := chanbuf(c, idx)
	// When elemsize==0, we don't allocate a full buffer for the channel.
	// Instead of individual buffer entries, the race detector uses the
	// c.buf as the only buffer entry.  This simplification prevents us from
	// following the memory model's happens-before rules (rules that are
	// implemented in racereleaseacquire).  Instead, we accumulate happens-before
	// information in the synchronization object associated with c.buf.
	if c.elemsize == 0 {
		if sg == nil {
			raceacquire(qp)
			racerelease(qp)
		} else {
			raceacquireg(sg.g, qp)
			racereleaseg(sg.g, qp)
		}
	} else {
		if sg == nil {
			racereleaseacquire(qp)
		} else {
			racereleaseacquireg(sg.g, qp)
		}
	}
}
