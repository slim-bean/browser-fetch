package macro

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testMacro() *Macro {
	return &Macro{
		ID:   "test-site-flow",
		Site: "test.com",
		Steps: []Step{
			{Action: json.RawMessage(`{"kind":"navigate","url":"https://test.com"}`)},
			{Action: json.RawMessage(`{"kind":"type","field":"password"}`),
				Secret: "op://Finance/Test/login"},
			{Action: json.RawMessage(`{"kind":"click"}`),
				Recorded: &Recorded{Element: &Element{Candidates: []string{"#submit"}}}},
			{Action: json.RawMessage(`{"kind":"pause"}`),
				Pause: &Pause{Reason: "2FA code"}},
		},
	}
}

func TestValidate(t *testing.T) {
	if err := testMacro().Validate(); err != nil {
		t.Fatalf("valid macro rejected: %v", err)
	}

	m := testMacro()
	m.Steps[1].Secret = ""
	if err := m.Validate(); err == nil {
		t.Fatal("type step without secret must be rejected")
	}

	m = testMacro()
	m.ID = ""
	if err := m.Validate(); err == nil {
		t.Fatal("missing id must be rejected")
	}

	m = testMacro()
	m.Steps = nil
	if err := m.Validate(); err == nil {
		t.Fatal("empty steps must be rejected")
	}

	m = testMacro()
	m.Steps[1].Action = json.RawMessage(`{"kind":"type","field":"password","text":"hunter2"}`)
	if err := m.Validate(); err == nil {
		t.Fatal("inline secret value must be rejected")
	}

	m = testMacro()
	m.Steps[3].Pause = &Pause{}
	if err := m.Validate(); err == nil {
		t.Fatal("pause without reason must be rejected")
	}
}

func TestStoreRoundTripAndApprove(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := testMacro()
	if err := s.Put(m, false); err != nil {
		t.Fatalf("put: %v", err)
	}
	// Duplicate put without overwrite must fail.
	if err := s.Put(m, false); err == nil {
		t.Fatal("duplicate put must fail")
	}
	if err := s.Put(m, true); err != nil {
		t.Fatalf("overwrite put: %v", err)
	}

	got, err := s.Get("test-site-flow")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != m.ID || len(got.Steps) != 4 {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if got.Approved != nil {
		t.Fatal("fresh macro must not be approved")
	}

	if err := s.Approve(m.ID, "ed"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	got, _ = s.Get(m.ID)
	if got.Approved == nil || got.Approved.By != "ed" {
		t.Fatalf("approval not persisted: %+v", got.Approved)
	}

	ids, err := s.List()
	if err != nil || len(ids) != 1 || ids[0] != "test-site-flow" {
		t.Fatalf("list: %v %v", ids, err)
	}

	// Files must be owner-only.
	info, err := os.Stat(filepath.Join(dir, "test-site-flow.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("macro file mode: %v", info.Mode())
	}
}

func TestHashProfile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Local State"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	h1, err := HashProfile(dir)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	h2, _ := HashProfile(dir)
	if h1 != h2 {
		t.Fatal("hash must be stable")
	}
	// Different profile → different hash.
	if err := os.WriteFile(filepath.Join(dir, "Preferences"), []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}
	h3, _ := HashProfile(dir)
	if h1 == h3 {
		t.Fatal("changed profile must change hash")
	}
	// Missing profile → error.
	if _, err := HashProfile(t.TempDir()); err == nil {
		t.Fatal("missing Local State must error")
	}
}

func TestValidateAutofillStep(t *testing.T) {
	m := &Macro{ID: "m", Site: "s", Steps: []Step{
		{Action: json.RawMessage(`{"kind":"autofill","field":"password","selector":"#pw"}`)},
		{Action: json.RawMessage(`{"kind":"click"}`)},
	}}
	if err := m.Validate(); err != nil {
		t.Fatalf("autofill step should validate without a secret: %v", err)
	}
	// Autofill carrying a value is refused like type steps.
	bad := &Macro{ID: "m", Site: "s", Steps: []Step{
		{Action: json.RawMessage(`{"kind":"autofill","field":"password","selector":"#pw","text":"hunter2"}`)},
	}}
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "not carry a value") {
		t.Fatalf("value-carrying autofill must be refused, got: %v", err)
	}
	// Autofill without a selector is refused (replay could not check anything).
	noSel := &Macro{ID: "m", Site: "s", Steps: []Step{
		{Action: json.RawMessage(`{"kind":"autofill"}`)},
	}}
	if err := noSel.Validate(); err == nil {
		t.Fatal("autofill without selector must be refused")
	}
}
