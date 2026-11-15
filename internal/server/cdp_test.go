package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
		for {
			data, op, err := wsutil.ReadClientData(conn)
			if err != nil {
				return
			}
			if err := wsutil.WriteServerMessage(conn, op, data); err != nil {
				return
			}
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
	// Exercise masking kernels on both sides of their block boundaries and
	// extended payload lengths through the real HTTP-hijacking proxy path.
	for _, size := range []int{0, 15, 16, 17, 125, 126, 65537} {
		t.Run(fmt.Sprintf("binary-%d", size), func(t *testing.T) {
			payload := bytes.Repeat([]byte{0x00, 0x80, 0xff, 0x42}, (size+3)/4)[:size]
			if err := wsutil.WriteClientBinary(conn, payload); err != nil {
				t.Fatal(err)
			}
			got, op, err := wsutil.ReadServerData(conn)
			if err != nil || op != ws.OpBinary || !bytes.Equal(got, payload) {
				t.Fatalf("binary echo: opcode=%v bytes=%d err=%v", op, len(got), err)
			}
		})
	}
	// A control frame between text fragments must not appear in the echoed
	// message or disrupt the UTF-8 sequence split across fragment boundaries.
	fragments := []ws.Frame{
		ws.NewFrame(ws.OpText, false, []byte{'a', 0xe2}),
		ws.NewPingFrame([]byte("probe")),
		ws.NewFrame(ws.OpContinuation, true, []byte{0x82, 0xac, 'z'}),
	}
	for _, frame := range fragments {
		if err := ws.WriteFrame(conn, ws.MaskFrameInPlace(frame)); err != nil {
			t.Fatal(err)
		}
	}
	got, op, err := wsutil.ReadServerData(conn)
	if err != nil || op != ws.OpText || string(got) != "a€z" {
		t.Fatalf("fragmented echo: opcode=%v payload=%q err=%v", op, got, err)
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
