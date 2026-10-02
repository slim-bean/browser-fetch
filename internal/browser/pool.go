package browser

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// page is one long-lived Chrome tab.
type page struct {
	id        int
	ctx       context.Context
	cancel    context.CancelFunc
	createdAt time.Time

	mu      sync.Mutex
	uses    int
	lastURL string
	docs    []docResponse // document responses seen during the current navigation
}

type docResponse struct {
	url    string
	status int
}

// pool hands out tabs. Tabs are created lazily up to size and then reused
// forever; they are parked on about:blank between jobs so no page state leaks
// between fetches, while cookies (shared by the browser context) persist.
type pool struct {
	m     *Manager
	alloc context.Context
	size  int

	mu      sync.Mutex
	pages   []*page
	free    chan *page
	changed chan struct{} // closed/replaced under mu when a tab or creation slot becomes available
	nextID  int
	created int
	busy    int
	retired int
}

type PoolStat struct {
	Size    int `json:"size"`
	Created int `json:"created"`
	Busy    int `json:"busy"`
	Idle    int `json:"idle"`
	Retired int `json:"retired"`
}

func newPool(m *Manager, alloc context.Context, size int) *pool {
	if size < 1 {
		size = 1
	}
	return &pool{m: m, alloc: alloc, size: size, free: make(chan *page, size), changed: make(chan struct{})}
}

func (p *pool) stat() PoolStat {
	p.mu.Lock()
	defer p.mu.Unlock()
	return PoolStat{
		Size:    p.size,
		Created: p.created,
		Busy:    p.busy,
		Idle:    len(p.free),
		Retired: p.retired,
	}
}

// acquire returns a ready tab, creating one if the pool is not yet full.
func (p *pool) acquire(ctx context.Context) (*page, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := p.alloc.Err(); err != nil {
			return nil, err
		}

		p.mu.Lock()
		// Idle workers can lose their CDP connection without Chrome restarting.
		// Discard them before any navigation; no request/action is being replayed.
		select {
		case pg := <-p.free:
			p.mu.Unlock()
			if pg.ctx.Err() != nil {
				p.retire(pg)
				continue
			}
			p.markBusy(+1)
			return pg, nil
		default:
		}
		canCreate := p.created < p.size
		if canCreate {
			p.created++
		}
		changed := p.changed
		p.mu.Unlock()

		if canCreate {
			pg, err := p.newPage()
			if err != nil {
				p.mu.Lock()
				p.created--
				p.notifyLocked()
				p.mu.Unlock()
				return nil, err
			}
			p.markBusy(+1)
			return pg, nil
		}

		// Retirement frees capacity without returning a tab. Wake up for both
		// cases, otherwise waiters on a full pool can stall until their deadline.
		select {
		case <-changed:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-p.alloc.Done():
			return nil, p.alloc.Err()
		}
	}
}

func (p *pool) notifyLocked() {
	close(p.changed)
	p.changed = make(chan struct{})
}

// release parks a healthy tab back in the pool, or retires a broken one.
func (p *pool) release(pg *page, healthy bool) {
	p.markBusy(-1)
	if !healthy || pg.ctx.Err() != nil {
		p.retire(pg)
		return
	}
	pg.mu.Lock()
	pg.uses++
	pg.mu.Unlock()
	if err := p.park(pg); err != nil || pg.ctx.Err() != nil {
		p.retire(pg)
		return
	}
	p.mu.Lock()
	p.free <- pg
	p.notifyLocked()
	p.mu.Unlock()
}

// Park using the worker's lifetime, not the finished request's context. Failure
// retires the worker instead of circulating a broken or unreset tab forever.
func (p *pool) park(pg *page) error {
	ctx, cancel := context.WithTimeout(pg.ctx, 5*time.Second)
	defer cancel()
	return chromedp.Run(ctx, chromedp.Navigate("about:blank"))
}

func (p *pool) retire(pg *page) {
	if pg.cancel != nil {
		pg.cancel()
	}
	p.mu.Lock()
	p.created--
	p.retired++
	for i, existing := range p.pages {
		if existing == pg {
			p.pages = append(p.pages[:i], p.pages[i+1:]...)
			break
		}
	}
	p.notifyLocked()
	p.mu.Unlock()
	pg.mu.Lock()
	uses := pg.uses
	pg.mu.Unlock()
	p.m.log.Warn("retired unhealthy tab", "page", pg.id, "uses", uses)
}

func (p *pool) markBusy(delta int) {
	p.mu.Lock()
	p.busy += delta
	p.mu.Unlock()
}

func (p *pool) newPage() (*page, error) {
	p.mu.Lock()
	p.nextID++
	id := p.nextID
	p.mu.Unlock()

	pctx, cancel := chromedp.NewContext(p.alloc)
	if p.m.opts.BackgroundTabs {
		// Targets allocates only the browser connection, not a foreground tab.
		// The control context owns that connection until this worker is retired.
		control, stopControl := pctx, cancel
		targets, err := chromedp.Targets(control)
		if err != nil {
			stopControl()
			return nil, &NavError{Err: err}
		}
		hasPage := false
		for _, info := range targets {
			if info.Type == "page" {
				hasPage = true
				break
			}
		}
		executor := cdp.WithExecutor(control, chromedp.FromContext(control).Browser)
		create := target.CreateTarget("about:blank").WithBackground(true)
		if !hasPage {
			create = create.WithNewWindow(true).WithWindowState(target.WindowStateMinimized)
		}
		targetID, err := create.Do(executor)
		if err != nil {
			stopControl()
			return nil, &NavError{Err: err}
		}
		var stopTab context.CancelFunc
		pctx, stopTab = chromedp.NewContext(control, chromedp.WithTargetID(targetID))
		cancel = func() { stopTab(); stopControl() }
	}
	pg := &page{id: id, ctx: pctx, cancel: cancel, createdAt: time.Now()}

	// Attach/initialize in Chrome's default browser context (shared cookies).
	if err := p.m.prepareTab(pctx); err != nil {
		cancel()
		return nil, &NavError{Err: err}
	}
	chromedp.ListenTarget(pctx, pg.onEvent)

	p.mu.Lock()
	p.pages = append(p.pages, pg)
	p.mu.Unlock()

	p.m.log.Info("opened tab", "page", id)
	return pg, nil
}

func (p *pool) closeAll() {
	p.mu.Lock()
	pages := append([]*page(nil), p.pages...)
	p.pages = nil
	p.mu.Unlock()
	for _, pg := range pages {
		pg.cancel()
	}
}

// onEvent records document responses so we can report the target's real HTTP
// status. Redirect chains produce several; we match the final URL later.
func (pg *page) onEvent(ev any) {
	e, ok := ev.(*network.EventResponseReceived)
	if !ok || e.Type != network.ResourceTypeDocument {
		return
	}
	pg.mu.Lock()
	if len(pg.docs) < 32 {
		pg.docs = append(pg.docs, docResponse{url: e.Response.URL, status: int(e.Response.Status)})
	}
	pg.mu.Unlock()
}

func (pg *page) beginNav() {
	pg.mu.Lock()
	pg.docs = pg.docs[:0]
	pg.mu.Unlock()
}

// status returns the HTTP status for finalURL, falling back to the last
// document response seen, or 0 when Chrome served it from cache/no event.
func (pg *page) status(finalURL string) int {
	pg.mu.Lock()
	defer pg.mu.Unlock()
	for i := len(pg.docs) - 1; i >= 0; i-- {
		if pg.docs[i].url == finalURL {
			return pg.docs[i].status
		}
	}
	if len(pg.docs) > 0 {
		return pg.docs[len(pg.docs)-1].status
	}
	return 0
}

func decodeJSON(r io.Reader, v any) error {
	return json.NewDecoder(r).Decode(v)
}
