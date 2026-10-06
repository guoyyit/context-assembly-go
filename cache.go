package assembly

import (
	"container/list"
	"sync"
)

// cacheKey 唯一标识一次组装结果；会话版本与预算变化都会得到不同的键，
// 因此来源变更或摘要推进（都会推进版本）后旧结果自然失效。
type cacheKey struct {
	SessionID string
	Version   uint64
	Budget    Budget
}

// cacheEntry 是缓存中的一个条目。
type cacheEntry struct {
	key    cacheKey
	result Result
}

// Cache 是容量受限的组装结果缓存（LRU 淘汰）。
// 淘汰只删除整条结果，键中包含版本号，
// 因此淘汰不会破坏"结果必须对应完整版本"的一致性。
type Cache struct {
	mu       sync.Mutex
	capacity int
	ll       *list.List //  front = 最近使用
	items    map[cacheKey]*list.Element
}

// NewCache 创建容量为 capacity 的缓存；capacity <= 0 表示不缓存。
func NewCache(capacity int) *Cache {
	if capacity <= 0 {
		return nil
	}
	return &Cache{
		capacity: capacity,
		ll:       list.New(),
		items:    make(map[cacheKey]*list.Element),
	}
}

// Get 查询缓存，命中时把条目移到最新位置。
func (c *Cache) Get(k cacheKey) (Result, bool) {
	if c == nil {
		return Result{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[k]; ok {
		c.ll.MoveToFront(el)
		return el.Value.(cacheEntry).result, true
	}
	return Result{}, false
}

// Add 写入缓存，容量不足时淘汰最久未使用的条目。
func (c *Cache) Add(k cacheKey, r Result) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[k]; ok {
		el.Value = cacheEntry{key: k, result: r}
		c.ll.MoveToFront(el)
		return
	}
	el := c.ll.PushFront(cacheEntry{key: k, result: r})
	c.items[k] = el
	for c.ll.Len() > c.capacity {
		old := c.ll.Back()
		if old == nil {
			break
		}
		c.ll.Remove(old)
		delete(c.items, old.Value.(cacheEntry).key)
	}
}

// Len 返回当前缓存条目数（主要用于测试）。
func (c *Cache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}
