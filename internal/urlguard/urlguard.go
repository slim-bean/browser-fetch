// Package urlguard validates fetch targets before a browser is pointed at them.
//
// The gateway drives a real browser holding real cookies, so an attacker who
// can influence the URL (prompt injection in a page the agent already read)
// must not be able to reach VM-internal services or cloud metadata endpoints.
// Resolution happens here, and the resolved IPs are checked, not just the
// hostname — otherwise a public name pointing at 127.0.0.1 would slip through.
package urlguard

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
)

type Guard struct {
	AllowPrivate bool
	// Resolver is swappable for tests.
	Resolver interface {
		LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
	}
}

func New(allowPrivate bool) *Guard {
	return &Guard{AllowPrivate: allowPrivate, Resolver: net.DefaultResolver}
}

// Check parses and validates rawURL, returning the parsed URL.
func (g *Guard) Check(ctx context.Context, rawURL string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fmt.Errorf("invalid url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("unsupported scheme %q (http and https only)", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("url has no host")
	}
	if g.AllowPrivate {
		return u, nil
	}

	// Literal IP in the URL.
	if ip := net.ParseIP(host); ip != nil {
		if blocked, why := blockedIP(ip); blocked {
			return nil, fmt.Errorf("target %s is %s", ip, why)
		}
		return u, nil
	}

	// ".localhost" and friends never resolve publicly.
	lower := strings.ToLower(host)
	if lower == "localhost" || strings.HasSuffix(lower, ".localhost") {
		return nil, fmt.Errorf("target %s is loopback", host)
	}

	addrs, err := g.Resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("cannot resolve %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("cannot resolve %s", host)
	}
	for _, a := range addrs {
		if blocked, why := blockedIP(a.IP); blocked {
			return nil, fmt.Errorf("target %s resolves to %s which is %s", host, a.IP, why)
		}
	}
	return u, nil
}

func blockedIP(ip net.IP) (bool, string) {
	switch {
	case ip.IsLoopback():
		return true, "loopback"
	case ip.IsPrivate():
		return true, "a private address"
	case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast():
		return true, "link-local (cloud metadata range)"
	case ip.IsUnspecified():
		return true, "unspecified"
	case ip.IsMulticast():
		return true, "multicast"
	case isCGNAT(ip):
		return true, "carrier-grade NAT"
	}
	return false, ""
}

// isCGNAT reports 100.64.0.0/10, used by Tailscale and carrier NAT.
func isCGNAT(ip net.IP) bool {
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
	return v4[0] == 100 && v4[1]&0xc0 == 64
}
