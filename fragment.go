package assembly

import "time"

// SourceID 标识片段来源（如 system、user、检索结果、工具输出等）。
type SourceID string

// Fragment 是调用方声明的一段输入内容。
type Fragment struct {
	ID        string    // 片段标识（同一来源内唯一）
	Source    SourceID  // 来源标识
	Version   uint64    // 同一来源内的版本号，新版本替换旧版本
	Priority  int       // 优先级，数值越大越重要
	Timestamp time.Time // 时效信息，用于新旧比较与摘要排序

	// ConflictGroup 非空时，同组片段互斥，按确定性规则只保留一个。
	ConflictGroup string

	Content string // 文本内容
	Tokens  int    // 预计算的 token 数；为 0 时由 TokenCounter 估算

	Pinned      bool // 关键约束：预算不足时不得静默丢弃
	Unavailable bool // 来源不可用：内容视为缺失
}

// EffectiveTokens 返回片段实际占用的 token 数。
// 优先使用预计算的 Tokens，否则用 TokenCounter 按统一规则估算。
// 来源不可用的片段不占用预算。
func (f Fragment) EffectiveTokens(c TokenCounter) int {
	if f.Unavailable {
		return 0
	}
	if f.Tokens > 0 {
		return f.Tokens
	}
	if c == nil {
		c = DefaultTokenCounter{}
	}
	return c.Count(f.Content)
}
