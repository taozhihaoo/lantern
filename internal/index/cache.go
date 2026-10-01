package index

import (
	"container/list"
	"sync"
)

// cacheKey 标识缓存中的一个块:段 ID + 文件名 + 偏移 + 长度。
type cacheKey struct {
	seg  string
	file string
	off  int64
	ln   int
}

// BlockCache 是带容量上限的 LRU 块缓存(默认 64MB),键为
// (段, 文件, 偏移, 长度)。并发安全。缓存的 []byte 为只读共享,
// 调用方不得修改。
type BlockCache struct {
	mu   sync.Mutex
	cap  int
	size int
	ll   *list.List // 前端=最新
	m    map[cacheKey]*list.Element
}

type cacheEntry struct {
	k cacheKey
	b []byte
}

// NewBlockCache 创建容量为 capBytes 的缓存;capBytes<=0 使用默认 64MB。
func NewBlockCache(capBytes int) *BlockCache {
	if capBytes <= 0 {
		capBytes = DefaultCacheBytes
	}
	return &BlockCache{cap: capBytes, ll: list.New(), m: map[cacheKey]*list.Element{}}
}

// Get 返回缓存块(共享只读)。
func (c *BlockCache) Get(k cacheKey) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.m[k]; ok {
		c.ll.MoveToFront(el)
		return el.Value.(*cacheEntry).b, true
	}
	return nil, false
}

// Put 放入块,超容量时从最旧端逐出。
func (c *BlockCache) Put(k cacheKey, b []byte) {
	if len(b) > c.cap {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.m[k]; ok {
		return
	}
	c.m[k] = c.ll.PushFront(&cacheEntry{k: k, b: b})
	c.size += len(b)
	for c.size > c.cap {
		back := c.ll.Back()
		if back == nil {
			return
		}
		e := back.Value.(*cacheEntry)
		c.ll.Remove(back)
		delete(c.m, e.k)
		c.size -= len(e.b)
	}
}
