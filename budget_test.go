package assembly

import "testing"

func TestBudgetLayersResolve(t *testing.T) {
	def := Budget{MaxTokens: 1000, ReserveOutput: 200}
	sess := Budget{MaxTokens: 800, ReserveOutput: 100}
	req := Budget{MaxTokens: 900, ReserveOutput: 50}

	got := BudgetLayers{Default: def, Session: &sess, Request: &req}.Resolve()
	// 容量取各层最小值，输出预留取各层最大值。
	if got.MaxTokens != 800 {
		t.Fatalf("MaxTokens = %d, want 800", got.MaxTokens)
	}
	if got.ReserveOutput != 200 {
		t.Fatalf("ReserveOutput = %d, want 200", got.ReserveOutput)
	}
	if got.InputLimit() != 600 {
		t.Fatalf("InputLimit = %d, want 600", got.InputLimit())
	}
}

func TestRequestCannotSqueezeOutput(t *testing.T) {
	def := Budget{MaxTokens: 1000, ReserveOutput: 300}
	req := Budget{ReserveOutput: 10} // 请求层试图压低输出预留

	got := BudgetLayers{Default: def, Request: &req}.Resolve()
	if got.ReserveOutput != 300 {
		t.Fatalf("ReserveOutput = %d, want 300（请求层不得挤占输出空间）", got.ReserveOutput)
	}
}

func TestBudgetResolveDeterministic(t *testing.T) {
	def := Budget{MaxTokens: 500, ReserveOutput: 100}
	a := BudgetLayers{Default: def}.Resolve()
	b := BudgetLayers{Default: def}.Resolve()
	if a != b {
		t.Fatalf("resolve must be deterministic: %+v vs %+v", a, b)
	}
}
