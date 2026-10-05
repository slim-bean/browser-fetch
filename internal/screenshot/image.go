// Package screenshot owns bounded visual snapshots, independent of agent presentation.
package screenshot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
)

const (
	Version        = 1
	Width          = 1280
	ViewportHeight = 900
	MaxHeight      = 12000
	SegmentHeight  = 1400
	Overlap        = 100
	MaxSegments    = 10
	MaxImageBytes  = 384 << 10
	MaxSourceBytes = 24 << 20
)

// Tile retains encoded bytes only, not the potentially large source bitmap.
type Tile struct {
	Data          []byte
	Width, Height int
	YStart, YEnd  int
	SHA256        string
}

type Capture struct {
	PageWidth, PageHeight int
	CapturedHeight        int
	Tiles                 []Tile
}

// Split checks dimensions before decoding, crops before scaling, and bounds every output.
func Split(ctx context.Context, source []byte, pageWidth, pageHeight, capturedHeight int) (*Capture, error) {
	if len(source) > MaxSourceBytes {
		return nil, errors.New("screenshot source exceeds byte limit")
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(source))
	if err != nil {
		return nil, err
	}
	if cfg.Width != Width || cfg.Height != capturedHeight || capturedHeight < 1 || capturedHeight > MaxHeight || pageWidth < 1 || pageHeight < capturedHeight {
		return nil, errors.New("unexpected screenshot dimensions")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	img, err := png.Decode(bytes.NewReader(source))
	if err != nil {
		return nil, err
	}
	out := &Capture{PageWidth: pageWidth, PageHeight: pageHeight, CapturedHeight: capturedHeight}
	for y := 0; y < capturedHeight; y += SegmentHeight - Overlap {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := min(y+SegmentHeight, capturedHeight)
		crop := image.NewRGBA(image.Rect(0, 0, Width, end-y))
		draw.Draw(crop, crop.Bounds(), img, image.Pt(0, y), draw.Src)
		tile, err := encode(ctx, crop)
		if err != nil {
			return nil, err
		}
		tile.YStart, tile.YEnd = y, end
		out.Tiles = append(out.Tiles, tile)
		if len(out.Tiles) > MaxSegments {
			return nil, errors.New("screenshot exceeds segment limit")
		}
		if end == capturedHeight {
			break
		}
	}
	return out, nil
}

func encode(ctx context.Context, img *image.RGBA) (Tile, error) {
	for {
		for _, quality := range []int{85, 70, 55} {
			if err := ctx.Err(); err != nil {
				return Tile{}, err
			}
			var b bytes.Buffer
			if err := jpeg.Encode(&b, img, &jpeg.Options{Quality: quality}); err != nil {
				return Tile{}, err
			}
			if b.Len() <= MaxImageBytes {
				data := b.Bytes()
				sum := sha256.Sum256(data)
				return Tile{Data: data, Width: img.Bounds().Dx(), Height: img.Bounds().Dy(), SHA256: hex.EncodeToString(sum[:])}, nil
			}
		}
		w, h := img.Bounds().Dx()*3/4, img.Bounds().Dy()*3/4
		if w < 256 || h < 1 {
			return Tile{}, errors.New("cannot fit screenshot segment within byte limit")
		}
		// Area averaging preserves fine lines better than nearest-neighbour reduction.
		scaled := image.NewRGBA(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			if err := ctx.Err(); err != nil {
				return Tile{}, err
			}
			for x := 0; x < w; x++ {
				x0, x1 := x*img.Bounds().Dx()/w, (x+1)*img.Bounds().Dx()/w
				y0, y1 := y*img.Bounds().Dy()/h, (y+1)*img.Bounds().Dy()/h
				var r, g, b, a, n uint32
				for sy := y0; sy < y1; sy++ {
					for sx := x0; sx < x1; sx++ {
						i := img.PixOffset(sx, sy)
						r += uint32(img.Pix[i])
						g += uint32(img.Pix[i+1])
						b += uint32(img.Pix[i+2])
						a += uint32(img.Pix[i+3])
						n++
					}
				}
				i := scaled.PixOffset(x, y)
				scaled.Pix[i], scaled.Pix[i+1], scaled.Pix[i+2], scaled.Pix[i+3] = byte(r/n), byte(g/n), byte(b/n), byte(a/n)
			}
		}
		img = scaled
	}
}
