package server

import (
	"context"
	"encoding/json"
	"fmt"
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

func TestHistoryHelperProcess(t *testing.T) {
	if os.Getenv("BROWSER_FETCH_TEST_HELPER") != "1" {
		return
	}
	var request map[string]any
	_ = json.NewDecoder(os.Stdin).Decode(&request)
	switch os.Args[len(os.Args)-1] {
	case "large":
		fmt.Print(strings.Repeat("x", historyOutputLimit+1))
	case "bad":
		fmt.Print(`{"version":999}`)
	default:
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"version": 1, "operation": request["operation"], "params": request["params"], "content": []any{}, "details": map[string]any{}})
	}
	os.Exit(0)
}

func TestHistoryHTTPContract(t *testing.T) {
	t.Setenv("BROWSER_FETCH_TEST_HELPER", "1")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Default()
	cfg.Token = "fixture"
	mgr := browser.New(context.Background(), browser.Options{ChromeURL: cfg.ChromeURL, Logger: log})
	defer mgr.Close()
	s := New(cfg, log, mgr)
	handler := s.Handler()
	request := func(path, token, body string) *httptest.ResponseRecorder {
		method := "POST"
		if strings.HasSuffix(path, "sources") {
			method = "GET"
		}
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", token)
		}
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		return res
	}
	if got := request("/history/search", "", `{}`).Code; got != 401 {
		t.Fatal(got)
	}
	if got := request("/history/search", "xxxxxxxfixture", `{}`).Code; got != 401 {
		t.Fatal("invalid auth scheme accepted", got)
	}
	if got := request("/history/search", "Bearer fixture", `{}`).Code; got != 501 {
		t.Fatal(got)
	}
	for _, mode := range []string{"ok", "bad", "large"} {
		s.cfg.HistoryCommand = []string{os.Args[0], "-test.run=TestHistoryHelperProcess", "--", mode}
		res := request("/history/search", "Bearer fixture", `{"query":"example"}`)
		if mode != "ok" {
			if res.Code != 502 {
				t.Fatalf("%s: %d", mode, res.Code)
			}
			continue
		}
		if res.Code != 200 || !strings.Contains(res.Body.String(), `"query":"example"`) {
			t.Fatal(res.Code, res.Body.String())
		}
	}
	s.cfg.HistoryCommand = []string{os.Args[0], "-test.run=TestHistoryHelperProcess", "--", "ok"}
	if got := request("/history/sources", "Bearer fixture", ""); got.Code != 200 || !strings.Contains(got.Body.String(), `"operation":"sources"`) {
		t.Fatal(got.Code, got.Body.String())
	}
	if got := request("/history/search", "Bearer fixture", `{"query":"`+strings.Repeat("x", 65536)+`"}`).Code; got != 400 {
		t.Fatal(got)
	}
	s.historySlots <- struct{}{}
	s.historySlots <- struct{}{}
	if got := request("/history/search", "Bearer fixture", `{}`).Code; got != http.StatusTooManyRequests {
		t.Fatal(got)
	}
}
