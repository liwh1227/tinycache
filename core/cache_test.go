package core

import (
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"
	"unsafe"
)

// ========== 测试用的结构体 ==========

// UserProfile 纯值类型 —— GC 友好 ✅
type UserProfile struct {
	ID        uint64
	Score     float64
	Level     uint16
	Status    uint8
	Timestamp int64
}

// BadStruct 包含 string 指针 —— GC 不友好 ❌
type BadStruct struct {
	ID   uint64
	Name string // string 底层含指针
}

// NestedBadStruct 嵌套了包含 slice 的结构体 ❌
type NestedBadStruct struct {
	ID    uint64
	Inner struct {
		Tags [3]int
		Ptrs []int // slice 含指针
	}
}

type BadStruct2 struct {
	Pp unsafe.Pointer
}

// ========== 基础 CRUD 测试 ==========

func TestBasicSetAndGet(t *testing.T) {
	c := New[UserProfile](Config{})
	defer c.Close()

	profile := UserProfile{
		ID:        1001,
		Score:     99.5,
		Level:     42,
		Status:    1,
		Timestamp: time.Now().UnixNano(),
	}

	c.Set("user:1001", profile, 0)

	got, err := c.Get("user:1001")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if got.ID != profile.ID || got.Score != profile.Score || got.Level != profile.Level {
		t.Fatalf("data mismatch: expected %+v, got %+v", profile, got)
	}
}

func TestGetNotFound(t *testing.T) {
	c := New[UserProfile](Config{})
	defer c.Close()

	_, err := c.Get("nonexistent")
	if err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestUpdate(t *testing.T) {
	c := New[UserProfile](Config{})
	defer c.Close()

	c.Set("user:1", UserProfile{ID: 1, Score: 10.0}, 0)
	c.Set("user:1", UserProfile{ID: 1, Score: 99.0}, 0) // 更新

	got, err := c.Get("user:1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Score != 99.0 {
		t.Fatalf("expected Score=99.0 after update, got %f", got.Score)
	}
}

func TestDelete(t *testing.T) {
	c := New[UserProfile](Config{})
	defer c.Close()

	c.Set("user:1", UserProfile{ID: 1}, 0)
	c.Delete("user:1")

	_, err := c.Get("user:1")
	if err != ErrNotFound {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
}

func TestDeleteNonExistent(t *testing.T) {
	c := New[UserProfile](Config{})
	defer c.Close()

	// 删除不存在的 key 应该静默成功
	c.Delete("ghost")
}

func TestLen(t *testing.T) {
	c := New[UserProfile](Config{ShardCount: 4})
	defer c.Close()

	for i := 0; i < 100; i++ {
		c.Set(fmt.Sprintf("user:%d", i), UserProfile{ID: uint64(i)}, 0)
	}

	if c.Len() != 100 {
		t.Fatalf("expected Len()=100, got %d", c.Len())
	}

	for i := 0; i < 30; i++ {
		c.Delete(fmt.Sprintf("user:%d", i))
	}

	t.Log(c.Len())

	if c.Len() != 70 {
		t.Fatalf("expected Len()=70 after deleting 30, got %d", c.Len())
	}
}

// ========== TTL 过期测试 ==========

func TestTTLExpiration(t *testing.T) {
	c := New[UserProfile](Config{})
	defer c.Close()

	c.Set("ephemeral", UserProfile{ID: 1}, 50*time.Millisecond)

	// 过期前应该能读到
	got, err := c.Get("ephemeral")
	if err != nil {
		t.Fatalf("expected data before expiry, got %v", err)
	}
	if got.ID != 1 {
		t.Fatalf("expected ID=1, got %d", got.ID)
	}

	// 等待过期
	time.Sleep(80 * time.Millisecond)

	_, err = c.Get("ephemeral")
	if err != ErrNotFound {
		t.Fatalf("expected ErrNotFound after expiry, got %v", err)
	}
}

func TestTTLZeroMeansNoExpire(t *testing.T) {
	c := New[UserProfile](Config{})
	defer c.Close()

	c.Set("permanent", UserProfile{ID: 1}, 0) // ttl=0 永不过期

	time.Sleep(50 * time.Millisecond)

	_, err := c.Get("permanent")
	if err != nil {
		t.Fatalf("ttl=0 entry should not expire, got %v", err)
	}
}

func TestCleanExpired(t *testing.T) {
	c := New[UserProfile](Config{ShardCount: 2})
	defer c.Close()

	// 写入 10 条短 TTL 和 10 条永不过期
	for i := 0; i < 10; i++ {
		c.Set(fmt.Sprintf("short:%d", i), UserProfile{ID: uint64(i)}, 50*time.Millisecond)
	}
	for i := 0; i < 10; i++ {
		c.Set(fmt.Sprintf("long:%d", i), UserProfile{ID: uint64(i + 100)}, 0)
	}

	if c.Len() != 20 {
		t.Fatalf("expected 20 entries, got %d", c.Len())
	}

	time.Sleep(80 * time.Millisecond)
	c.CleanExpired()

	if c.Len() != 10 {
		t.Fatalf("expected 10 entries after cleanup, got %d", c.Len())
	}

	// 确认过期的被清理，未过期的还在
	for i := 0; i < 10; i++ {
		_, err := c.Get(fmt.Sprintf("short:%d", i))
		if err != ErrNotFound {
			t.Fatalf("short:%d should be expired", i)
		}
		_, err = c.Get(fmt.Sprintf("long:%d", i))
		if err != nil {
			t.Fatalf("long:%d should still exist, got %v", i, err)
		}
	}
}

// ========== FreeList 复用测试 ==========

func TestFreeListReuse(t *testing.T) {
	c := New[UserProfile](Config{ShardCount: 1, InitShardSize: 4})
	defer c.Close()

	// 写入 5 条数据
	for i := 0; i < 5; i++ {
		c.Set(fmt.Sprintf("k:%d", i), UserProfile{ID: uint64(i)}, 0)
	}

	stats1 := c.Stats()
	if stats1.DataSlotCount != 5 {
		t.Fatalf("expected 5 data slots, got %d", stats1.DataSlotCount)
	}
	if stats1.FreeSlotCount != 0 {
		t.Fatalf("expected 0 free slots, got %d", stats1.FreeSlotCount)
	}

	// 删除前 3 条
	for i := 0; i < 3; i++ {
		c.Delete(fmt.Sprintf("k:%d", i))
	}

	stats2 := c.Stats()
	if stats2.Entries != 2 {
		t.Fatalf("expected 2 entries, got %d", stats2.Entries)
	}
	if stats2.FreeSlotCount != 3 {
		t.Fatalf("expected 3 free slots, got %d", stats2.FreeSlotCount)
	}
	if stats2.DataSlotCount != 5 {
		t.Fatalf("data slice should not shrink, expected 5, got %d", stats2.DataSlotCount)
	}

	// 写入 2 条新数据 —— 应该复用 freeList 中的槽位，而不是 append
	c.Set("new:0", UserProfile{ID: 100}, 0)
	c.Set("new:1", UserProfile{ID: 101}, 0)

	stats3 := c.Stats()
	if stats3.DataSlotCount != 5 {
		t.Fatalf("data slice should NOT grow (freeList reused), expected 5, got %d", stats3.DataSlotCount)
	}
	if stats3.FreeSlotCount != 1 {
		t.Fatalf("expected 1 remaining free slot, got %d", stats3.FreeSlotCount)
	}
	if stats3.Entries != 4 {
		t.Fatalf("expected 4 entries, got %d", stats3.Entries)
	}

	// 验证新数据可以正确读取
	got, err := c.Get("new:0")
	if err != nil || got.ID != 100 {
		t.Fatalf("expected ID=100 for new:0, got %+v, err=%v", got, err)
	}
}

// ========== 并发安全测试 ==========

func TestConcurrentAccess(t *testing.T) {
	c := New[UserProfile](Config{ShardCount: 8})
	defer c.Close()

	const (
		goroutines = 100
		opsPerG    = 1000
	)

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for g := 0; g < goroutines; g++ {
		go func(id int) {
			defer wg.Done()
			r := rand.New(rand.NewSource(int64(id)))

			for i := 0; i < opsPerG; i++ {
				key := fmt.Sprintf("key:%d", r.Intn(500))
				op := r.Intn(10)

				switch {
				case op < 5: // 50% 读
					_, err := c.Get(key)
					if err != nil {
						return
					}
				case op < 8: // 30% 写
					c.Set(key, UserProfile{ID: uint64(r.Int63())}, time.Second)
				default: // 20% 删
					c.Delete(key)
				}
			}
		}(g)
	}

	wg.Wait()

	// 如果没有 panic 或 race 报告，测试通过
	t.Logf("concurrent test passed: %d goroutines × %d ops, final len=%d",
		goroutines, opsPerG, c.Len())
}

// ========== 指针检测测试 ==========

func TestRejectPointerType(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for pointer-containing type, got none")
		}
		t.Logf("correctly rejected: %v", r)
	}()

	// BadStruct 包含 string（指针类型），应该 panic
	New[BadStruct](Config{})
}

func TestUnsafePointerType(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for pointer-containing type, got none")
		}
		t.Logf("correctly rejected: %v", r)
	}()

	New[BadStruct2](Config{})
}

func TestRejectNestedPointerType(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for nested pointer-containing type, got none")
		}
		t.Logf("correctly rejected: %v", r)
	}()

	New[NestedBadStruct](Config{})
}

func TestAcceptPureValueType(t *testing.T) {
	// 这些类型都应该通过检查，不 panic
	_ = New[UserProfile](Config{})

	type SimpleStruct struct {
		A int64
		B float64
		C bool
	}
	_ = New[SimpleStruct](Config{})

	type WithArray struct {
		Values [8]uint32
	}
	_ = New[WithArray](Config{})
}

func TestErrorType(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for pointer-containing type, got none")
		}
		t.Logf("correctly rejected: %v", r)
	}()

	New[error](Config{})
}

// ========== Stats 测试 ==========

func TestStats(t *testing.T) {
	c := New[UserProfile](Config{ShardCount: 4})
	defer c.Close()

	stats := c.Stats()
	if stats.ShardCount != 4 {
		t.Fatalf("expected 4 shards, got %d", stats.ShardCount)
	}

	for i := 0; i < 50; i++ {
		c.Set(fmt.Sprintf("k:%d", i), UserProfile{ID: uint64(i)}, 0)
	}

	stats = c.Stats()
	if stats.Entries != 50 {
		t.Fatalf("expected 50 entries, got %d", stats.Entries)
	}
}

// ========== 配置测试 ==========

func TestShardCountRoundUp(t *testing.T) {
	// ShardCount=3 应该向上取整为 4
	c := New[UserProfile](Config{ShardCount: 3})
	defer c.Close()

	if len(c.shards) != 4 {
		t.Fatalf("expected 4 shards (rounded up from 3), got %d", len(c.shards))
	}
}

func TestDefaultConfig(t *testing.T) {
	c := New[UserProfile](Config{})
	defer c.Close()

	if len(c.shards) != defaultShardCount {
		t.Fatalf("expected default %d shards, got %d", defaultShardCount, len(c.shards))
	}
}

// ========== Benchmark ==========

func BenchmarkSet(b *testing.B) {
	c := New[UserProfile](Config{ShardCount: 16})
	defer c.Close()

	profile := UserProfile{ID: 1, Score: 99.5, Level: 42, Status: 1}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			c.Set(fmt.Sprintf("user:%d", i), profile, time.Minute)
			i++
		}
	})
}

func BenchmarkGet(b *testing.B) {
	c := New[UserProfile](Config{ShardCount: 16})
	defer c.Close()

	// 预填充
	for i := 0; i < 10000; i++ {
		c.Set(fmt.Sprintf("user:%d", i), UserProfile{ID: uint64(i)}, 0)
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			c.Get(fmt.Sprintf("user:%d", i%10000))
			i++
		}
	})
}

func BenchmarkSetAndGet_Mixed(b *testing.B) {
	c := New[UserProfile](Config{ShardCount: 16})
	defer c.Close()

	profile := UserProfile{ID: 1, Score: 99.5}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := fmt.Sprintf("user:%d", i%5000)
			if i%3 == 0 {
				c.Set(key, profile, time.Minute)
			} else {
				c.Get(key)
			}
			i++
		}
	})
}

func BenchmarkFreeListReuse(b *testing.B) {
	c := New[UserProfile](Config{ShardCount: 1})
	defer c.Close()

	profile := UserProfile{ID: 1, Score: 99.5}

	// 预填充后删除一半，制造 freeList
	for i := 0; i < 1000; i++ {
		c.Set(fmt.Sprintf("pre:%d", i), profile, 0)
	}
	for i := 0; i < 500; i++ {
		c.Delete(fmt.Sprintf("pre:%d", i))
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		key := fmt.Sprintf("bench:%d", i)
		c.Set(key, profile, time.Minute)
		c.Delete(key) // 立即删除，使 freeList 持续可用
	}
}
