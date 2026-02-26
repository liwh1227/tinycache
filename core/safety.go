package core

import (
	"fmt"
	"reflect"
)

// assertNoPointers 在创建缓存时校验 T 是否为纯值类型
//
// 这是整个 GC 优化方案的基石：只有当 []slot[T] 不包含任何指针时，
// Go runtime 才会将其标记为 no-scan，GC 扫描时间从 O(N) 降为 O(1)。
//
// 包含指针的类型（即使是间接的）：
//   - string（底层 StringHeader 含 Data 指针）
//   - slice（底层 SliceHeader 含 Data 指针）
//   - map、chan、func、interface、*T
//
// 不包含指针的类型：
//   - int/uint/float/complex/bool 系列
//   - [N]int 等固定长度数组（元素也必须无指针）
//   - struct（所有字段都必须无指针）
func assertNoPointers[T any]() {
	//var zero T
	//t := reflect.TypeOf(zero)

	t := reflect.TypeOf((*T)(nil)).Elem()

	// T 是 interface 或 nil 时的特殊处理
	//if t == nil {
	//	panic("tinycache: type parameter T must be a concrete type, not interface or nil")
	//}

	if path := findPointerField(t, ""); path != "" {
		panic(fmt.Sprintf(
			"tinycache: type %q contains pointer at %s — "+
				"this breaks the GC optimization. "+
				"All fields must be value types (int, float, bool, fixed-size arrays, etc.)",
			t.String(), path,
		))
	}
}

// findPointerField 递归检查类型 t 中是否存在指针类型字段
//
// 如果存在，返回字段路径（如 "Name" 或 "Inner.Tags"）；不存在返回 ""。
func findPointerField(t reflect.Type, prefix string) string {
	switch t.Kind() {

	// 显式指针类型
	case reflect.Ptr, reflect.Map, reflect.Slice,
		reflect.Chan, reflect.Func, reflect.Interface, reflect.UnsafePointer:
		if prefix == "" {
			return t.String() + " (root type is a pointer type)"
		}
		return prefix

	// string 底层含指针（StringHeader.Data）
	case reflect.String:
		if prefix == "" {
			return "string (root type contains pointer)"
		}
		return prefix

	// struct 需要递归检查每个字段
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)
			fieldPath := field.Name
			if prefix != "" {
				fieldPath = prefix + "." + field.Name
			}
			if result := findPointerField(field.Type, fieldPath); result != "" {
				return result
			}
		}
		return ""

	// 固定长度数组需要检查元素类型
	case reflect.Array:
		elemPath := prefix + "[]"
		if prefix == "" {
			elemPath = "[]"
		}
		return findPointerField(t.Elem(), elemPath)

	default:
		// bool, int*, uint*, float*, complex* 等基本值类型
		return ""
	}
}
