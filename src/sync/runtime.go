// Copyright 2012 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import "unsafe"

// defined in package runtime

// Semacquire waits until *s > 0 and then atomically decrements it.
// It is intended as a simple sleep primitive for use by the synchronization
// library and should not be used directly.
func runtime_Semacquire(s *uint32)

// runtime_SemacquireMutex(RW) 与 Semacquire 类似，但用于对竞争的 Mutex 和 RWMutex 进行性能分析。
// 如果 lifo 为 true，则将等待者排在等待队列的头部。
// skipframes 表示在跟踪时要省略的帧数，从 runtime_SemacquireMutex 的调用者开始计算。
// 这个函数的不同形式仅告诉运行时如何在回溯中呈现等待的原因，并用于计算一些指标。
// 否则，它们在功能上是相同的。
func runtime_SemacquireMutex(s *uint32, lifo bool, skipframes int)
func runtime_SemacquireRWMutexR(s *uint32, lifo bool, skipframes int)
func runtime_SemacquireRWMutex(s *uint32, lifo bool, skipframes int)

// 原子性地递增 *s，并且如果有 Goroutine 在 Semacquire 中被阻塞，通知它
// 这是一个简单的唤醒原语，供同步库使用，不应直接使用
// 如果 handoff 为 true，则直接将计数传递给第一个等待者
// skipframes 表示跟踪时要省略的帧数，从 runtime_Semrelease 的调用者开始计数
func runtime_Semrelease(s *uint32, handoff bool, skipframes int)

// See runtime/sema.go for documentation.
func runtime_notifyListAdd(l *notifyList) uint32

// See runtime/sema.go for documentation.
func runtime_notifyListWait(l *notifyList, t uint32)

// See runtime/sema.go for documentation.
func runtime_notifyListNotifyAll(l *notifyList)

// See runtime/sema.go for documentation.
func runtime_notifyListNotifyOne(l *notifyList)

// Ensure that sync and runtime agree on size of notifyList.
func runtime_notifyListCheck(size uintptr)
func init() {
	var n notifyList
	runtime_notifyListCheck(unsafe.Sizeof(n))
}

// 主动旋转运行时支持。
// runtime_canSpin 报告目前旋转是否有意义。
func runtime_canSpin(i int) bool

// 自旋。
func runtime_doSpin()

func runtime_nanotime() int64
