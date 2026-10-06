package assembly

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// runeCounter 以 1 字符 = 1 token 的规则让测试中的预算易于推算。
func runeCounter() TokenCounter {
	return TokenCounterFunc(func(s string) int { return len([]rune(s)) })
}

func newTestAssembler(store *Store, budget Budget) *Assembler {
	return &Assembler{
		Store:              store,
		Cache:              NewCache(16),
		Counter:            runeCounter(),
		Summarizer:         LocalSummarizer{Counter: runeCounter()},
		DefaultBudget:      budget,
		SummaryTimeout:     time.Second,
		SummaryTargetRatio: 0.25,
	}
}

func mkFrag(id string, source SourceID, version uint64, prio int, sec int64, content string) Fragment {
	return Fragment{
		ID:        id,
		Source:    source,
		Version:   version,
		Priority:  prio,
		Timestamp: time.Unix(sec, 0),
		Content:   content,
	}
}

func includedIDs(res Result) map[string]bool {
	ids := make(map[string]bool)
	for _, f := range res.Fragments {
		ids[f.ID] = true
	}
	return ids
}

func excludedByReason(res Result, reason ExcludeReason) []Excluded {
	var out []Excluded
	for _, ex := range res.Excluded {
		if ex.Reason == reason {
			out = append(out, ex)
		}
	}
	return out
}

func TestAssembleWithinBudget(t *testing.T) {
	a := newTestAssembler(NewStore(), Budget{MaxTokens: 100, ReserveOutput: 10})
	req := Request{SessionID: "s", Fragments: []Fragment{
		mkFrag("m3", "chat", 1, 1, 3, strings.Repeat("c", 10)),
		mkFrag("m1", "chat", 1, 1, 1, strings.Repeat("a", 10)),
		mkFrag("m2", "chat", 1, 1, 2, strings.Repeat("b", 10)),
	}}
	res, err := a.Assemble(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Degraded != nil {
		t.Fatalf("不应降级: %+v", res.Degraded)
	}
	if len(res.Fragments) != 3 || len(res.Excluded) != 0 {
		t.Fatalf("应全部入选, got %+v", res)
	}
	// 输出按时间升序，保证可复现。
	if res.Fragments[0].ID != "m1" || res.Fragments[2].ID != "m3" {
		t.Fatalf("输出顺序应为时间升序: %v", includedIDs(res))
	}
	if res.TokensUsed != 30 {
		t.Fatalf("TokensUsed = %d, want 30", res.TokensUsed)
	}
	if res.TokensUsed > res.Budget.InputLimit() {
		t.Fatal("结果不得超过输入预算")
	}
}

func TestAssembleOverBudgetNoSummarizer(t *testing.T) {
	a := newTestAssembler(NewStore(), Budget{MaxTokens: 40, ReserveOutput: 5})
	a.Summarizer = nil // 不做压缩，直接按预算取舍
	pinned := mkFrag("pin", "sys", 1, 0, 1, strings.Repeat("p", 10))
	pinned.Pinned = true
	res, err := a.Assemble(context.Background(), Request{SessionID: "s", Fragments: []Fragment{
		pinned,
		mkFrag("high", "chat", 1, 5, 2, strings.Repeat("h", 10)),
		mkFrag("low", "chat", 1, 1, 3, strings.Repeat("l", 20)),
	}})
	if err != nil {
		t.Fatal(err)
	}
	ids := includedIDs(res)
	if !ids["pin"] || !ids["high"] {
		t.Fatal("Pinned 与高优先级片段应优先保留")
	}
	if ids["low"] {
		t.Fatal("低优先级片段应被预算挤出")
	}
	if res.TokensUsed != 20 || res.TokensUsed > res.Budget.InputLimit() {
		t.Fatalf("TokensUsed = %d, 应不超预算 35", res.TokensUsed)
	}
	if n := len(excludedByReason(res, ReasonOverBudget)); n != 1 {
		t.Fatalf("应有 1 条超预算排除记录, got %d", n)
	}
}

func TestAssemblePinnedOverBudgetDegrades(t *testing.T) {
	a := newTestAssembler(NewStore(), Budget{MaxTokens: 40, ReserveOutput: 5})
	pinned := mkFrag("pin", "sys", 1, 0, 1, strings.Repeat("p", 50)) // 超过输入预算 35
	pinned.Pinned = true
	res, err := a.Assemble(context.Background(), Request{SessionID: "s", Fragments: []Fragment{pinned}})
	if !errors.Is(err, ErrBudgetTooSmall) {
		t.Fatalf("关键约束装不下应返回 ErrBudgetTooSmall, got %v", err)
	}
	if res.Degraded == nil || res.Degraded.Kind != DegradedBudgetCompressed {
		t.Fatalf("降级类型应为 budget-compressed, got %+v", res.Degraded)
	}
	if res.TokensUsed > res.Budget.InputLimit() {
		t.Fatal("降级结果也不得超出预算")
	}
}

func TestAssembleTriggersSummary(t *testing.T) {
	store := NewStore()
	a := newTestAssembler(store, Budget{MaxTokens: 220, ReserveOutput: 20}) // 输入预算 200
	var frags []Fragment
	for i := 1; i <= 10; i++ {
		frags = append(frags, mkFrag(fmt.Sprintf("m%d", i), "chat", 1, 1, int64(i), strings.Repeat("x", 30)))
	}
	res, err := a.Assemble(context.Background(), Request{SessionID: "s", Fragments: frags})
	if err != nil {
		t.Fatal(err)
	}
	if res.Degraded != nil {
		t.Fatalf("摘要应成功, 不应降级: %+v", res.Degraded)
	}
	if res.Summary == "" {
		t.Fatal("超预算的长会话应生成摘要")
	}
	if res.TokensUsed > res.Budget.InputLimit() {
		t.Fatalf("结果超预算: %d > %d", res.TokensUsed, res.Budget.InputLimit())
	}
	// 最旧的连续前缀被并入摘要，较新的原文保留。
	summarized := excludedByReason(res, ReasonSummarized)
	if len(summarized) == 0 {
		t.Fatal("应有片段被标记为已并入摘要")
	}
	ids := includedIDs(res)
	for _, ex := range summarized {
		if ids[ex.Fragment.ID] {
			t.Fatalf("片段 %s 不应同时出现在原文与摘要中", ex.Fragment.ID)
		}
	}
	if !ids["m10"] {
		t.Fatal("最新的消息应保留原文")
	}
	// 会话状态：摘要已提交，被覆盖片段已移除。
	snap := store.GetOrCreate("s").Snapshot()
	if snap.Summary == nil || snap.Summary.Version != 1 {
		t.Fatalf("会话应持有版本 1 的摘要, got %+v", snap.Summary)
	}
	if len(snap.Fragments)+len(snap.Summary.CoveredIDs) != 10 {
		t.Fatal("保留片段与被摘要覆盖片段之和应等于原始片段数")
	}

	// 第二次相同请求：版本未变，应命中缓存。
	res2, err := a.Assemble(context.Background(), Request{SessionID: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if !res2.FromCache {
		t.Fatal("状态未变时应命中缓存")
	}
	if res2.Summary != res.Summary {
		t.Fatal("缓存结果应与原始结果一致")
	}
}

// failingSummarizer 总是失败的摘要组件。
type failingSummarizer struct{ err error }

func (f failingSummarizer) Summarize(context.Context, []Fragment, int) (string, error) {
	return "", f.err
}

// blockingSummarizer 阻塞直到 ctx 取消的摘要组件。
type blockingSummarizer struct{}

func (blockingSummarizer) Summarize(ctx context.Context, _ []Fragment, _ int) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

func overBudgetRequest() Request {
	var frags []Fragment
	for i := 1; i <= 5; i++ {
		frags = append(frags, mkFrag(fmt.Sprintf("m%d", i), "chat", 1, 1, int64(i), strings.Repeat("x", 30)))
	}
	return Request{SessionID: "s", Fragments: frags}
}

func TestSummaryFailureLeavesStateIntact(t *testing.T) {
	store := NewStore()
	a := newTestAssembler(store, Budget{MaxTokens: 100, ReserveOutput: 10}) // 输入预算 90
	a.Summarizer = failingSummarizer{err: errors.New("boom")}

	res, err := a.Assemble(context.Background(), overBudgetRequest())
	if !errors.Is(err, ErrSummaryFailed) {
		t.Fatalf("应返回 ErrSummaryFailed, got %v", err)
	}
	if res.Degraded == nil || res.Degraded.Kind != DegradedSummaryFailed {
		t.Fatalf("降级类型应为 summary-failed, got %+v", res.Degraded)
	}
	if res.TokensUsed > res.Budget.InputLimit() {
		t.Fatal("降级结果不得超出预算")
	}
	// 状态不得留下半更新：无摘要、片段完整。
	snap := store.GetOrCreate("s").Snapshot()
	if snap.Summary != nil {
		t.Fatal("摘要失败不得提交任何摘要状态")
	}
	if len(snap.Fragments) != 5 {
		t.Fatalf("片段不得丢失, got %d", len(snap.Fragments))
	}

	// 修复摘要组件后可恢复（降级结果未被缓存，会重新计算）。
	a.Summarizer = LocalSummarizer{Counter: runeCounter()}
	res2, err := a.Assemble(context.Background(), Request{SessionID: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if res2.Summary == "" || res2.FromCache {
		t.Fatal("恢复后应重新生成摘要，而非使用降级的缓存结果")
	}
}

func TestSummaryTimeout(t *testing.T) {
	store := NewStore()
	a := newTestAssembler(store, Budget{MaxTokens: 100, ReserveOutput: 10})
	a.Summarizer = blockingSummarizer{}
	a.SummaryTimeout = 50 * time.Millisecond

	res, err := a.Assemble(context.Background(), overBudgetRequest())
	if !errors.Is(err, ErrSummaryTimeout) {
		t.Fatalf("应返回 ErrSummaryTimeout, got %v", err)
	}
	if res.Degraded == nil || res.Degraded.Kind != DegradedSummaryTimeout {
		t.Fatalf("降级类型应为 summary-timeout, got %+v", res.Degraded)
	}
	if res.TokensUsed > res.Budget.InputLimit() {
		t.Fatal("超时降级结果不得超出预算")
	}
	if snap := store.GetOrCreate("s").Snapshot(); snap.Summary != nil || len(snap.Fragments) != 5 {
		t.Fatal("超时不得留下半更新状态")
	}
}

func TestSummaryCancelLeavesNoPartialState(t *testing.T) {
	store := NewStore()
	a := newTestAssembler(store, Budget{MaxTokens: 100, ReserveOutput: 10})
	a.Summarizer = blockingSummarizer{}
	a.SummaryTimeout = time.Second

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	before := store.GetOrCreate("s").Snapshot()
	_, err := a.Assemble(ctx, overBudgetRequest())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("请求取消应返回 context.Canceled, got %v", err)
	}
	after := store.GetOrCreate("s").Snapshot()
	if after.Summary != nil || len(after.Fragments) != 5 || after.Version == before.Version {
		t.Fatal("取消不得留下半更新状态")
	}
}

func TestSourceVersionReplacement(t *testing.T) {
	a := newTestAssembler(NewStore(), Budget{MaxTokens: 500, ReserveOutput: 10})
	v1 := mkFrag("profile", "user-profile", 1, 5, 1, "name=alice")
	v2 := mkFrag("profile", "user-profile", 2, 5, 2, "name=bob")

	if _, err := a.Assemble(context.Background(), Request{SessionID: "s", Fragments: []Fragment{v1}}); err != nil {
		t.Fatal(err)
	}
	res, err := a.Assemble(context.Background(), Request{SessionID: "s", Fragments: []Fragment{v2}})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range res.Fragments {
		if f.ID == "profile" && f.Content != "name=bob" {
			t.Fatalf("同来源新版本必须替换旧版本, got %q", f.Content)
		}
	}
	sup := excludedByReason(res, ReasonSuperseded)
	if len(sup) != 1 || sup[0].Fragment.Content != "name=alice" {
		t.Fatalf("应记录被替换的旧版本, got %+v", sup)
	}
}

func TestConflictResolution(t *testing.T) {
	a := newTestAssembler(NewStore(), Budget{MaxTokens: 500, ReserveOutput: 10})
	// 同一冲突组、不同来源、同优先级：时间较新者胜。
	fa := mkFrag("a", "wiki", 1, 3, 1, "answer=A")
	fa.ConflictGroup = "answer"
	fb := mkFrag("b", "manual", 1, 3, 2, "answer=B")
	fb.ConflictGroup = "answer"

	res, err := a.Assemble(context.Background(), Request{SessionID: "s", Fragments: []Fragment{fa, fb}})
	if err != nil {
		t.Fatal(err)
	}
	ids := includedIDs(res)
	if !ids["b"] || ids["a"] {
		t.Fatalf("时间较新的 b 应胜出, got %v", ids)
	}
	lost := excludedByReason(res, ReasonConflictLost)
	if len(lost) != 1 || lost[0].Fragment.ID != "a" {
		t.Fatalf("应记录冲突落选者, got %+v", lost)
	}

	// 优先级更高者胜，即使时间更旧。
	fc := mkFrag("c", "official", 1, 9, 1, "answer=C")
	fc.ConflictGroup = "answer"
	res2, err := a.Assemble(context.Background(), Request{SessionID: "s", Fragments: []Fragment{fc}})
	if err != nil {
		t.Fatal(err)
	}
	if !includedIDs(res2)["c"] || includedIDs(res2)["b"] {
		t.Fatal("高优先级来源应胜出")
	}
}

func TestSourceUnavailable(t *testing.T) {
	a := newTestAssembler(NewStore(), Budget{MaxTokens: 500, ReserveOutput: 10})
	down := mkFrag("critical", "policy", 1, 9, 1, "")
	down.Unavailable = true
	down.Pinned = true
	ok := mkFrag("note", "chat", 1, 1, 2, "hello")

	res, err := a.Assemble(context.Background(), Request{SessionID: "s", Fragments: []Fragment{down, ok}})
	if !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("关键来源不可用应返回 ErrSourceUnavailable, got %v", err)
	}
	if res.Degraded == nil || res.Degraded.Kind != DegradedSourceUnavailable {
		t.Fatalf("降级类型应为 source-unavailable, got %+v", res.Degraded)
	}
	if n := len(excludedByReason(res, ReasonSourceUnavailable)); n != 1 {
		t.Fatal("不可用来源必须有排除记录，不得静默丢弃")
	}
	if !includedIDs(res)["note"] {
		t.Fatal("可用来源的内容仍应正常组装")
	}
}

func TestExtremeBudgetCompression(t *testing.T) {
	a := newTestAssembler(NewStore(), Budget{MaxTokens: 100, ReserveOutput: 10})
	tight := Budget{MaxTokens: 10, ReserveOutput: 20} // 输入预算 <= 0
	res, err := a.Assemble(context.Background(), Request{
		SessionID: "s",
		Fragments: []Fragment{mkFrag("m1", "chat", 1, 1, 1, "hello")},
		Budget:    &tight,
	})
	if !errors.Is(err, ErrBudgetTooSmall) {
		t.Fatalf("应返回 ErrBudgetTooSmall, got %v", err)
	}
	if res.Degraded == nil || res.Degraded.Kind != DegradedBudgetCompressed {
		t.Fatalf("降级类型应为 budget-compressed, got %+v", res.Degraded)
	}
	if len(res.Fragments) != 0 || res.TokensUsed != 0 {
		t.Fatal("极端压缩下不得返回任何内容")
	}
}

func TestCacheInvalidationOnSourceUpdate(t *testing.T) {
	a := newTestAssembler(NewStore(), Budget{MaxTokens: 500, ReserveOutput: 10})
	v1 := mkFrag("doc", "kb", 1, 1, 1, "old content")
	req := Request{SessionID: "s", Fragments: []Fragment{v1}}

	res1, err := a.Assemble(context.Background(), req)
	if err != nil || res1.FromCache {
		t.Fatalf("首次应重新计算, err=%v cached=%v", err, res1.FromCache)
	}
	res2, err := a.Assemble(context.Background(), req)
	if err != nil || !res2.FromCache {
		t.Fatal("内容未变时应命中缓存")
	}

	// 来源内容变更（新版本）后，旧缓存结果不得再被使用。
	v2 := mkFrag("doc", "kb", 2, 1, 2, "new content")
	res3, err := a.Assemble(context.Background(), Request{SessionID: "s", Fragments: []Fragment{v2}})
	if err != nil {
		t.Fatal(err)
	}
	if res3.FromCache {
		t.Fatal("来源更新后不得使用旧缓存")
	}
	found := false
	for _, f := range res3.Fragments {
		if f.Content == "new content" {
			found = true
		}
		if f.Content == "old content" {
			t.Fatal("旧版本内容不得再出现")
		}
	}
	if !found {
		t.Fatal("新版本内容应被组装")
	}
}

func TestDeterministicAssembly(t *testing.T) {
	build := func() Result {
		a := newTestAssembler(NewStore(), Budget{MaxTokens: 60, ReserveOutput: 5})
		a.Summarizer = nil
		res, err := a.Assemble(context.Background(), Request{SessionID: "s", Fragments: []Fragment{
			mkFrag("m1", "chat", 1, 2, 1, strings.Repeat("a", 20)),
			mkFrag("m2", "chat", 1, 1, 2, strings.Repeat("b", 20)),
			mkFrag("m3", "chat", 1, 3, 3, strings.Repeat("c", 20)),
		}})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	r1, r2 := build(), build()
	if len(r1.Fragments) != len(r2.Fragments) || len(r1.Excluded) != len(r2.Excluded) {
		t.Fatal("相同输入的组装结果必须可复现")
	}
	for i := range r1.Fragments {
		if r1.Fragments[i].ID != r2.Fragments[i].ID {
			t.Fatal("入选片段顺序必须可复现")
		}
	}
	for i := range r1.Excluded {
		if r1.Excluded[i].Fragment.ID != r2.Excluded[i].Fragment.ID ||
			r1.Excluded[i].Reason != r2.Excluded[i].Reason {
			t.Fatal("排除记录必须可复现")
		}
	}
}

// recordingSummarizer 记录每次摘要调用的输入，用于验证摘要折叠行为。
type recordingSummarizer struct {
	inner Summarizer
	mu    sync.Mutex
	calls [][]Fragment
}

func (r *recordingSummarizer) Summarize(ctx context.Context, frags []Fragment, target int) (string, error) {
	r.mu.Lock()
	r.calls = append(r.calls, append([]Fragment(nil), frags...))
	r.mu.Unlock()
	return r.inner.Summarize(ctx, frags, target)
}

func (r *recordingSummarizer) callInputs() [][]Fragment {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]Fragment(nil), r.calls...)
}

func TestSummaryFoldsPreviousSummary(t *testing.T) {
	store := NewStore()
	a := newTestAssembler(store, Budget{MaxTokens: 220, ReserveOutput: 20}) // 输入预算 200
	rec := &recordingSummarizer{inner: LocalSummarizer{Counter: runeCounter()}}
	a.Summarizer = rec

	var frags []Fragment
	for i := 1; i <= 10; i++ {
		frags = append(frags, mkFrag(fmt.Sprintf("m%d", i), "chat", 1, 1, int64(i), strings.Repeat("x", 30)))
	}
	res1, err := a.Assemble(context.Background(), Request{SessionID: "s", Fragments: frags})
	if err != nil {
		t.Fatal(err)
	}
	if res1.Summary == "" {
		t.Fatal("第一轮应产生摘要")
	}

	// 追加更多消息，触发第二轮摘要；旧摘要应折叠进新摘要。
	var more []Fragment
	for i := 11; i <= 15; i++ {
		more = append(more, mkFrag(fmt.Sprintf("m%d", i), "chat", 1, 1, int64(i), strings.Repeat("y", 30)))
	}
	res2, err := a.Assemble(context.Background(), Request{SessionID: "s", Fragments: more})
	if err != nil {
		t.Fatal(err)
	}
	if res2.Summary == "" || res2.Summary == res1.Summary {
		t.Fatal("第二轮应产生推进后的新摘要")
	}

	calls := rec.callInputs()
	if len(calls) != 2 {
		t.Fatalf("应发生两次摘要, got %d", len(calls))
	}
	// 第二次摘要的输入必须包含旧摘要文本（伪片段），保证语义衔接。
	second := calls[1]
	if len(second) == 0 || second[0].ID != summaryPseudoID {
		t.Fatalf("第二次摘要输入应首先包含旧摘要, got %+v", second[0])
	}
	if second[0].Content != res1.Summary {
		t.Fatal("折叠的旧摘要文本应与第一轮摘要一致")
	}
	if got := store.GetOrCreate("s").Snapshot().Summary.Version; got != 2 {
		t.Fatalf("摘要版本应推进到 2, got %d", got)
	}
	if res2.TokensUsed > res2.Budget.InputLimit() {
		t.Fatal("摘要推进后结果仍不得超预算")
	}
}
