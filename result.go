package assembly

// DegradationKind 区分不同的降级原因。
type DegradationKind int

const (
	DegradedNone              DegradationKind = iota // 未降级
	DegradedSourceUnavailable                        // 来源不可用
	DegradedSummaryTimeout                           // 摘要生成超时
	DegradedSummaryFailed                            // 摘要生成失败
	DegradedBudgetCompressed                         // 预算被极端压缩
)

// String 返回降级原因的可读描述。
func (k DegradationKind) String() string {
	switch k {
	case DegradedNone:
		return "none"
	case DegradedSourceUnavailable:
		return "source-unavailable"
	case DegradedSummaryTimeout:
		return "summary-timeout"
	case DegradedSummaryFailed:
		return "summary-failed"
	case DegradedBudgetCompressed:
		return "budget-compressed"
	default:
		return "unknown"
	}
}

// Degradation 描述一次降级及其原因。
type Degradation struct {
	Kind   DegradationKind
	Detail string
}

// ExcludeReason 说明片段未入选的原因。
type ExcludeReason int

const (
	ReasonSuperseded        ExcludeReason = iota // 被同来源的新版本替换
	ReasonConflictLost                           // 来源冲突中按规则落选
	ReasonOverBudget                             // 预算不足
	ReasonSummarized                             // 已并入摘要
	ReasonSourceUnavailable                      // 来源不可用
)

// String 返回排除原因的可读描述。
func (r ExcludeReason) String() string {
	switch r {
	case ReasonSuperseded:
		return "superseded"
	case ReasonConflictLost:
		return "conflict-lost"
	case ReasonOverBudget:
		return "over-budget"
	case ReasonSummarized:
		return "summarized"
	case ReasonSourceUnavailable:
		return "source-unavailable"
	default:
		return "unknown"
	}
}

// Excluded 记录一个未入选片段及其原因，保证取舍可追溯。
type Excluded struct {
	Fragment Fragment
	Reason   ExcludeReason
	Detail   string
}

// Result 是一次上下文组装的完整结果。
type Result struct {
	SessionID  string
	Version    uint64     // 结果对应的会话版本
	Budget     Budget     // 实际生效的预算
	Summary    string     // 命中的摘要文本（无则为空）
	Fragments  []Fragment // 入选片段，按组装顺序排列
	Excluded   []Excluded // 未入选片段及原因
	TokensUsed int        // 实际占用的 token 数（含摘要）
	Degraded   *Degradation
	FromCache  bool
}
