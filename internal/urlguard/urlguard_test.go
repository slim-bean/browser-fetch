package urlguard

import (
	"context"
	"net"
	"strings"
	"testing"
)

type fakeResolver map[string][]string

func (f fakeResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	ips, ok := f[host]
	if !ok {
		return nil, &net.DNSError{Err: "not found", Name: host, IsNotFound: true}
	}
	out := make([]net.IPAddr, 0, len(ips))
	for _, s := range ips {
		out = append(out, net.IPAddr{IP: net.ParseIP(s)})
	}
	return out, nil
}

func newTestGuard(allowPrivate bool, res fakeResolver) *Guard {
	g := New(allowPrivate)
	g.Resolver = res
	return g
}

func TestRejectsNonHTTPSchemes(t *testing.T) {
	g := newTestGuard(false, fakeResolver{})
	for _, raw := range []string{
		"file:///etc/passwd",
		"chrome://settings",
		"data:text/html,<h1>hi",
		"javascript:alert(1)",
		"ftp://example.com/x",
	} {
		if _, err := g.Check(context.Background(), raw); err == nil {
			t.Errorf("Check(%q) allowed, want rejection", raw)
		}
	}
}

func TestRejectsLiteralPrivateAddresses(t *testing.T) {
	g := newTestGuard(false, fakeResolver{})
	cases := map[string]string{
		"http://127.0.0.1:9222/json":      "loopback",
		"http://[::1]:8080/":              "loopback",
		"http://10.1.2.3/":                "private",
		"http://192.168.1.1/":             "private",
		"http://172.16.5.4/":              "private",
		"http://169.254.169.254/metadata": "link-local",
		"http://100.100.0.1/":             "carrier-grade",
		"http://0.0.0.0/":                 "unspecified",
	}
	for raw, want := range cases {
		_, err := g.Check(context.Background(), raw)
		if err == nil {
			t.Errorf("Check(%q) allowed, want rejection", raw)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Check(%q) error = %q, want mention of %q", raw, err, want)
		}
	}
}

func TestRejectsLocalhostNames(t *testing.T) {
	g := newTestGuard(false, fakeResolver{})
	for _, raw := range []string{"http://localhost:3000/", "http://api.localhost/"} {
		if _, err := g.Check(context.Background(), raw); err == nil {
			t.Errorf("Check(%q) allowed, want rejection", raw)
		}
	}
}

// A public name that resolves to a private address is the interesting attack:
// hostname checks alone would let it through.
func TestRejectsDNSRebinding(t *testing.T) {
	g := newTestGuard(false, fakeResolver{"evil.example": {"127.0.0.1"}})
	_, err := g.Check(context.Background(), "http://evil.example/")
	if err == nil {
		t.Fatal("allowed a name resolving to loopback")
	}
	if !strings.Contains(err.Error(), "resolves to") {
		t.Fatalf("error = %q, want it to mention resolution", err)
	}
}

func TestRejectsWhenAnyAddressIsPrivate(t *testing.T) {
	g := newTestGuard(false, fakeResolver{"mixed.example": {"93.184.216.34", "10.0.0.5"}})
	if _, err := g.Check(context.Background(), "http://mixed.example/"); err == nil {
		t.Fatal("allowed a name with one private address")
	}
}

func TestAllowsPublicTargets(t *testing.T) {
	g := newTestGuard(false, fakeResolver{"example.com": {"93.184.216.34"}})
	u, err := g.Check(context.Background(), "https://example.com/a/b?c=d")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if u.Host != "example.com" || u.Path != "/a/b" {
		t.Fatalf("parsed %+v, want example.com/a/b", u)
	}
}

func TestAllowPrivateBypassesChecks(t *testing.T) {
	g := newTestGuard(true, fakeResolver{})
	if _, err := g.Check(context.Background(), "http://127.0.0.1:8080/"); err != nil {
		t.Fatalf("with AllowPrivate: %v", err)
	}
}

func TestRejectsUnresolvable(t *testing.T) {
	g := newTestGuard(false, fakeResolver{})
	if _, err := g.Check(context.Background(), "https://nx.invalid/"); err == nil {
		t.Fatal("allowed an unresolvable host")
	}
}
