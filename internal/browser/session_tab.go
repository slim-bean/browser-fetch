package browser

import (
	"context"

	cdppage "github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

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

// StartRecorder installs a gateway-authored interaction capture script:
// Runtime.addBinding exposes window.__bfRecord(payload) to the page,
// Page.addScriptToEvaluateOnNewDocument re-installs it on every navigation,
// and an immediate evaluate covers the current document. Events arrive via
// the returned channel. The script only *reports*; it never mutates the page.
func (t *SessionTab) StartRecorder(ctx context.Context, script, binding string) (<-chan string, error) {
	events := make(chan string, 64)
	chromedp.ListenTarget(t.p.ctx, func(ev any) {
		if e, ok := ev.(*runtime.EventBindingCalled); ok && e.Name == binding {
			select {
			case events <- e.Payload:
			default: // drop when the recorder is not draining; replay is paused
			}
		}
	})
	err := chromedp.Run(ctx,
		runtime.AddBinding(binding),
		chromedp.ActionFunc(func(ctx context.Context) error {
			_, err := cdppage.AddScriptToEvaluateOnNewDocument(script).Do(ctx)
			return err
		}),
		chromedp.Evaluate(script, nil),
	)
	if err != nil {
		return nil, err
	}
	return events, nil
}
