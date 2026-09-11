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
	"context"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/slim-bean/browser-fetch/internal/browser"
	"github.com/slim-bean/browser-fetch/internal/config"
	"github.com/slim-bean/browser-fetch/internal/logx"
	"github.com/slim-bean/browser-fetch/internal/metrics"
	"github.com/slim-bean/browser-fetch/internal/reqlog"
	"github.com/slim-bean/browser-fetch/internal/scheduler"
	"github.com/slim-bean/browser-fetch/internal/urlguard"
)

type Server struct {
	cfg   config.Config
	log   *slog.Logger
	mgr   *browser.Manager
	sched *scheduler.Scheduler[*browser.Result]
	guard *urlguard.Guard
	ring  *reqlog.Ring
	met   *metrics.Metrics
	start time.Time
}

func New(cfg config.Config, log *slog.Logger, mgr *browser.Manager) *Server {
	s := &Server{
		cfg:   cfg,
		log:   log,
		mgr:   mgr,
		guard: urlguard.New(cfg.AllowPrivate),
		ring:  reqlog.NewRing(cfg.DebugRing),
		start: time.Now(),
		sched: scheduler.New[*browser.Result](scheduler.Options{
			MaxSlots: cfg.MaxTabs,
			HostGap:  cfg.HostGap,
			Jitter:   cfg.HostJitter,
		}),
	}
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

// route wraps a handler with request-id, logging, metrics and optional auth.
func (s *Server) route(name string, needAuth bool, h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		id := logx.NextRequestID()
		log := s.log.With("req", id, "route", name, "method", r.Method, "path", r.URL.Path,
			"remote", r.RemoteAddr)
		ctx := logx.With(r.Context(), log)
		r = r.WithContext(ctx)

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		w.Header().Set("X-Request-Id", id)

		if needAuth && !s.authorized(r) {
			log.Warn("unauthorized request")
			writeJSON(rec, http.StatusUnauthorized, errorBody{Error: "missing or invalid bearer token", Code: "unauthorized"})
		} else {
			h(rec, r)
		}

		s.met.HTTPTotal.With(prometheus.Labels{"route": name, "status": itoa(rec.status)}).Inc()
		// Every request is logged, per the gateway's operating rules.
		log.Info("request", "status", rec.status, "bytes", rec.bytes,
			"duration", time.Since(started).Round(time.Millisecond))
	})
}

func (s *Server) authorized(r *http.Request) bool {
	if s.cfg.Token == "" {
		return s.cfg.AllowNoToken
	}
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(prefix) {
		return false
	}
	got, want := h[len(prefix):], s.cfg.Token
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
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
