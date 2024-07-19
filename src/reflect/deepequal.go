// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Deep equality test via reflection

package reflect

import (
	"internal/bytealg"
	"unsafe"
)

// During deepValueEqual, must keep track of checks that are
// in progress. The comparison algorithm assumes that all
// checks in progress are true when it reencounters them.
// Visited comparisons are stored in a map indexed by visit.
type visit struct {
	a1  unsafe.Pointer
	a2  unsafe.Pointer
	typ Type
}

// 函数用于比较两个反射值 v1 和 v2 是否深度相等。
func deepValueEqual(v1, v2 Value, visited map[visit]bool) bool {
	// 如果 v1 或 v2 无效，则比较它们的有效性
	if !v1.IsValid() || !v2.IsValid() {
		return v1.IsValid() == v2.IsValid()
	}

	// 如果 v1 和 v2 的类型不同，则它们不可能是深度相等的
	if v1.Type() != v2.Type() {
		return false
	}

	// 循环引用相关的处理

	// hard 函数用于确定是否需要考虑循环引用
	hard := func(v1, v2 Value) bool {
		switch v1.Kind() {
		case Pointer:
			if v1.typ().PtrBytes == 0 {
				// 不在堆上的指针不会是循环引用
				return false
			}
			fallthrough
		case Map, Slice, Interface:
			// 空指针不会是循环引用
			return !v1.IsNil() && !v2.IsNil()
		}
		return false
	}

	// 如果存在循环引用，则通过内部指针进行检查
	if hard(v1, v2) {
		// 对于指针或映射值，我们需要检查flagIndir，我们通过调用指针方法。
		// 对于切片或接口，总是设置flagIndir，使用v.Ptr就足够了。
		ptrval := func(v Value) unsafe.Pointer {
			switch v.Kind() {
			case Pointer, Map:
				return v.pointer()
			default:
				return v.ptr
			}
		}
		addr1 := ptrval(v1)
		addr2 := ptrval(v2)
		if uintptr(addr1) > uintptr(addr2) {
			// 规范化顺序以减少访问的条目数量。
			// 假设非移动垃圾收集器。
			addr1, addr2 = addr2, addr1
		}

		// 如果引用已经被检查过，则直接返回 true
		typ := v1.Type()
		v := visit{addr1, addr2, typ}
		if visited[v] {
			return true
		}
		// 记录当前引用
		visited[v] = true
	}

	// 真正的比较

	// 返回一个 reflect.Value 对象所表示的值的种类
	switch v1.Kind() {
	case Array: // 对于数组类型的反射值，逐个比较数组元素是否深度相等
		// 遍历数组的每个元素
		for i := 0; i < v1.Len(); i++ {
			// 递归调用 deepValueEqual 函数比较数组的每个元素
			if !deepValueEqual(v1.Index(i), v2.Index(i), visited) {
				return false
			}
		}
		// 如果所有元素都相等，则返回 true
		return true

	case Slice: // 对于切片类型的反射值，逐个比较切片元素是否深度相等
		// 如果一个切片为 nil 而另一个不为 nil，则它们不相等
		if v1.IsNil() != v2.IsNil() {
			return false
		}

		// 如果两个切片长度不相等，则它们不相等
		if v1.Len() != v2.Len() {
			return false
		}

		// 如果切片底层数组的指针相同，则认为它们相等
		if v1.UnsafePointer() == v2.UnsafePointer() {
			return true
		}

		// 对于 []byte 类型的切片，采用特殊处理，直接比较字节内容
		if v1.Type().Elem().Kind() == Uint8 {
			return bytealg.Equal(v1.Bytes(), v2.Bytes())
		}

		// 逐个比较切片中的元素
		for i := 0; i < v1.Len(); i++ {
			// 递归调用 deepValueEqual 函数比较切片的每个元素
			if !deepValueEqual(v1.Index(i), v2.Index(i), visited) {
				return false
			}
		}
		// 如果所有元素都相等，则返回 true
		return true

	case Interface: // 对于接口类型的反射值，比较接口持有的值是否深度相等
		// 如果其中一个接口值为 nil，则两个接口值只有在都为 nil 时才相等
		if v1.IsNil() || v2.IsNil() {
			return v1.IsNil() == v2.IsNil()
		}
		// 递归比较接口持有的值
		return deepValueEqual(v1.Elem(), v2.Elem(), visited)

	case Pointer: // 对于指针类型的反射值，比较指向的值是否深度相等
		// 如果两个指针指向相同的内存地址，则认为它们相等
		if v1.UnsafePointer() == v2.UnsafePointer() {
			return true
		}
		// 否则，递归比较指针指向的值
		return deepValueEqual(v1.Elem(), v2.Elem(), visited)

	case Struct: // 对于结构体类型的反射值，逐个比较结构体字段的值是否深度相等
		// 遍历结构体的每个字段
		for i, n := 0, v1.NumField(); i < n; i++ {
			// 递归比较两个结构体字段的值
			if !deepValueEqual(v1.Field(i), v2.Field(i), visited) {
				return false
			}
		}
		// 所有字段的值都相等，返回 true
		return true

	case Map: // 对于 Map 类型的反射值，逐个比较映射的键值对是否深度相等
		// 首先检查两个映射是否同时为 nil 或非 nil
		if v1.IsNil() != v2.IsNil() {
			return false
		}

		// 检查两个映射的长度是否相等
		if v1.Len() != v2.Len() {
			return false
		}

		// 如果两个映射指向相同的内存地址，则认为它们相等
		if v1.UnsafePointer() == v2.UnsafePointer() {
			return true
		}

		// 遍历第一个映射的所有键
		for _, k := range v1.MapKeys() {
			// 获取键对应的值
			val1 := v1.MapIndex(k)
			val2 := v2.MapIndex(k)
			// 检查值是否有效，以及递归比较值是否相等
			if !val1.IsValid() || !val2.IsValid() || !deepValueEqual(val1, val2, visited) {
				return false
			}
		}
		// 所有键值对相等，返回 true
		return true

	case Func: // 对于不同函数类型的反射值做相等性比较
		// 函数类型的比较，只有当两个函数均为 nil 时才相等
		if v1.IsNil() && v2.IsNil() {
			return true
		}
		// 否则认为不相等
		return false

	case Int, Int8, Int16, Int32, Int64:
		// 对于整型类型，比较其整数值是否相等
		return v1.Int() == v2.Int()
	case Uint, Uint8, Uint16, Uint32, Uint64, Uintptr:
		// 对于无符号整型类型，比较其无符号整数值是否相等
		return v1.Uint() == v2.Uint()

	case String:
		// 对于字符串类型，比较其字符串值是否相等
		return v1.String() == v2.String()

	case Bool:
		// 对于布尔类型，比较其布尔值是否相等
		return v1.Bool() == v2.Bool()

	case Float32, Float64:
		// 对于浮点数类型，比较其浮点数值是否相等
		return v1.Float() == v2.Float()

	case Complex64, Complex128:
		// 对于复数类型，比较其复数值是否相等
		return v1.Complex() == v2.Complex()
	default:
		// 对于其他类型，使用普通的相等性比较
		return valueInterface(v1, false) == valueInterface(v2, false)
	}
}

// DeepEqual 报告 x 和 y 是否“深度相等”，定义如下：
// 如果以下情况之一适用，则相同类型的两个值被视为深度相等，不同类型的值永远不会深度相等。
//
// 当其对应元素深度相等时，数组值被视为深度相等。
//
// 如果导出字段和非导出字段均深度相等，则结构体值被视为深度相等。
//
// 如果两者均为 nil，则函数值深度相等；否则，它们不深度相等。
//
// 如果持有深度相等具体值，则接口值被视为深度相等。
//
// 当以下所有条件都为真时，映射值被视为深度相等：
// 它们均为 nil 或均非 nil，它们具有相同的长度，并且它们是同一个映射对象或它们的对应键（使用 Go 等式进行匹配）映射到深度相等值。
//
// 如果它们使用 Go 的 == 运算符相等，或者它们指向深度相等值，则指针值被视为深度相等。
//
// 当以下所有条件都为真时，切片值被视为深度相等：
// 它们均为 nil 或均非 nil，它们具有相同的长度，并且它们指向相同基础数组的同一个初始条目（也就是 &x[0] == &y[0]），
// 或者它们的对应元素（直到长度结束）是深度相等的。
// 注意，非 nil 空切片和 nil 切片（例如，[]byte{} 和 []byte(nil)）不深度相等。
//
// 其他值 - 数字、布尔值、字符串和通道 - 如果它们使用 Go 的 == 运算符相等，则它们被视为深度相等。
//
// 通常来说，DeepEqual 是 Go 的 == 运算符的递归松弛。
// 然而，这个想法在没有一些不一致性的情况下是不可能实现的。
// 具体来说，一个值可能不等于它自身，因为它是函数类型（通常无法比较），
// 或者因为它是浮点 NaN 值（在浮点比较中不等于自身），
// 或者因为它是包含这样一个值的数组、结构体或接口。
// 另一方面，指针值始终等于自身，即使它们指向或包含这样的问题值，
// 因为它们使用 Go 的 == 运算符比较相等，并且这是深度相等的充分条件，而不考虑内容。
// DeepEqual 已经被定义为对切片和映射应用相同的快捷方式：如果 x 和 y 是相同的切片或相同的映射，则它们被视为深度相等，无论内容如何。
//
// 当 DeepEqual 遍历数据值时，可能会发现一个循环。DeepEqual 第二次及以后比较两个先前比较过的指针值时，
// 它将将这些值视为相等，而不是检查它们指向的值。
// 这确保了 DeepEqual 终止。
func DeepEqual(x, y any) bool {
	// 如果 x 或 y 有一个为 nil，则直接比较它们的指针是否相等
	if x == nil || y == nil {
		return x == y
	}

	// 获取 x 和 y 的反射值
	v1 := ValueOf(x)
	v2 := ValueOf(y)

	// 如果 x 和 y 的类型不相同，则它们不可能是深度相等的
	if v1.Type() != v2.Type() {
		return false
	}

	// 调用 deepValueEqual 函数进行深度比较
	return deepValueEqual(v1, v2, make(map[visit]bool))
}
