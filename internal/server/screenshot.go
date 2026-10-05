package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/slim-bean/browser-fetch/internal/screenshot"
)

// Each configured credential class has its own namespace. Even root cannot
// retrieve a reader's capture by ID. Anonymous mode is one operator-owned scope.
func (s *Server) screenshotOwner(r *http.Request) string {
	return fmt.Sprintf("class:%d", s.tokenClass(r))
}

func (s *Server) handleScreenshot(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CaptureID string `json:"capture_id"`
		Segment   int    `json:"segment"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Code: "bad_request", Error: "invalid screenshot request"})
		return
	}
	if err := dec.Decode(new(any)); err != io.EOF || req.CaptureID == "" || len(req.CaptureID) > 128 || req.Segment < 1 {
		writeJSON(w, http.StatusBadRequest, errorBody{Code: "bad_request", Error: "capture_id and a positive segment number are required"})
		return
	}
	shot, err := s.screenshots.Get(s.screenshotOwner(r), req.CaptureID, req.Segment)
	if err != nil {
		status, code := http.StatusGone, "screenshot_unavailable"
		if errors.Is(err, screenshot.ErrSegment) {
			status, code = http.StatusBadRequest, "bad_request"
		}
		writeJSON(w, status, errorBody{Code: code, Error: err.Error()})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, shot)
}
