package core

// FNV-1a 哈希常量
// 参考: https://en.wikipedia.org/wiki/Fowler–Noll–Vo_hash_function#FNV-1a_hash
const (
	fnvOffset64 = 14695981039346656037
	fnvPrime64  = 1099511628211
)

// fnvHash 对字符串进行 FNV-1a 64 位哈希运算
//
// 特点：
//   - 零内存分配：直接遍历 string 底层字节，不转换为 []byte
//   - 雪崩效应好：输入微小变化导致输出大幅变化
//   - 碰撞率低：64 位空间下，2^32 个不同 key 的碰撞概率约为 50%（生日悖论）
//
// 这也是 BigCache 默认使用的哈希函数。
func fnvHash(key string) uint64 {
	h := uint64(fnvOffset64)
	for i := 0; i < len(key); i++ {
		h ^= uint64(key[i])
		h *= fnvPrime64
	}
	return h
}
