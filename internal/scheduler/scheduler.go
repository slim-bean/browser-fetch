// Package scheduler paces browser navigations.
//
// Chrome will happily load a dozen tabs at once, but bot-protection vendors
// score burst rate per IP+session+host. So:
//
//   - concurrency across hosts is capped globally (one tab per slot);
//   - concurrency within a host is exactly 1, with a jittered minimum gap
//     between the end of one navigation and the start of the next;
//   - identical in-flight URLs are coalesced onto a single navigation, because
//     agents re-request the same URL constantly;
//   - every wait honours the caller's context, so queued work fails fast on the
//     request deadline instead of piling up.
//
// The host gate is taken before the global slot: a request waiting out its
// host's cooldown must not occupy a slot that another host could use.
package scheduler

import (
	"context"
	"math/rand"
	"sync"
	"time"
)

type Options struct {
	MaxSlots int
	HostGap  time.Duration
	Jitter   time.Duration
}

// Scheduler serialises and paces work of type T.
type Scheduler[T any] struct {
	slots  chan struct{}
	gap    time.Duration
	jitter time.Duration

	mu       sync.Mutex
	hosts    map[string]*hostState
	inflight map[string]*call[T]
	stats    stats

	// now and sleep are swappable for tests.
	now   func() time.Time
	sleep func(context.Context, time.Duration) error
}

type hostState struct {
	token    chan struct{} // capacity 1: per-host concurrency
	lastAt   time.Time
	lastURL  string
	queued   int
	inflight int
	total    uint64
	usedAt   time.Time
}

type call[T any] struct {
	done chan struct{}
	val  T
	err  error
}

type stats struct {
	queued    int
	running   int
	total     uint64
	shared    uint64
	timedOut  uint64
	completed uint64
	failed    uint64
}

func New[T any](o Options) *Scheduler[T] {
	if o.MaxSlots < 1 {
		o.MaxSlots = 1
	}
	return &Scheduler[T]{
		slots:    make(chan struct{}, o.MaxSlots),
		gap:      o.HostGap,
		jitter:   o.Jitter,
		hosts:    make(map[string]*hostState),
		inflight: make(map[string]*call[T]),
		now:      time.Now,
		sleep:    sleepCtx,
	}
}

// Do runs fn under the pacing rules. key dedupes identical work (use the URL);
// host selects the per-host gate. The bool reports whether the result came from
// an already in-flight identical call.
func (s *Scheduler[T]) Do(ctx context.Context, host, key string, fn func(context.Context) (T, error)) (T, bool, error) {
	var zero T

	// 1. Coalesce identical in-flight work.
	s.mu.Lock()
	if c, ok := s.inflight[key]; ok {
		s.stats.shared++
		s.mu.Unlock()
		select {
		case <-c.done:
			return c.val, true, c.err
		case <-ctx.Done():
			return zero, true, ctx.Err()
		}
	}
	c := &call[T]{done: make(chan struct{})}
	s.inflight[key] = c
	s.stats.total++
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.inflight, key)
		s.mu.Unlock()
		close(c.done)
	}()

	hs := s.hostState(host)

	// 2. Per-host gate (concurrency 1).
	s.mark(hs, +1, 0)
	select {
	case hs.token <- struct{}{}:
	case <-ctx.Done():
		s.mark(hs, -1, 0)
		s.countTimeout()
		c.err = ctx.Err()
		return zero, false, c.err
	}
	s.mark(hs, -1, 0)
	defer func() { <-hs.token }()

	// 3. Cooldown since this host's last navigation finished.
	if wait := s.hostWait(hs); wait > 0 {
		s.mark(hs, +1, 0)
		err := s.sleep(ctx, wait)
		s.mark(hs, -1, 0)
		if err != nil {
			s.countTimeout()
			c.err = err
			return zero, false, err
		}
	}

	// 4. Global slot.
	s.mark(hs, +1, 0)
	select {
	case s.slots <- struct{}{}:
	case <-ctx.Done():
		s.mark(hs, -1, 0)
		s.countTimeout()
		c.err = ctx.Err()
		return zero, false, c.err
	}
	s.mark(hs, -1, 0)
	defer func() { <-s.slots }()

	// 5. Run.
	s.mark(hs, 0, +1)
	val, err := fn(ctx)
	s.mark(hs, 0, -1)

	s.mu.Lock()
	hs.lastAt = s.now()
	hs.lastURL = key
	hs.total++
	if err != nil {
		s.stats.failed++
	} else {
		s.stats.completed++
	}
	s.mu.Unlock()

	c.val, c.err = val, err
	return val, false, err
}

func (s *Scheduler[T]) hostState(host string) *hostState {
	s.mu.Lock()
	defer s.mu.Unlock()
	hs, ok := s.hosts[host]
	if !ok {
		hs = &hostState{token: make(chan struct{}, 1)}
		s.hosts[host] = hs
		s.pruneLocked()
	}
	hs.usedAt = s.now()
	return hs
}

// pruneLocked keeps the host map from growing without bound in long sessions.
func (s *Scheduler[T]) pruneLocked() {
	if len(s.hosts) <= 1024 {
		return
	}
	cutoff := s.now().Add(-time.Hour)
	for k, v := range s.hosts {
		if v.inflight == 0 && v.queued == 0 && v.usedAt.Before(cutoff) {
			delete(s.hosts, k)
		}
	}
}

func (s *Scheduler[T]) hostWait(hs *hostState) time.Duration {
	s.mu.Lock()
	last := hs.lastAt
	s.mu.Unlock()
	if last.IsZero() || s.gap <= 0 {
		return 0
	}
	target := s.gap
	if s.jitter > 0 {
		target += time.Duration(rand.Int63n(int64(s.jitter)))
	}
	if elapsed := s.now().Sub(last); elapsed < target {
		return target - elapsed
	}
	return 0
}

func (s *Scheduler[T]) mark(hs *hostState, queued, running int) {
	s.mu.Lock()
	hs.queued += queued
	hs.inflight += running
	s.stats.queued += queued
	s.stats.running += running
	s.mu.Unlock()
}

func (s *Scheduler[T]) countTimeout() {
	s.mu.Lock()
	s.stats.timedOut++
	s.mu.Unlock()
}

// Snapshot is a point-in-time view for /stats and /debug.
type Snapshot struct {
	Slots     int                  `json:"slots"`
	Running   int                  `json:"running"`
	Queued    int                  `json:"queued"`
	Total     uint64               `json:"total"`
	Completed uint64               `json:"completed"`
	Failed    uint64               `json:"failed"`
	Shared    uint64               `json:"shared_dedupe"`
	TimedOut  uint64               `json:"timed_out"`
	Hosts     map[string]HostStats `json:"hosts"`
}

type HostStats struct {
	Queued     int       `json:"queued"`
	Running    int       `json:"running"`
	Total      uint64    `json:"total"`
	LastAt     time.Time `json:"last_at,omitzero"`
	LastURL    string    `json:"last_url,omitempty"`
	CooldownMS int64     `json:"cooldown_ms"`
}

func (s *Scheduler[T]) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := Snapshot{
		Slots:     cap(s.slots),
		Running:   s.stats.running,
		Queued:    s.stats.queued,
		Total:     s.stats.total,
		Completed: s.stats.completed,
		Failed:    s.stats.failed,
		Shared:    s.stats.shared,
		TimedOut:  s.stats.timedOut,
		Hosts:     make(map[string]HostStats, len(s.hosts)),
	}
	now := s.now()
	for host, hs := range s.hosts {
		var cooldown int64
		if !hs.lastAt.IsZero() {
			if left := s.gap - now.Sub(hs.lastAt); left > 0 {
				cooldown = left.Milliseconds()
			}
		}
		snap.Hosts[host] = HostStats{
			Queued:     hs.queued,
			Running:    hs.inflight,
			Total:      hs.total,
			LastAt:     hs.lastAt,
			LastURL:    hs.lastURL,
			CooldownMS: cooldown,
		}
	}
	return snap
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
