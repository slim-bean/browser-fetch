package macro

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// Recorder captures a human's interaction with a tab into a Macro draft.
//
// The gateway injects a gateway-authored capture script (page-level listeners
// for click/change/submit and navigation) that forwards events over CDP
// Runtime.bindingCalled. The no-caller-JS rule is unchanged: this script is
// ours, versioned in the repo, and only *reports* — it never mutates the page.
//
// Recording does NOT see typed secret values: password/value payloads from the
// binding are replaced with redacted markers by the page script; the human's
// keystrokes never leave the browser through this channel.
type Recorder struct {
	ID   string
	Site string

	mu      sync.Mutex
	started time.Time
	steps   []Step
	draft   *Macro
}

func NewRecorder(id, site string) *Recorder {
	return &Recorder{ID: id, Site: site, started: time.Now().UTC()}
}

// CaptureScript is the gateway-authored page script. It registers a binding
// and forwards normalized interaction events. Values are redacted: for
// password fields nothing is sent; for other inputs we send the final value
// only if cfg says so — see below (we send nothing: replay resolves values
// from secrets, so recording never needs them).
const CaptureScript = `(() => {
  if (window.__bfRecorderInstalled) return; // idempotent per document
  window.__bfRecorderInstalled = true;
  const send = (payload) => {
    try { window.__bfRecord(JSON.stringify(payload)); } catch (e) {}
  };
  const describe = (el) => {
    const r = (el.getAttribute && (el.getAttribute('role') || '')) || '';
    const tag = (el.tagName || '').toLowerCase();
    const role = r || (tag === 'input' ? (el.type === 'password' ? 'password' : 'textbox')
      : tag === 'button' ? 'button' : tag === 'a' ? 'link' : tag === 'select' ? 'combobox' : tag);
    const text = (tag === 'input' || tag === 'textarea')
      ? (el.placeholder || el.getAttribute('aria-label') || el.name || '')
      : ((el.getAttribute && el.getAttribute('aria-label')) || (el.innerText || '').trim().slice(0, 80));
    const attrs = {};
    for (const a of ['id', 'name', 'type', 'aria-label', 'data-test', 'data-testid']) {
      const v = el.getAttribute && el.getAttribute(a);
      if (v) attrs[a] = v;
    }
    // Selector ladder, strongest first.
    const cands = [];
    if (el.id) cands.push('#' + CSS.escape(el.id));
    for (const k of ['data-test', 'data-testid']) {
      if (attrs[k]) cands.push('[' + k + '="' + attrs[k] + '"]');
    }
    if (el.name) cands.push(tag + '[name="' + el.name + '"]');
    if (attrs['aria-label']) cands.push('[aria-label="' + attrs['aria-label'].replace(/"/g, '\\"') + '"]');
    let p = el, path = [];
    while (p && p.tagName && p.tagName.toLowerCase() !== 'body' && path.length < 5) {
      let sel = p.tagName.toLowerCase();
      if (p.id) { sel += '#' + CSS.escape(p.id); path.unshift(sel); break; }
      if (p.className && typeof p.className === 'string') {
        const c = p.className.trim().split(/\\s+/).slice(0, 2).map(c => '.' + CSS.escape(c)).join('');
        sel += c;
      }
      path.unshift(sel);
      p = p.parentElement;
    }
    if (path.length) cands.push(path.join(' > '));
    return { role, text, attrs, candidates: cands };
  };
  const url = () => location.href;
  const at = () => new Date().toISOString();

  document.addEventListener('click', (ev) => {
    const el = ev.target && ev.target.closest ? ev.target.closest('button, a, [role=button], input[type=submit], input[type=checkbox], select, li, tr') : null;
    if (!el) return;
    send({ ev: 'click', element: describe(el), url: url(), at: at() });
  }, true);

  // Change events (text inputs, selects). NEVER send the value for password
  // fields; for text inputs the recorded flow only needs the field identity —
  // values are resolved from the secret store at replay time.
  // Chrome password-manager autofill sets field values programmatically and
  // fires trusted 'change' events exactly like a human edit, so a naive
  // change listener records autofill as typing. Distinguish them: track
  // recent real key events per element; a change on an element that saw no
  // keystrokes is a fill. Fills become 'autofill' events (replay waits for
  // the field to carry a value and asserts it) — never 'type' steps, which
  // require a secret reference by policy.
  // An element counts as human-typed only after a REAL printable/editing
  // keystroke ON THAT ELEMENT. Any-key timing windows are wrong: pressing
  // Escape/Tab anywhere (e.g. dismissing a popup) must not make an autofill
  // that fires a second later look like typing, and selecting a checkbox
  // with Space fires change without being a text edit.
  const typed = new WeakMap(); // element -> true after a real text keystroke on it
  document.addEventListener('keydown', (ev) => {
    if (!ev.isTrusted) return;
    const k = ev.key;
    // Only printable characters count as text entry. Enter/Tab/Escape and
    // friends are navigation, not typing; Space on a checkbox is selection.
    if (k && k.length === 1 && k !== ' ') typed.set(ev.target, true);
  }, true);
  document.addEventListener('beforeinput', (ev) => {
    if (ev.isTrusted && ev.inputType && ev.inputType.startsWith('insert')) typed.set(ev.target, true);
  }, true);
  document.addEventListener('change', (ev) => {
    const el = ev.target;
    if (!el || !el.tagName) return;
    const tag = el.tagName.toLowerCase();
    if (tag !== 'input' && tag !== 'textarea' && tag !== 'select') return;
    if (el.type === 'hidden') return;
    const humanTyped = !!typed.get(el);
    typed.set(el, humanTyped);
    const field = el.type === 'password' ? 'password' : 'text';
    if (!humanTyped) {
      send({ ev: 'autofill', field: field, element: describe(el), url: url(), at: at() });
      return;
    }
    if (el.type === 'password') {
      send({ ev: 'type', field: 'password', element: describe(el), url: url(), at: at() });
      return;
    }
    send({ ev: 'type', field: 'text', element: describe(el), url: url(), at: at() });
  }, true);

  document.addEventListener('submit', (ev) => {
    send({ ev: 'submit', url: url(), at: at() });
  }, true);
})();`

// BindingName is the CDP binding added via Runtime.addBinding; the page
// script calls window.__bfRecord(payload).
const BindingName = "__bfRecord"

// Event is one decoded binding payload.
type Event struct {
	Ev      string   `json:"ev"` // click | type | submit | autofill | nav
	Field   string   `json:"field,omitempty"`
	Element *Element `json:"element,omitempty"`
	URL     string   `json:"url,omitempty"`
	At      string   `json:"at,omitempty"`
}

// AddEvent ingests one binding event and appends the corresponding step.
// Text values are never accepted: the capture script does not send them, and
// AddEvent ignores any "value"/"text" field even if present.
func (r *Recorder) AddEvent(payload []byte) error {
	var e Event
	if err := json.Unmarshal(payload, &e); err != nil {
		return fmt.Errorf("bad recorder event: %w", err)
	}
	at := time.Now().UTC()
	if e.At != "" {
		if t, err := time.Parse(time.RFC3339, e.At); err == nil {
			at = t
		}
	}
	rec := &Recorded{URL: e.URL, At: at}
	if e.Element != nil {
		rec.Element = e.Element
	}
	var step Step
	switch e.Ev {
	case "click":
		step = Step{Action: json.RawMessage(`{"kind":"click"}`), Recorded: rec}
	case "type":
		step = Step{Action: json.RawMessage(`{"kind":"type","field":"` + e.Field + `"}`), Recorded: rec}
	case "submit":
		step = Step{Action: json.RawMessage(`{"kind":"click"}`), Recorded: rec}
	case "autofill":
		// A programmatic fill (Chrome password manager et al). Recorded so
		// replay can assert the field is actually filled — if the fill ever
		// stops happening, replay stops instead of submitting an empty form.
		// The step's selector is the recorded element's best candidate.
		step = Step{Action: json.RawMessage(
			`{"kind":"autofill","field":"` + e.Field + `","selector":` + selectorJSON(e.Element) + `}`),
			Recorded: rec}
	default:
		return fmt.Errorf("unknown recorder event %q", e.Ev)
	}
	r.mu.Lock()
	r.steps = append(r.steps, step)
	r.mu.Unlock()
	return nil
}

// Steps returns the recorded steps so far.
func (r *Recorder) Steps() []Step {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Step, len(r.steps))
	copy(out, r.steps)
	return out
}

// Draft returns the macro draft (unapproved) for human review.
func (r *Recorder) Draft() *Macro {
	return &Macro{
		ID:      r.ID,
		Site:    r.Site,
		Created: r.started.UTC(),
		Steps:   r.Steps(),
	}
}

// Runner is the interface the replay engine implements; declared here so the
// recorder package stays dependency-light in tests.
type Runner interface {
	Run(ctx context.Context, m *Macro) error
}

// selectorJSON picks the strongest recorded candidate as the replay selector
// and returns it as a JSON string (empty string when nothing was described).
func selectorJSON(e *Element) string {
	if e == nil || len(e.Candidates) == 0 {
		return `""`
	}
	c, _ := json.Marshal(e.Candidates[0])
	return string(c)
}
