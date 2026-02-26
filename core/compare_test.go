package core

import (
	"fmt"
	"math/rand"
	"runtime"
	"runtime/debug"
	"sync"
	"testing"
	"time"
)

// ============================================================================
//  对照组：原生 map + sync.RWMutex（包含指针，GC 需要全量扫描）
// ============================================================================

// NativeMapCache 模拟最常见的"朴素缓存"实现
//
// 两个 GC 痛点：
//   - map 的 key 是 string（底层含指针）
//   - map 的 value 是 *UserProfile（显式指针）
//
// 当条目数达到百万级时，GC 需要扫描每一个 key 和 value 指针。
type NativeMapCache struct {
	mu   sync.RWMutex
	data map[string]*UserProfile
}

func NewNativeMapCache(initCap int) *NativeMapCache {
	return &NativeMapCache{
		data: make(map[string]*UserProfile, initCap),
	}
}

func (c *NativeMapCache) Set(key string, val *UserProfile) {
	c.mu.Lock()
	c.data[key] = val
	c.mu.Unlock()
}

func (c *NativeMapCache) Get(key string) (*UserProfile, bool) {
	c.mu.RLock()
	v, ok := c.data[key]
	c.mu.RUnlock()
	return v, ok
}

func (c *NativeMapCache) Delete(key string) {
	c.mu.Lock()
	delete(c.data, key)
	c.mu.Unlock()
}

func (c *NativeMapCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.data)
}

// ============================================================================
//  对照组 B：map[string]UserProfile（值类型 value，但 key 仍是 string 指针）
// ============================================================================

// NativeMapValueCache 比指针版好一些，但 string key 仍然使 GC 需要扫描
type NativeMapValueCache struct {
	mu   sync.RWMutex
	data map[string]UserProfile
}

func NewNativeMapValueCache(initCap int) *NativeMapValueCache {
	return &NativeMapValueCache{
		data: make(map[string]UserProfile, initCap),
	}
}

func (c *NativeMapValueCache) Set(key string, val UserProfile) {
	c.mu.Lock()
	c.data[key] = val
	c.mu.Unlock()
}

func (c *NativeMapValueCache) Get(key string) (UserProfile, bool) {
	c.mu.RLock()
	v, ok := c.data[key]
	c.mu.RUnlock()
	return v, ok
}

func (c *NativeMapValueCache) Delete(key string) {
	c.mu.Lock()
	delete(c.data, key)
	c.mu.Unlock()
}

// ============================================================================
//  1. 单线程吞吐量对比
// ============================================================================

// --- Set ---

func BenchmarkCompare_Set_TinyCache(b *testing.B) {
	c := New[UserProfile](Config{ShardCount: 16})
	defer c.Close()
	profile := UserProfile{ID: 1, Score: 99.5, Level: 42, Status: 1}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.Set(fmt.Sprintf("user:%d", i), profile, time.Minute)
	}
}

func BenchmarkCompare_Set_NativeMapPtr(b *testing.B) {
	c := NewNativeMapCache(1024)
	profile := &UserProfile{ID: 1, Score: 99.5, Level: 42, Status: 1}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.Set(fmt.Sprintf("user:%d", i), profile)
	}
}

func BenchmarkCompare_Set_NativeMapValue(b *testing.B) {
	c := NewNativeMapValueCache(1024)
	profile := UserProfile{ID: 1, Score: 99.5, Level: 42, Status: 1}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.Set(fmt.Sprintf("user:%d", i), profile)
	}
}

// --- Get (预填充后读取) ---

func BenchmarkCompare_Get_TinyCache(b *testing.B) {
	c := New[UserProfile](Config{ShardCount: 16})
	defer c.Close()
	for i := 0; i < 10000; i++ {
		c.Set(fmt.Sprintf("user:%d", i), UserProfile{ID: uint64(i)}, 0)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.Get(fmt.Sprintf("user:%d", i%10000))
	}
}

func BenchmarkCompare_Get_NativeMapPtr(b *testing.B) {
	c := NewNativeMapCache(10000)
	for i := 0; i < 10000; i++ {
		c.Set(fmt.Sprintf("user:%d", i), &UserProfile{ID: uint64(i)})
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.Get(fmt.Sprintf("user:%d", i%10000))
	}
}

func BenchmarkCompare_Get_NativeMapValue(b *testing.B) {
	c := NewNativeMapValueCache(10000)
	for i := 0; i < 10000; i++ {
		c.Set(fmt.Sprintf("user:%d", i), UserProfile{ID: uint64(i)})
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.Get(fmt.Sprintf("user:%d", i%10000))
	}
}

// --- Delete ---

func BenchmarkCompare_Delete_TinyCache(b *testing.B) {
	c := New[UserProfile](Config{ShardCount: 16})
	defer c.Close()
	profile := UserProfile{ID: 1, Score: 99.5}
	// 每次迭代：写入 → 删除，测量删除 + freeList 回收的开销
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		key := fmt.Sprintf("user:%d", i)
		c.Set(key, profile, 0)
		c.Delete(key)
	}
}

func BenchmarkCompare_Delete_NativeMapPtr(b *testing.B) {
	c := NewNativeMapCache(1024)
	profile := &UserProfile{ID: 1, Score: 99.5}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		key := fmt.Sprintf("user:%d", i)
		c.Set(key, profile)
		c.Delete(key)
	}
}

// ============================================================================
//  2. 并发吞吐量对比（读多写少 7:3）
// ============================================================================

func BenchmarkCompare_Parallel_ReadHeavy_TinyCache(b *testing.B) {
	c := New[UserProfile](Config{ShardCount: 16})
	defer c.Close()
	profile := UserProfile{ID: 1, Score: 99.5}
	for i := 0; i < 5000; i++ {
		c.Set(fmt.Sprintf("user:%d", i), profile, 0)
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := fmt.Sprintf("user:%d", i%5000)
			if i%10 < 7 {
				c.Get(key)
			} else {
				c.Set(key, profile, time.Minute)
			}
			i++
		}
	})
}

func BenchmarkCompare_Parallel_ReadHeavy_NativeMapPtr(b *testing.B) {
	c := NewNativeMapCache(5000)
	profile := &UserProfile{ID: 1, Score: 99.5}
	for i := 0; i < 5000; i++ {
		c.Set(fmt.Sprintf("user:%d", i), profile)
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := fmt.Sprintf("user:%d", i%5000)
			if i%10 < 7 {
				c.Get(key)
			} else {
				c.Set(key, profile)
			}
			i++
		}
	})
}

// ============================================================================
//  3. 并发吞吐量对比（写多读少 3:7）
// ============================================================================

func BenchmarkCompare_Parallel_WriteHeavy_TinyCache(b *testing.B) {
	c := New[UserProfile](Config{ShardCount: 16})
	defer c.Close()
	profile := UserProfile{ID: 1, Score: 99.5}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := fmt.Sprintf("user:%d", i%5000)
			if i%10 < 3 {
				c.Get(key)
			} else {
				c.Set(key, profile, time.Minute)
			}
			i++
		}
	})
}

func BenchmarkCompare_Parallel_WriteHeavy_NativeMapPtr(b *testing.B) {
	c := NewNativeMapCache(5000)
	profile := &UserProfile{ID: 1, Score: 99.5}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := fmt.Sprintf("user:%d", i%5000)
			if i%10 < 3 {
				c.Get(key)
			} else {
				c.Set(key, profile)
			}
			i++
		}
	})
}

// ============================================================================
//  4. GC 暂停时间对比（核心指标！）
//
//  测试方法：
//    1. 向缓存中写入 N 个条目
//    2. 手动触发 GC
//    3. 测量 GC 暂停时间
//
//  预期结果：
//    - NativeMap: GC 暂停时间随条目数线性增长（需扫描所有指针）
//    - TinyCache: GC 暂停时间几乎不变（无指针需要扫描）
// ============================================================================

func TestCompare_GCPause(t *testing.T) {
	// 多组规模测试，观察 GC 暂停时间的增长趋势
	for _, count := range []int{100_000, 500_000, 1_000_000} {
		t.Run(fmt.Sprintf("entries=%d", count), func(t *testing.T) {

			// ---------- 测试 TinyCache ----------
			tc := New[UserProfile](Config{ShardCount: 64})
			for i := 0; i < count; i++ {
				tc.Set(fmt.Sprintf("k:%d", i), UserProfile{
					ID:        uint64(i),
					Score:     float64(i) * 1.1,
					Level:     uint16(i % 100),
					Status:    uint8(i % 5),
					Timestamp: int64(i),
				}, 0)
			}
			tcPause := measureGCPause()
			tc.Close()
			// 释放 TinyCache 内存，避免影响下一轮测试
			tc = nil
			runtime.GC()

			// ---------- 测试 NativeMap (指针版) ----------
			nm := NewNativeMapCache(count)
			for i := 0; i < count; i++ {
				nm.Set(fmt.Sprintf("k:%d", i), &UserProfile{
					ID:        uint64(i),
					Score:     float64(i) * 1.1,
					Level:     uint16(i % 100),
					Status:    uint8(i % 5),
					Timestamp: int64(i),
				})
			}
			nmPause := measureGCPause()
			// 释放
			nm = nil
			runtime.GC()

			// ---------- 测试 NativeMap (值类型版) ----------
			nv := NewNativeMapValueCache(count)
			for i := 0; i < count; i++ {
				nv.Set(fmt.Sprintf("k:%d", i), UserProfile{
					ID:        uint64(i),
					Score:     float64(i) * 1.1,
					Level:     uint16(i % 100),
					Status:    uint8(i % 5),
					Timestamp: int64(i),
				})
			}
			nvPause := measureGCPause()
			nv = nil
			runtime.GC()

			// ---------- 输出对比结果 ----------
			t.Logf("╔══════════════════════════════════════════════════════╗")
			t.Logf("║  GC Pause Comparison  (%d entries)                  ║", count)
			t.Logf("╠════════════════════════════╦═════════════════════════╣")
			t.Logf("║  TinyCache                 ║  %-22s ║", tcPause)
			t.Logf("║  NativeMap (ptr value)      ║  %-22s ║", nmPause)
			t.Logf("║  NativeMap (struct value)   ║  %-22s ║", nvPause)
			t.Logf("╚════════════════════════════╩═════════════════════════╝")

			// TinyCache 的 GC 暂停时间应该显著低于原生 map
			if tcPause > nmPause {
				t.Logf("WARNING: TinyCache GC pause (%s) > NativeMap (%s), "+
					"may be affected by test environment or small dataset", tcPause, nmPause)
			}
		})
	}
}

// measureGCPause 触发一次完整 GC 并返回最近一次 GC 的暂停时间
func measureGCPause() time.Duration {
	// 先关闭自动 GC，确保测量的是我们主动触发的那次
	debug.SetGCPercent(-1)
	defer debug.SetGCPercent(100)

	// 清理上一轮残留
	runtime.GC()
	runtime.GC()

	// 记录 GC 前的统计信息
	var statsBefore debug.GCStats
	debug.ReadGCStats(&statsBefore)
	numBefore := statsBefore.NumGC

	// 触发测量目标 GC
	runtime.GC()

	// 读取 GC 后的统计信息
	var statsAfter debug.GCStats
	debug.ReadGCStats(&statsAfter)

	// 提取本次 GC 的暂停时间
	// debug.GCStats.Pause 是一个按时间倒序排列的暂停时间切片，
	// Pause[0] 是最近一次 GC 的暂停时间
	if statsAfter.NumGC > numBefore && len(statsAfter.Pause) > 0 {
		return statsAfter.Pause[0]
	}

	return 0
}

// ============================================================================
//  5. 内存分配对比
//
//  通过 b.ReportAllocs() 观察每次操作的堆分配次数和字节数
//  TinyCache 的 Set 在 freeList 有空闲槽位时应该是零堆分配（除 fmt.Sprintf 外）
// ============================================================================

func BenchmarkCompare_Alloc_Set_TinyCache(b *testing.B) {
	c := New[UserProfile](Config{ShardCount: 16})
	defer c.Close()
	profile := UserProfile{ID: 1, Score: 99.5}

	// 预填充 + 删除，确保 freeList 有大量空闲槽位
	for i := 0; i < 10000; i++ {
		c.Set(fmt.Sprintf("pre:%d", i), profile, 0)
	}
	for i := 0; i < 10000; i++ {
		c.Delete(fmt.Sprintf("pre:%d", i))
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.Set(fmt.Sprintf("k:%d", i%10000), profile, 0)
	}
}

func BenchmarkCompare_Alloc_Set_NativeMapPtr(b *testing.B) {
	c := NewNativeMapCache(10000)
	profile := &UserProfile{ID: 1, Score: 99.5}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.Set(fmt.Sprintf("k:%d", i%10000), profile)
	}
}

func BenchmarkCompare_Alloc_Get_TinyCache(b *testing.B) {
	c := New[UserProfile](Config{ShardCount: 16})
	defer c.Close()
	for i := 0; i < 10000; i++ {
		c.Set(fmt.Sprintf("k:%d", i), UserProfile{ID: uint64(i)}, 0)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.Get(fmt.Sprintf("k:%d", i%10000))
	}
}

func BenchmarkCompare_Alloc_Get_NativeMapPtr(b *testing.B) {
	c := NewNativeMapCache(10000)
	for i := 0; i < 10000; i++ {
		c.Set(fmt.Sprintf("k:%d", i), &UserProfile{ID: uint64(i)})
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.Get(fmt.Sprintf("k:%d", i%10000))
	}
}

// ============================================================================
//  6. FreeList 复用 vs 原生 map 反复增删
//
//  模拟真实场景：不断写入新 key、删除旧 key（如会话缓存、临时令牌）
//  TinyCache 通过 freeList 复用槽位，data slice 不会持续增长
//  NativeMap 反复 delete 后 bucket 只增不减
// ============================================================================

func BenchmarkCompare_ChurnWorkload_TinyCache(b *testing.B) {
	c := New[UserProfile](Config{ShardCount: 16})
	defer c.Close()
	profile := UserProfile{ID: 1, Score: 99.5}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		key := fmt.Sprintf("session:%d", i)
		c.Set(key, profile, time.Minute)
		// 模拟：新写入后删除较早的条目
		if i >= 100 {
			c.Delete(fmt.Sprintf("session:%d", i-100))
		}
	}
}

func BenchmarkCompare_ChurnWorkload_NativeMapPtr(b *testing.B) {
	c := NewNativeMapCache(1024)
	profile := &UserProfile{ID: 1, Score: 99.5}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		key := fmt.Sprintf("session:%d", i)
		c.Set(key, profile)
		if i >= 100 {
			c.Delete(fmt.Sprintf("session:%d", i-100))
		}
	}
}

// ============================================================================
//  7. 大规模数据下的并发读取性能（缓存命中率 100%）
//
//  预填充大量数据后，纯并发读取性能对比。
//  TinyCache 需要额外的 hash 计算 + shard 路由，但锁粒度小（16 shard）
//  NativeMap 是直接 map[string] 查找，但全局单一 RLock
// ============================================================================

func BenchmarkCompare_BulkRead_TinyCache(b *testing.B) {
	const dataSize = 100_000
	c := New[UserProfile](Config{ShardCount: 16})
	defer c.Close()

	keys := make([]string, dataSize)
	for i := 0; i < dataSize; i++ {
		keys[i] = fmt.Sprintf("user:%d", i)
		c.Set(keys[i], UserProfile{ID: uint64(i)}, 0)
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		r := rand.New(rand.NewSource(time.Now().UnixNano()))
		for pb.Next() {
			c.Get(keys[r.Intn(dataSize)])
		}
	})
}

func BenchmarkCompare_BulkRead_NativeMapPtr(b *testing.B) {
	const dataSize = 100_000
	c := NewNativeMapCache(dataSize)

	keys := make([]string, dataSize)
	for i := 0; i < dataSize; i++ {
		keys[i] = fmt.Sprintf("user:%d", i)
		c.Set(keys[i], &UserProfile{ID: uint64(i)})
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		r := rand.New(rand.NewSource(time.Now().UnixNano()))
		for pb.Next() {
			c.Get(keys[r.Intn(dataSize)])
		}
	})
}

// ============================================================================
//
//  8. GC 期间的吞吐量影响（最能体现 TinyCache 价值的场景）
//
//     方法：在大量数据存在的背景下，边做读写边触发 GC，
//     测量"有 GC 干扰时"的操作耗时。
//
//     预期：
//     - NativeMap: 每次 GC 都要扫描 50 万个指针，ops 显著下降
//     - TinyCache: GC 几乎不扫描缓存数据，ops 不受影响
//
// ============================================================================
// BenchmarkCompare_ThroughputUnderGC_TinyCache-8   	 2529865	       499.4 ns/op
func BenchmarkCompare_ThroughputUnderGC_TinyCache(b *testing.B) {
	const bgEntries = 500_000
	c := New[UserProfile](Config{ShardCount: 32})
	defer c.Close()

	profile := UserProfile{ID: 1, Score: 99.5}
	// 背景数据：50 万条常驻缓存
	for i := 0; i < bgEntries; i++ {
		c.Set(fmt.Sprintf("bg:%d", i), profile, 0)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		key := fmt.Sprintf("hot:%d", i%1000)
		c.Set(key, profile, time.Minute)
		c.Get(key)

		// 每 1000 次操作触发一次 GC，模拟 GC 频繁的场景
		if i%1000 == 0 {
			runtime.GC()
		}
	}
}

// BenchmarkCompare_ThroughputUnderGC_NativeMapPtr-8   	  126810	      9523 ns/op
func BenchmarkCompare_ThroughputUnderGC_NativeMapPtr(b *testing.B) {
	const bgEntries = 500_000
	c := NewNativeMapCache(bgEntries)

	profile := &UserProfile{ID: 1, Score: 99.5}
	for i := 0; i < bgEntries; i++ {
		c.Set(fmt.Sprintf("bg:%d", i), profile)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		key := fmt.Sprintf("hot:%d", i%1000)
		c.Set(key, profile)
		c.Get(key)

		if i%1000 == 0 {
			runtime.GC()
		}
	}
}

// ============================================================================
//  9. 内存占用快照对比
//
//  不是 Benchmark，而是 Test。
//  输出写入 N 条数据后的堆内存占用，直观感受内存效率差异。
// ============================================================================

func TestCompare_MemoryFootprint(t *testing.T) {
	const count = 200_000

	// ---------- TinyCache ----------
	runtime.GC()
	var m1 runtime.MemStats
	runtime.ReadMemStats(&m1)

	tc := New[UserProfile](Config{ShardCount: 16})
	for i := 0; i < count; i++ {
		tc.Set(fmt.Sprintf("k:%d", i), UserProfile{ID: uint64(i), Score: float64(i)}, 0)
	}

	runtime.GC()
	var m2 runtime.MemStats
	runtime.ReadMemStats(&m2)
	tcMem := m2.HeapInuse - m1.HeapInuse
	tc.Close()
	tc = nil

	// ---------- NativeMap (ptr) ----------
	runtime.GC()
	runtime.ReadMemStats(&m1)

	nm := NewNativeMapCache(count)
	for i := 0; i < count; i++ {
		nm.Set(fmt.Sprintf("k:%d", i), &UserProfile{ID: uint64(i), Score: float64(i)})
	}

	runtime.GC()
	runtime.ReadMemStats(&m2)
	nmMem := m2.HeapInuse - m1.HeapInuse
	nm = nil

	// ---------- NativeMap (value) ----------
	runtime.GC()
	runtime.ReadMemStats(&m1)

	nv := NewNativeMapValueCache(count)
	for i := 0; i < count; i++ {
		nv.Set(fmt.Sprintf("k:%d", i), UserProfile{ID: uint64(i), Score: float64(i)})
	}

	runtime.GC()
	runtime.ReadMemStats(&m2)
	nvMem := m2.HeapInuse - m1.HeapInuse
	nv = nil

	// ---------- 输出 ----------
	t.Logf("╔══════════════════════════════════════════════════════════╗")
	t.Logf("║  Memory Footprint Comparison  (%d entries)             ║", count)
	t.Logf("╠════════════════════════════╦═════════════════════════════╣")
	t.Logf("║  TinyCache                 ║  %-26s ║", formatBytes(tcMem))
	t.Logf("║  NativeMap (ptr value)      ║  %-26s ║", formatBytes(nmMem))
	t.Logf("║  NativeMap (struct value)   ║  %-26s ║", formatBytes(nvMem))
	t.Logf("╚════════════════════════════╩═════════════════════════════╝")
}

func formatBytes(b uint64) string {
	const (
		KB = 1024
		MB = 1024 * KB
	)
	switch {
	case b >= MB:
		return fmt.Sprintf("%.2f MB", float64(b)/float64(MB))
	case b >= KB:
		return fmt.Sprintf("%.2f KB", float64(b)/float64(KB))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
