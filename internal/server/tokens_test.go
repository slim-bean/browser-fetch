package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/slim-bean/browser-fetch/internal/config"
)

// httptestRequest builds a request with the given bearer token.
func httptestRequest(tok string) *http.Request {
	r := httptest.NewRequest("POST", "/session/open", nil)
	if tok != "" {
		r.Header.Set("Authorization", "Bearer "+tok)
	}
	return r
}

func TestTokenClasses(t *testing.T) {
	s := &Server{cfg: config.Config{
		Token:       "root",
		DriverToken: "drive",
		ReaderToken: "read",
	}}

	cases := []struct {
		tok  string
		want tokenClass
	}{
		{"root", classFull},
		{"drive", classDriver},
		{"read", classReader},
		{"wrong", classNone},
		{"", classNone},
	}
	for _, c := range cases {
		if got := s.tokenClass(httptestRequest(c.tok)); got != c.want {
			t.Fatalf("token %q: got class %d, want %d", c.tok, got, c.want)
		}
	}
}

func TestTokenClassesNoRootToken(t *testing.T) {
	// AllowNoToken grants full; otherwise nothing.
	s := &Server{cfg: config.Config{AllowNoToken: true}}
	if got := s.tokenClass(httptestRequest("")); got != classFull {
		t.Fatalf("allow-no-token should be full, got %d", got)
	}
	s = &Server{cfg: config.Config{}}
	if got := s.tokenClass(httptestRequest("")); got != classNone {
		t.Fatalf("no token configured should be none, got %d", got)
	}
}

func TestAllowHosts(t *testing.T) {
	check := allowHosts([]string{"chase.com", ".fidelity.com"})
	for _, ok := range []string{
		"https://chase.com/login",
		"https://CHASE.com/x",
		"https://fidelity.com",
		"https://oltx.fidelity.com/ftg",
	} {
		if err := check(ok); err != nil {
			t.Fatalf("%s should be allowed: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"https://notchase.com",          // suffix without dot boundary
		"https://evil-chase.com",        // ditto
		"https://fidelity.com.evil.com", // suffix on the wrong side
		"https://chase.com.evil.com",
	} {
		if err := check(bad); err == nil {
			t.Fatalf("%s should be rejected", bad)
		}
	}
	// Empty list = no restriction.
	if check := allowHosts(nil); check != nil {
		t.Fatal("empty allowlist must disable the hook")
	}
}
