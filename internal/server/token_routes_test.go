package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/slim-bean/browser-fetch/internal/browser"
	"github.com/slim-bean/browser-fetch/internal/config"
)

// Mutation-sensitive route tests: accepting any recognized token on generic
// routes would let a finance driver bypass typed-action restrictions using CDP.
func TestTokenClassesAcrossRealRoutes(t *testing.T) {
	var contacted atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contacted.Add(1)
		writeJSON(w, 200, map[string]string{"webSocketDebuggerUrl": "ws://localhost/devtools/browser/test"})
	}))
	defer upstream.Close()
	cfg := config.Default()
	cfg.Token = "root"
	cfg.DriverToken = "drive"
	cfg.ReaderToken = "read"
	cfg.EnableCDP = true
	cfg.ChromeURL = upstream.URL
	cfg.MetricsAuth = true
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := browser.New(context.Background(), browser.Options{ChromeURL: cfg.ChromeURL, Logger: log})
	defer mgr.Close()
	handler := New(cfg, log, mgr).Handler()
	call := func(token, method, path string) int {
		req := httptest.NewRequest(method, path, strings.NewReader(`{}`))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		return res.Code
	}
	for _, path := range []string{"/runtime", "/stats", "/metrics", "/debug", "/history/sources", "/cdp/json/version"} {
		for _, token := range []string{"", "read", "drive"} {
			if code := call(token, "GET", path); code != 401 {
				t.Fatalf("%s reached root route %s: %d", token, path, code)
			}
		}
	}
	if contacted.Load() != 0 {
		t.Fatal("non-root request reached Chrome")
	}
	for _, token := range []string{"root", "drive", "read"} {
		if code := call(token, "POST", "/fetch"); code == 401 {
			t.Fatalf("%s denied fetch", token)
		}
	}
	for _, path := range []string{"/session/open", "/session/action", "/session/close", "/macro/replay", "/macro/record/start"} {
		if code := call("read", "POST", path); code != 401 {
			t.Fatalf("reader reached %s: %d", path, code)
		}
		for _, token := range []string{"drive", "root"} {
			if code := call(token, "POST", path); code == 401 {
				t.Fatalf("%s denied existing driver route %s", token, path)
			}
		}
	}
	if code := call("root", "GET", "/cdp/json/version"); code != 200 {
		t.Fatal("root CDP denied", code)
	}
	if contacted.Load() != 1 {
		t.Fatal("expected only root to contact Chrome")
	}
}
