package assembly

// Budget 描述一次组装可用的 token 预算。
type Budget struct {
	MaxTokens     int // 总窗口
	ReserveOutput int // 为输出预留的空间
}

// Input 返回可用于输入的 token 数，不会为负。
// 输出预留优先：输入永远不得挤占 ReserveOutput。
func (b Budget) Input() int {
	n := b.MaxTokens - b.ReserveOutput
	if n < 0 {
		return 0
	}
	return n
}

// BudgetPolicy 按层级给出预算：请求级 > 会话级 > 全局默认。
type BudgetPolicy struct {
	Global  Budget
	Session Budget
	Request Budget
}

// Resolve 按确定优先级解析出最终预算。
// 逐字段取最具体层级的非零值：Request > Session > Global，
// 因此层级冲突时的取舍是确定且可解释的。
func (p BudgetPolicy) Resolve() Budget {
	var out Budget
	out.MaxTokens = firstNonZero(p.Request.MaxTokens, p.Session.MaxTokens, p.Global.MaxTokens)
	out.ReserveOutput = firstNonZero(p.Request.ReserveOutput, p.Session.ReserveOutput, p.Global.ReserveOutput)
	return out
}

func firstNonZero(vals ...int) int {
	for _, v := range vals {
		if v != 0 {
			return v
		}
	}
	return 0
}
