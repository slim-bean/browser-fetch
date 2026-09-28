package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// A fixed upstream, not a general forward proxy. All paths and upgrades are
// authenticated by route(). Only discovery is transformed; WS frames are opaque.
func (s *Server) cdpProxy() http.Handler {
	upstream, _ := url.Parse(s.cfg.ChromeURL)
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(upstream)
			pr.Out.URL.Path = strings.TrimSuffix(upstream.Path, "/") + strings.TrimPrefix(pr.In.URL.Path, "/cdp")
			pr.Out.URL.RawPath = ""
			pr.Out.Host = upstream.Host // Chrome rejects arbitrary DNS Host headers
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Del("Origin")
			pr.Out.Header.Del("Accept-Encoding")
		},
		Transport: &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext, ResponseHeaderTimeout: 10 * time.Second},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			writeJSON(w, http.StatusBadGateway, errorBody{Code: "cdp_unavailable", Error: "Chrome CDP is unavailable"})
		},
	}
	proxy.ModifyResponse = func(res *http.Response) error {
		if res.StatusCode != http.StatusOK {
			return nil
		}
		path := strings.TrimRight(strings.TrimPrefix(res.Request.URL.Path, strings.TrimSuffix(upstream.Path, "/")), "/")
		if path != "/json" && path != "/json/list" && path != "/json/version" {
			return nil
		}
		defer res.Body.Close()
		data, err := io.ReadAll(io.LimitReader(res.Body, 2*1024*1024+1))
		if err != nil {
			return err
		}
		if len(data) > 2*1024*1024 {
			return fmt.Errorf("oversized CDP discovery")
		}
		var document any
		if err := json.Unmarshal(data, &document); err != nil {
			return err
		}
		// The original client request is retained via context by the outer handler.
		external, ok := res.Request.Context().Value(cdpBaseKey{}).(string)
		if !ok {
			return fmt.Errorf("missing public CDP address")
		}
		rewriteDebuggerURLs(document, external)
		data, err = json.Marshal(document)
		if err != nil {
			return err
		}
		res.Body = io.NopCloser(bytes.NewReader(data))
		res.ContentLength = int64(len(data))
		res.Header.Set("Content-Length", strconv.Itoa(len(data)))
		res.Header.Set("Cache-Control", "no-store")
		return nil
	}
	return proxy
}

type cdpBaseKey struct{}

func rewriteDebuggerURLs(value any, base string) {
	switch v := value.(type) {
	case map[string]any:
		if raw, ok := v["webSocketDebuggerUrl"].(string); ok {
			if parsed, err := url.Parse(raw); err == nil {
				v["webSocketDebuggerUrl"] = base + parsed.EscapedPath()
			}
		}
		for _, item := range v {
			rewriteDebuggerURLs(item, base)
		}
	case []any:
		for _, item := range v {
			rewriteDebuggerURLs(item, base)
		}
	}
}

func (s *Server) cdpBase(r *http.Request) string {
	base := strings.TrimSuffix(s.cfg.PublicURL, "/")
	if base == "" {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		base = scheme + "://" + r.Host
	}
	// Forwarded headers are deliberately not trusted. Configure public-url when
	// TLS terminates elsewhere or an ingress strips a path prefix.
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return ""
	}
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/cdp"
	return u.String()
}
