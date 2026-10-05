package screenshot

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"sync"
	"time"
)

const (
	TTL           = 10 * time.Minute
	MaxCaptures   = 32
	MaxStoreBytes = 64 << 20
)

var (
	ErrUnavailable = errors.New("screenshot capture unavailable (expired, restarted, or not owned by this credential); explicitly request a new capture if needed")
	ErrSegment     = errors.New("screenshot segment out of range")
	ErrCapacity    = errors.New("screenshot store is full; wait for captures to expire")
)

type Image struct {
	Data     string `json:"data"`
	MimeType string `json:"mime_type"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	Bytes    int    `json:"bytes"`
	SHA256   string `json:"sha256"`
}

// Segment is the wire protocol; segment numbers are one-based.
type Segment struct {
	Version             int       `json:"version"`
	CaptureID           string    `json:"capture_id"`
	URL                 string    `json:"url"`
	Title               string    `json:"title"`
	ExpiresAt           time.Time `json:"expires_at"`
	Segment             int       `json:"segment"`
	Segments            int       `json:"segments"`
	ViewportWidth       int       `json:"viewport_width"`
	PageWidth           int       `json:"page_width"`
	PageHeight          int       `json:"page_height"`
	CapturedHeight      int       `json:"captured_height"`
	Truncated           bool      `json:"truncated"`
	HorizontalTruncated bool      `json:"horizontal_truncated"`
	YStart              int       `json:"y_start"`
	YEnd                int       `json:"y_end"`
	Image               Image     `json:"image"`
}

type entry struct {
	owner, url, title string
	expires           time.Time
	capture           *Capture
	bytes             int
}

// Store is memory-only, so private images disappear on gateway restart. Entries
// own immutable tiles; callers must not mutate captures after Add.
type Store struct {
	mu      sync.Mutex
	entries map[string]entry
	bytes   int
	now     func() time.Time
}

func NewStore() *Store { return &Store{entries: make(map[string]entry), now: time.Now} }

func (s *Store) Add(owner, url, title string, capture *Capture) (*Segment, error) {
	if capture == nil || len(capture.Tiles) < 1 || len(capture.Tiles) > MaxSegments {
		return nil, ErrSegment
	}
	n := 0
	for _, t := range capture.Tiles {
		if len(t.Data) > MaxImageBytes || len(t.Data) == 0 {
			return nil, ErrCapacity
		}
		n += len(t.Data)
	}
	id := rand.Text()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reapLocked()
	if len(s.entries) >= MaxCaptures || s.bytes+n > MaxStoreBytes {
		return nil, ErrCapacity
	}
	e := entry{owner: owner, url: url, title: title, expires: s.now().Add(TTL), capture: capture, bytes: n}
	s.entries[id] = e
	s.bytes += n
	return segment(id, e, 1), nil
}

func (s *Store) Get(owner, id string, number int) (*Segment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reapLocked()
	e, ok := s.entries[id]
	// Identical error for wrong ownership and absent/expired IDs: no existence oracle.
	if !ok || e.owner != owner {
		return nil, ErrUnavailable
	}
	if number < 1 || number > len(e.capture.Tiles) {
		return nil, ErrSegment
	}
	return segment(id, e, number), nil
}

func (s *Store) Reap() { s.mu.Lock(); defer s.mu.Unlock(); s.reapLocked() }
func (s *Store) reapLocked() {
	for id, e := range s.entries {
		if !s.now().Before(e.expires) {
			delete(s.entries, id)
			s.bytes -= e.bytes
		}
	}
}

func segment(id string, e entry, number int) *Segment {
	c, t := e.capture, e.capture.Tiles[number-1]
	return &Segment{
		Version: Version, CaptureID: id, URL: e.url, Title: e.title, ExpiresAt: e.expires,
		Segment: number, Segments: len(c.Tiles), ViewportWidth: Width,
		PageWidth: c.PageWidth, PageHeight: c.PageHeight, CapturedHeight: c.CapturedHeight,
		Truncated: c.PageHeight > c.CapturedHeight, HorizontalTruncated: c.PageWidth > Width,
		YStart: t.YStart, YEnd: t.YEnd,
		Image: Image{Data: base64.StdEncoding.EncodeToString(t.Data), MimeType: "image/jpeg", Width: t.Width, Height: t.Height, Bytes: len(t.Data), SHA256: t.SHA256},
	}
}
