package assembly

import "errors"

// 可区分的异常链路错误。
var (
	// ErrSourceUnavailable 表示关键来源不可用。
	ErrSourceUnavailable = errors.New("assembly: source unavailable")
	// ErrSummaryTimeout 表示摘要生成超时。
	ErrSummaryTimeout = errors.New("assembly: summary generation timeout")
	// ErrSummaryFailed 表示摘要生成失败。
	ErrSummaryFailed = errors.New("assembly: summary generation failed")
	// ErrBudgetTooSmall 表示预算被压缩到无法容纳任何内容。
	ErrBudgetTooSmall = errors.New("assembly: budget too small for any content")
)
