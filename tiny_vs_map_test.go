package tinycache

//
//import (
//	"fmt"
//	"math/rand"
//	"sync"
//	"testing"
//)
//
//// ==========================================
//// 辅助准备：预生成 Key，彻底消除 Sprintf 的干扰
//// ==========================================
//
//var pregenKeys []string
//
//func init() {
//	// 在测试运行前，提前在内存中生成好 100 万个字符串 Key
//	// 这样在 benchmark 循环中就不需要调用 fmt.Sprintf，实现纯粹的 0 alloc 压测
//	pregenKeys = make([]string, MaxKeys)
//	for i := 0; i < MaxKeys; i++ {
//		pregenKeys[i] = fmt.Sprintf("user:%d", i)
//	}
//}
//
//// ==========================================
//// 原生 Go Map 基准测试 (Baseline)
//// ==========================================
//
//// NativeMapCache 模拟最朴素的并发安全原生 Map
//type NativeMapCache struct {
//	mu   sync.RWMutex
//	data map[string]UserProfile
//}
//
//func NewNativeMapCache() *NativeMapCache {
//	return &NativeMapCache{
//		data: make(map[string]UserProfile, MaxKeys),
//	}
//}
//
//func BenchmarkGoMap_Set(b *testing.B) {
//	cache := NewNativeMapCache()
//	profile := UserProfile{ID: 1001, Score: 99.5, Level: 42, Flags: 1}
//
//	b.ResetTimer()
//	b.ReportAllocs()
//
//	for i := 0; i < b.N; i++ {
//		key := pregenKeys[i%MaxKeys]
//		cache.mu.Lock()
//		cache.data[key] = profile
//		cache.mu.Unlock()
//	}
//}
//
//func BenchmarkGoMap_Get(b *testing.B) {
//	cache := NewNativeMapCache()
//	profile := UserProfile{ID: 1001, Score: 99.5, Level: 42, Flags: 1}
//
//	// 预热数据
//	for i := 0; i < MaxKeys; i++ {
//		key := pregenKeys[i]
//		cache.data[key] = profile
//	}
//
//	b.ResetTimer()
//	b.ReportAllocs()
//
//	for i := 0; i < b.N; i++ {
//		key := pregenKeys[i%MaxKeys]
//		cache.mu.RLock()
//		_ = cache.data[key]
//		cache.mu.RUnlock()
//	}
//}
//
//func BenchmarkGoMap_SetParallel(b *testing.B) {
//	cache := NewNativeMapCache()
//	profile := UserProfile{ID: 1001, Score: 99.5, Level: 42, Flags: 1}
//
//	b.ResetTimer()
//	b.ReportAllocs()
//
//	b.RunParallel(func(pb *testing.PB) {
//		for pb.Next() {
//			key := pregenKeys[rand.Intn(MaxKeys)]
//			cache.mu.Lock()
//			cache.data[key] = profile
//			cache.mu.Unlock()
//		}
//	})
//}
//
//func BenchmarkGoMap_GetParallel(b *testing.B) {
//	cache := NewNativeMapCache()
//	profile := UserProfile{ID: 1001, Score: 99.5, Level: 42, Flags: 1}
//
//	// 预热数据
//	for i := 0; i < MaxKeys; i++ {
//		key := pregenKeys[i]
//		cache.data[key] = profile
//	}
//
//	b.ResetTimer()
//	b.ReportAllocs()
//
//	b.RunParallel(func(pb *testing.PB) {
//		for pb.Next() {
//			key := pregenKeys[rand.Intn(MaxKeys)]
//			cache.mu.RLock()
//			_ = cache.data[key]
//			cache.mu.RUnlock()
//		}
//	})
//}
//
//// ==========================================
//// 纯净版 TinyCache 基准测试 (修复了 Sprintf 分配)
//// ==========================================
//
///* // 同样取消注释以运行
//func BenchmarkTinyCache_Set_Pure(b *testing.B) {
//	cache := tinycache.New[UserProfile](1024)
//	profile := UserProfile{ID: 1001, Score: 99.5, Level: 42, Flags: 1}
//
//	b.ResetTimer()
//	b.ReportAllocs()
//
//	for i := 0; i < b.N; i++ {
//		key := pregenKeys[i%MaxKeys]
//		cache.Set(key, profile, TTL)
//	}
//}
//
//func BenchmarkTinyCache_GetParallel_Pure(b *testing.B) {
//	cache := tinycache.New[UserProfile](1024)
//	profile := UserProfile{ID: 1001, Score: 99.5, Level: 42, Flags: 1}
//
//	for i := 0; i < MaxKeys; i++ {
//		key := pregenKeys[i]
//		cache.Set(key, profile, TTL)
//	}
//
//	b.ResetTimer()
//	b.ReportAllocs()
//
//	b.RunParallel(func(pb *testing.PB) {
//		for pb.Next() {
//			key := pregenKeys[rand.Intn(MaxKeys)]
//			_, _ = cache.Get(key)
//		}
//	})
//}
//*/
