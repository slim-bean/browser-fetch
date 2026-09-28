package macro

import (
	"encoding/json"
	"testing"
)

func TestAddEventRedactsValues(t *testing.T) {
	r := NewRecorder("id", "site.com")
	// A capture payload that (against protocol) carries a value/text must not
	// leak it into the recorded step.
	payloads := []string{
		`{"ev":"type","field":"password","value":"hunter2","element":{"candidates":["#pw"]},"url":"https://site.com","at":"2026-09-29T12:00:00Z"}`,
		`{"ev":"type","field":"text","text":"hunter2","element":{"candidates":["#user"]},"url":"https://site.com"}`,
	}
	for _, p := range payloads {
		if err := r.AddEvent([]byte(p)); err != nil {
			t.Fatalf("add: %v", err)
		}
	}
	draft, _ := json.Marshal(r.Draft())
	draftStr := string(draft)
	for _, banned := range []string{"hunter2", `"value"`} {
		if contains(draftStr, banned) {
			t.Fatalf("draft contains %q:\n%s", banned, draftStr)
		}
	}
	steps := r.Steps()
	if len(steps) != 2 {
		t.Fatalf("want 2 steps, got %d", len(steps))
	}
	if steps[0].Secret != "" {
		t.Fatal("recorded type step must not invent a secret")
	}
}

func TestAddEventClickAndSubmit(t *testing.T) {
	r := NewRecorder("id", "site.com")
	if err := r.AddEvent([]byte(`{"ev":"click","element":{"role":"button","text":"Sign in","candidates":["#login"]}}`)); err != nil {
		t.Fatal(err)
	}
	if err := r.AddEvent([]byte(`{"ev":"submit"}`)); err != nil {
		t.Fatal(err)
	}
	steps := r.Steps()
	if len(steps) != 2 {
		t.Fatalf("want 2 steps, got %d", len(steps))
	}
	var a struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(steps[0].Action, &a); err != nil || a.Kind != "click" {
		t.Fatalf("click step: %s %v", steps[0].Action, err)
	}
	if err := json.Unmarshal(steps[1].Action, &a); err != nil || a.Kind != "click" {
		t.Fatalf("submit→click step: %s %v", steps[1].Action, err)
	}
	if steps[1].Recorded == nil {
		t.Fatal("submit step should carry Recorded context")
	}
}

func TestAddEventUnknownKind(t *testing.T) {
	r := NewRecorder("id", "site.com")
	if err := r.AddEvent([]byte(`{"ev":"scroll"}`)); err == nil {
		t.Fatal("unknown event must be rejected")
	}
	if err := r.AddEvent([]byte(`not json`)); err == nil {
		t.Fatal("bad json must be rejected")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
