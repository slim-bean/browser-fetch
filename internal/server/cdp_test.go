package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
	"github.com/slim-bean/browser-fetch/internal/browser"
	"github.com/slim-bean/browser-fetch/internal/config"
)

func TestCDPDiscoveryAndAuthenticatedWebSocket(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Error("proxy token leaked to Chrome")
		}
		if strings.HasPrefix(r.URL.Path, "/json") {
			writeJSON(w, 200, map[string]any{"Browser": "fixture", "webSocketDebuggerUrl": "ws://127.0.0.1:9222/devtools/browser/fixture"})
			return
		}
		conn, _, _, err := ws.UpgradeHTTP(r, w)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		data, op, err := wsutil.ReadClientData(conn)
		if err == nil {
			_ = wsutil.WriteServerMessage(conn, op, data)
		}
	}))
	defer upstream.Close()
	cfg := config.Default()
	cfg.Token = "test-token"
	cfg.EnableCDP = true
	cfg.ChromeURL = upstream.URL
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := browser.New(context.Background(), browser.Options{ChromeURL: upstream.URL, Logger: log})
	defer mgr.Close()
	gateway := httptest.NewServer(New(cfg, log, mgr).Handler())
	defer gateway.Close()
	for _, path := range []string{"/cdp/json/version", "/cdp/devtools/browser/fixture"} {
		res, err := http.Get(gateway.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 401 {
			t.Fatalf("unauthenticated %s: %d", path, res.StatusCode)
		}
	}
	if upstreamCalls.Load() != 0 {
		t.Fatal("unauthenticated request reached Chrome")
	}
	req, _ := http.NewRequest("GET", gateway.URL+"/cdp/json/version/", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var version map[string]string
	if err := json.NewDecoder(res.Body).Decode(&version); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	want := strings.Replace(gateway.URL, "http://", "ws://", 1) + "/cdp/devtools/browser/fixture"
	if version["webSocketDebuggerUrl"] != want {
		t.Fatalf("discovery URL: %+v", version)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// A WebSocket upgrade must authenticate too, not just HTTP discovery.
	if conn, _, _, err := ws.DefaultDialer.Dial(ctx, want); err == nil {
		conn.Close()
		t.Fatal("unauthenticated websocket accepted")
	}
	dialer := ws.Dialer{Header: ws.HandshakeHeaderHTTP(http.Header{"Authorization": []string{"Bearer test-token"}})}
	conn, _, _, err := dialer.Dial(ctx, want)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	payload := []byte(`{"id":1,"method":"Browser.getVersion"}`)
	if err := wsutil.WriteClientText(conn, payload); err != nil {
		t.Fatal(err)
	}
	got, _, err := wsutil.ReadServerData(conn)
	if err != nil || string(got) != string(payload) {
		t.Fatalf("websocket round trip: %s %v", got, err)
	}
}

func TestCDPExplicitPublicURLAndOptIn(t *testing.T) {
	cfg := config.Default()
	cfg.PublicURL = "https://browser.example/assistant/"
	s := &Server{cfg: cfg}
	request := httptest.NewRequest("GET", "http://untrusted.invalid/cdp/json/version", nil)
	request.Header.Set("X-Forwarded-Host", "not-trusted.invalid")
	if got := s.cdpBase(request); got != "wss://browser.example/assistant/cdp" {
		t.Fatal(got)
	}
	data := []any{map[string]any{"webSocketDebuggerUrl": "ws://127.0.0.1/devtools/page/abc"}}
	rewriteDebuggerURLs(data, s.cdpBase(request))
	if data[0].(map[string]any)["webSocketDebuggerUrl"] != "wss://browser.example/assistant/cdp/devtools/page/abc" {
		t.Fatal(data)
	}
}
