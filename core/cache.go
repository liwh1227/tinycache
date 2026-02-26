package core

import (
	"errors"
	"time"
)

var (
	ErrNotFound = errors.New("tinycache: key not found")
	ErrClosed   = errors.New("tinycache: cache is closed")
)

const (
	defaultShardCount    = 16
	defaultInitShardSize = 256
)

// Config 缓存配置
type Config struct {
	// ShardCount 分片数量，必须是 2 的幂，默认 16
	ShardCount int

	// InitShardSize 每个分片的初始容量（slot 数量），默认 256
	InitShardSize int

	// CleanInterval 后台清理过期条目的间隔，<= 0 表示不启动后台清理
	CleanInterval time.Duration
}

// Cache 顶层缓存对象，通过泛型参数 T 约束存储类型
//
// 核心约束：T 必须是纯值类型（不含 string/slice/map/pointer/interface/chan/func），
// 否则 New 时会 panic，因为 GC 优化的前提是 []T 不包含任何指针。
type Cache[T any] struct {
	shards    []*shard[T]
	shardMask uint64 // 用于位运算路由：hash & mask
	closed    chan struct{}
}

// New 创建缓存实例
//
// 如果 T 包含指针类型字段，会 panic 并提示具体的字段路径。
func New[T any](cfg Config) *Cache[T] {
	// 编译期无法校验泛型参数的内部字段，因此在运行时检查
	assertNoPointers[T]()

	shardCount := nextPowerOf2(cfg.ShardCount)
	if shardCount <= 0 {
		shardCount = defaultShardCount
	}

	initSize := cfg.InitShardSize
	if initSize <= 0 {
		initSize = defaultInitShardSize
	}

	c := &Cache[T]{
		shards:    make([]*shard[T], shardCount),
		shardMask: uint64(shardCount - 1),
		closed:    make(chan struct{}),
	}

	for i := range c.shards {
		c.shards[i] = newShard[T](initSize)
	}

	// 如果配置了清理间隔，启动后台清理协程
	if cfg.CleanInterval > 0 {
		go c.cleanLoop(cfg.CleanInterval)
	}

	return c
}

// Set 写入缓存条目
//
// ttl 为过期时间，<= 0 表示永不过期。
// 如果 key 的哈希值与已有条目冲突，旧条目会被覆盖（BigCache 同款策略）。
func (c *Cache[T]) Set(key string, value T, ttl time.Duration) {
	h := fnvHash(key)
	s := c.shards[h&c.shardMask]
	s.set(h, value, ttl)
}

// Get 读取缓存条目，返回值的拷贝
//
// 如果 key 不存在或已过期，返回 T 的零值和 ErrNotFound。
// 过期条目不会在 Get 中被立即清理（避免在读路径加写锁），
// 而是由 Set 覆盖或 CleanExpired 批量回收。
func (c *Cache[T]) Get(key string) (T, error) {
	h := fnvHash(key)
	s := c.shards[h&c.shardMask]
	return s.get(h)
}

// Delete 删除缓存条目
//
// 被删除条目的槽位会被回收到 FreeList，供后续 Set 复用。
// 如果 key 不存在，静默返回。
func (c *Cache[T]) Delete(key string) {
	h := fnvHash(key)
	s := c.shards[h&c.shardMask]
	s.del(h)
}

// Len 返回缓存条目总数（各分片之和，非精确快照）
func (c *Cache[T]) Len() int {
	total := 0
	for _, s := range c.shards {
		total += s.len()
	}
	return total
}

// CleanExpired 遍历所有分片，清理过期条目并回收槽位到 FreeList
//
// 适合在低峰期定期调用，或由后台协程执行。
func (c *Cache[T]) CleanExpired() {
	for _, s := range c.shards {
		s.cleanExpired()
	}
}

// Stats 返回缓存的统计信息
func (c *Cache[T]) Stats() CacheStats {
	var total CacheStats
	for _, s := range c.shards {
		st := s.stats()
		total.Entries += st.Entries
		total.DataSlotCount += st.DataSlotCount
		total.FreeSlotCount += st.FreeSlotCount
	}
	total.ShardCount = len(c.shards)
	return total
}

// Close 关闭缓存，停止后台清理协程
func (c *Cache[T]) Close() {
	close(c.closed)
}

// cleanLoop 后台定时清理过期条目
func (c *Cache[T]) cleanLoop(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			c.CleanExpired()
		case <-c.closed:
			return
		}
	}
}

// CacheStats 缓存统计信息
type CacheStats struct {
	ShardCount    int // 分片总数
	Entries       int // 有效条目总数
	DataSlotCount int // data 切片已分配的槽位总数
	FreeSlotCount int // FreeList 中可复用的槽位总数
}

// nextPowerOf2 将 n 向上取整为 2 的幂
func nextPowerOf2(n int) int {
	if n <= 0 {
		return 0
	}
	n--
	n |= n >> 1
	n |= n >> 2
	n |= n >> 4
	n |= n >> 8
	n |= n >> 16
	n++
	return n
}
