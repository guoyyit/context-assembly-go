package assembly

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

const (
	defaultSummaryTimeout     = 5 * time.Second
	defaultSummaryTargetRatio = 0.25
	maxAssembleAttempts       = 3
	summaryPseudoID           = "__summary__"
)

// Assembler 在请求处理链路上执行上下文组装。
type Assembler struct {
	Store              *Store
	Cache              *Cache // 可为 nil，表示不缓存
	Counter            TokenCounter
	Summarizer         Summarizer // 可为 nil，表示不做长会话压缩
	DefaultBudget      Budget
	SummaryTimeout     time.Duration // 摘要生成的超时时间
	SummaryTargetRatio float64       // 摘要目标长度占输入预算的比例
}

// Request 是一次组装请求。
type Request struct {
	SessionID     string
	Fragments     []Fragment // 请求级片段，先并入会话再组装
	SessionBudget *Budget    // 会话级预算（可选）
	Budget        *Budget    // 请求级预算偏好（可选）
}

// NewAssembler 创建使用默认配置的 Assembler。
func NewAssembler(store *Store) *Assembler {
	if store == nil {
		store = NewStore()
	}
	return &Assembler{
		Store:              store,
		Cache:              NewCache(128),
		Counter:            DefaultTokenCounter{},
		Summarizer:         LocalSummarizer{},
		DefaultBudget:      Budget{MaxTokens: 8192, ReserveOutput: 1024},
		SummaryTimeout:     defaultSummaryTimeout,
		SummaryTargetRatio: defaultSummaryTargetRatio,
	}
}

func (a *Assembler) counter() TokenCounter {
	if a.Counter != nil {
		return a.Counter
	}
	return DefaultTokenCounter{}
}

func (a *Assembler) summaryTimeout() time.Duration {
	if a.SummaryTimeout > 0 {
		return a.SummaryTimeout
	}
	return defaultSummaryTimeout
}

func (a *Assembler) summaryTargetRatio() float64 {
	if a.SummaryTargetRatio > 0 && a.SummaryTargetRatio < 1 {
		return a.SummaryTargetRatio
	}
	return defaultSummaryTargetRatio
}

// fragKey 是去重键：同一来源同一 ID 的片段视为同一内容的不同版本。
func fragKey(f Fragment) string { return string(f.Source) + "\x1f" + f.ID }

// Assemble 执行一次上下文组装。
//
// 返回的 Result 一定不超预算；异常链路（来源不可用、摘要超时/失败、
// 预算极端压缩）会返回带 Degradation 的降级结果与可区分的错误。
func (a *Assembler) Assemble(ctx context.Context, req Request) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	store := a.Store
	if store == nil {
		store = NewStore()
	}
	sess := store.GetOrCreate(req.SessionID)

	// 1. 并入请求级片段；被同来源同 ID 新版本替换的旧版本记录在案。
	var superseded []Excluded
	for _, f := range req.Fragments {
		if old, replaced := sess.Upsert(f); replaced {
			superseded = append(superseded, Excluded{
				Fragment: old,
				Reason:   ReasonSuperseded,
				Detail:   fmt.Sprintf("replaced by %s/%s version %d", f.Source, f.ID, f.Version),
			})
		}
	}

	// 2. 解析层级预算；预算被极端压缩时给出可区分的降级结果。
	budget := BudgetLayers{Default: a.DefaultBudget, Session: req.SessionBudget, Request: req.Budget}.Resolve()
	if budget.InputLimit() <= 0 {
		snap := sess.Snapshot()
		res := Result{
			SessionID: sess.ID(),
			Version:   snap.Version,
			Budget:    budget,
			Degraded:  &Degradation{Kind: DegradedBudgetCompressed, Detail: "input budget is not positive"},
		}
		for _, f := range snap.Fragments {
			res.Excluded = append(res.Excluded, Excluded{Fragment: f, Reason: ReasonOverBudget, Detail: "no input budget available"})
		}
		res.Excluded = append(res.Excluded, superseded...)
		return res, ErrBudgetTooSmall
	}

	// 3. 组装循环：如需摘要则生成并原子提交后基于新版本重试。
	key := cacheKey{SessionID: sess.ID(), Budget: budget}
	var summarized []Excluded
	for attempt := 0; attempt < maxAssembleAttempts; attempt++ {
		snap := sess.Snapshot()
		key.Version = snap.Version
		if cached, ok := a.Cache.Get(key); ok {
			cached.FromCache = true
			return cached, nil
		}

		plan := a.plan(snap, budget, a.Summarizer != nil)
		if len(plan.summarize) > 0 {
			input := plan.summarize
			if plan.foldSummary != "" {
				pseudo := Fragment{ID: summaryPseudoID, Source: "__summary__", Content: plan.foldSummary}
				input = append([]Fragment{pseudo}, input...)
			}
			sctx, cancel := context.WithTimeout(ctx, a.summaryTimeout())
			text, serr := a.Summarizer.Summarize(sctx, input, plan.summaryTarget)
			cancel()
			if serr != nil {
				if ctx.Err() != nil {
					return Result{}, ctx.Err()
				}
				if errors.Is(serr, context.DeadlineExceeded) {
					res := a.degradedResult(snap, budget, Degradation{Kind: DegradedSummaryTimeout, Detail: serr.Error()})
					res.SessionID = sess.ID()
					res.Excluded = append(res.Excluded, superseded...)
					return res, ErrSummaryTimeout
				}
				res := a.degradedResult(snap, budget, Degradation{Kind: DegradedSummaryFailed, Detail: serr.Error()})
				res.SessionID = sess.ID()
				res.Excluded = append(res.Excluded, superseded...)
				return res, fmt.Errorf("%w: %v", ErrSummaryFailed, serr)
			}
			// 防御：摘要组件违约超目标时截断，保证不超预算。
			if n := a.counter().Count(text); n > plan.summaryTarget {
				text = truncateToTokens(text, plan.summaryTarget, a.counter())
			}
			ids := make([]string, 0, len(plan.summarize))
			for _, f := range plan.summarize {
				ids = append(ids, f.ID)
			}
			if sess.CommitSummary(snap.Version, SummaryState{Text: text, CoveredIDs: ids}, plan.summarize) {
				for _, f := range plan.summarize {
					summarized = append(summarized, Excluded{Fragment: f, Reason: ReasonSummarized, Detail: "covered by summary"})
				}
				continue // 提交成功，基于推进后的新版本重新组装
			}
			continue // 并发写入导致提交失败，基于新快照重试
		}

		res := plan.result
		res.SessionID = sess.ID()
		res.Excluded = append(res.Excluded, superseded...)
		res.Excluded = append(res.Excluded, summarized...)
		if res.Degraded == nil {
			a.Cache.Add(key, res)
		}
		return res, plan.err
	}

	// 并发冲突重试耗尽：不生成摘要，基于最新快照降级返回。
	snap := sess.Snapshot()
	res := a.degradedResult(snap, budget, Degradation{Kind: DegradedSummaryFailed, Detail: "summary commit kept conflicting with concurrent writes"})
	res.SessionID = sess.ID()
	res.Excluded = append(res.Excluded, superseded...)
	return res, ErrSummaryFailed
}

// assemblyPlan 是一次组装的计划：要么直接产出结果，要么要求先做摘要。
type assemblyPlan struct {
	result        Result
	summarize     []Fragment // 需要并入摘要的片段（按时间从旧到新）
	foldSummary   string     // 需要折叠进新摘要的旧摘要文本
	summaryTarget int        // 新摘要的 token 目标
	err           error      // 伴随结果的可区分错误
}

// plan 基于快照制定组装计划。allowSummary 为 false 时不做摘要规划。
func (a *Assembler) plan(snap Snapshot, budget Budget, allowSummary bool) assemblyPlan {
	c := a.counter()
	limit := budget.InputLimit()

	var p assemblyPlan
	p.result = Result{Version: snap.Version, Budget: budget}

	// 1. 分离不可用来源。
	var available []Fragment
	anyUnavailable, pinnedUnavailable := false, false
	for _, f := range snap.Fragments {
		if f.Unavailable {
			anyUnavailable = true
			if f.Pinned {
				pinnedUnavailable = true
			}
			p.result.Excluded = append(p.result.Excluded, Excluded{
				Fragment: f, Reason: ReasonSourceUnavailable, Detail: "source marked unavailable",
			})
			continue
		}
		available = append(available, f)
	}

	// 2. 去重：同一来源同一 ID 保留最高版本（防御性，写入侧已保证）。
	candidates := dedupFragments(available, &p.result)

	// 3. 来源冲突处理：同一 ConflictGroup 内按确定性规则只保留一个。
	candidates = resolveConflicts(candidates, &p.result)

	// 4. 预算内尝试全部保留。
	summaryTokens := 0
	if snap.Summary != nil && snap.Summary.Text != "" {
		p.result.Summary = snap.Summary.Text
		summaryTokens = c.Count(snap.Summary.Text)
	}
	total := summaryTokens
	for _, f := range candidates {
		total += f.EffectiveTokens(c)
	}
	if total <= limit {
		p.result.Fragments = presentationOrder(candidates)
		p.result.TokensUsed = total
		applyUnavailableDegradation(&p, anyUnavailable, pinnedUnavailable)
		return p
	}

	// 5. 超预算：规划摘要（仅摘要最旧的连续前缀，保证与保留原文语义衔接）。
	if allowSummary {
		if sp, ok := a.planSummary(candidates, snap.Summary, limit, c); ok {
			return sp
		}
	}

	// 6. 无法摘要或摘要也救不回来：按优先级贪心装入，其余记为超预算。
	summaryText := ""
	if snap.Summary != nil {
		summaryText = snap.Summary.Text
	}
	summaryTruncated := false
	if summaryTokens > limit {
		// 极端预算压缩：旧摘要自身已超预算，截断到预算内，绝不返回超预算内容。
		summaryText = truncateToTokens(summaryText, limit, c)
		summaryTokens = c.Count(summaryText)
		summaryTruncated = true
	}
	prior := p.result.Excluded
	p.result = greedyFit(candidates, summaryTokens, limit, c, snap)
	p.result.Budget = budget
	p.result.Summary = summaryText
	p.result.Excluded = append(p.result.Excluded, prior...)
	if summaryTruncated && p.result.Degraded == nil {
		p.result.Degraded = &Degradation{Kind: DegradedBudgetCompressed, Detail: "existing summary truncated to fit budget"}
	}
	if droppedPinned(p.result) {
		p.result.Degraded = &Degradation{Kind: DegradedBudgetCompressed, Detail: "pinned fragment does not fit in budget"}
		p.err = ErrBudgetTooSmall
	}
	applyUnavailableDegradation(&p, anyUnavailable, pinnedUnavailable)
	return p
}

// planSummary 计算摘要计划：找到最短的"最旧连续前缀"，
// 使剩余片段加上摘要后不超过预算。优先让摘要拿到完整的目标预算
// （输入预算 × SummaryTargetRatio），保证摘要信息量；仅在完整目标
// 放不下时才收缩目标。
func (a *Assembler) planSummary(candidates []Fragment, old *SummaryState, limit int, c TokenCounter) (assemblyPlan, bool) {
	var pinned, summarizable []Fragment
	for _, f := range candidates {
		if f.Pinned {
			pinned = append(pinned, f)
		} else {
			summarizable = append(summarizable, f)
		}
	}
	// 摘要只覆盖最旧的连续前缀：按时间升序（并列按 ID）排列。
	sort.Slice(summarizable, func(i, j int) bool {
		a, b := summarizable[i], summarizable[j]
		if !a.Timestamp.Equal(b.Timestamp) {
			return a.Timestamp.Before(b.Timestamp)
		}
		return a.ID < b.ID
	})

	rest := 0
	for _, f := range pinned {
		rest += f.EffectiveTokens(c)
	}
	for _, f := range summarizable {
		rest += f.EffectiveTokens(c)
	}

	target := int(float64(limit) * a.summaryTargetRatio())
	if target < 1 {
		target = 1
	}
	fold := ""
	if old != nil {
		fold = old.Text
	}
	// 首选：最小的最旧前缀，使摘要能拿到完整目标预算。
	for k := 1; k <= len(summarizable); k++ {
		rest -= summarizable[k-1].EffectiveTokens(c)
		if rest+target <= limit {
			return assemblyPlan{summarize: summarizable[:k], summaryTarget: target, foldSummary: fold}, true
		}
	}
	// 兜底：摘要全部非 Pinned 片段，目标收缩为剩余空间。
	if len(summarizable) > 0 {
		if shrunk := limit - rest; shrunk >= 1 {
			return assemblyPlan{summarize: summarizable, summaryTarget: shrunk, foldSummary: fold}, true
		}
	}
	return assemblyPlan{}, false
}

// greedyFit 按确定性顺序（Pinned 优先，其后优先级降序、时间降序、ID 升序）
// 在预算内贪心装入片段，装不下的记为超预算。
func greedyFit(candidates []Fragment, summaryTokens, limit int, c TokenCounter, snap Snapshot) Result {
	res := Result{Version: snap.Version}
	ordered := append([]Fragment(nil), candidates...)
	sort.Slice(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if a.Pinned != b.Pinned {
			return a.Pinned
		}
		if a.Priority != b.Priority {
			return a.Priority > b.Priority
		}
		if !a.Timestamp.Equal(b.Timestamp) {
			return a.Timestamp.After(b.Timestamp)
		}
		return a.ID < b.ID
	})
	remaining := limit - summaryTokens
	if remaining < 0 {
		remaining = 0
	}
	used := summaryTokens
	var included []Fragment
	for _, f := range ordered {
		t := f.EffectiveTokens(c)
		if t <= remaining {
			included = append(included, f)
			remaining -= t
			used += t
		} else {
			res.Excluded = append(res.Excluded, Excluded{
				Fragment: f, Reason: ReasonOverBudget,
				Detail: fmt.Sprintf("needs %d tokens, only %d left", t, remaining),
			})
		}
	}
	res.Fragments = presentationOrder(included)
	res.TokensUsed = used
	return res
}

// degradedResult 在不生成摘要的前提下给出降级结果，并附上降级原因。
// 已有更基础的降级（预算压缩、来源不可用）时保留原降级类型。
func (a *Assembler) degradedResult(snap Snapshot, budget Budget, deg Degradation) Result {
	p := a.plan(snap, budget, false)
	res := p.result
	if res.Degraded == nil {
		res.Degraded = &deg
	} else {
		res.Degraded.Detail += "; " + deg.Detail
	}
	return res
}

// applyUnavailableDegradation 在存在不可用来源时标记降级；
// 关键约束（Pinned）来源不可用时返回可区分的错误。
func applyUnavailableDegradation(p *assemblyPlan, anyUnavailable, pinnedUnavailable bool) {
	if !anyUnavailable {
		return
	}
	if p.result.Degraded == nil {
		p.result.Degraded = &Degradation{Kind: DegradedSourceUnavailable, Detail: "one or more sources unavailable"}
	}
	if pinnedUnavailable && p.err == nil {
		p.err = ErrSourceUnavailable
	}
}

// dedupFragments 按 (Source, ID) 去重，保留最高版本；
// 并列时取时间较新、再并列取 ID 较大者，保证确定性。
func dedupFragments(frags []Fragment, res *Result) []Fragment {
	best := make(map[string]Fragment)
	var order []string
	for _, f := range frags {
		k := fragKey(f)
		cur, ok := best[k]
		if !ok {
			best[k] = f
			order = append(order, k)
			continue
		}
		winner, loser := pickVersion(cur, f)
		best[k] = winner
		res.Excluded = append(res.Excluded, Excluded{
			Fragment: loser, Reason: ReasonSuperseded,
			Detail: fmt.Sprintf("superseded by %s/%s version %d", winner.Source, winner.ID, winner.Version),
		})
	}
	out := make([]Fragment, 0, len(order))
	for _, k := range order {
		out = append(out, best[k])
	}
	return out
}

// pickVersion 在同一 (Source, ID) 的两个版本中确定性地选出保留者。
func pickVersion(a, b Fragment) (winner, loser Fragment) {
	if a.Version != b.Version {
		if a.Version > b.Version {
			return a, b
		}
		return b, a
	}
	if !a.Timestamp.Equal(b.Timestamp) {
		if a.Timestamp.After(b.Timestamp) {
			return a, b
		}
		return b, a
	}
	if a.ID >= b.ID {
		return a, b
	}
	return b, a
}

// resolveConflicts 处理 ConflictGroup 互斥：组内按
// 优先级降序、时间降序、来源升序、ID 升序 的确定性规则取舍。
func resolveConflicts(frags []Fragment, res *Result) []Fragment {
	groups := make(map[string][]int)
	var kept []Fragment
	for i, f := range frags {
		if f.ConflictGroup == "" {
			kept = append(kept, f)
			continue
		}
		groups[f.ConflictGroup] = append(groups[f.ConflictGroup], i)
	}
	if len(groups) == 0 {
		return frags
	}
	// 按组名排序遍历，保证结果可复现。
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		idxs := groups[k]
		winner := idxs[0]
		for _, idx := range idxs[1:] {
			if conflictWins(frags[idx], frags[winner]) {
				winner = idx
			}
		}
		for _, idx := range idxs {
			if idx == winner {
				kept = append(kept, frags[idx])
				continue
			}
			res.Excluded = append(res.Excluded, Excluded{
				Fragment: frags[idx], Reason: ReasonConflictLost,
				Detail: fmt.Sprintf("conflict group %q: lost to %s/%s", frags[idx].ConflictGroup, frags[winner].Source, frags[winner].ID),
			})
		}
	}
	return kept
}

// conflictWins 判定 challenger 是否在冲突组中胜过 current。
func conflictWins(challenger, current Fragment) bool {
	if challenger.Priority != current.Priority {
		return challenger.Priority > current.Priority
	}
	if !challenger.Timestamp.Equal(current.Timestamp) {
		return challenger.Timestamp.After(current.Timestamp)
	}
	if challenger.Source != current.Source {
		return challenger.Source < current.Source
	}
	return challenger.ID < current.ID
}

// presentationOrder 输出顺序：按时间升序（并列按 ID），保证可复现。
func presentationOrder(frags []Fragment) []Fragment {
	out := append([]Fragment(nil), frags...)
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if !a.Timestamp.Equal(b.Timestamp) {
			return a.Timestamp.Before(b.Timestamp)
		}
		return a.ID < b.ID
	})
	return out
}

// droppedPinned 判断结果中是否有 Pinned 片段因预算被丢弃。
func droppedPinned(res Result) bool {
	for _, ex := range res.Excluded {
		if ex.Reason == ReasonOverBudget && ex.Fragment.Pinned {
			return true
		}
	}
	return false
}
