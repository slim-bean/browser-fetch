package screenshot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math/rand/v2"
	"testing"
)

func source(t *testing.T, w, h int, noise bool) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	rng := rand.New(rand.NewPCG(1, 2))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := color.RGBA{uint8(y / 100), uint8(y / 20), uint8(y / 10), 255}
			if noise {
				c = color.RGBA{uint8(rng.Uint32()), uint8(rng.Uint32()), uint8(rng.Uint32()), 255}
			}
			img.SetRGBA(x, y, c)
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestSplitOverlappingCropsAndLimits(t *testing.T) {
	for _, height := range []int{900, 1400, 1401, 2700, MaxHeight} {
		c, err := Split(context.Background(), source(t, Width, height, false), Width+200, height+500, height)
		if err != nil {
			t.Fatal(err)
		}
		if len(c.Tiles) > MaxSegments || c.PageHeight != height+500 {
			t.Fatal("bad capture manifest")
		}
		for i, tile := range c.Tiles {
			if tile.Width > Width || tile.Height > SegmentHeight || len(tile.Data) > MaxImageBytes {
				t.Fatal("unbounded tile")
			}
			if i == 0 && tile.YStart != 0 {
				t.Fatal("first crop missing top")
			}
			if i > 0 && c.Tiles[i-1].YEnd-tile.YStart != Overlap {
				t.Fatal("missing overlap")
			}
			img, err := jpeg.Decode(bytes.NewReader(tile.Data))
			if err != nil {
				t.Fatal(err)
			}
			if img.Bounds().Dx() != tile.Width || img.Bounds().Dy() != tile.Height {
				t.Fatal("bad encoded dimensions")
			}
			sum := sha256.Sum256(tile.Data)
			if tile.SHA256 != hex.EncodeToString(sum[:]) {
				t.Fatal("bad checksum")
			}
			// Top row must be from the source crop, not a globally resized page.
			r, _, _, _ := img.At(100, 0).RGBA()
			if abs(int(r>>8)-tile.YStart/100) > 3 {
				t.Fatalf("wrong crop at %d: red=%d", tile.YStart, r>>8)
			}
		}
		if c.Tiles[len(c.Tiles)-1].YEnd != height {
			t.Fatal("bottom not covered")
		}
	}
}

func TestNoisyImageScalesWithinByteBudget(t *testing.T) {
	c, err := Split(context.Background(), source(t, Width, SegmentHeight, true), Width, SegmentHeight, SegmentHeight)
	if err != nil {
		t.Fatal(err)
	}
	tile := c.Tiles[0]
	if tile.Width >= Width || len(tile.Data) > MaxImageBytes {
		t.Fatal("expected bounded scaling of high entropy image")
	}
	if tile.YEnd != SegmentHeight {
		t.Fatal("scaling changed source coordinates")
	}
}

func TestSplitRejectsSourceDimensionsAndCancellation(t *testing.T) {
	for _, tc := range []struct{ w, h, claimed int }{{Width + 1, 1, 1}, {Width, 1, MaxHeight + 1}, {Width, 1, 2}} {
		if _, err := Split(context.Background(), source(t, tc.w, tc.h, false), Width, tc.claimed, tc.claimed); err == nil {
			t.Fatal("accepted unexpected dimensions")
		}
	}
	if _, err := Split(context.Background(), make([]byte, MaxSourceBytes+1), Width, 1, 1); err == nil {
		t.Fatal("accepted oversized source")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Split(ctx, source(t, Width, 1, false), Width, 1, 1); err != context.Canceled {
		t.Fatal("ignored cancellation", err)
	}
}
func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
