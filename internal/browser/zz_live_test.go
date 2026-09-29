package browser

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// TestZZLiveWorkerContext connects to a real Chrome DevTools endpoint (default
// http://127.0.0.1:9222, override with BROWSER_FETCH_TEST_CHROME_URL), creates
// a worker page exactly like pool.newPage does (BackgroundTabs=true path), and
// then runs chromedp.Run on the page context. It fails if Run returns
// ErrInvalidContext. Skips when no Chrome is reachable.
func TestZZLiveWorkerContext(t *testing.T) {
	url := envOr("BROWSER_FETCH_TEST_CHROME_URL", "http://127.0.0.1:9222")
	root := context.Background()
	m := New(root, Options{
		ChromeURL:      url,
		MaxTabs:        1,
		BlockMedia:     false,
		BackgroundTabs: true,
	})
	defer m.Close()

	allocCtx, cancel := context.WithTimeout(root, 5*time.Second)
	defer cancel()
	if err := m.Probe(allocCtx); err != nil {
		t.Skipf("no Chrome at %s: %v", url, err)
	}

	pool, err := m.currentPool(root)
	if err != nil {
		t.Fatalf("currentPool: %v", err)
	}
	pg, err := pool.newPage()
	if err != nil {
		t.Fatalf("newPage: %v", err)
	}
	defer pg.cancel()

	ctx, cancel := context.WithTimeout(pg.ctx, 15*time.Second)
	defer cancel()
	if err := chromedp.Run(ctx, chromedp.Navigate("https://example.com")); err != nil {
		if errors.Is(err, chromedp.ErrInvalidContext) {
			t.Fatalf("Run on fresh worker page context: invalid context")
		}
		t.Fatalf("Run on fresh worker page context: %v", err)
	}
	var title string
	if err := chromedp.Run(ctx, chromedp.Title(&title)); err != nil {
		t.Fatalf("Title: %v", err)
	}
	t.Logf("navigate+title ok, title=%q", title)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
