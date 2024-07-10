// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/abi"
	"internal/goarch"
	"runtime/internal/atomic"
	"unsafe"
)

const itabInitSize = 512

var (
	itabLock      mutex                               // lock for accessing itab table
	itabTable     = &itabTableInit                    // pointer to current table
	itabTableInit = itabTableType{size: itabInitSize} // starter table
)

// Note: change the formula in the mallocgc call in itabAdd if you change these fields.
type itabTableType struct {
	size    uintptr             // length of entries array. Always a power of 2.
	count   uintptr             // current number of filled entries.
	entries [itabInitSize]*itab // really [size] large
}

func itabHashFunc(inter *interfacetype, typ *_type) uintptr {
	// compiler has provided some good hash codes for us.
	return uintptr(inter.Type.Hash ^ typ.Hash)
}

func getitab(inter *interfacetype, typ *_type, canfail bool) *itab {
	if len(inter.Methods) == 0 {
		throw("internal error - misuse of itab")
	}

	// 简单情况
	if typ.TFlag&abi.TFlagUncommon == 0 {
		// 如果是简单情况且可以失败，则返回 nil
		if canfail {
			return nil
		}
		// 否则创建 TypeAssertionError 错误并抛出
		name := toRType(&inter.Type).nameOff(inter.Methods[0].Name)
		panic(&TypeAssertionError{nil, typ, &inter.Type, name.Name()})
	}

	var m *itab

	// 首先，在不使用锁的情况下检查现有表中是否存在需要的 itab
	// 这是最常见的情况，因此不使用锁
	// 使用 atomic 来确保我们看到由更新 itabTable 字段的线程执行的任何先前写入操作
	t := (*itabTableType)(atomic.Loadp(unsafe.Pointer(&itabTable)))
	// 在 itabTable 中查找给定的接口/类型对。 如果给定的接口/类型对不存在，则返回 nil。
	if m = t.find(inter, typ); m != nil {
		goto finish
	}

	// 没找到。获得锁并再次尝试
	lock(&itabLock)
	// 在 itabTable 中查找给定的接口/类型对。 如果给定的接口/类型对不存在，则返回 nil。
	if m = itabTable.find(inter, typ); m != nil {
		unlock(&itabLock)
		goto finish
	}

	// 条目不存在。创建一个新条目并添加
	m = (*itab)(persistentalloc(unsafe.Sizeof(itab{})+uintptr(len(inter.Methods)-1)*goarch.PtrSize, 0, &memstats.other_sys))
	m.inter = inter
	m._type = typ
	// 哈希用于类型切换。但是，编译器静态生成适用于 switch 的所有接口/类型对的 itab
	// （这些 itab 在 itabsinit 中添加到 itabTable 中）。动态生成的 itab 不参与
	// 类型切换，因此哈希是不相关的。
	// 注意：m.hash 不是运行时 itabTable 哈希表使用的哈希值。
	m.hash = 0
	m.init()
	// 将给定的 itab 添加到 itab 哈希表中。必须持有 itab 锁。
	itabAdd(m)
	unlock(&itabLock)
finish:
	if m.fun[0] != 0 {
		return m
	}
	if canfail {
		return nil
	}
	// 这只会在已使用 , ok 形式的一次转换
	// 并且我们有一个缓存的负面结果时发生。
	// 缓存的结果不记录缺少的接口函数，因此重新初始化 itab
	// 以获取缺少的函数名称。
	panic(&TypeAssertionError{concrete: typ, asserted: &inter.Type, missingMethod: m.init()})
}

// 在 itabTable 中查找给定的接口/类型对。 如果给定的接口/类型对不存在，则返回 nil。
func (t *itabTableType) find(inter *interfacetype, typ *_type) *itab {
	// 使用二次探测法实现。
	// 探测序列为 h(i) = h0 + i*(i+1)/2 mod 2^k。
	// 使用此探测序列可确保访问到所有表项。
	mask := t.size - 1
	// 计算初始哈希值
	h := itabHashFunc(inter, typ) & mask
	// 开始二次探测循环
	for i := uintptr(1); ; i++ {
		// 根据当前哈希值计算数组索引
		p := (**itab)(add(unsafe.Pointer(&t.entries), h*goarch.PtrSize))
		// 在这里使用原子读取，以便如果看到 m != nil，则还要看到 m 字段的初始化。
		// m := *p
		m := (*itab)(atomic.Loadp(unsafe.Pointer(p)))
		// 检查该位置的 itab 是否为空，如果为空则表示未找到对应的映射
		if m == nil {
			return nil
		}
		// 检查找到的 itab 是否匹配目标的接口和类型
		if m.inter == inter && m._type == typ {
			return m // 找到了匹配的 itab，返回之
		}
		// 更新哈希值，进行下一次探测
		h += i
		h &= mask
	}
}

// 将给定的 itab 添加到 itab 哈希表中。必须持有 itab 锁。
func itabAdd(m *itab) {
	// 存在错误可能导致在分配内存时调用该函数，
	// 通常是因为在 panic 时调用了该函数。
	// 可靠地引发崩溃，而不仅仅是在需要扩展哈希表时才引发。
	if getg().m.mallocing != 0 {
		throw("malloc deadlock")
	}

	t := itabTable
	if t.count >= 3*(t.size/4) { // 75% 负载因子
		// 扩展哈希表。
		// t2 = new(itabTableType) + 一些额外的表项
		// 我们欺骗 malloc，告诉它我们希望得到无指针的内存，因为所有指向的值都不在堆中。
		t2 := (*itabTableType)(mallocgc((2+2*t.size)*goarch.PtrSize, nil, true))
		t2.size = t.size * 2

		// 复制条目。
		// 注意：在复制过程中，其他线程可能在查找 itab 时失败。
		// 这没有关系，它们接下来会尝试获取 itab 锁，
		// 结果等到此复制完成。
		iterate_itabs(t2.add)
		if t2.count != t.count {
			throw("mismatched count during itab table copy")
		}
		// 发布新的哈希表。使用原子写入：参见 getitab 中的注释。
		atomicstorep(unsafe.Pointer(&itabTable), unsafe.Pointer(t2))
		// 将新表格作为自己的表格。
		t = itabTable
		// 注意：旧表在此处可能被 GC。
	}
	t.add(m)
}

// add adds the given itab to itab table t.
// itabLock must be held.
func (t *itabTableType) add(m *itab) {
	// See comment in find about the probe sequence.
	// Insert new itab in the first empty spot in the probe sequence.
	mask := t.size - 1
	h := itabHashFunc(m.inter, m._type) & mask
	for i := uintptr(1); ; i++ {
		p := (**itab)(add(unsafe.Pointer(&t.entries), h*goarch.PtrSize))
		m2 := *p
		if m2 == m {
			// A given itab may be used in more than one module
			// and thanks to the way global symbol resolution works, the
			// pointed-to itab may already have been inserted into the
			// global 'hash'.
			return
		}
		if m2 == nil {
			// Use atomic write here so if a reader sees m, it also
			// sees the correctly initialized fields of m.
			// NoWB is ok because m is not in heap memory.
			// *p = m
			atomic.StorepNoWB(unsafe.Pointer(p), unsafe.Pointer(m))
			t.count++
			return
		}
		h += i
		h &= mask
	}
}

// init fills in the m.fun array with all the code pointers for
// the m.inter/m._type pair. If the type does not implement the interface,
// it sets m.fun[0] to 0 and returns the name of an interface function that is missing.
// It is ok to call this multiple times on the same m, even concurrently.
func (m *itab) init() string {
	inter := m.inter
	typ := m._type
	x := typ.Uncommon()

	// both inter and typ have method sorted by name,
	// and interface names are unique,
	// so can iterate over both in lock step;
	// the loop is O(ni+nt) not O(ni*nt).
	ni := len(inter.Methods)
	nt := int(x.Mcount)
	xmhdr := (*[1 << 16]abi.Method)(add(unsafe.Pointer(x), uintptr(x.Moff)))[:nt:nt]
	j := 0
	methods := (*[1 << 16]unsafe.Pointer)(unsafe.Pointer(&m.fun[0]))[:ni:ni]
	var fun0 unsafe.Pointer
imethods:
	for k := 0; k < ni; k++ {
		i := &inter.Methods[k]
		itype := toRType(&inter.Type).typeOff(i.Typ)
		name := toRType(&inter.Type).nameOff(i.Name)
		iname := name.Name()
		ipkg := pkgPath(name)
		if ipkg == "" {
			ipkg = inter.PkgPath.Name()
		}
		for ; j < nt; j++ {
			t := &xmhdr[j]
			rtyp := toRType(typ)
			tname := rtyp.nameOff(t.Name)
			if rtyp.typeOff(t.Mtyp) == itype && tname.Name() == iname {
				pkgPath := pkgPath(tname)
				if pkgPath == "" {
					pkgPath = rtyp.nameOff(x.PkgPath).Name()
				}
				if tname.IsExported() || pkgPath == ipkg {
					if m != nil {
						ifn := rtyp.textOff(t.Ifn)
						if k == 0 {
							fun0 = ifn // we'll set m.fun[0] at the end
						} else {
							methods[k] = ifn
						}
					}
					continue imethods
				}
			}
		}
		// didn't find method
		m.fun[0] = 0
		return iname
	}
	m.fun[0] = uintptr(fun0)
	return ""
}

func itabsinit() {
	lockInit(&itabLock, lockRankItab)
	lock(&itabLock)
	for _, md := range activeModules() {
		for _, i := range md.itablinks {
			itabAdd(i)
		}
	}
	unlock(&itabLock)
}

// panicdottypeE is called when doing an e.(T) conversion and the conversion fails.
// have = the dynamic type we have.
// want = the static type we're trying to convert to.
// iface = the static type we're converting from.
func panicdottypeE(have, want, iface *_type) {
	panic(&TypeAssertionError{iface, have, want, ""})
}

// panicdottypeI is called when doing an i.(T) conversion and the conversion fails.
// Same args as panicdottypeE, but "have" is the dynamic itab we have.
func panicdottypeI(have *itab, want, iface *_type) {
	var t *_type
	if have != nil {
		t = have._type
	}
	panicdottypeE(t, want, iface)
}

// panicnildottype is called when doing an i.(T) conversion and the interface i is nil.
// want = the static type we're trying to convert to.
func panicnildottype(want *_type) {
	panic(&TypeAssertionError{nil, nil, want, ""})
	// TODO: Add the static type we're converting from as well.
	// It might generate a better error message.
	// Just to match other nil conversion errors, we don't for now.
}

// The specialized convTx routines need a type descriptor to use when calling mallocgc.
// We don't need the type to be exact, just to have the correct size, alignment, and pointer-ness.
// However, when debugging, it'd be nice to have some indication in mallocgc where the types came from,
// so we use named types here.
// We then construct interface values of these types,
// and then extract the type word to use as needed.
type (
	uint16InterfacePtr uint16
	uint32InterfacePtr uint32
	uint64InterfacePtr uint64
	stringInterfacePtr string
	sliceInterfacePtr  []byte
)

var (
	uint16Eface any = uint16InterfacePtr(0)
	uint32Eface any = uint32InterfacePtr(0)
	uint64Eface any = uint64InterfacePtr(0)
	stringEface any = stringInterfacePtr("")
	sliceEface  any = sliceInterfacePtr(nil)

	uint16Type *_type = efaceOf(&uint16Eface)._type
	uint32Type *_type = efaceOf(&uint32Eface)._type
	uint64Type *_type = efaceOf(&uint64Eface)._type
	stringType *_type = efaceOf(&stringEface)._type
	sliceType  *_type = efaceOf(&sliceEface)._type
)

// The conv and assert functions below do very similar things.
// The convXXX functions are guaranteed by the compiler to succeed.
// The assertXXX functions may fail (either panicking or returning false,
// depending on whether they are 1-result or 2-result).
// The convXXX functions succeed on a nil input, whereas the assertXXX
// functions fail on a nil input.

// convT converts a value of type t, which is pointed to by v, to a pointer that can
// be used as the second word of an interface value.
func convT(t *_type, v unsafe.Pointer) unsafe.Pointer {
	if raceenabled {
		raceReadObjectPC(t, v, getcallerpc(), abi.FuncPCABIInternal(convT))
	}
	if msanenabled {
		msanread(v, t.Size_)
	}
	if asanenabled {
		asanread(v, t.Size_)
	}
	x := mallocgc(t.Size_, t, true)
	typedmemmove(t, x, v)
	return x
}
func convTnoptr(t *_type, v unsafe.Pointer) unsafe.Pointer {
	// TODO: maybe take size instead of type?
	if raceenabled {
		raceReadObjectPC(t, v, getcallerpc(), abi.FuncPCABIInternal(convTnoptr))
	}
	if msanenabled {
		msanread(v, t.Size_)
	}
	if asanenabled {
		asanread(v, t.Size_)
	}

	x := mallocgc(t.Size_, t, false)
	memmove(x, v, t.Size_)
	return x
}

func convT16(val uint16) (x unsafe.Pointer) {
	if val < uint16(len(staticuint64s)) {
		x = unsafe.Pointer(&staticuint64s[val])
		if goarch.BigEndian {
			x = add(x, 6)
		}
	} else {
		x = mallocgc(2, uint16Type, false)
		*(*uint16)(x) = val
	}
	return
}

func convT32(val uint32) (x unsafe.Pointer) {
	if val < uint32(len(staticuint64s)) {
		x = unsafe.Pointer(&staticuint64s[val])
		if goarch.BigEndian {
			x = add(x, 4)
		}
	} else {
		x = mallocgc(4, uint32Type, false)
		*(*uint32)(x) = val
	}
	return
}

func convT64(val uint64) (x unsafe.Pointer) {
	if val < uint64(len(staticuint64s)) {
		x = unsafe.Pointer(&staticuint64s[val])
	} else {
		x = mallocgc(8, uint64Type, false)
		*(*uint64)(x) = val
	}
	return
}

func convTstring(val string) (x unsafe.Pointer) {
	if val == "" {
		x = unsafe.Pointer(&zeroVal[0])
	} else {
		x = mallocgc(unsafe.Sizeof(val), stringType, true)
		*(*string)(x) = val
	}
	return
}

func convTslice(val []byte) (x unsafe.Pointer) {
	// Note: this must work for any element type, not just byte.
	if (*slice)(unsafe.Pointer(&val)).array == nil {
		x = unsafe.Pointer(&zeroVal[0])
	} else {
		x = mallocgc(unsafe.Sizeof(val), sliceType, true)
		*(*[]byte)(x) = val
	}
	return
}

// convI2I returns the new itab to be used for the destination value
// when converting a value with itab src to the dst interface.
func convI2I(dst *interfacetype, src *itab) *itab {
	if src == nil {
		return nil
	}
	if src.inter == dst {
		return src
	}
	return getitab(dst, src._type, false)
}

func assertI2I(inter *interfacetype, tab *itab) *itab {
	if tab == nil {
		// explicit conversions require non-nil interface value.
		panic(&TypeAssertionError{nil, nil, &inter.Type, ""})
	}
	if tab.inter == inter {
		return tab
	}
	return getitab(inter, tab._type, false)
}

func assertI2I2(inter *interfacetype, i iface) (r iface) {
	tab := i.tab
	if tab == nil {
		return
	}
	if tab.inter != inter {
		tab = getitab(inter, tab._type, true)
		if tab == nil {
			return
		}
	}
	r.tab = tab
	r.data = i.data
	return
}

func assertE2I(inter *interfacetype, t *_type) *itab {
	if t == nil {
		// explicit conversions require non-nil interface value.
		panic(&TypeAssertionError{nil, nil, &inter.Type, ""})
	}
	return getitab(inter, t, false)
}

func assertE2I2(inter *interfacetype, e eface) (r iface) {
	t := e._type
	if t == nil {
		return
	}
	tab := getitab(inter, t, true)
	if tab == nil {
		return
	}
	r.tab = tab
	r.data = e.data
	return
}

//go:linkname reflect_ifaceE2I reflect.ifaceE2I
func reflect_ifaceE2I(inter *interfacetype, e eface, dst *iface) {
	*dst = iface{assertE2I(inter, e._type), e.data}
}

//go:linkname reflectlite_ifaceE2I internal/reflectlite.ifaceE2I
func reflectlite_ifaceE2I(inter *interfacetype, e eface, dst *iface) {
	*dst = iface{assertE2I(inter, e._type), e.data}
}

func iterate_itabs(fn func(*itab)) {
	// Note: only runs during stop the world or with itabLock held,
	// so no other locks/atomics needed.
	t := itabTable
	for i := uintptr(0); i < t.size; i++ {
		m := *(**itab)(add(unsafe.Pointer(&t.entries), i*goarch.PtrSize))
		if m != nil {
			fn(m)
		}
	}
}

// staticuint64s is used to avoid allocating in convTx for small integer values.
var staticuint64s = [...]uint64{
	0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07,
	0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f,
	0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17,
	0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f,
	0x20, 0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27,
	0x28, 0x29, 0x2a, 0x2b, 0x2c, 0x2d, 0x2e, 0x2f,
	0x30, 0x31, 0x32, 0x33, 0x34, 0x35, 0x36, 0x37,
	0x38, 0x39, 0x3a, 0x3b, 0x3c, 0x3d, 0x3e, 0x3f,
	0x40, 0x41, 0x42, 0x43, 0x44, 0x45, 0x46, 0x47,
	0x48, 0x49, 0x4a, 0x4b, 0x4c, 0x4d, 0x4e, 0x4f,
	0x50, 0x51, 0x52, 0x53, 0x54, 0x55, 0x56, 0x57,
	0x58, 0x59, 0x5a, 0x5b, 0x5c, 0x5d, 0x5e, 0x5f,
	0x60, 0x61, 0x62, 0x63, 0x64, 0x65, 0x66, 0x67,
	0x68, 0x69, 0x6a, 0x6b, 0x6c, 0x6d, 0x6e, 0x6f,
	0x70, 0x71, 0x72, 0x73, 0x74, 0x75, 0x76, 0x77,
	0x78, 0x79, 0x7a, 0x7b, 0x7c, 0x7d, 0x7e, 0x7f,
	0x80, 0x81, 0x82, 0x83, 0x84, 0x85, 0x86, 0x87,
	0x88, 0x89, 0x8a, 0x8b, 0x8c, 0x8d, 0x8e, 0x8f,
	0x90, 0x91, 0x92, 0x93, 0x94, 0x95, 0x96, 0x97,
	0x98, 0x99, 0x9a, 0x9b, 0x9c, 0x9d, 0x9e, 0x9f,
	0xa0, 0xa1, 0xa2, 0xa3, 0xa4, 0xa5, 0xa6, 0xa7,
	0xa8, 0xa9, 0xaa, 0xab, 0xac, 0xad, 0xae, 0xaf,
	0xb0, 0xb1, 0xb2, 0xb3, 0xb4, 0xb5, 0xb6, 0xb7,
	0xb8, 0xb9, 0xba, 0xbb, 0xbc, 0xbd, 0xbe, 0xbf,
	0xc0, 0xc1, 0xc2, 0xc3, 0xc4, 0xc5, 0xc6, 0xc7,
	0xc8, 0xc9, 0xca, 0xcb, 0xcc, 0xcd, 0xce, 0xcf,
	0xd0, 0xd1, 0xd2, 0xd3, 0xd4, 0xd5, 0xd6, 0xd7,
	0xd8, 0xd9, 0xda, 0xdb, 0xdc, 0xdd, 0xde, 0xdf,
	0xe0, 0xe1, 0xe2, 0xe3, 0xe4, 0xe5, 0xe6, 0xe7,
	0xe8, 0xe9, 0xea, 0xeb, 0xec, 0xed, 0xee, 0xef,
	0xf0, 0xf1, 0xf2, 0xf3, 0xf4, 0xf5, 0xf6, 0xf7,
	0xf8, 0xf9, 0xfa, 0xfb, 0xfc, 0xfd, 0xfe, 0xff,
}

// The linker redirects a reference of a method that it determined
// unreachable to a reference to this function, so it will throw if
// ever called.
func unreachableMethod() {
	throw("unreachable method called. linker bug?")
}
