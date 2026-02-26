package core

import (
	"sync"
	"time"
)

// slot 是 data 切片中的存储单元
//
// 只要 T 不含指针，slot 也不含指针，整个 []slot[T] 对 GC 而言是 no-scan 的。
// 字段说明：
//   - keyHash:  完整的 64 位哈希值，用于 Get 时做冲突校验
//   - expireAt: 过期时间（UnixNano），0 表示永不过期
//   - data:     用户的业务数据
type slot[T any] struct {
	keyHash  uint64
	expireAt int64
	data     T
}

// shard 是真正的缓存执行单元，每个 shard 独立持有锁、索引和数据
//
// 三层结构：
//   - index:    map[uint64]uint32  纯整数索引，GC 直接跳过
//   - data:     []slot[T]          连续内存数据仓库，GC 只看到一个指针
//   - freeList: []uint32           空闲槽位栈，解决 delete 留下的空洞问题
type shard[T any] struct {
	mu       sync.RWMutex
	index    map[uint64]uint32
	data     []slot[T]
	freeList []uint32
}

func newShard[T any](initCap int) *shard[T] {
	return &shard[T]{
		index:    make(map[uint64]uint32, initCap),
		data:     make([]slot[T], 0, initCap),
		freeList: make([]uint32, 0),
	}
}

// set 写入或更新一个条目
//
// 流程：
//  1. 如果 keyHash 已存在于 index，说明是更新（或哈希冲突覆盖），直接原地覆盖 data 中对应 slot
//  2. 如果 freeList 非空，弹出一个空闲下标，将数据写入该位置（槽位复用）
//  3. 都不满足，append 到 data 尾部（分配新槽位）
func (s *shard[T]) set(keyHash uint64, value T, ttl time.Duration) {
	var expireAt int64
	if ttl > 0 {
		expireAt = time.Now().Add(ttl).UnixNano()
	}

	entry := slot[T]{
		keyHash:  keyHash,
		expireAt: expireAt,
		data:     value,
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// 路径 1：key 已存在（更新）或哈希冲突（覆盖）
	if idx, ok := s.index[keyHash]; ok {
		s.data[idx] = entry
		return
	}

	// 路径 2：从 freeList 复用空闲槽位
	if n := len(s.freeList); n > 0 {
		freeIdx := s.freeList[n-1] // LIFO: 弹出栈顶
		s.freeList = s.freeList[:n-1]
		s.data[freeIdx] = entry
		s.index[keyHash] = freeIdx
		return
	}

	// 路径 3：追加到 data 尾部
	idx := uint32(len(s.data))
	s.data = append(s.data, entry)
	s.index[keyHash] = idx
}

// get 读取条目，返回值拷贝
//
// 流程：
//  1. 从 index 查找下标，不存在返回 ErrNotFound
//  2. 通过下标从 data 中读取 slot
//  3. 校验 keyHash 是否一致（防冲突覆盖后读到错误数据）
//  4. 检查是否过期
//  5. 返回 data 的值拷贝（读锁释放后调用方持有的是独立副本，无并发风险）
func (s *shard[T]) get(keyHash uint64) (T, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	idx, ok := s.index[keyHash]
	if !ok {
		var zero T
		return zero, ErrNotFound
	}

	entry := s.data[idx]

	// 哈希冲突校验：如果 slot 中记录的 keyHash 与查询的不一致，
	// 说明该槽位已被另一个 key 覆盖
	if entry.keyHash != keyHash {
		var zero T
		return zero, ErrNotFound
	}

	// 过期检查（惰性判定，不在读路径加写锁清理）
	if entry.expireAt > 0 && time.Now().UnixNano() > entry.expireAt {
		var zero T
		return zero, ErrNotFound
	}

	return entry.data, nil
}

// del 删除条目，将槽位回收到 freeList
//
// 精髓：不做物理内存释放，而是将槽位下标压入 freeList，
// 下次 set 时优先从 freeList 取用，实现内存复用。
func (s *shard[T]) del(keyHash uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	idx, ok := s.index[keyHash]
	if !ok {
		return
	}

	// 从索引中移除
	delete(s.index, keyHash)

	// 清零 slot，防止持有过期的业务数据引用
	// （虽然 T 不含指针，清零仍是好习惯，避免数据残留被误读）
	var zeroSlot slot[T]
	s.data[idx] = zeroSlot

	// 将下标归还给 freeList
	s.freeList = append(s.freeList, idx)
}

// cleanExpired 扫描 index，清理所有过期条目并回收槽位
func (s *shard[T]) cleanExpired() {
	now := time.Now().UnixNano()

	s.mu.Lock()
	defer s.mu.Unlock()

	for keyHash, idx := range s.index {
		entry := s.data[idx]
		if entry.expireAt > 0 && now > entry.expireAt {
			delete(s.index, keyHash)

			var zeroSlot slot[T]
			s.data[idx] = zeroSlot

			s.freeList = append(s.freeList, idx)
		}
	}
}

// len 返回当前分片的有效条目数
func (s *shard[T]) len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.index)
}

// stats 返回分片级统计信息
func (s *shard[T]) stats() CacheStats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return CacheStats{
		Entries:       len(s.index),
		DataSlotCount: len(s.data),
		FreeSlotCount: len(s.freeList),
	}
}
