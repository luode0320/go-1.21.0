// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/abi"
	"internal/goarch"
	"runtime/internal/math"
	"runtime/internal/sys"
	"unsafe"
)

type slice struct {
	array unsafe.Pointer // 数组指针
	len   int            // 长度
	cap   int            // 容量
}

// A notInHeapSlice is a slice backed by runtime/internal/sys.NotInHeap memory.
type notInHeapSlice struct {
	array *notInHeap
	len   int
	cap   int
}

func panicmakeslicelen() {
	panic(errorString("makeslice: len out of range"))
}

func panicmakeslicecap() {
	panic(errorString("makeslice: cap out of range"))
}

// makeslicecopy allocates a slice of "tolen" elements of type "et",
// then copies "fromlen" elements of type "et" into that new allocation from "from".
func makeslicecopy(et *_type, tolen int, fromlen int, from unsafe.Pointer) unsafe.Pointer {
	var tomem, copymem uintptr
	if uintptr(tolen) > uintptr(fromlen) {
		var overflow bool
		tomem, overflow = math.MulUintptr(et.Size_, uintptr(tolen))
		if overflow || tomem > maxAlloc || tolen < 0 {
			panicmakeslicelen()
		}
		copymem = et.Size_ * uintptr(fromlen)
	} else {
		// fromlen is a known good length providing and equal or greater than tolen,
		// thereby making tolen a good slice length too as from and to slices have the
		// same element width.
		tomem = et.Size_ * uintptr(tolen)
		copymem = tomem
	}

	var to unsafe.Pointer
	if et.PtrBytes == 0 {
		to = mallocgc(tomem, nil, false)
		if copymem < tomem {
			memclrNoHeapPointers(add(to, copymem), tomem-copymem)
		}
	} else {
		// Note: can't use rawmem (which avoids zeroing of memory), because then GC can scan uninitialized memory.
		to = mallocgc(tomem, et, true)
		if copymem > 0 && writeBarrier.enabled {
			// Only shade the pointers in old.array since we know the destination slice to
			// only contains nil pointers because it has been cleared during alloc.
			bulkBarrierPreWriteSrcOnly(uintptr(to), uintptr(from), copymem)
		}
	}

	if raceenabled {
		callerpc := getcallerpc()
		pc := abi.FuncPCABIInternal(makeslicecopy)
		racereadrangepc(from, copymem, callerpc, pc)
	}
	if msanenabled {
		msanread(from, copymem)
	}
	if asanenabled {
		asanread(from, copymem)
	}

	memmove(to, from, copymem)

	return to
}

// 创建切片
func makeslice(et *_type, len, cap int) unsafe.Pointer {
	// 计算分配内存的总大小，并检查溢出以及内存是否超出最大值
	mem, overflow := math.MulUintptr(et.Size_, uintptr(cap))
	if overflow || mem > maxAlloc || len < 0 || len > cap {
		// 注意：当有人执行 make([]T, 大数) 时，生成 'len 超出范围' 错误而不是 'cap 超出范围' 错误。
		// 'cap 超出范围' 也是正确的，但由于 cap 只是隐式提供的，说明 len 更清晰。
		// 参见 golang.org/issue/4085。
		// 如果长度超出范围，抛出异常
		mem, overflow := math.MulUintptr(et.Size_, uintptr(len))
		if overflow || mem > maxAlloc || len < 0 {
			panicmakeslicelen()
		}
		panicmakeslicecap()
	}

	// 分配内存空间并返回指向该内存空间的指针
	return mallocgc(mem, et, true)
}

func makeslice64(et *_type, len64, cap64 int64) unsafe.Pointer {
	len := int(len64)
	if int64(len) != len64 {
		panicmakeslicelen()
	}

	cap := int(cap64)
	if int64(cap) != cap64 {
		panicmakeslicecap()
	}

	return makeslice(et, len, cap)
}

// This is a wrapper over runtime/internal/math.MulUintptr,
// so the compiler can recognize and treat it as an intrinsic.
func mulUintptr(a, b uintptr) (uintptr, bool) {
	return math.MulUintptr(a, b)
}

// 函数用于在 slice 需要扩容时分配新的底层数组。
// 它接受旧的数组指针、新的长度、旧的容量、要添加的元素数量和元素类型，
// 并返回新的数组指针、新的长度和新的容量。
func growslice(oldPtr unsafe.Pointer, newLen, oldCap, num int, et *_type) slice {
	// 计算原有的长度
	oldLen := newLen - num

	// 如果启用了竞态检测、内存安全检测或地址安全检测，记录读取范围
	if raceenabled {
		callerpc := getcallerpc()
		racereadrangepc(oldPtr, uintptr(oldLen*int(et.Size_)), callerpc, abi.FuncPCABIInternal(growslice))
	}
	if msanenabled {
		msanread(oldPtr, uintptr(oldLen*int(et.Size_)))
	}
	if asanenabled {
		asanread(oldPtr, uintptr(oldLen*int(et.Size_)))
	}

	// 检查新的长度是否合理
	if newLen < 0 {
		panic(errorString("切片扩容: len 超出范围"))
	}

	// 如果元素大小为 0，特殊处理
	if et.Size_ == 0 {
		// append 不应创建具有nil指针但非零len的切片。
		// 我们假设append在这种情况下不需要保留oldPtr。
		return slice{unsafe.Pointer(&zerobase), newLen, newLen}
	}

	// 计算新的容量
	newcap := oldCap
	doublecap := newcap + newcap
	if newLen > doublecap {
		newcap = newLen
	} else {
		// 容量如果小于256, 直接翻倍扩容
		const threshold = 256
		if oldCap < threshold {
			newcap = doublecap
		} else {
			// 对于大容量的 slice，采用 1.25 倍的增长策略
			for 0 < newcap && newcap < newLen {
				// 从小切片的2倍增长到大切片的1.25倍。
				// 这个公式给出了两者之间的平滑过渡。
				newcap += (newcap + 3*threshold) / 4
			}
			// 将newcap设置为请求的cap，当newcap计算溢出。
			if newcap <= 0 {
				newcap = newLen
			}
		}
	}

	// 检查容量计算是否溢出
	var overflow bool
	// 各个属性的内存大小
	var lenmem, newlenmem, capmem uintptr

	// 计算旧长度、新长度和容量各自所需的内存大小，并检查是否溢出
	// 最终确定哈希表的内存大小以及新的容量，并在需要时进行向上取整的优化计算。
	switch {
	case et.Size_ == 1:
		lenmem = uintptr(oldLen)    // 计算旧长度的内存大小（每个元素大小为1）
		newlenmem = uintptr(newLen) // 计算新长度的内存大小（每个元素大小为1）
		// 对容量进行向上取整的优化计算
		capmem = roundupsize(uintptr(newcap)) // 计算新容量的内存大小并向上取整
		overflow = uintptr(newcap) > maxAlloc // 检查新容量是否超出最大分配内存限制
		newcap = int(capmem)                  // 更新新容量为向上取整后的值

	// 如果系统是 64 位，那么 PtrSize 的值将是 8；如果是 32 位，那么 PtrSize 的值将是 4
	case et.Size_ == goarch.PtrSize:
		lenmem = uintptr(oldLen) * goarch.PtrSize    // 计算旧长度的内存大小（每个元素大小为指针大小）
		newlenmem = uintptr(newLen) * goarch.PtrSize // 计算新长度的内存大小（每个元素大小为指针大小）
		// 对容量进行向上取整的优化计算
		capmem = roundupsize(uintptr(newcap) * goarch.PtrSize) // 计算新容量的内存大小并向上取整
		overflow = uintptr(newcap) > maxAlloc/goarch.PtrSize   // 检查新容量是否超出最大分配内存限制
		newcap = int(capmem / goarch.PtrSize)                  // 更新新容量为向上取整后的值

	// 如果元素大小是2的幂次方
	case isPowerOfTwo(et.Size_):
		// 定义位移量变量
		var shift uintptr
		// 如果指针大小为8字节
		if goarch.PtrSize == 8 {
			// 用于更好的代码生成，对位移进行掩码操作
			// 计算位移量并进行掩码操作保留低6位
			shift = uintptr(sys.TrailingZeros64(uint64(et.Size_))) & 63
		} else {
			// 如果指针大小为4字节，计算位移量并进行掩码操作保留低5位
			shift = uintptr(sys.TrailingZeros32(uint32(et.Size_))) & 31
		}

		lenmem = uintptr(oldLen) << shift    // 计算旧长度的内存大小并左移位移量
		newlenmem = uintptr(newLen) << shift // 计算新长度的内存大小并左移位移量
		// 对容量进行向上取整的优化计算
		capmem = roundupsize(uintptr(newcap) << shift)   // 计算新容量的内存大小并左移位移量，然后向上取整
		overflow = uintptr(newcap) > (maxAlloc >> shift) // 检查新容量是否超出最大分配内存限制并考虑位移
		newcap = int(capmem >> shift)                    // 更新新容量为向上取整后的值右移位移量
		capmem = uintptr(newcap) << shift                // 更新容量为新容量左移位移量

	default:
		lenmem = uintptr(oldLen) * et.Size_    // 计算旧长度的内存大小（一般情况下）
		newlenmem = uintptr(newLen) * et.Size_ // 计算新长度的内存大小（一般情况下）
		// 计算容量并检查是否溢出
		capmem, overflow = math.MulUintptr(et.Size_, uintptr(newcap)) // 用新容量乘以元素大小计算总内存，同时检查是否溢出
		// 对容量进行向上取整的优化计算
		capmem = roundupsize(capmem)        // 计算新容量的内存大小并向上取整
		newcap = int(capmem / et.Size_)     // 更新新容量为向上取整后的值除以元素大小
		capmem = uintptr(newcap) * et.Size_ // 更新容量为新容量乘以元素大小
	}

	// 检查是否溢出或超出最大分配大小
	if overflow || capmem > maxAlloc {
		panic(errorString("growslice: len out of range"))
	}

	// 分配新的内存
	var p unsafe.Pointer
	if et.PtrBytes == 0 {
		p = mallocgc(capmem, nil, false)
		// 只有在新分配的内存中未被覆盖的部分进行清零
		memclrNoHeapPointers(add(p, newlenmem), capmem-newlenmem)
	} else {
		// 注意: 不能使用rawmem (避免内存清零)，因为GC可以扫描未初始化的内存。
		p = mallocgc(capmem, et, true)
		if lenmem > 0 && writeBarrier.enabled {
			// 只需遮蔽旧数组中的指针，因为新数组已被清零
			bulkBarrierPreWriteSrcOnly(uintptr(p), uintptr(oldPtr), lenmem-et.Size_+et.PtrBytes)
		}
	}

	// 复制原有数据到新数组
	memmove(p, oldPtr, lenmem)

	// 返回新的 slice 结构
	return slice{p, newLen, newcap}
}

//go:linkname reflect_growslice reflect.growslice
func reflect_growslice(et *_type, old slice, num int) slice {
	// Semantically equivalent to slices.Grow, except that the caller
	// is responsible for ensuring that old.len+num > old.cap.
	num -= old.cap - old.len // preserve memory of old[old.len:old.cap]
	new := growslice(old.array, old.cap+num, old.cap, num, et)
	// growslice does not zero out new[old.cap:new.len] since it assumes that
	// the memory will be overwritten by an append() that called growslice.
	// Since the caller of reflect_growslice is not append(),
	// zero out this region before returning the slice to the reflect package.
	if et.PtrBytes == 0 {
		oldcapmem := uintptr(old.cap) * et.Size_
		newlenmem := uintptr(new.len) * et.Size_
		memclrNoHeapPointers(add(new.array, oldcapmem), newlenmem-oldcapmem)
	}
	new.len = old.len // preserve the old length
	return new
}

func isPowerOfTwo(x uintptr) bool {
	return x&(x-1) == 0
}

// 用于从字符串或不含指针的元素切片复制到另一个切片中。
func slicecopy(toPtr unsafe.Pointer, toLen int, fromPtr unsafe.Pointer, fromLen int, width uintptr) int {
	// 如果源切片长度为0或目标切片长度为0，则直接返回0，无需复制
	if fromLen == 0 || toLen == 0 {
		return 0
	}
	// 需要复制的元素数量初始化为源切片长度
	n := fromLen
	if toLen < n {
		// 如果目标切片长度 小于 源切片长度，则取目标切片长度作为需要复制的元素数量
		n = toLen
	}

	// 如果每个元素的宽度为0，则直接返回需要复制的元素数量n
	if width == 0 {
		return n
	}

	// 计算总共需要复制的字节大小
	size := uintptr(n) * width

	if raceenabled {
		callerpc := getcallerpc()                    // 获取调用者的PC值
		pc := abi.FuncPCABIInternal(slicecopy)       // 获取函数slicecopy的PC值
		racereadrangepc(fromPtr, size, callerpc, pc) // 对源地址范围进行读取访问的race检测
		racewriterangepc(toPtr, size, callerpc, pc)  // 对目标地址范围进行写入访问的race检测
	}
	if msanenabled {
		msanread(fromPtr, size) // 对源地址范围进行内存清洁读取访问的msan检测
		msanwrite(toPtr, size)  // 对目标地址范围进行内存清洁写入访问的msan检测
	}
	if asanenabled {
		asanread(fromPtr, size) // 对源地址范围进行地址清洁读取访问的asan检测
		asanwrite(toPtr, size)  // 对目标地址范围进行地址清洁写入访问的asan检测
	}

	// 如果复制的元素大小为1字节
	if size == 1 {
		// TODO: is this still worth it with new memmove impl?
		*(*byte)(toPtr) = *(*byte)(fromPtr) // 直接按字节进行复制，已知这里是字节指针
	} else {
		// 复制原有数据到新数组
		memmove(toPtr, fromPtr, size)
	}
	return n
}

//go:linkname bytealg_MakeNoZero internal/bytealg.MakeNoZero
func bytealg_MakeNoZero(len int) []byte {
	if uintptr(len) > maxAlloc {
		panicmakeslicelen()
	}
	return unsafe.Slice((*byte)(mallocgc(uintptr(len), nil, false)), len)
}
