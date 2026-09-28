package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/slim-bean/browser-fetch/internal/history"
)

// Root-token-only, read-only data API. Ranking/query-language/presentation belong
// to the client. No subprocess, source checkout, SQL or path supplied by a caller.
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	if s.cfg.HistoryRoot == "" {
		writeJSON(w, http.StatusNotImplemented, errorBody{Code: "history_disabled", Error: "Native history root is not configured"})
		return
	}
	select {
	case s.historySlots <- struct{}{}:
		defer func() { <-s.historySlots }()
	default:
		writeJSON(w, http.StatusTooManyRequests, errorBody{Code: "history_busy", Error: "History is busy; retry shortly"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	reader := history.Reader{Root: s.cfg.HistoryRoot, Name: s.cfg.HistorySource}
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodGet {
		sources, err := reader.Sources()
		if err != nil {
			s.log.Warn("history discovery failed", "error", err)
			writeJSON(w, http.StatusServiceUnavailable, errorBody{Code: "history_unavailable", Error: "Configured history root is unavailable"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"version": history.Version, "sources": sources})
		return
	}
	var query history.Query
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&query); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Code: "bad_request", Error: "Expected a history v2 query; unknown fields, SQL and paths are not accepted"})
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		writeJSON(w, http.StatusBadRequest, errorBody{Code: "bad_request", Error: "Expected one JSON object"})
		return
	}
	if err := query.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Code: "bad_request", Error: err.Error()})
		return
	}
	result, err := reader.Query(ctx, query)
	if err != nil {
		if errors.Is(err, history.ErrSourceNotFound) {
			writeJSON(w, http.StatusNotFound, errorBody{Code: "unknown_source", Error: "Unknown history source; refresh /history/sources"})
			return
		}
		// Do not log query terms or rows. Source ids and backend errors are diagnostic
		// metadata; no request controls a filesystem path or database operation.
		s.log.Warn("history read failed", "source", query.SourceID, "error", err)
		writeJSON(w, http.StatusServiceUnavailable, errorBody{Code: "history_unavailable", Error: "History snapshot unavailable, changing, oversized or unreadable; retry or inspect server logs"})
		return
	}
	writeJSON(w, http.StatusOK, result)
}
