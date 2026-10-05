package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image/jpeg"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/slim-bean/browser-fetch/internal/browser"
	"github.com/slim-bean/browser-fetch/internal/config"
	"github.com/slim-bean/browser-fetch/internal/screenshot"
)

// Opt-in only: the endpoint MUST be an isolated test Chrome, not a personal profile.
func TestLivePaginatedScreenshots(t *testing.T) {
	chrome := os.Getenv("BROWSER_FETCH_TEST_CHROME_URL")
	if chrome == "" {
		t.Skip("requires isolated Chrome via BROWSER_FETCH_TEST_CHROME_URL")
	}
	var navigations atomic.Int32
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cancel" {
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, `<html><body>unfinished navigation`)
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
			case <-time.After(3 * time.Second):
			}
			return
		}
		if r.URL.Path != "/" {
			w.WriteHeader(404)
			return
		}
		navigations.Add(1)
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, `<html><head><title>Visual fixture</title><style>body{margin:0}#viewport{position:absolute;top:0;right:0}section{height:1300px}section:nth-child(odd){background:#f00}section:nth-child(even){background:#00f}</style></head><body>`+strings.Repeat(`<section>Frozen synthetic listing</section>`, 10)+`<span id="viewport"></span><script>document.querySelector('#viewport').textContent='viewport:'+innerWidth</script></body></html>`)
	}))
	defer fixture.Close()
	cfg := config.Default()
	cfg.Token, cfg.ReaderToken, cfg.ChromeURL = "root", "read", chrome
	cfg.AllowPrivate, cfg.BackgroundTabs = true, true
	cfg.HostGap, cfg.HostJitter = 0, 0
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := browser.New(context.Background(), browser.Options{ChromeURL: chrome, MaxTabs: 1, NavTimeout: 10 * time.Second, BackgroundTabs: true, Logger: log})
	defer mgr.Close()
	gateway := httptest.NewServer(New(cfg, log, mgr).Handler())
	defer gateway.Close()
	call := func(path string, body any, out any) {
		t.Helper()
		data, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", gateway.URL+path, bytes.NewReader(data))
		req.Header.Set("Authorization", "Bearer read")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != 200 {
			b, _ := io.ReadAll(res.Body)
			t.Fatal(res.Status, string(b))
		}
		if err := json.NewDecoder(res.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
	var baseline fetchResponse
	call("/fetch", fetchRequest{URL: fixture.URL}, &baseline)
	viewport := regexp.MustCompile(`id="viewport">viewport:[0-9]+`)
	originalViewport := viewport.FindString(baseline.HTML)
	if originalViewport == "" {
		t.Fatal("fixture did not report viewport")
	}
	var first fetchResponse
	call("/fetch", fetchRequest{URL: fixture.URL, Screenshot: true}, &first)
	if viewport.FindString(first.HTML) != `id="viewport">viewport:1280` {
		t.Fatal("capture viewport not deterministic")
	}
	shot := first.Screenshot
	if shot == nil || shot.Segments != 10 || shot.CapturedHeight != screenshot.MaxHeight || !shot.Truncated || !strings.Contains(first.HTML, "Frozen synthetic listing") {
		t.Fatal("bad capture manifest", shot)
	}
	decode := func(s *screenshot.Segment) {
		t.Helper()
		data, err := base64.StdEncoding.DecodeString(s.Image.Data)
		if err != nil {
			t.Fatal(err)
		}
		img, err := jpeg.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		if img.Bounds().Dx() != s.Image.Width || img.Bounds().Dy() != s.Image.Height || len(data) > screenshot.MaxImageBytes {
			t.Fatal("unbounded or invalid image")
		}
	}
	decode(shot)
	count := navigations.Load()
	for i := 1; i <= shot.Segments; i++ {
		var next screenshot.Segment
		call("/fetch/screenshot", map[string]any{"capture_id": shot.CaptureID, "segment": i}, &next)
		decode(&next)
		if i == 1 && next.Image.SHA256 != shot.Image.SHA256 {
			t.Fatal("snapshot changed")
		}
	}
	if navigations.Load() != count {
		t.Fatal("continuation navigated")
	}
	var text fetchResponse
	call("/fetch", fetchRequest{URL: fixture.URL}, &text)
	if text.Screenshot != nil || text.HTML == "" {
		t.Fatal("text-only fetch regressed")
	}
	if viewport.FindString(text.HTML) != originalViewport {
		t.Fatal("capture emulation leaked into reused text worker")
	}
	// Caller cancellation after viewport setup must also reset emulation before reuse.
	cancelBody, _ := json.Marshal(fetchRequest{URL: fixture.URL + "/cancel", Screenshot: true, TimeoutMS: 100})
	cancelReq, _ := http.NewRequest("POST", gateway.URL+"/fetch", bytes.NewReader(cancelBody))
	cancelReq.Header.Set("Authorization", "Bearer read")
	cancelRes, err := http.DefaultClient.Do(cancelReq)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, cancelRes.Body)
	_ = cancelRes.Body.Close()
	if cancelRes.StatusCode != 504 {
		t.Fatal("expected caller deadline", cancelRes.Status)
	}
	call("/fetch", fetchRequest{URL: fixture.URL}, &text)
	if viewport.FindString(text.HTML) != originalViewport {
		t.Fatal("capture emulation leaked after cancellation")
	}
	// Text and visual requests to the same URL must NOT deduplicate together.
	errors := make(chan error, 2)
	for _, visual := range []bool{false, true} {
		go func() {
			data, _ := json.Marshal(fetchRequest{URL: fixture.URL, Screenshot: visual})
			req, _ := http.NewRequest("POST", gateway.URL+"/fetch", bytes.NewReader(data))
			req.Header.Set("Authorization", "Bearer read")
			res, err := http.DefaultClient.Do(req)
			if err == nil {
				defer res.Body.Close()
				var result fetchResponse
				err = json.NewDecoder(res.Body).Decode(&result)
				if res.StatusCode != 200 {
					err = io.ErrUnexpectedEOF
				}
				if err == nil && (result.Screenshot != nil) != visual {
					err = io.ErrUnexpectedEOF
				}
			}
			errors <- err
		}()
	}
	for i := 0; i < 2; i++ {
		if err := <-errors; err != nil {
			t.Fatal("dedupe mixed text and screenshot", err)
		}
	}
}
