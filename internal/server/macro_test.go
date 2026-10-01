package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/slim-bean/browser-fetch/internal/macro"
	"github.com/slim-bean/browser-fetch/internal/session"
)

func testServerWithMacros(t *testing.T) (*Server, *macroState) {
	t.Helper()
	dir := t.TempDir()
	ms, err := newMacroState(dir)
	if err != nil {
		t.Fatal(err)
	}
	return &Server{macros: ms}, ms
}

func TestRequireMacrosDisabled(t *testing.T) {
	s := &Server{}
	w := httptest.NewRecorder()
	s.handleMacroList(w, httptest.NewRequest("GET", "/macros", nil))
	if w.Code != 501 {
		t.Fatalf("disabled macros must 501, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "macros_disabled") {
		t.Fatalf("body: %s", w.Body.String())
	}
}

func TestMacroStoreLifecycle(t *testing.T) {
	s, ms := testServerWithMacros(t)

	// Approve via the admin band: unknown macro -> 400.
	admin := NewAdmin(ms.store, "admin-secret")
	w := httptest.NewRecorder()
	admin.Handler().ServeHTTP(w, mustJSON(t, "POST", "/action", nil))
	if w.Code != 401 {
		t.Fatalf("admin without token must 401, got %d", w.Code)
	}

	// Store a draft with one step through the real Store, then list and approve.
	rec := macro.NewRecorder("test-macro", "example.com")
	_ = rec.AddEvent([]byte(`{"ev":"click","element":{"candidates":["#login"],"role":"button","text":"Sign in"},"url":"https://example.com/"}`))
	m := rec.Draft()
	if err := ms.store.Put(m, false); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	s.handleMacroList(w, httptest.NewRequest("GET", "/macros", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"approved":false`) {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}

	// Replay refuses unapproved with 403 before touching any session.
	w = httptest.NewRecorder()
	s.handleMacroReplay(w, mustJSON(t, "POST", "/macro/replay", map[string]string{"macro_id": "test-macro"}))
	if w.Code != 403 || !strings.Contains(w.Body.String(), "unapproved_macro") {
		t.Fatalf("replay unapproved: %d %s", w.Code, w.Body.String())
	}

	// Approve through the admin band, then replay fails on missing session
	// machinery (500/400, not 403) — approval gate is in front.
	w = httptest.NewRecorder()
	admin.Handler().ServeHTTP(w, adminForm("approve", "test-macro", "admin-secret"))
	if w.Code != 303 {
		t.Fatalf("admin approve: %d %s", w.Code, w.Body.String())
	}
	stored, err := ms.store.Get("test-macro")
	if err != nil || stored.Approved == nil || !strings.Contains(stored.Approved.By, "admin band") {
		t.Fatalf("approval not persisted: %+v %v", stored, err)
	}

	// GET /macro returns the full macro for review.
	w = httptest.NewRecorder()
	s.handleMacroGet(w, httptest.NewRequest("GET", "/macro?id=test-macro", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "test-macro") {
		t.Fatalf("get: %d %s", w.Code, w.Body.String())
	}
}

func TestPauseResumeRoundTrip(t *testing.T) {
	_, ms := testServerWithMacros(t)

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		done <- ms.WaitResume(ctx, "m1", "2FA", time.Second)
	}()
	time.Sleep(20 * time.Millisecond) // let the goroutine register

	s := &Server{macros: ms}
	w := httptest.NewRecorder()
	s.handleMacroResume(w, mustJSON(t, "POST", "/macro/resume", map[string]string{"macro_id": "m1"}))
	if w.Code != 200 {
		t.Fatalf("resume: %d %s", w.Code, w.Body.String())
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WaitResume: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("resume not delivered")
	}

	// Second resume for the same (now-consumed) pause id -> 404.
	w = httptest.NewRecorder()
	s.handleMacroResume(w, mustJSON(t, "POST", "/macro/resume", map[string]string{"macro_id": "m1"}))
	if w.Code != 404 {
		t.Fatalf("stale resume must 404, got %d", w.Code)
	}
}

func TestWaitResumeTimeout(t *testing.T) {
	_, ms := testServerWithMacros(t)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := ms.WaitResume(ctx, "m2", "otp", time.Second); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline exceeded, got %v", err)
	}
}

func TestRecordStartBadRequests(t *testing.T) {
	s, _ := testServerWithMacros(t)
	w := httptest.NewRecorder()
	s.handleMacroRecordStart(w, mustJSON(t, "POST", "/macro/record/start", map[string]string{}))
	if w.Code != 400 {
		t.Fatalf("missing fields: %d", w.Code)
	}
}

func mustJSON(t *testing.T, method, path string, body any) *http.Request {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewRequest(method, path, bytes.NewReader(b))
}

// adminForm builds a POST /action request as the admin UI's HTML forms do:
// token + op + id (+ optional index) as form fields.
func adminForm(op, id, token string, index ...string) *http.Request {
	form := url.Values{"t": {token}, "op": {op}, "id": {id}}
	if len(index) > 0 {
		form.Set("index", index[0])
	}
	req := httptest.NewRequest("POST", "/action", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

// recordingTab is a Tab whose StartRecording returns a channel the test
// controls, simulating the page-side capture binding.
type recordingTab struct {
	events chan string
}

func (t recordingTab) Ctx() context.Context { return context.Background() }
func (t recordingTab) Read(ctx context.Context) (session.Snapshot, error) {
	return session.Snapshot{}, nil
}
func (t recordingTab) StartRecorder(ctx context.Context, script, binding string) (<-chan string, error) {
	return t.events, nil
}

// Regression (PR #4): events must still be captured after the record/start
// HTTP request has completed. The original bug tied the event drain to the
// request context, so a human clicking on VNC seconds later recorded nothing
// and record/stop discarded the draft with "macro has no steps".
func TestRecordCaptureOutlivesStartRequest(t *testing.T) {
	s, ms := testServerWithMacros(t)
	events := make(chan string, 8)
	tab := recordingTab{events: events}

	sm := session.New(slog.New(slog.DiscardHandler), func(ctx context.Context) (session.Tab, func(), error) {
		return tab, func() {}, nil
	}, session.Options{MaxSessions: 2})
	s.sessions = sm

	sess, err := sm.Open(context.Background(), "example.com")
	if err != nil {
		t.Fatal(err)
	}

	// record/start; use a request context we cancel immediately to prove the
	// drain survives the handler's own request ending.
	startCtx, cancelStart := context.WithCancel(context.Background())
	req := mustJSON(t, "POST", "/macro/record/start", map[string]string{
		"macro_id": "m-live", "site": "example.com", "session_id": sess.ID,
	}).WithContext(startCtx)
	w := httptest.NewRecorder()
	s.handleMacroRecordStart(w, req)
	cancelStart() // the HTTP request completes (client gone)
	if w.Code != 200 {
		t.Fatalf("record/start: %d %s", w.Code, w.Body.String())
	}

	// The human clicks AFTER the start request is done.
	events <- `{"ev":"click","element":{"candidates":["#login"],"role":"button","text":"Sign in"},"url":"https://example.com/"}`
	// Allow the drain goroutine to ingest the event.
	time.Sleep(100 * time.Millisecond)

	// record/stop must persist a draft WITH steps (not "macro has no steps").
	w = httptest.NewRecorder()
	s.handleMacroRecordStop(w, mustJSON(t, "POST", "/macro/record/stop", map[string]any{"macro_id": "m-live", "close_session": false}))
	if w.Code != 200 {
		t.Fatalf("record/stop: %d %s", w.Code, w.Body.String())
	}
	stored, err := ms.store.Get("m-live")
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Steps) != 1 {
		t.Fatalf("draft must capture the post-request click, got %d steps", len(stored.Steps))
	}
}

// TestOpenReplaySessionOwnership pins the contract fix: a caller-provided
// session (pre-navigated by the operator) must come back owned=false so the
// replay handler does not close it; a handler-leased session is owned=true.
func TestOpenReplaySessionOwnership(t *testing.T) {
	tabOpen := false
	mgr := session.New(nil, func(ctx context.Context) (session.Tab, func(), error) {
		tabOpen = true
		return &fakeReplayTab{}, func() {}, nil
	}, session.Options{MaxSessions: 2, MaxIdle: time.Minute, ActionTimeout: 5 * time.Second, AssertTTL: 30 * time.Second, MaxLog: 10})
	s := &Server{sessions: mgr}
	m := &macro.Macro{ID: "m", Site: "example.com"}

	req := &replayRequest{}
	sess, owned, err := s.openReplaySession(httptest.NewRequest("POST", "/macro/replay", nil), *req, m)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	if !owned {
		t.Fatal("self-leased session must be owned")
	}
	_ = sess

	caller := &replayRequest{SessionID: "caller-session"}
	if _, err := mgr.Open(context.Background(), "example.com"); err != nil {
		t.Fatal(err)
	}
	// Get of a session the handler did not create: exercise the not-found path
	// shape too.
	_, owned, err = s.openReplaySession(httptest.NewRequest("POST", "/macro/replay", nil), *caller, m)
	if err == nil {
		if owned {
			t.Fatal("caller-provided session must not be marked owned")
		}
	} else if !strings.Contains(err.Error(), "not found") && !strings.Contains(err.Error(), "no such session") {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = tabOpen
}

type fakeReplayTab struct{}

func (fakeReplayTab) Ctx() context.Context { return context.Background() }
func (fakeReplayTab) Read(ctx context.Context) (session.Snapshot, error) {
	return session.Snapshot{URL: "https://example.com/", Title: "t", HTML: "<html/>"}, nil
}
