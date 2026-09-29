// Package session implements L1 interaction primitives: a leased tab that
// holds one page between typed actions, with a per-action evidence log and
// assertion-gated credential typing.
//
// Design constraints (see docs/l1-design.md):
//
//   - no caller-supplied JavaScript: actions are a typed vocabulary resolved
//     through CDP runtime calls the gateway itself constructs;
//   - every action appends to a bounded evidence log, newest-N, in memory
//     only — evidence that leaves the gateway is the flow engine's job;
//   - typing into a password field is refused unless an exact screen
//     assertion passed within the session's assertion TTL.
package session

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/chromedp"
)

// Manager owns live sessions. Sessions are keyed by ID and expire when idle
// longer than MaxIdle; the gateway is single-tenant, so the map stays small.
type Manager struct {
	log *slog.Logger
	// Acquire leases a pooled tab for the session; release re-parks it.
	acquire func(ctx context.Context) (Tab, func(), error)
	opts    Options

	mu       sync.Mutex
	sessions map[string]*Session
	nextID   int
}

// Options bounds sessions.
type Options struct {
	// MaxSessions caps concurrently open sessions (1 tab each).
	MaxSessions int
	// MaxIdle evicts a session whose last action is older than this.
	MaxIdle time.Duration
	// ActionTimeout bounds a single action.
	ActionTimeout time.Duration
	// AssertTTL is how long a passing assertion stays fresh for typing.
	AssertTTL time.Duration
	// MaxLog caps the per-session evidence log.
	MaxLog int
	// AllowNavigate, when set, must return nil for URLs the session may
	// navigate to. It is a tripwire on the navigate primitive, not a sandbox:
	// clicking a link can still navigate the tab.
	AllowNavigate func(url string) error
}

func DefaultOptions() Options {
	return Options{
		MaxSessions:   2,
		MaxIdle:       10 * time.Minute,
		ActionTimeout: 30 * time.Second,
		AssertTTL:     30 * time.Second,
		MaxLog:        200,
	}
}

func (m *Manager) logger() *slog.Logger {
	if m.log == nil {
		return slog.Default()
	}
	return m.log
}

// New builds a Manager. acquire/release come from the browser tab pool.
func New(log *slog.Logger, acquire func(ctx context.Context) (Tab, func(), error), opts Options) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{log: log, acquire: acquire, opts: opts, sessions: map[string]*Session{}}
}

// Tab is one leased browser tab as the session package sees it.
type Tab interface {
	// Ctx is the tab's long-lived context; actions run against it.
	Ctx() context.Context
	// Read snapshots the current page.
	Read(ctx context.Context) (Snapshot, error)
}

// Snapshot is one atomic read of the page's identity and markup.
type Snapshot struct {
	URL   string `json:"url"`
	Title string `json:"title"`
	HTML  string `json:"html"`
}

// Session is one live tab under flow control.
type Session struct {
	ID     string    `json:"id"`
	Host   string    `json:"host"`
	OpenAt time.Time `json:"open_at"`

	mu          sync.Mutex
	tab         Tab
	release     func()
	lastUsed    time.Time
	assertedAt  time.Time
	assertTTL   time.Duration
	log         []ActionRecord
	closing     bool
	pendingType bool // a type action is mid-flight; screenshots wait
	hold        bool // recording sessions are not idle-reaped
}

// ActionRecord is one evidence-log entry.
type ActionRecord struct {
	Seq    int           `json:"seq"`
	At     time.Time     `json:"at"`
	Action string        `json:"action"`
	OK     bool          `json:"ok"`
	TookMS int64         `json:"took_ms"`
	Detail string        `json:"detail,omitempty"`
	Took   time.Duration `json:"-"`
	Error  string        `json:"error,omitempty"`
}

func (m *Manager) Open(ctx context.Context, host string) (*Session, error) {
	m.mu.Lock()
	if len(m.sessions) >= m.opts.MaxSessions {
		m.mu.Unlock()
		return nil, errors.New("session limit reached")
	}
	m.nextID++
	id := fmt.Sprintf("s%d-%d", time.Now().Unix(), m.nextID)
	m.mu.Unlock()

	tab, release, err := m.acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("no tab available: %w", err)
	}
	s := &Session{
		ID: id, Host: host, OpenAt: time.Now(),
		tab: tab, release: release, lastUsed: time.Now(),
		assertTTL: m.opts.AssertTTL,
	}
	m.mu.Lock()
	m.sessions[id] = s
	m.mu.Unlock()
	m.logger().Info("session opened", "session", id, "host", host)
	return s, nil
}

func (m *Manager) Get(id string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return nil, fmt.Errorf("no such session %q", id)
	}
	return s, nil
}

func (m *Manager) Close(id string) error {
	m.mu.Lock()
	s, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("no such session %q", id)
	}
	delete(m.sessions, id)
	m.mu.Unlock()
	return s.close()
}

func (m *Manager) List() []*Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s)
	}
	return out
}

// Reap closes sessions idle past MaxIdle. Called from a ticker.
func (m *Manager) Reap() {
	m.mu.Lock()
	var expired []*Session
	for id, s := range m.sessions {
		s.mu.Lock()
		held := s.hold
		s.mu.Unlock()
		if held {
			continue
		}
		if time.Since(s.lastUsed) > m.opts.MaxIdle {
			expired = append(expired, s)
			delete(m.sessions, id)
		}
	}
	m.mu.Unlock()
	for _, s := range expired {
		m.logger().Info("reaping idle session", "session", s.ID)
		_ = s.close()
	}
}

func (s *Session) close() error {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return nil
	}
	s.closing = true
	tab, release := s.tab, s.release
	s.mu.Unlock()

	// Park the tab like the pool does between jobs.
	parkCtx, cancel := context.WithTimeout(tab.Ctx(), 5*time.Second)
	defer cancel()
	_ = runOn(parkCtx, chromedp.Navigate("about:blank"))
	release()
	return nil
}

// Snapshot returns the current page snapshot (read-only, no action logged).
func (s *Session) Snapshot() (Snapshot, error) {
	s.mu.Lock()
	tab := s.tab
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(tab.Ctx(), 10*time.Second)
	defer cancel()
	return tab.Read(ctx)
}

// Run executes one action and appends to the evidence log.
func (m *Manager) Run(ctx context.Context, s *Session, a Action) (any, error) {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return nil, errors.New("session closed")
	}
	if s.pendingType {
		s.mu.Unlock()
		return nil, errors.New("another action is mid-flight on this session")
	}
	s.lastUsed = time.Now()
	tab := s.tab
	s.mu.Unlock()

	// Browser commands must run on the tab's chromedp context: chromedp.Run
	// needs the *Context values (Allocator/Browser/Target) that live in
	// tab.Ctx(). Deriving the action context from the caller's HTTP request
	// context yields chromedp.ErrInvalidContext instantly. The request context
	// still bounds the action: a shorter caller deadline or early disconnect
	// cancels actx, while ActionTimeout bounds the tab side.
	actx, cancel, unwatch := m.actionContext(ctx, tab)
	defer unwatch()
	defer cancel()

	started := time.Now()
	res, err := m.dispatch(actx, s, tab, a)
	rec := ActionRecord{
		At: started, Action: a.Kind(), OK: err == nil,
		TookMS: time.Since(started).Milliseconds(),
		Detail: actionDetail(a), Took: time.Since(started),
	}
	if err != nil {
		rec.Error = err.Error()
	}
	s.mu.Lock()
	s.log = append(s.log, rec)
	if len(s.log) > m.opts.MaxLog {
		s.log = s.log[len(s.log)-m.opts.MaxLog:]
	}
	if err == nil {
		if as, ok := a.(AssertAction); ok && as.Passed(res) {
			s.assertedAt = time.Now()
		}
	}
	s.mu.Unlock()

	if err != nil {
		m.logger().Warn("action failed", "session", s.ID, "action", a.Kind(), "err", err)
	}
	return res, err
}

// SetHold prevents idle reaping while set. Recording sessions need this: a human
// interacting over VNC does not call actions, so lastUsed stays stale.
func (s *Session) SetHold(hold bool) {
	s.mu.Lock()
	s.hold = hold
	s.mu.Unlock()
}

// SetHold toggles idle-reap protection on an open session.
func (m *Manager) SetHold(id string, hold bool) error {
	s, err := m.Get(id)
	if err != nil {
		return err
	}
	s.SetHold(hold)
	return nil
}

// RecorderStarter is implemented by tabs that can install the gateway's
// interaction capture script (macro recording). The script text is
// gateway-authored — see internal/macro.CaptureScript.
type RecorderStarter interface {
	StartRecorder(ctx context.Context, script, binding string) (<-chan string, error)
}

// StartRecording installs the capture script on this session's tab and returns
// the event channel. It fails when the underlying tab does not support
// recording.
func (s *Session) StartRecording(ctx context.Context, script, binding string) (<-chan string, error) {
	s.mu.Lock()
	tab := s.tab
	s.mu.Unlock()
	rs, ok := tab.(RecorderStarter)
	if !ok {
		return nil, errors.New("this tab does not support interaction recording")
	}
	return rs.StartRecorder(ctx, script, binding)
}

// CanTypeCredentials reports whether an exact assertion passed recently.
func (s *Session) CanTypeCredentials() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Since(s.assertedAt) < s.assertTTL
}

// Records returns a copy of the evidence log, oldest first.
func (s *Session) Records() []ActionRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ActionRecord, len(s.log))
	copy(out, s.log)
	return out
}

// ---- actions ---------------------------------------------------------------

// Info is a safe external view of a live session for /sessions.
type Info struct {
	ID          string    `json:"id"`
	Host        string    `json:"host"`
	OpenAt      time.Time `json:"open_at"`
	IdleSeconds float64   `json:"idle_seconds"`
	Actions     int       `json:"actions"`
	CanTypeCred bool      `json:"can_type_credentials"`
}

// Info returns views of all live sessions.
func (m *Manager) Info() []Info {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Info, 0, len(m.sessions))
	for _, s := range m.sessions {
		s.mu.Lock()
		out = append(out, Info{
			ID:          s.ID,
			Host:        s.Host,
			OpenAt:      s.OpenAt,
			IdleSeconds: time.Since(s.lastUsed).Seconds(),
			Actions:     len(s.log),
			CanTypeCred: !s.assertedAt.IsZero() && time.Since(s.assertedAt) < s.assertTTL,
		})
		s.mu.Unlock()
	}
	return out
}

// Action is one typed primitive.
type Action interface {
	Kind() string
}

// NavigateAction points the session's tab at a URL.
type NavigateAction struct {
	URL string `json:"url"`
}

func (NavigateAction) Kind() string { return "navigate" }

// ClickAction clicks the first element matching a Playwright-style selector.
// Over the agent API only the single Selector form ever appeared, and it is
// now internal-only (macro replay). Candidates is the recorded selector
// ladder: dispatch tries each in order; the miss count is the drift signal.
type ClickAction struct {
	Selector   string   `json:"selector"`
	Candidates []string `json:"candidates,omitempty"`
}

func (ClickAction) Kind() string { return "click" }

// TypeAction types text into the focused element. Field classifies the input:
// "password" requires a fresh passing assertion; "text" does not.
type TypeAction struct {
	Text   string `json:"text"`
	Field  string `json:"field"`            // "text" | "password"
	Submit bool   `json:"submit,omitempty"` // press Enter after
}

func (TypeAction) Kind() string { return "type" }

// WaitAction blocks until a selector is visible or the URL matches a pattern.
type WaitAction struct {
	Selector  string `json:"selector,omitempty"`
	URLRegexp string `json:"url_regexp,omitempty"`
	TimeoutMS int64  `json:"timeout_ms,omitempty"`
}

func (WaitAction) Kind() string { return "wait" }

// ScreenshotAction captures the viewport as PNG.
type ScreenshotAction struct {
	// FullPage captures beyond the viewport.
	FullPage bool `json:"full_page,omitempty"`
}

func (ScreenshotAction) Kind() string { return "screenshot" }

// ContentAction reads the page snapshot (url/title/html).
type ContentAction struct{}

func (ContentAction) Kind() string { return "content" }

// AssertKind enumerates layered screen identity checks.
type AssertKind string

const (
	AssertURL       AssertKind = "url"       // exact regex match on location.href
	AssertTitle     AssertKind = "title"     // exact regex match on title
	AssertLandmark  AssertKind = "landmark"  // text visible in the DOM
	AssertLandmarkN AssertKind = "landmarks" // N-of-M weak landmarks
)

// AssertAction asserts something about the current screen. Only URL and title
// (exact matches) count as "exact" for credential-typing freshness.
type AssertAction struct {
	// AssertKind selects what to check. JSON field "expect" — the outer action
	// envelope already used "kind", so the inner field must not collide with it
	// after envelope stripping.
	AssertKind AssertKind `json:"expect"`
	Pattern    string     `json:"pattern,omitempty"`   // url/title regex
	Landmarks  []string   `json:"landmarks,omitempty"` // texts expected visible
	MinMatch   int        `json:"min_match,omitempty"` // for landmarks
}

func (a AssertAction) Kind() string { return "assert" }

// Passed reports whether a successful result satisfies the "exact" bar.
func (a AssertAction) Passed(res any) bool {
	if res == nil {
		return false
	}
	_, exact := res.(assertExactResult)
	return exact
}

type assertExactResult struct {
	Matched bool `json:"-"`
}

// ---- dispatch ---------------------------------------------------------------

func actionDetail(a Action) string {
	switch v := a.(type) {
	case NavigateAction:
		return v.URL
	case ClickAction:
		if len(v.Candidates) > 0 {
			return fmt.Sprintf("ladder=%d", len(v.Candidates))
		}
		return v.Selector
	case TypeAction:
		// Never record typed values for password fields.
		if v.Field == "password" {
			return fmt.Sprintf("field=password len=%d", len(v.Text))
		}
		return fmt.Sprintf("field=%s len=%d", v.Field, len(v.Text))
	case WaitAction:
		if v.Selector != "" {
			return v.Selector
		}
		return v.URLRegexp
	case AssertAction:
		return string(v.AssertKind)
	}
	return ""
}

// actionContext derives an action context from the tab's chromedp context,
// bounded by ActionTimeout. The caller's context (HTTP request) also cancels
// the action context, so a short request deadline still cuts the action off —
// without the request context ever being used to execute CDP commands.
func (m *Manager) actionContext(ctx context.Context, tab Tab) (context.Context, context.CancelFunc, func()) {
	actx, cancel := context.WithTimeout(tab.Ctx(), m.opts.ActionTimeout)
	// If the caller's deadline/disconnect fires first, cancel the tab-derived
	// context so the in-flight CDP command aborts immediately.
	callerDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			cancel()
		case <-callerDone:
		}
	}()
	return actx, cancel, func() { close(callerDone) }
}

// dispatch routes one action. The ctx is always derived from tab.Ctx() (see
// actionContext); dispatchers must not run CDP on any other context.
func (m *Manager) dispatch(ctx context.Context, s *Session, tab Tab, a Action) (any, error) {
	switch v := a.(type) {
	case NavigateAction:
		if m.opts.AllowNavigate != nil {
			if err := m.opts.AllowNavigate(v.URL); err != nil {
				return nil, err
			}
		}
		return nil, runOn(ctx, chromedp.Navigate(v.URL))
	case ClickAction:
		return dispatchClick(ctx, v)
	case TypeAction:
		return nil, m.dispatchType(ctx, s, v)
	case WaitAction:
		return nil, dispatchWait(ctx, tab, v)
	case ScreenshotAction:
		return dispatchScreenshot(ctx, v)
	case ContentAction:
		return tab.Read(ctx)
	case DownloadAction:
		return dispatchDownload(ctx, tab, v)
	case AssertAction:
		return dispatchAssert(ctx, tab, v)
	default:
		return nil, fmt.Errorf("unknown action kind %T", a)
	}
}

// runOn executes CDP actions on a tab. A package var so unit tests can stub
// the browser out; production always uses chromedp.Run.
var runOn = chromedp.Run

// clickCandidateTimeout bounds one ladder attempt; the outer action timeout
// still caps the whole dispatch.
const clickCandidateTimeout = 5 * time.Second

// dispatchClick clicks via the selector ladder. XPath candidates (starting
// with "//") go through DOM.performSearch (BySearch), CSS through
// querySelectorAll. Returns the number of ladder misses before the hit so the
// evidence log carries a drift signal.
func dispatchClick(ctx context.Context, a ClickAction) (any, error) {
	ladder := a.Candidates
	if len(ladder) == 0 {
		if a.Selector == "" {
			return nil, errors.New("click needs selector or candidates")
		}
		ladder = []string{a.Selector}
	}
	var misses []string
	for _, sel := range ladder {
		attempt, cancel := context.WithTimeout(ctx, clickCandidateTimeout)
		err := runOn(attempt, chromedp.Click(sel, bySearchIfXPath(sel), chromedp.NodeVisible))
		cancel()
		if err == nil {
			return ClickResult{Misses: len(misses), Matched: sel}, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		misses = append(misses, sel)
	}
	return nil, fmt.Errorf("no ladder candidate matched (%d tried): %s", len(misses), strings.Join(misses, ", "))
}

// ClickResult reports ladder drift for the evidence log.
type ClickResult struct {
	// Misses is how many ladder candidates failed before the hit.
	Misses int `json:"misses"`
	// Matched is the selector that finally matched.
	Matched string `json:"matched"`
}

func bySearchIfXPath(sel string) chromedp.QueryOption {
	if strings.HasPrefix(sel, "//") {
		return chromedp.BySearch
	}
	return chromedp.ByQueryAll
}

func (m *Manager) dispatchType(ctx context.Context, s *Session, a TypeAction) error {
	if a.Field == "password" && !s.CanTypeCredentials() {
		return errors.New("refusing to type credentials: no fresh exact assertion on this session")
	}
	if a.Text == "" {
		return errors.New("empty text")
	}
	if err := runOn(ctx, chromedp.SendKeys(":focus", a.Text)); err != nil {
		return err
	}
	if a.Submit {
		return runOn(ctx, chromedp.KeyEvent("\r"))
	}
	return nil
}

// actionDeadline reports the remaining budget of an action context (derived
// from the tab context with the caller's deadline layered on), or 0 when the
// context carries no deadline.
func actionDeadline(ctx context.Context) time.Duration {
	d, ok := ctx.Deadline()
	if !ok {
		return 0
	}
	return time.Until(d)
}

// waitBudget picks the wait's own timeout capped by the action deadline when
// one exists; 0 (no deadline) must not collapse the budget to an instantly
// expired context.
func waitBudget(a WaitAction, deadline time.Duration) time.Duration {
	if deadline <= 0 {
		return waitTimeout(a)
	}
	return min(waitTimeout(a), deadline)
}

func dispatchWait(ctx context.Context, tab Tab, a WaitAction) error {
	if a.Selector == "" && a.URLRegexp == "" {
		return errors.New("wait needs selector or url_regexp")
	}
	if a.Selector != "" {
		// The wait's shorter deadline caps the tab-derived action ctx so a
		// long selector wait cannot outlive its own budget.
		tctx, cancel := context.WithTimeout(tab.Ctx(), waitBudget(a, actionDeadline(ctx)))
		defer cancel()
		return runOn(tctx, chromedp.WaitVisible(a.Selector, chromedp.ByQueryAll))
	}
	re, err := regexp.Compile(a.URLRegexp)
	if err != nil {
		return fmt.Errorf("bad url_regexp: %w", err)
	}
	deadline := time.Now().Add(waitTimeout(a))
	for {
		snap, err := tab.Read(ctx)
		if err == nil && re.MatchString(snap.URL) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("url %q did not match %q in time", snapOrURL(snap), a.URLRegexp)
		}
		if err := sleepCtx(ctx, 250*time.Millisecond); err != nil {
			return err
		}
	}
}

func waitTimeout(a WaitAction) time.Duration {
	if a.TimeoutMS > 0 {
		return time.Duration(a.TimeoutMS) * time.Millisecond
	}
	return 15 * time.Second
}

func snapOrURL(s Snapshot) string {
	if s.URL != "" {
		return s.URL
	}
	return "(unreadable)"
}

func dispatchScreenshot(ctx context.Context, a ScreenshotAction) (any, error) {
	var buf []byte
	var act chromedp.Action
	if a.FullPage {
		act = chromedp.FullScreenshot(&buf, 100)
	} else {
		act = chromedp.CaptureScreenshot(&buf)
	}
	if err := runOn(ctx, act); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(buf)
	return ScreenshotResult{
		PNGBase64: base64.StdEncoding.EncodeToString(buf),
		Bytes:     len(buf),
		SHA256:    fmt.Sprintf("%x", sum),
	}, nil
}

type ScreenshotResult struct {
	PNGBase64 string `json:"png_base64"`
	Bytes     int    `json:"bytes"`
	SHA256    string `json:"sha256"`
}

func dispatchAssert(ctx context.Context, tab Tab, a AssertAction) (any, error) {
	snap, err := tab.Read(ctx)
	if err != nil {
		return nil, err
	}
	switch a.AssertKind {
	case AssertURL:
		if a.Pattern == "" {
			return nil, errors.New("url assert needs pattern")
		}
		re, err := regexp.Compile(a.Pattern)
		if err != nil {
			return nil, fmt.Errorf("bad pattern: %w", err)
		}
		ok := re.MatchString(snap.URL)
		return assertExactResult{Matched: ok}, boolErr("url", snap.URL, ok)
	case AssertTitle:
		if a.Pattern == "" {
			return nil, errors.New("title assert needs pattern")
		}
		re, err := regexp.Compile(a.Pattern)
		if err != nil {
			return nil, fmt.Errorf("bad pattern: %w", err)
		}
		ok := re.MatchString(snap.Title)
		return assertExactResult{Matched: ok}, boolErr("title", snap.Title, ok)
	case AssertLandmark:
		if len(a.Landmarks) != 1 {
			return nil, errors.New("landmark assert needs exactly one landmark")
		}
		ok := strings.Contains(snap.HTML, a.Landmarks[0])
		return assertResult{Matched: ok}, boolErr("landmark", a.Landmarks[0], ok)
	case AssertLandmarkN:
		if len(a.Landmarks) == 0 {
			return nil, errors.New("landmarks assert needs landmarks")
		}
		matched := 0
		for _, l := range a.Landmarks {
			if strings.Contains(snap.HTML, l) {
				matched++
			}
		}
		min := a.MinMatch
		if min == 0 {
			min = 1
		}
		ok := matched >= min
		return assertResult{Matched: ok, Count: matched}, boolErr("landmarks",
			fmt.Sprintf("%d/%d", matched, len(a.Landmarks)), ok)
	default:
		return nil, fmt.Errorf("unknown assert kind %q", a.AssertKind)
	}
}

type assertResult struct {
	Matched bool `json:"matched"`
	Count   int  `json:"count,omitempty"`
}

func boolErr(what, got string, ok bool) error {
	if ok {
		return nil
	}
	return fmt.Errorf("assertion %s failed: %q does not match", what, truncate(got, 120))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
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
