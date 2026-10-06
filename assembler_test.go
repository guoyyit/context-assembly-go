package assembly

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

var base = time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)

func frag(id, source string, prio int, version uint64, content string) Fragment {
	return Fragment{
		ID:        id,
		Source:    source,
		Priority:  prio,
		Version:   version,
		CreatedAt: base.Add(time.Duration(len(content)) * time.Second),
		Content:   content,
	}
}

func hasReason(r Result, substr string) bool {
	for _, s := range r.DegradeReasons {
		if strings.Contains(s, substr) {
			return true
		}
	}
	return false
}

func hasAction(r Result, id, action string) bool {
	for _, d := range r.Decisions {
		if d.FragmentID == id && d.Action == action {
			return true
		}
	}
	return false
}

func TestAssembleWithinBudget(t *testing.T) {
	a := NewAssembler(WithBudgetPolicy(BudgetPolicy{Global: Budget{MaxTokens: 1000}}))
	sess := a.Session("s1")
	sess.Upsert(frag("f1", "chat", 1, 1, "hello world"), frag("f2", "chat", 1, 1, "second message"))

	res, err := a.Assemble(context.Background(), Request{SessionID: "s1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Degraded {
		t.Fatalf("unexpected degradation: %v", res.DegradeReasons)
	}
	if !strings.Contains(res.Content, "hello world") || !strings.Contains(res.Content, "second message") {
		t.Fatalf("content missing fragments: %q", res.Content)
	}
	if res.TokensUsed > 1000 {
		t.Fatalf("over budget: %d", res.TokensUsed)
	}
	if !hasAction(res, "f1", "included") || !hasAction(res, "f2", "included") {
		t.Fatalf("expected included decisions: %+v", res.Decisions)
	}
}

func TestAssembleOverBudgetSummarizes(t *testing.T) {
	a := NewAssembler(WithBudgetPolicy(BudgetPolicy{Global: Budget{MaxTokens: 40}}))
	sess := a.Session("s1")
	for i, text := range []string{
		"第一段内容，讲述需求背景与目标。",
		"第二段内容，记录方案讨论过程。",
		"第三段内容，确认最终结论与行动项。",
		"最新的实时问题需要保留。",
	} {
		sess.Upsert(Fragment{
			ID:        string(rune('a' + i)),
			Source:    "chat",
			Priority:  i, // 越早优先级越低
			Version:   1,
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
			Content:   text,
		})
	}

	res, err := a.Assemble(context.Background(), Request{SessionID: "s1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Degraded {
		t.Fatalf("unexpected degradation: %v", res.DegradeReasons)
	}
	if !res.SummaryUsed {
		t.Fatal("expected summary to be used")
	}
	if res.TokensUsed > 40 {
		t.Fatalf("over budget: used=%d budget=40", res.TokensUsed)
	}
	if !strings.Contains(res.Content, "最新的实时问题需要保留。") {
		t.Fatalf("newest fragment should be kept: %q", res.Content)
	}
	if !strings.HasPrefix(res.Content, "[早前内容摘要]") {
		t.Fatalf("summary should prefix content: %q", res.Content)
	}
	if !hasAction(res, "a", "summarized") {
		t.Fatalf("expected oldest fragment summarized: %+v", res.Decisions)
	}
	// 摘要必须推进到会话状态中。
	if snap := sess.Snapshot(); snap.Summary.Text == "" || snap.Summary.Version != 1 {
		t.Fatalf("summary not committed: %+v", snap.Summary)
	}
}

func TestAssembleSummaryFailureDegrades(t *testing.T) {
	fail := SummarizerFunc(func(ctx context.Context, fs []Fragment, maxTokens int) (string, error) {
		return "", errors.New("boom")
	})
	a := NewAssembler(
		WithBudgetPolicy(BudgetPolicy{Global: Budget{MaxTokens: 20}}),
		WithSummarizer(fail),
	)
	sess := a.Session("s1")
	sess.Upsert(frag("f1", "chat", 1, 1, "一段比较长的历史内容需要被压缩处理否则无法放入上下文窗口"))
	sess.Upsert(frag("f2", "chat", 5, 1, "重要"))

	res, err := a.Assemble(context.Background(), Request{SessionID: "s1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Degraded || !hasReason(res, ErrSummarizeFailed.Error()) {
		t.Fatalf("expected summarize-failed degradation: %+v", res)
	}
	if res.TokensUsed > 20 {
		t.Fatalf("degraded result over budget: %d", res.TokensUsed)
	}
	if !strings.Contains(res.Content, "重要") {
		t.Fatalf("high priority fragment must survive: %q", res.Content)
	}
	// 摘要失败不得留下半更新状态。
	if snap := sess.Snapshot(); snap.Summary.Text != "" || snap.Summary.Version != 0 {
		t.Fatalf("summary must not be committed on failure: %+v", snap.Summary)
	}
}

func TestAssembleSummaryTimeoutDegrades(t *testing.T) {
	slow := SummarizerFunc(func(ctx context.Context, fs []Fragment, maxTokens int) (string, error) {
		return "", ErrSummarizeTimeout
	})
	a := NewAssembler(
		WithBudgetPolicy(BudgetPolicy{Global: Budget{MaxTokens: 20}}),
		WithSummarizer(slow),
	)
	sess := a.Session("s1")
	sess.Upsert(frag("f1", "chat", 1, 1, "一段比较长的历史内容需要被压缩处理否则无法放入上下文窗口"))

	res, err := a.Assemble(context.Background(), Request{SessionID: "s1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Degraded || !hasReason(res, ErrSummarizeTimeout.Error()) {
		t.Fatalf("expected timeout degradation: %+v", res)
	}
}

func TestAssembleBudgetTooSmall(t *testing.T) {
	a := NewAssembler(WithBudgetPolicy(BudgetPolicy{Global: Budget{MaxTokens: 100, ReserveOutput: 100}}))
	sess := a.Session("s1")
	sess.Upsert(frag("f1", "chat", 1, 1, "content"))

	res, err := a.Assemble(context.Background(), Request{SessionID: "s1"})
	if !errors.Is(err, ErrBudgetTooSmall) {
		t.Fatalf("expected ErrBudgetTooSmall, got %v", err)
	}
	if !res.Degraded || res.Content != "" {
		t.Fatalf("budget-too-small must return empty degraded result: %+v", res)
	}
}

func TestAssembleSourceUnavailable(t *testing.T) {
	a := NewAssembler(WithBudgetPolicy(BudgetPolicy{Global: Budget{MaxTokens: 100}}))
	res, err := a.Assemble(context.Background(), Request{
		SessionID: "s1",
		Fragments: []Fragment{
			frag("f1", "doc", 1, 1, "可用内容"),
			{ID: "f2", Source: "broken-api", Unavailable: true},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Degraded || !hasReason(res, ErrSourceUnavailable.Error()) {
		t.Fatalf("expected source-unavailable degradation: %+v", res)
	}
	if !strings.Contains(res.Content, "可用内容") {
		t.Fatalf("available fragment must be kept: %q", res.Content)
	}
	if !hasAction(res, "f2", "source-unavailable") {
		t.Fatalf("expected source-unavailable decision: %+v", res.Decisions)
	}
}

func TestSourceVersionReplacement(t *testing.T) {
	a := NewAssembler(WithBudgetPolicy(BudgetPolicy{Global: Budget{MaxTokens: 1000}}))
	sess := a.Session("s1")
	sess.Upsert(frag("doc-v1", "doc", 1, 1, "旧版本内容"))

	res, err := a.Assemble(context.Background(), Request{
		SessionID: "s1",
		Fragments: []Fragment{frag("doc-v2", "doc", 1, 2, "新版本内容")},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(res.Content, "旧版本内容") {
		t.Fatalf("old version must be replaced: %q", res.Content)
	}
	if !strings.Contains(res.Content, "新版本内容") {
		t.Fatalf("new version must be used: %q", res.Content)
	}
	if !hasAction(res, "doc-v1", "replaced") {
		t.Fatalf("expected replaced decision: %+v", res.Decisions)
	}
}

func TestConflictResolutionDeterministic(t *testing.T) {
	a := NewAssembler(WithBudgetPolicy(BudgetPolicy{Global: Budget{MaxTokens: 1000}}))
	req := Request{
		SessionID: "s1",
		Fragments: []Fragment{
			{ID: "a1", Source: "source-b", Priority: 1, Version: 1, CreatedAt: base, Content: "低优先级说法", ConflictKey: "fact-x"},
			{ID: "a2", Source: "source-a", Priority: 5, Version: 1, CreatedAt: base, Content: "高优先级说法", ConflictKey: "fact-x"},
		},
	}
	res, err := a.Assemble(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(res.Content, "高优先级说法") || strings.Contains(res.Content, "低优先级说法") {
		t.Fatalf("higher priority must win: %q", res.Content)
	}
	if !hasAction(res, "a1", "conflict-lost") {
		t.Fatalf("expected conflict-lost decision: %+v", res.Decisions)
	}

	// 同优先级时时效新者胜，结果可复现。
	req2 := Request{
		SessionID: "s2",
		Fragments: []Fragment{
			{ID: "b1", Source: "s1", Priority: 1, Version: 1, CreatedAt: base, Content: "旧说法", ConflictKey: "fact-y"},
			{ID: "b2", Source: "s2", Priority: 1, Version: 1, CreatedAt: base.Add(time.Hour), Content: "新说法", ConflictKey: "fact-y"},
		},
	}
	res2, _ := a.Assemble(context.Background(), req2)
	if !strings.Contains(res2.Content, "新说法") {
		t.Fatalf("newer must win at equal priority: %q", res2.Content)
	}
}

func TestBudgetHierarchyAndReserve(t *testing.T) {
	a := NewAssembler(WithBudgetPolicy(BudgetPolicy{
		Global:  Budget{MaxTokens: 1000, ReserveOutput: 100},
		Session: Budget{MaxTokens: 500},
	}))
	sess := a.Session("s1")
	sess.Upsert(frag("f1", "chat", 1, 1, "x"))

	// 请求级覆盖会话级与全局。
	res, err := a.Assemble(context.Background(), Request{
		SessionID: "s1",
		Budget:    Budget{MaxTokens: 200, ReserveOutput: 50},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Budget.MaxTokens != 200 || res.Budget.ReserveOutput != 50 {
		t.Fatalf("request budget must win: %+v", res.Budget)
	}

	// 未声明请求级时会话级覆盖全局的 MaxTokens，ReserveOutput 落到全局。
	res2, _ := a.Assemble(context.Background(), Request{SessionID: "s1"})
	if res2.Budget.MaxTokens != 500 || res2.Budget.ReserveOutput != 100 {
		t.Fatalf("session should override global per-field: %+v", res2.Budget)
	}
	if res2.Budget.Input() != 400 {
		t.Fatalf("input budget must respect output reserve: %d", res2.Budget.Input())
	}
}

func TestDedupByID(t *testing.T) {
	a := NewAssembler(WithBudgetPolicy(BudgetPolicy{Global: Budget{MaxTokens: 1000}}))
	sess := a.Session("s1")
	sess.Upsert(frag("f1", "chat", 1, 1, "旧声明"))
	res, err := a.Assemble(context.Background(), Request{
		SessionID: "s1",
		Fragments: []Fragment{frag("f1", "chat", 1, 2, "新声明")},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(res.Content, "新声明") || strings.Contains(res.Content, "旧声明") {
		t.Fatalf("dedup must keep newer declaration: %q", res.Content)
	}
}
