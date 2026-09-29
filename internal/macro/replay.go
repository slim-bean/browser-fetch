package macro

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/slim-bean/browser-fetch/internal/session"
)

// SecretResolver turns op:// references into values at replay time. The
// gateway itself never sees plaintext outside the replay call frame; the
// concrete implementation (1Password CLI bridge) lands with Phase 2.
type SecretResolver interface {
	Resolve(ctx context.Context, ref string) (string, error)
}

// Pauser waits for a human to complete an OTP/CAPTCHA step. The server
// implements this over its assist/notification channel.
type Pauser interface {
	// WaitResume blocks until the human signals resume or the timeout lapses.
	WaitResume(ctx context.Context, macroID string, reason string, timeout time.Duration) error
}

// Replayer runs an approved macro against a live session.
type Replayer struct {
	Secrets SecretResolver
	Pauser  Pauser

	// DefaultPauseTimeout bounds pause steps that carry no explicit timeout.
	DefaultPauseTimeout time.Duration
	// MinTextLenForXPath: recorded element text shorter than this is too
	// ambiguous for a text XPath candidate.
	MinTextLenForXPath int
}

// ReplayError reports where a replay stopped and why. Errors are
// step-addressed so the operator sees exactly which human action no longer
// matches the live UI.
type ReplayError struct {
	Step  int    `json:"step"`
	Kind  string `json:"kind"`
	Cause string `json:"cause"`
	// Drift is the number of ladder candidates that missed before the step
	// succeeded (0 when a step failed outright).
	Drift int `json:"drift,omitempty"`
}

func (e *ReplayError) Error() string {
	return fmt.Sprintf("replay aborted at step %d (%s): %s", e.Step, e.Kind, e.Cause)
}

// Executor executes one typed action against the replay's session. The
// session Manager satisfies this via Run; the indirection keeps tests free of
// the session package's runOn stubbing.
type Executor interface {
	Run(ctx context.Context, sess *session.Session, a session.Action) (any, error)
}

// Run executes every step of an approved macro in order. Replay aborts at the
// first failed step — it never improvises.
func (r *Replayer) Run(ctx context.Context, exec Executor, sess *session.Session, m *Macro) error {
	if m.Approved == nil {
		return fmt.Errorf("macro %q is not approved; a human must review the recording first", m.ID)
	}
	if err := m.Validate(); err != nil {
		return err
	}
	for i, step := range m.Steps {
		if err := r.runStep(ctx, exec, sess, m, i, step); err != nil {
			var re *ReplayError
			if errors.As(err, &re) {
				return re
			}
			return &ReplayError{Step: i, Kind: stepKind(step), Cause: err.Error()}
		}
	}
	return nil
}

func stepKind(s Step) string {
	var a struct {
		Kind string `json:"kind"`
	}
	_ = json.Unmarshal(s.Action, &a)
	return a.Kind
}

func (r *Replayer) runStep(ctx context.Context, exec Executor, sess *session.Session, m *Macro, i int, step Step) error {
	kind := stepKind(step)
	switch kind {
	case "pause":
		return r.runPause(ctx, exec, sess, m, i, step)
	case "type":
		return r.runType(ctx, exec, sess, i, step)
	case "click":
		return r.runClick(ctx, exec, sess, i, step)
	case "autofill":
		return r.runAutofill(ctx, exec, sess, i, step)
	case "navigate", "wait", "assert", "screenshot", "content", "download":
		action, err := decodeMacroAction(step.Action)
		if err != nil {
			return &ReplayError{Step: i, Kind: kind, Cause: err.Error()}
		}
		if _, err := exec.Run(ctx, sess, action); err != nil {
			return &ReplayError{Step: i, Kind: kind, Cause: err.Error()}
		}
		return nil
	default:
		return &ReplayError{Step: i, Kind: kind, Cause: "unknown action kind in macro"}
	}
}

// decodeMacroAction decodes a macro step's raw action JSON into the typed
// action union (same shapes as the server's strict decoder, minus the
// agent-API kind restriction: the macro came from a human recording).
func decodeMacroAction(raw json.RawMessage) (session.Action, error) {
	var env struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("bad action: %w", err)
	}
	switch env.Kind {
	case "navigate":
		var a session.NavigateAction
		return a, json.Unmarshal(raw, &a)
	case "click":
		var a session.ClickAction
		return a, json.Unmarshal(raw, &a)
	case "type":
		var a session.TypeAction
		return a, json.Unmarshal(raw, &a)
	case "autofill":
		var a session.AutofillAction
		return a, json.Unmarshal(raw, &a)
	case "wait":
		var a session.WaitAction
		return a, json.Unmarshal(raw, &a)
	case "screenshot":
		var a session.ScreenshotAction
		return a, json.Unmarshal(raw, &a)
	case "content":
		var a session.ContentAction
		return a, json.Unmarshal(raw, &a)
	case "assert":
		var a session.AssertAction
		return a, json.Unmarshal(raw, &a)
	case "download":
		var a session.DownloadAction
		return a, json.Unmarshal(raw, &a)
	default:
		return nil, fmt.Errorf("unknown action kind %q", env.Kind)
	}
}

func (r *Replayer) runPause(ctx context.Context, exec Executor, sess *session.Session, m *Macro, i int, step Step) error {
	if step.Pause == nil {
		return &ReplayError{Step: i, Kind: "pause", Cause: "pause step lacks pause metadata"}
	}
	timeout := step.Pause.Timeout
	if timeout <= 0 {
		timeout = r.DefaultPauseTimeout
	}
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	if r.Pauser == nil {
		return &ReplayError{Step: i, Kind: "pause", Cause: "no pause channel configured (human cannot be notified)"}
	}
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := r.Pauser.WaitResume(pctx, m.ID, step.Pause.Reason, timeout); err != nil {
		return &ReplayError{Step: i, Kind: "pause", Cause: fmt.Sprintf("human resume not received: %v", err)}
	}
	if len(step.ResumeAssert) > 0 {
		action, err := decodeMacroAction(step.ResumeAssert)
		if err != nil {
			return &ReplayError{Step: i, Kind: "pause", Cause: "bad resume_assert: " + err.Error()}
		}
		if _, err := exec.Run(ctx, sess, action); err != nil {
			return &ReplayError{Step: i, Kind: "pause", Cause: "resume assert failed: " + err.Error()}
		}
	}
	return nil
}

// runType resolves the secret reference and types it. The resolved value
// exists only inside this call frame.
func (r *Replayer) runType(ctx context.Context, exec Executor, sess *session.Session, i int, step Step) error {
	var a session.TypeAction
	if err := json.Unmarshal(step.Action, &a); err != nil {
		return &ReplayError{Step: i, Kind: "type", Cause: err.Error()}
	}
	if step.Secret == "" {
		return &ReplayError{Step: i, Kind: "type", Cause: "type step without secret reference"}
	}
	if r.Secrets == nil {
		return &ReplayError{Step: i, Kind: "type", Cause: "no secret resolver configured"}
	}
	value, err := r.Secrets.Resolve(ctx, step.Secret)
	if err != nil {
		return &ReplayError{Step: i, Kind: "type", Cause: "secret resolve failed: " + err.Error()}
	}
	a.Text = value
	if _, err := exec.Run(ctx, sess, a); err != nil {
		return &ReplayError{Step: i, Kind: "type", Cause: err.Error()}
	}
	return nil
}

// runAutofill handles a programmatic fill (e.g. Chrome password manager).
// Replay types nothing: it waits for the browser to fill the field and
// verifies the field carries a value before the flow continues. If the fill
// never arrives (autofill disabled, profile moved, site markup changed),
// replay aborts rather than proceeding with an empty form.
func (r *Replayer) runAutofill(ctx context.Context, exec Executor, sess *session.Session, i int, step Step) error {
	var a session.AutofillAction
	if err := json.Unmarshal(step.Action, &a); err != nil {
		return &ReplayError{Step: i, Kind: "autofill", Cause: err.Error()}
	}
	if a.Selector == "" {
		return &ReplayError{Step: i, Kind: "autofill", Cause: "autofill step lacks selector"}
	}
	if _, err := exec.Run(ctx, sess, a); err != nil {
		return &ReplayError{Step: i, Kind: "autofill", Cause: err.Error()}
	}
	return nil
}

// runClick builds the selector ladder from the recorded element: recorded
// candidates first, then a generated text XPath (tier-2 fuzzy match). Ladder
// misses surface in the evidence log via ClickResult and in ReplayError.Drift.
func (r *Replayer) runClick(ctx context.Context, exec Executor, sess *session.Session, i int, step Step) error {
	var candidates []string
	if step.Recorded != nil && step.Recorded.Element != nil {
		candidates = append(candidates, step.Recorded.Element.Candidates...)
		candidates = append(candidates, textXPaths(step.Recorded.Element, r.MinTextLenForXPath)...)
	}
	if len(candidates) == 0 {
		return &ReplayError{Step: i, Kind: "click", Cause: "click step without recorded element candidates"}
	}
	res, err := exec.Run(ctx, sess, session.ClickAction{Candidates: candidates})
	if err != nil {
		return &ReplayError{Step: i, Kind: "click", Cause: err.Error()}
	}
	if cr, ok := res.(session.ClickResult); ok && cr.Misses > 0 {
		// Soft drift: a fallback candidate matched. Visible in the evidence
		// log; replay continues.
		_ = cr
	}
	return nil
}

// textXPaths generates tier-2 fuzzy-match candidates from the recorded role
// and text: //button[contains(normalize-space(.),'Sign in')] style probes.
func textXPaths(e *Element, minTextLen int) []string {
	if e == nil || len(strings.TrimSpace(e.Text)) < minTextLen {
		return nil
	}
	text := strings.ReplaceAll(strings.TrimSpace(e.Text), "'", "\\'")
	xp := func(tag string) string {
		return fmt.Sprintf("//%s[contains(normalize-space(.), '%s')]", tag, text)
	}
	switch e.Role {
	case "button":
		return []string{xp("button")}
	case "link":
		return []string{xp("a")}
	case "textbox", "password":
		return []string{
			fmt.Sprintf("//input[contains(@aria-label,'%s')]", text),
			fmt.Sprintf("//input[@placeholder='%s']", text),
		}
	case "combobox":
		return []string{xp("select")}
	default:
		return []string{xp("*")}
	}
}
