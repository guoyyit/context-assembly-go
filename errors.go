package assembly

import "errors"

// 可区分的降级原因，调用方可用 errors.Is 判定。
var (
	// ErrSourceUnavailable 表示声明的来源不可用。
	ErrSourceUnavailable = errors.New("assembly: source unavailable")
	// ErrSummarizeFailed 表示摘要生成失败。
	ErrSummarizeFailed = errors.New("assembly: summarization failed")
	// ErrSummarizeTimeout 表示摘要生成超时或被取消。
	ErrSummarizeTimeout = errors.New("assembly: summarization timeout")
	// ErrBudgetTooSmall 表示预算被压缩到无法容纳任何内容。
	ErrBudgetTooSmall = errors.New("assembly: budget too small")
)
