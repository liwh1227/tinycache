package tinycache

import (
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"math"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"
)

// ============================================================
// 辅助准备：预生成 Key，彻底消除 Benchmark 循环中的 Sprintf/Itoa 分配
// ==========================================
var pregenBenchKeys []string

const maxPregenKeys = 500000 // 足够覆盖绝大多数压测的键空间

func init() {
	pregenBenchKeys = make([]string, maxPregenKeys)
	for i := 0; i < maxPregenKeys; i++ {
		pregenBenchKeys[i] = "key:" + strconv.Itoa(i)
	}
}

// ============================================================
// BigCache 核心特性模拟
//
// 目标：隔离并量化 BigCache 与 TinyCache 之间核心设计差异的
// 性能影响，而非做两个库的整体竞赛。
// ============================================================

// --- entry 布局 ---
// [hash uint64 | keyLen uint16 | valLen uint32 | key bytes | value bytes]
const entryHeaderSize = 8 + 2 + 4 // 14 bytes

// bigcacheShard 模拟 BigCache 的单个 shard
type bigcacheShard struct {
	mu    sync.RWMutex
	index map[uint64]uint32 // hash → queue 偏移量
	queue []byte            // BytesQueue: FIFO 追加写
}

// bigcacheSim 带分片的 BigCache 模拟
type bigcacheSim struct {
	shards    []*bigcacheShard
	shardMask uint64
}

func newBigcacheSim(shardCount, initCap int) *bigcacheSim {
	bc := &bigcacheSim{
		shards:    make([]*bigcacheShard, shardCount),
		shardMask: uint64(shardCount - 1),
	}
	for i := range bc.shards {
		bc.shards[i] = &bigcacheShard{
			index: make(map[uint64]uint32, initCap/shardCount),
			queue: make([]byte, 0, (initCap/shardCount)*128),
		}
	}
	return bc
}

// bigcacheHash 模拟 BigCache 默认的哈希方式（标准库 fnv）
func bigcacheHash(key string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(key))
	return h.Sum64()
}

func (bc *bigcacheSim) getShard(hash uint64) *bigcacheShard {
	return bc.shards[hash&bc.shardMask]
}

func (bc *bigcacheSim) set(key string, value []byte) {
	hash := bigcacheHash(key)
	s := bc.getShard(hash)

	s.mu.Lock()
	defer s.mu.Unlock()

	offset := uint32(len(s.queue))

	var hdr [entryHeaderSize]byte
	binary.LittleEndian.PutUint64(hdr[0:8], hash)
	binary.LittleEndian.PutUint16(hdr[8:10], uint16(len(key)))
	binary.LittleEndian.PutUint32(hdr[10:14], uint32(len(value)))
	s.queue = append(s.queue, hdr[:]...)
	s.queue = append(s.queue, key...)
	s.queue = append(s.queue, value...)

	s.index[hash] = offset
}

func (bc *bigcacheSim) get(key string) ([]byte, bool) {
	hash := bigcacheHash(key)
	s := bc.getShard(hash)

	s.mu.RLock()
	defer s.mu.RUnlock()

	offset, ok := s.index[hash]
	if !ok {
		return nil, false
	}

	pos := int(offset)
	if pos+entryHeaderSize > len(s.queue) {
		return nil, false
	}

	keyLen := int(binary.LittleEndian.Uint16(s.queue[pos+8 : pos+10]))
	valLen := int(binary.LittleEndian.Uint32(s.queue[pos+10 : pos+14]))

	keyStart := pos + entryHeaderSize
	valStart := keyStart + keyLen

	if string(s.queue[keyStart:keyStart+keyLen]) != key {
		return nil, false
	}

	if valStart+valLen > len(s.queue) {
		return nil, false
	}

	return s.queue[valStart : valStart+valLen], true
}

func (bc *bigcacheSim) del(key string) {
	hash := bigcacheHash(key)
	s := bc.getShard(hash)

	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.index, hash)
}

func (bc *bigcacheSim) totalQueueLen() int {
	total := 0
	for _, s := range bc.shards {
		s.mu.RLock()
		total += len(s.queue)
		s.mu.RUnlock()
	}
	return total
}

// ============================================================
// 序列化辅助：模拟 BigCache 用户必须执行的 encode/decode
// ============================================================

// BenchStruct 测试用结构体（96 字节，纯值类型）
type BenchStruct struct {
	ID        uint64
	Score     float64
	Level     uint32
	Status    uint8
	_         [3]byte
	Timestamp int64
	Extra     [64]byte
}

// 修复点：弃用 binary.Write 反射，手写硬编码序列化以达到性能极限
func encodeBenchStruct(s *BenchStruct) []byte {
	b := make([]byte, 96)
	binary.LittleEndian.PutUint64(b[0:8], s.ID)
	binary.LittleEndian.PutUint64(b[8:16], math.Float64bits(s.Score))
	binary.LittleEndian.PutUint32(b[16:20], s.Level)
	b[20] = s.Status
	// padding 21-23 忽略
	binary.LittleEndian.PutUint64(b[24:32], uint64(s.Timestamp))
	copy(b[32:96], s.Extra[:])
	return b
}

func decodeBenchStruct(data []byte) BenchStruct {
	var s BenchStruct
	if len(data) < 96 {
		return s
	}
	s.ID = binary.LittleEndian.Uint64(data[0:8])
	s.Score = math.Float64frombits(binary.LittleEndian.Uint64(data[8:16]))
	s.Level = binary.LittleEndian.Uint32(data[16:20])
	s.Status = data[20]
	s.Timestamp = int64(binary.LittleEndian.Uint64(data[24:32]))
	copy(s.Extra[:], data[32:96])
	return s
}

// ============================================================
// Benchmark 1: 哈希函数 — 标准库 fnv vs 内联 FNV-1a
// ============================================================

func BenchmarkHash_StdlibFNV(b *testing.B) {
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		bigcacheHash(pregenBenchKeys[i%maxPregenKeys])
	}
}

func BenchmarkHash_InlineFNV(b *testing.B) {
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		// 假设 tinycache 包中存在 fnvHash，如果没有，请替换为内部实际调用的哈希函数
		fnvHash(pregenBenchKeys[i%maxPregenKeys])
	}
}

// ============================================================
// Benchmark 2: Set 操作 — 序列化+追加 vs 直接赋值
// ============================================================

func BenchmarkSet_BigCacheSim(b *testing.B) {
	const keySpace = 50000
	cache := newBigcacheSim(16, keySpace)
	s := BenchStruct{ID: 42, Score: 99.5, Level: 10, Timestamp: time.Now().UnixNano()}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		key := pregenBenchKeys[i%keySpace]
		val := encodeBenchStruct(&s)
		cache.set(key, val)
	}
}

func BenchmarkSet_TinyCache(b *testing.B) {
	const keySpace = 50000
	cache := New[BenchStruct](Config{ShardCount: 16, InitShardSize: keySpace / 16})
	s := BenchStruct{ID: 42, Score: 99.5, Level: 10, Timestamp: time.Now().UnixNano()}
	defer cache.Close()

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		key := pregenBenchKeys[i%keySpace]
		cache.Set(key, s, 0)
	}
}

// ============================================================
// Benchmark 3: Get 操作 — 反序列化 vs 直接返回
// ============================================================

func BenchmarkGet_BigCacheSim(b *testing.B) {
	const n = 50000
	cache := newBigcacheSim(16, n)
	s := BenchStruct{ID: 42, Score: 99.5}
	for i := 0; i < n; i++ {
		cache.set(pregenBenchKeys[i], encodeBenchStruct(&s))
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		key := pregenBenchKeys[i%n]
		data, ok := cache.get(key)
		if ok {
			_ = decodeBenchStruct(data)
		}
	}
}

func BenchmarkGet_TinyCache(b *testing.B) {
	const n = 50000
	cache := New[BenchStruct](Config{ShardCount: 16})
	s := BenchStruct{ID: 42, Score: 99.5}
	for i := 0; i < n; i++ {
		cache.Set(pregenBenchKeys[i], s, 0)
	}
	defer cache.Close()

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		key := pregenBenchKeys[i%n]
		cache.Get(key)
	}
}

// ============================================================
// Benchmark 4: 热点 Key 更新 — FIFO 追加 vs 原地覆盖
// ============================================================

func BenchmarkUpdateHotKeys_BigCacheSim(b *testing.B) {
	const hotKeys = 1000
	cache := newBigcacheSim(16, hotKeys)
	s := BenchStruct{ID: 1, Score: 1.0}

	for i := 0; i < hotKeys; i++ {
		cache.set(pregenBenchKeys[i], encodeBenchStruct(&s))
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s.ID = uint64(i)
		key := pregenBenchKeys[i%hotKeys]
		cache.set(key, encodeBenchStruct(&s))
	}
}

func BenchmarkUpdateHotKeys_TinyCache(b *testing.B) {
	const hotKeys = 1000
	cache := New[BenchStruct](Config{ShardCount: 16})
	s := BenchStruct{ID: 1, Score: 1.0}
	defer cache.Close()

	for i := 0; i < hotKeys; i++ {
		cache.Set(pregenBenchKeys[i], s, 0)
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s.ID = uint64(i)
		key := pregenBenchKeys[i%hotKeys]
		cache.Set(key, s, 0)
	}
}

// ============================================================
// Benchmark 6: 并发混合读写 — 预加载后多 goroutine 并发
// ============================================================

func BenchmarkConcurrentMixed_BigCacheSim(b *testing.B) {
	const preload = 100_000
	cache := newBigcacheSim(16, preload)
	s := BenchStruct{ID: 42, Score: 99.5}
	for i := 0; i < preload; i++ {
		cache.set(pregenBenchKeys[i], encodeBenchStruct(&s))
	}

	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := pregenBenchKeys[i%preload]
			if i%5 == 0 { // 20% 写
				s.ID = uint64(i)
				cache.set(key, encodeBenchStruct(&s))
			} else { // 80% 读
				data, ok := cache.get(key)
				if ok {
					_ = decodeBenchStruct(data)
				}
			}
			i++
		}
	})
}

func BenchmarkConcurrentMixed_TinyCache(b *testing.B) {
	const preload = 100_000
	cache := New[BenchStruct](Config{ShardCount: 16})
	s := BenchStruct{ID: 42, Score: 99.5}
	for i := 0; i < preload; i++ {
		cache.Set(pregenBenchKeys[i], s, 0)
	}
	defer cache.Close()

	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := pregenBenchKeys[i%preload]
			if i%5 == 0 {
				s.ID = uint64(i)
				cache.Set(key, s, 0)
			} else {
				cache.Get(key)
			}
			i++
		}
	})
}

// ============================================================
// 内存统计辅助函数
// ============================================================

func cleanHeap() {
	runtime.GC()
	runtime.GC()
	time.Sleep(10 * time.Millisecond)
}

func fmtB(bytes uint64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

// ============================================================
// Tests: 功能对比与内存增长验证
// ============================================================

func TestCompare_HashAllocs(t *testing.T) {
	key := "user:profile:12345"

	stdAllocs := testing.AllocsPerRun(1000, func() {
		bigcacheHash(key)
	})

	inlineAllocs := testing.AllocsPerRun(1000, func() {
		fnvHash(key)
	})

	t.Logf("")
	t.Logf("┌─────────────────────────────────────────────────────┐")
	t.Logf("│  哈希函数堆分配对比                                 │")
	t.Logf("├──────────────────────────┬──────────────────────────┤")
	t.Logf("│  hash/fnv (BigCache 默认)│  %.0f allocs/op            │", stdAllocs)
	t.Logf("│  inline FNV (TinyCache)  │  %.0f allocs/op            │", inlineAllocs)
	t.Logf("└──────────────────────────┴──────────────────────────┘")
}

func TestCompare_SetAllocs(t *testing.T) {
	s := BenchStruct{ID: 42, Score: 99.5}

	bc := newBigcacheSim(16, 10000)
	for i := 0; i < 5000; i++ {
		bc.set("pre:"+strconv.Itoa(i), encodeBenchStruct(&s))
	}

	bcAllocs := testing.AllocsPerRun(1000, func() {
		s.ID++
		bc.set("pre:0", encodeBenchStruct(&s)) // 恒定 key，无拼接开销
	})

	tc := New[BenchStruct](Config{ShardCount: 16})
	defer tc.Close()
	for i := 0; i < 5000; i++ {
		tc.Set("pre:"+strconv.Itoa(i), s, 0)
	}

	tcAllocs := testing.AllocsPerRun(1000, func() {
		s.ID++
		tc.Set("pre:0", s, 0)
	})

	t.Logf("")
	t.Logf("┌─────────────────────────────────────────────────────┐")
	t.Logf("│  Set 操作堆分配对比 (更新已有 Key)                  │")
	t.Logf("├──────────────────────────┬──────────────────────────┤")
	t.Logf("│  BigCache Sim (encode)   │  %.0f allocs/op            │", bcAllocs)
	t.Logf("│  TinyCache (路径 1)      │  %.0f allocs/op            │", tcAllocs)
	t.Logf("└──────────────────────────┴──────────────────────────┘")
}

func TestCompare_UpdateMemory(t *testing.T) {
	const (
		keys   = 100_000
		rounds = 10
	)

	s := BenchStruct{ID: 1, Score: 1.0}

	// ---- BigCache Sim ----
	cleanHeap()
	bc := newBigcacheSim(16, keys)
	for i := 0; i < keys; i++ {
		bc.set("k:"+strconv.Itoa(i), encodeBenchStruct(&s))
	}
	queueRound1 := bc.totalQueueLen()

	for r := 1; r < rounds; r++ {
		for i := 0; i < keys; i++ {
			s.ID = uint64(r*keys + i)
			bc.set("k:"+strconv.Itoa(i), encodeBenchStruct(&s))
		}
	}

	runtime.GC()
	var bcMS runtime.MemStats
	runtime.ReadMemStats(&bcMS)
	bcHeap := bcMS.HeapInuse
	queueRoundN := bc.totalQueueLen()
	runtime.KeepAlive(bc)
	bc = nil
	cleanHeap()

	// ---- TinyCache ----
	tc := New[BenchStruct](Config{ShardCount: 16})
	for i := 0; i < keys; i++ {
		tc.Set("k:"+strconv.Itoa(i), s, 0)
	}

	for r := 1; r < rounds; r++ {
		for i := 0; i < keys; i++ {
			s.ID = uint64(r*keys + i)
			tc.Set("k:"+strconv.Itoa(i), s, 0)
		}
	}

	runtime.GC()
	var tcMS runtime.MemStats
	runtime.ReadMemStats(&tcMS)
	tcHeap := tcMS.HeapInuse
	tcStats := tc.Stats() // 假设 TinyCache 有暴露 Stats()
	runtime.KeepAlive(tc)
	tc.Close()

	queueGrowth := float64(queueRoundN) / float64(queueRound1)

	t.Logf("")
	t.Logf("┌─────────────────────────────────────────────────────────┐")
	t.Logf("│  热点 Key 更新内存对比  (%d keys × %d 轮)           │", keys, rounds)
	t.Logf("├───────────────────────────┬─────────────────────────────┤")
	t.Logf("│  BigCache Sim             │                             │")
	t.Logf("│    Queue 第 1 轮后        │  %-27s │", fmtB(uint64(queueRound1)))
	t.Logf("│    Queue 第%2d轮后        │  %-27s │", rounds, fmtB(uint64(queueRoundN)))
	t.Logf("│    Queue 增长倍数         │  %-27s │", fmt.Sprintf("%.1fx", queueGrowth))
	t.Logf("│    HeapInuse              │  %-27s │", fmtB(bcHeap))
	t.Logf("├───────────────────────────┼─────────────────────────────┤")
	t.Logf("│  TinyCache                │                             │")
	t.Logf("│    Data Slots 总量        │  %-27d │", tcStats.DataSlotCount)
	t.Logf("│    FreeList 大小          │  %-27d │", tcStats.FreeSlotCount)
	t.Logf("│    HeapInuse              │  %-27s │", fmtB(tcHeap))
	t.Logf("└───────────────────────────┴─────────────────────────────┘")
	t.Logf("")
	// 修复点：严谨阐述这是未触发环形截断前的废弃空间堆积
	t.Logf("  📝 BigCache 追加模型下（未触发环形淘汰前），产生大量废弃 Entry 堆积")
	if bcHeap > tcHeap {
		t.Logf("  ✅ TinyCache 内存约为 BigCache 模拟器的 1/%.1f（由于原址覆盖特性）", float64(bcHeap)/float64(tcHeap))
	}
}

func TestCompare_DeleteReinsertMemory(t *testing.T) {
	const n = 200_000
	s := BenchStruct{ID: 1, Score: 1.0}

	// ---- BigCache Sim ----
	cleanHeap()
	bc := newBigcacheSim(16, n)
	for i := 0; i < n; i++ {
		bc.set("k:"+strconv.Itoa(i), encodeBenchStruct(&s))
	}
	qInsert := bc.totalQueueLen()

	for i := 0; i < n; i++ {
		bc.del("k:" + strconv.Itoa(i))
	}
	qDelete := bc.totalQueueLen()

	for i := 0; i < n; i++ {
		s.ID = uint64(n + i)
		bc.set("k:"+strconv.Itoa(i), encodeBenchStruct(&s))
	}
	qReinsert := bc.totalQueueLen()

	runtime.GC()
	var bcMS runtime.MemStats
	runtime.ReadMemStats(&bcMS)
	bcHeap := bcMS.HeapInuse
	runtime.KeepAlive(bc)
	bc = nil
	cleanHeap()

	// ---- TinyCache ----
	tc := New[BenchStruct](Config{ShardCount: 16})
	for i := 0; i < n; i++ {
		tc.Set("k:"+strconv.Itoa(i), s, 0)
	}

	for i := 0; i < n; i++ {
		tc.Delete("k:" + strconv.Itoa(i))
	}
	statsDel := tc.Stats()

	for i := 0; i < n; i++ {
		s.ID = uint64(n + i)
		tc.Set("k:"+strconv.Itoa(i), s, 0)
	}

	runtime.GC()
	var tcMS runtime.MemStats
	runtime.ReadMemStats(&tcMS)
	tcHeap := tcMS.HeapInuse
	statsReins := tc.Stats()
	runtime.KeepAlive(tc)
	tc.Close()

	t.Logf("")
	t.Logf("┌─────────────────────────────────────────────────────────┐")
	t.Logf("│  删除后重新插入内存对比  (%d 条目)                  │", n)
	t.Logf("├───────────────────────────┬─────────────────────────────┤")
	t.Logf("│  BigCache Sim             │                             │")
	t.Logf("│    Queue: 插入后          │  %-27s │", fmtB(uint64(qInsert)))
	t.Logf("│    Queue: 删除后          │  %-27s │", fmtB(uint64(qDelete)))
	t.Logf("│    Queue: 重新插入后      │  %-27s │", fmtB(uint64(qReinsert)))
	t.Logf("│    HeapInuse              │  %-27s │", fmtB(bcHeap))
	t.Logf("├───────────────────────────┼─────────────────────────────┤")
	t.Logf("│  TinyCache                │                             │")
	t.Logf("│    删除后 FreeList        │  %-27d │", statsDel.FreeSlotCount)
	t.Logf("│    重新插入后 FreeList    │  %-27d │", statsReins.FreeSlotCount)
	t.Logf("│    重新插入后 Data Slots  │  %-27d │", statsReins.DataSlotCount)
	t.Logf("│    HeapInuse              │  %-27s │", fmtB(tcHeap))
	t.Logf("└───────────────────────────┴─────────────────────────────┘")
	t.Logf("")
	if qDelete == qInsert {
		t.Logf("  📝 BigCache 删除后 Queue 大小不变（因不支持物理回收）")
	}
	t.Logf("  📝 BigCache 重新插入后 Queue ≈ %.1fx（废弃槽位叠加新增）", float64(qReinsert)/float64(qInsert))
	if statsReins.FreeSlotCount == 0 {
		t.Logf("  ✅ TinyCache 优先消耗 FreeList，槽位完美复用")
	}
}
