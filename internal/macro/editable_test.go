package macro

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testDraft(id, site string) *Macro {
	return &Macro{
		ID:      id,
		Site:    site,
		Created: time.Now().UTC(),
		Steps: []Step{
			{Action: json.RawMessage(`{"kind":"navigate","url":"https://example.test/login"}`)},
			{Action: json.RawMessage(`{"kind":"autofill","selector":"#user"}`)},
		},
	}
}

func TestAgentPutRequiresEditableSite(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := testDraft("m1", "example.test")
	if err := s.AgentPut(m, false); err == nil {
		t.Fatal("AgentPut must refuse non-editable sites")
	}
	if _, err := os.Stat(filepath.Join(dir, "m1.json")); err == nil {
		t.Fatal("refused AgentPut must not write a file")
	}
}

func TestAgentPutAndEditOnEditableSite(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.EnableAgentEditable("https://www.Example.test/", "ed", "practice site"); err != nil {
		t.Fatal(err)
	}
	// Site normalization: the grant must match the macro's site spelling.
	if !s.AgentEditable("example.test") {
		t.Fatal("normalized site should be editable after grant")
	}

	m := testDraft("m1", "example.test")
	if err := s.AgentPut(m, false); err != nil {
		t.Fatalf("AgentPut on editable site: %v", err)
	}
	stored, err := s.Get("m1")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Approved != nil {
		t.Fatal("agent-put macro must be a draft")
	}

	// Edit the draft: replace step 0.
	newAction := []byte(`{"kind":"navigate","url":"https://example.test/signin"}`)
	if err := s.AgentUpdate("m1", func(m *Macro) {
		m.Steps[0] = Step{Action: json.RawMessage(appendCopy(newAction))}
	}); err != nil {
		t.Fatalf("AgentUpdate: %v", err)
	}
	stored, _ = s.Get("m1")
	var nav struct {
		Kind string `json:"kind"`
		URL  string `json:"url"`
	}
	if err := json.Unmarshal(stored.Steps[0].Action, &nav); err != nil {
		t.Fatal(err)
	}
	if nav.URL != "https://example.test/signin" {
		t.Fatalf("edit not applied: %s", stored.Steps[0].Action)
	}
	if stored.Approved != nil {
		t.Fatal("edited draft must stay unapproved")
	}
}

func TestAgentCannotOverwriteApprovedMacro(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.EnableAgentEditable("example.test", "ed", ""); err != nil {
		t.Fatal(err)
	}
	m := testDraft("m1", "example.test")
	if err := s.Put(m, false); err != nil {
		t.Fatal(err)
	}
	if err := s.Approve("m1", "ed"); err != nil {
		t.Fatal(err)
	}

	if err := s.AgentPut(testDraft("m1", "example.test"), false); err == nil {
		t.Fatal("AgentPut must not overwrite an approved macro")
	}
	if err := s.AgentPut(testDraft("m1", "example.test"), true); err == nil {
		t.Fatal("AgentPut(overwrite) must not overwrite an approved macro")
	}
	if err := s.AgentUpdate("m1", func(m *Macro) {}); err == nil {
		t.Fatal("AgentUpdate must refuse approved macros")
	}
	stored, _ := s.Get("m1")
	if stored.Approved == nil {
		t.Fatal("approved macro must remain approved after refused agent writes")
	}
}

// appendCopy returns a fresh mutable copy of b (RawMessage must not alias
// literals shared across writes).
func appendCopy(b []byte) []byte {
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

func TestAgentEditRefusedAfterSiteRevoked(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.EnableAgentEditable("example.test", "ed", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.AgentPut(testDraft("m1", "example.test"), false); err != nil {
		t.Fatal(err)
	}
	if err := s.DisableAgentEditable("example.test"); err != nil {
		t.Fatal(err)
	}
	if s.AgentEditable("example.test") {
		t.Fatal("site should no longer be editable")
	}
	if err := s.AgentUpdate("m1", func(m *Macro) {}); err == nil {
		t.Fatal("AgentUpdate must refuse after revocation")
	}
	// A draft stored while editable stays but is not test-replayable.
	m, _ := s.Get("m1")
	if s.AgentTestReplayable(m) {
		t.Fatal("draft on revoked site must not be test-replayable")
	}
}

func TestAgentTestReplayableRules(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)
	if err := s.EnableAgentEditable("example.test", "ed", ""); err != nil {
		t.Fatal(err)
	}
	draft := testDraft("d1", "example.test")
	if !s.AgentTestReplayable(draft) {
		t.Fatal("editable-site draft should be test-replayable")
	}
	draft.Approved = &Approval{By: "ed", At: time.Now()}
	if s.AgentTestReplayable(draft) {
		t.Fatal("approved macro replays the production path, not the test path")
	}
	other := testDraft("d2", "blocked.test")
	if s.AgentTestReplayable(other) {
		t.Fatal("non-editable-site draft must not be test-replayable")
	}
}

func TestRegistryPersistsAndSurvivesReload(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)
	if err := s.EnableAgentEditable("example.test", "ed", "note"); err != nil {
		t.Fatal(err)
	}
	// Reload from disk in a fresh store.
	s2, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !s2.AgentEditable("example.test") {
		t.Fatal("grant must persist across store reloads")
	}
	sites := s2.AgentEditableSites()
	if len(sites) != 1 || sites[0].Site != "example.test" || sites[0].GrantedBy != "ed" {
		t.Fatalf("unexpected registry content: %+v", sites)
	}
}

func TestRegistryFilePermissions(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)
	if err := s.EnableAgentEditable("example.test", "ed", ""); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "agent-editable-sites.json"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("registry file must be 0600, got %o", perm)
	}
}
