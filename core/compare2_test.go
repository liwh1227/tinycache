package core

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ============================================================
// 测试用结构体
// ============================================================

// BigStruct 纯值类型，用于 TinyCache（GC 友好 ✅）
type BigStruct struct {
	ID        uint64
	Score     float64
	Level     uint32
	Status    uint8
	_         [3]byte
	Timestamp int64
	Extra     [64]byte
}

// BigStructWithPtr 包含指针字段，用于 native map 对比（GC 不友好 ❌）
type BigStructWithPtr struct {
	ID        uint64
	Score     float64
	Level     uint32
	Status    uint8
	Timestamp int64
	Name      string // ← 指针
	Tags      string // ← 指针
}

// ============================================================
// 测试 1：堆对象数量对比（最直接的证据）
//
// HeapObjects 直接反映 GC 需要追踪和扫描的对象数量。
// TinyCache 的核心优势：无论存多少条目，堆对象数保持在常数级。
// native map[string]*Struct：每存一条数据就多一个堆对象。
// ============================================================

func TestCompare_HeapObjects(t *testing.T) {
	for _, n := range []int{100_000, 500_000, 1_000_000} {
		t.Run(fmt.Sprintf("entries=%d", n), func(t *testing.T) {
			// ---------- TinyCache ----------
			var tcHeapObj uint64
			func() {
				runtime.GC()
				var before runtime.MemStats
				runtime.ReadMemStats(&before)

				c := New[BigStruct](Config{ShardCount: 16})
				for i := 0; i < n; i++ {
					c.Set(fmt.Sprintf("k:%d", i), BigStruct{ID: uint64(i)}, 0)
				}

				runtime.GC()
				var after runtime.MemStats
				runtime.ReadMemStats(&after)
				if after.HeapObjects > before.HeapObjects {
					tcHeapObj = after.HeapObjects - before.HeapObjects
				}
				_ = c.Len()
				c.Close()
			}()

			// ---------- Native map[string]*BigStructWithPtr ----------
			var nativeHeapObj uint64
			func() {
				runtime.GC()
				var before runtime.MemStats
				runtime.ReadMemStats(&before)

				m := make(map[string]*BigStructWithPtr, n)
				for i := 0; i < n; i++ {
					m[fmt.Sprintf("k:%d", i)] = &BigStructWithPtr{
						ID:   uint64(i),
						Name: fmt.Sprintf("user-%d", i),
						Tags: fmt.Sprintf("tag-%d", i),
					}
				}

				runtime.GC()
				var after runtime.MemStats
				runtime.ReadMemStats(&after)
				if after.HeapObjects > before.HeapObjects {
					nativeHeapObj = after.HeapObjects - before.HeapObjects
				}
				runtime.KeepAlive(m)
			}()

			t.Logf("")
			t.Logf("╔═══════════════════════════════════════════════════════╗")
			t.Logf("║  Heap Objects Comparison  (%d entries)          ║", n)
			t.Logf("╠═════════════════════════════╦═════════════════════════╣")
			t.Logf("║  TinyCache                  ║  %-22d ║", tcHeapObj)
			t.Logf("║  NativeMap (ptr value)       ║  %-22d ║", nativeHeapObj)
			t.Logf("╠═════════════════════════════╩═════════════════════════╣")

			if nativeHeapObj > tcHeapObj && tcHeapObj > 0 {
				t.Logf("║  ✅ TinyCache has %.0fx fewer heap objects            ║",
					float64(nativeHeapObj)/float64(tcHeapObj))
			}
			t.Logf("╚═══════════════════════════════════════════════════════╝")
		})
	}
}

// ============================================================
// 测试 2：GC CPU 占用对比（核心性能指标）
//
// GCCPUFraction 反映 GC 在并发标记阶段消耗的 CPU 比例。
// 这才是 "指针过多" 的真正代价 —— 不是 STW pause 变长，
// 而是 GC 并发标记阶段抢占了更多的应用 CPU 时间。
// ============================================================

func TestCompare_GCCPUFraction(t *testing.T) {
	for _, n := range []int{500_000, 1_000_000, 3_000_000} {
		t.Run(fmt.Sprintf("entries=%d", n), func(t *testing.T) {

			// ---------- TinyCache ----------
			tcFraction := measureGCCPU(func() {
				c := New[BigStruct](Config{ShardCount: 16})
				for i := 0; i < n; i++ {
					c.Set(fmt.Sprintf("k:%d", i), BigStruct{
						ID: uint64(i), Score: float64(i) * 0.1,
					}, 0)
				}
				forceGC(5)
				_ = c.Len()
				c.Close()
			})

			// ---------- Native map[string]*BigStructWithPtr ----------
			nativePtrFraction := measureGCCPU(func() {
				m := make(map[string]*BigStructWithPtr, n)
				for i := 0; i < n; i++ {
					m[fmt.Sprintf("k:%d", i)] = &BigStructWithPtr{
						ID: uint64(i), Score: float64(i) * 0.1,
						Name: fmt.Sprintf("user-%d", i),
						Tags: fmt.Sprintf("tag-%d", i),
					}
				}
				forceGC(5)
				_ = len(m)
			})

			t.Logf("")
			t.Logf("╔═══════════════════════════════════════════════════════╗")
			t.Logf("║  GC CPU Fraction  (%d entries)                 ║", n)
			t.Logf("╠═════════════════════════════╦═════════════════════════╣")
			t.Logf("║  TinyCache                  ║  %.4f%%               ║", tcFraction*100)
			t.Logf("║  NativeMap (ptr value)       ║  %.4f%%               ║", nativePtrFraction*100)
			t.Logf("╚═════════════════════════════╩═════════════════════════╝")

			if nativePtrFraction > 0 && tcFraction < nativePtrFraction {
				t.Logf("✅ TinyCache GC CPU is %.1fx lower", nativePtrFraction/tcFraction)
			}
		})
	}
}

// ============================================================
// 测试 3：GC 总工作量对比
//
// 对比 GC 次数、总 STW 暂停时间、堆内存使用量。
// 在 数据量足够大 的前提下，STW 暂停的累计差异会显现。
// ============================================================

func TestCompare_GCTotalWork(t *testing.T) {
	for _, n := range []int{1_000_000, 3_000_000} {
		t.Run(fmt.Sprintf("entries=%d", n), func(t *testing.T) {

			tcStats := measureGCWork(func() {
				c := New[BigStruct](Config{ShardCount: 16})
				for i := 0; i < n; i++ {
					c.Set(fmt.Sprintf("k:%d", i), BigStruct{ID: uint64(i)}, 0)
				}
				forceGC(3)
				_ = c.Len()
				c.Close()
			})

			nativeStats := measureGCWork(func() {
				m := make(map[string]*BigStructWithPtr, n)
				for i := 0; i < n; i++ {
					m[fmt.Sprintf("k:%d", i)] = &BigStructWithPtr{
						ID: uint64(i), Name: fmt.Sprintf("user-%d", i),
						Tags: fmt.Sprintf("tag-%d", i),
					}
				}
				forceGC(3)
				_ = len(m)
			})

			t.Logf("")
			t.Logf("╔════════════════════════════════════════════════════════════════╗")
			t.Logf("║  GC Total Work  (%d entries)                            ║", n)
			t.Logf("╠═══════════════════════╦═══════════════╦════════════════════════╣")
			t.Logf("║                       ║  GC Cycles    ║  Total STW Pause       ║")
			t.Logf("╠═══════════════════════╬═══════════════╬════════════════════════╣")
			t.Logf("║  TinyCache            ║  %-11d  ║  %-20s  ║", tcStats.numGC, tcStats.totalPause)
			t.Logf("║  NativeMap (ptr)      ║  %-11d  ║  %-20s  ║", nativeStats.numGC, nativeStats.totalPause)
			t.Logf("╠═══════════════════════╬═══════════════╬════════════════════════╣")
			t.Logf("║                       ║  Heap Inuse   ║  Heap Objects          ║")
			t.Logf("╠═══════════════════════╬═══════════════╬════════════════════════╣")
			t.Logf("║  TinyCache            ║  %-11s  ║  %-20d  ║", fmtBytes(tcStats.heapInuse), tcStats.heapObjects)
			t.Logf("║  NativeMap (ptr)      ║  %-11s  ║  %-20d  ║", fmtBytes(nativeStats.heapInuse), nativeStats.heapObjects)
			t.Logf("╚═══════════════════════╩═══════════════╩════════════════════════╝")
		})
	}
}

// ============================================================
// 测试 4：GC 压力下的吞吐量（最贴近生产环境的指标）
//
// 模拟真实场景：预加载大量数据 → 在持有数据的同时持续读写。
// GC 并发标记通过 "mark assist" 机制抢占工作线程 CPU：
//   当 goroutine 分配内存时，如果 GC 标记进度落后，
//   该 goroutine 会被迫暂停业务逻辑，先帮 GC 做一些标记工作。
//   指针越多 → 标记工作量越大 → mark assist 越频繁 → 吞吐量越低。
// ============================================================

func TestCompare_ThroughputUnderGC(t *testing.T) {
	const (
		preload  = 2_000_000
		duration = 3 * time.Second
		workers  = 4
	)

	// ---------- TinyCache ----------
	tcOps := func() int64 {
		runtime.GC()
		debug.SetGCPercent(100)

		c := New[BigStruct](Config{ShardCount: 16})
		for i := 0; i < preload; i++ {
			c.Set(fmt.Sprintf("k:%d", i), BigStruct{ID: uint64(i)}, 0)
		}

		return measureOps(duration, workers, func(id, iter int) {
			key := fmt.Sprintf("k:%d", iter%preload)
			if iter%3 == 0 {
				c.Set(key, BigStruct{ID: uint64(iter)}, time.Minute)
			} else {
				c.Get(key)
			}
		})
	}()

	// ---------- Native map + RWMutex ----------
	nativeOps := func() int64 {
		runtime.GC()
		debug.SetGCPercent(100)

		var mu sync.RWMutex
		m := make(map[string]*BigStructWithPtr, preload)
		for i := 0; i < preload; i++ {
			m[fmt.Sprintf("k:%d", i)] = &BigStructWithPtr{
				ID: uint64(i), Name: fmt.Sprintf("user-%d", i),
				Tags: fmt.Sprintf("tag-%d", i),
			}
		}

		return measureOps(duration, workers, func(id, iter int) {
			key := fmt.Sprintf("k:%d", iter%preload)
			if iter%3 == 0 {
				mu.Lock()
				m[key] = &BigStructWithPtr{ID: uint64(iter), Name: "updated"}
				mu.Unlock()
			} else {
				mu.RLock()
				_ = m[key]
				mu.RUnlock()
			}
		})
	}()

	t.Logf("")
	t.Logf("╔═══════════════════════════════════════════════════════════════╗")
	t.Logf("║  Throughput Under GC  (%dM entries, %d workers, %s)    ║", preload/1_000_000, workers, duration)
	t.Logf("╠══════════════════════════════╦════════════════════════════════╣")
	t.Logf("║  TinyCache                   ║  %12d ops/sec          ║", tcOps)
	t.Logf("║  NativeMap (ptr + RWMutex)   ║  %12d ops/sec          ║", nativeOps)
	t.Logf("╠══════════════════════════════╩════════════════════════════════╣")

	if tcOps > nativeOps && nativeOps > 0 {
		t.Logf("║  ✅ TinyCache throughput is %.1fx higher                      ║",
			float64(tcOps)/float64(nativeOps))
	}
	t.Logf("╚═══════════════════════════════════════════════════════════════╝")
}

// ============================================================
// 辅助函数
// ============================================================

func measureGCCPU(fn func()) float64 {
	runtime.GC()
	debug.SetGCPercent(100)

	fn()

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.GCCPUFraction
}

type gcStats struct {
	numGC       uint32
	totalPause  time.Duration
	heapInuse   uint64
	heapObjects uint64
}

func measureGCWork(fn func()) gcStats {
	runtime.GC()
	debug.SetGCPercent(100)
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	fn()

	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	return gcStats{
		numGC:       after.NumGC - before.NumGC,
		totalPause:  time.Duration(after.PauseTotalNs - before.PauseTotalNs),
		heapInuse:   after.HeapInuse,
		heapObjects: after.HeapObjects,
	}
}

func measureOps(dur time.Duration, workers int, op func(workerID, iter int)) int64 {
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

func forceGC(n int) {
	for i := 0; i < n; i++ {
		runtime.GC()
	}
}

func fmtBytes(b uint64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(b)/(1<<20))
	default:
		return fmt.Sprintf("%.1f KB", float64(b)/(1<<10))
	}
}
