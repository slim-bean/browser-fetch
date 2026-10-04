package session

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

var liveChrome = flag.String("live-chrome-url", os.Getenv("BROWSER_FETCH_TEST_CHROME_URL"), "CDP url of a real Chrome for live probe tests")

// TestZZProbeLiveGesture drives a real Chrome if available: navigates to the
// tirerack login URL, dispatches the wake gesture on #emailLogin, polls
// fieldFilled, then dumps the element's live properties via one evaluate.
// Probes the "fields says empty but screenshot says filled" discrepancy live.
func TestZZProbeLiveGesture(t *testing.T) {
	if *liveChrome == "" {
		t.Skip("no live chrome URL (set -live-chrome-url or BROWSER_FETCH_TEST_CHROME_URL)")
	}
	root := context.Background()
	allocCtx, allocCancel := chromedp.NewRemoteAllocator(root, *liveChrome)
	defer allocCancel()
	tctx, tcancel := chromedp.NewContext(allocCtx)
	defer tcancel()
	tab := &liveTab{ctx: tctx}

	if err := runOn(tctx, chromedp.Navigate("https://www.tirerack.com/register/LoginServlet?goWhere=/register/Vehicles.jsp")); err != nil {
		t.Fatal(err)
	}
	sleepCtx(tctx, 5*time.Second)

	// Pre-gesture state.
	logFields(t, tctx, "pre-gesture")

	if err := dispatchFieldGesture(tctx, tab, "#emailLogin"); err != nil {
		t.Log("gesture error:", err)
	}
	for i := 0; i < 30; i++ {
		filled, err := fieldFilled(tctx, tab, "#emailLogin")
		if filled {
			t.Logf("poll %d: filled=true err=%v", i, err)
			break
		}
		if i%5 == 0 {
			t.Logf("poll %d: filled=false err=%v", i, err)
		}
		sleepCtx(tctx, 500*time.Millisecond)
	}
	logFields(t, tctx, "post-gesture")
}

func logFields(t *testing.T, ctx context.Context, label string) {
	var dump string
	script := `(() => {
		const probe = (sel) => { const el = document.querySelector(sel); if (!el) return null;
			return {tag: el.tagName, type: el.type, value: el.value, valueLen: String(el.value||"").length,
				id: el.id, name: el.name, class: el.className,
				hasProp: ('value' in el), protoValue: Object.getOwnPropertyDescriptor(HTMLInputElement.prototype,'value') ? 'ok' : 'no'};
		};
		return JSON.stringify({
			emailLogin: probe('#emailLogin'), passLogin: probe('#passLogin'),
			qs: document.querySelectorAll('#emailLogin').length,
			active: document.activeElement ? (document.activeElement.id||document.activeElement.tagName) : null,
		});
	})()`
	if err := runOn(ctx, chromedp.Evaluate(script, &dump)); err != nil {
		t.Fatalf("%s: %v", label, err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(dump), &m); err != nil {
		t.Fatalf("%s: decode %q: %v", label, dump, err)
	}
	t.Logf("%s: %s", label, dump)
}

type liveTab struct{ ctx context.Context }

func (l *liveTab) Ctx() context.Context { return l.ctx }
func (l *liveTab) Read(ctx context.Context) (Snapshot, error) {
	var u, title, html string
	if err := runOn(ctx, chromedp.Location(&u), chromedp.Title(&title), chromedp.OuterHTML("html", &html, chromedp.ByQuery)); err != nil {
		return Snapshot{}, err
	}
	return Snapshot{URL: u, Title: title, HTML: html}, nil
}

var _ = fmt.Sprintf
