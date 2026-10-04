package macro

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/slim-bean/browser-fetch/internal/session"
)

// The replayed type action must target the recorded element directly instead
// of relying on the ":focus" path, which stalls on focus-managing pages.
func TestReplayTypeTargetsRecordedElement(t *testing.T) {
	exec := &fakeExec{}
	r := &Replayer{Secrets: stubResolver{}, MinTextLenForXPath: 3}
	st := Step{
		Action: json.RawMessage(`{"kind":"type","field":"text"}`),
		Secret: "op://Finance/Test/login",
		Recorded: &Recorded{Element: &Element{Candidates: []string{"#emailLogin", "input#emailLogin"}, Role: "textbox"}},
	}
	m := approvedMacro(nav("https://test.com/login"), st)
	if err := r.Run(context.Background(), exec, nil, m); err != nil {
		t.Fatalf("replay: %v", err)
	}
	ta, ok := exec.run[1].(session.TypeAction)
	if !ok {
		t.Fatalf("step 1 is %T", exec.run[1])
	}
	if ta.Target != "#emailLogin" {
		t.Fatalf("want target #emailLogin, got %q", ta.Target)
	}
	if ta.Text != "op-resolved" {
		t.Fatalf("unexpected text: %d chars", len(ta.Text))
	}
	if !strings.Contains(ta.Field, "text") {
		t.Fatalf("field: %q", ta.Field)
	}
}

// Without a recorded element the type action must leave Target empty so the
// ":focus" fallback still applies.
func TestReplayTypeWithoutRecordedKeepsFocusPath(t *testing.T) {
	exec := &fakeExec{}
	r := &Replayer{Secrets: stubResolver{}, MinTextLenForXPath: 3}
	m := approvedMacro(nav("https://test.com/login"), typeStep("op://Finance/Test/login"))
	if err := r.Run(context.Background(), exec, nil, m); err != nil {
		t.Fatalf("replay: %v", err)
	}
	ta := exec.run[1].(session.TypeAction)
	if ta.Target != "" {
		t.Fatalf("want empty target, got %q", ta.Target)
	}
}
