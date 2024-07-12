// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build amd64 || 386

package runtime

import (
	"internal/goarch"
	"unsafe"
)

// 调整 Gobuf（goroutine 的缓冲区），使其看起来像执行了一次对 fn 的调用，
// 使用 ctxt 作为上下文，并且在 fn 的第一条指令前停止。
func gostartcall(buf *gobuf, fn, ctxt unsafe.Pointer) {
	// 获取当前栈指针（SP）
	sp := buf.sp

	// 预留返回值空间
	sp -= goarch.PtrSize
	// 在栈上存储返回地址，通常是当前的程序计数器（PC）
	// 将当前的程序计数器（PC）存储在新的栈指针位置上
	*(*uintptr)(unsafe.Pointer(sp)) = buf.pc
	// 更新栈指针
	buf.sp = sp
	// 设置新的程序计数器（PC），指向即将调用的函数
	// 这时候的pc才是 goroutine 的函数
	buf.pc = uintptr(fn)
	// 设置上下文（ctxt），这可能包含了函数调用所需的额外信息
	buf.ctxt = ctxt
}
