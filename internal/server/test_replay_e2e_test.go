package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/slim-bean/browser-fetch/internal/session"
)

// scriptTab executes navigate by recording it, everything else by failing
// like the real chromedp.Run does on a browser-less tab ctx.
type scriptTab struct{ lastURL string }

func (t *scriptTab) Ctx() context.Context { return context.Background() }

func TestTestReplayRunsStepsOnEditableSite(t *testing.T) {
	mgr := session.New(nil, func(ctx context.Context) (session.Tab, func(), error) {
		return failingStepTab{}, func() {}, nil
	}, session.Options{MaxSessions: 2, MaxIdle: time.Minute, ActionTimeout: 5 * time.Second, AssertTTL: 30 * time.Second, MaxLog: 50})

	s, ms := testServerWithMacros(t)
	s.sessions = mgr
	s.admin = NewAdmin(ms.store, "admin-secret")

	req := httptest.NewRequest("POST", "/action", strings.NewReader("op=enable_agent_edit&site=example.test&t=admin-secret"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.admin.Handler().ServeHTTP(httptest.NewRecorder(), req)

	body := `{"id":"d1","site":"example.test","steps":[{"action":{"kind":"navigate","url":"https://example.test/"}}]}`
	w := httptest.NewRecorder()
	req2 := httptest.NewRequest("POST", "/macro/put", strings.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	s.handleMacroPut(w, req2)
	if w.Code != 201 {
		t.Fatalf("put: %d %s", w.Code, w.Body.String())
	}

	w2 := httptest.NewRecorder()
	s.handleMacroReplay(w2, mustJSON(t, "POST", "/macro/replay", map[string]string{"macro_id": "d1"}))
	if w2.Code == 409 && strings.Contains(w2.Body.String(), "not approved") {
		t.Fatalf("test replay must bypass the approval gate: %d %s", w2.Code, w2.Body.String())
	}
	if !strings.Contains(w2.Body.String(), `"test_replay":true`) {
		t.Fatalf("replay must be labeled test_replay: %d %s", w2.Code, w2.Body.String())
	}
	// The navigate step runs; the session records it.
	var resp struct {
		TestReplay bool                   `json:"test_replay"`
		Evidence   []session.ActionRecord `json:"evidence"`
	}
	if err := json.Unmarshal(w2.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, w2.Body.String())
	}
	if len(resp.Evidence) == 0 || resp.Evidence[0].Action != "replay_marker" {
		t.Fatalf("evidence must start with the replay marker: %+v", resp.Evidence)
	}
	if len(resp.Evidence) < 2 || resp.Evidence[1].Action != "navigate" {
		t.Fatalf("navigate step must have been attempted: %+v", resp.Evidence)
	}
	if resp.Evidence[1].OK {
		t.Fatalf("browser-less stub tab must fail the navigate (proving the step ran): %+v", resp.Evidence[1])
	}
}
