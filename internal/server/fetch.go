package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/slim-bean/browser-fetch/internal/browser"
	"github.com/slim-bean/browser-fetch/internal/logx"
	"github.com/slim-bean/browser-fetch/internal/metrics"
	"github.com/slim-bean/browser-fetch/internal/reqlog"
)

type fetchRequest struct {
	URL       string `json:"url"`
	TimeoutMS int    `json:"timeout_ms,omitempty"`
	AssistMS  *int   `json:"assist_ms,omitempty"`
}

type fetchResponse struct {
	URL         string `json:"url"`
	Title       string `json:"title"`
	HTML        string `json:"html"`
	Status      int    `json:"status"`
	PageID      int    `json:"page_id"`
	AssistedMS  int64  `json:"assisted_ms,omitempty"`
	RequestID   string `json:"request_id"`
	DurationMS  int64  `json:"duration_ms"`
	QueueWaitMS int64  `json:"queue_wait_ms"`
	Deduped     bool   `json:"deduped"`
}

func (s *Server) handleFetch(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	log := logx.From(r.Context())
	reqID := w.Header().Get("X-Request-Id")

	req, err := parseFetchRequest(r)
	if err != nil {
		s.finishFetch(w, log, reqlog.Entry{
			ID: reqID, At: started, Outcome: metrics.OutcomeBadRequest,
			Error: err.Error(), RemoteAddr: r.RemoteAddr,
		}, started, http.StatusBadRequest, errorBody{
			Error: err.Error(), Code: metrics.OutcomeBadRequest, RequestID: reqID,
		})
		return
	}

	// Validate and resolve before a browser ever sees the URL.
	u, err := s.guard.Check(r.Context(), req.URL)
	if err != nil {
		log.Warn("rejected target", "url", req.URL, "err", err)
		s.finishFetch(w, log, reqlog.Entry{
			ID: reqID, At: started, URL: req.URL, Outcome: metrics.OutcomeRejected,
			Error: err.Error(), RemoteAddr: r.RemoteAddr,
		}, started, http.StatusForbidden, errorBody{
			Error: err.Error(), Code: metrics.OutcomeRejected, RequestID: reqID,
		})
		return
	}
	host := strings.ToLower(u.Hostname())

	timeout := s.cfg.RequestTimeout
	if req.TimeoutMS > 0 {
		timeout = time.Duration(req.TimeoutMS) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	var assist *time.Duration
	if req.AssistMS != nil {
		d := time.Duration(*req.AssistMS) * time.Millisecond
		assist = &d
	}

	// Queue wait is measured from handler entry to the moment the navigation
	// actually starts, so /debug shows pacing separately from page load time.
	var queueWait time.Duration
	res, shared, err := s.sched.Do(ctx, host, u.String(), func(ctx context.Context) (*browser.Result, error) {
		queueWait = time.Since(started)
		s.met.QueueWait.Observe(queueWait.Seconds())
		log.Info("navigating", "host", host, "queue_wait", queueWait.Round(time.Millisecond))
		return s.mgr.Fetch(ctx, browser.Request{URL: u.String(), Assist: assist})
	})

	entry := reqlog.Entry{
		ID: reqID, At: started, URL: u.String(), Host: host,
		QueueWaitMS: queueWait.Milliseconds(), Shared: shared, RemoteAddr: r.RemoteAddr,
	}
	if shared {
		s.met.Deduped.Inc()
	}

	if err != nil {
		code, httpStatus, vendor := classify(err, ctx)
		entry.Outcome, entry.Error, entry.Vendor = code, err.Error(), vendor
		log.Warn("fetch failed", "code", code, "err", err, "vendor", vendor)
		s.finishFetch(w, log, entry, started, httpStatus, errorBody{
			Error: err.Error(), Code: code, Vendor: vendor, RequestID: reqID,
		})
		return
	}

	entry.Outcome = metrics.OutcomeOK
	entry.FinalURL = res.URL
	entry.Status = res.Status
	entry.Bytes = len(res.HTML)
	entry.PageID = res.PageID
	entry.AssistedMS = res.AssistedMS
	s.met.HTMLBytes.Observe(float64(len(res.HTML)))

	s.finishFetch(w, log, entry, started, http.StatusOK, fetchResponse{
		URL: res.URL, Title: res.Title, HTML: res.HTML, Status: res.Status,
		PageID: res.PageID, AssistedMS: res.AssistedMS, RequestID: reqID,
		DurationMS: time.Since(started).Milliseconds(), QueueWaitMS: queueWait.Milliseconds(),
		Deduped: shared,
	})
}

// finishFetch records metrics plus the /debug ring entry and writes the body.
func (s *Server) finishFetch(
	w http.ResponseWriter,
	log *slog.Logger,
	entry reqlog.Entry,
	started time.Time,
	httpStatus int,
	body any,
) {
	dur := time.Since(started)
	entry.DurationMS = dur.Milliseconds()
	s.ring.Add(entry)
	s.met.FetchTotal.With(prometheus.Labels{"outcome": entry.Outcome, "host": entry.Host}).Inc()
	s.met.FetchDuration.With(prometheus.Labels{"outcome": entry.Outcome}).Observe(dur.Seconds())
	log.Info("fetch complete", "outcome", entry.Outcome, "http", httpStatus,
		"bytes", entry.Bytes, "target_status", entry.Status, "duration", dur.Round(time.Millisecond))
	writeJSON(w, httpStatus, body)
}

func parseFetchRequest(r *http.Request) (fetchRequest, error) {
	var req fetchRequest
	if r.Method == http.MethodGet {
		req.URL = r.URL.Query().Get("url")
		if req.URL == "" {
			return req, errors.New("missing url query parameter")
		}
		return req, nil
	}
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return req, errors.New("invalid JSON body: " + err.Error())
	}
	if strings.TrimSpace(req.URL) == "" {
		return req, errors.New("body field \"url\" is required")
	}
	return req, nil
}

// classify maps an error to (code, HTTP status, vendor).
func classify(err error, ctx context.Context) (string, int, string) {
	var chErr *browser.ChallengeError
	if errors.As(err, &chErr) {
		return metrics.OutcomeChallenge, http.StatusBadGateway, chErr.Vendor
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return metrics.OutcomeTimeout, http.StatusGatewayTimeout, ""
	}
	if errors.Is(err, context.Canceled) {
		return metrics.OutcomeTimeout, 499, ""
	}
	var navErr *browser.NavError
	if errors.As(err, &navErr) {
		return metrics.OutcomeNavError, http.StatusBadGateway, ""
	}
	return metrics.OutcomeUnavail, http.StatusServiceUnavailable, ""
}
