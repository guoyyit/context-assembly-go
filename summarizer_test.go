package assembly

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestExtractiveSummarizerRespectsBudget(t *testing.T) {
	frags := []Fragment{
		{ID: "1", Content: "第一句讲述背景。第二句展开细节。第三句给出结论。"},
		{ID: "2", Content: "另一片段的补充说明。"},
	}
	for _, maxTokens := range []int{8, 12, 20, 100} {
		got, err := ExtractiveSummarizer{}.Summarize(context.Background(), frags, maxTokens)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if EstimateTokens(got) > maxTokens {
			t.Fatalf("summary over budget: tokens=%d max=%d", EstimateTokens(got), maxTokens)
		}
		if !strings.HasPrefix(got, "[早前内容摘要]") {
			t.Fatalf("summary must carry衔接 prefix: %q", got)
		}
	}
}

func TestExtractiveSummarizerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	frags := []Fragment{{ID: "1", Content: "内容。"}}
	_, err := ExtractiveSummarizer{}.Summarize(ctx, frags, 10)
	if !errors.Is(err, ErrSummarizeTimeout) {
		t.Fatalf("expected ErrSummarizeTimeout, got %v", err)
	}
}

func TestExtractiveSummarizerZeroBudget(t *testing.T) {
	_, err := ExtractiveSummarizer{}.Summarize(context.Background(), []Fragment{{Content: "x"}}, 0)
	if !errors.Is(err, ErrBudgetTooSmall) {
		t.Fatalf("expected ErrBudgetTooSmall, got %v", err)
	}
}
