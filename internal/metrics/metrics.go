// Package metrics exposes Prometheus instrumentation for the gateway.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Outcome labels used across metrics and /debug.
const (
	OutcomeOK         = "ok"
	OutcomeChallenge  = "challenge"
	OutcomeNavError   = "nav_error"
	OutcomeRejected   = "rejected_url"
	OutcomeTimeout    = "timeout"
	OutcomeBadRequest = "bad_request"
	OutcomeUnavail    = "chrome_unavailable"
)

type Metrics struct {
	FetchTotal    *prometheus.CounterVec
	FetchDuration *prometheus.HistogramVec
	QueueWait     prometheus.Histogram
	HTMLBytes     prometheus.Histogram
	Deduped       prometheus.Counter
	HTTPTotal     *prometheus.CounterVec

	Registry *prometheus.Registry
}

// Gauges the caller supplies as live values.
type Sources struct {
	TabsBusy       func() float64
	TabsIdle       func() float64
	TabsCreated    func() float64
	QueueDepth     func() float64
	Running        func() float64
	AssistsPending func() float64
	Connected      func() float64
}

func New(src Sources) *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		Registry: reg,
		FetchTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "browser_fetch_fetch_total",
			Help: "Fetch attempts by outcome.",
		}, []string{"outcome", "host"}),
		FetchDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "browser_fetch_fetch_duration_seconds",
			Help:    "End-to-end fetch duration including queue wait.",
			Buckets: []float64{0.25, 0.5, 1, 2, 4, 8, 15, 30, 60},
		}, []string{"outcome"}),
		QueueWait: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "browser_fetch_queue_wait_seconds",
			Help:    "Time spent waiting for a host slot and a tab.",
			Buckets: []float64{0.05, 0.25, 0.5, 1, 2, 4, 8, 15, 30},
		}),
		HTMLBytes: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "browser_fetch_html_bytes",
			Help:    "Size of returned rendered HTML.",
			Buckets: prometheus.ExponentialBuckets(1024, 4, 8),
		}),
		Deduped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "browser_fetch_deduped_total",
			Help: "Requests served by coalescing onto an in-flight identical fetch.",
		}),
		HTTPTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "browser_fetch_http_requests_total",
			Help: "HTTP requests to the gateway by route and status.",
		}, []string{"route", "status"}),
	}

	reg.MustRegister(m.FetchTotal, m.FetchDuration, m.QueueWait, m.HTMLBytes, m.Deduped, m.HTTPTotal)

	gauge := func(name, help string, fn func() float64) {
		if fn == nil {
			return
		}
		reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: name, Help: help}, fn))
	}
	gauge("browser_fetch_tabs_busy", "Tabs currently navigating.", src.TabsBusy)
	gauge("browser_fetch_tabs_idle", "Tabs parked and available.", src.TabsIdle)
	gauge("browser_fetch_tabs_created", "Tabs currently open.", src.TabsCreated)
	gauge("browser_fetch_queue_depth", "Requests waiting for a host slot or tab.", src.QueueDepth)
	gauge("browser_fetch_running", "Fetches executing now.", src.Running)
	gauge("browser_fetch_assists_pending", "Challenges waiting for a human click.", src.AssistsPending)
	gauge("browser_fetch_chrome_connected", "1 when the DevTools endpoint answered the last probe.", src.Connected)

	return m
}
