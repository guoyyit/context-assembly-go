package assembly

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
	"sync"
)

// Request 描述一次推理调用的组装请求。
type Request struct {
	SessionID string
	Fragments []Fragment // 本次请求随附的实时素材
	Budget    Budget     // 请求级预算偏好，零值字段表示不覆盖
}

// Decision 记录一次取舍的原因，供调用方解释组装结果。
type Decision struct {
	FragmentID string
	Action     string // included / evicted / summarized / replaced / conflict-lost / source-unavailable
	Reason     string
}

// Result 是组装输出。
type Result struct {
	Content        string
	TokensUsed     int
	Budget         Budget
	Version        uint64 // 结果对应的会话完整版本
	SummaryUsed    bool
	Degraded       bool
	DegradeReasons []string
	Decisions      []Decision
}

// Assembler 在请求处理链路上执行上下文组装。
type Assembler struct {
	mu       sync.Mutex
	sessions map[string]*Session
	policy   BudgetPolicy
	summ     Summarizer
	cache    *Cache
}

// Option 配置 Assembler。
type Option func(*Assembler)

// WithBudgetPolicy 设置全局与会话级预算。
func WithBudgetPolicy(p BudgetPolicy) Option {
	return func(a *Assembler) { a.policy = p }
}

// WithSummarizer 替换本地摘要组件。
func WithSummarizer(s Summarizer) Option {
	return func(a *Assembler) { a.summ = s }
}

// WithCache 设置结果缓存。
func WithCache(c *Cache) Option {
	return func(a *Assembler) { a.cache = c }
}

// NewAssembler 创建 Assembler。
func NewAssembler(opts ...Option) *Assembler {
	a := &Assembler{
		sessions: make(map[string]*Session),
		summ:     ExtractiveSummarizer{},
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// Session 返回指定会话，不存在则创建。
func (a *Assembler) Session(id string) *Session {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sessions[id]
	if !ok {
		s = NewSession(id)
		a.sessions[id] = s
	}
	return s
}

// Assemble 执行一次上下文组装。
// 返回的 Result 始终不超预算；降级时 Degraded 为 true 且原因可区分。
func (a *Assembler) Assemble(ctx context.Context, req Request) (Result, error) {
	budget := BudgetPolicy{
		Global:  a.policy.Global,
		Session: a.policy.Session,
		Request: req.Budget,
	}.Resolve()

	sess := a.Session(req.SessionID)
	snap := sess.Snapshot()

	res := Result{Budget: budget, Version: snap.Version}

	// 预算被极端压缩：给出可区分的降级结果，不返回任何内容。
	if budget.Input() <= 0 {
		res.Degraded = true
		res.DegradeReasons = append(res.DegradeReasons, ErrBudgetTooSmall.Error())
		return res, ErrBudgetTooSmall
	}

	// 缓存键：会话版本 + 摘要版本 + 预算 + 请求素材哈希。
	// 任一变化都会产生新 key，旧结果不会再被命中。
	key := CacheKey{
		SessionID:      snap.SessionID,
		SessionVersion: snap.Version,
		SummaryVersion: snap.Summary.Version,
		Budget:         budget,
		RequestHash:    hashFragments(req.Fragments),
	}
	if a.cache.Enabled() {
		if cached, ok := a.cache.Get(key); ok {
			return cached, nil
		}
	}

	// 合并会话片段与请求片段：去重、来源版本替换、矛盾取舍。
	merged, mergeDecisions, unavailable := mergeFragments(snap.Fragments, req.Fragments)
	res.Decisions = append(res.Decisions, mergeDecisions...)
	if unavailable {
		res.Degraded = true
		res.DegradeReasons = append(res.DegradeReasons, ErrSourceUnavailable.Error())
	}

	budgetIn := budget.Input()

	summaryText := snap.Summary.Text
	summaryTokens := 0
	if summaryText != "" {
		summaryTokens = EstimateTokens(summaryText)
	}

	total := summaryTokens
	for _, f := range merged {
		total += f.TokenCount()
	}

	var content string
	summaryUsed := summaryText != ""

	switch {
	case total <= budgetIn:
		// 全部保留。
		for _, f := range merged {
			res.Decisions = append(res.Decisions, Decision{FragmentID: f.ID, Action: "included", Reason: "within budget"})
		}
		content = joinContent(summaryText, merged)

	default:
		// 超预算：为摘要预留 1/4，其余按重要性保留。
		summaryBudget := budgetIn / 4
		if summaryBudget < 1 {
			summaryBudget = 1
		}
		keepBudget := budgetIn - summaryBudget

		kept, evicted := greedyKeep(merged, keepBudget)

		// 被挤出的片段连同旧摘要一起压缩。
		toSummarize := make([]Fragment, 0, len(evicted)+1)
		if summaryText != "" {
			toSummarize = append(toSummarize, Fragment{ID: "prev-summary", Content: summaryText, Tokens: summaryTokens})
		}
		toSummarize = append(toSummarize, evicted...)

		newSummary, err := a.summ.Summarize(ctx, toSummarize, summaryBudget)
		if err != nil {
			// 摘要失败/超时/取消：降级为纯按预算截断。
			// 不提交摘要，会话状态不受半更新影响。
			res.Degraded = true
			reason := ErrSummarizeFailed
			if errors.Is(err, ErrSummarizeTimeout) || ctx.Err() != nil {
				reason = ErrSummarizeTimeout
			}
			res.DegradeReasons = append(res.DegradeReasons, reason.Error())
			kept, evicted = greedyKeep(merged, budgetIn)
			for _, f := range kept {
				res.Decisions = append(res.Decisions, Decision{FragmentID: f.ID, Action: "included", Reason: "kept by priority/recency"})
			}
			for _, f := range evicted {
				res.Decisions = append(res.Decisions, Decision{FragmentID: f.ID, Action: "evicted", Reason: reason.Error()})
			}
			content = joinContent("", kept)
			summaryUsed = false
		} else {
			// 自定义摘要器可能超限，强制截断到摘要预算。
			if EstimateTokens(newSummary) > summaryBudget {
				newSummary = truncateToTokens(newSummary, summaryBudget)
			}
			for _, f := range kept {
				res.Decisions = append(res.Decisions, Decision{FragmentID: f.ID, Action: "included", Reason: "kept by priority/recency"})
			}
			for _, f := range evicted {
				res.Decisions = append(res.Decisions, Decision{FragmentID: f.ID, Action: "summarized", Reason: "compressed into summary"})
			}
			// 提交摘要推进并原子移除被覆盖的片段；
			// 并发变更导致版本漂移时放弃提交，
			// 本次结果仍对应 snap.Version 的完整状态。
			evictedIDs := make([]string, 0, len(evicted))
			for _, f := range evicted {
				evictedIDs = append(evictedIDs, f.ID)
			}
			if !sess.CommitSummary(snap.Version, Summary{Text: newSummary, Tokens: EstimateTokens(newSummary)}, evictedIDs...) {
				res.Decisions = append(res.Decisions, Decision{Action: "summary-commit-skipped", Reason: "concurrent session update"})
			} else if a.cache.Enabled() {
				// 摘要推进使旧版本结果全部失效。
				a.cache.InvalidateSession(snap.SessionID)
			}
			content = joinContent(newSummary, kept)
			summaryUsed = true
		}
	}

	// 硬性保证：返回内容绝不超预算。
	if t := EstimateTokens(content); t > budgetIn {
		content = truncateToTokens(content, budgetIn)
		res.Degraded = true
		res.DegradeReasons = append(res.DegradeReasons, "content truncated to fit budget")
	}

	res.Content = content
	res.TokensUsed = EstimateTokens(content)
	res.SummaryUsed = summaryUsed

	// 降级结果不缓存，避免临时故障被固化。
	if !res.Degraded && a.cache.Enabled() {
		a.cache.Put(key, res)
	}
	return res, nil
}

// mergeFragments 合并会话片段与请求片段：
// 同 ID 去重（高版本优先，同版本请求侧优先），
// 同来源高版本替换低版本，同 ConflictKey 的矛盾片段按确定性规则取舍。
func mergeFragments(sessionFrags, reqFrags []Fragment) (merged []Fragment, decisions []Decision, unavailable bool) {
	byID := make(map[string]Fragment)
	fromReq := make(map[string]bool)
	for _, f := range sessionFrags {
		byID[f.ID] = f
	}
	for _, f := range reqFrags {
		fromReq[f.ID] = true
		if old, ok := byID[f.ID]; ok {
			if old.Version > f.Version {
				decisions = append(decisions, Decision{FragmentID: f.ID, Action: "replaced", Reason: "stale version ignored"})
				continue
			}
			decisions = append(decisions, Decision{FragmentID: old.ID, Action: "replaced", Reason: "newer declaration replaces old"})
		}
		byID[f.ID] = f
	}

	// 过滤来源不可用的片段。
	candidates := make([]Fragment, 0, len(byID))
	for _, f := range byID {
		if f.Unavailable {
			unavailable = true
			decisions = append(decisions, Decision{FragmentID: f.ID, Action: "source-unavailable", Reason: ErrSourceUnavailable.Error()})
			continue
		}
		candidates = append(candidates, f)
	}

	// 同来源：仅当存在更高版本时，低版本才被替换；同版本片段共存。
	maxVer := make(map[string]uint64)
	for _, f := range candidates {
		if f.Source == "" {
			continue
		}
		if v, ok := maxVer[f.Source]; !ok || f.Version > v {
			maxVer[f.Source] = f.Version
		}
	}
	var versioned []Fragment
	for _, f := range candidates {
		if f.Source != "" && f.Version < maxVer[f.Source] {
			decisions = append(decisions, Decision{
				FragmentID: f.ID,
				Action:     "replaced",
				Reason:     fmt.Sprintf("superseded by newer version %d of source %s", maxVer[f.Source], f.Source),
			})
			continue
		}
		versioned = append(versioned, f)
	}

	// 同 ConflictKey 的矛盾片段：优先级高者胜，其次时效新者，再按来源与 ID 字典序。
	byConflict := make(map[string][]Fragment)
	for _, f := range versioned {
		if f.ConflictKey != "" {
			byConflict[f.ConflictKey] = append(byConflict[f.ConflictKey], f)
		}
	}
	lost := make(map[string]string) // 失败片段 ID -> 胜者 ID
	for _, group := range byConflict {
		winner := group[0]
		for _, f := range group[1:] {
			if conflictWins(f, winner) {
				winner = f
			}
		}
		for _, f := range group {
			if f.ID != winner.ID {
				lost[f.ID] = winner.ID
			}
		}
	}
	for _, f := range versioned {
		if winnerID, ok := lost[f.ID]; ok {
			decisions = append(decisions, Decision{
				FragmentID: f.ID,
				Action:     "conflict-lost",
				Reason:     fmt.Sprintf("contradicts %s; lost by priority/recency/source order", winnerID),
			})
			continue
		}
		merged = append(merged, f)
	}

	// 输出按时间线排序，保证可复现。
	sort.Slice(merged, func(i, j int) bool {
		if !merged[i].CreatedAt.Equal(merged[j].CreatedAt) {
			return merged[i].CreatedAt.Before(merged[j].CreatedAt)
		}
		return merged[i].ID < merged[j].ID
	})
	return merged, decisions, unavailable
}

// conflictWins 判定 f 是否胜过 cur：优先级 > 时效 > 来源与 ID 字典序。
func conflictWins(f, cur Fragment) bool {
	if f.Priority != cur.Priority {
		return f.Priority > cur.Priority
	}
	if !f.CreatedAt.Equal(cur.CreatedAt) {
		return f.CreatedAt.After(cur.CreatedAt)
	}
	if f.Source != cur.Source {
		return f.Source < cur.Source
	}
	return f.ID < cur.ID
}

// greedyKeep 按 优先级降序、时效降序、ID 升序 贪心保留，返回保留与挤出的片段，
// 两个集合都按时间线排序。
func greedyKeep(frags []Fragment, budget int) (kept, evicted []Fragment) {
	order := make([]Fragment, len(frags))
	copy(order, frags)
	sort.Slice(order, func(i, j int) bool {
		if order[i].Priority != order[j].Priority {
			return order[i].Priority > order[j].Priority
		}
		if !order[i].CreatedAt.Equal(order[j].CreatedAt) {
			return order[i].CreatedAt.After(order[j].CreatedAt)
		}
		return order[i].ID < order[j].ID
	})
	fits := make(map[string]bool)
	used := 0
	for _, f := range order {
		if used+f.TokenCount() <= budget {
			fits[f.ID] = true
			used += f.TokenCount()
		}
	}
	for _, f := range frags { // frags 已按时间线排序
		if fits[f.ID] {
			kept = append(kept, f)
		} else {
			evicted = append(evicted, f)
		}
	}
	return kept, evicted
}

// joinContent 把摘要与保留片段拼成最终上下文，摘要在前以衔接语义。
func joinContent(summary string, frags []Fragment) string {
	var b strings.Builder
	if summary != "" {
		b.WriteString(summary)
	}
	for _, f := range frags {
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(f.Content)
	}
	return b.String()
}

// hashFragments 计算请求素材的确定性哈希，用于缓存键。
func hashFragments(frags []Fragment) uint64 {
	sorted := make([]Fragment, len(frags))
	copy(sorted, frags)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	h := fnv.New64a()
	for _, f := range sorted {
		fmt.Fprintf(h, "%s|%s|%d|%d|%t|%s\n", f.ID, f.Source, f.Version, f.Priority, f.Unavailable, f.Content)
	}
	return h.Sum64()
}
