package server

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/slim-bean/browser-fetch/internal/browser"
	"github.com/slim-bean/browser-fetch/internal/config"
)

func TestNativeHistoryHTTPContract(t *testing.T) {
	root := t.TempDir()
	os.Mkdir(filepath.Join(root, "Default"), 0700)
	db, err := sql.Open("sqlite", filepath.Join(root, "Default", "History"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE urls(url TEXT,title TEXT,visit_count INTEGER,last_visit_time INTEGER,hidden INTEGER);
 INSERT INTO urls VALUES('https://example.com','fixture',2,13394473600000000,0);`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Default()
	cfg.Token = "fixture"
	cfg.DriverToken = "drive"
	cfg.ReaderToken = "read"
	cfg.HistoryRoot = root
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
	for _, token := range []string{"", "Bearer read", "Bearer drive", "xxxxxxxfixture"} {
		for _, path := range []string{"/history/query", "/history/sources", "/history/search"} {
			if got := request(path, token, `{}`).Code; got != 401 {
				t.Fatalf("%s %s: %d", token, path, got)
			}
		}
	}
	if res := request("/history/sources", "Bearer fixture", ""); res.Code != 200 || !strings.Contains(res.Body.String(), `"version":2`) || strings.Contains(res.Body.String(), root) {
		t.Fatal(res.Code, res.Body.String())
	}
	res := request("/history/query", "Bearer fixture", `{"version":2,"sourceId":"assistant/Default","terms":["fixture"]}`)
	if res.Code != 200 || !strings.Contains(res.Body.String(), `"url":"https://example.com"`) || strings.Contains(res.Body.String(), `"content"`) {
		t.Fatal(res.Code, res.Body.String())
	}
	for _, body := range []string{`{"version":2,"sourceId":"assistant/Default","sql":"select *"}`, `{"version":2,"sourceId":"assistant/Default","path":"/etc/passwd"}`, `{"version":1}`, `{} {}`, `{"terms":["` + strings.Repeat("x", 65536) + `"]}`} {
		if res := request("/history/query", "Bearer fixture", body); res.Code != 400 {
			t.Fatal(res.Code, res.Body.String())
		}
	}
	if res := request("/history/query", "Bearer fixture", `{"version":2,"sourceId":"../../etc/passwd"}`); res.Code != 404 {
		t.Fatal(res.Code, res.Body.String())
	}
	if res := request("/history/search", "Bearer fixture", `{"query":"fixture"}`); res.Code != 410 {
		t.Fatal(res.Code, res.Body.String())
	}
	s.historySlots <- struct{}{}
	s.historySlots <- struct{}{}
	if got := request("/history/query", "Bearer fixture", `{}`).Code; got != http.StatusTooManyRequests {
		t.Fatal(got)
	}
	<-s.historySlots
	<-s.historySlots
	s.cfg.HistoryRoot = ""
	if got := request("/history/sources", "Bearer fixture", "").Code; got != 501 {
		t.Fatal(got)
	}
}
