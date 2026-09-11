// Package browser drives an already-running, user-launched Chrome over CDP.
//
// Design notes:
//
//   - We attach (NewRemoteAllocator) rather than launch. Chrome owns its own
//     lifecycle, updates and crash recovery; the human can log into sites in the
//     same window the agent uses, and there is no --enable-automation flag.
//   - Chrome locks its user-data-dir, so "many processes sharing one profile" is
//     impossible. Parallelism is therefore tabs, which is what we want anyway:
//     tabs in the default browser context share cookies, so a challenge solved
//     (or a login performed) in one tab benefits every later fetch.
//   - Tabs are pooled and parked on about:blank between jobs. They are never
//     closed, which also guarantees Chrome always has a target open.
package browser

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

type Options struct {
	ChromeURL     string
	MaxTabs       int
	NavTimeout    time.Duration
	ChallengeWait time.Duration
	AssistTimeout time.Duration
	// ChallengeRetries re-navigates after an unresolved challenge. Observed
	// behaviour: a first attempt that renders a captcha page still sets cookies,
	// and an immediate second navigation to the same URL can return real
	// content. Cheap to try, so it defaults to 1.
	ChallengeRetries    int
	ChallengeRetryDelay time.Duration
	BlockMedia          bool
	Logger              *slog.Logger
}

// Manager owns the CDP connection and the tab pool.
type Manager struct {
	opts  Options
	log   *slog.Logger
	alloc context.Context
	stop  context.CancelFunc

	pool *pool

	mu        sync.Mutex
	connected bool
	version   string
	lastErr   string
	assists   map[int]*Assist
}

// Assist records a challenge waiting for a human at the VM's screen.
type Assist struct {
	PageID int       `json:"page_id"`
	URL    string    `json:"url"`
	Vendor string    `json:"vendor"`
	Since  time.Time `json:"since"`
}

// Request is a single fetch.
type Request struct {
	URL string
	// Assist overrides the configured assist timeout for this request.
	Assist *time.Duration
}

// Result is a successfully rendered page.
type Result struct {
	URL    string `json:"url"`
	Title  string `json:"title"`
	HTML   string `json:"html"`
	Status int    `json:"status"`
	// PageID identifies the tab used, for log correlation.
	PageID int `json:"page_id"`
	// Attempts is how many navigations it took (>1 means a challenge retry).
	Attempts int `json:"attempts,omitempty"`
	// AssistedMS is how long a human took to clear a challenge, if any.
	AssistedMS int64 `json:"assisted_ms,omitempty"`
}

// ChallengeError means the page never resolved into real content.
type ChallengeError struct {
	Vendor      string
	Interactive bool
	URL         string
}

func (e *ChallengeError) Error() string {
	return fmt.Sprintf("unresolved %s interstitial at %s", e.Vendor, e.URL)
}

// NavError wraps a navigation failure (DNS, TLS, timeout, crashed tab).
type NavError struct{ Err error }

func (e *NavError) Error() string { return "navigation failed: " + e.Err.Error() }
func (e *NavError) Unwrap() error { return e.Err }

func New(ctx context.Context, o Options) *Manager {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	allocCtx, stop := chromedp.NewRemoteAllocator(ctx, o.ChromeURL)
	m := &Manager{
		opts:    o,
		log:     o.Logger,
		alloc:   allocCtx,
		stop:    stop,
		assists: make(map[int]*Assist),
	}
	m.pool = newPool(m, o.MaxTabs)
	return m
}

func (m *Manager) Close() {
	m.pool.closeAll()
	m.stop()
}

// Probe checks the DevTools endpoint and caches the browser version. It is
// cheap and safe to call from /healthz.
func (m *Manager) Probe(ctx context.Context) error {
	base := strings.TrimSuffix(m.opts.ChromeURL, "/")
	base = strings.Replace(base, "ws://", "http://", 1)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/json/version", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		m.setConn(false, "", err.Error())
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("devtools returned HTTP %d", resp.StatusCode)
		m.setConn(false, "", err.Error())
		return err
	}
	var v struct {
		Browser string `json:"Browser"`
	}
	if err := decodeJSON(resp.Body, &v); err != nil {
		m.setConn(false, "", err.Error())
		return err
	}
	m.setConn(true, v.Browser, "")
	return nil
}

func (m *Manager) setConn(ok bool, version, errMsg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.connected = ok
	if version != "" {
		m.version = version
	}
	m.lastErr = errMsg
}

// Health is the /healthz payload.
type Health struct {
	Connected bool     `json:"connected"`
	Version   string   `json:"chrome_version,omitempty"`
	LastError string   `json:"last_error,omitempty"`
	Tabs      PoolStat `json:"tabs"`
	Assists   []Assist `json:"assists,omitempty"`
}

func (m *Manager) Health() Health {
	m.mu.Lock()
	h := Health{Connected: m.connected, Version: m.version, LastError: m.lastErr}
	for _, a := range m.assists {
		h.Assists = append(h.Assists, *a)
	}
	m.mu.Unlock()
	h.Tabs = m.pool.stat()
	return h
}

// Fetch renders one URL. Pacing and deduplication are the scheduler's job;
// this function assumes it may navigate immediately.
func (m *Manager) Fetch(ctx context.Context, req Request) (*Result, error) {
	p, err := m.pool.acquire(ctx)
	if err != nil {
		return nil, err
	}
	healthy := true
	defer func() { m.pool.release(p, healthy) }()

	log := m.log.With("page", p.id, "url", req.URL)

	navCtx, cancelNav := context.WithTimeout(p.ctx, m.opts.NavTimeout)
	defer cancelNav()
	unwatch := watch(ctx, cancelNav)
	defer unwatch()

	attempts := m.opts.ChallengeRetries + 1
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if attempt > 1 {
			log.Info("retrying after unresolved challenge",
				"attempt", attempt, "of", attempts, "delay", m.opts.ChallengeRetryDelay)
			if err := sleepCtx(ctx, m.opts.ChallengeRetryDelay); err != nil {
				return nil, err
			}
		}

		p.beginNav()
		if err := chromedp.Run(navCtx, chromedp.Navigate(req.URL)); err != nil {
			// A dead tab must not be reused.
			if isFatalTabErr(err) {
				healthy = false
			}
			// The caller's deadline takes precedence in the reported error.
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, &NavError{Err: err}
		}

		res, err := m.settle(ctx, p, req, log)
		if err == nil {
			res.PageID = p.id
			res.Attempts = attempt
			return res, nil
		}
		if isFatalTabErr(err) {
			healthy = false
			return nil, err
		}
		lastErr = err

		// Only a challenge is worth retrying; navigation and deadline errors are not.
		var chErr *ChallengeError
		if !errors.As(err, &chErr) || ctx.Err() != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

func (m *Manager) addAssist(pageID int, url, vendor string) {
	m.mu.Lock()
	m.assists[pageID] = &Assist{PageID: pageID, URL: url, Vendor: vendor, Since: time.Now()}
	m.mu.Unlock()
}

func (m *Manager) removeAssist(pageID int) {
	m.mu.Lock()
	delete(m.assists, pageID)
	m.mu.Unlock()
}

// mediaPatterns are blocked when BlockMedia is set. URLPattern constructor
// syntax, per Network.setBlockedURLs.
var mediaPatterns = func() []*network.BlockPattern {
	exts := []string{
		"jpg", "jpeg", "png", "gif", "webp", "avif", "svg", "ico",
		"mp4", "webm", "mp3", "woff", "woff2", "ttf", "otf",
	}
	out := make([]*network.BlockPattern, 0, len(exts))
	for _, ext := range exts {
		out = append(out, &network.BlockPattern{URLPattern: "*://*:*/*." + ext, Block: true})
	}
	return out
}()

func (m *Manager) prepareTab(pctx context.Context) error {
	actions := []chromedp.Action{network.Enable()}
	if m.opts.BlockMedia {
		actions = append(actions, network.SetBlockedURLs().WithURLPatterns(mediaPatterns))
	}
	return chromedp.Run(pctx, actions...)
}

// watch cancels a navigation when the caller's context ends first.
func watch(ctx context.Context, cancel context.CancelFunc) (stop func()) {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			cancel()
		case <-done:
		}
	}()
	return func() { close(done) }
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func isFatalTabErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	s := err.Error()
	for _, marker := range []string{
		"target closed", "websocket", "connection refused",
		"page crash", "Inspected target", "not connected",
	} {
		if strings.Contains(strings.ToLower(s), strings.ToLower(marker)) {
			return true
		}
	}
	return false
}

// errEmptyDocument is returned when a tab never produced a document.
var errEmptyDocument = errors.New("page produced no document")
