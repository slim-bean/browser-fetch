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

// ---- agent-authored drafts (operator-designated sites only) ---------------------
//
// When the operator marks a site agent-editable in the admin band, the agent
// may author and edit DRAFT macros for that site and test-replay them without
// approval. Approved macros stay frozen everywhere; approval still requires
// the admin band. A draft written here records its authorship.

// agentMacroPayload is the strict input schema for /macro/put.
type agentMacroPayload struct {
	ID          string          `json:"id"`
	Site        string          `json:"site"`
	Description string          `json:"description,omitempty"`
	Steps       []macro.Step    `json:"steps"`
	Step        json.RawMessage `json:"step,omitempty"` // convenience: single-step macros
}

func (s *Server) handleMacroPut(w http.ResponseWriter, r *http.Request) {
	if !s.requireMacros(w) {
		return
	}
	var req agentMacroPayload
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: err.Error(), Code: "bad_request"})
		return
	}
	if req.ID == "" || req.Site == "" {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "id and site are required", Code: "bad_request"})
		return
	}
	steps := req.Steps
	if len(steps) == 0 && len(req.Step) > 0 {
		steps = []macro.Step{{Action: req.Step}}
	}
	m := &macro.Macro{
		ID:          req.ID,
		Site:        req.Site,
		Description: req.Description,
		Created:     time.Now().UTC(),
		Steps:       steps,
	}
	if err := s.macros.store.AgentPut(m, false); err != nil {
		writeJSON(w, http.StatusForbidden, errorBody{Error: err.Error(), Code: "agent_edit_refused"})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"macro_id": m.ID, "steps": len(m.Steps), "approved": false,
		"note": "agent-authored draft; test-replayable while the site is agent-editable; approval still requires the admin band",
	})
}

// editOp is one strict, bounded mutation. Nothing here can grant approval,
// widen the site allowlist, or touch another site.
type editOp struct {
	Op     string          `json:"op"`
	Index  int             `json:"index"`
	Step   json.RawMessage `json:"step,omitempty"`
	Secret string          `json:"secret,omitempty"`
	Descr  string          `json:"description,omitempty"`
}

type editRequest struct {
	MacroID string   `json:"macro_id"`
	Ops     []editOp `json:"ops"`
}

func (s *Server) handleMacroEdit(w http.ResponseWriter, r *http.Request) {
	if !s.requireMacros(w) {
		return
	}
	var req editRequest
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: err.Error(), Code: "bad_request"})
		return
	}
	if req.MacroID == "" || len(req.Ops) == 0 {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "macro_id and at least one op are required", Code: "bad_request"})
		return
	}
	err := s.macros.store.AgentUpdate(req.MacroID, func(m *macro.Macro) {
		for _, op := range req.Ops {
			applyEditOp(m, op)
		}
		if req.Ops[len(req.Ops)-1].Descr != "" {
			m.Description = req.Ops[len(req.Ops)-1].Descr
		}
	})
	if err != nil {
		writeJSON(w, http.StatusForbidden, errorBody{Error: err.Error(), Code: "agent_edit_refused"})
		return
	}
	m, err := s.macros.store.Get(req.MacroID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: err.Error(), Code: "macro_store_failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"macro_id": m.ID, "steps": len(m.Steps), "approved": false,
	})
}

// applyEditOp applies one bounded op. Unknown ops and out-of-bounds indexes
// are recorded as malformed steps — the outer Update re-validates and an
// invalid result is never persisted.
func applyEditOp(m *macro.Macro, op editOp) {
	switch op.Op {
	case "append_step":
		if len(op.Step) == 0 {
			return
		}
		step := macro.Step{Action: op.Step}
		if op.Secret != "" {
			step.Secret = op.Secret
		}
		m.Steps = append(m.Steps, step)
	case "replace_step":
		if op.Index < 0 || op.Index >= len(m.Steps) || len(op.Step) == 0 {
			return
		}
		step := macro.Step{Action: op.Step}
		if op.Secret != "" {
			step.Secret = op.Secret
		}
		m.Steps[op.Index] = step
	case "drop_step":
		if op.Index < 0 || op.Index >= len(m.Steps) {
			return
		}
		m.Steps = append(m.Steps[:op.Index], m.Steps[op.Index+1:]...)
	case "move_step":
		j := op.Index + opIndexDelta(op)
		if op.Index < 0 || op.Index >= len(m.Steps) || j < 0 || j >= len(m.Steps) {
			return
		}
		m.Steps[op.Index], m.Steps[j] = m.Steps[j], m.Steps[op.Index]
	case "set_description":
		m.Description = op.Descr
	}
}

func opIndexDelta(op editOp) int {
	var d struct {
		Delta int `json:"delta"`
	}
	_ = json.Unmarshal(op.Step, &d) // move_step carries {"delta":±1} in step
	if d.Delta == 0 {
		var dir struct {
			Direction string `json:"direction"`
		}
		_ = json.Unmarshal(op.Step, &dir)
		switch dir.Direction {
		case "up":
			return -1
		case "down":
			return 1
		}
	}
	return d.Delta
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
	// Two replay classes: production (approved macro, any site) and agent
	// test replay (unapproved draft on an operator-designated site). Test
	// replays are labeled in the response, the evidence log and the admin
	// dashboard; nothing about them grants approval.
	testReplay := false
	if m.Approved == nil {
		if !s.macros.store.AgentTestReplayable(m) {
			writeJSON(w, http.StatusForbidden, errorBody{
				Error: "macro is not approved; a human must review the recording first (or mark its site agent-editable in the admin band)",
				Code:  "unapproved_macro",
			})
			return
		}
		testReplay = true
	}

	sess, ownedSession, err := s.openReplaySession(r, req, m)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: err.Error(), Code: "session_unavailable"})
		return
	}
	// Close only sessions this handler leased. A caller-provided session
	// (pre-navigated by the operator or mid-flow) must survive the replay.
	if ownedSession {
		defer func() { _ = s.sessions.Close(sess.ID) }()
	}

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
		s.admin.RecordReplay(m.ID, runErr == nil, time.Since(started), aborted, cause, testReplay)
	}()

	result := map[string]any{
		"macro_id":    m.ID,
		"session_id":  sess.ID,
		"duration_ms": time.Since(started).Milliseconds(),
		"test_replay": testReplay,
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
		result["evidence"] = evidenceViewFor(sess, testReplay)
		log.Warn("macro replay aborted", "macro", m.ID, "err", runErr)
		writeJSON(w, http.StatusConflict, result)
		return
	}
	result["ok"] = true
	result["steps"] = len(m.Steps)
	result["evidence"] = evidenceViewFor(sess, testReplay)
	log.Info("macro replay complete", "macro", m.ID)
	writeJSON(w, http.StatusOK, result)
}

func evidenceView(sess *session.Session) []session.ActionRecord {
	return sess.Records()
}

// evidenceViewFor prefixes the session action log with a test-replay marker
// so a labeled test run is recognizable inside its own evidence output.
func evidenceViewFor(sess *session.Session, testReplay bool) []session.ActionRecord {
	records := sess.Records()
	if !testReplay {
		return records
	}
	marker := session.ActionRecord{
		At:     time.Now().UTC(),
		Action: "replay_marker",
		OK:     true,
		Detail: "agent test replay of an unapproved draft on an operator-designated editable site; not an approved production run",
	}
	return append([]session.ActionRecord{marker}, records...)
}

func (s *Server) openReplaySession(r *http.Request, req replayRequest, m *macro.Macro) (*session.Session, bool, error) {
	if req.SessionID != "" {
		sess, err := s.sessions.Get(req.SessionID)
		return sess, false, err
	}
	host := req.Site
	if host == "" {
		host = m.Site
	}
	sess, err := s.sessions.Open(r.Context(), host)
	return sess, true, err
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
