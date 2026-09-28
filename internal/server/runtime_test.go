package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/slim-bean/browser-fetch/internal/browser"
	"github.com/slim-bean/browser-fetch/internal/config"
)

func TestRuntimeRequiresAuthAndIdentifiesConfiguration(t *testing.T) {
	cfg := config.Default()
	cfg.Token = "test-secret"
	cfg.ChromeURL = "http://127.0.0.1:19322"
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := browser.New(context.Background(), browser.Options{ChromeURL: cfg.ChromeURL, MaxTabs: 1, Logger: log})
	defer mgr.Close()
	handler := New(cfg, log, mgr).Handler()
	for _, token := range []string{"", "wrong", cfg.Token} {
		req := httptest.NewRequest(http.MethodGet, "/runtime", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		if token != cfg.Token {
			if res.Code != http.StatusUnauthorized {
				t.Fatalf("unauthenticated runtime: %d", res.Code)
			}
			continue
		}
		if res.Code != http.StatusOK {
			t.Fatalf("runtime: %d", res.Code)
		}
		var result struct {
			Service   string `json:"service"`
			PID       int    `json:"pid"`
			ChromeURL string `json:"chrome_url"`
		}
		if err := json.Unmarshal(res.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.Service != "browser-fetch" || result.PID != os.Getpid() || result.ChromeURL != cfg.ChromeURL {
			t.Fatalf("bad identity: %+v", result)
		}
		if strings.Contains(res.Body.String(), cfg.Token) {
			t.Fatal("runtime response contains a secret")
		}
	}
}
