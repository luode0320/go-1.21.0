// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"runtime/internal/atomic"
	"unsafe"
)

const _DWORD_MAX = 0xffffffff

const _INVALID_HANDLE_VALUE = ^uintptr(0)

// net_op must be the same as beginning of internal/poll.operation.
// Keep these in sync.
type net_op struct {
	// used by windows
	o overlapped
	// used by netpoll
	pd    *pollDesc
	mode  int32
	errno int32
	qty   uint32
}

type overlappedEntry struct {
	key      *pollDesc
	op       *net_op // In reality it's *overlapped, but we cast it to *net_op anyway.
	internal uintptr
	qty      uint32
}

var (
	iocphandle uintptr = _INVALID_HANDLE_VALUE // completion port io handle

	netpollWakeSig atomic.Uint32 // used to avoid duplicate calls of netpollBreak
)

func netpollinit() {
	iocphandle = stdcall4(_CreateIoCompletionPort, _INVALID_HANDLE_VALUE, 0, 0, _DWORD_MAX)
	if iocphandle == 0 {
		println("runtime: CreateIoCompletionPort failed (errno=", getlasterror(), ")")
		throw("runtime: netpollinit failed")
	}
}

func netpollIsPollDescriptor(fd uintptr) bool {
	return fd == iocphandle
}

func netpollopen(fd uintptr, pd *pollDesc) int32 {
	// TODO(iant): Consider using taggedPointer on 64-bit systems.
	if stdcall4(_CreateIoCompletionPort, fd, iocphandle, uintptr(unsafe.Pointer(pd)), 0) == 0 {
		return int32(getlasterror())
	}
	return 0
}

func netpollclose(fd uintptr) int32 {
	// nothing to do
	return 0
}

func netpollarm(pd *pollDesc, mode int) {
	throw("runtime: unused")
}

func netpollBreak() {
	// Failing to cas indicates there is an in-flight wakeup, so we're done here.
	if !netpollWakeSig.CompareAndSwap(0, 1) {
		return
	}

	if stdcall4(_PostQueuedCompletionStatus, iocphandle, 0, 0, 0) == 0 {
		println("runtime: netpoll: PostQueuedCompletionStatus failed (errno=", getlasterror(), ")")
		throw("runtime: netpoll: PostQueuedCompletionStatus failed")
	}
}

// 函数用于检查就绪的网络连接。运行时网络 I/O 的关键部分，
// 它利用平台的 IO 完成端口机制来高效地检测就绪的网络连接，并准备好相应的 goroutine 进行后续的网络操作
// 返回一个 goroutine 列表，表示这些 goroutine 的网络阻塞已经停止, 可以开始调度运行。
// delay 参数：
//
//	< 0: 无限期阻塞
//	== 0: 不阻塞，只进行轮询
//	> 0: 最多阻塞指定的纳秒数
func netpoll(delay int64) gList {
	var entries [64]overlappedEntry   // 初始化一个 overlappedEntry 数组用于存储 IO 完成端口的完成包。
	var wait, qty, flags, n, i uint32 // 初始化多个 uint32 变量用于存储各种计数和标志。
	var errno int32                   // 存储错误号。
	var op *net_op                    // 指向 net_op 结构体的指针。
	var toRun gList                   // 初始化一个 goroutine 列表。

	// 获取当前 goroutine 所在的 m 结构体。
	mp := getg().m

	// 如果 IO 完成端口未初始化，直接返回空列表。
	if iocphandle == _INVALID_HANDLE_VALUE {
		return gList{}
	}

	// 根据 delay 参数计算等待时间。
	if delay < 0 {
		wait = _INFINITE // 无限期等待。
	} else if delay == 0 {
		wait = 0 // 不等待，立即返回。
	} else if delay < 1e6 {
		wait = 1 // 小于 1 毫秒的延迟视为 1 毫秒。
	} else if delay < 1e15 {
		wait = uint32(delay / 1e6) // 超过 1 毫秒但小于 1e15 纳秒的延迟转换为毫秒。
	} else {
		// 任意上限，限制等待时间，1e9 毫秒大约等于 11.5 天。
		wait = 1e9
	}

	// 计算要查询的条目数量，至少为 8，最多为 entries 数组长度除以 gomaxprocs。
	n = uint32(len(entries) / int(gomaxprocs))
	if n < 8 {
		n = 8
	}

	// 如果延迟不为 0，标记当前 m 结构体为阻塞状态。
	if delay != 0 {
		mp.blocked = true
	}

	// 调用 GetQueuedCompletionStatusEx 函数等待完成包 n。
	if stdcall6(_GetQueuedCompletionStatusEx, iocphandle, uintptr(unsafe.Pointer(&entries[0])), uintptr(n), uintptr(unsafe.Pointer(&n)), uintptr(wait), 0) == 0 {
		mp.blocked = false            // 清除阻塞状态。
		errno = int32(getlasterror()) // 获取最后的错误码。
		if errno == _WAIT_TIMEOUT {   // 如果超时，则返回空列表。
			return gList{}
		}
		println("runtime: GetQueuedCompletionStatusEx failed (errno=", errno, ")")
		throw("runtime: netpoll failed") // 其他错误，抛出异常。
	}

	// 清除阻塞状态。
	mp.blocked = false

	// 处理每个完成包。
	for i = 0; i < n; i++ {
		// 获取操作指针。
		op = entries[i].op
		// 如果操作有效且关联的 pd 与完成包的 key 匹配。
		if op != nil && op.pd == entries[i].key {
			errno = 0
			qty = 0
			// 调用 WSAGetOverlappedResult 函数获取重叠操作的状态。
			if stdcall5(_WSAGetOverlappedResult, op.pd.fd, uintptr(unsafe.Pointer(op)), uintptr(unsafe.Pointer(&qty)), 0, uintptr(unsafe.Pointer(&flags))) == 0 {
				errno = int32(getlasterror()) // 获取最后的错误码。
			}
			// 处理完成的操作，更新可运行的 goroutine 列表。
			handlecompletion(&toRun, op, errno, qty)
		} else {
			// 如果完成包无效，清除 netpollWakeSig。
			netpollWakeSig.Store(0)
			// 如果延迟为 0，将通知转发给阻塞的轮询器。
			if delay == 0 {
				// Forward the notification to the
				// blocked poller.
				netpollBreak()
			}
		}
	}

	// 返回可运行的 goroutine 列表。
	return toRun
}

func handlecompletion(toRun *gList, op *net_op, errno int32, qty uint32) {
	mode := op.mode
	if mode != 'r' && mode != 'w' {
		println("runtime: GetQueuedCompletionStatusEx returned invalid mode=", mode)
		throw("runtime: netpoll failed")
	}
	op.errno = errno
	op.qty = qty
	netpollready(toRun, op.pd, mode)
}
