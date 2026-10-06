package assembly

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestCommitSummaryCAS(t *testing.T) {
	s := NewSession("s1")
	v := s.Upsert(frag("f1", "chat", 1, 1, "hello"))

	// 版本不匹配时提交被拒绝，状态不变。
	if ok := s.CommitSummary(v+1, Summary{Text: "x"}); ok {
		t.Fatal("commit with stale version must fail")
	}
	if snap := s.Snapshot(); snap.Summary.Text != "" {
		t.Fatalf("rejected commit must not change state: %+v", snap.Summary)
	}

	// 版本匹配时提交成功，摘要版本递增。
	if ok := s.CommitSummary(v, Summary{Text: "摘要"}, "f1"); !ok {
		t.Fatal("commit with current version must succeed")
	}
	snap := s.Snapshot()
	if snap.Summary.Text != "摘要" || snap.Summary.Version != 1 {
		t.Fatalf("summary not committed: %+v", snap.Summary)
	}
	if len(snap.Fragments) != 0 {
		t.Fatalf("summarized fragments must be removed: %+v", snap.Fragments)
	}
}

func TestConcurrentUpsertAndSnapshot(t *testing.T) {
	s := NewSession("s1")
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				s.Upsert(Fragment{
					ID:      fmt.Sprintf("g%d-f%d", g, i),
					Source:  fmt.Sprintf("src-%d-%d", g, i),
					Version: uint64(i + 1),
					Content: "内容",
				})
			}
		}(g)
	}
	// 并发读取：每个快照都必须对应某个完整版本。
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			prev := uint64(0)
			for i := 0; i < 100; i++ {
				snap := s.Snapshot()
				if snap.Version < prev {
					t.Errorf("version went backwards: %d < %d", snap.Version, prev)
				}
				prev = snap.Version
				// 快照内片段按到达顺序排列。
				for j := 1; j < len(snap.Fragments); j++ {
					a, b := snap.Fragments[j-1], snap.Fragments[j]
					if a.CreatedAt.After(b.CreatedAt) {
						t.Errorf("snapshot fragments out of order")
					}
				}
			}
		}()
	}
	wg.Wait()
	if got := len(s.Snapshot().Fragments); got != 400 {
		t.Fatalf("expected 400 fragments, got %d", got)
	}
}

func TestConcurrentSummaryAndUpsert(t *testing.T) {
	s := NewSession("s1")
	s.Upsert(frag("seed", "chat", 1, 1, "种子内容"))
	var wg sync.WaitGroup

	// 一路持续写入。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			s.Upsert(Fragment{ID: fmt.Sprintf("f%d", i), Source: "chat", Version: 1, Content: "消息"})
		}
	}()

	// 一路尝试提交摘要：只允许在版本一致时成功。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			snap := s.Snapshot()
			ids := make([]string, 0, len(snap.Fragments))
			for _, f := range snap.Fragments {
				ids = append(ids, f.ID)
			}
			s.CommitSummary(snap.Version, Summary{Text: fmt.Sprintf("摘要%d", i)}, ids...)
		}
	}()
	wg.Wait()

	// 最终快照必须自洽：摘要版本单调，片段与摘要同属一个版本。
	snap := s.Snapshot()
	if snap.Summary.Version > snap.Version {
		t.Fatalf("summary version %d exceeds session version %d", snap.Summary.Version, snap.Version)
	}
}

func TestConcurrentAssembleSharedSession(t *testing.T) {
	a := NewAssembler(
		WithBudgetPolicy(BudgetPolicy{Global: Budget{MaxTokens: 60}}),
		WithCache(NewCache(64)),
	)
	sess := a.Session("shared")
	for i := 0; i < 10; i++ {
		sess.Upsert(Fragment{
			ID:      fmt.Sprintf("seed-%d", i),
			Source:  "chat",
			Version: 1,
			Content: strings.Repeat("长", 10),
		})
	}

	var wg sync.WaitGroup
	errs := make(chan error, 128)
	for g := 0; g < 6; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				// 一半 goroutine 写入新片段，一半读取组装。
				if g%2 == 0 {
					sess.Upsert(Fragment{
						ID:      fmt.Sprintf("g%d-f%d", g, i),
						Source:  "chat",
						Version: 1,
						Content: strings.Repeat("新", 8),
					})
					continue
				}
				res, err := a.Assemble(context.Background(), Request{SessionID: "shared"})
				if err != nil {
					errs <- err
					continue
				}
				if res.TokensUsed > res.Budget.Input() {
					errs <- fmt.Errorf("result over budget: %d > %d", res.TokensUsed, res.Budget.Input())
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}
