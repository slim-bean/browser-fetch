// Package macro implements human-recorded, agent-replayed site flows.
//
// The agent never operates a site: a human performs the flow once in the
// VNC-visible Chrome while the gateway records interaction events; the
// recording becomes a macro that replay executes deterministically, with
// drift detection and a pause step for human-in-the-loop moments (OTP,
// CAPTCHA). See docs/macros.md for the model and format.
package macro

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Element describes a recorded DOM element well enough to re-find it after
// cosmetic drift: a ladder of candidate selectors plus stable attributes.
type Element struct {
	// Candidates is an ordered list of CSS selectors, strongest first.
	Candidates []string `json:"candidates"`
	// Role is the ARIA-ish role guess (textbox, button, link, …).
	Role string `json:"role,omitempty"`
	// Text is visible inner text / value / aria-label, used for fuzzy match.
	Text string `json:"text,omitempty"`
	// Attrs are stable attributes worth matching (name, type, id, data-test).
	Attrs map[string]string `json:"attrs,omitempty"`
}

// Step is one recorded action plus its recording context.
type Step struct {
	// Action is the typed primitive to execute. For type steps, Text must be
	// empty and Secret set; values never live in macro files.
	Action json.RawMessage `json:"action"`
	// Secret is an op:// reference resolved at replay time. Empty for
	// non-secret steps.
	Secret string `json:"secret,omitempty"`
	// Recorded captures the human context: element fingerprint, page URL,
	// pre-step screenshot hash. Replay uses it for drift detection.
	Recorded *Recorded `json:"recorded,omitempty"`
	// Pause marks a human-in-the-loop point (OTP, CAPTCHA). Replay waits for
	// a human signal before continuing.
	Pause *Pause `json:"pause,omitempty"`
	// ResumeAssert, on pause steps, must pass after the human signals resume.
	ResumeAssert json.RawMessage `json:"resume_assert,omitempty"`
}

// Recorded is the capture-time context of a step.
type Recorded struct {
	// URL of the page the step happened on.
	URL string `json:"url,omitempty"`
	// Element fingerprint (interaction steps only).
	Element *Element `json:"element,omitempty"`
	// ScreenshotSHA is the pre-step screenshot hash for visual diffing.
	ScreenshotSHA string `json:"screenshot_sha,omitempty"`
	// At is when the human performed the step (recording order matters).
	At time.Time `json:"at"`
}

// Pause describes a wait-for-human step.
type Pause struct {
	// Reason is shown to the human ("2FA code", "CAPTCHA").
	Reason string `json:"reason"`
	// Timeout bounds the wait.
	Timeout time.Duration `json:"timeout,omitempty"`
}

// Macro is a recorded, human-approved flow for one site.
type Macro struct {
	ID          string    `json:"id"`
	Site        string    `json:"site"`
	Description string    `json:"description,omitempty"`
	Created     time.Time `json:"created"`
	// Approved is nil until a human has reviewed the recording. Replay only
	// runs approved macros.
	Approved    *Approval `json:"approved,omitempty"`
	ProfileHash string    `json:"profile_hash,omitempty"`
	Steps       []Step    `json:"steps"`
}

// Approval records the human sign-off.
type Approval struct {
	By string    `json:"by"`
	At time.Time `json:"at"`
}

// Validate checks structural invariants that must hold before a macro can be
// approved: every type step references a secret (no inline values), pause
// steps carry a reason, and steps are non-empty.
func (m *Macro) Validate() error {
	if strings.TrimSpace(m.ID) == "" {
		return errors.New("macro id is required")
	}
	if strings.TrimSpace(m.Site) == "" {
		return errors.New("macro site is required")
	}
	if len(m.Steps) == 0 {
		return errors.New("macro has no steps")
	}
	for i, s := range m.Steps {
		var a struct {
			Kind string `json:"kind"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(s.Action, &a); err != nil {
			return fmt.Errorf("step %d: bad action: %w", i, err)
		}
		if a.Kind == "" {
			return fmt.Errorf("step %d: action kind missing", i)
		}
		if a.Kind == "type" && a.Text != "" {
			return fmt.Errorf("step %d: type step must reference a secret, not carry a value", i)
		}
		if a.Kind == "type" && s.Secret == "" && s.Pause == nil {
			return fmt.Errorf("step %d: type step needs a secret reference", i)
		}
		if a.Kind == "autofill" {
			if a.Text != "" {
				return fmt.Errorf("step %d: autofill step must reference a fill, not carry a value", i)
			}
			var sel struct {
				Selector string `json:"selector"`
			}
			_ = json.Unmarshal(s.Action, &sel)
			if strings.TrimSpace(sel.Selector) == "" {
				return fmt.Errorf("step %d: autofill step needs a selector", i)
			}
		}
		if s.Pause != nil && strings.TrimSpace(s.Pause.Reason) == "" {
			return fmt.Errorf("step %d: pause needs a reason", i)
		}
	}
	return nil
}

// ---- store ------------------------------------------------------------------

// Store keeps macro JSON files on disk. Macros are account-grade sensitive
// (they reveal a bank's navigation map): the caller points Store at an
// encrypted-at-rest location; here we only enforce atomic writes and
// 0600/0700 permissions.
type Store struct {
	dir string
	mu  sync.Mutex
}

func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

func (s *Store) path(id string) string {
	return filepath.Join(s.dir, sanitizeID(id)+".json")
}

func sanitizeID(id string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(id) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// Put writes a macro atomically. Existing macros are never silently
// overwritten unless the caller replaces an unapproved one.
func (s *Store) Put(m *Macro, overwrite bool) error {
	if err := m.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.path(m.ID)
	if _, err := os.Stat(path); err == nil && !overwrite {
		return fmt.Errorf("macro %q already exists", m.ID)
	}
	if !overwrite {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("macro %q already exists", m.ID)
		}
	}
	buf, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Get loads a macro by id.
func (s *Store) Get(id string) (*Macro, error) {
	buf, err := os.ReadFile(s.path(id))
	if err != nil {
		return nil, err
	}
	var m Macro
	if err := json.Unmarshal(buf, &m); err != nil {
		return nil, fmt.Errorf("macro %q: %w", id, err)
	}
	return &m, nil
}

// List returns ids of all stored macros, pending ones included (they carry
// Approved == nil).
func (s *Store) List() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		ids = append(ids, strings.TrimSuffix(e.Name(), ".json"))
	}
	sort.Strings(ids)
	return ids, nil
}

// Approve stamps a macro as human-approved and persists it.
func (s *Store) Approve(id, by string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.Get(id)
	if err != nil {
		return err
	}
	if err := m.Validate(); err != nil {
		return err
	}
	m.Approved = &Approval{By: by, At: time.Now().UTC()}
	buf, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path(id) + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(id))
}

// HashProfile summarizes a browser profile directory (userdata dir contents)
// so a macro can refuse to replay against a different fingerprint. We hash
// selected identity-bearing file names + sizes rather than the whole profile
// (multi-GB).
func HashProfile(profileDir string) (string, error) {
	const marker = "Local State"
	st, err := os.Stat(filepath.Join(profileDir, marker))
	if err != nil {
		return "", fmt.Errorf("profile hash: %w", err)
	}
	entries, err := os.ReadDir(profileDir)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	fmt.Fprintf(h, "marker:%d\n", st.Size())
	for _, e := range entries {
		if e.IsDir() {
			fmt.Fprintf(h, "d:%s\n", e.Name())
			continue
		}
		if info, err := e.Info(); err == nil {
			fmt.Fprintf(h, "f:%s:%d\n", e.Name(), info.Size())
		}
	}
	return fmt.Sprintf("sha256:%x", h.Sum(nil)), nil
}
