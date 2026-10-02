package browser

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// TestZZLiveWorkerContext requires an isolated Chrome endpoint explicitly set
// in BROWSER_FETCH_TEST_CHROME_URL. It creates a BackgroundTabs worker and runs
// chromedp.Run on its context. Ordinary tests never probe a personal browser.
func TestZZLiveWorkerContext(t *testing.T) {
	url := os.Getenv("BROWSER_FETCH_TEST_CHROME_URL")
	if url == "" {
		t.Skip("requires an isolated Chrome via BROWSER_FETCH_TEST_CHROME_URL")
	}
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><head><title>Worker context fixture</title></head><body>synthetic content</body></html>"))
	}))
	defer fixture.Close()
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
		t.Fatalf("configured test Chrome unavailable at %s: %v", url, err)
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
	if err := chromedp.Run(ctx, chromedp.Navigate(fixture.URL)); err != nil {
		if errors.Is(err, chromedp.ErrInvalidContext) {
			t.Fatalf("Run on fresh worker page context: invalid context")
		}
		t.Fatalf("Run on fresh worker page context: %v", err)
	}
	var title string
	if err := chromedp.Run(ctx, chromedp.Title(&title)); err != nil {
		t.Fatalf("Title: %v", err)
	}
	if title != "Worker context fixture" {
		t.Fatalf("unexpected title %q", title)
	}
	t.Logf("navigate+title ok, title=%q", title)
}
