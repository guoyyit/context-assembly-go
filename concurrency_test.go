package assembly

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestConcurrentSharedSession 验证多个并发请求共享同一会话时：
// 任一请求读到的上下文都对应某个完整版本，且结果永不超预算。
func TestConcurrentSharedSession(t *testing.T) {
	store := NewStore()
	a := newTestAssembler(store, Budget{MaxTokens: 300, ReserveOutput: 20})

	const writers = 4
	const readers = 4
	const rounds = 50
	var wg sync.WaitGroup
	errs := make(chan string, (writers+readers)*rounds)

	for g := 0; g < writers+readers; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				req := Request{SessionID: "shared"}
				if g < writers {
					req.Fragments = []Fragment{
						mkFrag(fmt.Sprintf("g%d-m%d", g, i), "chat", 1, 1,
							time.Now().UnixNano(), strings.Repeat("x", 40)),
					}
				}
				res, err := a.Assemble(context.Background(), req)
				if err != nil && !errors.Is(err, ErrSummaryFailed) {
					errs <- fmt.Sprintf("unexpected error: %v", err)
					continue
				}
				if res.TokensUsed > res.Budget.InputLimit() {
					errs <- fmt.Sprintf("over budget: %d > %d", res.TokensUsed, res.Budget.InputLimit())
				}
				seen := make(map[string]bool)
				for _, f := range res.Fragments {
					if seen[f.ID] {
						errs <- "duplicate fragment in result: " + f.ID
					}
					seen[f.ID] = true
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
}

// TestCommitSummaryConcurrentCAS 验证同一基版本上并发的摘要提交只有一个成功。
func TestCommitSummaryConcurrentCAS(t *testing.T) {
	s := newSession("s")
	victim := mkFrag("a", "u", 1, 1, 1, "x")
	s.Upsert(victim)
	base := s.Version()

	const n = 8
	var wg sync.WaitGroup
	wins := make(chan bool, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			wins <- s.CommitSummary(base, SummaryState{Text: fmt.Sprintf("s%d", i)}, []Fragment{victim})
		}(i)
	}
	wg.Wait()
	close(wins)
	count := 0
	for w := range wins {
		if w {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("同一基版本只能有一个提交成功, got %d", count)
	}
	if snap := s.Snapshot(); snap.Summary == nil || len(snap.Fragments) != 0 {
		t.Fatal("提交成功后状态必须完整一致")
	}
}
