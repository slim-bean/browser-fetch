package browser

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// Run only against an isolated test Chrome. pi-assistant/test/live.ts supplies
// its temporary-profile endpoint; ordinary go test never connects to a browser.
func TestLivePoolRecovery(t *testing.T) {
	endpoint := os.Getenv("BROWSER_FETCH_TEST_CDP_URL")
	if endpoint == "" {
		t.Skip("requires an isolated Chrome via BROWSER_FETCH_TEST_CDP_URL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	started := make(chan struct{}, 2)
	var slowHits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			slowHits.Add(1)
			started <- struct{}{}
			select {
			case <-r.Context().Done():
			case <-ctx.Done():
			}
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html><head><title>Pool recovery fixture</title></head><body>synthetic content</body></html>")
	}))
	m := New(ctx, Options{
		ChromeURL: endpoint, MaxTabs: 1, NavTimeout: 10 * time.Second, BackgroundTabs: true,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	defer func() { m.Close(); cancel(); server.Close() }()
	fetch := func() *Result {
		t.Helper()
		result, err := m.Fetch(ctx, Request{URL: server.URL})
		if err != nil || result == nil || result.Title != "Pool recovery fixture" {
			t.Fatalf("fetch failed: result=%+v err=%v", result, err)
		}
		return result
	}
	first := fetch()
	pool, err := m.currentPool(ctx)
	if err != nil {
		t.Fatal(err)
	}
	worker := func() *page {
		t.Helper()
		pool.mu.Lock()
		defer pool.mu.Unlock()
		if len(pool.pages) != 1 {
			t.Fatalf("want one worker, got %d", len(pool.pages))
		}
		return pool.pages[0]
	}
	startSlow := func(requestCtx context.Context) <-chan error {
		t.Helper()
		result := make(chan error, 1)
		go func() { _, err := m.Fetch(requestCtx, Request{URL: server.URL + "/slow"}); result <- err }()
		select {
		case <-started:
		case err := <-result:
			t.Fatalf("slow navigation failed before reaching fixture: %v", err)
		case <-ctx.Done():
			t.Fatal("slow navigation never started")
		}
		return result
	}
	awaitError := func(result <-chan error) error {
		t.Helper()
		select {
		case err := <-result:
			if err == nil {
				t.Fatal("interrupted navigation unexpectedly succeeded")
			}
			return err
		case <-ctx.Done():
			t.Fatal("interrupted navigation did not finish")
			return ctx.Err()
		}
	}

	// Cancelling one HTTP request must not poison a healthy long-lived worker.
	requestCtx, cancelRequest := context.WithCancel(ctx)
	defer cancelRequest()
	result := startSlow(requestCtx)
	cancelRequest()
	if err := awaitError(result); !errors.Is(err, context.Canceled) {
		t.Fatalf("want caller cancellation, got %v", err)
	}
	if got := fetch(); got.PageID != first.PageID || pool.stat().Retired != 0 {
		t.Fatal("caller cancellation unnecessarily retired a healthy worker")
	}

	// Simulate an idle CDP connection loss without changing Chrome's generation.
	worker().cancel()
	second := fetch()
	if second.PageID == first.PageID || pool.stat().Retired != 1 {
		t.Fatal("cancelled idle worker was not replaced")
	}

	// A disconnect during navigation fails this request; it must NOT replay it.
	result = startSlow(ctx)
	worker().cancel()
	awaitError(result)
	if ctx.Err() != nil || slowHits.Load() != 2 {
		t.Fatalf("worker failure cancelled caller or replayed navigation: ctx=%v hits=%d", ctx.Err(), slowHits.Load())
	}
	third := fetch() // new, explicit request is allowed to create a replacement
	if third.PageID == second.PageID || pool.stat().Retired != 2 {
		t.Fatal("failed in-flight worker was returned to the pool")
	}
	current, err := m.currentPool(ctx)
	if err != nil || current != pool {
		t.Fatalf("worker recovery should not require a browser restart: %v", err)
	}
}
