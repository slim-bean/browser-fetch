package macro

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/slim-bean/browser-fetch/internal/session"
)

// fakeExec records executed actions and can be scripted to fail.
type fakeExec struct {
	run     []session.Action
	failOn  map[int]error // index into run
	results []any
}

func (f *fakeExec) Run(ctx context.Context, sess *session.Session, a session.Action) (any, error) {
	i := len(f.run)
	f.run = append(f.run, a)
	if err, ok := f.failOn[i]; ok {
		return nil, err
	}
	var res any
	if len(f.results) > 0 {
		res, f.results = f.results[0], f.results[1:]
	}
	return res, nil
}

func approvedMacro(steps ...Step) *Macro {
	m := testMacro()
	m.Steps = steps
	m.Approved = &Approval{By: "ed", At: time.Now()}
	return m
}

func nav(url string) Step {
	return Step{Action: json.RawMessage(`{"kind":"navigate","url":"` + url + `"}`)}
}
func click(elem string) Step {
	return Step{Action: json.RawMessage(`{"kind":"click"}`),
		Recorded: &Recorded{Element: &Element{Candidates: []string{elem}, Role: "button", Text: "Sign in"}}}
}
func typeStep(secret string) Step {
	return Step{Action: json.RawMessage(`{"kind":"type","field":"password"}`), Secret: secret}
}
func assertStep(pattern string) Step {
	return Step{Action: json.RawMessage(`{"kind":"assert","expect":"url","pattern":"` + pattern + `"}`)}
}

func TestReplayHappyPath(t *testing.T) {
	exec := &fakeExec{}
	r := &Replayer{Secrets: stubResolver{}, MinTextLenForXPath: 3}
	m := approvedMacro(
		nav("https://test.com/login"),
		typeStep("op://Finance/Test/login"),
		click("#submit"),
		assertStep(`accounts`),
	)
	if err := r.Run(context.Background(), exec, nil, m); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(exec.run) != 4 {
		t.Fatalf("want 4 executed actions, got %d", len(exec.run))
	}
	// The type action must carry the resolved value, not the reference.
	ta, ok := exec.run[1].(session.TypeAction)
	if !ok {
		t.Fatalf("step 1 is %T", exec.run[1])
	}
	if ta.Text != "op-resolved" || ta.Field != "password" {
		t.Fatalf("unexpected type action: %+v", ta)
	}
	// The click action must carry the ladder: recorded candidate + text XPath.
	ca, ok := exec.run[2].(session.ClickAction)
	if !ok {
		t.Fatalf("step 2 is %T", exec.run[2])
	}
	if len(ca.Candidates) != 2 || !strings.Contains(ca.Candidates[1], "Sign in") {
		t.Fatalf("unexpected click ladder: %v", ca.Candidates)
	}
	if ca.Selector != "" {
		t.Fatal("ladder click should not set Selector")
	}
}

func TestReplayRefusesUnapproved(t *testing.T) {
	exec := &fakeExec{}
	r := &Replayer{}
	m := testMacro() // no approval
	if err := r.Run(context.Background(), exec, nil, m); err == nil || !strings.Contains(err.Error(), "not approved") {
		t.Fatalf("unapproved macro must be refused, got %v", err)
	}
	if len(exec.run) != 0 {
		t.Fatal("nothing may execute for an unapproved macro")
	}
}

func TestReplayAbortsAtFailedStep(t *testing.T) {
	exec := &fakeExec{failOn: map[int]error{1: errors.New("no ladder candidate matched (2 tried): #submit, //button")}}
	r := &Replayer{MinTextLenForXPath: 3}
	m := approvedMacro(
		nav("https://test.com"),
		click("#submit"),
		nav("https://test.com/accounts"),
	)
	err := r.Run(context.Background(), exec, nil, m)
	var re *ReplayError
	if !errors.As(err, &re) {
		t.Fatalf("want ReplayError, got %v", err)
	}
	if re.Step != 1 || re.Kind != "click" {
		t.Fatalf("abort at wrong step: %+v", re)
	}
	// Step 2 must not have executed.
	if len(exec.run) != 2 {
		t.Fatalf("replay must stop at the failed step; executed %d", len(exec.run))
	}
}

func TestReplayTypeRequiresSecretAndResolver(t *testing.T) {
	r := &Replayer{}
	exec := &fakeExec{}
	m := approvedMacro(typeStep("op://x/y"))
	if err := r.Run(context.Background(), exec, nil, m); err == nil || !strings.Contains(err.Error(), "no secret resolver") {
		t.Fatalf("want resolver error, got %v", err)
	}
	r = &Replayer{Secrets: stubResolver{err: errors.New("locked")}}
	if err := r.Run(context.Background(), exec, nil, m); err == nil || !strings.Contains(err.Error(), "secret resolve failed") {
		t.Fatalf("want resolve error, got %v", err)
	}
	// Nothing executed in either failure mode.
	if len(exec.run) != 0 {
		t.Fatal("failed secret resolution must not reach the browser")
	}
}

type stubResolver struct{ err error }

func (s stubResolver) Resolve(ctx context.Context, ref string) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	return "op-resolved", nil
}

func TestReplayPauseFlow(t *testing.T) {
	var pausedReason string
	pauser := fakePauser{fn: func(id, reason string) { pausedReason = reason }}
	r := &Replayer{Pauser: pauser, MinTextLenForXPath: 3}
	pause := Step{
		Action:       json.RawMessage(`{"kind":"pause"}`),
		Pause:        &Pause{Reason: "2FA code"},
		ResumeAssert: json.RawMessage(`{"kind":"assert","expect":"url","pattern":"accounts"}`),
	}
	m := approvedMacro(pause, nav("https://test.com/accounts"))
	exec := &fakeExec{}
	if err := r.Run(context.Background(), exec, nil, m); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if pausedReason != "2FA code" {
		t.Fatalf("pause reason not passed: %q", pausedReason)
	}
	// resume_assert executed as a session action.
	if len(exec.run) != 2 {
		t.Fatalf("want resume assert + navigate, got %d", len(exec.run))
	}
	if _, ok := exec.run[0].(session.AssertAction); !ok {
		t.Fatalf("first exec should be the resume assert, got %T", exec.run[0])
	}
}

func TestReplayPauseWithoutChannel(t *testing.T) {
	r := &Replayer{}
	m := approvedMacro(Step{Action: json.RawMessage(`{"kind":"pause"}`), Pause: &Pause{Reason: "otp"}})
	err := r.Run(context.Background(), &fakeExec{}, nil, m)
	var re *ReplayError
	if !errors.As(err, &re) || !strings.Contains(re.Cause, "no pause channel") {
		t.Fatalf("want pause-channel error, got %v", err)
	}
}

type fakePauser struct {
	fn func(id, reason string)
}

func (f fakePauser) WaitResume(ctx context.Context, macroID, reason string, timeout time.Duration) error {
	f.fn(macroID, reason)
	return nil
}

func TestTextXPaths(t *testing.T) {
	got := textXPaths(&Element{Role: "button", Text: "Sign in"}, 3)
	if len(got) != 1 || got[0] != `//button[contains(normalize-space(.), 'Sign in')]` {
		t.Fatalf("unexpected xpath: %v", got)
	}
	if textXPaths(&Element{Role: "button", Text: "OK"}, 3) != nil {
		t.Fatal("short text must not generate ambiguous xpaths")
	}
	got = textXPaths(&Element{Role: "textbox", Text: "Username"}, 3)
	if len(got) != 2 || !strings.Contains(got[0], "@aria-label") {
		t.Fatalf("textbox xpaths: %v", got)
	}
}

func TestDecodeMacroAction(t *testing.T) {
	a, err := decodeMacroAction(json.RawMessage(`{"kind":"assert","expect":"url","pattern":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	aa, ok := a.(session.AssertAction)
	if !ok || aa.AssertKind != session.AssertURL || aa.Pattern != "x" {
		t.Fatalf("assert decode: %+v %v", a, err)
	}
	if _, err := decodeMacroAction(json.RawMessage(`{"kind":"run_js","js":"1"}`)); err == nil {
		t.Fatal("unknown kinds must be rejected")
	}
	if _, err := decodeMacroAction(json.RawMessage(`{"kind":"type","field":"password","text":"secret"}`)); err == nil {
		// decode succeeds but Validate() rejects inline values before replay;
		// belt and braces: the replayer overwrites Text with the resolved
		// secret anyway.
		t.Log("type decodes; macro Validate blocks inline values")
	}
}

func TestReplayErrorFormatting(t *testing.T) {
	e := &ReplayError{Step: 3, Kind: "click", Cause: "boom", Drift: 2}
	if !strings.Contains(e.Error(), "step 3") || !strings.Contains(e.Error(), "click") {
		t.Fatalf("format: %s", e.Error())
	}
	_ = fmt.Sprint(e)
}
