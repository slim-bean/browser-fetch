package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/slim-bean/browser-fetch/internal/macro"
)

// TestAgentApproveRemoved pins the Phase 2a separation-of-authority decision:
// the agent-facing POST /macro/approve must never approve again. It serves
// 410 and leaves the macro untouched — approval authority lives only in the
// admin band, which the agent cannot reach (separate listener + token).
func TestAgentApproveRemoved(t *testing.T) {
	s, ms := testServerWithMacros(t)
	rec := macro.NewRecorder("m-agent", "example.com")
	_ = rec.AddEvent([]byte(`{"ev":"click","element":{"candidates":["#login"]},"url":"https://example.com/"}`))
	if err := ms.store.Put(rec.Draft(), false); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	s.handleAgentApproveGone(w, mustJSON(t, "POST", "/macro/approve", map[string]string{"macro_id": "m-agent"}))
	if w.Code != 410 || !strings.Contains(w.Body.String(), "approval_requires_admin_band") {
		t.Fatalf("agent approve must 410, got %d %s", w.Code, w.Body.String())
	}
	m, err := ms.store.Get("m-agent")
	if err != nil || m.Approved != nil {
		t.Fatalf("agent approve must not approve: %+v %v", m, err)
	}
}

// TestAdminEditClearsApproval pins: any edit through the admin band resets
// the macro to draft. Approval certifies the exact step list.
func TestAdminEditClearsApproval(t *testing.T) {
	_, ms := testServerWithMacros(t)
	rec := macro.NewRecorder("m-edit", "example.com")
	for _, ev := range []string{
		`{"ev":"click","element":{"candidates":["#one"],"role":"button","text":"One"},"url":"https://example.com/"}`,
		`{"ev":"click","element":{"candidates":["#two"],"role":"button","text":"Two"},"url":"https://example.com/"}`,
		`{"ev":"click","element":{"candidates":["#three"],"role":"button","text":"Three"},"url":"https://example.com/"}`,
	} {
		_ = rec.AddEvent([]byte(ev))
	}
	if err := ms.store.Put(rec.Draft(), false); err != nil {
		t.Fatal(err)
	}
	if err := ms.store.Approve("m-edit", "ed"); err != nil {
		t.Fatal(err)
	}

	admin := NewAdmin(ms.store, "admin-secret")
	h := admin.Handler()

	// Drop the middle step; approval must clear.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, adminForm("drop_step", "m-edit", "admin-secret", "1"))
	if w.Code != 303 {
		t.Fatalf("drop_step: %d %s", w.Code, w.Body.String())
	}
	m, err := ms.store.Get("m-edit")
	if err != nil {
		t.Fatal(err)
	}
	if m.Approved != nil {
		t.Fatal("edited macro must be a draft again")
	}
	if len(m.Steps) != 2 || string(m.Steps[0].Recorded.Element.Candidates[0]) != "#one" ||
		string(m.Steps[1].Recorded.Element.Candidates[0]) != "#three" {
		t.Fatalf("wrong steps after drop: %d", len(m.Steps))
	}

	// Re-approve, then move_up on the last step; approval clears again.
	if err := ms.store.Approve("m-edit", "ed"); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, adminForm("move_up", "m-edit", "admin-secret", "1"))
	if w.Code != 303 {
		t.Fatalf("move_up: %d %s", w.Code, w.Body.String())
	}
	m, _ = ms.store.Get("m-edit")
	if m.Approved != nil {
		t.Fatal("reorder must clear approval")
	}
	if string(m.Steps[0].Recorded.Element.Candidates[0]) != "#three" {
		t.Fatalf("move_up did not reorder: %v", m.Steps[0].Recorded.Element.Candidates)
	}

	// Revoke keeps steps but clears approval.
	if err := ms.store.Approve("m-edit", "ed"); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, adminForm("revoke", "m-edit", "admin-secret"))
	if w.Code != 303 {
		t.Fatalf("revoke: %d %s", w.Code, w.Body.String())
	}
	m, _ = ms.store.Get("m-edit")
	if m.Approved != nil || len(m.Steps) != 2 {
		t.Fatalf("revoke must clear approval, keep steps: %d steps", len(m.Steps))
	}

	// Delete removes the macro.
	w = httptest.NewRecorder()
	h.ServeHTTP(w, adminForm("delete", "m-edit", "admin-secret"))
	if w.Code != 303 {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	if _, err := ms.store.Get("m-edit"); err == nil {
		t.Fatal("deleted macro must be gone")
	}
}

// TestAdminSetSecretPinsOpRef pins: set_secret attaches an op:// reference
// (never a value) to a recorded type step, clears approval, and refuses
// non-op payloads. clear_secret empties the reference again.
func TestAdminSetSecretPinsOpRef(t *testing.T) {
	_, ms := testServerWithMacros(t)
	rec := macro.NewRecorder("m-secret", "example.com")
	_ = rec.AddEvent([]byte(`{"ev":"click","element":{"candidates":["#login"],"role":"button","text":"Log in"},"url":"https://example.com/"}`))
	_ = rec.AddEvent([]byte(`{"ev":"type","field":"password"}`))
	if err := ms.store.Put(rec.Draft(), false); err != nil {
		t.Fatalf("draft with pending secret must put: %v", err)
	}

	admin := NewAdmin(ms.store, "admin-secret")
	h := admin.Handler()

	// Non-op reference refused (fail re-renders the view with 200 + error text).
	w := httptest.NewRecorder()
	req := adminFormWithRef("set_secret", "m-secret", "admin-secret", "1", "hunter2")
	h.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), "op:// reference") {
		t.Fatalf("non-op ref must be refused with an explanation: %d %s", w.Code, w.Body.String())
	}

	// A value must never be stored: only op:// refs pass.
	w = httptest.NewRecorder()
	req = adminFormWithRef("set_secret", "m-secret", "admin-secret", "1", "op://fin-browser/TireRack/password")
	h.ServeHTTP(w, req)
	if w.Code != 303 {
		t.Fatalf("set_secret: %d %s", w.Code, w.Body.String())
	}
	m, err := ms.store.Get("m-secret")
	if err != nil {
		t.Fatal(err)
	}
	if m.Steps[1].Secret != "op://fin-browser/TireRack/password" {
		t.Fatalf("ref not attached: %q", m.Steps[1].Secret)
	}
	if m.Approved != nil {
		t.Fatal("set_secret must clear approval")
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, adminForm("clear_secret", "m-secret", "admin-secret", "1"))
	if w.Code != 303 {
		t.Fatalf("clear_secret: %d %s", w.Code, w.Body.String())
	}
	m, _ = ms.store.Get("m-secret")
	if m.Steps[1].Secret != "" {
		t.Fatalf("ref not cleared: %q", m.Steps[1].Secret)
	}
}

// TestAdminTokenRequired pins the band's authentication: wrong or missing
// token can neither read nor mutate.
func TestAdminTokenRequired(t *testing.T) {
	_, ms := testServerWithMacros(t)
	rec := macro.NewRecorder("m-sec", "example.com")
	_ = rec.AddEvent([]byte(`{"ev":"click","element":{"candidates":["#login"]},"url":"https://example.com/"}`))
	if err := ms.store.Put(rec.Draft(), false); err != nil {
		t.Fatal(err)
	}
	admin := NewAdmin(ms.store, "admin-secret")
	h := admin.Handler()

	cases := []struct {
		name string
		req  *httptest.ResponseRecorder
	}{}
	_ = cases

	// Missing token on dashboard.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 401 {
		t.Fatalf("dashboard without token: %d", w.Code)
	}
	// Wrong token on a mutating action.
	w = httptest.NewRecorder()
	h.ServeHTTP(w, adminForm("approve", "m-sec", "wrong-token"))
	if w.Code != 401 {
		t.Fatalf("action with wrong token: %d", w.Code)
	}
	m, _ := ms.store.Get("m-sec")
	if m.Approved != nil {
		t.Fatal("wrong token must not approve")
	}
	// Correct token via form field works (plain-browser flow).
	w = httptest.NewRecorder()
	h.ServeHTTP(w, adminForm("approve", "m-sec", "admin-secret"))
	if w.Code != 303 {
		t.Fatalf("action with form token: %d %s", w.Code, w.Body.String())
	}
	// Correct token via header works too.
	w = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer admin-secret")
	h.ServeHTTP(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "m-sec") {
		t.Fatalf("dashboard with header token: %d", w.Code)
	}
}

// TestAdminDashboardShowsState is a smoke test for the review UI: drafts and
// approved macros render with their state, and replay outcomes appear.
func TestAdminDashboardShowsState(t *testing.T) {
	_, ms := testServerWithMacros(t)
	rec := macro.NewRecorder("m-ui", "example.com")
	_ = rec.AddEvent([]byte(`{"ev":"click","element":{"candidates":["#login"],"role":"button","text":"Sign in"},"url":"https://example.com/"}`))
	if err := ms.store.Put(rec.Draft(), false); err != nil {
		t.Fatal(err)
	}
	admin := NewAdmin(ms.store, "admin-secret")
	admin.RecordReplay("m-ui", false, 1200, 0, "selector miss", false, "")

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/?t=admin-secret", nil)
	admin.Handler().ServeHTTP(w, req)
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, "m-ui") || !strings.Contains(body, "draft") {
		t.Fatalf("dashboard missing draft macro: %d", w.Code)
	}
	if !strings.Contains(body, "aborted at step 0") {
		t.Fatal("dashboard must surface replay failures")
	}

	// The macro detail view renders steps with an edit warning for
	// fingerprint-less steps.
	w = httptest.NewRecorder()
	admin.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/macro?id=m-ui&t=admin-secret", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "autofill") && !strings.Contains(w.Body.String(), "click") {
		t.Fatalf("macro view: %d", w.Code)
	}
}

// TestOrphanParkingAndRecovery pins the draft-preservation fix: when the
// draft fails to store, its steps are parked as an orphan the admin band can
// recover, instead of being discarded (this lost two live recordings before
// the fix).
func TestOrphanParkingAndRecovery(t *testing.T) {
	_, ms := testServerWithMacros(t)
	rec := macro.NewRecorder("m-orphan", "example.com")
	_ = rec.AddEvent([]byte(`{"ev":"click","element":{"candidates":["#login"],"role":"button","text":"Sign in"},"url":"https://example.com/"}`))
	draft := rec.Draft()

	// Simulate a failed store: the regular Put refuses (id collision with an
	// existing macro), so record/stop parks an orphan.
	if err := ms.store.Put(&macro.Macro{ID: "m-orphan", Site: "example.com",
		Steps: []macro.Step{{Action: []byte(`{"kind":"wait"}`)}}}, false); err != nil {
		t.Fatal(err)
	}
	if err := ms.store.Put(draft, false); err == nil {
		t.Fatal("expected the colliding Put to fail")
	}
	if err := ms.store.PutOrphan(draft); err != nil {
		t.Fatal(err)
	}

	orphans, err := ms.store.ListOrphans()
	if err != nil || len(orphans) != 1 || orphans[0] != "m-orphan" {
		t.Fatalf("orphan not listed: %v %v", orphans, err)
	}

	// Regular List must not gain a duplicate entry from the orphan file: the
	// blocker macro "m-orphan" is listed once, the parked draft is not listed.
	ids, _ := ms.store.List()
	if len(ids) != 1 || ids[0] != "m-orphan" {
		t.Fatalf("orphan leaked into the regular macro list: %v", ids)
	}

	// Promote through the admin band; the old draft is replaced (it was not
	// approved) and the orphan disappears.
	admin := NewAdmin(ms.store, "admin-secret")
	w := httptest.NewRecorder()
	admin.Handler().ServeHTTP(w, adminForm("promote_orphan", "m-orphan", "admin-secret"))
	if w.Code != 303 {
		t.Fatalf("promote_orphan: %d %s", w.Code, w.Body.String())
	}
	m, err := ms.store.Get("m-orphan")
	if err != nil || len(m.Steps) != 1 || m.Approved != nil {
		t.Fatalf("promote failed: %+v %v", m, err)
	}
	if orphans, _ := ms.store.ListOrphans(); len(orphans) != 0 {
		t.Fatal("orphan must be consumed by promotion")
	}

	// Discard path.
	if err := ms.store.PutOrphan(draft); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	admin.Handler().ServeHTTP(w, adminForm("discard_orphan", "m-orphan", "admin-secret"))
	if w.Code != 303 {
		t.Fatalf("discard_orphan: %d %s", w.Code, w.Body.String())
	}
	if orphans, _ := ms.store.ListOrphans(); len(orphans) != 0 {
		t.Fatal("orphan must be gone after discard")
	}
}

// TestAdminPromoteRefusesOverApproved pins: an approved macro is never
// implicitly replaced by an orphan promotion — the human must revoke first.
func TestAdminPromoteRefusesOverApproved(t *testing.T) {
	_, ms := testServerWithMacros(t)
	rec := macro.NewRecorder("m-guard", "example.com")
	_ = rec.AddEvent([]byte(`{"ev":"click","element":{"candidates":["#login"]},"url":"https://example.com/"}`))
	if err := ms.store.Put(rec.Draft(), false); err != nil {
		t.Fatal(err)
	}
	if err := ms.store.Approve("m-guard", "ed"); err != nil {
		t.Fatal(err)
	}
	if err := ms.store.PutOrphan(rec.Draft()); err != nil {
		t.Fatal(err)
	}
	admin := NewAdmin(ms.store, "admin-secret")
	w := httptest.NewRecorder()
	admin.Handler().ServeHTTP(w, adminForm("promote_orphan", "m-guard", "admin-secret"))
	// The handler re-renders the view with an error (200 + error text) or
	// redirects; either way the approved macro must be untouched.
	m, err := ms.store.Get("m-guard")
	if err != nil || m.Approved == nil {
		t.Fatalf("approved macro must survive a refused promotion: %v %v", m, err)
	}
	if orphans, _ := ms.store.ListOrphans(); len(orphans) != 1 {
		t.Fatal("refused orphan must remain parked")
	}
}

// adminFormWithRef builds a set_secret action form carrying a ref field.
func adminFormWithRef(op, id, token, index, ref string) *http.Request {
	form := url.Values{"t": {token}, "op": {op}, "id": {id}, "index": {index}, "ref": {ref}}
	req := httptest.NewRequest("POST", "/action", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}
