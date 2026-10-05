package browser

import (
	"context"
	"strings"
	"testing"
)

func TestScreenshotBlockedMediaFailsBeforeAcquiringOrContactingChrome(t *testing.T) {
	// Deliberately no pool, allocator or capture slots: none should be touched.
	m := &Manager{opts: Options{BlockMedia: true}}
	if _, err := m.Fetch(context.Background(), Request{URL: "https://example.com", Screenshot: true}); err == nil || !strings.Contains(err.Error(), "block-media=false") {
		t.Fatal("media-blocked screenshot was not explicitly rejected", err)
	}
}
