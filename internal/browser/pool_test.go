package browser

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

func TestPoolAcquireSkipsDeadTabs(t *testing.T) {
	m := &Manager{log: slog.Default()}
	p := newPool(m, context.Background(), 3)

	// Two live tabs and one dead one, all parked in the free channel.
	live1 := &page{id: 1, ctx: context.Background()}
	live2 := &page{id: 2, ctx: context.Background()}
	deadCtx, cancel := context.WithCancel(context.Background())
	cancel() // a human closed this tab out from under us
	dead := &page{id: 3, ctx: deadCtx}

	p.mu.Lock()
	p.created = 3
	p.pages = []*page{live1, live2, dead}
	p.mu.Unlock()
	p.free <- dead
	p.free <- live1
	p.free <- live2

	// Every acquire must return a LIVE tab, skipping the corpse.
	seen := map[int]bool{}
	for i := 0; i < 2; i++ {
		pg, err := p.acquire(context.Background())
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		if pg.ctx.Err() != nil {
			t.Fatalf("acquire %d returned a dead tab", i)
		}
		seen[pg.id] = true
		p.release(pg, true)
	}
	if seen[3] {
		t.Fatal("dead tab was leased")
	}
	if p.retired != 1 {
		t.Fatalf("dead tab must be retired exactly once, retired=%d", p.retired)
	}
}

func TestPoolAcquireCreatesReplacementForRetiredTab(t *testing.T) {
	m := &Manager{log: slog.Default()}
	p := newPool(m, context.Background(), 2)

	deadCtx, cancel := context.WithCancel(context.Background())
	cancel()
	dead := &page{id: 1, ctx: deadCtx}
	p.mu.Lock()
	p.created = 2 // at capacity
	p.pages = []*page{dead}
	p.mu.Unlock()
	p.free <- dead

	// Pool at capacity, the only idle tab is dead: acquire must retire it,
	// free the capacity slot, and wait for (or create) a replacement rather
	// than leasing the corpse. No live tab is coming in this test, so the
	// acquire blocks until the caller's deadline — assert the retirement
	// happened and no dead tab was handed out.
	done := make(chan error, 1)
	go func() {
		pg, err := p.acquire(withDeadline(context.Background(), 300*time.Millisecond))
		if pg != nil {
			p.release(pg, true)
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected deadline error with no live tabs available")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("acquire did not return after caller deadline")
	}
	if p.retired != 1 {
		t.Fatalf("dead tab must be retired, retired=%d", p.retired)
	}
	if p.created != 1 {
		t.Fatalf("retirement must free a creation slot, created=%d", p.created)
	}
}

func withDeadline(ctx context.Context, d time.Duration) context.Context {
	c, cancel := context.WithTimeout(ctx, d)
	go func() {
		<-c.Done()
		cancel()
	}()
	return c
}
