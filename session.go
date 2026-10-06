package assembly

import (
	"fmt"
	"sort"
	"sync"
)

// Summary 记录一次摘要推进的结果。
type Summary struct {
	Version   uint64 // 摘要自身的版本，随推进递增
	CoversSeq uint64 // 摘要覆盖到的会话序号
	Text      string
	Tokens    int
}

// Session 保存同一会话的片段与摘要状态，支持并发读写。
// 所有读取都通过 Snapshot 拿到某一完整版本的一致视图，
// 不会出现片段与摘要交叉的混合状态。
type Session struct {
	mu        sync.RWMutex
	id        string
	seq       uint64 // 单调递增序号，标记片段到达顺序
	version   uint64 // 状态版本，任何变更都使其递增
	fragments map[string]Fragment
	fragSeq   map[string]uint64 // 片段 ID -> 到达序号
	summary   Summary
}

// Snapshot 是会话在某一完整版本上的不可变视图。
type Snapshot struct {
	SessionID string
	Version   uint64
	Fragments []Fragment // 按到达顺序排列
	Summary   Summary
}

// NewSession 创建一个空会话。
func NewSession(id string) *Session {
	return &Session{
		id:        id,
		fragments: make(map[string]Fragment),
		fragSeq:   make(map[string]uint64),
	}
}

// ID 返回会话标识。
func (s *Session) ID() string { return s.id }

// Upsert 写入或替换片段，返回新的会话版本。
// 同一来源的高版本片段会替换低版本片段；同 ID 去重。
func (s *Session) Upsert(frags ...Fragment) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for _, f := range frags {
		if f.ID == "" {
			s.seq++
			f.ID = fmt.Sprintf("anon-%d", s.seq)
		}
		// 同 ID：保留版本更高者。
		if old, ok := s.fragments[f.ID]; ok && old.Version > f.Version {
			continue
		}
		// 同来源：新版本替换旧版本。
		for id, old := range s.fragments {
			if id != f.ID && old.Source == f.Source && f.Source != "" && old.Version < f.Version {
				delete(s.fragments, id)
				delete(s.fragSeq, id)
			}
		}
		if old, ok := s.fragments[f.ID]; !ok {
			s.seq++
			s.fragSeq[f.ID] = s.seq
			changed = true
		} else if old.Version != f.Version || old.Content != f.Content {
			changed = true
		}
		s.fragments[f.ID] = f
	}
	if changed {
		s.version++
	}
	return s.version
}

// CommitSummary 在期望版本未变时提交摘要，并原子移除被摘要覆盖的片段，
// 返回是否成功。版本已推进时提交被拒绝，不会留下半更新状态。
func (s *Session) CommitSummary(expectVersion uint64, sum Summary, summarizedIDs ...string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.version != expectVersion {
		return false
	}
	sum.Version = s.summary.Version + 1
	sum.CoversSeq = s.seq
	s.summary = sum
	for _, id := range summarizedIDs {
		delete(s.fragments, id)
		delete(s.fragSeq, id)
	}
	s.version++
	return true
}

// Snapshot 返回当前完整版本的一致视图。
func (s *Session) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	frags := make([]Fragment, 0, len(s.fragments))
	for _, f := range s.fragments {
		frags = append(frags, f)
	}
	sort.Slice(frags, func(i, j int) bool {
		return s.fragSeq[frags[i].ID] < s.fragSeq[frags[j].ID]
	})
	return Snapshot{
		SessionID: s.id,
		Version:   s.version,
		Fragments: frags,
		Summary:   s.summary,
	}
}
