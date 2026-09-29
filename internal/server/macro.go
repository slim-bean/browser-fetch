// Macro HTTP surface: record a human's interaction into a macro draft, review
// and approve it, replay approved macros. See docs/macros.md for the model.
//
// Trust invariants enforced here:
//   - Recording installs the gateway-authored capture script only; it reports,
//     never mutates, and never sees secret values.
//   - Replay runs only approved macros, through the same session machinery as
//     everything else (allow-hosts, evidence log, scheduler pacing).
//   - Replay of type steps needs a secret resolver (Phase 2); macros that
//     resolve nothing can replay today.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/slim-bean/browser-fetch/internal/logx"
	"github.com/slim-bean/browser-fetch/internal/macro"
	"github.com/slim-bean/browser-fetch/internal/session"
)

// macroState carries the server's macro subsystem: the on-disk store plus the
// live recordings and the human-resume registry for pause steps.
type macroState struct {
	store *macro.Store

	mu         sync.Mutex
	recordings map[string]*recording // keyed by macro id
	resumes    map[string]chan struct{}
}

type recording struct {
	macroID   string
	sessionID string
	rec       *macro.Recorder
	cancel    context.CancelFunc
}

func newMacroState(dir string) (*macroState, error) {
	if dir == "" {
		return nil, nil
	}
	store, err := macro.NewStore(dir)
	if err != nil {
		return nil, err
	}
	return &macroState{
		store:      store,
		recordings: map[string]*recording{},
		resumes:    map[string]chan struct{}{},
	}, nil
}

func (s *Server) requireMacros(w http.ResponseWriter) bool {
	if s.macros == nil {
		writeJSON(w, http.StatusNotImplemented, errorBody{
			Code:  "macros_disabled",
			Error: "macro store is not configured (-macro-store)",
		})
		return false
	}
	return true
}

// ---- recording ---------------------------------------------------------------

type recordStartRequest struct {
	MacroID string `json:"macro_id"`
	Site    string `json:"site"`
	// SessionID optionally records into an existing session; otherwise a new
	// session is opened for the site.
	SessionID string `json:"session_id,omitempty"`
}

func (s *Server) handleMacroRecordStart(w http.ResponseWriter, r *http.Request) {
	log := logx.From(r.Context())
	if !s.requireMacros(w) {
		return
	}
	var req recordStartRequest
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: err.Error(), Code: "bad_request"})
		return
	}
	if req.MacroID == "" || req.Site == "" {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "macro_id and site are required", Code: "bad_request"})
		return
	}

	s.macros.mu.Lock()
	if _, live := s.macros.recordings[req.MacroID]; live {
		s.macros.mu.Unlock()
		writeJSON(w, http.StatusConflict, errorBody{Error: "a recording with this macro_id is already running", Code: "recording_active"})
		return
	}
	s.macros.mu.Unlock()

	// Lease the session the human will operate.
	var sess *session.Session
	if req.SessionID != "" {
		var err error
		sess, err = s.sessions.Get(req.SessionID)
		if err != nil {
			writeJSON(w, http.StatusNotFound, errorBody{Error: "session not found: " + err.Error(), Code: "not_found"})
			return
		}
	} else {
		var err error
		sess, err = s.sessions.Open(r.Context(), req.Site)
		if err != nil {
			writeJSON(w, http.StatusTooManyRequests, errorBody{Error: err.Error(), Code: "session_unavailable"})
			return
		}
	}

	// Install on a context that outlives this HTTP request: the binding and
	// its ListenTarget subscription stay active for the whole recording
	// session, not just this handler's lifetime. The recorder's own
	// installTimeout bounds the install; install failure is reported here.
	events, err := sess.StartRecording(context.Background(), macro.CaptureScript, macro.BindingName)
	if err != nil {
		if req.SessionID == "" {
			_ = s.sessions.Close(sess.ID)
		}
		log.Warn("recorder install failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: err.Error(), Code: "recorder_failed"})
		return
	}

	sess.SetHold(true) // a human is driving; do not idle-reap
	rec := macro.NewRecorder(req.MacroID, req.Site)
	// The event drain must outlive the record/start HTTP request: the human
	// clicks in a VNC browser long after this handler returns, so a context
	// derived from r.Context() would cancel the drain immediately and every
	// macro would stop with "no steps".
	ctx, cancel := context.WithCancel(context.Background())
	live := &recording{macroID: req.MacroID, sessionID: sess.ID, rec: rec, cancel: cancel}

	// Drain capture events into the recorder for the recording's lifetime.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case payload, ok := <-events:
				if !ok {
					return
				}
				if err := rec.AddEvent([]byte(payload)); err != nil {
					log.Warn("bad recorder event", "err", err)
				}
			}
		}
	}()

	s.macros.mu.Lock()
	s.macros.recordings[req.MacroID] = live
	s.macros.mu.Unlock()

	log.Info("macro recording started", "macro", req.MacroID, "session", sess.ID)
	writeJSON(w, http.StatusOK, map[string]any{
		"macro_id": req.MacroID, "session_id": sess.ID,
		"note": "operate the browser now; POST /macro/record/stop when done",
	})
}

type recordStopRequest struct {
	MacroID     string `json:"macro_id"`
	Description string `json:"description,omitempty"`
	// CloseSession closes the leased session too (default true).
	CloseSession *bool `json:"close_session,omitempty"`
}

func (s *Server) handleMacroRecordStop(w http.ResponseWriter, r *http.Request) {
	log := logx.From(r.Context())
	if !s.requireMacros(w) {
		return
	}
	var req recordStopRequest
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: err.Error(), Code: "bad_request"})
		return
	}
	s.macros.mu.Lock()
	live, ok := s.macros.recordings[req.MacroID]
	if ok {
		delete(s.macros.recordings, req.MacroID)
	}
	s.macros.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusNotFound, errorBody{Error: "no live recording with this macro_id", Code: "not_found"})
		return
	}
	live.cancel()

	draft := live.rec.Draft()
	draft.Description = req.Description
	if err := s.macros.store.Put(draft, false); err != nil {
		log.Warn("macro draft store failed", "err", err, "macro", req.MacroID, "steps", len(draft.Steps))
		// The draft must not be lost: the human's clicks are irreplaceable.
		// Park it as <id>.orphan.json (0600) so the admin band can recover it.
		if orphanErr := s.macros.store.PutOrphan(draft); orphanErr == nil {
			log.Info("draft parked for admin recovery", "macro", req.MacroID)
		}
		writeJSON(w, http.StatusInternalServerError, errorBody{
			Error: err.Error() + " (draft parked in the macro store for admin recovery)",
			Code:  "macro_store_failed",
		})
		return
	}
	if req.CloseSession == nil || *req.CloseSession {
		s.sessions.SetHold(live.sessionID, false)
		_ = s.sessions.Close(live.sessionID)
	} else {
		s.sessions.SetHold(live.sessionID, false)
	}
	log.Info("macro recording stopped", "macro", req.MacroID, "steps", len(draft.Steps))
	writeJSON(w, http.StatusOK, map[string]any{
		"macro_id": draft.ID,
		"steps":    len(draft.Steps),
		"approved": false,
		"note":     "draft stored unapproved; review then POST /macro/approve",
	})
}

// ---- review / approve ----------------------------------------------------------

func (s *Server) handleMacroList(w http.ResponseWriter, r *http.Request) {
	if !s.requireMacros(w) {
		return
	}
	ids, err := s.macros.store.List()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: err.Error(), Code: "macro_store_failed"})
		return
	}
	type entry struct {
		ID       string          `json:"id"`
		Approved bool            `json:"approved"`
		Macro    json.RawMessage `json:"-"`
	}
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		m, err := s.macros.store.Get(id)
		if err != nil {
			continue
		}
		out = append(out, map[string]any{
			"id": m.ID, "site": m.Site, "approved": m.Approved != nil,
			"steps": len(m.Steps), "created": m.Created,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"macros": out})
}

func (s *Server) handleMacroGet(w http.ResponseWriter, r *http.Request) {
	if !s.requireMacros(w) {
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "query parameter \"id\" is required", Code: "bad_request"})
		return
	}
	m, err := s.macros.store.Get(id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errorBody{Error: err.Error(), Code: "not_found"})
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// handleAgentApproveGone serves the old approval route. Approval moved to
// the human-only admin band (internal/server/admin.go): an approve endpoint
// on the agent-facing surface would make the approval stamp meaningless —
// the agent could grant itself authority.
func (s *Server) handleAgentApproveGone(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusGone, errorBody{
		Error: "macro approval moved to the human-only admin band (-admin-addr); the agent cannot approve",
		Code:  "approval_requires_admin_band",
	})
}

// ---- replay ---------------------------------------------------------------------
// Approval used to be handled here (POST /macro/approve with {macro_id, by}).
// It moved to the human-only admin band (internal/server/admin.go): an
// approve endpoint on the agent-facing surface would make the approval stamp
// meaningless — the agent could grant itself authority. The route now serves
// 410 (see server.go).

type replayRequest struct {
	MacroID   string `json:"macro_id"`
	SessionID string `json:"session_id,omitempty"`
	// Site opens a fresh session for this host when session_id is empty.
	Site string `json:"site,omitempty"`
}

func (s *Server) handleMacroReplay(w http.ResponseWriter, r *http.Request) {
	log := logx.From(r.Context())
	if !s.requireMacros(w) {
		return
	}
	var req replayRequest
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: err.Error(), Code: "bad_request"})
		return
	}
	m, err := s.macros.store.Get(req.MacroID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errorBody{Error: err.Error(), Code: "not_found"})
		return
	}
	if m.Approved == nil {
		writeJSON(w, http.StatusForbidden, errorBody{Error: "macro is not approved; a human must review the recording first", Code: "unapproved_macro"})
		return
	}

	sess, err := s.openReplaySession(r, req, m)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: err.Error(), Code: "session_unavailable"})
		return
	}
	defer func() { _ = s.sessions.Close(sess.ID) }()

	re := &macro.Replayer{
		Secrets:            s.secrets,
		Pauser:             s.macros,
		MinTextLenForXPath: 3,
	}
	started := time.Now()
	runErr := re.Run(r.Context(), s.sessions, sess, m)
	defer func() {
		aborted, cause := -1, ""
		var reErr *macro.ReplayError
		if errors.As(runErr, &reErr) {
			aborted, cause = reErr.Step, reErr.Cause
		} else if runErr != nil {
			cause = runErr.Error()
		}
		s.admin.RecordReplay(m.ID, runErr == nil, time.Since(started), aborted, cause)
	}()

	result := map[string]any{
		"macro_id":    m.ID,
		"session_id":  sess.ID,
		"duration_ms": time.Since(started).Milliseconds(),
	}
	if runErr != nil {
		var reErr *macro.ReplayError
		if errors.As(runErr, &reErr) {
			result["aborted_at_step"] = reErr.Step
			result["step_kind"] = reErr.Kind
			result["cause"] = reErr.Cause
			result["drift"] = reErr.Drift
		} else {
			result["cause"] = runErr.Error()
		}
		result["evidence"] = evidenceView(sess)
		log.Warn("macro replay aborted", "macro", m.ID, "err", runErr)
		writeJSON(w, http.StatusConflict, result)
		return
	}
	result["ok"] = true
	result["steps"] = len(m.Steps)
	result["evidence"] = evidenceView(sess)
	log.Info("macro replay complete", "macro", m.ID)
	writeJSON(w, http.StatusOK, result)
}

func evidenceView(sess *session.Session) []session.ActionRecord {
	return sess.Records()
}

func (s *Server) openReplaySession(r *http.Request, req replayRequest, m *macro.Macro) (*session.Session, error) {
	if req.SessionID != "" {
		return s.sessions.Get(req.SessionID)
	}
	host := req.Site
	if host == "" {
		host = m.Site
	}
	return s.sessions.Open(r.Context(), host)
}

// ---- pause/resume ------------------------------------------------------------------

// WaitResume implements macro.Pauser: blocks until POST /macro/resume signals
// the macro id, or the context lapses.
func (ms *macroState) WaitResume(ctx context.Context, macroID, reason string, timeout time.Duration) error {
	ch := make(chan struct{})
	ms.mu.Lock()
	ms.resumes[macroID] = ch
	ms.mu.Unlock()
	defer func() {
		ms.mu.Lock()
		delete(ms.resumes, macroID)
		ms.mu.Unlock()
	}()
	_ = reason // carried in the macro; surfaced by the notifier
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type resumeRequest struct {
	MacroID string `json:"macro_id"`
}

func (s *Server) handleMacroResume(w http.ResponseWriter, r *http.Request) {
	log := logx.From(r.Context())
	if !s.requireMacros(w) {
		return
	}
	var req resumeRequest
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: err.Error(), Code: "bad_request"})
		return
	}
	s.macros.mu.Lock()
	ch, ok := s.macros.resumes[req.MacroID]
	s.macros.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusNotFound, errorBody{Error: "no paused replay waiting for this macro_id", Code: "not_found"})
		return
	}
	close(ch)
	log.Info("macro replay resumed by human", "macro", req.MacroID)
	writeJSON(w, http.StatusOK, map[string]any{"resumed": req.MacroID})
}
