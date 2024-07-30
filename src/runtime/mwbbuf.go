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

// wbBuf is a per-P buffer of pointers queued by the write barrier.
// This buffer is flushed to the GC workbufs when it fills up and on
// various GC transitions.
//
// This is closely related to a "sequential store buffer" (SSB),
// except that SSBs are usually used for maintaining remembered sets,
// while this is used for marking.
type wbBuf struct {
	// next points to the next slot in buf. It must not be a
	// pointer type because it can point past the end of buf and
	// must be updated without write barriers.
	//
	// This is a pointer rather than an index to optimize the
	// write barrier assembly.
	next uintptr

	// end points to just past the end of buf. It must not be a
	// pointer type because it points past the end of buf and must
	// be updated without write barriers.
	end uintptr

	// buf stores a series of pointers to execute write barriers on.
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

// reset empties b by resetting its next and end pointers.
func (b *wbBuf) reset() {
	start := uintptr(unsafe.Pointer(&b.buf[0]))
	b.next = start
	if testSmallBuf {
		// For testing, make the buffer smaller but more than
		// 1 write barrier's worth, so it tests both the
		// immediate flush and delayed flush cases.
		b.end = uintptr(unsafe.Pointer(&b.buf[wbMaxEntriesPerCall+1]))
	} else {
		b.end = start + uintptr(len(b.buf))*unsafe.Sizeof(b.buf[0])
	}

	if (b.end-b.next)%unsafe.Sizeof(b.buf[0]) != 0 {
		throw("bad write barrier buffer bounds")
	}
}

// discard resets b's next pointer, but not its end pointer.
//
// This must be nosplit because it's called by wbBufFlush.
//
//go:nosplit
func (b *wbBuf) discard() {
	b.next = uintptr(unsafe.Pointer(&b.buf[0]))
}

// empty reports whether b contains no pointers.
func (b *wbBuf) empty() bool {
	return b.next == uintptr(unsafe.Pointer(&b.buf[0]))
}

// getX returns space in the write barrier buffer to store X pointers.
// getX will flush the buffer if necessary. Callers should use this as:
//
//	buf := &getg().m.p.ptr().wbBuf
//	p := buf.get2()
//	p[0], p[1] = old, new
//	... actual memory write ...
//
// The caller must ensure there are no preemption points during the
// above sequence. There must be no preemption points while buf is in
// use because it is a per-P resource. There must be no preemption
// points between the buffer put and the write to memory because this
// could allow a GC phase change, which could result in missed write
// barriers.
//
// getX must be nowritebarrierrec to because write barriers here would
// corrupt the write barrier buffer. It (and everything it calls, if
// it called anything) has to be nosplit to avoid scheduling on to a
// different P and a different buffer.
//
//go:nowritebarrierrec
//go:nosplit
func (b *wbBuf) get1() *[1]uintptr {
	if b.next+goarch.PtrSize > b.end {
		wbBufFlush()
	}
	p := (*[1]uintptr)(unsafe.Pointer(b.next))
	b.next += goarch.PtrSize
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
		// 丢弃当前 P 的写屏障缓冲区
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
