package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"

	"strings"
	"testing"
	"time"

	"github.com/slim-bean/browser-fetch/internal/macro"
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

	// Approve an unknown macro -> 400.
	w := httptest.NewRecorder()
	s.handleMacroApprove(w, mustJSON(t, "POST", "/macro/approve", map[string]string{"macro_id": "nope"}))
	if w.Code != 400 {
		t.Fatalf("approve unknown: %d %s", w.Code, w.Body.String())
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

	// Approve, then replay fails on missing session machinery (500/400, not
	// 403) — approval gate is in front.
	w = httptest.NewRecorder()
	s.handleMacroApprove(w, mustJSON(t, "POST", "/macro/approve", map[string]string{"macro_id": "test-macro", "by": "ed"}))
	if w.Code != 200 {
		t.Fatalf("approve: %d %s", w.Code, w.Body.String())
	}
	stored, err := ms.store.Get("test-macro")
	if err != nil || stored.Approved == nil || stored.Approved.By != "ed" {
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
