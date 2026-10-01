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

func TestAddEventAutofill(t *testing.T) {
	r := NewRecorder("id", "site.com")
	if err := r.AddEvent([]byte(`{"ev":"autofill","field":"password","element":{"candidates":["#newPassLogin","div form > input"],"role":"password"},"url":"https://site.com/login"}`)); err != nil {
		t.Fatalf("add: %v", err)
	}
	steps := r.Steps()
	if len(steps) != 1 {
		t.Fatalf("want 1 step, got %d", len(steps))
	}
	var a struct {
		Kind     string `json:"kind"`
		Field    string `json:"field"`
		Selector string `json:"selector"`
	}
	if err := json.Unmarshal(steps[0].Action, &a); err != nil {
		t.Fatal(err)
	}
	if a.Kind != "autofill" || a.Field != "password" || a.Selector != "#newPassLogin" {
		t.Fatalf("autofill step: %s", steps[0].Action)
	}
	if steps[0].Secret != "" {
		t.Fatal("autofill step must not carry a secret reference")
	}
	draft, _ := json.Marshal(r.Draft())
	if contains(string(draft), `"kind":"type"`) {
		t.Fatalf("autofill must not become a type step:\n%s", draft)
	}
}

// Regression: tirerack.com live run 2026-09-29. Ed dismissed a popup with a
// keypress (Escape/Enter); Chrome autofilled the login form within the old
// script's 1500ms any-key window, so the fill was misclassified as a type
// step at step 0 and the validator refused the draft. Classification must be
// per-element real text entry, never a global keystroke timer.
func TestAddEventKeypressBeforeAutofillIsNotTyping(t *testing.T) {
	r := NewRecorder("id", "site.com")
	// Capture script must not contain a global timing window on lastKey.
	script := CaptureScript
	if contains(script, "lastKey") {
		t.Fatal("capture script still keys classification off a global keystroke timer")
	}
	if !contains(script, "isTrusted") || !contains(script, "WeakMap") {
		t.Fatal("capture script must classify via trusted per-element keystroke tracking")
	}
	// Any keydown before autofill without text entry on the field itself is a fill.
	// The fill path is exercised in TestAddEventAutofill; here the assertion
	// is that the SCRIPT can no longer misclassify: classification is
	// per-element trusted text entry, with no global keystroke timer.
	if err := r.AddEvent([]byte(`{"ev":"autofill","field":"text","element":{"candidates":["#newEmailLogin"]}}`)); err != nil {
		t.Fatalf("add: %v", err)
	}
	steps := r.Steps()
	if len(steps) != 1 {
		t.Fatalf("want 1 step, got %d", len(steps))
	}
	var a struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(steps[0].Action, &a); err != nil {
		t.Fatal(err)
	}
	if a.Kind != "autofill" {
		t.Fatalf("keyless fill must stay autofill, got %s", steps[0].Action)
	}
}

func TestAddEventNav(t *testing.T) {
	r := NewRecorder("id", "site.com")
	if err := r.AddEvent([]byte(`{"ev":"nav","url":"https://site.com/login","at":"2026-01-01T00:00:00Z"}`)); err != nil {
		t.Fatalf("add: %v", err)
	}
	steps := r.Steps()
	if len(steps) != 1 {
		t.Fatalf("want 1 step, got %d", len(steps))
	}
	var a struct {
		Kind string `json:"kind"`
		URL  string `json:"url"`
	}
	if err := json.Unmarshal(steps[0].Action, &a); err != nil {
		t.Fatal(err)
	}
	if a.Kind != "navigate" || a.URL != "https://site.com/login" {
		t.Fatalf("nav step: %s", steps[0].Action)
	}
}

func TestAddEventNavSkipsAboutBlank(t *testing.T) {
	r := NewRecorder("id", "site.com")
	for _, u := range []string{"about:blank", "data:text/html,x"} {
		if err := r.AddEvent([]byte(`{"ev":"nav","url":"` + u + `"}`)); err != nil {
			t.Fatalf("add %s: %v", u, err)
		}
	}
	if steps := r.Steps(); len(steps) != 0 {
		t.Fatalf("about:/data: navs must be skipped, got %d steps", len(steps))
	}
}

func TestAddEventNavCollapsesRedirectChain(t *testing.T) {
	r := NewRecorder("id", "site.com")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(r.AddEvent([]byte(`{"ev":"nav","url":"https://site.com/a"}`)))
	must(r.AddEvent([]byte(`{"ev":"nav","url":"https://site.com/b"}`))) // redirect
	must(r.AddEvent([]byte(`{"ev":"click","element":{"candidates":["#btn"],"role":"button"}}`)))
	must(r.AddEvent([]byte(`{"ev":"nav","url":"https://site.com/after-click"}`)))
	steps := r.Steps()
	if len(steps) != 3 {
		t.Fatalf("want 3 steps (nav, click, nav), got %d", len(steps))
	}
	var a struct {
		Kind string `json:"kind"`
		URL  string `json:"url"`
	}
	if err := json.Unmarshal(steps[0].Action, &a); err != nil {
		t.Fatal(err)
	}
	if a.URL != "https://site.com/b" {
		t.Fatalf("redirect chain must keep the final URL, got %s", a.URL)
	}
}

func TestCaptureScriptEmitsNav(t *testing.T) {
	if !contains(CaptureScript, `ev: 'nav'`) {
		t.Fatal("capture script must report navigations; recordings without nav steps cannot replay from a fresh session")
	}
	if !contains(CaptureScript, "__bfNavReported") {
		t.Fatal("capture script must dedupe per-document nav reports")
	}
}
