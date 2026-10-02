package browser

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// A deliberately unavailable remote CDP endpoint: allocation attempts can be
// observed without launching Chrome or touching any real browser/profile.
func testPool(t *testing.T, size int) (*pool, *atomic.Int32) {
	t.Helper()
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		http.Error(w, "synthetic CDP unavailable", http.StatusServiceUnavailable)
	}))
	alloc, stop := chromedp.NewRemoteAllocator(context.Background(),
		"ws"+strings.TrimPrefix(server.URL, "http")+"/devtools/browser/fixture", chromedp.NoModifyURL)
	m := &Manager{opts: Options{BackgroundTabs: true}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	p := newPool(m, alloc, size)
	t.Cleanup(func() { p.closeAll(); stop(); server.Close() })
	return p, &attempts
}

func testPage(id int) *page {
	ctx, cancel := context.WithCancel(context.Background())
	return &page{id: id, ctx: ctx, cancel: cancel}
}

func TestFatalTabErrorDistinguishesWorkerAndRequestCancellation(t *testing.T) {
	alive := context.Background()
	dead, cancel := context.WithCancel(alive)
	cancel()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{"healthy", alive, nil, false},
		{"caller cancelled", alive, context.Canceled, false},
		{"wrapped caller cancellation", alive, fmt.Errorf("navigation: %w", context.Canceled), false},
		{"operation deadline", alive, context.DeadlineExceeded, false},
		{"worker cancelled", dead, context.Canceled, true},
		{"worker cancelled with no operation error", dead, nil, true},
		{"target closed", alive, errors.New("target closed"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isFatalTabErr(tc.ctx, tc.err); got != tc.want {
				t.Fatalf("isFatalTabErr = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAcquireDiscardsCancelledIdleWorkers(t *testing.T) {
	p, attempts := testPool(t, 2)
	dead, live := testPage(1), testPage(2)
	dead.cancel()
	p.pages = []*page{dead, live}
	p.created = 2
	p.free <- dead
	p.free <- live
	got, err := p.acquire(context.Background())
	if err != nil || got != live {
		t.Fatalf("acquire returned %v, %v; want live worker", got, err)
	}
	if stats := p.stat(); stats != (PoolStat{Size: 2, Created: 1, Busy: 1, Retired: 1}) {
		t.Fatalf("unexpected stats: %+v", stats)
	}
	if attempts.Load() != 0 {
		t.Fatal("should reuse the remaining live worker, not allocate")
	}
	p.release(got, false)
}

func TestAllDeadIdleWorkersFreeCapacityForReplacement(t *testing.T) {
	p, attempts := testPool(t, 1)
	dead := testPage(1)
	dead.cancel()
	p.created = 1
	p.pages = []*page{dead}
	p.free <- dead
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got, err := p.acquire(ctx)
	if got != nil || err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want replacement allocation error, not a dead worker or deadline: %v, %v", got, err)
	}
	if attempts.Load() != 1 {
		t.Fatalf("replacement attempts = %d, want 1", attempts.Load())
	}
	if stats := p.stat(); stats.Created != 0 || stats.Retired != 1 || stats.Busy != 0 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

func TestReleaseRetiresCancelledAndUnparkableWorkers(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancelled=%v", cancelled), func(t *testing.T) {
			p, _ := testPool(t, 1)
			pg := testPage(1)
			if cancelled {
				pg.cancel()
			}
			// The uncancelled fixture has no CDP context: parking must fail.
			p.created, p.busy = 1, 1
			p.pages = []*page{pg}
			p.release(pg, true)
			if stats := p.stat(); stats != (PoolStat{Size: 1, Retired: 1}) {
				t.Fatalf("broken worker was recirculated: %+v", stats)
			}
		})
	}
}

// Synchronize on a full-pool wait without timing-dependent sleeps.
type observedWait struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *observedWait) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func TestRetirementAndFailedAllocationWakeWaiters(t *testing.T) {
	p, attempts := testPool(t, 1)
	pg := testPage(1)
	p.created, p.busy = 1, 1
	p.pages = []*page{pg}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	results := make(chan error, 2)
	for range 2 {
		wait := &observedWait{Context: ctx, waiting: make(chan struct{})}
		go func() { _, err := p.acquire(wait); results <- err }()
		select {
		case <-wait.waiting:
		case <-ctx.Done():
			t.Fatal("acquirer did not start waiting")
		}
	}
	p.release(pg, false)
	for range 2 {
		if err := <-results; err == nil || errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("waiter did not attempt replacement: %v", err)
		}
	}
	if attempts.Load() != 2 {
		t.Fatalf("allocation attempts = %d, want both waiters to wake", attempts.Load())
	}
	if stats := p.stat(); stats.Created != 0 || stats.Busy != 0 || stats.Retired != 1 {
		t.Fatalf("capacity leaked: %+v", stats)
	}
}

func TestAllocatorCancellationWakesWaiter(t *testing.T) {
	p, attempts := testPool(t, 1)
	lifetime, stop := context.WithCancel(p.alloc)
	defer stop()
	p.alloc = lifetime
	pg := testPage(1)
	p.pages = []*page{pg}
	p.created, p.busy = 1, 1
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	wait := &observedWait{Context: ctx, waiting: make(chan struct{})}
	result := make(chan error, 1)
	go func() { _, err := p.acquire(wait); result <- err }()
	select {
	case <-wait.waiting:
	case <-ctx.Done():
		t.Fatal("acquirer did not start waiting")
	}
	stop()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("generation cancellation did not wake waiter: %v", err)
	}
	if attempts.Load() != 0 {
		t.Fatal("cancelled generation attempted to allocate a worker")
	}
	p.release(pg, false)
}

func TestSessionReleaseUsesOwningPool(t *testing.T) {
	first, _ := testPool(t, 1)
	next, _ := testPool(t, 1)
	pg := testPage(1)
	first.pages = []*page{pg}
	first.created = 1
	first.free <- pg
	m := first.m
	m.opts.ChromeURL, m.wsURL = "ws://fixture", "ws://fixture"
	m.pool = first
	lease, release, err := m.Acquire(context.Background())
	if err != nil || lease.p != pg {
		t.Fatalf("lease failed: %v", err)
	}
	m.pool = next // simulate a generation change while the old tab is leased
	pg.cancel()
	release()
	if stats := first.stat(); stats.Busy != 0 || stats.Created != 0 || stats.Retired != 1 {
		t.Fatalf("old lease was not retired in its own pool: %+v", stats)
	}
	if stats := next.stat(); stats != (PoolStat{Size: 1}) {
		t.Fatalf("old lease corrupted the new pool: %+v", stats)
	}
}

func TestCancelledAcquireDoesNotConsumeIdleWorker(t *testing.T) {
	p, _ := testPool(t, 1)
	pg := testPage(1)
	p.pages = []*page{pg}
	p.created = 1
	p.free <- pg
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := p.acquire(ctx); got != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled caller acquired a worker: %v, %v", got, err)
	}
	if stats := p.stat(); stats.Idle != 1 || stats.Busy != 0 {
		t.Fatalf("cancelled caller consumed capacity: %+v", stats)
	}
}
