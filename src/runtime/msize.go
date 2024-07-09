// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Malloc small size classes.
//
// See malloc.go for overview.
// See also mksizeclasses.go for how we decide what size classes to use.

package runtime

// 如果您要求大小，则返回 mallocgc 将分配的内存块的大小。
func roundupsize(size uintptr) uintptr {
	if size < _MaxSmallSize {
		if size <= smallSizeMax-8 {
			// 如果大小小于可分配的小块最大值，根据分配类别返回对齐后的大小
			return uintptr(class_to_size[size_to_class8[divRoundUp(size, smallSizeDiv)]])
		} else {
			// 如果大小大于可分配的小块最大值，根据分配类别返回对齐后的大小
			return uintptr(class_to_size[size_to_class128[divRoundUp(size-smallSizeMax, largeSizeDiv)]])
		}
	}
	if size+_PageSize < size {
		// 通过分页大小进行对齐
		return size
	}
	return alignUp(size, _PageSize)
}
