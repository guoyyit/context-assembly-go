package assembly

import (
	"context"
	"fmt"
	"strings"
)

// Summarizer 是可替换的本地摘要组件，不得依赖外部服务。
type Summarizer interface {
	// Summarize 把 fragments 压缩到 targetTokens 以内的摘要文本。
	// 实现必须保证：返回的摘要占用不超过 targetTokens；
	// ctx 取消时返回 ctx.Err()，不得返回半成品。
	Summarize(ctx context.Context, fragments []Fragment, targetTokens int) (string, error)
}

// LocalSummarizer 是默认的本地抽取式摘要实现，确定性、无外部依赖。
// 它为每个片段截取开头片段并保留来源与 ID 标记，
// 使摘要与保留的原文在语义上可衔接、可追溯。
type LocalSummarizer struct {
	Counter TokenCounter
}

func (s LocalSummarizer) counter() TokenCounter {
	if s.Counter != nil {
		return s.Counter
	}
	return DefaultTokenCounter{}
}

// Summarize 实现 Summarizer。
func (s LocalSummarizer) Summarize(ctx context.Context, fragments []Fragment, targetTokens int) (string, error) {
	c := s.counter()
	if len(fragments) == 0 {
		return "", nil
	}
	if targetTokens <= 0 {
		return "", ErrSummaryFailed
	}

	const header = "[摘要] 以下为较早内容的压缩:\n"
	remaining := targetTokens - c.Count(header)
	if remaining <= 0 {
		return "", ErrSummaryFailed
	}

	var b strings.Builder
	b.WriteString(header)
	for _, f := range fragments {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		prefix := fmt.Sprintf("- (%s/%s) ", f.Source, f.ID)
		allowance := remaining - c.Count(prefix) - c.Count("\n")
		if allowance <= 0 {
			break
		}
		excerpt := truncateToTokens(f.Content, allowance, c)
		line := prefix + excerpt + "\n"
		b.WriteString(line)
		remaining -= c.Count(line)
		if remaining <= 0 {
			break
		}
	}

	out := b.String()
	// 硬约束：无论如何不得超出目标。
	if c.Count(out) > targetTokens {
		out = truncateToTokens(out, targetTokens, c)
	}
	return out, nil
}

// truncateToTokens 把 s 截断到不超过 maxTokens（按 counter 计量）。
func truncateToTokens(s string, maxTokens int, c TokenCounter) string {
	if maxTokens <= 0 {
		return ""
	}
	if c.Count(s) <= maxTokens {
		return s
	}
	runes := []rune(s)
	lo, hi := 0, len(runes)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if c.Count(string(runes[:mid])) <= maxTokens {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return string(runes[:lo])
}
