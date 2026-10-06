package assembly

import "testing"

func TestUpsertVersionRules(t *testing.T) {
	s := newSession("s")
	f1 := Fragment{ID: "a", Source: "src", Version: 1, Content: "v1"}
	if _, replaced := s.Upsert(f1); replaced {
		t.Fatal("首次写入不应报告替换")
	}
	v1 := s.Version()

	// 幂等写入：相同版本相同内容不推进版本。
	if _, replaced := s.Upsert(f1); replaced || s.Version() != v1 {
		t.Fatal("幂等写入不应推进版本")
	}
	// 旧版本写入被忽略。
	stale := Fragment{ID: "a", Source: "src", Version: 0, Content: "v0"}
	if _, replaced := s.Upsert(stale); replaced || s.Version() != v1 {
		t.Fatal("旧版本写入必须被忽略")
	}
	// 新版本替换旧版本。
	f2 := Fragment{ID: "a", Source: "src", Version: 2, Content: "v2"}
	old, replaced := s.Upsert(f2)
	if !replaced || old.Content != "v1" {
		t.Fatalf("新版本必须替换旧版本, replaced=%v old=%q", replaced, old.Content)
	}
	snap := s.Snapshot()
	if len(snap.Fragments) != 1 || snap.Fragments[0].Content != "v2" {
		t.Fatalf("快照应只包含 v2, got %+v", snap.Fragments)
	}
}

func TestCommitSummaryCAS(t *testing.T) {
	s := newSession("s")
	s.Upsert(Fragment{ID: "a", Source: "u", Version: 1, Content: "hello"})
	base := s.Version()

	// 基版本过期：拒绝提交，且不留半更新状态。
	if s.CommitSummary(base+1, SummaryState{Text: "x"}, []Fragment{{ID: "a", Source: "u"}}) {
		t.Fatal("过期基版本必须被拒绝")
	}
	if snap := s.Snapshot(); snap.Summary != nil || len(snap.Fragments) != 1 {
		t.Fatal("被拒绝的提交不得改变状态")
	}

	// 基版本正确：原子提交，片段移除，版本推进。
	if !s.CommitSummary(base, SummaryState{Text: "sum", CoveredIDs: []string{"a"}}, []Fragment{{ID: "a", Source: "u"}}) {
		t.Fatal("正确基版本的提交应成功")
	}
	snap := s.Snapshot()
	if snap.Summary == nil || snap.Summary.Text != "sum" || snap.Summary.Version != 1 {
		t.Fatalf("摘要状态不正确: %+v", snap.Summary)
	}
	if len(snap.Fragments) != 0 {
		t.Fatal("被摘要覆盖的片段应被移除")
	}
	if snap.Version != base+1 {
		t.Fatalf("版本应推进到 %d, got %d", base+1, snap.Version)
	}

	// 摘要再次推进：摘要版本递增。
	s.Upsert(Fragment{ID: "b", Source: "u", Version: 1, Content: "more"})
	if !s.CommitSummary(s.Version(), SummaryState{Text: "sum2"}, []Fragment{{ID: "b", Source: "u"}}) {
		t.Fatal("第二次提交应成功")
	}
	if got := s.Snapshot().Summary.Version; got != 2 {
		t.Fatalf("摘要版本应为 2, got %d", got)
	}
}

func TestSnapshotIsolation(t *testing.T) {
	s := newSession("s")
	s.Upsert(Fragment{ID: "a", Source: "u", Version: 1, Content: "x"})
	snap := s.Snapshot()
	// 快照之后继续写入，快照内容不得变化。
	s.Upsert(Fragment{ID: "b", Source: "u", Version: 1, Content: "y"})
	s.CommitSummary(s.Version(), SummaryState{Text: "s"}, nil)
	if len(snap.Fragments) != 1 || snap.Summary != nil {
		t.Fatal("快照必须是某一完整版本的不可变视图")
	}
}
