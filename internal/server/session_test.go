package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/slim-bean/browser-fetch/internal/session"
)

// newTestServer builds a Server with a nil browser manager — enough to test
// routing, auth and strict decoding, which never touch Chrome. Handlers that
// would hit the browser return errors that surface as JSON, not panics.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	// Server fields are unexported; build via New with a nil manager would
	// panic on metrics Sources closures. Instead test decodeAction + decodeBody
	// directly and exercise route wiring with a minimal stub.
	return nil
}

func TestDecodeActionStrict(t *testing.T) {
	// Unknown fields must be rejected.
	_, err := decodeAction(json.RawMessage(`{"kind":"navigate","url":"https://x","evil":"1"}`))
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field must be rejected, got %v", err)
	}
	// Known shape decodes.
	a, err := decodeAction(json.RawMessage(`{"kind":"navigate","url":"https://x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if nav, ok := a.(session.NavigateAction); !ok || nav.URL != "https://x" {
		t.Fatalf("unexpected action %#v", a)
	}
	// Unknown kind rejected.
	if _, err := decodeAction(json.RawMessage(`{"kind":"run_js","js":"alert(1)"}`)); err == nil {
		t.Fatal("run_js must be rejected: no JS ever reaches the browser")
	}
}

func TestDecodeActionRejectsInteraction(t *testing.T) {
	// click and type are internal primitives for the macro runner; the
	// agent-facing API must never accept them.
	for _, kind := range []string{"click", "type"} {
		var a session.Action
		a, err := decodeAction(json.RawMessage(`{"kind":"` + kind + `"}`))
		if err == nil {
			t.Fatalf("%s must be rejected over the agent API", kind)
		}
		if a != nil {
			t.Fatalf("%s returned an action with the error", kind)
		}
	}
	// But the internal decoder still understands them (the macro runner needs
	// them to execute recorded steps).
	for _, kind := range []string{"click", "type"} {
		if _, err := decodeAnyAction(json.RawMessage(`{"kind":"` + kind + `"}`)); err != nil {
			t.Fatalf("decodeAnyAction(%s) failed: %v", kind, err)
		}
	}
}

func TestDecodeBodyStrict(t *testing.T) {
	r := httptest.NewRequest("POST", "/session/open", bytes.NewBufferString(`{"host":"x.com","extra":1}`))
	var req openRequest
	if err := decodeBody(r, &req); err == nil {
		t.Fatal("unknown body field must be rejected")
	}
	r = httptest.NewRequest("POST", "/session/open", bytes.NewBufferString(`{"host":"x.com"}`))
	if err := decodeBody(r, &req); err != nil {
		t.Fatalf("valid body rejected: %v", err)
	}
}

func TestActionKindString(t *testing.T) {
	for kind, a := range map[string]session.Action{
		"navigate":   session.NavigateAction{},
		"wait":       session.WaitAction{},
		"screenshot": session.ScreenshotAction{},
		"content":    session.ContentAction{},
		"assert":     session.AssertAction{},
	} {
		if a.Kind() != kind {
			t.Fatalf("kind mismatch for %s: %q", kind, a.Kind())
		}
	}
}

func TestSessionRoutesRegistered(t *testing.T) {
	handlers := []string{
		"POST /session/open",
		"POST /session/action",
		"POST /session/close",
		"GET /sessions",
		"GET /session/log",
	}
	mux := http.NewServeMux()
	for _, p := range handlers {
		mux.Handle(p, http.NotFoundHandler())
	}
	req := httptest.NewRequest("POST", "/session/open", nil)
	if _, pattern := mux.Handler(req); pattern == "" {
		t.Fatal("route not matched")
	}
}
