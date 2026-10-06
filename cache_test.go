package assembly

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestAssembleUsesCache(t *testing.T) {
	cache := NewCache(8)
	a := NewAssembler(
		WithBudgetPolicy(BudgetPolicy{Global: Budget{MaxTokens: 100}}),
		WithCache(cache),
	)
	a.Session("s1") // 建立空会话，版本为 0

	// 预置与请求对应的缓存条目，验证 Assemble 会命中。
	key := CacheKey{
		SessionID:      "s1",
		SessionVersion: 0,
		SummaryVersion: 0,
		Budget:         Budget{MaxTokens: 100},
		RequestHash:    hashFragments(nil),
	}
	cache.Put(key, Result{Content: "CACHED", Budget: Budget{MaxTokens: 100}})

	res, err := a.Assemble(context.Background(), Request{SessionID: "s1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Content != "CACHED" {
		t.Fatalf("expected cached result, got %q", res.Content)
	}
}

func TestCacheInvalidatedOnSourceUpdate(t *testing.T) {
	cache := NewCache(8)
	a := NewAssembler(
		WithBudgetPolicy(BudgetPolicy{Global: Budget{MaxTokens: 1000}}),
		WithCache(cache),
	)
	sess := a.Session("s1")
	sess.Upsert(frag("doc-v1", "doc", 1, 1, "旧内容"))

	res1, err := a.Assemble(context.Background(), Request{SessionID: "s1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(res1.Content, "旧内容") {
		t.Fatalf("unexpected content: %q", res1.Content)
	}
	if cache.Len() != 1 {
		t.Fatalf("expected result cached, len=%d", cache.Len())
	}

	// 来源内容变更：会话版本推进，旧缓存不得再被使用。
	sess.Upsert(frag("doc-v2", "doc", 1, 2, "新内容"))
	res2, err := a.Assemble(context.Background(), Request{SessionID: "s1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(res2.Content, "新内容") || strings.Contains(res2.Content, "旧内容") {
		t.Fatalf("stale cache used after source update: %q", res2.Content)
	}

	// 摘要推进同样使旧结果失效。
	snap := sess.Snapshot()
	ids := []string{snap.Fragments[0].ID}
	if !sess.CommitSummary(snap.Version, Summary{Text: "压缩后的摘要"}, ids...) {
		t.Fatal("commit should succeed")
	}
	res3, err := a.Assemble(context.Background(), Request{SessionID: "s1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(res3.Content, "压缩后的摘要") {
		t.Fatalf("stale cache used after summary advance: %q", res3.Content)
	}
}

func TestCacheLRUEviction(t *testing.T) {
	c := NewCache(2)
	k1 := CacheKey{SessionID: "s1", SessionVersion: 1}
	k2 := CacheKey{SessionID: "s1", SessionVersion: 2}
	k3 := CacheKey{SessionID: "s1", SessionVersion: 3}

	c.Put(k1, Result{Content: "r1"})
	c.Put(k2, Result{Content: "r2"})
	c.Put(k3, Result{Content: "r3"}) // 超出容量，淘汰最久未使用的 k1

	if c.Len() != 2 {
		t.Fatalf("expected len 2, got %d", c.Len())
	}
	if _, ok := c.Get(k1); ok {
		t.Fatal("k1 should have been evicted")
	}
	// 命中 k2 使其成为最新，再写入应淘汰 k3。
	if _, ok := c.Get(k2); !ok {
		t.Fatal("k2 should be present")
	}
	k4 := CacheKey{SessionID: "s1", SessionVersion: 4}
	c.Put(k4, Result{Content: "r4"})
	if _, ok := c.Get(k3); ok {
		t.Fatal("k3 should have been evicted after k2 was touched")
	}
	if _, ok := c.Get(k2); !ok || c.Len() != 2 {
		t.Fatal("k2 and k4 should remain")
	}
}

func TestCacheInvalidateSession(t *testing.T) {
	c := NewCache(8)
	for i := 0; i < 3; i++ {
		c.Put(CacheKey{SessionID: "s1", SessionVersion: uint64(i)}, Result{Content: fmt.Sprintf("a%d", i)})
		c.Put(CacheKey{SessionID: "s2", SessionVersion: uint64(i)}, Result{Content: fmt.Sprintf("b%d", i)})
	}
	c.InvalidateSession("s1")
	if c.Len() != 3 {
		t.Fatalf("expected 3 entries left, got %d", c.Len())
	}
	if _, ok := c.Get(CacheKey{SessionID: "s1", SessionVersion: 0}); ok {
		t.Fatal("s1 entries must be invalidated")
	}
	if _, ok := c.Get(CacheKey{SessionID: "s2", SessionVersion: 0}); !ok {
		t.Fatal("s2 entries must survive")
	}
}

func TestDegradedResultNotCached(t *testing.T) {
	cache := NewCache(8)
	calls := 0
	fail := SummarizerFunc(func(ctx context.Context, fs []Fragment, maxTokens int) (string, error) {
		calls++
		return "", ErrSummarizeFailed
	})
	a := NewAssembler(
		WithBudgetPolicy(BudgetPolicy{Global: Budget{MaxTokens: 10}}),
		WithSummarizer(fail),
		WithCache(cache),
	)
	sess := a.Session("s1")
	sess.Upsert(frag("f1", "chat", 1, 1, "超出预算的长内容需要压缩"))

	for i := 0; i < 2; i++ {
		res, err := a.Assemble(context.Background(), Request{SessionID: "s1"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !res.Degraded {
			t.Fatal("expected degraded result")
		}
	}
	// 降级结果不缓存，每次都重新尝试摘要。
	if calls != 2 {
		t.Fatalf("expected summarizer called twice, got %d", calls)
	}
	if cache.Len() != 0 {
		t.Fatalf("degraded results must not be cached, len=%d", cache.Len())
	}
}
