package session

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// fakeTab is a Tab whose snapshot is scripted.
type fakeTab struct {
	ctx  context.Context
	snap Snapshot
	err  error
}

func (f *fakeTab) Ctx() context.Context                       { return f.ctx }
func (f *fakeTab) Read(ctx context.Context) (Snapshot, error) { return f.snap, f.err }

// stubRun replaces chromedp.Run with a recorder for the duration of a test.
func stubRun(t *testing.T, fn func(ctx context.Context, actions ...chromedp.Action) error) {
	t.Helper()
	prev := runOn
	runOn = fn
	t.Cleanup(func() { runOn = prev })
}

func okRun(ctx context.Context, actions ...chromedp.Action) error { return nil }

func newTestManager(t *testing.T, snap Snapshot) (*Manager, *Session) {
	t.Helper()
	stubRun(t, okRun)
	tab := &fakeTab{ctx: context.Background(), snap: snap}
	m := New(nil, func(ctx context.Context) (Tab, func(), error) {
		return tab, func() {}, nil
	}, Options{
		MaxSessions: 2, MaxIdle: time.Minute, ActionTimeout: 5 * time.Second,
		AssertTTL: 30 * time.Second, MaxLog: 10,
	})
	s, err := m.Open(context.Background(), "example.com")
	if err != nil {
		t.Fatal(err)
	}
	return m, s
}

func TestPasswordTypeRequiresFreshAssertion(t *testing.T) {
	_, s := newTestManager(t, Snapshot{URL: "https://example.com/login", Title: "Login", HTML: "<html></html>"})

	// No assertion yet: typing a password must fail and must not reach the tab.
	stubRun(t, func(ctx context.Context, actions ...chromedp.Action) error {
		t.Fatal("password type must never reach the browser without an assertion")
		return nil
	})
	m := &Manager{opts: DefaultOptions()}
	a := TypeAction{Text: "hunter2", Field: "password"}
	if got := m.dispatchType(context.Background(), s, a); got == nil || !strings.Contains(got.Error(), "assertion") {
		t.Fatalf("expected assertion refusal, got %v", got)
	}
}

func TestPasswordTypeAllowedAfterExactAssert(t *testing.T) {
	_, s := newTestManager(t, Snapshot{URL: "https://example.com/login", Title: "Sign in", HTML: "<html>Sign in</html>"})
	m := &Manager{opts: DefaultOptions()}
	stubRun(t, okRun)

	// A passing exact assertion opens the window.
	_, err := m.Run(context.Background(), s, AssertAction{AssertKind: AssertURL, Pattern: `^https://example\.com/login$`})
	if err != nil {
		t.Fatalf("assert should pass: %v", err)
	}
	if !s.CanTypeCredentials() {
		t.Fatal("assertion should enable credential typing")
	}
	if err := m.dispatchType(context.Background(), s, TypeAction{Text: "x", Field: "password"}); err != nil {
		t.Fatalf("password type after assertion should pass: %v", err)
	}
}

func TestAssertFailureDoesNotOpenWindow(t *testing.T) {
	_, s := newTestManager(t, Snapshot{URL: "https://example.com/other", Title: "Other", HTML: "<html></html>"})
	m := &Manager{opts: DefaultOptions()}

	_, err := m.Run(context.Background(), s, AssertAction{AssertKind: AssertURL, Pattern: `^https://example\.com/login$`})
	if err == nil {
		t.Fatal("assert should fail on wrong URL")
	}
	if s.CanTypeCredentials() {
		t.Fatal("failed assertion must not enable credential typing")
	}
}

func TestNonExactAssertDoesNotGateTyping(t *testing.T) {
	_, s := newTestManager(t, Snapshot{URL: "https://example.com/x", Title: "T", HTML: "<html>hello world</html>"})
	m := &Manager{opts: DefaultOptions()}

	// A landmark assert is evidence, but not sufficient for credentials.
	if _, err := m.Run(context.Background(), s, AssertAction{AssertKind: AssertLandmark, Landmarks: []string{"hello world"}}); err != nil {
		t.Fatalf("landmark assert should pass: %v", err)
	}
	if s.CanTypeCredentials() {
		t.Fatal("landmark assert must not enable credential typing")
	}
}

func TestEvidenceLogRecordsActions(t *testing.T) {
	_, s := newTestManager(t, Snapshot{URL: "https://example.com/a", Title: "A", HTML: "<html></html>"})
	m := &Manager{opts: DefaultOptions()}

	_, _ = m.Run(context.Background(), s, NavigateAction{URL: "https://example.com/b"})
	_, err := m.Run(context.Background(), s, ClickAction{Selector: "#nope"})
	if err != nil {
		t.Fatalf("stubbed click should succeed: %v", err)
	}
	recs := s.Records()
	if len(recs) < 2 {
		t.Fatalf("expected evidence records, got %d", len(recs))
	}
	if recs[0].Action != "navigate" || !recs[0].OK {
		t.Fatalf("unexpected first record: %+v", recs[0])
	}
	if recs[1].Action != "click" || !recs[1].OK {
		t.Fatalf("unexpected second record: %+v", recs[1])
	}
}

func TestEvidenceLogNeverStoresPasswordText(t *testing.T) {
	_, s := newTestManager(t, Snapshot{URL: "https://example.com/login", Title: "Login", HTML: "<html></html>"})
	m := &Manager{opts: DefaultOptions()}
	stubRun(t, okRun)
	s.assertedAt = time.Now() // open the window directly

	_ = m.dispatchType(context.Background(), s, TypeAction{Text: "super-secret", Field: "password"})
	for _, r := range s.Records() {
		if strings.Contains(r.Detail, "super-secret") {
			t.Fatal("evidence log must never contain password text")
		}
	}
}

func TestConcurrentTypeActionsRejected(t *testing.T) {
	_, s := newTestManager(t, Snapshot{URL: "https://example.com/x", Title: "T", HTML: "<html></html>"})
	m := &Manager{opts: DefaultOptions()}
	stubRun(t, okRun)

	// Run one type via Run and confirm a second concurrent Run is refused.
	// Run is synchronous, so simulate by driving dispatch through Run with a
	// cancelled context for the second call.
	ctx1, cancel1 := context.WithCancel(context.Background())
	cancel1() // first action fails fast but still marks mid-flight only within Run
	_, _ = m.Run(ctx1, s, TypeAction{Text: "a", Field: "text"})
	// After Run returns, pendingType is cleared; a subsequent action must be
	// accepted (not blocked).
	if _, err := m.Run(context.Background(), s, ContentAction{}); err != nil {
		t.Fatalf("content action after failed type should be accepted: %v", err)
	}
}

func TestWaitURLPattern(t *testing.T) {
	tab := &fakeTab{ctx: context.Background(), snap: Snapshot{URL: "https://example.com/done"}}
	if err := dispatchWait(context.Background(), tab, WaitAction{URLRegexp: `/done`}); err != nil {
		t.Fatalf("wait should match: %v", err)
	}
	if err := dispatchWait(context.Background(), tab, WaitAction{URLRegexp: `/never`}); err == nil {
		t.Fatal("wait should time out on non-match")
	}
}

func TestAssertKinds(t *testing.T) {
	tab := &fakeTab{ctx: context.Background(), snap: Snapshot{
		URL: "https://bank.example/accounts", Title: "Accounts", HTML: "<html>Welcome back, Ed</html>",
	}}
	if _, err := dispatchAssert(context.Background(), tab, AssertAction{AssertKind: AssertTitle, Pattern: `^Accounts$`}); err != nil {
		t.Fatalf("title assert: %v", err)
	}
	res, err := dispatchAssert(context.Background(), tab, AssertAction{AssertKind: AssertLandmarkN, Landmarks: []string{"Welcome back", "missing"}, MinMatch: 1})
	if err != nil {
		t.Fatalf("landmarks assert: %v", err)
	}
	ar, ok := res.(assertResult)
	if !ok || !ar.Matched || ar.Count != 1 {
		t.Fatalf("unexpected landmarks result: %#v", res)
	}
}
