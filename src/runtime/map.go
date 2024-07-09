// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

// This file contains the implementation of Go's map type.
//
// A map is just a hash table. The data is arranged
// into an array of buckets. Each bucket contains up to
// 8 key/elem pairs. The low-order bits of the hash are
// used to select a bucket. Each bucket contains a few
// high-order bits of each hash to distinguish the entries
// within a single bucket.
//
// If more than 8 keys hash to a bucket, we chain on
// extra buckets.
//
// When the hashtable grows, we allocate a new array
// of buckets twice as big. Buckets are incrementally
// copied from the old bucket array to the new bucket array.
//
// Map iterators walk through the array of buckets and
// return the keys in walk order (bucket #, then overflow
// chain order, then bucket index).  To maintain iteration
// semantics, we never move keys within their bucket (if
// we did, keys might be returned 0 or 2 times).  When
// growing the table, iterators remain iterating through the
// old table and must check the new table if the bucket
// they are iterating through has been moved ("evacuated")
// to the new table.

// Picking loadFactor: too large and we have lots of overflow
// buckets, too small and we waste a lot of space. I wrote
// a simple program to check some stats for different loads:
// (64-bit, 8 byte keys and elems)
//  loadFactor    %overflow  bytes/entry     hitprobe    missprobe
//        4.00         2.13        20.77         3.00         4.00
//        4.50         4.05        17.30         3.25         4.50
//        5.00         6.85        14.77         3.50         5.00
//        5.50        10.55        12.94         3.75         5.50
//        6.00        15.27        11.67         4.00         6.00
//        6.50        20.90        10.79         4.25         6.50
//        7.00        27.14        10.15         4.50         7.00
//        7.50        34.03         9.73         4.75         7.50
//        8.00        41.10         9.40         5.00         8.00
//
// %overflow   = percentage of buckets which have an overflow bucket
// bytes/entry = overhead bytes used per key/elem pair
// hitprobe    = # of entries to check when looking up a present key
// missprobe   = # of entries to check when looking up an absent key
//
// Keep in mind this data is for maximally loaded tables, i.e. just
// before the table grows. Typical tables will be somewhat less loaded.

import (
	"internal/abi"
	"internal/goarch"
	"runtime/internal/atomic"
	"runtime/internal/math"
	"unsafe"
)

const (
	// Maximum number of key/elem pairs a bucket can hold.
	bucketCntBits = abi.MapBucketCountBits
	bucketCnt     = abi.MapBucketCount

	// Maximum average load of a bucket that triggers growth is bucketCnt*13/16 (about 80% full)
	// Because of minimum alignment rules, bucketCnt is known to be at least 8.
	// Represent as loadFactorNum/loadFactorDen, to allow integer math.
	loadFactorDen = 2
	loadFactorNum = (bucketCnt * 13 / 16) * loadFactorDen

	// Maximum key or elem size to keep inline (instead of mallocing per element).
	// Must fit in a uint8.
	// Fast versions cannot handle big elems - the cutoff size for
	// fast versions in cmd/compile/internal/gc/walk.go must be at most this elem.
	maxKeySize  = abi.MapMaxKeyBytes
	maxElemSize = abi.MapMaxElemBytes

	// data offset should be the size of the bmap struct, but needs to be
	// aligned correctly. For amd64p32 this means 64-bit alignment
	// even though pointers are 32 bit.
	dataOffset = unsafe.Offsetof(struct {
		b bmap
		v int64
	}{}.v)

	// Possible tophash values. We reserve a few possibilities for special marks.
	// Each bucket (including its overflow buckets, if any) will have either all or none of its
	// entries in the evacuated* states (except during the evacuate() method, which only happens
	// during map writes and thus no one else can observe the map during that time).
	emptyRest      = 0 // 此单元格为空，并且在较高的索引或溢出处不再有非空单元格。
	emptyOne       = 1 // this cell is empty
	evacuatedX     = 2 // key/elem is valid.  Entry has been evacuated to first half of larger table.
	evacuatedY     = 3 // same as above, but evacuated to second half of larger table.
	evacuatedEmpty = 4 // cell is empty, bucket is evacuated.
	minTopHash     = 5 // 正常填充单元格的最小 tophash 。

	// flags
	iterator     = 1 // there may be an iterator using buckets
	oldIterator  = 2 // there may be an iterator using oldbuckets
	hashWriting  = 4 // a goroutine is writing to the map
	sameSizeGrow = 8 // the current map growth is to a new map of the same size

	// sentinel bucket ID for iterator checks
	noCheck = 1<<(8*goarch.PtrSize) - 1
)

// isEmpty reports whether the given tophash array entry represents an empty bucket entry.
func isEmpty(x uint8) bool {
	return x <= emptyOne
}

// 一个Go语言map的头部结构定义。
type hmap struct {
	// 存活的单元格数 == map的大小。必须放在首位（被len()内建函数使用）
	count int

	// 标志位
	flags uint8

	// log_2(# 桶的数量)
	B uint8

	// 大概的溢出桶数量；详细见incrnoverflow
	noverflow uint16

	// 哈希种子,计算 key 的哈希的时候会传入哈希函数
	hash0 uint32

	// 存放2^B个桶的数组。如果count==0，可能为nil。
	buckets unsafe.Pointer

	// 先前的桶数组
	// 等量扩容的时候，buckets 长度和 oldbuckets 相等
	// 双倍扩容的时候，buckets 长度会是 oldbuckets 的两倍
	oldbuckets unsafe.Pointer

	// 指示扩容进度，小于此地址的 buckets 迁移完成
	nevacuate uintptr

	// 可选字段
	extra *mapextra
}

// 包含不是所有映射都具有的字段。
type mapextra struct {
	// 如果key和elem都不包含指针且为内联方式，则将bucket类型标记为不包含指针。这样可以避免扫描这样的映射。
	// 但是，bmap.overflow是一个指针。为了保持溢出桶的存活状态，我们在hmap.extra.overflow和hmap.extra.oldoverflow中存储所有溢出桶的指针。
	// 只有当key和elem不包含指针时才使用overflow和oldoverflow。
	// overflow包含hmap.buckets的溢出桶。
	// oldoverflow包含hmap.oldbuckets的溢出桶。
	// 通过间接方式，在hiter中存储指向切片的指针。
	overflow    *[]*bmap // 溢出桶的指针切片
	oldoverflow *[]*bmap // 旧溢出桶的指针切片

	// nextOverflow保存指向空闲溢出桶的指针。
	nextOverflow *bmap // 指向下一个空闲溢出桶的指针
}

// Go map中的一个桶的定义。
type bmap struct {
	// tophash 通常包含哈希值的高字节，用于每个键在此桶中。
	// 如果 tophash[0] < minTopHash，tophash[0] 实际上是一个桶撤离状态(buckets 迁移完成)。
	tophash [bucketCnt]uint8
	// 后面跟着 bucketCnt 个键，然后是 bucketCnt 个元素。
	// 注意：将所有键连续存放，然后是所有元素，虽然代码会比交替键/元素/键/元素... 更加复杂，
	// 但这样可以消除填充，否则可能需要填充，例如对于map[int64]int8。
	// 接着是溢出指针。
}

// A hash iteration structure.
// If you modify hiter, also change cmd/compile/internal/reflectdata/reflect.go
// and reflect/value.go to match the layout of this structure.
type hiter struct {
	key         unsafe.Pointer // Must be in first position.  Write nil to indicate iteration end (see cmd/compile/internal/walk/range.go).
	elem        unsafe.Pointer // Must be in second position (see cmd/compile/internal/walk/range.go).
	t           *maptype
	h           *hmap
	buckets     unsafe.Pointer // bucket ptr at hash_iter initialization time
	bptr        *bmap          // current bucket
	overflow    *[]*bmap       // keeps overflow buckets of hmap.buckets alive
	oldoverflow *[]*bmap       // keeps overflow buckets of hmap.oldbuckets alive
	startBucket uintptr        // bucket iteration started at
	offset      uint8          // intra-bucket offset to start from during iteration (should be big enough to hold bucketCnt-1)
	wrapped     bool           // already wrapped around from end of bucket array to beginning
	B           uint8
	i           uint8
	bucket      uintptr
	checkBucket uintptr
}

// 返回 1 << b，针对代码生成进行了优化。
func bucketShift(b uint8) uintptr {
	// 掩蔽移位量允许省略溢出检查。
	return uintptr(1) << (b & (goarch.PtrSize*8 - 1))
}

// 返回 1 << b-1，针对代码生成进行了优化。
func bucketMask(b uint8) uintptr {
	return bucketShift(b) - 1
}

// 计算hash的tophash值。
func tophash(hash uintptr) uint8 {
	top := uint8(hash >> (goarch.PtrSize*8 - 8))
	if top < minTopHash {
		top += minTopHash
	}
	return top
}

func evacuated(b *bmap) bool {
	h := b.tophash[0]
	return h > emptyOne && h < minTopHash
}

func (b *bmap) overflow(t *maptype) *bmap {
	return *(**bmap)(add(unsafe.Pointer(b), uintptr(t.BucketSize)-goarch.PtrSize))
}

func (b *bmap) setoverflow(t *maptype, ovf *bmap) {
	*(**bmap)(add(unsafe.Pointer(b), uintptr(t.BucketSize)-goarch.PtrSize)) = ovf
}

func (b *bmap) keys() unsafe.Pointer {
	return add(unsafe.Pointer(b), dataOffset)
}

// incrnoverflow increments h.noverflow.
// noverflow counts the number of overflow buckets.
// This is used to trigger same-size map growth.
// See also tooManyOverflowBuckets.
// To keep hmap small, noverflow is a uint16.
// When there are few buckets, noverflow is an exact count.
// When there are many buckets, noverflow is an approximate count.
func (h *hmap) incrnoverflow() {
	// We trigger same-size map growth if there are
	// as many overflow buckets as buckets.
	// We need to be able to count to 1<<h.B.
	if h.B < 16 {
		h.noverflow++
		return
	}
	// Increment with probability 1/(1<<(h.B-15)).
	// When we reach 1<<15 - 1, we will have approximately
	// as many overflow buckets as buckets.
	mask := uint32(1)<<(h.B-15) - 1
	// Example: if h.B == 18, then mask == 7,
	// and fastrand & 7 == 0 with probability 1/8.
	if fastrand()&mask == 0 {
		h.noverflow++
	}
}

func (h *hmap) newoverflow(t *maptype, b *bmap) *bmap {
	var ovf *bmap
	if h.extra != nil && h.extra.nextOverflow != nil {
		// We have preallocated overflow buckets available.
		// See makeBucketArray for more details.
		ovf = h.extra.nextOverflow
		if ovf.overflow(t) == nil {
			// We're not at the end of the preallocated overflow buckets. Bump the pointer.
			h.extra.nextOverflow = (*bmap)(add(unsafe.Pointer(ovf), uintptr(t.BucketSize)))
		} else {
			// This is the last preallocated overflow bucket.
			// Reset the overflow pointer on this bucket,
			// which was set to a non-nil sentinel value.
			ovf.setoverflow(t, nil)
			h.extra.nextOverflow = nil
		}
	} else {
		ovf = (*bmap)(newobject(t.Bucket))
	}
	h.incrnoverflow()
	if t.Bucket.PtrBytes == 0 {
		h.createOverflow()
		*h.extra.overflow = append(*h.extra.overflow, ovf)
	}
	b.setoverflow(t, ovf)
	return ovf
}

func (h *hmap) createOverflow() {
	if h.extra == nil {
		h.extra = new(mapextra)
	}
	if h.extra.overflow == nil {
		h.extra.overflow = new([]*bmap)
	}
}

func makemap64(t *maptype, hint int64, h *hmap) *hmap {
	if int64(int(hint)) != hint {
		hint = 0
	}
	return makemap(t, int(hint), h)
}

// makemap_small implements Go map creation for make(map[k]v) and
// make(map[k]v, hint) when hint is known to be at most bucketCnt
// at compile time and the map needs to be allocated on the heap.
func makemap_small() *hmap {
	h := new(hmap)
	h.hash0 = fastrand()
	return h
}

// 函数实现了Go map的创建 make(map[k]v, hint)。
// 如果编译器确定 map 或第一个桶可以在堆栈上创建，h和/或bucket可能非nil。
// 如果 h != nil，则 map 可以直接在 h 中创建。
// 如果 h.buckets != nil ，则指向的桶可以用作第一个桶。
func makemap(t *maptype, hint int, h *hmap) *hmap {
	mem, overflow := math.MulUintptr(uintptr(hint), t.Bucket.Size_)
	if overflow || mem > maxAlloc {
		hint = 0
	}

	// 初始化 hmap
	if h == nil {
		h = new(hmap)
	}
	h.hash0 = fastrand()

	// 找到大小参数B以容纳请求的元素数量。
	// 比较 hint 是否超过 6.5 的负载因子, 如果超过 B++
	B := uint8(0)
	for overLoadFactor(hint, B) {
		B++
	}
	h.B = B

	// 分配初始哈希表
	// 如果B == 0，则buckets字段稍后会延迟分配（在mapassign中）
	// 如果hint很大，将内存清零可能需要一段时间。
	if h.B != 0 {
		var nextOverflow *bmap
		h.buckets, nextOverflow = makeBucketArray(t, h.B, nil)
		if nextOverflow != nil {
			h.extra = new(mapextra)
			h.extra.nextOverflow = nextOverflow
		}
	}

	return h
}

// 函数初始化用于存储 map桶 的后备数组。
// 1 << b 左移是要分配的最小桶数。
// dirtyalloc 应该是nil或之前由相同t和b参数使用makeBucketArray分配的桶数组。
// 如果dirtyalloc是nil，则将分配一个新的后备数组，
// 否则dirtyalloc将被清空并重新用作后备数组。
func makeBucketArray(t *maptype, b uint8, dirtyalloc unsafe.Pointer) (buckets unsafe.Pointer, nextOverflow *bmap) {
	// 返回 1 << b，针对代码生成进行了优化。
	base := bucketShift(b)
	nbuckets := base

	// 对于小于4的b，溢出桶不太可能产生。
	// 避免计算的开销。
	if b >= 4 {
		// 添加上预期的溢出桶数量
		// 为了插入与b值一起使用的中位数元素所需的数量。
		// 返回 1 << (b - 4)，针对代码生成进行了优化。
		nbuckets += bucketShift(b - 4)
		sz := t.Bucket.Size_ * nbuckets
		// 如果您要求大小，则返回 mallocgc 将分配的内存块的大小。
		up := roundupsize(sz)
		if up != sz {
			nbuckets = up / t.Bucket.Size_
		}
	}

	if dirtyalloc == nil {
		buckets = newarray(t.Bucket, int(nbuckets))
	} else {
		// dirtyalloc 之前由上面的 newarray(t.Bucket, int(nbuckets)) 生成。 但可能不是空的。
		buckets = dirtyalloc
		size := t.Bucket.Size_ * nbuckets
		if t.Bucket.PtrBytes != 0 {
			memclrHasPointers(buckets, size)
		} else {
			memclrNoHeapPointers(buckets, size)
		}
	}

	if base != nbuckets {
		//我们预先分配了一些溢出桶。
		//为了将对这些溢出桶的跟踪开销降到最低，
		//我们使用了一个约定，即如果一个预分配的溢出桶的溢出指针为nil，那么通过增加指针，可以获得更多可用的溢出桶。
		//我们需要一个安全的非nil指针作为最后一个溢出桶；只需使用buckets。
		nextOverflow = (*bmap)(add(buckets, base*uintptr(t.BucketSize)))
		last := (*bmap)(add(buckets, (nbuckets-1)*uintptr(t.BucketSize)))
		last.setoverflow(t, (*bmap)(buckets))
	}
	return buckets, nextOverflow
}

// 返回指向h[key]的指针。永远不返回nil，而是如果key不在map中，则返回elem类型的零对象的引用。
// 注意：返回的指针可能会保持整个map活跃，所以不要长时间保留它。
func mapaccess1(t *maptype, h *hmap, key unsafe.Pointer) unsafe.Pointer {
	// 如果启用了race检查，并且h不为nil，则进行race检查
	if raceenabled && h != nil {
		callerpc := getcallerpc()
		pc := abi.FuncPCABIInternal(mapaccess1)
		racereadpc(unsafe.Pointer(h), callerpc, pc)
		raceReadObjectPC(t.Key, key, callerpc, pc)
	}
	// 如果启用了msan检查，并且h不为nil，则进行msan检查
	if msanenabled && h != nil {
		msanread(key, t.Key.Size_)
	}
	// 如果启用了asan检查，并且h不为nil，则进行asan检查
	if asanenabled && h != nil {
		asanread(key, t.Key.Size_)
	}
	// 如果h为nil或者计数为0，则返回elem类型的零对象的引用
	if h == nil || h.count == 0 {
		if t.HashMightPanic() {
			t.Hasher(key, 0) // see issue 23734
		}
		return unsafe.Pointer(&zeroVal[0])
	}
	// 如果标记为hashWriting，则表示发生了并发map读写，程序报错
	if h.flags&hashWriting != 0 {
		fatal("并发map读取和map写入")
	}

	// 计算hash值
	hash := t.Hasher(key, uintptr(h.hash0))
	// 返回 1 << b-1，针对代码生成进行了优化。
	m := bucketMask(h.B)
	// 将哈希表的buckets指针与哈希值hash进行运算，并根据掩码值m得到桶索引位置，最终计算出要查找的桶的指针位置
	b := (*bmap)(add(h.buckets, (hash&m)*uintptr(t.BucketSize)))
	// 处理旧buckets
	if c := h.oldbuckets; c != nil {
		if !h.sameSizeGrow() {
			// 之前有一半的桶；再掩码一次向下减少2的幂次
			m >>= 1
		}
		oldb := (*bmap)(add(c, (hash&m)*uintptr(t.BucketSize)))
		if !evacuated(oldb) {
			b = oldb
		}
	}
	// 计算高8位hash
	top := tophash(hash)

bucketloop: // 遍历bucket链表
	for ; b != nil; b = b.overflow(t) {
		// 遍历桶中条目
		for i := uintptr(0); i < bucketCnt; i++ {
			// 如果当前桶中的 tophash[i] 不等于 top
			if b.tophash[i] != top {
				// 如果当前桶中的 tophash[i] 等于 emptyRest(空)，则跳出 bucketloop
				if b.tophash[i] == emptyRest {
					break bucketloop
				}
				// 继续下一轮循环
				continue
			}
			// 计算当前 key 对应的指针地址
			k := add(unsafe.Pointer(b), dataOffset+i*uintptr(t.KeySize))
			// 如果需要间接引用key，则进行间接引用
			if t.IndirectKey() {
				k = *((*unsafe.Pointer)(k))
			}
			// 如果找到匹配的key，则返回对应的value指针
			if t.Key.Equal(key, k) {
				// 计算当前 value 对应的指针地址
				e := add(unsafe.Pointer(b), dataOffset+bucketCnt*uintptr(t.KeySize)+i*uintptr(t.ValueSize))
				// 如果需要间接引用elem，则进行间接引用
				if t.IndirectElem() {
					e = *((*unsafe.Pointer)(e))
				}
				// 返回找到的 value 指针
				return e
			}
		}
	}

	// 未找到匹配的key，返回elem类型的零对象的引用
	return unsafe.Pointer(&zeroVal[0])
}

func mapaccess2(t *maptype, h *hmap, key unsafe.Pointer) (unsafe.Pointer, bool) {
	if raceenabled && h != nil {
		callerpc := getcallerpc()
		pc := abi.FuncPCABIInternal(mapaccess2)
		racereadpc(unsafe.Pointer(h), callerpc, pc)
		raceReadObjectPC(t.Key, key, callerpc, pc)
	}
	if msanenabled && h != nil {
		msanread(key, t.Key.Size_)
	}
	if asanenabled && h != nil {
		asanread(key, t.Key.Size_)
	}
	if h == nil || h.count == 0 {
		if t.HashMightPanic() {
			t.Hasher(key, 0) // see issue 23734
		}
		return unsafe.Pointer(&zeroVal[0]), false
	}
	if h.flags&hashWriting != 0 {
		fatal("concurrent map read and map write")
	}
	hash := t.Hasher(key, uintptr(h.hash0))
	m := bucketMask(h.B)
	b := (*bmap)(add(h.buckets, (hash&m)*uintptr(t.BucketSize)))
	if c := h.oldbuckets; c != nil {
		if !h.sameSizeGrow() {
			// There used to be half as many buckets; mask down one more power of two.
			m >>= 1
		}
		oldb := (*bmap)(add(c, (hash&m)*uintptr(t.BucketSize)))
		if !evacuated(oldb) {
			b = oldb
		}
	}
	top := tophash(hash)
bucketloop:
	for ; b != nil; b = b.overflow(t) {
		for i := uintptr(0); i < bucketCnt; i++ {
			if b.tophash[i] != top {
				if b.tophash[i] == emptyRest {
					break bucketloop
				}
				continue
			}
			k := add(unsafe.Pointer(b), dataOffset+i*uintptr(t.KeySize))
			if t.IndirectKey() {
				k = *((*unsafe.Pointer)(k))
			}
			if t.Key.Equal(key, k) {
				e := add(unsafe.Pointer(b), dataOffset+bucketCnt*uintptr(t.KeySize)+i*uintptr(t.ValueSize))
				if t.IndirectElem() {
					e = *((*unsafe.Pointer)(e))
				}
				return e, true
			}
		}
	}
	return unsafe.Pointer(&zeroVal[0]), false
}

// returns both key and elem. Used by map iterator.
func mapaccessK(t *maptype, h *hmap, key unsafe.Pointer) (unsafe.Pointer, unsafe.Pointer) {
	if h == nil || h.count == 0 {
		return nil, nil
	}
	hash := t.Hasher(key, uintptr(h.hash0))
	m := bucketMask(h.B)
	b := (*bmap)(add(h.buckets, (hash&m)*uintptr(t.BucketSize)))
	if c := h.oldbuckets; c != nil {
		if !h.sameSizeGrow() {
			// There used to be half as many buckets; mask down one more power of two.
			m >>= 1
		}
		oldb := (*bmap)(add(c, (hash&m)*uintptr(t.BucketSize)))
		if !evacuated(oldb) {
			b = oldb
		}
	}
	top := tophash(hash)
bucketloop:
	for ; b != nil; b = b.overflow(t) {
		for i := uintptr(0); i < bucketCnt; i++ {
			if b.tophash[i] != top {
				if b.tophash[i] == emptyRest {
					break bucketloop
				}
				continue
			}
			k := add(unsafe.Pointer(b), dataOffset+i*uintptr(t.KeySize))
			if t.IndirectKey() {
				k = *((*unsafe.Pointer)(k))
			}
			if t.Key.Equal(key, k) {
				e := add(unsafe.Pointer(b), dataOffset+bucketCnt*uintptr(t.KeySize)+i*uintptr(t.ValueSize))
				if t.IndirectElem() {
					e = *((*unsafe.Pointer)(e))
				}
				return k, e
			}
		}
	}
	return nil, nil
}

func mapaccess1_fat(t *maptype, h *hmap, key, zero unsafe.Pointer) unsafe.Pointer {
	e := mapaccess1(t, h, key)
	if e == unsafe.Pointer(&zeroVal[0]) {
		return zero
	}
	return e
}

func mapaccess2_fat(t *maptype, h *hmap, key, zero unsafe.Pointer) (unsafe.Pointer, bool) {
	e := mapaccess1(t, h, key)
	if e == unsafe.Pointer(&zeroVal[0]) {
		return zero, false
	}
	return e, true
}

// 函数的作用是在 map 中分配或更新一个键值对。
// 如果键已经存在于 map 中，函数会更新其对应的值；
// 如果键不存在，则函数会为该键分配一个新的位置并插入键值对。
func mapassign(t *maptype, h *hmap, key unsafe.Pointer) unsafe.Pointer {
	if h == nil {
		// 如果 map 为 nil，抛出异常
		panic(plainError("assignment to entry in nil map"))
	}
	// 竞态检测和内存安全检查，这部分主要是为了并发和调试目的
	if raceenabled {
		callerpc := getcallerpc()
		pc := abi.FuncPCABIInternal(mapassign)
		racewritepc(unsafe.Pointer(h), callerpc, pc)
		raceReadObjectPC(t.Key, key, callerpc, pc)
	}
	if msanenabled {
		msanread(key, t.Key.Size_)
	}
	if asanenabled {
		asanread(key, t.Key.Size_)
	}
	// 检查 map 是否处于写入状态，防止并发写入
	if h.flags&hashWriting != 0 {
		fatal("并发 map 写入")
	}

	// 计算 key 的哈希值
	hash := t.Hasher(key, uintptr(h.hash0))
	// 设置写标志
	h.flags ^= hashWriting

	// 如果 map 的 buckets 未初始化，则初始化 buckets
	if h.buckets == nil {
		h.buckets = newobject(t.Bucket) // newarray(t.Bucket, 1)
	}

again:
	// 计算 key 应该存放的 bucket 的索引
	bucket := hash & bucketMask(h.B)
	// 如果 map 正在增长，执行增长操作
	if h.growing() {
		growWork(t, h, bucket)
	}
	// 获取当前 bucket 的地址
	b := (*bmap)(add(h.buckets, bucket*uintptr(t.BucketSize)))
	// 获取高8位，用于快速查找和比较
	top := tophash(hash)

	// 高8位
	var inserti *uint8
	// key
	var insertk unsafe.Pointer
	// value
	var elem unsafe.Pointer
bucketloop:
	for {
		// 遍历 bucket 中的所有键值对, 如果相同key则更新, 如果遇到一个空位则写入
		for i := uintptr(0); i < bucketCnt; i++ {
			if b.tophash[i] != top {
				// 如果当前槽的 top hash 不匹配，检查是否为空可以插入
				if isEmpty(b.tophash[i]) && inserti == nil {
					// 存储高8位
					inserti = &b.tophash[i]
					// 存储key
					insertk = add(unsafe.Pointer(b), dataOffset+i*uintptr(t.KeySize))
					// 存储value
					elem = add(unsafe.Pointer(b), dataOffset+bucketCnt*uintptr(t.KeySize)+i*uintptr(t.ValueSize))
				}
				if b.tophash[i] == emptyRest {
					break bucketloop
				}
				continue
			}
			// 获取键的地址
			k := add(unsafe.Pointer(b), dataOffset+i*uintptr(t.KeySize))
			// 如果需要间接引用key，则进行间接引用
			if t.IndirectKey() {
				k = *((*unsafe.Pointer)(k))
			}
			// 比较如果key与当前地址的key不相同, 则退出继续遍历下一个
			if !t.Key.Equal(key, k) {
				continue
			}
			// 如果 key 匹配，更新值
			if t.NeedKeyUpdate() {
				typedmemmove(t.Key, k, key)
			}
			elem = add(unsafe.Pointer(b), dataOffset+bucketCnt*uintptr(t.KeySize)+i*uintptr(t.ValueSize))
			goto done
		}
		// 如果 bucket 已满，遍历下一个 bucket
		ovf := b.overflow(t)
		if ovf == nil {
			break
		}
		b = ovf
	}

	// 未找到键的映射。分配新 bucket 并链接到上一个 bucket 后面。

	// 如果 map 达到了最大负载因子，或者溢出桶的数量过多，
	if !h.growing() && (overLoadFactor(h.count+1, h.B) || tooManyOverflowBuckets(h.noverflow, h.B)) {
		// 如果需要对 map 进行扩容，执行扩容操作
		hashGrow(t, h)
		// 执行扩容之后重新开始查找，因为 map 结构可能已经发生变化导致插入的位置改变
		// 重新查找会执行数据迁移
		goto again
	}

	// 如果没有找到空槽，需要处理 map 的增长或创建新的 overflow bucket
	if inserti == nil {
		// 如果 bucket 和 overflow bucket 都满了，创建新的 overflow bucket
		newb := h.newoverflow(t, b)
		// 存储高8位
		inserti = &newb.tophash[0]
		// 存储key
		insertk = add(unsafe.Pointer(newb), dataOffset)
		// 存储value
		elem = add(insertk, bucketCnt*uintptr(t.KeySize))
	}

	// 插入新的键值对
	if t.IndirectKey() {
		kmem := newobject(t.Key)
		*(*unsafe.Pointer)(insertk) = kmem
		insertk = kmem
	}
	// 返回值的地址，如果 key 是间接的，则返回间接地址
	if t.IndirectElem() {
		vmem := newobject(t.Elem)
		*(*unsafe.Pointer)(elem) = vmem
	}
	typedmemmove(t.Key, insertk, key)
	// 存储高8位
	*inserti = top
	// 计数
	h.count++

done:
	// 清除写标志
	if h.flags&hashWriting == 0 {
		fatal("并发 map 写入")
	}
	h.flags &^= hashWriting
	// 返回值的地址，如果 key 是间接的，则返回间接地址
	if t.IndirectElem() {
		elem = *((*unsafe.Pointer)(elem))
	}
	return elem
}

func mapdelete(t *maptype, h *hmap, key unsafe.Pointer) {
	if raceenabled && h != nil {
		callerpc := getcallerpc()
		pc := abi.FuncPCABIInternal(mapdelete)
		racewritepc(unsafe.Pointer(h), callerpc, pc)
		raceReadObjectPC(t.Key, key, callerpc, pc)
	}
	if msanenabled && h != nil {
		msanread(key, t.Key.Size_)
	}
	if asanenabled && h != nil {
		asanread(key, t.Key.Size_)
	}
	if h == nil || h.count == 0 {
		if t.HashMightPanic() {
			t.Hasher(key, 0) // see issue 23734
		}
		return
	}
	if h.flags&hashWriting != 0 {
		fatal("concurrent map writes")
	}

	hash := t.Hasher(key, uintptr(h.hash0))

	// Set hashWriting after calling t.hasher, since t.hasher may panic,
	// in which case we have not actually done a write (delete).
	h.flags ^= hashWriting

	bucket := hash & bucketMask(h.B)
	if h.growing() {
		growWork(t, h, bucket)
	}
	b := (*bmap)(add(h.buckets, bucket*uintptr(t.BucketSize)))
	bOrig := b
	top := tophash(hash)
search:
	for ; b != nil; b = b.overflow(t) {
		for i := uintptr(0); i < bucketCnt; i++ {
			if b.tophash[i] != top {
				if b.tophash[i] == emptyRest {
					break search
				}
				continue
			}
			k := add(unsafe.Pointer(b), dataOffset+i*uintptr(t.KeySize))
			k2 := k
			if t.IndirectKey() {
				k2 = *((*unsafe.Pointer)(k2))
			}
			if !t.Key.Equal(key, k2) {
				continue
			}
			// Only clear key if there are pointers in it.
			if t.IndirectKey() {
				*(*unsafe.Pointer)(k) = nil
			} else if t.Key.PtrBytes != 0 {
				memclrHasPointers(k, t.Key.Size_)
			}
			e := add(unsafe.Pointer(b), dataOffset+bucketCnt*uintptr(t.KeySize)+i*uintptr(t.ValueSize))
			if t.IndirectElem() {
				*(*unsafe.Pointer)(e) = nil
			} else if t.Elem.PtrBytes != 0 {
				memclrHasPointers(e, t.Elem.Size_)
			} else {
				memclrNoHeapPointers(e, t.Elem.Size_)
			}
			b.tophash[i] = emptyOne
			// If the bucket now ends in a bunch of emptyOne states,
			// change those to emptyRest states.
			// It would be nice to make this a separate function, but
			// for loops are not currently inlineable.
			if i == bucketCnt-1 {
				if b.overflow(t) != nil && b.overflow(t).tophash[0] != emptyRest {
					goto notLast
				}
			} else {
				if b.tophash[i+1] != emptyRest {
					goto notLast
				}
			}
			for {
				b.tophash[i] = emptyRest
				if i == 0 {
					if b == bOrig {
						break // beginning of initial bucket, we're done.
					}
					// Find previous bucket, continue at its last entry.
					c := b
					for b = bOrig; b.overflow(t) != c; b = b.overflow(t) {
					}
					i = bucketCnt - 1
				} else {
					i--
				}
				if b.tophash[i] != emptyOne {
					break
				}
			}
		notLast:
			h.count--
			// Reset the hash seed to make it more difficult for attackers to
			// repeatedly trigger hash collisions. See issue 25237.
			if h.count == 0 {
				h.hash0 = fastrand()
			}
			break search
		}
	}

	if h.flags&hashWriting == 0 {
		fatal("concurrent map writes")
	}
	h.flags &^= hashWriting
}

// 初始化用于遍历映射的 hiter 结构。
// hiter 结构由编译器的 order pass 在栈上分配，
// 或者通过 reflect_mapiterinit 在堆上分配，
// 两者都需要一个已清零的 hiter 结构，因为它包含指针。
func mapiterinit(t *maptype, h *hmap, it *hiter) {
	// 如果启用了竞态检测并且 h 不为 nil，
	// 记录对 h 的读取范围和调用者 PC。
	if raceenabled && h != nil {
		callerpc := getcallerpc()
		racereadpc(unsafe.Pointer(h), callerpc, abi.FuncPCABIInternal(mapiterinit))
	}

	// 设置迭代器的映射类型
	it.t = t
	// 如果 h 为 nil 或者映射没有元素，直接返回。
	if h == nil || h.count == 0 {
		return
	}

	// 检查 hiter 结构的大小是否正确，
	// 这个检查在编译阶段的反射数据生成中很重要。
	if unsafe.Sizeof(hiter{})/goarch.PtrSize != 12 {
		throw("hash_iter size incorrect") // see cmd/compile/internal/reflectdata/reflect.go
	}

	// 设置迭代器指向的 hmap
	it.h = h

	// 获取 bucket 的状态快照
	it.B = h.B             // 记录映射的基数（B值）
	it.buckets = h.buckets // 记录桶数组
	if t.Bucket.PtrBytes == 0 {
		// 如果桶内元素不是指针类型，
		// 创建溢出桶的切片并记住指向当前和旧的溢出桶的指针。
		// 这样可以保证即使在迭代期间映射增长或者添加了新的溢出桶，
		// 所有相关溢出桶仍然存活。
		h.createOverflow()
		it.overflow = h.extra.overflow
		it.oldoverflow = h.extra.oldoverflow
	}

	// 决定开始位置
	// 使用 fastrand 或 fastrand64 生成随机数，
	// 具体取决于映射的基数 B 是否大于 31 - bucketCntBits。
	var r uintptr
	if h.B > 31-bucketCntBits {
		r = uintptr(fastrand64())
	} else {
		r = uintptr(fastrand())
	}
	// 计算开始的 bucket 索引
	it.startBucket = r & bucketMask(h.B)
	// 计算 bucket 内的偏移量
	it.offset = uint8(r >> h.B & (bucketCnt - 1))
	// 设置迭代器的初始 bucket 状态
	it.bucket = it.startBucket

	// 标记此映射已经有迭代器在运行
	// 可以与另一个 mapiterinit() 函数并发执行。
	if old := h.flags; old&(iterator|oldIterator) != iterator|oldIterator {
		atomic.Or8(&h.flags, iterator|oldIterator)
	}

	// 调用 mapiternext 来定位下一个可访问的键值对。
	mapiternext(it)
}

// 更新 hiter 结构，使其指向映射中的下一个元素。
// 这个函数用于遍历映射，找到下一个有效的键值对。
func mapiternext(it *hiter) {
	// 当前映射的 hmap 结构
	h := it.h
	if raceenabled {
		callerpc := getcallerpc()
		racereadpc(unsafe.Pointer(h), callerpc, abi.FuncPCABIInternal(mapiternext))
	}
	if h.flags&hashWriting != 0 {
		fatal("concurrent map iteration and map write")
	}

	t := it.t                     // 映射的类型
	bucket := it.bucket           // 当前桶的索引
	b := it.bptr                  // 当前桶的指针
	i := it.i                     // 当前桶中的元素索引
	checkBucket := it.checkBucket // 需要检查的桶

next:
	if b == nil {
		// 如果当前桶为空
		if bucket == it.startBucket && it.wrapped {
			// 当前桶回到起始桶并且已经遍历了一轮，迭代结束
			it.key = nil
			it.elem = nil
			return
		}
		if h.growing() && it.B == h.B {
			// 如果哈希表正在扩容并且迭代器的 bucket值 等于哈希表的 bucket数
			// 表示迭代器在映射扩容中启动，映射尚未完成扩容
			// 需要处理旧桶和新桶之间元素的迁移

			// 确定要处理的旧桶
			oldbucket := bucket & it.h.oldbucketmask()
			b = (*bmap)(add(h.oldbuckets, oldbucket*uintptr(t.BucketSize)))
			// 是否已经被搬迁过
			if !evacuated(b) {
				// 如果当前桶还没有被迁移，需要遍历旧桶并且仅返回那些将要迁移到当前桶的元素
				checkBucket = bucket
			} else {
				// 如果当前桶已经迁移完成，则直接指向当前桶
				b = (*bmap)(add(it.buckets, bucket*uintptr(t.BucketSize)))
				checkBucket = noCheck
			}
		} else {
			// 非扩容状态下直接处理当前桶
			b = (*bmap)(add(it.buckets, bucket*uintptr(t.BucketSize)))
			checkBucket = noCheck
		}

		// 处理完当前桶后，准备处理下一个桶
		bucket++
		if bucket == bucketShift(it.B) {
			// 如果已经处理完整个哈希表，回到第一个桶，同时标记为已经遍历过一轮
			bucket = 0
			it.wrapped = true
		}
		// 重置桶中元素的索引
		i = 0
	}
	for ; i < bucketCnt; i++ {
		// 计算当前桶中元素的索引
		offi := (i + it.offset) & (bucketCnt - 1)
		// 如果当前位置是空的或者被迁移为空标记，继续遍历下一个位置
		if isEmpty(b.tophash[offi]) || b.tophash[offi] == evacuatedEmpty {
			continue
		}

		// 计算键和值的指针位置
		k := add(unsafe.Pointer(b), dataOffset+uintptr(offi)*uintptr(t.KeySize))
		if t.IndirectKey() {
			k = *((*unsafe.Pointer)(k))
		}
		e := add(unsafe.Pointer(b), dataOffset+bucketCnt*uintptr(t.KeySize)+uintptr(offi)*uintptr(t.ValueSize))

		// 特殊情况：迭代开始于映射扩容至更大尺寸的过程中，且扩容尚未完成。
		if checkBucket != noCheck && !h.sameSizeGrow() {
			// 我们正在处理一个其旧桶尚未清空的桶。需要检查元素是否属于当前新桶。
			if t.ReflexiveKey() || t.Key.Equal(k, k) {
				// 如果 oldbucket 中的项目不是指定给迭代中的当前新存储桶，则跳过它。
				hash := t.Hasher(k, uintptr(h.hash0))
				if hash&bucketMask(it.B) != checkBucket {
					continue
				}
			} else {
				// 处理 key 不等于自身的情况
				if checkBucket>>(it.B-1) != uintptr(b.tophash[offi]&1) {
					continue
				}
			}
		}
		if (b.tophash[offi] != evacuatedX && b.tophash[offi] != evacuatedY) ||
			!(t.ReflexiveKey() || t.Key.Equal(k, k)) {
			// 找到有效的键值对，设置迭代器的键和值
			it.key = k
			if t.IndirectElem() {
				e = *((*unsafe.Pointer)(e))
			}
			it.elem = e
		} else {
			// 映射已经扩容，当前键的有效数据现在位于其他地方，重新查找键值对
			rk, re := mapaccessK(t, h, k)
			if rk == nil {
				continue // 键已被删除
			}
			it.key = rk
			it.elem = re
		}

		// 设置迭代器的桶、当前处理的桶指针、元素索引等信息
		it.bucket = bucket
		if it.bptr != b { // 避免不必要的写入障碍; 请参阅问题14921
			it.bptr = b
		}
		it.i = i + 1
		it.checkBucket = checkBucket
		return
	}
	b = b.overflow(t) // 如果桶中有溢出链表，移动到溢出链表继续查找。
	i = 0
	goto next
}

// mapclear deletes all keys from a map.
func mapclear(t *maptype, h *hmap) {
	if raceenabled && h != nil {
		callerpc := getcallerpc()
		pc := abi.FuncPCABIInternal(mapclear)
		racewritepc(unsafe.Pointer(h), callerpc, pc)
	}

	if h == nil || h.count == 0 {
		return
	}

	if h.flags&hashWriting != 0 {
		fatal("concurrent map writes")
	}

	h.flags ^= hashWriting

	// Mark buckets empty, so existing iterators can be terminated, see issue #59411.
	markBucketsEmpty := func(bucket unsafe.Pointer, mask uintptr) {
		for i := uintptr(0); i <= mask; i++ {
			b := (*bmap)(add(bucket, i*uintptr(t.BucketSize)))
			for ; b != nil; b = b.overflow(t) {
				for i := uintptr(0); i < bucketCnt; i++ {
					b.tophash[i] = emptyRest
				}
			}
		}
	}
	markBucketsEmpty(h.buckets, bucketMask(h.B))
	if oldBuckets := h.oldbuckets; oldBuckets != nil {
		markBucketsEmpty(oldBuckets, h.oldbucketmask())
	}

	h.flags &^= sameSizeGrow
	h.oldbuckets = nil
	h.nevacuate = 0
	h.noverflow = 0
	h.count = 0

	// Reset the hash seed to make it more difficult for attackers to
	// repeatedly trigger hash collisions. See issue 25237.
	h.hash0 = fastrand()

	// Keep the mapextra allocation but clear any extra information.
	if h.extra != nil {
		*h.extra = mapextra{}
	}

	// makeBucketArray clears the memory pointed to by h.buckets
	// and recovers any overflow buckets by generating them
	// as if h.buckets was newly alloced.
	_, nextOverflow := makeBucketArray(t, h.B, h.buckets)
	if nextOverflow != nil {
		// If overflow buckets are created then h.extra
		// will have been allocated during initial bucket creation.
		h.extra.nextOverflow = nextOverflow
	}

	if h.flags&hashWriting == 0 {
		fatal("concurrent map writes")
	}
	h.flags &^= hashWriting
}

// 扩容
func hashGrow(t *maptype, h *hmap) {
	// 如果达到了负载因子，需要扩容
	// 否则，溢出桶太多，保持相同桶数，稍后进行水平"扩容"
	bigger := uint8(1)
	if !overLoadFactor(h.count+1, h.B) {
		bigger = 0
		h.flags |= sameSizeGrow
	}
	oldbuckets := h.buckets
	// 创建新的桶数组
	newbuckets, nextOverflow := makeBucketArray(t, h.B+bigger, nil)

	// 处理标志位，保留与迭代器相关的标志位
	flags := h.flags &^ (iterator | oldIterator)
	if h.flags&iterator != 0 {
		flags |= oldIterator
	}

	// 提交扩容操作（与 GC 原子操作）

	// 更新桶的数量
	h.B += bigger
	h.flags = flags
	// 记录旧桶
	h.oldbuckets = oldbuckets
	// 使用新桶
	h.buckets = newbuckets
	h.nevacuate = 0
	h.noverflow = 0

	if h.extra != nil && h.extra.overflow != nil {
		// 将当前的溢出桶提升为旧的一代
		if h.extra.oldoverflow != nil {
			throw("oldoverflow is not nil")
		}
		h.extra.oldoverflow = h.extra.overflow
		h.extra.overflow = nil
	}
	if nextOverflow != nil {
		// 将新的溢出桶链接到链表中
		if h.extra == nil {
			h.extra = new(mapextra)
		}
		h.extra.nextOverflow = nextOverflow
	}

	// 实际的哈希表数据复制是由 growWork() 和 evacuate() 函数逐步进行的
}

// 报告放置在 1<<B 存储桶中的count items是否超过loadFactor。
func overLoadFactor(count int, B uint8) bool {
	return count > bucketCnt && uintptr(count) > loadFactorNum*(bucketShift(B)/loadFactorDen)
}

// tooManyOverflowBuckets reports whether noverflow buckets is too many for a map with 1<<B buckets.
// Note that most of these overflow buckets must be in sparse use;
// if use was dense, then we'd have already triggered regular map growth.
func tooManyOverflowBuckets(noverflow uint16, B uint8) bool {
	// If the threshold is too low, we do extraneous work.
	// If the threshold is too high, maps that grow and shrink can hold on to lots of unused memory.
	// "too many" means (approximately) as many overflow buckets as regular buckets.
	// See incrnoverflow for more details.
	if B > 15 {
		B = 15
	}
	// The compiler doesn't see here that B < 16; mask B to generate shorter shift code.
	return noverflow >= uint16(1)<<(B&15)
}

// 报告h是否在扩容迁移数据。h.oldbuckets != nil
func (h *hmap) growing() bool {
	return h.oldbuckets != nil
}

// sameSizeGrow reports whether the current growth is to a map of the same size.
func (h *hmap) sameSizeGrow() bool {
	return h.flags&sameSizeGrow != 0
}

// noldbuckets calculates the number of buckets prior to the current map growth.
func (h *hmap) noldbuckets() uintptr {
	oldB := h.B
	if !h.sameSizeGrow() {
		oldB--
	}
	return bucketShift(oldB)
}

// oldbucketmask 提供可用于计算n % noldbuckets() 的掩码。
func (h *hmap) oldbucketmask() uintptr {
	return h.noldbuckets() - 1
}

// 扩容迁移数据
func growWork(t *maptype, h *hmap, bucket uintptr) {
	// 用于在 map 扩容期间迁移旧 bucket 中的数据到新 bucket
	evacuate(t, h, bucket&h.oldbucketmask())

	// 报告h是否在扩容迁移数据。h.oldbuckets != nil
	if h.growing() {
		// 迁移一个额外的旧桶以推动扩容进度
		// nevacuate 指示扩容进度，小于此地址的 buckets 迁移完成
		evacuate(t, h, h.nevacuate)
	}
}

func bucketEvacuated(t *maptype, h *hmap, bucket uintptr) bool {
	b := (*bmap)(add(h.oldbuckets, bucket*uintptr(t.BucketSize)))
	return evacuated(b)
}

// evacDst is an evacuation destination.
type evacDst struct {
	b *bmap          // current destination bucket
	i int            // key/elem index into b
	k unsafe.Pointer // pointer to current key storage
	e unsafe.Pointer // pointer to current elem storage
}

// 函数用于在 map 扩容期间迁移旧 bucket 中的数据到新 bucket。
// 它根据新的 bucket 数量和大小，将数据从旧的 bucket 分发到新的 bucket。
func evacuate(t *maptype, h *hmap, oldbucket uintptr) {
	// 获取旧 bucket 的地址
	b := (*bmap)(add(h.oldbuckets, oldbucket*uintptr(t.BucketSize)))
	// 新的 bucket 数量的位表示
	newbit := h.noldbuckets()
	// 检查 bucket 是否已经被迁移过
	if !evacuated(b) {
		// xy包含x和y (低和高) 疏散目标。
		var xy [2]evacDst
		// x 是目的地之一
		x := &xy[0]
		// x 的 bucket 地址
		x.b = (*bmap)(add(h.buckets, oldbucket*uintptr(t.BucketSize)))
		// x 的 key 开始地址
		x.k = add(unsafe.Pointer(x.b), dataOffset)
		// x 的 value 开始地址
		x.e = add(x.k, bucketCnt*uintptr(t.KeySize))

		// 报告当前增长是否为相同大小的映射
		if !h.sameSizeGrow() {
			// 如果不是同尺寸扩容
			// y 是另一个目的地
			y := &xy[1]
			// y 的 bucket 地址
			y.b = (*bmap)(add(h.buckets, (oldbucket+newbit)*uintptr(t.BucketSize)))
			// y 的 key 开始地址
			y.k = add(unsafe.Pointer(y.b), dataOffset)
			// y 的 value 开始地址
			y.e = add(y.k, bucketCnt*uintptr(t.KeySize))
		}

		// 遍历旧 bucket
		for ; b != nil; b = b.overflow(t) {
			// 当前 bucket 的 key 开始地址
			k := add(unsafe.Pointer(b), dataOffset)
			// 当前 bucket 的 value 开始地址
			e := add(k, bucketCnt*uintptr(t.KeySize))

			// 遍历 bucket 中的每一个槽
			for i := 0; i < bucketCnt; i, k, e = i+1, add(k, uintptr(t.KeySize)), add(e, uintptr(t.ValueSize)) {
				// 当前槽的 tophash 高8位值
				top := b.tophash[i]
				// 如果槽是空的
				if isEmpty(top) {
					// 标记槽为空且已被迁移
					b.tophash[i] = evacuatedEmpty
					continue
				}
				// 检查 tophash 高8位的有效性
				if top < minTopHash {
					throw("bad map state") // 如果无效，抛出错误
				}

				// 赋值k的地址
				k2 := k
				if t.IndirectKey() {
					k2 = *((*unsafe.Pointer)(k2))
				}

				// 用于决定使用 x 还是 y 目的地
				var useY uint8
				// 如果不是同尺寸扩容
				if !h.sameSizeGrow() {
					// 计算 key 的哈希值
					hash := t.Hasher(k2, uintptr(h.hash0))
					// 特殊情况处理
					if h.flags&iterator != 0 && !t.ReflexiveKey() && !t.Key.Equal(k2, k2) {
						useY = top & 1      // 用 tophash 的最低位决定
						top = tophash(hash) // 重计算 tophash
					} else {
						// 根据哈希值决定使用 x 还是 y
						if hash&newbit != 0 {
							useY = 1
						}
					}
				}

				// 确保 evacuatedX 和 evacuatedY 的关系正确
				if evacuatedX+1 != evacuatedY || evacuatedX^1 != evacuatedY {
					throw("bad evacuatedN")
				}

				b.tophash[i] = evacuatedX + useY // evacuatedX + 1 == evacuatedY
				// 选择目的地
				dst := &xy[useY]
				// 如果目的地的槽已满
				if dst.i == bucketCnt {
					// 创建新的 overflow bucket
					dst.b = h.newoverflow(t, dst.b)
					dst.i = 0
					// 更新 key 开始地址
					dst.k = add(unsafe.Pointer(dst.b), dataOffset)
					// 更新 value 开始地址
					dst.e = add(dst.k, bucketCnt*uintptr(t.KeySize))
				}

				// 将 key 和 value 移动到目的地
				dst.b.tophash[dst.i&(bucketCnt-1)] = top // 设置目的地的 tophash
				if t.IndirectKey() {
					*(*unsafe.Pointer)(dst.k) = k2 // 如果 key 是间接的，复制指针
				} else {
					typedmemmove(t.Key, dst.k, k) // 否则，复制 key
				}
				if t.IndirectElem() {
					*(*unsafe.Pointer)(dst.e) = *(*unsafe.Pointer)(e) // 如果 value 是间接的，复制指针
				} else {
					typedmemmove(t.Elem, dst.e, e) // 否则，复制 value
				}

				// 更新目的地的槽计数
				dst.i++
				// 更新 key 和 value 的地址，准备下一次迭代
				dst.k = add(dst.k, uintptr(t.KeySize))
				dst.e = add(dst.e, uintptr(t.ValueSize))
			}
		}
		// 清理旧 bucket 的溢出桶并清除 key 和 value，帮助 GC
		if h.flags&oldIterator == 0 && t.Bucket.PtrBytes != 0 {
			b := add(h.oldbuckets, oldbucket*uintptr(t.BucketSize))
			// 保留b.tophash，因为疏散状态保持在那里。
			ptr := add(b, dataOffset)
			n := uintptr(t.BucketSize) - dataOffset
			memclrHasPointers(ptr, n)
		}
	}

	// 更新 map 的 evacuation 标记
	if oldbucket == h.nevacuate {
		advanceEvacuationMark(h, t, newbit)
	}
}

func advanceEvacuationMark(h *hmap, t *maptype, newbit uintptr) {
	h.nevacuate++
	// Experiments suggest that 1024 is overkill by at least an order of magnitude.
	// Put it in there as a safeguard anyway, to ensure O(1) behavior.
	stop := h.nevacuate + 1024
	if stop > newbit {
		stop = newbit
	}
	for h.nevacuate != stop && bucketEvacuated(t, h, h.nevacuate) {
		h.nevacuate++
	}
	if h.nevacuate == newbit { // newbit == # of oldbuckets
		// Growing is all done. Free old main bucket array.
		h.oldbuckets = nil
		// Can discard old overflow buckets as well.
		// If they are still referenced by an iterator,
		// then the iterator holds a pointers to the slice.
		if h.extra != nil {
			h.extra.oldoverflow = nil
		}
		h.flags &^= sameSizeGrow
	}
}

// Reflect stubs. Called from ../reflect/asm_*.s

//go:linkname reflect_makemap reflect.makemap
func reflect_makemap(t *maptype, cap int) *hmap {
	// Check invariants and reflects math.
	if t.Key.Equal == nil {
		throw("runtime.reflect_makemap: unsupported map key type")
	}
	if t.Key.Size_ > maxKeySize && (!t.IndirectKey() || t.KeySize != uint8(goarch.PtrSize)) ||
		t.Key.Size_ <= maxKeySize && (t.IndirectKey() || t.KeySize != uint8(t.Key.Size_)) {
		throw("key size wrong")
	}
	if t.Elem.Size_ > maxElemSize && (!t.IndirectElem() || t.ValueSize != uint8(goarch.PtrSize)) ||
		t.Elem.Size_ <= maxElemSize && (t.IndirectElem() || t.ValueSize != uint8(t.Elem.Size_)) {
		throw("elem size wrong")
	}
	if t.Key.Align_ > bucketCnt {
		throw("key align too big")
	}
	if t.Elem.Align_ > bucketCnt {
		throw("elem align too big")
	}
	if t.Key.Size_%uintptr(t.Key.Align_) != 0 {
		throw("key size not a multiple of key align")
	}
	if t.Elem.Size_%uintptr(t.Elem.Align_) != 0 {
		throw("elem size not a multiple of elem align")
	}
	if bucketCnt < 8 {
		throw("bucketsize too small for proper alignment")
	}
	if dataOffset%uintptr(t.Key.Align_) != 0 {
		throw("need padding in bucket (key)")
	}
	if dataOffset%uintptr(t.Elem.Align_) != 0 {
		throw("need padding in bucket (elem)")
	}

	return makemap(t, cap, nil)
}

//go:linkname reflect_mapaccess reflect.mapaccess
func reflect_mapaccess(t *maptype, h *hmap, key unsafe.Pointer) unsafe.Pointer {
	elem, ok := mapaccess2(t, h, key)
	if !ok {
		// reflect wants nil for a missing element
		elem = nil
	}
	return elem
}

//go:linkname reflect_mapaccess_faststr reflect.mapaccess_faststr
func reflect_mapaccess_faststr(t *maptype, h *hmap, key string) unsafe.Pointer {
	elem, ok := mapaccess2_faststr(t, h, key)
	if !ok {
		// reflect wants nil for a missing element
		elem = nil
	}
	return elem
}

//go:linkname reflect_mapassign reflect.mapassign0
func reflect_mapassign(t *maptype, h *hmap, key unsafe.Pointer, elem unsafe.Pointer) {
	p := mapassign(t, h, key)
	typedmemmove(t.Elem, p, elem)
}

//go:linkname reflect_mapassign_faststr reflect.mapassign_faststr0
func reflect_mapassign_faststr(t *maptype, h *hmap, key string, elem unsafe.Pointer) {
	p := mapassign_faststr(t, h, key)
	typedmemmove(t.Elem, p, elem)
}

//go:linkname reflect_mapdelete reflect.mapdelete
func reflect_mapdelete(t *maptype, h *hmap, key unsafe.Pointer) {
	mapdelete(t, h, key)
}

//go:linkname reflect_mapdelete_faststr reflect.mapdelete_faststr
func reflect_mapdelete_faststr(t *maptype, h *hmap, key string) {
	mapdelete_faststr(t, h, key)
}

//go:linkname reflect_mapiterinit reflect.mapiterinit
func reflect_mapiterinit(t *maptype, h *hmap, it *hiter) {
	mapiterinit(t, h, it)
}

//go:linkname reflect_mapiternext reflect.mapiternext
func reflect_mapiternext(it *hiter) {
	mapiternext(it)
}

//go:linkname reflect_mapiterkey reflect.mapiterkey
func reflect_mapiterkey(it *hiter) unsafe.Pointer {
	return it.key
}

//go:linkname reflect_mapiterelem reflect.mapiterelem
func reflect_mapiterelem(it *hiter) unsafe.Pointer {
	return it.elem
}

//go:linkname reflect_maplen reflect.maplen
func reflect_maplen(h *hmap) int {
	if h == nil {
		return 0
	}
	if raceenabled {
		callerpc := getcallerpc()
		racereadpc(unsafe.Pointer(h), callerpc, abi.FuncPCABIInternal(reflect_maplen))
	}
	return h.count
}

//go:linkname reflect_mapclear reflect.mapclear
func reflect_mapclear(t *maptype, h *hmap) {
	mapclear(t, h)
}

//go:linkname reflectlite_maplen internal/reflectlite.maplen
func reflectlite_maplen(h *hmap) int {
	if h == nil {
		return 0
	}
	if raceenabled {
		callerpc := getcallerpc()
		racereadpc(unsafe.Pointer(h), callerpc, abi.FuncPCABIInternal(reflect_maplen))
	}
	return h.count
}

const maxZero = 1024 // must match value in reflect/value.go:maxZero cmd/compile/internal/gc/walk.go:zeroValSize
var zeroVal [maxZero]byte

// mapinitnoop is a no-op function known the Go linker; if a given global
// map (of the right size) is determined to be dead, the linker will
// rewrite the relocation (from the package init func) from the outlined
// map init function to this symbol. Defined in assembly so as to avoid
// complications with instrumentation (coverage, etc).
func mapinitnoop()

// mapclone for implementing maps.Clone
//
//go:linkname mapclone maps.clone
func mapclone(m any) any {
	e := efaceOf(&m)
	e.data = unsafe.Pointer(mapclone2((*maptype)(unsafe.Pointer(e._type)), (*hmap)(e.data)))
	return m
}

// moveToBmap moves a bucket from src to dst. It returns the destination bucket or new destination bucket if it overflows
// and the pos that the next key/value will be written, if pos == bucketCnt means needs to written in overflow bucket.
func moveToBmap(t *maptype, h *hmap, dst *bmap, pos int, src *bmap) (*bmap, int) {
	for i := 0; i < bucketCnt; i++ {
		if isEmpty(src.tophash[i]) {
			continue
		}

		for ; pos < bucketCnt; pos++ {
			if isEmpty(dst.tophash[pos]) {
				break
			}
		}

		if pos == bucketCnt {
			dst = h.newoverflow(t, dst)
			pos = 0
		}

		srcK := add(unsafe.Pointer(src), dataOffset+uintptr(i)*uintptr(t.KeySize))
		srcEle := add(unsafe.Pointer(src), dataOffset+bucketCnt*uintptr(t.KeySize)+uintptr(i)*uintptr(t.ValueSize))
		dstK := add(unsafe.Pointer(dst), dataOffset+uintptr(pos)*uintptr(t.KeySize))
		dstEle := add(unsafe.Pointer(dst), dataOffset+bucketCnt*uintptr(t.KeySize)+uintptr(pos)*uintptr(t.ValueSize))

		dst.tophash[pos] = src.tophash[i]
		if t.IndirectKey() {
			srcK = *(*unsafe.Pointer)(srcK)
			if t.NeedKeyUpdate() {
				kStore := newobject(t.Key)
				typedmemmove(t.Key, kStore, srcK)
				srcK = kStore
			}
			// Note: if NeedKeyUpdate is false, then the memory
			// used to store the key is immutable, so we can share
			// it between the original map and its clone.
			*(*unsafe.Pointer)(dstK) = srcK
		} else {
			typedmemmove(t.Key, dstK, srcK)
		}
		if t.IndirectElem() {
			srcEle = *(*unsafe.Pointer)(srcEle)
			eStore := newobject(t.Elem)
			typedmemmove(t.Elem, eStore, srcEle)
			*(*unsafe.Pointer)(dstEle) = eStore
		} else {
			typedmemmove(t.Elem, dstEle, srcEle)
		}
		pos++
		h.count++
	}
	return dst, pos
}

func mapclone2(t *maptype, src *hmap) *hmap {
	dst := makemap(t, src.count, nil)
	dst.hash0 = src.hash0
	dst.nevacuate = 0
	//flags do not need to be copied here, just like a new map has no flags.

	if src.count == 0 {
		return dst
	}

	if src.flags&hashWriting != 0 {
		fatal("concurrent map clone and map write")
	}

	if src.B == 0 && !(t.IndirectKey() && t.NeedKeyUpdate()) && !t.IndirectElem() {
		// Quick copy for small maps.
		dst.buckets = newobject(t.Bucket)
		dst.count = src.count
		typedmemmove(t.Bucket, dst.buckets, src.buckets)
		return dst
	}

	if dst.B == 0 {
		dst.buckets = newobject(t.Bucket)
	}
	dstArraySize := int(bucketShift(dst.B))
	srcArraySize := int(bucketShift(src.B))
	for i := 0; i < dstArraySize; i++ {
		dstBmap := (*bmap)(add(dst.buckets, uintptr(i*int(t.BucketSize))))
		pos := 0
		for j := 0; j < srcArraySize; j += dstArraySize {
			srcBmap := (*bmap)(add(src.buckets, uintptr((i+j)*int(t.BucketSize))))
			for srcBmap != nil {
				dstBmap, pos = moveToBmap(t, dst, dstBmap, pos, srcBmap)
				srcBmap = srcBmap.overflow(t)
			}
		}
	}

	if src.oldbuckets == nil {
		return dst
	}

	oldB := src.B
	srcOldbuckets := src.oldbuckets
	if !src.sameSizeGrow() {
		oldB--
	}
	oldSrcArraySize := int(bucketShift(oldB))

	for i := 0; i < oldSrcArraySize; i++ {
		srcBmap := (*bmap)(add(srcOldbuckets, uintptr(i*int(t.BucketSize))))
		if evacuated(srcBmap) {
			continue
		}

		if oldB >= dst.B { // main bucket bits in dst is less than oldB bits in src
			dstBmap := (*bmap)(add(dst.buckets, (uintptr(i)&bucketMask(dst.B))*uintptr(t.BucketSize)))
			for dstBmap.overflow(t) != nil {
				dstBmap = dstBmap.overflow(t)
			}
			pos := 0
			for srcBmap != nil {
				dstBmap, pos = moveToBmap(t, dst, dstBmap, pos, srcBmap)
				srcBmap = srcBmap.overflow(t)
			}
			continue
		}

		// oldB < dst.B, so a single source bucket may go to multiple destination buckets.
		// Process entries one at a time.
		for srcBmap != nil {
			// move from oldBlucket to new bucket
			for i := uintptr(0); i < bucketCnt; i++ {
				if isEmpty(srcBmap.tophash[i]) {
					continue
				}

				if src.flags&hashWriting != 0 {
					fatal("concurrent map clone and map write")
				}

				srcK := add(unsafe.Pointer(srcBmap), dataOffset+i*uintptr(t.KeySize))
				if t.IndirectKey() {
					srcK = *((*unsafe.Pointer)(srcK))
				}

				srcEle := add(unsafe.Pointer(srcBmap), dataOffset+bucketCnt*uintptr(t.KeySize)+i*uintptr(t.ValueSize))
				if t.IndirectElem() {
					srcEle = *((*unsafe.Pointer)(srcEle))
				}
				dstEle := mapassign(t, dst, srcK)
				typedmemmove(t.Elem, dstEle, srcEle)
			}
			srcBmap = srcBmap.overflow(t)
		}
	}
	return dst
}

// keys for implementing maps.keys
//
//go:linkname keys maps.keys
func keys(m any, p unsafe.Pointer) {
	e := efaceOf(&m)
	t := (*maptype)(unsafe.Pointer(e._type))
	h := (*hmap)(e.data)

	if h == nil || h.count == 0 {
		return
	}
	s := (*slice)(p)
	r := int(fastrand())
	offset := uint8(r >> h.B & (bucketCnt - 1))
	if h.B == 0 {
		copyKeys(t, h, (*bmap)(h.buckets), s, offset)
		return
	}
	arraySize := int(bucketShift(h.B))
	buckets := h.buckets
	for i := 0; i < arraySize; i++ {
		bucket := (i + r) & (arraySize - 1)
		b := (*bmap)(add(buckets, uintptr(bucket)*uintptr(t.BucketSize)))
		copyKeys(t, h, b, s, offset)
	}

	if h.growing() {
		oldArraySize := int(h.noldbuckets())
		for i := 0; i < oldArraySize; i++ {
			bucket := (i + r) & (oldArraySize - 1)
			b := (*bmap)(add(h.oldbuckets, uintptr(bucket)*uintptr(t.BucketSize)))
			if evacuated(b) {
				continue
			}
			copyKeys(t, h, b, s, offset)
		}
	}
	return
}

func copyKeys(t *maptype, h *hmap, b *bmap, s *slice, offset uint8) {
	for b != nil {
		for i := uintptr(0); i < bucketCnt; i++ {
			offi := (i + uintptr(offset)) & (bucketCnt - 1)
			if isEmpty(b.tophash[offi]) {
				continue
			}
			if h.flags&hashWriting != 0 {
				fatal("concurrent map read and map write")
			}
			k := add(unsafe.Pointer(b), dataOffset+offi*uintptr(t.KeySize))
			if t.IndirectKey() {
				k = *((*unsafe.Pointer)(k))
			}
			if s.len >= s.cap {
				fatal("concurrent map read and map write")
			}
			typedmemmove(t.Key, add(s.array, uintptr(s.len)*uintptr(t.KeySize)), k)
			s.len++
		}
		b = b.overflow(t)
	}
}

// values for implementing maps.values
//
//go:linkname values maps.values
func values(m any, p unsafe.Pointer) {
	e := efaceOf(&m)
	t := (*maptype)(unsafe.Pointer(e._type))
	h := (*hmap)(e.data)
	if h == nil || h.count == 0 {
		return
	}
	s := (*slice)(p)
	r := int(fastrand())
	offset := uint8(r >> h.B & (bucketCnt - 1))
	if h.B == 0 {
		copyValues(t, h, (*bmap)(h.buckets), s, offset)
		return
	}
	arraySize := int(bucketShift(h.B))
	buckets := h.buckets
	for i := 0; i < arraySize; i++ {
		bucket := (i + r) & (arraySize - 1)
		b := (*bmap)(add(buckets, uintptr(bucket)*uintptr(t.BucketSize)))
		copyValues(t, h, b, s, offset)
	}

	if h.growing() {
		oldArraySize := int(h.noldbuckets())
		for i := 0; i < oldArraySize; i++ {
			bucket := (i + r) & (oldArraySize - 1)
			b := (*bmap)(add(h.oldbuckets, uintptr(bucket)*uintptr(t.BucketSize)))
			if evacuated(b) {
				continue
			}
			copyValues(t, h, b, s, offset)
		}
	}
	return
}

func copyValues(t *maptype, h *hmap, b *bmap, s *slice, offset uint8) {
	for b != nil {
		for i := uintptr(0); i < bucketCnt; i++ {
			offi := (i + uintptr(offset)) & (bucketCnt - 1)
			if isEmpty(b.tophash[offi]) {
				continue
			}

			if h.flags&hashWriting != 0 {
				fatal("concurrent map read and map write")
			}

			ele := add(unsafe.Pointer(b), dataOffset+bucketCnt*uintptr(t.KeySize)+offi*uintptr(t.ValueSize))
			if t.IndirectElem() {
				ele = *((*unsafe.Pointer)(ele))
			}
			if s.len >= s.cap {
				fatal("concurrent map read and map write")
			}
			typedmemmove(t.Elem, add(s.array, uintptr(s.len)*uintptr(t.ValueSize)), ele)
			s.len++
		}
		b = b.overflow(t)
	}
}
