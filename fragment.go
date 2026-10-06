package assembly

import "time"

// Fragment 是调用方声明的一段输入内容。
type Fragment struct {
	ID          string    // 片段标识，同 ID 用于去重
	Source      string    // 来源标识
	Priority    int       // 越大越重要
	Version     uint64    // 同一来源的版本号，新版本替换旧版本
	CreatedAt   time.Time // 时效信息
	Content     string
	Tokens      int  // 预先计算的 token 数，0 表示按内容估算
	Unavailable bool // 来源不可用：调用方声明了片段但取不到内容

	// ConflictKey 非空时，相同 ConflictKey 而内容不同的片段视为相互矛盾，
	// 组装时按确定性规则只保留一个。
	ConflictKey string
}

// TokenCount 返回片段的 token 占用。
func (f Fragment) TokenCount() int {
	if f.Tokens > 0 {
		return f.Tokens
	}
	return EstimateTokens(f.Content)
}

// EstimateTokens 给出确定性的本地 token 估算，不依赖外部服务。
// 规则：ASCII 字符约 4 个计 1 token，非 ASCII 字符每个计 1 token。
func EstimateTokens(s string) int {
	ascii, nonASCII := 0, 0
	for _, r := range s {
		if r < 128 {
			ascii++
		} else {
			nonASCII++
		}
	}
	return (ascii+3)/4 + nonASCII
}
