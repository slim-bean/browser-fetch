package browser

import "context"

// Snapshot is one atomic read of a page's identity and markup.
type Snapshot = snapshot

// SessionTab is a pooled tab leased to an interactive L1 session. The tab is
// NOT parked on about:blank while leased: the session holds page state between
// actions. Cookies persist in the shared browser context either way.
type SessionTab struct {
	m *Manager
	p *page
}

// Acquire leases a tab for a session. The returned release func must be called
// exactly once; it re-parks the tab and returns it to the pool.
func (m *Manager) Acquire(ctx context.Context) (*SessionTab, func(), error) {
	p, err := m.pool.acquire(ctx)
	if err != nil {
		return nil, nil, err
	}
	st := &SessionTab{m: m, p: p}
	release := func() { m.pool.release(p, true) }
	return st, release, nil
}

// Ctx returns the tab's long-lived context; session actions run against it.
func (t *SessionTab) Ctx() context.Context { return t.p.ctx }

// PageID identifies the tab in logs and evidence records.
func (t *SessionTab) PageID() int { return t.p.id }

// Read snapshots the page. The evaluated JS is constructed here in the
// gateway, never supplied by a caller.
func (t *SessionTab) Read(ctx context.Context) (Snapshot, error) {
	return t.m.read(ctx, t.p)
}
