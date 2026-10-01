// L1 session HTTP surface: open a session (lease a tab), run typed actions
// against it, close it. See docs/l1-design.md for the constraints: no
// caller-supplied JavaScript, scheduler-paced, evidence-logged, and credential
// typing gated on a fresh passing assertion.
package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/slim-bean/browser-fetch/internal/logx"
	"github.com/slim-bean/browser-fetch/internal/session"
)

// openRequest names the host the session will work against. The host is not
// enforced as a navigation guard by itself — navigate uses the URL guard — but
// it labels the session for the evidence log.
type openRequest struct {
	Host string `json:"host"`
}

func (s *Server) handleSessionOpen(w http.ResponseWriter, r *http.Request) {
	log := logx.From(r.Context())
	var req openRequest
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: err.Error(), Code: "bad_request"})
		return
	}
	if req.Host == "" {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "body field \"host\" is required", Code: "bad_request"})
		return
	}
	sess, err := s.sessions.Open(r.Context(), req.Host)
	if err != nil {
		log.Warn("session open failed", "err", err)
		writeJSON(w, http.StatusTooManyRequests, errorBody{Error: err.Error(), Code: "session_unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": sess.ID,
		"host":       sess.Host,
		"open_at":    sess.OpenAt,
	})
}

// actionRequest wraps one action. The action is a typed union decoded by kind.
type actionRequest struct {
	SessionID string          `json:"session_id"`
	Action    json.RawMessage `json:"action"`
}

type actionEnvelope struct {
	Kind string `json:"kind"`
}

func (s *Server) handleSessionAction(w http.ResponseWriter, r *http.Request) {
	log := logx.From(r.Context())
	var req actionRequest
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: err.Error(), Code: "bad_request"})
		return
	}
	sess, err := s.sessions.Get(req.SessionID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errorBody{Error: err.Error(), Code: "no_session"})
		return
	}

	action, err := decodeAction(req.Action)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: err.Error(), Code: "bad_request"})
		return
	}

	res, err := s.sessions.Run(r.Context(), sess, action)
	outcome := "ok"
	httpStatus := http.StatusOK
	if err != nil {
		outcome = "failed"
		httpStatus = http.StatusUnprocessableEntity
	}
	s.met.ActionsTotal.With(prometheus.Labels{"action": action.Kind(), "outcome": outcome}).Inc()

	body := map[string]any{
		"session_id": sess.ID,
		"action":     action.Kind(),
		"ok":         err == nil,
	}
	if err != nil {
		body["error"] = err.Error()
	} else {
		body["result"] = res
	}
	// Screenshot results can be large; evidence log holds the hash, not bytes.
	if shot, ok := res.(session.ScreenshotResult); ok {
		body["evidence"] = map[string]any{"sha256": shot.SHA256, "bytes": shot.Bytes}
	}
	writeJSON(w, httpStatus, body)
	_ = log
}

func (s *Server) handleSessionClose(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"session_id"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: err.Error(), Code: "bad_request"})
		return
	}
	if err := s.sessions.Close(req.SessionID); err != nil {
		writeJSON(w, http.StatusNotFound, errorBody{Error: err.Error(), Code: "no_session"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"closed": req.SessionID})
}

func (s *Server) handleSessionList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"sessions": s.sessions.Info()})
}

func (s *Server) handleSessionLog(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("session_id")
	sess, err := s.sessions.Get(id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errorBody{Error: err.Error(), Code: "no_session"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": sess.ID,
		"records":    sess.Records(),
	})
}

// decodeAction decodes the typed action union by its "kind" field. The kind
// is the envelope; the remaining fields are strict-decoded into the typed
// struct (unknown fields rejected).
//
// Only read-only kinds are accepted here. click and type are interaction
// primitives reserved for the macro recorder/replayer: the agent-facing API
// never accepts them, so a prompt-injected or compromised agent cannot operate
// the site. The runner calls session dispatch directly.
func decodeAction(raw json.RawMessage) (session.Action, error) {
	action, err := decodeAnyAction(raw)
	if err != nil {
		return nil, err
	}
	switch action.(type) {
	case session.ClickAction, session.TypeAction:
		return nil, errors.New("action kind is not allowed over the agent API; interaction is recorded by a human and replayed as macros")
	}
	return action, nil
}

// decodeAnyAction decodes any action kind, including the interaction
// primitives. Internal callers (the macro runner) use this.
func decodeAnyAction(raw json.RawMessage) (session.Action, error) {
	if len(raw) == 0 {
		return nil, errors.New("body field \"action\" is required")
	}
	var env actionEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, errors.New("invalid action: " + err.Error())
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, errors.New("invalid action: " + err.Error())
	}
	delete(fields, "kind")
	body, err := json.Marshal(fields)
	if err != nil {
		return nil, errors.New("invalid action: " + err.Error())
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	switch env.Kind {
	case "navigate":
		var a session.NavigateAction
		err := dec.Decode(&a)
		return a, wrapDecode(err)
	case "click":
		var a session.ClickAction
		err := dec.Decode(&a)
		return a, wrapDecode(err)
	case "type":
		var a session.TypeAction
		err := dec.Decode(&a)
		return a, wrapDecode(err)
	case "wait":
		var a session.WaitAction
		err := dec.Decode(&a)
		return a, wrapDecode(err)
	case "screenshot":
		var a session.ScreenshotAction
		err := dec.Decode(&a)
		return a, wrapDecode(err)
	case "content":
		var a session.ContentAction
		err := dec.Decode(&a)
		return a, wrapDecode(err)
	case "fields":
		var a session.FieldsAction
		err := dec.Decode(&a)
		return a, wrapDecode(err)
	case "assert":
		var a session.AssertAction
		err := dec.Decode(&a)
		return a, wrapDecode(err)
	case "download":
		var a session.DownloadAction
		err := dec.Decode(&a)
		return a, wrapDecode(err)
	default:
		return nil, errors.New("unknown action kind " + env.Kind)
	}
}

func wrapDecode(err error) error {
	if err != nil {
		return errors.New("invalid action body: " + err.Error())
	}
	return nil
}

// bytesReader is no longer needed; decodeAction re-marshals the field map.

// decodeBody decodes a JSON request body strictly.
func decodeBody(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}
