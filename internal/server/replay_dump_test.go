package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/slim-bean/browser-fetch/internal/macro"
	"github.com/slim-bean/browser-fetch/internal/session"
)

// failingStepTab is a Tab whose CDP actions fail for click steps; snapshots
// return a scripted page identity.
type failingStepTab struct{}

func (failingStepTab) Ctx() context.Context { return context.Background() }
func (failingStepTab) Read(ctx context.Context) (session.Snapshot, error) {
	return session.Snapshot{URL: "https://example.test/login", Title: "Sign In", HTML: "<html/>"}, nil
}

func TestReplay409CarriesFailureDump(t *testing.T) {
	mgr := session.New(nil, func(ctx context.Context) (session.Tab, func(), error) {
		return failingStepTab{}, func() {}, nil
	}, session.Options{MaxSessions: 2, MaxIdle: time.Minute, ActionTimeout: 5 * time.Second, AssertTTL: 30 * time.Second, MaxLog: 50})

	s, ms := testServerWithMacros(t)
	s.sessions = mgr
	s.admin = NewAdmin(ms.store, "admin-secret")

	// Approved macro whose step 1 is a click no ladder candidate satisfies
	// (the stub runOn fails every chromedp.Run).
	rec := macro.NewRecorder("dump-macro", "example.test")
	_ = rec.AddEvent([]byte(`{"ev":"navigate","url":"https://example.test/login"}`))
	_ = rec.AddEvent([]byte(`{"ev":"click","element":{"candidates":["#nope"],"role":"button","text":"Submit"},"url":"https://example.test/login"}`))
	m := rec.Draft()
	if err := ms.store.Put(m, false); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	NewAdmin(ms.store, "admin-secret").Handler().ServeHTTP(w, adminForm("approve", "dump-macro", "admin-secret"))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("approve: %d %s", w.Code, w.Body.String())
	}

	// Session package's runOn is stubbed per-package; in server tests it is
	// the real chromedp.Run and fails instantly on a browser-less tab ctx —
	// which is exactly the failure mode we want for the click step.
	w = httptest.NewRecorder()
	s.handleMacroReplay(w, mustJSON(t, "POST", "/macro/replay", map[string]string{"macro_id": "dump-macro"}))
	if w.Code != http.StatusConflict {
		t.Fatalf("replay must abort with 409, got %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		AbortedAtStep int                `json:"aborted_at_step"`
		StepKind      string             `json:"step_kind"`
		Cause         string             `json:"cause"`
		FailureDump   *macro.FailureDump `json:"failure_dump"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, w.Body.String())
	}
	if resp.FailureDump == nil {
		t.Fatal("409 must carry failure_dump")
	}
	if resp.AbortedAtStep != 0 || resp.StepKind != "click" {
		t.Fatalf("abort location wrong: %+v", resp)
	}
	if resp.FailureDump.URL != "https://example.test/login" || resp.FailureDump.Title != "Sign In" {
		t.Fatalf("dump identity wrong: %+v", resp.FailureDump)
	}
	// On a browser-less tab the probe itself fails ("invalid context") and
	// lands in dump_errors — the dump must surface the probe failure rather
	// than silently omitting it.
	if resp.FailureDump.URL != "https://example.test/login" {
		t.Fatalf("dump identity wrong: %+v", resp.FailureDump)
	}
	if len(resp.FailureDump.DumpErrs) == 0 {
		t.Fatalf("failed probe must be surfaced in dump_errors: %+v", resp.FailureDump)
	}
}
