package assembly

import (
	"context"
	"strings"
)

// Summarizer 是可替换的本地摘要组件，不得依赖外部服务。
type Summarizer interface {
	// Summarize 把 fragments 压缩到 maxTokens 以内。
	// ctx 取消或超时时必须返回 ErrSummarizeTimeout。
	Summarize(ctx context.Context, fragments []Fragment, maxTokens int) (string, error)
}

// SummarizerFunc 把函数适配为 Summarizer。
type SummarizerFunc func(ctx context.Context, fragments []Fragment, maxTokens int) (string, error)

// Summarize 实现 Summarizer。
func (f SummarizerFunc) Summarize(ctx context.Context, fragments []Fragment, maxTokens int) (string, error) {
	return f(ctx, fragments, maxTokens)
}

// summaryPrefix 让摘要与保留原文在语义上衔接。
const summaryPrefix = "[早前内容摘要] "

// ExtractiveSummarizer 是默认的本地摘要实现：按句抽取，
// 确定性、不依赖外部服务，可随时被 WithSummarizer 替换。
type ExtractiveSummarizer struct{}

// Summarize 实现 Summarizer。
func (e ExtractiveSummarizer) Summarize(ctx context.Context, fragments []Fragment, maxTokens int) (string, error) {
	if maxTokens <= 0 {
		return "", ErrBudgetTooSmall
	}
	var b strings.Builder
	b.WriteString(summaryPrefix)
	used := EstimateTokens(summaryPrefix)
	for _, f := range fragments {
		if err := ctx.Err(); err != nil {
			return "", ErrSummarizeTimeout
		}
		for _, sent := range splitSentences(f.Content) {
			if err := ctx.Err(); err != nil {
				return "", ErrSummarizeTimeout
			}
			t := EstimateTokens(sent)
			if used+t > maxTokens {
				// 预算不足时截断当前句子，保证不超限。
				remain := maxTokens - used
				if remain > 0 {
					b.WriteString(truncateToTokens(sent, remain))
				}
				return b.String(), nil
			}
			b.WriteString(sent)
			used += t
		}
	}
	return b.String(), nil
}

// splitSentences 按中英文句读切分，保留分隔符。
func splitSentences(s string) []string {
	var out []string
	start := 0
	for i, r := range s {
		switch r {
		case '。', '！', '？', '!', '?', '\n':
			w := len(string(r))
			out = append(out, s[start:i+w])
			start = i + w
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// truncateToTokens 按估算 token 数截断字符串，保证 EstimateTokens 结果不超过 maxTokens。
func truncateToTokens(s string, maxTokens int) string {
	if maxTokens <= 0 {
		return ""
	}
	var b strings.Builder
	ascii, nonASCII := 0, 0
	for _, r := range s {
		if r < 128 {
			if (ascii+1+3)/4+nonASCII > maxTokens {
				break
			}
			ascii++
		} else {
			if (ascii+3)/4+nonASCII+1 > maxTokens {
				break
			}
			nonASCII++
		}
		b.WriteRune(r)
	}
	return b.String()
}
