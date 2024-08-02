// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This implements the write barrier buffer. The write barrier itself
// is gcWriteBarrier and is implemented in assembly.
//
// See mbarrier.go for algorithmic details on the write barrier. This
// file deals only with the buffer.
//
// The write barrier has a fast path and a slow path. The fast path
// simply enqueues to a per-P write barrier buffer. It's written in
// assembly and doesn't clobber any general purpose registers, so it
// doesn't have the usual overheads of a Go call.
//
// When the buffer fills up, the write barrier invokes the slow path
// (wbBufFlush) to flush the buffer to the GC work queues. In this
// path, since the compiler didn't spill registers, we spill *all*
// registers and disallow any GC safe points that could observe the
// stack frame (since we don't know the types of the spilled
// registers).

package runtime

import (
	"internal/goarch"
	"runtime/internal/atomic"
	"unsafe"
)

// testSmallBuf forces a small write barrier buffer to stress write
// barrier flushing.
const testSmallBuf = false

// wbBuf 是一个每-P 的缓冲区，用于存储由写屏障排队的指针。
// 当缓冲区填满或在各种垃圾回收转换时，此缓冲区会被刷新到 GC 工作缓冲区中。
//
// 这个结构与“顺序存储缓冲区”（SSB）密切相关，
// 但 SSB 通常用于维护记忆集，而这个缓冲区用于标记。
type wbBuf struct {
	// next 指向 buf 中的下一个可用槽位。它必须不是一个指针类型，
	// 因为它可以指向 buf 的末尾之外，并且必须在没有写屏障的情况下更新。
	//
	// 之所以使用 uintptr 而不是索引，是为了优化写屏障的汇编代码。
	next uintptr

	// end 指向 buf 的末尾之后的一个位置。它必须不是一个指针类型，
	// 因为它指向 buf 的末尾之外，并且必须在没有写屏障的情况下更新。
	end uintptr

	// buf 存储了一系列指针，这些指针用于执行写屏障操作。
	// wbBufEntries 定义了缓冲区的大小。
	buf [wbBufEntries]uintptr
}

const (
	// wbBufEntries is the maximum number of pointers that can be
	// stored in the write barrier buffer.
	//
	// This trades latency for throughput amortization. Higher
	// values amortize flushing overhead more, but increase the
	// latency of flushing. Higher values also increase the cache
	// footprint of the buffer.
	//
	// TODO: What is the latency cost of this? Tune this value.
	wbBufEntries = 512

	// Maximum number of entries that we need to ask from the
	// buffer in a single call.
	wbMaxEntriesPerCall = 8
)

// reset 通过重置 next 和 end 指针来清空缓冲区 b，使其准备好接收新的写屏障操作
func (b *wbBuf) reset() {
	// 变量用于保存缓冲区起始位置的地址
	start := uintptr(unsafe.Pointer(&b.buf[0]))
	// next 指针被重置为 start，即缓冲区的起始位置。
	// 这意味着缓冲区现在准备好接收新的写屏障操作
	b.next = start
	if testSmallBuf {
		// end 指针被设置为缓冲区中的较小位置，用于测试目的
		b.end = uintptr(unsafe.Pointer(&b.buf[wbMaxEntriesPerCall+1]))
	} else {
		// end 指针被设置为缓冲区的实际末尾位置
		// 这是通过计算 buf 数组长度乘以其元素大小，并加上 start 得到的
		b.end = start + uintptr(len(b.buf))*unsafe.Sizeof(b.buf[0])
	}

	// 检查 next 和 end 之间的距离是否能被单个条目的大小整除。
	// 如果不能整除，则抛出错误，指示缓冲区边界有问题
	if (b.end-b.next)%unsafe.Sizeof(b.buf[0]) != 0 {
		throw("bad write barrier buffer bounds")
	}
}

// discard 重置缓冲区的状态，但只重置 next 指针而不重置 end 指针
//
// 这个方法必须是 nosplit，因为它由 wbBufFlush 调用。
//
//go:nosplit
func (b *wbBuf) discard() {
	b.next = uintptr(unsafe.Pointer(&b.buf[0]))
}

// empty 报告 B 是否不包含指针。
func (b *wbBuf) empty() bool {
	return b.next == uintptr(unsafe.Pointer(&b.buf[0]))
}

// getX 返回写屏障缓冲区中用于存储 x 个指针的空间。
// 函数为写屏障缓冲区提供了用于存储新引用的位置
//
// getX 必要时会刷新缓冲区。调用者应使用此函数如下：
//
//	buf := &getg().m.p.ptr().wbBuf
//	p := buf.get1()
//	p[0] = old
//	... 实际内存写操作 ...
//	... 完成写屏障下的缓冲区写入 ...
//
// 调用者必须确保在上述序列期间没有抢占点。当 buf 正在使用时，
// 必须没有抢占点，因为它是每个 P 的资源。在缓冲区 put 和写入
// 内存之间也必须没有抢占点，因为这可能会允许 GC 阶段改变，
// 导致错过写屏障。
//
// getX 必须是 nowritebarrierrec，因为在写屏障中出现写屏障
// 会导致缓冲区损坏。它（以及它调用的任何东西，如果有的话）
// 必须是 nosplit，以避免调度到不同的 P 和不同的缓冲区。
//
//go:nowritebarrierrec
//go:nosplit
func (b *wbBuf) get1() *[1]uintptr {
	// 如果写屏障缓冲区已满，刷新缓冲区。
	if b.next+goarch.PtrSize > b.end {
		wbBufFlush()
	}

	// 获取写屏障缓冲区中的下一个可用位置。
	p := (*[1]uintptr)(unsafe.Pointer(b.next))

	// 更新 next 指针，指向缓冲区中的下一个可用位置。
	b.next += goarch.PtrSize

	// 返回用于存储指针的切片。
	return p
}

//go:nowritebarrierrec
//go:nosplit
func (b *wbBuf) get2() *[2]uintptr {
	if b.next+2*goarch.PtrSize > b.end {
		wbBufFlush()
	}
	p := (*[2]uintptr)(unsafe.Pointer(b.next))
	b.next += 2 * goarch.PtrSize
	return p
}

// wbBufFlush 将当前 P 的写屏障缓冲区刷新到垃圾回收的工作缓冲区。
//
// 本函数不允许包含写屏障，因为它本身就是写屏障实现的一部分。
//
// 本函数及其调用的所有函数都必须不包含分割点（nosplit），因为：
// 1) 栈中包含来自 gcWriteBarrier 的未类型化的槽位。
// 2) 在调用者中的写屏障测试和刷新缓冲区之间不能有 GC 安全点。
//
// TODO: 一个 "go:nosplitrec" 注解对于这个函数来说非常合适。
//
//go:nowritebarrierrec
//go:nosplit
func wbBufFlush() {
	// 注意：本函数中的每一个可能的返回路径都必须重置缓冲区的 next 指针，
	// 以防止缓冲区溢出。
	// 本函数不允许包含写屏障，因为它本身就是写屏障实现的一部分

	// 如果当前 goroutine 正在关闭 (getg().m.dying > 0)，则直接丢弃写屏障缓冲区的内容
	if getg().m.dying > 0 {
		// 重置缓冲区的状态，但只重置 next 指针而不重置 end 指针
		getg().m.p.ptr().wbBuf.discard()
		return
	}

	// 切换到系统栈，以避免 GC 安全点，确保写屏障操作的连续性
	systemstack(func() {
		wbBufFlush1(getg().m.p.ptr()) // 刷新写屏障缓冲区
	})
}

// wbBufFlush1 将 P 的写屏障缓冲区刷新到垃圾回收的工作队列。
//
// 本函数不允许包含写屏障，因为它本身就是写屏障实现的一部分，因此可能会导致无限循环或缓冲区损坏。
//
// 本函数必须是非抢占式的，因为它使用了 P 的工作缓冲区。
//
//go:nowritebarrierrec
//go:systemstack
func wbBufFlush1(pp *p) {
	// 获取缓冲区中的指针。
	start := uintptr(unsafe.Pointer(&pp.wbBuf.buf[0]))
	// 计算缓冲区中的指针数量
	n := (pp.wbBuf.next - start) / unsafe.Sizeof(pp.wbBuf.buf[0])
	// 获取缓冲区中的前 n 个指针
	ptrs := pp.wbBuf.buf[:n]

	// 将缓冲区的 next 指针设置为 0，防止在处理缓冲区期间有新的指针被加入
	pp.wbBuf.next = 0

	// 如果使用 Checkmark 模式，则遍历所有指针并调用 shade 函数将它们标记为灰色
	if useCheckmark {
		// 遍历所有指针
		for _, ptr := range ptrs {
			shade(ptr) // 将指针标记为灰色
		}
		pp.wbBuf.reset() // 重置写屏障缓冲区
		return
	}

	// 标记缓冲区中的所有指针，并只记录那些被标记为灰色的指针。
	// 我们使用缓冲区本身来临时记录被标记为灰色的指针。
	//
	// TODO: scanobject/scanblock 是否可以直接将指针放入 wbBuf？如果是这样，这将成为唯一的灰色标记路径。
	//
	// TODO: 如果栈已经被标记，我们可以避免标记缓冲区中的“新”指针，甚至完全避免将它们放入缓冲区（这将使缓冲区容量翻倍）。
	// 这对于缓冲区稍微有些复杂；我们可以跟踪是否有未标记的 goroutine 使用了缓冲区，或者全局跟踪是否有未标记的栈，并在每次栈扫描后刷新。

	// 获取当前 P 的垃圾回收工作缓冲区
	gcw := &pp.gcw
	// 初始化位置指针
	pos := 0
	// 遍历所有指针
	for _, ptr := range ptrs {
		// 过滤掉非法指针和已标记的指针
		if ptr < minLegalPointer {
			continue
		}

		// 查找指针所指向的对象
		obj, span, objIndex := findObject(ptr, 0, 0)
		if obj == 0 {
			continue
		}

		// TODO: 考虑采用两步法，第一步只是预取标记位。

		// 获取对象的标记位
		mbits := span.markBitsForIndex(objIndex)
		if mbits.isMarked() {
			continue
		}
		// 将对象标记为已标记
		mbits.setMarked()

		// 标记 span。
		arena, pageIdx, pageMask := pageIndexOf(span.base())
		if arena.pageMarks[pageIdx]&pageMask == 0 {
			// 标记 span 所在的页
			atomic.Or8(&arena.pageMarks[pageIdx], pageMask)
		}

		// 如果 span 无需扫描，则标记为黑色
		if span.spanclass.noscan() {
			gcw.bytesMarked += uint64(span.elemsize)
			continue
		}

		// 将对象加入到灰色指针数组中
		ptrs[pos] = obj
		pos++
	}

	// 将标记为灰色的对象加入到工作缓冲区中
	gcw.putBatch(ptrs[:pos])
	// 重置写屏障缓冲区
	pp.wbBuf.reset()
}
