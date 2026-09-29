package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// tabCtxMarker keys a value injected into a fake tab's context. The regression
// tests assert that Manager.Run executes browser actions on a context that
// still carries the tab's values (i.e. is derived from tab.Ctx(), not from the
// caller's HTTP request context).
type tabCtxMarker struct{}

// fakeCtxTab is a minimal Tab whose context carries tabCtxMarker.
type fakeCtxTab struct {
	Tab // embeds nil implementations; only Ctx is used by actionContext
	ctx context.Context
}

func (f fakeCtxTab) Ctx() context.Context { return f.ctx }

// Regression: the deployed gateway ran every Run-dispatched action on the HTTP
// request context, which carries no chromedp values, so chromedp.Run failed
// instantly with ErrInvalidContext ("invalid context"). Dispatchers must
// receive a context derived from tab.Ctx().
func TestRunActionContextCarriesTabValues(t *testing.T) {
	tabCtx, cancelTab := context.WithCancel(context.WithValue(context.Background(), tabCtxMarker{}, "present"))
	defer cancelTab()

	m := &Manager{opts: Options{ActionTimeout: 5 * time.Second}}
	s := &Session{tab: fakeCtxTab{ctx: tabCtx}}

	var got any
	runOn = func(ctx context.Context, action ...chromedp.Action) error {
		got = ctx.Value(tabCtxMarker{})
		return nil
	}
	t.Cleanup(func() { runOn = chromedp.Run })

	if _, err := m.Run(context.Background(), s, NavigateAction{URL: "https://example.com"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got != "present" {
		t.Fatal("action ran on a context that lost the tab's chromedp values (request-context leak)")
	}
}

// Regression companion: the action context must also die when the caller's
// (request) context is cancelled, so short request deadlines still cut
// actions off.
func TestRunActionContextHonorsCallerCancel(t *testing.T) {
	tabCtx, cancelTab := context.WithCancel(context.Background())
	defer cancelTab()

	m := &Manager{opts: Options{ActionTimeout: 10 * time.Second}}
	s := &Session{tab: fakeCtxTab{ctx: tabCtx}}

	runOn = func(ctx context.Context, action ...chromedp.Action) error {
		<-ctx.Done() // simulate a CDP call that only ends when its ctx dies
		return ctx.Err()
	}
	t.Cleanup(func() { runOn = chromedp.Run })

	reqCtx, cancelReq := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := m.Run(reqCtx, s, NavigateAction{URL: "https://example.com"})
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancelReq()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run returned nil despite caller cancellation")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("action did not abort after caller cancellation")
	}
}

// dispatchWait's selector branch runs on the tab context with the shorter of
// (wait budget, action deadline).
func TestDispatchWaitSelectorRunsOnTabContext(t *testing.T) {
	tabCtx, cancelTab := context.WithCancel(context.WithValue(context.Background(), tabCtxMarker{}, "present"))
	defer cancelTab()

	var got any
	runOn = func(ctx context.Context, action ...chromedp.Action) error {
		got = ctx.Value(tabCtxMarker{})
		return nil
	}
	t.Cleanup(func() { runOn = chromedp.Run })

	if err := dispatchWait(context.Background(), fakeCtxTab{ctx: tabCtx}, WaitAction{Selector: "h1"}); err != nil {
		t.Fatalf("dispatchWait: %v", err)
	}
	if got != "present" {
		t.Fatal("dispatchWait selector branch ran on a context that lost the tab's chromedp values")
	}
}

// Guard the guard: with the OLD behavior (request-derived ctx) the first test
// would pass vacuously if runOn never saw any context. Ensure the stubbed
// runOn is actually invoked.
func TestRunInvokesRunOn(t *testing.T) {
	tabCtx, cancelTab := context.WithCancel(context.Background())
	defer cancelTab()

	m := &Manager{opts: Options{ActionTimeout: 5 * time.Second}}
	s := &Session{tab: fakeCtxTab{ctx: tabCtx}}

	called := false
	runOn = func(ctx context.Context, action ...chromedp.Action) error {
		called = true
		return errors.New("stop")
	}
	t.Cleanup(func() { runOn = chromedp.Run })

	_, err := m.Run(context.Background(), s, NavigateAction{URL: "https://example.com"})
	if err == nil || !called {
		t.Fatalf("runOn not invoked or error swallowed: called=%v err=%v", called, err)
	}
}

// A caller context with no deadline must not collapse the selector wait's
// budget to an instantly expired context.
func TestDispatchWaitNoDeadlineKeepsBudget(t *testing.T) {
	if waitBudget(WaitAction{Selector: "h1"}, 0) != 15*time.Second {
		t.Fatal("zero deadline must fall back to the wait's own timeout")
	}
	if waitBudget(WaitAction{Selector: "h1", TimeoutMS: 3000}, 10*time.Second) != 3*time.Second {
		t.Fatal("explicit wait timeout should win when under the deadline")
	}
	if waitBudget(WaitAction{Selector: "h1"}, 2*time.Second) != 2*time.Second {
		t.Fatal("deadline should cap the wait budget")
	}
}
