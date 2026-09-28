package browser

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

func TestPoolFollowsBrowserGeneration(t *testing.T) {
	var generation atomic.Int32
	generation.Store(1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if generation.Load() == 1 {
			_, _ = w.Write([]byte(`{"webSocketDebuggerUrl":"ws://127.0.0.1:1/devtools/browser/first"}`))
		} else {
			_, _ = w.Write([]byte(`{"webSocketDebuggerUrl":"ws://127.0.0.1:1/devtools/browser/second"}`))
		}
	}))
	defer server.Close()
	ctx := context.Background()
	mgr := New(ctx, Options{ChromeURL: server.URL, MaxTabs: 2})
	defer mgr.Close()
	first, err := mgr.currentPool(ctx)
	if err != nil {
		t.Fatal(err)
	}
	same, err := mgr.currentPool(ctx)
	if err != nil || first != same {
		t.Fatalf("same generation should reuse pool: %v", err)
	}
	generation.Store(2)
	next, err := mgr.currentPool(ctx)
	if err != nil || next == first {
		t.Fatalf("new browser must get a fresh pool: %v", err)
	}
	if first.alloc.Err() == nil {
		t.Fatal("old allocator not cancelled")
	}
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pool, err := mgr.currentPool(ctx)
			if err != nil || pool != next {
				t.Errorf("concurrent generation lookup: %v", err)
			}
			_ = mgr.Health()
		}()
	}
	wg.Wait()
}
