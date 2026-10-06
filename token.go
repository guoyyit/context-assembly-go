package assembly

import "unicode"

// TokenCounter 以统一规则计算文本占用的 token 数，
// 所有片段与摘要的占用都通过它度量，保证口径一致。
type TokenCounter interface {
	Count(text string) int
}

// TokenCounterFunc 把普通函数适配为 TokenCounter。
type TokenCounterFunc func(text string) int

// Count 实现 TokenCounter。
func (f TokenCounterFunc) Count(text string) int { return f(text) }

// DefaultTokenCounter 是默认的本地估算实现：
// CJK 字符按 1 个 token 计，其余字符约每 4 个计 1 个 token（向上取整）。
// 该规则确定、可复现，不依赖任何外部服务。
type DefaultTokenCounter struct{}

// Count 实现 TokenCounter。
func (DefaultTokenCounter) Count(text string) int {
	cjk, other := 0, 0
	for _, r := range text {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
			unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) {
			cjk++
		} else {
			other++
		}
	}
	return cjk + (other+3)/4
}
