// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/goarch"
	"runtime/internal/atomic"
	"runtime/internal/sys"
	"unsafe"
)

const (
	_WorkbufSize = 2048 // in bytes; larger values result in less contention

	// workbufAlloc is the number of bytes to allocate at a time
	// for new workbufs. This must be a multiple of pageSize and
	// should be a multiple of _WorkbufSize.
	//
	// Larger values reduce workbuf allocation overhead. Smaller
	// values reduce heap fragmentation.
	workbufAlloc = 32 << 10
)

func init() {
	if workbufAlloc%pageSize != 0 || workbufAlloc%_WorkbufSize != 0 {
		throw("bad workbufAlloc")
	}
}

// Garbage collector work pool abstraction.
//
// This implements a producer/consumer model for pointers to grey
// objects. A grey object is one that is marked and on a work
// queue. A black object is marked and not on a work queue.
//
// Write barriers, root discovery, stack scanning, and object scanning
// produce pointers to grey objects. Scanning consumes pointers to
// grey objects, thus blackening them, and then scans them,
// potentially producing new pointers to grey objects.

// A gcWork provides the interface to produce and consume work for the
// garbage collector.
//
// A gcWork can be used on the stack as follows:
//
//	(preemption must be disabled)
//	gcw := &getg().m.p.ptr().gcw
//	.. call gcw.put() to produce and gcw.tryGet() to consume ..
//
// It's important that any use of gcWork during the mark phase prevent
// the garbage collector from transitioning to mark termination since
// gcWork may locally hold GC work buffers. This can be done by
// disabling preemption (systemstack or acquirem).
type gcWork struct {
	// wbuf1 和 wbuf2 是主要和次要的工作缓冲区。
	//
	// 可以将这两个工作缓冲区的指针视为一个堆栈的组合。当我们在堆栈中弹出最后一个指针时，
	// 我们会将堆栈向上移动一个工作缓冲区，通过引入一个新的满缓冲区并丢弃一个空缓冲区。
	// 当我们填充了两个缓冲区时，我们会将堆栈向下移动一个工作缓冲区，通过引入一个新的空缓冲区
	// 并丢弃一个满缓冲区。这样我们就有了一个缓冲区的滞回效果，这使得获取或放置工作缓冲区的成本
	// 被摊销到了至少一个缓冲区的工作量上，并减少了对全局工作列表的竞争。
	//
	// wbuf1 总是当前我们正在推送和弹出的缓冲区，而 wbuf2 是下一个将被丢弃的缓冲区。
	//
	// 不变式：wbuf1 和 wbuf2 要么都为 nil，要么都不为 nil。
	wbuf1, wbuf2 *workbuf

	// 是一个无符号 64 位整数，表示在这个 gcWork 上标记（黑化）的字节数。
	// 这个值会在 dispose 函数中被汇总到 work.bytesMarked 中。
	// work.bytesMarked 用于跟踪整个垃圾回收过程中标记的总字节数。
	bytesMarked uint64

	// heapScanWork 表示在这个 gcWork 上执行的堆扫描工作量。
	// 这个工作量会被汇总到 gcController 中，并且可能会被调用者冲刷。
	// 其他类型的扫描工作则会立即冲刷。
	heapScanWork int64

	// 是一个有符号 64 位整数，表示在这个 gcWork 上执行的堆扫描工作量。
	// 这个值会在 dispose 函数中被汇总到 gcController 中。
	// gcController 用于控制垃圾回收的进度和效率。
	// 其他类型的扫描工作（非堆扫描工作）会立即被清除。
	flushedWork bool
}

// Most of the methods of gcWork are go:nowritebarrierrec because the
// write barrier itself can invoke gcWork methods but the methods are
// not generally re-entrant. Hence, if a gcWork method invoked the
// write barrier while the gcWork was in an inconsistent state, and
// the write barrier in turn invoked a gcWork method, it could
// permanently corrupt the gcWork.

func (w *gcWork) init() {
	w.wbuf1 = getempty()
	wbuf2 := trygetfull()
	if wbuf2 == nil {
		wbuf2 = getempty()
	}
	w.wbuf2 = wbuf2
}

// put 方法将一个指针加入垃圾回收器的工作队列中以供追踪。
// obj 必须指向堆对象或 oblet 的起始位置。
//
//go:nowritebarrierrec
func (w *gcWork) put(obj uintptr) {
	flushed := false
	wbuf := w.wbuf1
	// 记录这个方法可能会获取 wbufSpans 或 heap 锁来分配一个工作缓冲区。
	lockWithRankMayAcquire(&work.wbufSpans.lock, lockRankWbufSpans)
	lockWithRankMayAcquire(&mheap_.lock, lockRankMheap)

	if wbuf == nil { // 如果 wbuf1 为空，则初始化工作缓冲区。
		w.init()
		wbuf = w.wbuf1
	} else if wbuf.nobj == len(wbuf.obj) { // 如果 wbuf1 已经满了。
		// 交换 wbuf1 和 wbuf2。
		w.wbuf1, w.wbuf2 = w.wbuf2, w.wbuf1
		wbuf = w.wbuf1

		// 如果交换后 wbuf1 仍然满了。
		if wbuf.nobj == len(wbuf.obj) {
			putfull(wbuf)        // 将 wbuf 放回全局队列。
			w.flushedWork = true // 标记为已冲刷。
			wbuf = getempty()    // 获取一个新的空工作缓冲区。
			w.wbuf1 = wbuf       // 赋值
			flushed = true       // 标记为已冲刷。
		}
	}

	wbuf.obj[wbuf.nobj] = obj // 将对象放入工作缓冲区。
	wbuf.nobj++               // 增加工作缓冲区中的对象数量。

	// 如果我们已经将一个缓冲区的工作放回到全局队列，则通知 GC 控制器，
	// 以便它可以鼓励更多的工作者运行。我们延迟到 put 方法的末尾再做这件事，
	// 因为 enlistWorker 可能会自身操纵 w，所以我们需要确保 w 处于一致的状态。
	if flushed && gcphase == _GCmark {
		gcController.enlistWorker() // 唤醒一个工作者。
	}
}

// putFast 方法尝试快速地将一个对象放入工作缓冲区，并报告是否成功。
// 如果不能快速放入，则返回 false，此时调用者需要调用 put 方法。
//
//go:nowritebarrierrec
func (w *gcWork) putFast(obj uintptr) bool {
	// 获取当前的工作缓冲区 wbuf1。
	wbuf := w.wbuf1

	// 如果 wbuf1 为空，或者已经达到最大容量，则不能快速放入。
	if wbuf == nil || wbuf.nobj == len(wbuf.obj) {
		return false
	}

	wbuf.obj[wbuf.nobj] = obj // 将对象放入工作缓冲区。
	wbuf.nobj++               // 增加工作缓冲区中的对象数量。
	return true
}

// putBatch performs a put on every pointer in obj. See put for
// constraints on these pointers.
//
//go:nowritebarrierrec
func (w *gcWork) putBatch(obj []uintptr) {
	if len(obj) == 0 {
		return
	}

	flushed := false
	wbuf := w.wbuf1
	if wbuf == nil {
		w.init()
		wbuf = w.wbuf1
	}

	for len(obj) > 0 {
		for wbuf.nobj == len(wbuf.obj) {
			putfull(wbuf)
			w.flushedWork = true
			w.wbuf1, w.wbuf2 = w.wbuf2, getempty()
			wbuf = w.wbuf1
			flushed = true
		}
		n := copy(wbuf.obj[wbuf.nobj:], obj)
		wbuf.nobj += n
		obj = obj[n:]
	}

	if flushed && gcphase == _GCmark {
		gcController.enlistWorker()
	}
}

// tryGet 从垃圾回收器的工作队列中获取一个指针以供追踪。
//
// 如果当前的 gcWork 或全局队列中没有剩余的指针，则 tryGet 返回 0。
// 注意，即使返回 0，也可能存在其他 gcWork 实例或其他缓存中仍有待处理的指针。
//
//go:nowritebarrierrec
func (w *gcWork) tryGet() uintptr {
	// 获取 gcWork 结构中的工作缓冲区 wbuf
	wbuf := w.wbuf1
	// 如果 wbuf 为空 (nil)，则调用 init 方法初始化 gcWork，然后再次获取 wbuf
	if wbuf == nil {
		w.init()
		wbuf = w.wbuf1
		// 此时 wbuf 是空的。
	}

	// 如果当前 wbuf 中没有对象，则尝试通过交换 wbuf1 和 wbuf2 来获取另一个缓冲区中的对象
	if wbuf.nobj == 0 {
		w.wbuf1, w.wbuf2 = w.wbuf2, w.wbuf1
		wbuf = w.wbuf1

		// 如果交换后的 wbuf 依然为空，则尝试从全局队列中获取一个缓冲区
		if wbuf.nobj == 0 {
			owbuf := wbuf
			wbuf = trygetfull() // 尝试从全局队列 work.full 中获取一个缓冲区
			if wbuf == nil {
				return 0
			}
			// 如果成功获取到非空的缓冲区，则将原来的空缓冲区 owbuf 放回空缓冲区队列中
			putempty(owbuf)
			// 并将新的非空缓冲区设置为 wbuf1
			w.wbuf1 = wbuf
		}
	}

	// 从缓冲区中取出一个对象，并将 nobj 减少 1，表示该对象已被取出
	wbuf.nobj--
	return wbuf.obj[wbuf.nobj]
}

// tryGetFast 尝试快速地从垃圾回收器的工作队列中获取一个指针以供追踪。
// 如果有立即可用的指针，则返回该指针；否则返回 0，并期望调用者随后调用 tryGet()。
//
//go:nowritebarrierrec
func (w *gcWork) tryGetFast() uintptr {
	// 获取 gcWork 结构中的工作缓冲区 wbuf
	wbuf := w.wbuf1
	// 如果 wbuf 为空 (nil) 或者 nobj（表示缓冲区中的对象数量）为 0，则表示缓冲区为空，没有对象可供追踪
	if wbuf == nil || wbuf.nobj == 0 {
		return 0
	}

	// 如果缓冲区中有对象，则从缓冲区的末尾获取一个对象，并将 nobj 减少 1，表示该对象已被取出
	wbuf.nobj--
	return wbuf.obj[wbuf.nobj]
}

// dispose returns any cached pointers to the global queue.
// The buffers are being put on the full queue so that the
// write barriers will not simply reacquire them before the
// GC can inspect them. This helps reduce the mutator's
// ability to hide pointers during the concurrent mark phase.
//
//go:nowritebarrierrec
func (w *gcWork) dispose() {
	if wbuf := w.wbuf1; wbuf != nil {
		if wbuf.nobj == 0 {
			putempty(wbuf)
		} else {
			putfull(wbuf)
			w.flushedWork = true
		}
		w.wbuf1 = nil

		wbuf = w.wbuf2
		if wbuf.nobj == 0 {
			putempty(wbuf)
		} else {
			putfull(wbuf)
			w.flushedWork = true
		}
		w.wbuf2 = nil
	}
	if w.bytesMarked != 0 {
		// dispose happens relatively infrequently. If this
		// atomic becomes a problem, we should first try to
		// dispose less and if necessary aggregate in a per-P
		// counter.
		atomic.Xadd64(&work.bytesMarked, int64(w.bytesMarked))
		w.bytesMarked = 0
	}
	if w.heapScanWork != 0 {
		gcController.heapScanWork.Add(w.heapScanWork)
		w.heapScanWork = 0
	}
}

// balance 方法将一些缓存在当前 gcWork 中的工作移回到全局队列。
//
//go:nowritebarrierrec
func (w *gcWork) balance() {
	// 如果 wbuf1 为空，则表明没有缓存的工作，直接返回。
	if w.wbuf1 == nil {
		return
	}

	// 如果 wbuf2 不为空且包含对象，则将 wbuf2 中的工作放回全局队列。
	if wbuf := w.wbuf2; wbuf.nobj != 0 {
		putfull(wbuf)        // 将 wbuf2 放回全局队列。
		w.flushedWork = true // 标记为已冲刷。
		w.wbuf2 = getempty() // 重新获取一个空的工作缓冲区。
	} else if wbuf := w.wbuf1; wbuf.nobj > 4 {
		// 如果 wbuf1 包含超过 4 个对象，则将 wbuf1 中的工作放回全局队列。
		w.wbuf1 = handoff(wbuf) // 将 wbuf1 中的工作放回全局队列，并更新 wbuf1。
		w.flushedWork = true    // 标记为已冲刷。
	} else {
		return
	}

	// 我们已经将一个缓冲区的工作放回到了全局队列，因此唤醒一个工作者。
	if gcphase == _GCmark {
		gcController.enlistWorker() // 唤醒一个工作者。
	}
}

// 方法报告当前 gcWork 是否没有可标记的工作。
//
//go:nowritebarrierrec
func (w *gcWork) empty() bool {
	// 检查 wbuf1 是否为空，或者 wbuf1 和 wbuf2 都没有待标记的对象。
	return w.wbuf1 == nil || (w.wbuf1.nobj == 0 && w.wbuf2.nobj == 0)
}

// Internally, the GC work pool is kept in arrays in work buffers.
// The gcWork interface caches a work buffer until full (or empty) to
// avoid contending on the global work buffer lists.

// 结构体表示工作缓冲区的头部信息。
// 它包含了工作缓冲区的关键元数据。
//
// node 是一个轻量级的节点，用于将工作缓冲区链接到双向链表中。
// nobj 表示工作缓冲区中当前存储的对象数量。
type workbufhdr struct {
	node lfnode // 是一个轻量级的节点，用于将工作缓冲区链接到双向链表中。
	nobj int    // 工作缓冲区中当前存储的对象数量
}

type workbuf struct {
	_ sys.NotInHeap
	workbufhdr
	// account for the above fields
	obj [(_WorkbufSize - unsafe.Sizeof(workbufhdr{})) / goarch.PtrSize]uintptr
}

// workbuf factory routines. These funcs are used to manage the
// workbufs.
// If the GC asks for some work these are the only routines that
// make wbufs available to the GC.

func (b *workbuf) checknonempty() {
	if b.nobj == 0 {
		throw("workbuf is empty")
	}
}

func (b *workbuf) checkempty() {
	if b.nobj != 0 {
		throw("workbuf is not empty")
	}
}

// getempty pops an empty work buffer off the work.empty list,
// allocating new buffers if none are available.
//
//go:nowritebarrier
func getempty() *workbuf {
	var b *workbuf
	if work.empty != 0 {
		b = (*workbuf)(work.empty.pop())
		if b != nil {
			b.checkempty()
		}
	}
	// Record that this may acquire the wbufSpans or heap lock to
	// allocate a workbuf.
	lockWithRankMayAcquire(&work.wbufSpans.lock, lockRankWbufSpans)
	lockWithRankMayAcquire(&mheap_.lock, lockRankMheap)
	if b == nil {
		// Allocate more workbufs.
		var s *mspan
		if work.wbufSpans.free.first != nil {
			lock(&work.wbufSpans.lock)
			s = work.wbufSpans.free.first
			if s != nil {
				work.wbufSpans.free.remove(s)
				work.wbufSpans.busy.insert(s)
			}
			unlock(&work.wbufSpans.lock)
		}
		if s == nil {
			systemstack(func() {
				s = mheap_.allocManual(workbufAlloc/pageSize, spanAllocWorkBuf)
			})
			if s == nil {
				throw("out of memory")
			}
			// Record the new span in the busy list.
			lock(&work.wbufSpans.lock)
			work.wbufSpans.busy.insert(s)
			unlock(&work.wbufSpans.lock)
		}
		// Slice up the span into new workbufs. Return one and
		// put the rest on the empty list.
		for i := uintptr(0); i+_WorkbufSize <= workbufAlloc; i += _WorkbufSize {
			newb := (*workbuf)(unsafe.Pointer(s.base() + i))
			newb.nobj = 0
			lfnodeValidate(&newb.node)
			if i == 0 {
				b = newb
			} else {
				putempty(newb)
			}
		}
	}
	return b
}

// putempty puts a workbuf onto the work.empty list.
// Upon entry this goroutine owns b. The lfstack.push relinquishes ownership.
//
//go:nowritebarrier
func putempty(b *workbuf) {
	b.checkempty()
	work.empty.push(&b.node)
}

// putfull puts the workbuf on the work.full list for the GC.
// putfull accepts partially full buffers so the GC can avoid competing
// with the mutators for ownership of partially full buffers.
//
//go:nowritebarrier
func putfull(b *workbuf) {
	b.checknonempty()
	work.full.push(&b.node)
}

// trygetfull 尝试从全局队列 work.full 中获取一个缓冲区
// 如果没有立即可用的工作缓冲区，则返回 nil。
//
//go:nowritebarrier
func trygetfull() *workbuf {
	// 从 full 队列中尝试弹出一个工作缓冲区。
	b := (*workbuf)(work.full.pop())
	if b != nil {
		b.checknonempty() // 确保工作缓冲区不是空的。
		return b          // 返回找到的工作缓冲区
	}

	return b // 如果没有找到，则返回 nil。
}

//go:nowritebarrier
func handoff(b *workbuf) *workbuf {
	// Make new buffer with half of b's pointers.
	b1 := getempty()
	n := b.nobj / 2
	b.nobj -= n
	b1.nobj = n
	memmove(unsafe.Pointer(&b1.obj[0]), unsafe.Pointer(&b.obj[b.nobj]), uintptr(n)*unsafe.Sizeof(b1.obj[0]))

	// Put b on full list - let first half of b get stolen.
	putfull(b)
	return b1
}

// prepareFreeWorkbufs moves busy workbuf spans to free list so they
// can be freed to the heap. This must only be called when all
// workbufs are on the empty list.
func prepareFreeWorkbufs() {
	lock(&work.wbufSpans.lock)
	if work.full != 0 {
		throw("cannot free workbufs when work.full != 0")
	}
	// Since all workbufs are on the empty list, we don't care
	// which ones are in which spans. We can wipe the entire empty
	// list and move all workbuf spans to the free list.
	work.empty = 0
	work.wbufSpans.free.takeAll(&work.wbufSpans.busy)
	unlock(&work.wbufSpans.lock)
}

// freeSomeWbufs frees some workbufs back to the heap and returns
// true if it should be called again to free more.
func freeSomeWbufs(preemptible bool) bool {
	const batchSize = 64 // ~1–2 µs per span.
	lock(&work.wbufSpans.lock)
	if gcphase != _GCoff || work.wbufSpans.free.isEmpty() {
		unlock(&work.wbufSpans.lock)
		return false
	}
	systemstack(func() {
		gp := getg().m.curg
		for i := 0; i < batchSize && !(preemptible && gp.preempt); i++ {
			span := work.wbufSpans.free.first
			if span == nil {
				break
			}
			work.wbufSpans.free.remove(span)
			mheap_.freeManual(span, spanAllocWorkBuf)
		}
	})
	more := !work.wbufSpans.free.isEmpty()
	unlock(&work.wbufSpans.lock)
	return more
}
