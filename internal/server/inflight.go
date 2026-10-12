package server

import (
	"sort"
	"sync"
	"time"
)

// InFlightRecord 单条实时在途请求记录。
type InFlightRecord struct {
	ID        string    `json:"id"`
	Model     string    `json:"model"`
	UID       string    `json:"uid"`
	Nick      string    `json:"nick"`
	Stream    bool      `json:"stream"`
	ClientIP  string    `json:"client_ip"`
	Owner     string    `json:"owner"`
	StartedAt time.Time `json:"started_at"`
	Elapsed   string    `json:"elapsed"`
	ElapsedMs int64     `json:"elapsed_ms"`
}

// InFlightTracker 实时在途流请求观测跟踪器。
type InFlightTracker struct {
	mu      sync.RWMutex
	records map[string]*InFlightRecord
}

// NewInFlightTracker 创建 InFlightTracker 实例。
func NewInFlightTracker() *InFlightTracker {
	return &InFlightTracker{
		records: make(map[string]*InFlightRecord),
	}
}

// Begin 开始跟踪一条在途请求，返回在请求结束（完成或报错）时调用的清理函数。
func (t *InFlightTracker) Begin(id, model, uid, nick string, stream bool, clientIP, owner string) func() {
	if t == nil {
		return func() {}
	}
	now := time.Now()
	rec := &InFlightRecord{
		ID:        id,
		Model:     model,
		UID:       uid,
		Nick:      nick,
		Stream:    stream,
		ClientIP:  clientIP,
		Owner:     owner,
		StartedAt: now,
	}

	t.mu.Lock()
	t.records[id] = rec
	t.mu.Unlock()

	return func() {
		t.mu.Lock()
		delete(t.records, id)
		t.mu.Unlock()
	}
}

// Snapshot 获取当前在途请求快照列表，按持续时长倒序排列。
func (t *InFlightTracker) Snapshot() []*InFlightRecord {
	if t == nil {
		return nil
	}
	t.mu.RLock()
	defer t.mu.RUnlock()

	now := time.Now()
	out := make([]*InFlightRecord, 0, len(t.records))
	for _, r := range t.records {
		dur := now.Sub(r.StartedAt)
		copied := *r
		copied.Elapsed = dur.Round(time.Millisecond).String()
		copied.ElapsedMs = dur.Milliseconds()
		out = append(out, &copied)
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].StartedAt.Before(out[j].StartedAt) // 耗时最长的排在最前
	})

	return out
}

// Count 获取当前在途请求数量。
func (t *InFlightTracker) Count() int {
	if t == nil {
		return 0
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.records)
}
