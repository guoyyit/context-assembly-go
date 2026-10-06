package assembly

import "testing"

func TestCacheLRUEviction(t *testing.T) {
	c := NewCache(2)
	k1 := cacheKey{SessionID: "s", Version: 1}
	k2 := cacheKey{SessionID: "s", Version: 2}
	k3 := cacheKey{SessionID: "s", Version: 3}

	c.Add(k1, Result{Version: 1})
	c.Add(k2, Result{Version: 2})
	if _, ok := c.Get(k1); !ok {
		t.Fatal("k1 应命中")
	}
	c.Add(k3, Result{Version: 3}) // 容量满，淘汰最久未使用的 k2

	if _, ok := c.Get(k2); ok {
		t.Fatal("k2 应被淘汰")
	}
	if _, ok := c.Get(k1); !ok {
		t.Fatal("k1 应保留")
	}
	if c.Len() != 2 {
		t.Fatalf("缓存容量应恒为 2, got %d", c.Len())
	}
}

func TestCacheKeyContainsVersionAndBudget(t *testing.T) {
	c := NewCache(8)
	base := cacheKey{SessionID: "s", Version: 1, Budget: Budget{MaxTokens: 100, ReserveOutput: 10}}
	c.Add(base, Result{Version: 1})
	// 版本不同 → 不命中（来源变更/摘要推进后旧结果失效）。
	if _, ok := c.Get(cacheKey{SessionID: "s", Version: 2, Budget: base.Budget}); ok {
		t.Fatal("版本变化后旧结果不得命中")
	}
	// 预算不同 → 不命中。
	if _, ok := c.Get(cacheKey{SessionID: "s", Version: 1, Budget: Budget{MaxTokens: 50, ReserveOutput: 10}}); ok {
		t.Fatal("预算变化后旧结果不得命中")
	}
}

func TestNilCacheIsNoop(t *testing.T) {
	var c *Cache
	if _, ok := c.Get(cacheKey{}); ok {
		t.Fatal("nil 缓存不应命中")
	}
	c.Add(cacheKey{}, Result{}) // 不应 panic
	if NewCache(0) != nil {
		t.Fatal("capacity<=0 应返回 nil 缓存")
	}
}
