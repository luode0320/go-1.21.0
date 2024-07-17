// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"sync/atomic"
)

// Once 是一个仅执行一次操作的对象。
//
// 一个 Once 在第一次使用后不应再复制。
type Once struct {
	done uint32 // done 表示操作是否已经执行。
	m    Mutex  // 同步锁
}

// Do 方法只有在首次针对该 Once 实例调用 Do 时才调用函数 f。
func (o *Once) Do(f func()) {
	// 原子操作, 判断是否为 0, 未调用
	if atomic.LoadUint32(&o.done) == 0 {
		// 已实现慢路径以允许快路径内联。
		o.doSlow(f)
	}
}

func (o *Once) doSlow(f func()) {
	o.m.Lock()
	defer o.m.Unlock()

	// 加锁, 双重判断, 类似于单例模式
	if o.done == 0 {
		// 原子操作, 修改为, 已调用
		defer atomic.StoreUint32(&o.done, 1)

		f()
	}
}
