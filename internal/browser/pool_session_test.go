package browser

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Preserve the session/VNC pool-health regressions from upstream, using an
// explicit synthetic remote allocator so these tests cannot launch real Chrome.
func TestPoolAcquireSkipsDeadTabs(t *testing.T) {
	p, attempts := testPool(t, 3)
	live1, live2, dead := testPage(1), testPage(2), testPage(3)
	dead.cancel() // a human closed this tab out from under the session
	p.created = 3
	p.pages = []*page{live1, live2, dead}
	p.free <- dead
	p.free <- live1
	p.free <- live2

	var leased []*page
	seen := map[int]bool{}
	for range 2 {
		pg, err := p.acquire(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if pg.ctx.Err() != nil {
			t.Fatal("acquire returned a dead tab")
		}
		seen[pg.id] = true
		leased = append(leased, pg)
	}
	if seen[3] || !seen[1] || !seen[2] || p.stat().Retired != 1 {
		t.Fatalf("dead worker not skipped exactly once: seen=%v stats=%+v", seen, p.stat())
	}
	if attempts.Load() != 0 {
		t.Fatal("should reuse live workers without allocation")
	}
	// Fake contexts cannot park; retire after inspecting the acquisition state.
	for _, pg := range leased {
		p.release(pg, false)
	}
}

func TestPoolAcquireCreatesReplacementForRetiredTab(t *testing.T) {
	p, attempts := testPool(t, 2)
	dead, held := testPage(1), testPage(2)
	dead.cancel()
	p.created, p.busy = 2, 1 // at capacity: one leased tab and one dead idle tab
	p.pages = []*page{dead, held}
	p.free <- dead

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	pg, err := p.acquire(ctx)
	if pg != nil || err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want a replacement allocation attempt, not dead tab/deadline: %v, %v", pg, err)
	}
	if attempts.Load() != 1 {
		t.Fatalf("replacement attempts=%d, want 1", attempts.Load())
	}
	if stats := p.stat(); stats.Retired != 1 || stats.Created != 1 || stats.Busy != 1 {
		t.Fatalf("retirement must free a creation slot, retaining the lease: %+v", stats)
	}
	p.release(held, false)
}

func TestRetireWithoutCancelFunc(t *testing.T) {
	p, _ := testPool(t, 1)
	pg := testPage(1)
	pg.cancel()
	pg.cancel = nil // tolerate synthetic/already-detached tabs, as upstream did
	p.created = 1
	p.retire(pg)
	if stats := p.stat(); stats.Created != 0 || stats.Retired != 1 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}
