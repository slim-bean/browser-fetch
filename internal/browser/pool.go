package browser

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/chromedp/cdproto/network"
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
	m    *Manager
	size int

	mu      sync.Mutex
	pages   []*page
	free    chan *page
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

func newPool(m *Manager, size int) *pool {
	if size < 1 {
		size = 1
	}
	return &pool{m: m, size: size, free: make(chan *page, size)}
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
	// Prefer an idle tab.
	select {
	case pg := <-p.free:
		p.markBusy(+1)
		return pg, nil
	default:
	}

	p.mu.Lock()
	canCreate := p.created < p.size
	if canCreate {
		p.created++
	}
	p.mu.Unlock()

	if canCreate {
		pg, err := p.newPage()
		if err != nil {
			p.mu.Lock()
			p.created--
			p.mu.Unlock()
			return nil, err
		}
		p.markBusy(+1)
		return pg, nil
	}

	select {
	case pg := <-p.free:
		p.markBusy(+1)
		return pg, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// release parks a healthy tab back in the pool, or retires a broken one.
func (p *pool) release(pg *page, healthy bool) {
	p.markBusy(-1)
	if !healthy {
		p.retire(pg)
		return
	}
	pg.mu.Lock()
	pg.uses++
	pg.mu.Unlock()
	p.park(pg)
	p.free <- pg
}

// park navigates the tab away from the fetched page. Best effort: a failure
// here does not invalidate the tab, the next navigation will overwrite it.
func (p *pool) park(pg *page) {
	ctx, cancel := context.WithTimeout(pg.ctx, 5*time.Second)
	defer cancel()
	_ = chromedp.Run(ctx, chromedp.Navigate("about:blank"))
}

func (p *pool) retire(pg *page) {
	pg.cancel()
	p.mu.Lock()
	p.created--
	p.retired++
	for i, existing := range p.pages {
		if existing == pg {
			p.pages = append(p.pages[:i], p.pages[i+1:]...)
			break
		}
	}
	p.mu.Unlock()
	p.m.log.Warn("retired unhealthy tab", "page", pg.id, "uses", pg.uses)
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

	pctx, cancel := chromedp.NewContext(p.m.alloc)
	pg := &page{id: id, ctx: pctx, cancel: cancel, createdAt: time.Now()}

	// This first Run creates the target in Chrome's default browser context,
	// so the tab shares the profile's cookies.
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
