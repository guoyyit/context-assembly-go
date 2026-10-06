package assembly

// Budget 描述一次组装的预算约束。
type Budget struct {
	MaxTokens     int // 上下文窗口总容量
	ReserveOutput int // 为模型输出预留的空间
}

// InputLimit 返回可用于输入组装的 token 上限。
func (b Budget) InputLimit() int {
	return b.MaxTokens - b.ReserveOutput
}

// BudgetLayers 是预算的层级设置：默认 < 会话 < 请求。
//
// 层级冲突时的合并规则是确定性的：
//   - MaxTokens 取各层最小值（最严格的容量上限生效）；
//   - ReserveOutput 取各层最大值（最严格的输出保护生效）。
//
// 因此请求级偏好可以进一步收紧预算，但永远不能挤占
// 会话层或默认层为输出预留的空间。
type BudgetLayers struct {
	Default Budget
	Session *Budget
	Request *Budget
}

// Resolve 按确定性规则合并各层预算。
func (l BudgetLayers) Resolve() Budget {
	out := l.Default
	if l.Session != nil {
		out = mergeBudget(out, *l.Session)
	}
	if l.Request != nil {
		out = mergeBudget(out, *l.Request)
	}
	return out
}

// mergeBudget 把一层预算合并到已解析结果上：
// 容量取更小值，输出预留取更大值；各层中 <=0 的字段视为未设置。
func mergeBudget(base, over Budget) Budget {
	if over.MaxTokens > 0 && (base.MaxTokens <= 0 || over.MaxTokens < base.MaxTokens) {
		base.MaxTokens = over.MaxTokens
	}
	if over.ReserveOutput > base.ReserveOutput {
		base.ReserveOutput = over.ReserveOutput
	}
	return base
}
