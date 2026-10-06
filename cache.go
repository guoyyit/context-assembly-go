package assembly

import (
	"container/list"
	"sync"
)

// CacheKey 唯一标识一次可复用的组装结果。
// 会话版本或摘要版本推进后，旧 key 自然失效，不会再被命中。
type CacheKey struct {
	SessionID      string
	SessionVersion uint64
	SummaryVersion uint64
	Budget         Budget
	RequestHash    uint64
}

// Cache 是容量受限的组装结果缓存（LRU 淘汰）。
// 缓存条目写入后不可变，淘汰只移除整条记录，不破坏一致性。
type Cache struct {
	mu       sync.Mutex
	capacity int
	ll       *list.List
	items    map[CacheKey]*list.Element
}

type cacheEntry struct {
	key    CacheKey
	result Result
}

// NewCache 创建指定容量的缓存，capacity<=0 表示禁用缓存。
func NewCache(capacity int) *Cache {
	return &Cache{
		capacity: capacity,
		ll:       list.New(),
		items:    make(map[CacheKey]*list.Element),
	}
}

// Enabled 返回缓存是否启用。
func (c *Cache) Enabled() bool { return c != nil && c.capacity > 0 }

// Get 查询缓存，命中时把条目移到最新位置。
func (c *Cache) Get(key CacheKey) (Result, bool) {
	if !c.Enabled() {
		return Result{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.ll.MoveToFront(el)
		return el.Value.(cacheEntry).result, true
	}
	return Result{}, false
}

// Put 写入缓存，超容量时淘汰最久未使用的条目。
func (c *Cache) Put(key CacheKey, r Result) {
	if !c.Enabled() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		el.Value = cacheEntry{key: key, result: r}
		c.ll.MoveToFront(el)
		return
	}
	el := c.ll.PushFront(cacheEntry{key: key, result: r})
	c.items[key] = el
	for c.ll.Len() > c.capacity {
		back := c.ll.Back()
		if back == nil {
			break
		}
		delete(c.items, back.Value.(cacheEntry).key)
		c.ll.Remove(back)
	}
}

// InvalidateSession 使某会话的全部缓存失效。
func (c *Cache) InvalidateSession(sessionID string) {
	if !c.Enabled() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, el := range c.items {
		if key.SessionID == sessionID {
			delete(c.items, key)
			c.ll.Remove(el)
		}
	}
}

// Len 返回当前缓存条目数，主要用于测试。
func (c *Cache) Len() int {
	if !c.Enabled() {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}
