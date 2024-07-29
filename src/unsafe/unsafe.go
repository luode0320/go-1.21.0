// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

/*
Package unsafe contains operations that step around the type safety of Go programs.

Packages that import unsafe may be non-portable and are not protected by the
Go 1 compatibility guidelines.
*/
package unsafe

// ArbitraryType 仅用于文档目的，实际上并不是 unsafe 包的一部分。它表示任意 Go 表达式的类型。
type ArbitraryType int

// IntegerType 仅用于文档目的，实际上并不是 unsafe 包的一部分。它表示任意整数类型。
type IntegerType int

// Pointer 表示指向任意类型的指针。类型 Pointer 有四个特殊操作，其他类型没有：
//   - 任意类型的指针值可以转换为 Pointer。
//   - Pointer 可以转换为任意类型的指针值。
//   - uintptr 可以转换为 Pointer。
//   - Pointer 可以转换为 uintptr。
//
// 因此，Pointer 允许程序破坏类型系统，读取和写入任意内存。应该非常小心地使用它。
//
// 以下模式涉及 Pointer 是有效的。
// 不使用这些模式的代码在今天很可能是无效的，或者将来会无效。
// 即使下面的有效模式也带有重要的警告。
//
// 运行 "go vet" 可以帮助找到不符合这些模式的 Pointer 使用，
// 但是 "go vet" 的沉黙并不保证代码是有效的。
//
// (1) 将 *T1 转换为指向 *T2 的 Pointer。
//
// 假设 T2 不比 T1 大，并且两者共享等效的内存布局，这种转换允许将一种类型的数据重新解释为另一种类型的数据。例如，math.Float64bits 的实现如下：
//
//	func Float64bits(f float64) uint64 {
//		return *(*uint64)(unsafe.Pointer(&f))
//	}
//
// (2) 将 Pointer 转换为 uintptr（但不再转换回 Pointer）。
//
// 将 Pointer 转换为 uintptr 会将指向的值的内存地址作为整数值返回。通常用途是打印。
//
// 通常情况下，不能将 uintptr 转换回 Pointer。
//
// uintptr 是整数，而不是引用。
// 将 Pointer 转换为 uintptr 会创建一个没有指针语义的整数值。
// 即使 uintptr 持有某个对象的地址，垃圾回收器也不会在对象移动时更新 uintptr 的值，也不会阻止对象被回收。
//
// 剩下的模式列举了从 uintptr 转换为 Pointer 的唯一有效转换。
//
// (3) 将 Pointer 转换为 uintptr 并执行算术操作后再转换回来。
//
// 如果 p 指向一个分配的对象，则可以通过将其转换为 uintptr，添加偏移量，然后再转换回 Pointer 来对该对象进行操作。
//
//	p = unsafe.Pointer(uintptr(p) + offset)
//
// 这种模式的最常见用法是访问结构体的字段或数组的元素：
//
//	// 等同于 f := unsafe.Pointer(&s.f)
//	f := unsafe.Pointer(uintptr(unsafe.Pointer(&s)) + unsafe.Offsetof(s.f))
//
//	// 等同于 e := unsafe.Pointer(&x[i])
//	e := unsafe.Pointer(uintptr(unsafe.Pointer(&x[0])) + i*unsafe.Sizeof(x[0]))
//
// 在这种方式中，从指针中增加或减少偏移量是有效的。
// 也可以使用 &^ 进行指针舍入，通常用于对齐。
// 在所有情况下，结果必须继续指向原始分配的对象。
//
// 不像在 C 中，不可以将指针移动到超出原始分配结构的末尾：
//
//	// 无效：end 指向分配空间之外。
//	var s thing
//	end = unsafe.Pointer(uintptr(unsafe.Pointer(&s)) + unsafe.Sizeof(s))
//
//	// 无效：end 指向分配空间之外。
//	b := make([]byte, n)
//	end = unsafe.Pointer(uintptr(unsafe.Pointer(&b[0])) + uintptr(n))
//
// 请注意，这两个转换必须出现在同一个表达式中，中间只能有算术运算：
//
//	// 无效：uintptr 不能在转换回 Pointer 之前存储在变量中。
//	u := uintptr(p)
//	p = unsafe.Pointer(u + offset)
//
// 请注意，指针必须指向一个已分配的对象，因此不能为 nil。
//
//	// 无效：转换 nil 指针
//	u := unsafe.Pointer(nil)
//	p := unsafe.Pointer(uintptr(u) + offset)
//
// (4) 在调用 syscall.Syscall 时将 Pointer 转换为 uintptr。
//
// 包 syscall 中的 Syscall 函数将其 uintptr 参数直接传递给操作系统，然后根据调用的详细信息，
// 将其中一些重新解释为指针。
// 换句话说，系统调用实现隐式地将某些参数从 uintptr 转换回指针。
//
// 如果指针参数必须转换为 uintptr 以用作参数，那么转换必须出现在调用表达式本身中：
//
//	syscall.Syscall(SYS_READ, uintptr(fd), uintptr(unsafe.Pointer(p)), uintptr(n))
//
// 编译器通过在由汇编实现的函数调用的参数列表中将 Pointer 转换为 uintptr，
// 安排保持所引用的已分配对象，即使从类型上看似乎在调用期间不再需要该对象。
//
// 对于编译器来说，为了识别这种模式，
// 转换必须出现在参数列表中：
//
//	// 无效：uintptr 不能在系统调用期间的 Pointer 隐式转换回来之前存储在变量中。
//	u := uintptr(unsafe.Pointer(p))
//	syscall.Syscall(SYS_READ, uintptr(fd), u, uintptr(n))
//
// (5) 将 reflect.Value.Pointer 或 reflect.Value.UnsafeAddr 的结果从 uintptr 转换为 Pointer。
//
// reflect 包的 Value 方法名为 Pointer 和 UnsafeAddr 返回类型 uintptr 而不是 unsafe.Pointer，
// 以防止调用者在未导入 "unsafe" 的情况下将结果更改为任意类型。但是，这意味着结果是脆弱的，
// 必须在调用后立即将其转换为 Pointer，即在同一表达式中：
//
//	p := (*int)(unsafe.Pointer(reflect.ValueOf(new(int)).Pointer()))
//
// 与上述情况一样，在转换之前不能存储结果：
//
//	// 无效：uintptr 不能在转换回 Pointer 之前存储在变量中。
//	u := reflect.ValueOf(new(int)).Pointer()
//	p := (*int)(unsafe.Pointer(u))
//
// (6) 将 reflect.SliceHeader 或 reflect.StringHeader 的 Data 字段从 uintptr 转换为 Pointer 或相反。
//
// 与前一个情况一样，reflect 数据结构 SliceHeader 和 StringHeader 将 Data 字段声明为 uintptr，
// 以防止调用者在未导入 "unsafe" 的情况下将结果更改为任意类型。但是，这意味着
// SliceHeader 和 StringHeader 仅在解释实际片段或字符串值的内容时才有效。
//
//	var s string
//	hdr := (*reflect.StringHeader)(unsafe.Pointer(&s)) // 情况 1
//	hdr.Data = uintptr(unsafe.Pointer(p))              // 情况 6（此情况）
//	hdr.Len = n
//
// 在这种用法中，hdr.Data 实际上是指向字符串头部中底层指针的另一种方式，而不是一个 uintptr 变量本身。
//
// 通常情况下，reflect.SliceHeader 和 reflect.StringHeader 应该只作为指向实际切片或字符串的 *reflect.SliceHeader 和 *reflect.StringHeader 使用，
// 不应该作为普通结构体使用。
// 程序不应该声明或分配这些结构类型的变量。
//
//	// 无效：直接声明的头部不会保存 Data 作为引用。
//	var hdr reflect.StringHeader
//	hdr.Data = uintptr(unsafe.Pointer(p))
//	hdr.Len = n
//	s := *(*string)(unsafe.Pointer(&hdr))  // p 可能已经丢失
type Pointer *ArbitraryType

// Sizeof takes an expression x of any type and returns the size in bytes
// of a hypothetical variable v as if v was declared via var v = x.
// The size does not include any memory possibly referenced by x.
// For instance, if x is a slice, Sizeof returns the size of the slice
// descriptor, not the size of the memory referenced by the slice.
// For a struct, the size includes any padding introduced by field alignment.
// The return value of Sizeof is a Go constant if the type of the argument x
// does not have variable size.
// (A type has variable size if it is a type parameter or if it is an array
// or struct type with elements of variable size).
func Sizeof(x ArbitraryType) uintptr

// Offsetof returns the offset within the struct of the field represented by x,
// which must be of the form structValue.field. In other words, it returns the
// number of bytes between the start of the struct and the start of the field.
// The return value of Offsetof is a Go constant if the type of the argument x
// does not have variable size.
// (See the description of [Sizeof] for a definition of variable sized types.)
func Offsetof(x ArbitraryType) uintptr

// Alignof takes an expression x of any type and returns the required alignment
// of a hypothetical variable v as if v was declared via var v = x.
// It is the largest value m such that the address of v is always zero mod m.
// It is the same as the value returned by reflect.TypeOf(x).Align().
// As a special case, if a variable s is of struct type and f is a field
// within that struct, then Alignof(s.f) will return the required alignment
// of a field of that type within a struct. This case is the same as the
// value returned by reflect.TypeOf(s.f).FieldAlign().
// The return value of Alignof is a Go constant if the type of the argument
// does not have variable size.
// (See the description of [Sizeof] for a definition of variable sized types.)
func Alignof(x ArbitraryType) uintptr

// The function Add adds len to ptr and returns the updated pointer
// Pointer(uintptr(ptr) + uintptr(len)).
// The len argument must be of integer type or an untyped constant.
// A constant len argument must be representable by a value of type int;
// if it is an untyped constant it is given type int.
// The rules for valid uses of Pointer still apply.
func Add(ptr Pointer, len IntegerType) Pointer

// The function Slice returns a slice whose underlying array starts at ptr
// and whose length and capacity are len.
// Slice(ptr, len) is equivalent to
//
//	(*[len]ArbitraryType)(unsafe.Pointer(ptr))[:]
//
// except that, as a special case, if ptr is nil and len is zero,
// Slice returns nil.
//
// The len argument must be of integer type or an untyped constant.
// A constant len argument must be non-negative and representable by a value of type int;
// if it is an untyped constant it is given type int.
// At run time, if len is negative, or if ptr is nil and len is not zero,
// a run-time panic occurs.
func Slice(ptr *ArbitraryType, len IntegerType) []ArbitraryType

// SliceData returns a pointer to the underlying array of the argument
// slice.
//   - If cap(slice) > 0, SliceData returns &slice[:1][0].
//   - If slice == nil, SliceData returns nil.
//   - Otherwise, SliceData returns a non-nil pointer to an
//     unspecified memory address.
func SliceData(slice []ArbitraryType) *ArbitraryType

// String returns a string value whose underlying bytes
// start at ptr and whose length is len.
//
// The len argument must be of integer type or an untyped constant.
// A constant len argument must be non-negative and representable by a value of type int;
// if it is an untyped constant it is given type int.
// At run time, if len is negative, or if ptr is nil and len is not zero,
// a run-time panic occurs.
//
// Since Go strings are immutable, the bytes passed to String
// must not be modified afterwards.
func String(ptr *byte, len IntegerType) string

// StringData returns a pointer to the underlying bytes of str.
// For an empty string the return value is unspecified, and may be nil.
//
// Since Go strings are immutable, the bytes returned by StringData
// must not be modified.
func StringData(str string) *byte
