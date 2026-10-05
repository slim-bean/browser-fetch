package screenshot

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestStoreFrozenOwnershipExpiryAndCapacity(t *testing.T) {
	c, err := Split(context.Background(), source(t, Width, 2700, false), Width+100, 15000, 2700)
	if err != nil {
		t.Fatal(err)
	}
	s := NewStore()
	now := time.Now()
	s.now = func() time.Time { return now }
	first, err := s.Add("reader", "https://example.com/", "Fixture", c)
	if err != nil {
		t.Fatal(err)
	}
	if first.Segment != 1 || first.Segments != 2 || !first.Truncated || !first.HorizontalTruncated {
		t.Fatal("bad manifest")
	}
	second, err := s.Get("reader", first.CaptureID, 2)
	if err != nil || second.YStart != 1300 {
		t.Fatal("bad continuation", err)
	}
	again, _ := s.Get("reader", first.CaptureID, 1)
	if again.Image != first.Image {
		t.Fatal("capture not frozen")
	}
	for _, owner := range []string{"driver", "root", "anonymous"} {
		if _, err := s.Get(owner, first.CaptureID, 1); !errors.Is(err, ErrUnavailable) {
			t.Fatal("ownership leaked")
		}
	}
	if _, err := s.Get("reader", first.CaptureID, 0); !errors.Is(err, ErrSegment) {
		t.Fatal("accepted invalid segment")
	}
	if _, err := s.Get("reader", first.CaptureID, 3); !errors.Is(err, ErrSegment) {
		t.Fatal("accepted segment beyond capture")
	}
	for i := 1; i < MaxCaptures; i++ {
		if _, err := s.Add("reader", "https://example.com/", "", c); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Add("reader", "https://example.com/", "", c); !errors.Is(err, ErrCapacity) {
		t.Fatal("count limit ignored")
	}
	now = now.Add(TTL)
	if _, err := s.Get("reader", first.CaptureID, 1); !errors.Is(err, ErrUnavailable) {
		t.Fatal("expired capture returned")
	}
	if s.bytes != 0 || len(s.entries) != 0 {
		t.Fatal("expired images retained")
	}
	if _, err := s.Add("reader", "https://example.com/", "", c); err != nil {
		t.Fatal("capacity not reclaimed")
	}
	if _, err := NewStore().Get("reader", first.CaptureID, 1); !errors.Is(err, ErrUnavailable) {
		t.Fatal("capture survived restart")
	}
}

func TestStoreByteLimitAndConcurrentReads(t *testing.T) {
	s := NewStore()
	c := &Capture{PageWidth: Width, PageHeight: MaxHeight, CapturedHeight: MaxHeight}
	for i := 0; i < MaxSegments; i++ {
		c.Tiles = append(c.Tiles, Tile{Data: make([]byte, MaxImageBytes), Width: Width, Height: 1})
	}
	for i := 0; i < MaxStoreBytes/(MaxSegments*MaxImageBytes); i++ {
		if _, err := s.Add("reader", "https://example.com/", "", c); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Add("reader", "https://example.com/", "", c); !errors.Is(err, ErrCapacity) {
		t.Fatal("byte capacity ignored")
	}
	s = NewStore()
	first, err := s.Add("reader", "https://example.com/", "", c)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Go(func() {
			for j := 0; j < 8; j++ {
				if _, err := s.Get("reader", first.CaptureID, 1); err != nil {
					t.Error(err)
				}
				s.Reap()
			}
		})
	}
	wg.Wait()
}
