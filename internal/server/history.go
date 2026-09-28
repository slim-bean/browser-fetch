package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os/exec"
	"time"
)

const historyOutputLimit = 2 * 1024 * 1024

type limitedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.limit {
		return 0, errors.New("helper output exceeded limit")
	}
	return b.Buffer.Write(p)
}

// One short-lived pi-browser CLI process per query: reuse its SQLite snapshots,
// parser and ranking without another network listener or a second implementation.
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	if len(s.cfg.HistoryCommand) == 0 {
		writeJSON(w, http.StatusNotImplemented, errorBody{Code: "history_disabled", Error: "History helper is not configured"})
		return
	}
	select {
	case s.historySlots <- struct{}{}:
		defer func() { <-s.historySlots }()
	default:
		writeJSON(w, http.StatusTooManyRequests, errorBody{Code: "history_busy", Error: "History is busy; retry shortly"})
		return
	}
	operation := "sources"
	params := json.RawMessage(`{}`)
	if r.Method == http.MethodPost {
		operation = "search"
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64*1024))
		if err != nil || !json.Valid(body) {
			writeJSON(w, http.StatusBadRequest, errorBody{Code: "bad_request", Error: "Expected a JSON history query (maximum 64 KiB)"})
			return
		}
		params = body
	}
	input, _ := json.Marshal(map[string]any{"operation": operation, "params": params})
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	command := s.cfg.HistoryCommand
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.WaitDelay = time.Second
	cmd.Stdin = bytes.NewReader(input)
	output := &limitedBuffer{limit: historyOutputLimit}
	stderr := &limitedBuffer{limit: 16 * 1024}
	cmd.Stdout, cmd.Stderr = output, stderr
	if err := cmd.Run(); err != nil {
		// Do not log query/output, which may contain private URLs. The error identifies
		// a missing helper, timeout, or exit code without echoing its stderr/body.
		s.log.Warn("history helper failed", "error", err.Error())
		writeJSON(w, http.StatusBadGateway, errorBody{Code: "history_unavailable", Error: "History helper failed or timed out; check its installation/configuration"})
		return
	}
	var result struct {
		Version int    `json:"version"`
		Error   string `json:"error"`
		Code    string `json:"code"`
	}
	if json.Unmarshal(output.Bytes(), &result) != nil || result.Version != 1 {
		writeJSON(w, http.StatusBadGateway, errorBody{Code: "history_unavailable", Error: "History helper returned an incompatible response"})
		return
	}
	status := http.StatusOK
	if result.Error != "" {
		status = http.StatusServiceUnavailable
		if result.Code == "bad_request" {
			status = http.StatusBadRequest
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(output.Bytes())
}
