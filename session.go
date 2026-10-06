package assembly

import (
	"sort"
	"sync"
)

// SummaryState 记录会话当前的摘要状态。
// 一旦提交即视为不可变，推进时整体替换。
type SummaryState struct {
	Version    uint64   // 摘要版本，随每次推进递增
	Text       string   // 摘要文本
	CoveredIDs []string // 摘要已覆盖（替换）的片段 ID
}

// Session 保存一个会话的全部片段与摘要状态。
// 并发模型：读方通过 Snapshot 拿到某一完整版本的不可变视图；
// 写方（片段更新、摘要提交）在写锁内原子完成并推进版本号，
// 因此任何请求读到的上下文都对应某个完整版本，
// 不会出现片段与摘要交叉的混合状态。
type Session struct {
	mu        sync.RWMutex
	id        string
	version   uint64
	fragments map[string]Fragment
	summary   *SummaryState
}

func newSession(id string) *Session {
	return &Session{id: id, fragments: make(map[string]Fragment)}
}

// Snapshot 是会话某一完整版本的不可变视图。
type Snapshot struct {
	Version   uint64
	Fragments []Fragment // 按 ID 排序，保证确定性
	Summary   *SummaryState
}

// ID 返回会话标识。
func (s *Session) ID() string { return s.id }

// Version 返回当前版本号。
func (s *Session) Version() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.version
}

// Upsert 写入或更新片段（按 Source+ID 去重），并推进会话版本。
// 同一 (Source, ID) 只保留最高版本：写入更旧版本会被忽略；
// 重复写入相同版本相同内容是幂等的，不推进版本（保证缓存稳定）。
// 返回被替换掉的旧版本以及是否发生了替换。
func (s *Session) Upsert(f Fragment) (old Fragment, replaced bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := string(f.Source) + "\x1f" + f.ID
	if cur, ok := s.fragments[key]; ok {
		if cur.Version > f.Version {
			return cur, false // 忽略过期的旧版本写入
		}
		if cur.Version == f.Version && cur.Content == f.Content &&
			cur.Priority == f.Priority && cur.Pinned == f.Pinned &&
			cur.Unavailable == f.Unavailable {
			return cur, false // 幂等写入，不推进版本
		}
		s.fragments[key] = f
		s.version++
		return cur, true
	}
	s.fragments[key] = f
	s.version++
	return Fragment{}, false
}

// Snapshot 返回当前版本的快照。
func (s *Session) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	frags := make([]Fragment, 0, len(s.fragments))
	for _, f := range s.fragments {
		frags = append(frags, f)
	}
	sort.Slice(frags, func(i, j int) bool { return frags[i].ID < frags[j].ID })
	return Snapshot{Version: s.version, Fragments: frags, Summary: s.summary}
}

// CommitSummary 仅当 baseVersion 仍等于当前版本时提交摘要：
// 原子地删除被摘要覆盖的片段、替换摘要状态并推进版本。
// 若期间已有其他写入（版本已变化），则拒绝提交并返回 false，
// 调用方需基于新快照重试；失败或取消不会留下半更新状态。
func (s *Session) CommitSummary(baseVersion uint64, sum SummaryState, removed []Fragment) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.version != baseVersion {
		return false
	}
	for _, f := range removed {
		delete(s.fragments, fragKey(f))
	}
	covered := make([]string, len(sum.CoveredIDs))
	copy(covered, sum.CoveredIDs)
	sum.CoveredIDs = covered
	if s.summary != nil {
		sum.Version = s.summary.Version + 1
	} else {
		sum.Version = 1
	}
	s.summary = &sum
	s.version++
	return true
}

// Store 管理多个会话。
type Store struct {
	mu       sync.Mutex
	sessions map[string]*Session
}

// NewStore 创建空 Store。
func NewStore() *Store {
	return &Store{sessions: make(map[string]*Session)}
}

// GetOrCreate 返回指定 ID 的会话，不存在则创建。
func (st *Store) GetOrCreate(id string) *Session {
	st.mu.Lock()
	defer st.mu.Unlock()
	if s, ok := st.sessions[id]; ok {
		return s
	}
	s := newSession(id)
	st.sessions[id] = s
	return s
}
