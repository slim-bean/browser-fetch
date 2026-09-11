// Package reqlog keeps a bounded, in-memory history of fetch requests so
// /debug can show what the gateway has been doing without shipping logs
// anywhere. It is deliberately small and lossy: newest N entries only.
package reqlog

import (
	"sync"
	"time"
)

// Entry is one completed fetch attempt.
type Entry struct {
	ID          string    `json:"id"`
	At          time.Time `json:"at"`
	URL         string    `json:"url"`
	FinalURL    string    `json:"final_url,omitempty"`
	Host        string    `json:"host"`
	Outcome     string    `json:"outcome"`
	Status      int       `json:"status,omitempty"`
	Bytes       int       `json:"bytes,omitempty"`
	DurationMS  int64     `json:"duration_ms"`
	QueueWaitMS int64     `json:"queue_wait_ms"`
	Shared      bool      `json:"deduped,omitempty"`
	PageID      int       `json:"page_id,omitempty"`
	Vendor      string    `json:"vendor,omitempty"`
	AssistedMS  int64     `json:"assisted_ms,omitempty"`
	Error       string    `json:"error,omitempty"`
	RemoteAddr  string    `json:"remote_addr,omitempty"`
}

// Ring is a fixed-capacity circular buffer of entries.
type Ring struct {
	mu    sync.RWMutex
	buf   []Entry
	next  int
	count int
}

func NewRing(capacity int) *Ring {
	if capacity < 1 {
		capacity = 1
	}
	return &Ring{buf: make([]Entry, capacity)}
}

func (r *Ring) Add(e Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf[r.next] = e
	r.next = (r.next + 1) % len(r.buf)
	if r.count < len(r.buf) {
		r.count++
	}
}

// Recent returns up to limit entries, newest first.
func (r *Ring) Recent(limit int) []Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if limit <= 0 || limit > r.count {
		limit = r.count
	}
	out := make([]Entry, 0, limit)
	for i := 0; i < limit; i++ {
		idx := (r.next - 1 - i + len(r.buf)*2) % len(r.buf)
		out = append(out, r.buf[idx])
	}
	return out
}

func (r *Ring) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.count
}
