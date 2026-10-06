package assembly

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestLocalSummarizerRespectsTarget(t *testing.T) {
	c := TokenCounterFunc(func(s string) int { return len([]rune(s)) })
	s := LocalSummarizer{Counter: c}
	frags := []Fragment{
		{ID: "1", Source: "user", Content: "abcdefghijklmnopqrstuvwxyz"},
		{ID: "2", Source: "assistant", Content: "ABCDEFGHIJKLMNOPQRSTUVWXYZ"},
	}
	out, err := s.Summarize(context.Background(), frags, 40)
	if err != nil {
		t.Fatal(err)
	}
	if n := c.Count(out); n > 40 {
		t.Fatalf("摘要超出目标: %d > 40", n)
	}
	if !strings.Contains(out, "user/1") {
		t.Fatal("摘要应保留来源/ID 标记以便与原文衔接")
	}
	// 确定性：相同输入必须得到相同输出。
	out2, err := s.Summarize(context.Background(), frags, 40)
	if err != nil {
		t.Fatal(err)
	}
	if out != out2 {
		t.Fatal("摘要必须是确定性的")
	}
}

func TestLocalSummarizerCancel(t *testing.T) {
	s := LocalSummarizer{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.Summarize(ctx, []Fragment{{ID: "1", Source: "u", Content: "x"}}, 10)
	if err == nil {
		t.Fatal("取消的上下文必须返回错误")
	}
}

func TestLocalSummarizerZeroTarget(t *testing.T) {
	s := LocalSummarizer{}
	_, err := s.Summarize(context.Background(), []Fragment{{ID: "1", Source: "u", Content: "x"}}, 0)
	if !errors.Is(err, ErrSummaryFailed) {
		t.Fatalf("目标为 0 应返回 ErrSummaryFailed, got %v", err)
	}
}

func TestDefaultTokenCounter(t *testing.T) {
	c := DefaultTokenCounter{}
	if got := c.Count("abcd"); got != 1 {
		t.Fatalf("4 个 ASCII 字符应为 1 token, got %d", got)
	}
	if got := c.Count("你好"); got != 2 {
		t.Fatalf("2 个 CJK 字符应为 2 token, got %d", got)
	}
	if got := c.Count(""); got != 0 {
		t.Fatalf("空串应为 0, got %d", got)
	}
}
