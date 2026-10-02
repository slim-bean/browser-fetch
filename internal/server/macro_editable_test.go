package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/slim-bean/browser-fetch/internal/macro"
)

// putJSON PUTs an agent-authored draft through the real handler.
func agentPut(t *testing.T, s *Server, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleMacroPut(w, mustJSON(t, "POST", "/macro/put", body))
	return w
}

func TestMacroPutRequiresEditableSite(t *testing.T) {
	s, _ := testServerWithMacros(t)
	w := agentPut(t, s, map[string]any{
		"id": "agent-1", "site": "example.test",
		"steps": []map[string]any{
			{"action": map[string]any{"kind": "navigate", "url": "https://example.test/"}},
		},
	})
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "agent_edit_refused") {
		t.Fatalf("put on non-editable site: %d %s", w.Code, w.Body.String())
	}
}

func TestMacroPutAndEditLifecycle(t *testing.T) {
	s, ms := testServerWithMacros(t)

	// Grant via the admin band (the only authority surface). A grant without
	// a site param must fail.
	admin := NewAdmin(ms.store, "admin-secret")
	w := httptest.NewRecorder()
	admin.Handler().ServeHTTP(w, adminForm("enable_agent_edit", "", "admin-secret"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("grant without site must 400, got %d", w.Code)
	}
	req := httptest.NewRequest("POST", "/action", strings.NewReader(
		"op=enable_agent_edit&site=example.test&t=admin-secret"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	admin.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("grant: %d %s", w.Code, w.Body.String())
	}
	if !ms.store.AgentEditable("example.test") {
		t.Fatal("grant did not take effect")
	}

	// Agent authors a draft.
	w = agentPut(t, s, map[string]any{
		"id": "agent-1", "site": "example.test", "description": "agent-authored login",
		"steps": []map[string]any{
			{"action": map[string]any{"kind": "navigate", "url": "https://example.test/login"}},
			{"action": map[string]any{"kind": "autofill", "selector": "#user"}},
		},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("put: %d %s", w.Code, w.Body.String())
	}
	stored, err := ms.store.Get("agent-1")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Approved != nil || len(stored.Steps) != 2 {
		t.Fatalf("unexpected draft: approved=%v steps=%d", stored.Approved != nil, len(stored.Steps))
	}

	// Replay refuses production replay of the unapproved draft? No: the site
	// is editable, so this is a TEST replay — it proceeds far enough to fail
	// on missing session machinery (not on approval). The bare Server has a
	// nil session manager, so guard with recover.
	w = httptest.NewRecorder()
	func() {
		defer func() { _ = recover() }() // nil session.Manager in this test rig
		s.handleMacroReplay(w, mustJSON(t, "POST", "/macro/replay", map[string]string{"macro_id": "agent-1"}))
	}()
	if w.Code == http.StatusForbidden {
		t.Fatalf("editable-site draft must not hit the approval gate: %s", w.Body.String())
	}

	// Agent edits the draft (replace step 1).
	w = httptest.NewRecorder()
	s.handleMacroEdit(w, mustJSON(t, "POST", "/macro/edit", map[string]any{
		"macro_id": "agent-1",
		"ops": []map[string]any{
			{"op": "replace_step", "index": 1,
				"step": map[string]any{"kind": "autofill", "selector": "#email"}},
			{"op": "append_step",
				"step": map[string]any{"kind": "click"}},
		},
	}))
	if w.Code != http.StatusOK {
		t.Fatalf("edit: %d %s", w.Code, w.Body.String())
	}
	stored, _ = ms.store.Get("agent-1")
	if len(stored.Steps) != 3 {
		t.Fatalf("edit not applied: %d steps", len(stored.Steps))
	}
	var autofill struct {
		Kind     string `json:"kind"`
		Selector string `json:"selector"`
	}
	_ = json.Unmarshal(stored.Steps[1].Action, &autofill)
	if autofill.Selector != "#email" {
		t.Fatalf("replace_step not applied: %s", stored.Steps[1].Action)
	}

	// Admin approval after review, then the macro is frozen to the agent.
	req = httptest.NewRequest("POST", "/action", strings.NewReader(
		"op=approve&id=agent-1&t=admin-secret"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	admin.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("approve: %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	s.handleMacroEdit(w, mustJSON(t, "POST", "/macro/edit", map[string]any{
		"macro_id": "agent-1",
		"ops":      []map[string]any{{"op": "drop_step", "index": 0}},
	}))
	if w.Code != http.StatusForbidden {
		t.Fatalf("edit of approved macro must 403: %d %s", w.Code, w.Body.String())
	}

	// Revoke the site grant: drafts stop being test-replayable.
	req = httptest.NewRequest("POST", "/action", strings.NewReader(
		"op=disable_agent_edit&site=example.test&t=admin-secret"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	admin.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("revoke: %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	func() {
		defer func() { _ = recover() }() // nil session.Manager in this test rig
		s.handleMacroReplay(w, mustJSON(t, "POST", "/macro/replay", map[string]string{"macro_id": "agent-1"}))
	}()
	// The macro was approved above, so production replay applies — the
	// approval gate stays closed only for unapproved drafts. Revoke + edit a
	// fresh draft to prove the gate.
	w = agentPut(t, s, map[string]any{
		"id": "agent-2", "site": "example.test",
		"steps": []map[string]any{{"action": map[string]any{"kind": "navigate", "url": "https://example.test/"}}},
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("put after revoke: %d %s", w.Code, w.Body.String())
	}
}

func TestMacroEditValidationRejectsBadResult(t *testing.T) {
	_, ms := testServerWithMacros(t)
	if err := ms.store.EnableAgentEditable("example.test", "ed", ""); err != nil {
		t.Fatal(err)
	}
	if err := ms.store.AgentPut(&macro.Macro{
		ID: "v1", Site: "example.test", Created: time.Now().UTC(),
		Steps: []macro.Step{{Action: json.RawMessage(`{"kind":"navigate","url":"https://example.test/"}`)}},
	}, false); err != nil {
		t.Fatal(err)
	}
	// Append a type step carrying a literal value: Validate must refuse and
	// the mutation must not persist.
	s, _ := testServerWithMacros(t)
	w := httptest.NewRecorder()
	s.handleMacroEdit(w, mustJSON(t, "POST", "/macro/edit", map[string]any{
		"macro_id": "v1",
		"ops": []map[string]any{
			{"op": "append_step",
				"step": map[string]any{"kind": "type", "text": "hunter2"}},
		},
	}))
	if w.Code != http.StatusForbidden {
		t.Fatalf("value-carrying type step must be refused: %d %s", w.Code, w.Body.String())
	}
	stored, err := ms.store.Get("v1")
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Steps) != 1 {
		t.Fatalf("refused edit must not persist: %d steps", len(stored.Steps))
	}
}

func TestAgentEditableSitesOnDashboard(t *testing.T) {
	_, ms := testServerWithMacros(t)
	if err := ms.store.EnableAgentEditable("example.test", "ed", "practice"); err != nil {
		t.Fatal(err)
	}
	admin := NewAdmin(ms.store, "admin-secret")
	w := httptest.NewRecorder()
	admin.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/?t=admin-secret", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("dashboard: %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Agent-editable sites") ||
		!strings.Contains(w.Body.String(), "example.test") {
		t.Fatalf("dashboard must show the grant: %s", w.Body.String())
	}
}
