// Package server exposes the gateway over HTTP.
//
// Routes:
//
//	POST /fetch         render a URL (auth)      GET /fetch?url=… also works
//	GET  /healthz       Chrome + pool health     (no auth: for probes)
//	GET  /stats         scheduler snapshot (auth)
//	GET  /debug         recent requests, config, live state (auth, on by default)
//	GET  /debug/pprof/… Go profiling (auth)
//	GET  /metrics       Prometheus (auth optional, see -metrics-auth)
package server

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	neturl "net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/slim-bean/browser-fetch/internal/browser"
	"github.com/slim-bean/browser-fetch/internal/config"
	"github.com/slim-bean/browser-fetch/internal/history"
	"github.com/slim-bean/browser-fetch/internal/logx"
	"github.com/slim-bean/browser-fetch/internal/macro"
	"github.com/slim-bean/browser-fetch/internal/metrics"
	"github.com/slim-bean/browser-fetch/internal/reqlog"
	"github.com/slim-bean/browser-fetch/internal/scheduler"
	"github.com/slim-bean/browser-fetch/internal/secrets"
	"github.com/slim-bean/browser-fetch/internal/session"
	"github.com/slim-bean/browser-fetch/internal/urlguard"
)

type Server struct {
	cfg          config.Config
	log          *slog.Logger
	mgr          *browser.Manager
	sched        *scheduler.Scheduler[*browser.Result]
	guard        *urlguard.Guard
	ring         *reqlog.Ring
	met          *metrics.Metrics
	sessions     *session.Manager
	macros       *macroState
	admin        *Admin // human-only macro admin band; nil when disabled
	secrets      macro.SecretResolver
	start        time.Time
	historySlots chan struct{}
}

func New(cfg config.Config, log *slog.Logger, mgr *browser.Manager) *Server {
	// The 1Password resolver is built at startup so a bad service account
	// token fails loudly at boot, not mid-replay. Without a token the server
	// runs resolver-less: type steps abort at replay (fail-closed).
	var resolver macro.SecretResolver
	if cfg.OpServiceAccountToken != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		op, err := secrets.NewOnePassword(ctx, cfg.OpServiceAccountToken)
		if err != nil {
			log.Error("1password secret resolver unavailable; type steps will abort",
				"err", err)
		} else {
			resolver = op
			log.Info("1password secret resolver enabled",
				"note", "op:// references resolve at replay time; values never logged")
		}
	}
	s := &Server{
		cfg:          cfg,
		log:          log,
		mgr:          mgr,
		guard:        urlguard.New(cfg.AllowPrivate),
		ring:         reqlog.NewRing(cfg.DebugRing),
		start:        time.Now(),
		historySlots: make(chan struct{}, 2),
		secrets:      resolver,
		sched: scheduler.New[*browser.Result](scheduler.Options{
			MaxSlots: cfg.MaxTabs,
			HostGap:  cfg.HostGap,
			Jitter:   cfg.HostJitter,
		}),
	}
	s.sessions = session.New(log, func(ctx context.Context) (session.Tab, func(), error) {
		tab, release, err := mgr.Acquire(ctx)
		if err != nil {
			return nil, nil, err
		}
		return browserSessionTab{tab}, release, nil
	}, session.Options{
		MaxSessions:   cfg.MaxSessions,
		MaxIdle:       cfg.SessionIdle,
		ActionTimeout: cfg.ActionTimeout,
		AssertTTL:     cfg.AssertTTL,
		MaxLog:        200,
		AllowNavigate: allowHosts(cfg.AllowHosts),
	})
	if ms, msErr := newMacroState(cfg.MacroStore); msErr != nil {
		log.Error("macro store unavailable; macro endpoints disabled", "dir", cfg.MacroStore, "err", msErr)
	} else {
		s.macros = ms
		if cfg.AdminAddr != "" {
			s.admin = NewAdmin(ms.store, cfg.AdminToken)
		}
	}
	go s.sessionsReaper()
	s.met = metrics.New(metrics.Sources{
		TabsBusy:    func() float64 { return float64(mgr.Health().Tabs.Busy) },
		TabsIdle:    func() float64 { return float64(mgr.Health().Tabs.Idle) },
		TabsCreated: func() float64 { return float64(mgr.Health().Tabs.Created) },
		QueueDepth:  func() float64 { return float64(s.sched.Snapshot().Queued) },
		Running:     func() float64 { return float64(s.sched.Snapshot().Running) },
		AssistsPending: func() float64 {
			return float64(len(mgr.Health().Assists))
		},
		Connected: func() float64 {
			if mgr.Health().Connected {
				return 1
			}
			return 0
		},
	})
	return s
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.Handle("POST /fetch", s.route("fetch", true, s.handleFetch))
	mux.Handle("GET /fetch", s.route("fetch", true, s.handleFetch))
	mux.Handle("GET /healthz", s.route("healthz", false, s.handleHealth))
	mux.Handle("GET /stats", s.route("stats", true, s.handleStats))
	mux.Handle("POST /history/query", s.route("history", true, s.handleHistory))
	mux.Handle("POST /history/search", s.route("history", true, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusGone, errorBody{Code: "history_protocol_changed", Error: "The helper search API was removed; update your client to history protocol v2 (/history/query)"})
	}))
	mux.Handle("GET /history/sources", s.route("history", true, s.handleHistory))
	if s.cfg.EnableCDP {
		proxy := s.cdpProxy()
		mux.Handle("/cdp/", s.route("cdp", true, func(w http.ResponseWriter, r *http.Request) {
			base := s.cdpBase(r)
			if base == "" {
				writeJSON(w, http.StatusBadRequest, errorBody{Code: "bad_request", Error: "Invalid gateway Host"})
				return
			}
			proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), cdpBaseKey{}, base)))
		}))
	}
	// Authenticated identity for local supervisors. No token or target content.
	mux.Handle("GET /runtime", s.route("runtime", true, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"service": "browser-fetch", "pid": os.Getpid(), "chrome_url": s.cfg.ChromeURL,
			"capabilities":    map[string]bool{"cdp": s.cfg.EnableCDP, "history": s.cfg.HistoryRoot != ""},
			"historyProtocol": history.Version,
		})
	}))
	mux.Handle("POST /session/open", s.routeDriver("session_open", s.handleSessionOpen))
	mux.Handle("POST /session/action", s.routeDriver("session_action", s.handleSessionAction))
	mux.Handle("POST /session/close", s.routeDriver("session_close", s.handleSessionClose))
	mux.Handle("GET /sessions", s.routeDriver("sessions", s.handleSessionList))
	mux.Handle("GET /session/log", s.routeDriver("session_log", s.handleSessionLog))
	mux.Handle("POST /macro/record/start", s.routeDriver("macro_record_start", s.handleMacroRecordStart))
	mux.Handle("POST /macro/record/stop", s.routeDriver("macro_record_stop", s.handleMacroRecordStop))
	// Approval used to live here. It moved to the human-only admin band (a
	// separate listener on -admin-addr): an approve endpoint the agent could
	// call would make the approval stamp meaningless. 410 tells old clients
	// the authority is gone by design, not temporarily.
	mux.Handle("POST /macro/approve", s.routeDriver("macro_approve", s.handleAgentApproveGone))
	mux.Handle("POST /macro/put", s.routeDriver("macro_put", s.handleMacroPut))
	mux.Handle("POST /macro/edit", s.routeDriver("macro_edit", s.handleMacroEdit))
	mux.Handle("POST /macro/replay", s.routeDriver("macro_replay", s.handleMacroReplay))
	mux.Handle("POST /macro/resume", s.routeDriver("macro_resume", s.handleMacroResume))
	mux.Handle("GET /macros", s.routeDriver("macros", s.handleMacroList))
	mux.Handle("GET /macro", s.routeDriver("macro", s.handleMacroGet))
	mux.Handle("GET /metrics", s.route("metrics", s.cfg.MetricsAuth, promhttp.HandlerFor(
		s.met.Registry, promhttp.HandlerOpts{Registry: s.met.Registry},
	).ServeHTTP))

	if s.cfg.Debug {
		mux.Handle("GET /debug", s.route("debug", true, s.handleDebug))
		mux.Handle("GET /debug/{$}", s.route("debug", true, s.handleDebug))
		for path, h := range map[string]http.HandlerFunc{
			"/debug/pprof/":        pprof.Index,
			"/debug/pprof/cmdline": pprof.Cmdline,
			"/debug/pprof/profile": pprof.Profile,
			"/debug/pprof/symbol":  pprof.Symbol,
			"/debug/pprof/trace":   pprof.Trace,
		} {
			mux.Handle("GET "+path, s.route("pprof", true, h))
		}
	}

	mux.Handle("/", s.route("notfound", false, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, errorBody{Error: "no such route", Code: "not_found"})
	}))

	return mux
}

// routeDriver wraps a handler that requires the driver token class (driver
// or full; readers and anonymous callers are rejected).
func (s *Server) routeDriver(name string, h http.HandlerFunc) http.Handler {
	return s.routeClass(name, classDriver, h)
}

// route wraps a handler with request-id, logging, metrics and optional auth.
func (s *Server) route(name string, needAuth bool, h http.HandlerFunc) http.Handler {
	required := classNone
	if needAuth {
		required = classFull
		if name == "fetch" {
			required = classReader
		}
	}
	return s.routeClass(name, required, h)
}

func (s *Server) routeClass(name string, required tokenClass, h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		id := logx.NextRequestID()
		log := s.log.With("req", id, "route", name, "method", r.Method, "path", r.URL.Path,
			"remote", r.RemoteAddr)
		ctx := logx.With(r.Context(), log)
		r = r.WithContext(ctx)

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		w.Header().Set("X-Request-Id", id)

		if s.tokenClass(r) < required {
			log.Warn("unauthorized request")
			writeJSON(rec, http.StatusUnauthorized, errorBody{Error: "missing, invalid or insufficient bearer token", Code: "unauthorized"})
		} else {
			if name == "cdp" && strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
				log.Info("CDP WebSocket attempt") // connection-level evidence, no frame/body logging
			}
			h(rec, r)
		}

		s.met.HTTPTotal.With(prometheus.Labels{"route": name, "status": itoa(rec.status)}).Inc()
		// Every request is logged, per the gateway's operating rules.
		log.Info("request", "status", rec.status, "bytes", rec.bytes,
			"duration", time.Since(started).Round(time.Millisecond))
	})
}

// tokenClass identifies which bearer token the caller presented.
type tokenClass int

const (
	classNone   tokenClass = iota
	classReader            // ReaderToken: /fetch only, never sessions
	classDriver            // DriverToken: sessions + fetch
	classFull              // Token: everything
)

func (s *Server) tokenClass(r *http.Request) tokenClass {
	if s.cfg.Token == "" {
		if s.cfg.AllowNoToken {
			return classFull
		}
		return classNone
	}
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) || len(h) <= len(prefix) {
		return classNone
	}
	got := h[len(prefix):]
	if subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.Token)) == 1 {
		return classFull
	}
	if s.cfg.DriverToken != "" && subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.DriverToken)) == 1 {
		return classDriver
	}
	if s.cfg.ReaderToken != "" && subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.ReaderToken)) == 1 {
		return classReader
	}
	return classNone
}

// allowHosts builds the session navigate tripwire from the allowlist. Exact
// hostnames and .domain wildcards, case-insensitive; an empty list allows
// whatever the URL guard already allows.
func allowHosts(list []string) func(string) error {
	if len(list) == 0 {
		return nil
	}
	allowed := make([]string, len(list))
	copy(allowed, list)
	return func(rawURL string) error {
		host := strings.ToLower(rawURL)
		if u, err := neturl.Parse(rawURL); err == nil && u.Hostname() != "" {
			host = strings.ToLower(u.Hostname())
		}
		for _, entry := range allowed {
			if strings.HasPrefix(entry, ".") {
				if host == strings.TrimPrefix(entry, ".") || strings.HasSuffix(host, entry) {
					return nil
				}
				continue
			}
			if host == entry {
				return nil
			}
		}
		return fmt.Errorf("host %q is not on the session allowlist", host)
	}
}

// browserSessionTab adapts a leased pool tab to the session.Tab interface.
type browserSessionTab struct {
	t *browser.SessionTab
}

func (b browserSessionTab) Ctx() context.Context { return b.t.Ctx() }

func (b browserSessionTab) Read(ctx context.Context) (session.Snapshot, error) {
	snap, err := b.t.Read(ctx)
	if err != nil {
		return session.Snapshot{}, err
	}
	return session.Snapshot{URL: snap.URL, Title: snap.Title, HTML: snap.HTML}, nil
}

// StartRecorder forwards the gateway-authored capture script into the leased
// tab. Part of session.RecorderStarter.
func (b browserSessionTab) StartRecorder(ctx context.Context, script, binding string) (<-chan string, error) {
	return b.t.StartRecorder(ctx, script, binding)
}

// sessionsReaper closes sessions idle past their TTL so pooled tabs return.
func (s *Server) sessionsReaper() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for range t.C {
		s.sessions.Reap()
	}
}

// Serve runs the gateway's listeners until the context is cancelled: the
// main agent-facing API, plus — when configured — the human-only admin band
// on its own address. The admin listener shares the main shutdown.
func Serve(cfg config.Config, log *slog.Logger, mgr *browser.Manager) error {
	s := New(cfg, log, mgr)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	build := func(addr string, h http.Handler) *http.Server {
		return &http.Server{
			Addr:              addr,
			Handler:           h,
			ReadHeaderTimeout: 10 * time.Second,
			// Long enough for assist mode, where a human clicks a challenge.
			WriteTimeout: cfg.RequestTimeout + cfg.AssistTimeout + 30*time.Second,
			IdleTimeout:  90 * time.Second,
		}
	}

	type listener struct {
		srv  *http.Server
		name string
	}
	listeners := []listener{{build(cfg.Addr, s.Handler()), "gateway"}}
	if cfg.AdminAddr != "" && s.admin != nil {
		listeners = append(listeners, listener{build(cfg.AdminAddr, s.admin.Handler()), "admin"})
		log.Info("admin band listening", "addr", cfg.AdminAddr,
			"note", "human-only macro approve/revoke/edit/delete")
	}

	errc := make(chan error, len(listeners))
	for _, l := range listeners {
		go func(l listener) {
			log.Info("listening", "addr", l.srv.Addr, "band", l.name)
			if err := l.srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}(l)
	}

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, l := range listeners {
		_ = l.srv.Shutdown(shutCtx)
	}
	return nil
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	_ = s.mgr.Probe(ctx)
	h := s.mgr.Health()
	code := http.StatusOK
	if !h.Connected {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, map[string]any{
		"ok":       h.Connected,
		"uptime_s": int64(time.Since(s.start).Seconds()),
		"chrome":   h,
	})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"uptime_s":  int64(time.Since(s.start).Seconds()),
		"scheduler": s.sched.Snapshot(),
		"chrome":    s.mgr.Health(),
		"requests":  s.ring.Len(),
	})
}

type errorBody struct {
	Error     string `json:"error"`
	Code      string `json:"code"`
	Vendor    string `json:"vendor,omitempty"`
	Status    int    `json:"status,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := http.NewResponseController(r.ResponseWriter).Hijack()
	if err == nil {
		r.status = http.StatusSwitchingProtocols
	}
	return conn, rw, err
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
