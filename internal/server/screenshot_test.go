package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/slim-bean/browser-fetch/internal/browser"
	"github.com/slim-bean/browser-fetch/internal/config"
	"github.com/slim-bean/browser-fetch/internal/screenshot"
)

func TestScreenshotRoutesAndOwnership(t *testing.T) {
	cfg := config.Default()
	cfg.Token, cfg.DriverToken, cfg.ReaderToken = "root", "drive", "read"
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := browser.New(context.Background(), browser.Options{ChromeURL: "http://127.0.0.1:1", Logger: log})
	defer mgr.Close()
	s := New(cfg, log, mgr)
	h := s.Handler()
	var source bytes.Buffer
	if err := png.Encode(&source, image.NewRGBA(image.Rect(0, 0, screenshot.Width, 2700))); err != nil {
		t.Fatal(err)
	}
	capture, err := screenshot.Split(context.Background(), source.Bytes(), screenshot.Width, 2700, 2700)
	if err != nil {
		t.Fatal(err)
	}
	request := func(token, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/fetch/screenshot", strings.NewReader(body))
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for _, owner := range []struct {
		class tokenClass
		token string
	}{{classReader, "read"}, {classDriver, "drive"}, {classFull, "root"}} {
		first, err := s.screenshots.Add(fmt.Sprintf("class:%d", owner.class), "https://example.com/", "private fixture", capture)
		if err != nil {
			t.Fatal(err)
		}
		body := fmt.Sprintf(`{"capture_id":%q,"segment":2}`, first.CaptureID)
		w := request(owner.token, body)
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("owner denied", w.Code, w.Body.String())
		}
		var second screenshot.Segment
		if err := json.Unmarshal(w.Body.Bytes(), &second); err != nil || second.Segment != 2 || second.CaptureID != first.CaptureID {
			t.Fatal("bad segment", err)
		}
		for _, other := range []string{"read", "drive", "root"} {
			if other == owner.token {
				continue
			}
			w = request(other, body)
			if w.Code != 410 || strings.Contains(w.Body.String(), "private fixture") {
				t.Fatal("cross-credential disclosure", w.Code)
			}
		}
		if w := request("", body); w.Code != 401 {
			t.Fatal("unauthenticated image retrieval")
		}
		if w := request(owner.token, fmt.Sprintf(`{"capture_id":%q,"segment":3}`, first.CaptureID)); w.Code != 400 {
			t.Fatal("accepted invalid segment")
		}
	}
	for _, body := range []string{`{}`, `{"capture_id":"x","segment":0}`, `{"capture_id":"x","segment":1,"url":"https://example.com"}`, `{} {}`, `{"capture_id":"x","segment":1.5}`} {
		if w := request("read", body); w.Code != 400 {
			t.Fatal("accepted invalid body", body, w.Code)
		}
	}
}

func TestFetchScreenshotParsing(t *testing.T) {
	for _, value := range []string{"true", "false"} {
		r := httptest.NewRequest(http.MethodPost, "/fetch", strings.NewReader(`{"url":"https://example.com","screenshot":`+value+`}`))
		req, err := parseFetchRequest(r)
		if err != nil || req.Screenshot != (value == "true") {
			t.Fatal("bad screenshot parse", err)
		}
	}
	for _, body := range []string{`{"url":"https://example.com","screenshot":"true"}`, `{"url":"https://example.com","screenshot":{}}`, `{"url":"https://example.com"} {}`} {
		if _, err := parseFetchRequest(httptest.NewRequest(http.MethodPost, "/fetch", strings.NewReader(body))); err == nil {
			t.Fatal("accepted invalid screenshot request")
		}
	}
}
