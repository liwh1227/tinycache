package core

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ============================================================
// 测试用结构体
// ============================================================

// CmpBigStruct 纯值类型（GC 友好 ✅）
type CmpBigStruct struct {
	ID        uint64
	Score     float64
	Level     uint32
	Status    uint8
	_         [3]byte
	Timestamp int64
	Extra     [64]byte
}

// CmpBigStructWithPtr 包含指针字段（GC 不友好 ❌）
type CmpBigStructWithPtr struct {
	ID        uint64
	Score     float64
	Level     uint32
	Status    uint8
	Timestamp int64
	Name      string // ← 指针
	Tags      string // ← 指针
}

// ============================================================
// 辅助函数
// ============================================================

const keyPrefix = "key:"

func makeKey(i int) string {
	return keyPrefix + strconv.Itoa(i)
}

// cleanHeap 尽可能彻底地清理堆
func cleanHeap() {
	for i := 0; i < 5; i++ {
		runtime.GC()
	}
	time.Sleep(50 * time.Millisecond)
	runtime.GC()
}

// ============================================================
// 测试 1：堆对象数量对比（绝对值测量）
//
// 修正要点：
//   1. 使用绝对 HeapObjects（非 before/after 差值）
//   2. 先测 NativeMap 并释放，再测 TinyCache，避免交叉污染
//   3. 用 strconv.Itoa 替代 fmt.Sprintf 减少临时堆分配
//   4. 填充数据后多次 GC，确保只剩存活对象再采样
//
// 预期：
//   NativeMap HeapObjects ≈ N（每个 *Struct 和每个 string 都是堆对象）
//   TinyCache HeapObjects ≪ N（只有 shard 等内部结构，data/index 不含指针）
// ============================================================

func TestCompare3_HeapObjects(t *testing.T) {
	for _, n := range []int{500_000, 1_000_000, 2_000_000} {
		t.Run(fmt.Sprintf("entries=%d", n), func(t *testing.T) {

			// ---- 先测 NativeMap ----
			cleanHeap()
			nativeMap := make(map[string]*CmpBigStructWithPtr, n)
			for i := 0; i < n; i++ {
				nativeMap[makeKey(i)] = &CmpBigStructWithPtr{
					ID:   uint64(i),
					Name: "user-" + strconv.Itoa(i),
					Tags: "tag-" + strconv.Itoa(i),
				}
			}
			for i := 0; i < 3; i++ {
				runtime.GC()
			}
			var nativeMS runtime.MemStats
			runtime.ReadMemStats(&nativeMS)
			nativeHeapObj := nativeMS.HeapObjects
			nativeHeapInuse := nativeMS.HeapInuse

			// 释放 NativeMap
			nativeMap = nil
			cleanHeap()

			// ---- 再测 TinyCache ----
			tc := New[CmpBigStruct](Config{ShardCount: 16})
			for i := 0; i < n; i++ {
				tc.Set(makeKey(i), CmpBigStruct{
					ID: uint64(i), Score: float64(i) * 0.1,
				}, 0)
			}
			for i := 0; i < 3; i++ {
				runtime.GC()
			}
			var tcMS runtime.MemStats
			runtime.ReadMemStats(&tcMS)
			tcHeapObj := tcMS.HeapObjects
			tcHeapInuse := tcMS.HeapInuse

			tc.Close()
			cleanHeap()

			// ---- 输出 ----
			t.Logf("")
			t.Logf("╔══════════════════════════════════════════════════════════════╗")
			t.Logf("║  Heap Objects (absolute)  —  %d entries                ║", n)
			t.Logf("╠════════════════════════════╦══════════════╦════════════════╣")
			t.Logf("║                            ║ HeapObjects  ║  Heap Inuse    ║")
			t.Logf("╠════════════════════════════╬══════════════╬════════════════╣")
			t.Logf("║  TinyCache                 ║ %10d   ║  %-12s  ║", tcHeapObj, fmtB(tcHeapInuse))
			t.Logf("║  NativeMap (ptr value)      ║ %10d   ║  %-12s  ║", nativeHeapObj, fmtB(nativeHeapInuse))
			t.Logf("╠════════════════════════════╩══════════════╩════════════════╣")

			if nativeHeapObj > tcHeapObj {
				t.Logf("║  ✅ NativeMap has %.1fx MORE heap objects than TinyCache   ║",
					float64(nativeHeapObj)/float64(tcHeapObj))
			} else {
				t.Logf("║  ⚠️  Unexpected: TinyCache ≥ NativeMap.                     ║")
			}
			t.Logf("╚══════════════════════════════════════════════════════════════╝")
		})
	}
}

// ============================================================
// 测试 2：GC 压力下的吞吐量
//
// 预加载大量数据 → 多线程持续读写 → 测量 ops/sec
// GC 并发标记通过 mark assist 抢占应用线程 CPU：
//   指针越多 → 标记工作量越大 → mark assist 越频繁 → 吞吐量越低
// ============================================================

func TestCompare3_ThroughputUnderGC(t *testing.T) {
	const (
		preload  = 2_000_000
		duration = 3 * time.Second
		workers  = 4
	)

	// ---- NativeMap ----
	cleanHeap()
	debug.SetGCPercent(100)

	var mu sync.RWMutex
	nativeMap := make(map[string]*CmpBigStructWithPtr, preload)
	for i := 0; i < preload; i++ {
		nativeMap[makeKey(i)] = &CmpBigStructWithPtr{
			ID: uint64(i), Name: "user-" + strconv.Itoa(i),
			Tags: "tag-" + strconv.Itoa(i),
		}
	}
	runtime.GC()

	nativeOps := measureThroughput(duration, workers, func(id, iter int) {
		key := makeKey(iter % preload)
		if iter%3 == 0 {
			mu.Lock()
			nativeMap[key] = &CmpBigStructWithPtr{ID: uint64(iter), Name: "up"}
			mu.Unlock()
		} else {
			mu.RLock()
			_ = nativeMap[key]
			mu.RUnlock()
		}
	})

	nativeMap = nil
	cleanHeap()

	// ---- TinyCache ----
	debug.SetGCPercent(100)

	tc := New[CmpBigStruct](Config{ShardCount: 16})
	for i := 0; i < preload; i++ {
		tc.Set(makeKey(i), CmpBigStruct{ID: uint64(i)}, 0)
	}
	runtime.GC()

	tcOps := measureThroughput(duration, workers, func(id, iter int) {
		key := makeKey(iter % preload)
		if iter%3 == 0 {
			tc.Set(key, CmpBigStruct{ID: uint64(iter)}, time.Minute)
		} else {
			tc.Get(key)
		}
	})

	tc.Close()

	t.Logf("")
	t.Logf("╔═══════════════════════════════════════════════════════════════╗")
	t.Logf("║  Throughput Under GC  (%dM entries, %d workers, %s)    ║", preload/1_000_000, workers, duration)
	t.Logf("╠══════════════════════════════╦════════════════════════════════╣")
	t.Logf("║  TinyCache                   ║  %12d ops/sec          ║", tcOps)
	t.Logf("║  NativeMap (ptr + RWMutex)   ║  %12d ops/sec          ║", nativeOps)
	t.Logf("╠══════════════════════════════╩════════════════════════════════╣")

	if tcOps > nativeOps && nativeOps > 0 {
		t.Logf("║  ✅ TinyCache is %.1fx faster                                ║",
			float64(tcOps)/float64(nativeOps))
	}
	t.Logf("╚═══════════════════════════════════════════════════════════════╝")
}

// ============================================================
// 测试 3：GC 工作量（GC 次数 + 累计 STW + 墙钟时间）
// ============================================================

func TestCompare3_GCWork(t *testing.T) {
	const n = 2_000_000

	// ---- NativeMap ----
	cleanHeap()
	debug.SetGCPercent(100)

	var msBefore runtime.MemStats
	runtime.ReadMemStats(&msBefore)
	startNative := time.Now()

	nativeMap := make(map[string]*CmpBigStructWithPtr, n)
	for i := 0; i < n; i++ {
		nativeMap[makeKey(i)] = &CmpBigStructWithPtr{
			ID: uint64(i), Name: "u" + strconv.Itoa(i), Tags: "t" + strconv.Itoa(i),
		}
	}
	for i := 0; i < 5; i++ {
		runtime.GC()
	}
	nativeDur := time.Since(startNative)

	var msAfterNative runtime.MemStats
	runtime.ReadMemStats(&msAfterNative)
	nativeGCCycles := msAfterNative.NumGC - msBefore.NumGC
	nativePauseTotal := time.Duration(msAfterNative.PauseTotalNs - msBefore.PauseTotalNs)

	nativeMap = nil
	cleanHeap()

	// ---- TinyCache ----
	var msBefore2 runtime.MemStats
	runtime.ReadMemStats(&msBefore2)
	startTC := time.Now()

	tc := New[CmpBigStruct](Config{ShardCount: 16})
	for i := 0; i < n; i++ {
		tc.Set(makeKey(i), CmpBigStruct{ID: uint64(i)}, 0)
	}
	for i := 0; i < 5; i++ {
		runtime.GC()
	}
	tcDur := time.Since(startTC)

	var msAfterTC runtime.MemStats
	runtime.ReadMemStats(&msAfterTC)
	tcGCCycles := msAfterTC.NumGC - msBefore2.NumGC
	tcPauseTotal := time.Duration(msAfterTC.PauseTotalNs - msBefore2.PauseTotalNs)

	tc.Close()

	t.Logf("")
	t.Logf("╔══════════════════════════════════════════════════════════════════╗")
	t.Logf("║  GC Work Comparison  (%d entries)                         ║", n)
	t.Logf("╠══════════════════════╦════════════╦═════════════╦═════════════╣")
	t.Logf("║                      ║ Wall Time  ║  GC Cycles  ║  STW Total  ║")
	t.Logf("╠══════════════════════╬════════════╬═════════════╬═════════════╣")
	t.Logf("║  TinyCache           ║ %-9s  ║  %-9d  ║  %-9s  ║", tcDur.Round(time.Millisecond), tcGCCycles, tcPauseTotal.Round(time.Microsecond))
	t.Logf("║  NativeMap (ptr)     ║ %-9s  ║  %-9d  ║  %-9s  ║", nativeDur.Round(time.Millisecond), nativeGCCycles, nativePauseTotal.Round(time.Microsecond))
	t.Logf("╚══════════════════════╩════════════╩═════════════╩═════════════╝")
}

// ============================================================
// Benchmark
// ============================================================

func BenchmarkCompare3_ThroughputUnderGC_TinyCache(b *testing.B) {
	const preload = 2_000_000
	tc := New[CmpBigStruct](Config{ShardCount: 16})
	for i := 0; i < preload; i++ {
		tc.Set(makeKey(i), CmpBigStruct{ID: uint64(i)}, 0)
	}
	defer tc.Close()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := makeKey(i % preload)
			if i%3 == 0 {
				tc.Set(key, CmpBigStruct{ID: uint64(i)}, time.Minute)
			} else {
				tc.Get(key)
			}
			i++
		}
	})
}

func BenchmarkCompare3_ThroughputUnderGC_NativeMapPtr(b *testing.B) {
	const preload = 2_000_000
	var mu sync.RWMutex
	m := make(map[string]*CmpBigStructWithPtr, preload)
	for i := 0; i < preload; i++ {
		m[makeKey(i)] = &CmpBigStructWithPtr{
			ID: uint64(i), Name: "u" + strconv.Itoa(i), Tags: "t" + strconv.Itoa(i),
		}
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := makeKey(i % preload)
			if i%3 == 0 {
				mu.Lock()
				m[key] = &CmpBigStructWithPtr{ID: uint64(i), Name: "up"}
				mu.Unlock()
			} else {
				mu.RLock()
				_ = m[key]
				mu.RUnlock()
			}
			i++
		}
	})
}

// ============================================================
// 辅助函数
// ============================================================

func measureThroughput(dur time.Duration, workers int, op func(workerID, iter int)) int64 {
	var total atomic.Int64
	var wg sync.WaitGroup
	deadline := time.Now().Add(dur)

	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(id int) {
			defer wg.Done()
			ops := 0
			for i := 0; time.Now().Before(deadline); i++ {
				op(id, i)
				ops++
			}
			total.Add(int64(ops))
		}(w)
	}
	wg.Wait()
	return total.Load() / int64(dur.Seconds())
}

func fmtB(b uint64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(b)/(1<<20))
	default:
		return fmt.Sprintf("%.1f KB", float64(b)/(1<<10))
	}
}
