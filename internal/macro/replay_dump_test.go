package macro

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/slim-bean/browser-fetch/internal/session"
)

// newReplaySession builds a real session.Manager with a scripted-tab acquire
// so Snapshot() works without a browser. All CDP actions run through the
// session package's runOn stub (no-op recorder).
func newReplaySession(t *testing.T, snap session.Snapshot) (*session.Manager, *session.Session) {
	t.Helper()
	tab := &scriptedTab{ctx: context.Background(), snap: snap}
	m := session.New(nil, func(ctx context.Context) (session.Tab, func(), error) {
		return tab, func() {}, nil
	}, session.Options{
		MaxSessions: 2, MaxIdle: time.Minute, ActionTimeout: 5 * time.Second,
		AssertTTL: 30 * time.Second, MaxLog: 10,
	})
	s, err := m.Open(context.Background(), "example.test")
	if err != nil {
		t.Fatal(err)
	}
	return m, s
}

// scriptedTab is a session.Tab whose snapshot is scripted; CDP actions run
// through the session package's runOn stub, installed by the test.
type scriptedTab struct {
	session.Tab
	ctx  context.Context
	snap session.Snapshot
}

func (f *scriptedTab) Ctx() context.Context                               { return f.ctx }
func (f *scriptedTab) Read(ctx context.Context) (session.Snapshot, error) { return f.snap, nil }

func TestReplayFailureDumpCapturesPageState(t *testing.T) {
	_, sess := newReplaySession(t, session.Snapshot{
		URL:   "https://www.tirerack.com/register/LoginServlet",
		Title: "Sign In",
	})
	exec := &fakeExec{
		fields: []session.FieldState{
			{Selector: "#emailLogin", Found: true, Filled: false, ValueLength: 0, InputType: "email"},
		},
		failOn: map[int]error{1: errors.New(`field "#emailLogin" was never filled (autofill did not arrive in 10ms)`)},
	}
	r := &Replayer{MinTextLenForXPath: 3}
	m := approvedMacro(
		nav("https://www.tirerack.com/register/LoginServlet"),
		Step{Action: json.RawMessage(`{"kind":"autofill","selector":"#emailLogin"}`)},
	)
	err := r.Run(context.Background(), exec, sess, m)
	var re *ReplayError
	if !errors.As(err, &re) {
		t.Fatalf("want ReplayError, got %v", err)
	}
	if re.FailureDump == nil {
		t.Fatal("failed autofill step must carry a failure dump")
	}
	if re.FailureDump.URL != "https://www.tirerack.com/register/LoginServlet" || re.FailureDump.Title != "Sign In" {
		t.Fatalf("dump identity wrong: %+v", re.FailureDump)
	}
	if len(re.FailureDump.Fields) != 1 {
		t.Fatalf("want 1 probed field, got %d", len(re.FailureDump.Fields))
	}
	fs := re.FailureDump.Fields[0]
	if fs.Selector != "#emailLogin" || !fs.Found || fs.Filled || fs.ValueLength != 0 {
		t.Fatalf("field probe state wrong: %+v", fs)
	}
	// The field probe must be visible in the executor's action stream.
	last := exec.run[len(exec.run)-1]
	if !isFieldsProbe(last, "#emailLogin") {
		t.Fatalf("field probe missing from executor stream, last: %T", last)
	}
}

func TestReplayFailureDumpOnLadderClickFailure(t *testing.T) {
	_, sess := newReplaySession(t, session.Snapshot{URL: "https://test.com/login", Title: "T"})
	exec := &fakeExec{failOn: map[int]error{1: errors.New("no ladder candidate matched (1 tried): #submit")}}
	r := &Replayer{MinTextLenForXPath: 3}
	m := approvedMacro(
		nav("https://test.com/login"),
		click("#submit"),
	)
	err := r.Run(context.Background(), exec, sess, m)
	var re *ReplayError
	if !errors.As(err, &re) {
		t.Fatalf("want ReplayError, got %v", err)
	}
	if re.FailureDump == nil || re.FailureDump.URL != "https://test.com/login" {
		t.Fatalf("failed click step must carry a dump with page identity: %+v", re.FailureDump)
	}
	// Click steps probe the recorded candidates.
	if len(exec.run) < 3 || !isFieldsProbe(exec.run[len(exec.run)-1], "#submit") {
		t.Fatalf("dump must probe the recorded candidate, got %T", exec.run[len(exec.run)-1])
	}
}

func TestReplayFailureDumpOmitsSelectorsWhenNone(t *testing.T) {
	_, sess := newReplaySession(t, session.Snapshot{URL: "https://x.test/", Title: "X"})
	// Failing navigate step: no selectors in the action, no recorded element.
	exec := &fakeExec{failOn: map[int]error{0: errors.New("boom")}}
	r := &Replayer{}
	m := approvedMacro(nav("https://x.test/"))
	err := r.Run(context.Background(), exec, sess, m)
	var re *ReplayError
	if !errors.As(err, &re) {
		t.Fatalf("want ReplayError, got %v", err)
	}
	if re.FailureDump == nil {
		t.Fatal("dump must exist even without selectors")
	}
	if len(re.FailureDump.Fields) != 0 {
		t.Fatalf("navigate step has no selectors to probe: %+v", re.FailureDump.Fields)
	}
	if re.FailureDump.URL != "https://x.test/" {
		t.Fatalf("dump URL wrong: %+v", re.FailureDump)
	}
}

func isFieldsProbe(a session.Action, wantSel string) bool {
	fa, ok := a.(session.FieldsAction)
	if !ok || len(fa.Selectors) != 1 {
		return false
	}
	return fa.Selectors[0] == wantSel
}
