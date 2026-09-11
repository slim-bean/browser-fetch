package scheduler

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// tracker records concurrency, globally and per host.
type tracker struct {
	mu      sync.Mutex
	cur     map[string]int
	maxHost map[string]int
	curAll  int
	maxAll  int
	starts  []time.Time
	ends    []time.Time
	callCnt atomic.Int64
}

func newTracker() *tracker {
	return &tracker{cur: map[string]int{}, maxHost: map[string]int{}}
}

func (t *tracker) enter(host string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cur[host]++
	t.curAll++
	if t.cur[host] > t.maxHost[host] {
		t.maxHost[host] = t.cur[host]
	}
	if t.curAll > t.maxAll {
		t.maxAll = t.curAll
	}
	t.starts = append(t.starts, time.Now())
}

func (t *tracker) exit(host string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cur[host]--
	t.curAll--
	t.ends = append(t.ends, time.Now())
}

func TestPerHostConcurrencyIsOne(t *testing.T) {
	s := New[int](Options{MaxSlots: 8})
	tr := newTracker()

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Distinct keys so dedupe does not hide the concurrency.
			_, _, err := s.Do(context.Background(), "example.com", "u"+string(rune('a'+i)),
				func(context.Context) (int, error) {
					tr.enter("example.com")
					time.Sleep(20 * time.Millisecond)
					tr.exit("example.com")
					tr.callCnt.Add(1)
					return i, nil
				})
			if err != nil {
				t.Errorf("Do: %v", err)
			}
		}(i)
	}
	wg.Wait()

	if got := tr.maxHost["example.com"]; got != 1 {
		t.Fatalf("max per-host concurrency = %d, want 1", got)
	}
	if got := tr.callCnt.Load(); got != 6 {
		t.Fatalf("fn called %d times, want 6", got)
	}
}

func TestParallelAcrossHosts(t *testing.T) {
	const hosts = 3
	s := New[int](Options{MaxSlots: hosts})
	tr := newTracker()

	// Each fn waits for all three to be running, so the test deadlocks (and
	// fails via timeout) if hosts were serialised against each other.
	var barrier sync.WaitGroup
	barrier.Add(hosts)

	var wg sync.WaitGroup
	for i := 0; i < hosts; i++ {
		wg.Add(1)
		host := "h" + string(rune('a'+i)) + ".test"
		go func() {
			defer wg.Done()
			_, _, err := s.Do(context.Background(), host, host+"/x", func(context.Context) (int, error) {
				tr.enter(host)
				barrier.Done()
				barrier.Wait()
				tr.exit(host)
				return 1, nil
			})
			if err != nil {
				t.Errorf("Do(%s): %v", host, err)
			}
		}()
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("hosts did not run in parallel")
	}
	if tr.maxAll != hosts {
		t.Fatalf("max global concurrency = %d, want %d", tr.maxAll, hosts)
	}
}

func TestGlobalSlotCap(t *testing.T) {
	s := New[int](Options{MaxSlots: 2})
	tr := newTracker()

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		host := "h" + string(rune('a'+i)) + ".test" // all different hosts
		go func() {
			defer wg.Done()
			_, _, _ = s.Do(context.Background(), host, host, func(context.Context) (int, error) {
				tr.enter(host)
				time.Sleep(30 * time.Millisecond)
				tr.exit(host)
				return 0, nil
			})
		}()
	}
	wg.Wait()

	if tr.maxAll > 2 {
		t.Fatalf("max global concurrency = %d, want <= 2", tr.maxAll)
	}
}

func TestDedupeCoalescesInFlight(t *testing.T) {
	s := New[string](Options{MaxSlots: 4})
	var calls atomic.Int64
	release := make(chan struct{})

	var wg sync.WaitGroup
	results := make([]string, 4)
	sharedFlags := make([]bool, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v, shared, err := s.Do(context.Background(), "example.com", "same-url",
				func(context.Context) (string, error) {
					calls.Add(1)
					<-release // hold so all four overlap
					return "payload", nil
				})
			if err != nil {
				t.Errorf("Do: %v", err)
			}
			results[i], sharedFlags[i] = v, shared
		}(i)
	}

	// Give the goroutines time to pile up on the same key, then let it finish.
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("fn called %d times, want 1 (coalesced)", got)
	}
	sharedCount := 0
	for i := range results {
		if results[i] != "payload" {
			t.Fatalf("result[%d] = %q, want payload", i, results[i])
		}
		if sharedFlags[i] {
			sharedCount++
		}
	}
	if sharedCount != 3 {
		t.Fatalf("shared=true for %d callers, want 3", sharedCount)
	}
	if snap := s.Snapshot(); snap.Shared != 3 {
		t.Fatalf("Snapshot().Shared = %d, want 3", snap.Shared)
	}
}

func TestHostGapEnforced(t *testing.T) {
	const gap = 120 * time.Millisecond
	s := New[int](Options{MaxSlots: 4, HostGap: gap})

	var firstEnd, secondStart time.Time
	_, _, err := s.Do(context.Background(), "example.com", "a", func(context.Context) (int, error) {
		return 0, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	firstEnd = time.Now()

	_, _, err = s.Do(context.Background(), "example.com", "b", func(context.Context) (int, error) {
		secondStart = time.Now()
		return 0, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if elapsed := secondStart.Sub(firstEnd); elapsed < gap {
		t.Fatalf("second navigation started %v after the first, want >= %v", elapsed, gap)
	}
}

func TestGapDoesNotBlockOtherHosts(t *testing.T) {
	s := New[int](Options{MaxSlots: 1, HostGap: 400 * time.Millisecond})

	// Warm up host A so its cooldown is active.
	if _, _, err := s.Do(context.Background(), "a.test", "a1", func(context.Context) (int, error) {
		return 0, nil
	}); err != nil {
		t.Fatal(err)
	}

	// A second request to A must wait; a request to B must not, even though
	// MaxSlots is 1 — the host gate is taken before the global slot.
	go func() {
		_, _, _ = s.Do(context.Background(), "a.test", "a2", func(context.Context) (int, error) {
			return 0, nil
		})
	}()
	time.Sleep(50 * time.Millisecond)

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if _, _, err := s.Do(ctx, "b.test", "b1", func(context.Context) (int, error) {
		return 0, nil
	}); err != nil {
		t.Fatalf("host b was blocked by host a's cooldown: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("host b waited %v, expected to proceed immediately", elapsed)
	}
}

func TestQueueDeadline(t *testing.T) {
	s := New[int](Options{MaxSlots: 1})
	blocked := make(chan struct{})
	started := make(chan struct{})

	go func() {
		_, _, _ = s.Do(context.Background(), "a.test", "long", func(context.Context) (int, error) {
			close(started)
			<-blocked
			return 0, nil
		})
	}()
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	_, _, err := s.Do(ctx, "b.test", "short", func(context.Context) (int, error) {
		t.Error("fn should not run: no slot was available")
		return 0, nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	close(blocked)

	if snap := s.Snapshot(); snap.TimedOut != 1 {
		t.Fatalf("Snapshot().TimedOut = %d, want 1", snap.TimedOut)
	}
}

func TestSnapshotAccounting(t *testing.T) {
	s := New[int](Options{MaxSlots: 2})
	if _, _, err := s.Do(context.Background(), "example.com", "k", func(context.Context) (int, error) {
		return 7, nil
	}); err != nil {
		t.Fatal(err)
	}
	_, _, err := s.Do(context.Background(), "example.com", "bad", func(context.Context) (int, error) {
		return 0, errors.New("boom")
	})
	if err == nil {
		t.Fatal("expected error to propagate")
	}

	snap := s.Snapshot()
	if snap.Total != 2 || snap.Completed != 1 || snap.Failed != 1 {
		t.Fatalf("total/completed/failed = %d/%d/%d, want 2/1/1", snap.Total, snap.Completed, snap.Failed)
	}
	if snap.Running != 0 || snap.Queued != 0 {
		t.Fatalf("running/queued = %d/%d, want 0/0", snap.Running, snap.Queued)
	}
	h, ok := snap.Hosts["example.com"]
	if !ok || h.Total != 2 {
		t.Fatalf("host stats = %+v, want Total 2", h)
	}
}
