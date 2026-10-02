// Admin band: a separate HTTP listener, gated by its own token, that only a
// human reaches. Every authority-granting macro transition — approve, revoke,
// edit, delete — lives EXCLUSIVELY here. The agent-facing API deliberately
// has none of these powers: its /macro/approve was removed (410), and no
// /macro/edit exists at all. Approval that the agent could grant or forge
// would certify nothing.
//
// The UI is server-rendered HTML forms on purpose: no build pipeline, no
// client framework, no JavaScript. The admin token doubles as CSRF
// protection — every mutating form carries it as a hidden field, and the
// handler rejects posts without it.
package server

import (
	"crypto/subtle"
	"encoding/json"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/slim-bean/browser-fetch/internal/logx"
	"github.com/slim-bean/browser-fetch/internal/macro"
)

// Admin holds the human-only macro review surface.
type Admin struct {
	store    *macro.Store
	tokenVal string
	mux      *http.ServeMux
	logMu    sync.Mutex
	audit    []auditEntry   // recent admin actions, shown on the dashboard
	replays  []replayRecord // recent replay outcomes, shown on the dashboard
}

type auditEntry struct {
	At    time.Time
	Act   string // approve/revoke/delete/drop_step/move_step/set_description
	Macro string
	Note  string
}

type replayRecord struct {
	At        time.Time
	MacroID   string
	OK        bool
	Duration  time.Duration
	AbortedAt int // -1 when the replay ran to completion
	Cause     string
	Test      bool   // agent test replay of an unapproved, editable-site draft
	Dump      string // failure-time page state (URL/title/field probes) on abort
}

func NewAdmin(store *macro.Store, token string) *Admin {
	return &Admin{store: store, tokenVal: token}
}

// RecordReplay stores a replay outcome for the dashboard. The agent-facing
// replay handler feeds it: recording outcomes is observability, not
// authority.
func (a *Admin) RecordReplay(macroID string, ok bool, d time.Duration, abortedAt int, cause string, test bool, dump string) {
	if a == nil {
		return
	}
	a.logMu.Lock()
	defer a.logMu.Unlock()
	a.replays = append(a.replays, replayRecord{
		At: time.Now().UTC(), MacroID: macroID, OK: ok, Duration: d,
		AbortedAt: abortedAt, Cause: cause, Test: test, Dump: dump,
	})
	if len(a.replays) > 25 {
		a.replays = a.replays[len(a.replays)-25:]
	}
}

func (a *Admin) auditf(act, id, note string) {
	a.logMu.Lock()
	defer a.logMu.Unlock()
	a.audit = append(a.audit, auditEntry{At: time.Now().UTC(), Act: act, Macro: id, Note: note})
	if len(a.audit) > 50 {
		a.audit = a.audit[len(a.audit)-50:]
	}
}

// Handler builds the admin mux wrapped in the token guard.
func (a *Admin) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.guard(a.handleDashboard))
	mux.HandleFunc("GET /macro", a.guard(a.handleView))
	mux.HandleFunc("POST /action", a.guard(a.handleAction))
	a.mux = mux
	return a
}

func (a *Admin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if a.mux == nil {
		a.Handler() // tests may construct Admin directly
	}
	a.mux.ServeHTTP(w, r)
}

// token extracts the admin token from the Authorization header or the t=
// query/form field (plain HTML forms cannot set headers).
func (a *Admin) token(r *http.Request) string {
	const prefix = "Bearer "
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, prefix) && len(h) > len(prefix) {
		return h[len(prefix):]
	}
	if err := r.ParseForm(); err != nil {
		return ""
	}
	return r.FormValue("t")
}

// guard rejects anything without the exact admin token. Constant-time
// compare: the token is the only thing standing between the agent band and
// approval authority.
func (a *Admin) guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := a.token(r)
		if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(a.tokenVal)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error": "admin token required", "code": "unauthorized",
			})
			return
		}
		h(w, r)
	}
}

// ---- views -------------------------------------------------------------------

type macroRow struct {
	ID         string
	Site       string
	Created    time.Time
	Approved   bool
	ApprovedBy string
	Steps      int
}

type dashboardData struct {
	Token   string
	Macros  []macroRow
	Orphans []string // parked drafts from failed stores, recoverable here
	Audit   []auditEntry
	Replays []replayRecord
	// AgentEditableSites: sites the operator has granted agent edit rights
	// over (drafts + test replays). Grants and revocations happen here only.
	AgentEditableSites []macro.EditableSite
	// EditableToggleSites: distinct sites in the store without a grant, so
	// the operator can grant from this page without typing site names.
	EditableToggleSites []string
}

var dashboardTmpl = template.Must(template.New("dash").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><title>browser-fetch macro admin</title>
<style>
 body{font-family:system-ui,sans-serif;margin:2rem;max-width:60rem;color:#1a1a2e}
 table{border-collapse:collapse;width:100%;margin:1rem 0}
 th,td{border:1px solid #ccc;padding:.35rem .6rem;text-align:left;font-size:.9rem}
 th{background:#eef}
 form{display:inline}
 button{cursor:pointer}
 .draft{background:#fff3cd;padding:.1rem .4rem;border-radius:.3rem;font-size:.8rem}
 .approved{background:#d4edda;padding:.1rem .4rem;border-radius:.3rem;font-size:.8rem}
 section{margin:2rem 0}
 h1{font-size:1.3rem} h2{font-size:1.05rem}
 .muted{color:#666;font-size:.85rem}
</style></head><body>
<h1>Macro admin <span class="muted">— human band; the agent cannot reach this</span></h1>
<section>
<h2>Macros</h2>
<table>
<tr><th>ID</th><th>Site</th><th>State</th><th>Steps</th><th>Created</th><th>Actions</th></tr>
{{$t := .Token}}
{{range .Macros}}
<tr>
 <td><a href="/macro?id={{.ID}}&t={{$t}}">{{.ID}}</a></td>
 <td>{{.Site}}</td>
 <td>{{if .Approved}}<span class="approved">approved{{if .ApprovedBy}} by {{.ApprovedBy}}{{end}}</span>
     {{else}}<span class="draft">draft</span>{{end}}</td>
 <td>{{.Steps}}</td>
 <td>{{.Created.Format "2006-01-02 15:04"}}</td>
 <td>
   {{if .Approved}}
   <form method="post" action="/action"><input type="hidden" name="t" value="{{$t}}">
     <input type="hidden" name="op" value="revoke"><input type="hidden" name="id" value="{{.ID}}">
     <button>revoke</button></form>
   {{else}}
   <form method="post" action="/action"><input type="hidden" name="t" value="{{$t}}">
     <input type="hidden" name="op" value="approve"><input type="hidden" name="id" value="{{.ID}}">
     <button>approve</button></form>
   {{end}}
   <form method="post" action="/action"><input type="hidden" name="t" value="{{$t}}">
     <input type="hidden" name="op" value="delete"><input type="hidden" name="id" value="{{.ID}}">
     <button>delete</button></form>
 </td>
</tr>
{{else}}
<tr><td colspan="6">no macros stored</td></tr>
{{end}}
</table>
</section>
<section>
<h2>Agent-editable sites <span class="muted">(operator grant: the agent may author, edit and test-replay drafts on these sites; approved macros stay frozen)</span></h2>
<table>
<tr><th>Site</th><th>Granted</th><th>By</th><th>Note</th><th>Actions</th></tr>
{{range .AgentEditableSites}}
<tr><td>{{.Site}}</td><td>{{.GrantedAt.Format "2006-01-02 15:04"}}</td><td>{{.GrantedBy}}</td><td>{{.Note}}</td>
<td><form method="post" action="/action"><input type="hidden" name="t" value="{{$t}}">
  <input type="hidden" name="op" value="disable_agent_edit"><input type="hidden" name="site" value="{{.Site}}">
  <button>revoke</button></form></td></tr>
{{else}}
<tr><td colspan="5">none — the agent cannot author or edit any macros</td></tr>
{{end}}
</table>
{{if .EditableToggleSites}}
<p class="muted">Sites in the store without a grant:</p>
{{range .EditableToggleSites}}
<form method="post" action="/action" style="display:inline;margin-right:1rem"><input type="hidden" name="t" value="{{$t}}">
  <input type="hidden" name="op" value="enable_agent_edit"><input type="hidden" name="site" value="{{.}}">
  <button>allow agent edits: {{.}}</button></form>
{{end}}
{{end}}
</section>
<section>
<h2>Recent replays <span class="muted">(triggered by the agent, shown here for audit)</span></h2>
<table>
<tr><th>When</th><th>Macro</th><th>Result</th><th>Detail</th></tr>
{{range .Replays}}
<tr><td>{{.At.Format "15:04:05"}}</td><td>{{.MacroID}}{{if .Test}} <span class="draft">test</span>{{end}}</td>
 <td>{{if .OK}}ok{{else}}<b>aborted at step {{.AbortedAt}}</b>{{end}}</td>
 <td>{{.Duration}}{{if .Cause}} — {{.Cause}}{{end}}{{if .Dump}}<br><code class="muted">{{.Dump}}</code>{{end}}</td></tr>
{{else}}
<tr><td colspan="4">none yet</td></tr>
{{end}}
</table>
</section>
{{if .Orphans}}
<section>
<h2>Parked drafts <span class="muted">(recordings that failed to store; steps recovered here)</span></h2>
<table>
<tr><th>ID</th><th>Actions</th></tr>
{{range .Orphans}}
<tr><td>{{.}}</td>
<td>
  <form method="post" action="/action"><input type="hidden" name="t" value="{{$t}}">
    <input type="hidden" name="op" value="promote_orphan"><input type="hidden" name="id" value="{{.}}">
    <button>recover as draft</button></form>
  <form method="post" action="/action"><input type="hidden" name="t" value="{{$t}}">
    <input type="hidden" name="op" value="discard_orphan"><input type="hidden" name="id" value="{{.}}">
    <button>discard</button></form>
</td></tr>
{{end}}
</table>
</section>
{{end}}
<section>
<h2>Admin audit log</h2>
<table>
<tr><th>When</th><th>Action</th><th>Macro</th><th>Note</th></tr>
{{range .Audit}}
<tr><td>{{.At.Format "15:04:05"}}</td><td>{{.Act}}</td><td>{{.Macro}}</td><td>{{.Note}}</td></tr>
{{else}}
<tr><td colspan="4">none yet</td></tr>
{{end}}
</table>
</section>
</body></html>
`))

func (a *Admin) handleDashboard(w http.ResponseWriter, r *http.Request) {
	ids, _ := a.store.List()
	var rows []macroRow
	for _, id := range ids {
		m, err := a.store.Get(id)
		if err != nil {
			continue
		}
		row := macroRow{ID: m.ID, Site: m.Site, Created: m.Created, Steps: len(m.Steps)}
		if m.Approved != nil {
			row.Approved = true
			row.ApprovedBy = m.Approved.By
		}
		rows = append(rows, row)
	}
	a.logMu.Lock()
	granted := map[string]bool{}
	for _, e := range a.store.AgentEditableSites() {
		granted[e.Site] = true
	}
	// Sites from stored macros that have no grant yet: one-click grant UI.
	seen := map[string]bool{}
	var toggle []string
	for _, id := range ids {
		m, err := a.store.Get(id)
		if err != nil || m.Site == "" || seen[m.Site] {
			continue
		}
		seen[m.Site] = true
		if !granted[m.Site] {
			toggle = append(toggle, m.Site)
		}
	}
	data := dashboardData{Token: a.tokenVal, Macros: rows,
		Audit:               append([]auditEntry(nil), a.audit...),
		Replays:             append([]replayRecord(nil), a.replays...),
		AgentEditableSites:  a.store.AgentEditableSites(),
		EditableToggleSites: toggle}
	a.logMu.Unlock()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = dashboardTmpl.Execute(w, data)
}

// macroStepView renders one step for review.
type macroStepView struct {
	Index      int
	Kind       string
	Summary    string
	HasElement bool
	JSON       string
}

type macroViewData struct {
	Token string
	Macro *macro.Macro
	Steps []macroStepView
	Error string
}

var macroTmpl = template.Must(template.New("macro").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><title>macro {{.Macro.ID}}</title>
<style>
 body{font-family:system-ui,sans-serif;margin:2rem;max-width:60rem;color:#1a1a2e}
 table{border-collapse:collapse;width:100%;margin:1rem 0}
 th,td{border:1px solid #ccc;padding:.35rem .6rem;text-align:left;font-size:.85rem;vertical-align:top}
 th{background:#eef}
 pre{margin:0;white-space:pre-wrap;font-size:.75rem}
 form{display:inline}
 button{cursor:pointer}
 .draft{background:#fff3cd;padding:.1rem .4rem;border-radius:.3rem;font-size:.8rem}
 .approved{background:#d4edda;padding:.1rem .4rem;border-radius:.3rem;font-size:.8rem}
 .warn{background:#f8d7da;padding:.5rem;border-radius:.3rem}
 .muted{color:#666;font-size:.85rem}
</style></head><body>
<h1>{{.Macro.ID}} <span class="muted">on {{.Macro.Site}}</span>
 {{if .Macro.Approved}}<span class="approved">approved{{if .Macro.Approved.By}} by {{.Macro.Approved.By}} at {{.Macro.Approved.At.Format "2006-01-02 15:04:05"}}{{end}}</span>
 {{else}}<span class="draft">draft — not replayable</span>{{end}}</h1>
{{if .Error}}<p class="warn">{{.Error}}</p>{{end}}
{{$t := .Token}}
<p><a href="/?t={{$t}}">&larr; all macros</a></p>
<table>
<tr><th>#</th><th>Action</th><th>Recorded context</th><th>Edit</th></tr>
{{range .Steps}}
<tr>
 <td>{{.Index}}</td>
 <td><b>{{.Kind}}</b>{{if .Summary}}<br><span class="muted">{{.Summary}}</span>{{end}}</td>
 <td><pre>{{.JSON}}</pre>{{if not .HasElement}}<span class="warn">no element fingerprint — replay cannot match this step</span>{{end}}</td>
 <td>
   <form method="post" action="/action"><input type="hidden" name="t" value="{{$t}}">
     <input type="hidden" name="op" value="drop_step"><input type="hidden" name="id" value="{{$.Macro.ID}}">
     <input type="hidden" name="index" value="{{.Index}}"><button>drop</button></form>
   <form method="post" action="/action"><input type="hidden" name="t" value="{{$t}}">
     <input type="hidden" name="op" value="move_up"><input type="hidden" name="id" value="{{$.Macro.ID}}">
     <input type="hidden" name="index" value="{{.Index}}"><button>&uarr;</button></form>
   <form method="post" action="/action"><input type="hidden" name="t" value="{{$t}}">
     <input type="hidden" name="op" value="move_down"><input type="hidden" name="id" value="{{$.Macro.ID}}">
     <input type="hidden" name="index" value="{{.Index}}"><button>&darr;</button></form>
 </td>
</tr>
{{end}}
</table>
</body></html>
`))

func (a *Admin) handleView(w http.ResponseWriter, r *http.Request) {
	id := r.FormValue("id")
	m, err := a.store.Get(id)
	if err != nil {
		http.Error(w, "unknown macro", http.StatusNotFound)
		return
	}
	data := macroViewData{Token: a.tokenVal, Macro: m}
	for i, s := range m.Steps {
		var action struct {
			Kind     string `json:"kind"`
			Selector string `json:"selector"`
			Field    string `json:"field"`
		}
		_ = json.Unmarshal(s.Action, &action)
		summary := action.Field
		if action.Selector != "" {
			if summary != "" {
				summary += " → "
			}
			summary += action.Selector
		}
		data.Steps = append(data.Steps, macroStepView{
			Index: i, Kind: action.Kind, Summary: summary,
			HasElement: s.Recorded != nil && s.Recorded.Element != nil,
			JSON:       string(recordedJSON(s)),
		})
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = macroTmpl.Execute(w, data)
}

// recordedJSON renders the recorded context (or a placeholder).
func recordedJSON(s macro.Step) []byte {
	if s.Recorded == nil {
		return []byte("(no recorded context)")
	}
	b, err := json.MarshalIndent(s.Recorded, "", "  ")
	if err != nil {
		return []byte("(unrenderable context)")
	}
	return b
}

// ---- actions -------------------------------------------------------------------

func (a *Admin) handleAction(w http.ResponseWriter, r *http.Request) {
	log := logx.From(r.Context())
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	op := r.FormValue("op")
	id := r.FormValue("id")
	idx, hasIdx := -1, false
	if v := r.FormValue("index"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			idx, hasIdx = n, true
		} else {
			a.fail(w, id, "bad step index", err)
			return
		}
	}
	var err error
	note := ""
	switch op {
	case "approve":
		err = a.store.Approve(id, "ed (admin band)")
		note = "approval granted by human at admin band"
	case "revoke":
		err = a.store.Revoke(id)
		note = "approval revoked by human at admin band"
	case "delete":
		err = a.store.Delete(id)
		note = "deleted by human at admin band"
	case "drop_step":
		if !hasIdx {
			a.fail(w, id, "drop_step needs an index", nil)
			return
		}
		err = a.store.Update(id, func(m *macro.Macro) {
			if idx >= 0 && idx < len(m.Steps) {
				m.Steps = append(m.Steps[:idx], m.Steps[idx+1:]...)
			}
		})
		note = "step dropped by human at admin band"
	case "promote_orphan":
		err = a.store.PromoteOrphan(id)
		note = "parked draft recovered as draft by human at admin band"
	case "discard_orphan":
		err = a.store.DiscardOrphan(id)
		note = "parked draft discarded by human at admin band"
	case "enable_agent_edit":
		a.siteAction(w, r, op, func(site string) error {
			return a.store.EnableAgentEditable(site, "ed (admin band)", r.FormValue("note"))
		}, "agent edit grant recorded by human at admin band")
		return
	case "disable_agent_edit":
		a.siteAction(w, r, op, func(site string) error {
			return a.store.DisableAgentEditable(site)
		}, "agent edit grant revoked by human at admin band")
		return
	case "move_up", "move_down":
		if !hasIdx {
			a.fail(w, id, op+" needs an index", nil)
			return
		}
		err = a.store.Update(id, func(m *macro.Macro) {
			j := idx - 1
			if op == "move_down" {
				j = idx + 1
			}
			if idx < 0 || idx >= len(m.Steps) || j < 0 || j >= len(m.Steps) {
				return // out of bounds: no-op, macro re-persists unchanged
			}
			m.Steps[idx], m.Steps[j] = m.Steps[j], m.Steps[idx]
		})
		note = "reordered by human at admin band"
	default:
		http.Error(w, "unknown op", http.StatusBadRequest)
		return
	}
	if err != nil {
		log.Warn("admin action failed", "op", op, "macro", id, "err", err)
		a.fail(w, id, op+" failed: "+err.Error(), nil)
		return
	}
	a.auditf(op, id, note)
	log.Info("admin action", "op", op, "macro", id)
	http.Redirect(w, r, redirectTarget(op, id, a.tokenVal), http.StatusSeeOther)
}

// siteAction runs a site-scoped admin op (agent-edit grants/revocations),
// audits it, and redirects to the dashboard. Site ops carry no macro id.
func (a *Admin) siteAction(w http.ResponseWriter, r *http.Request, op string, run func(site string) error, note string) {
	site := r.FormValue("site")
	if site == "" {
		http.Error(w, op+" needs a site", http.StatusBadRequest)
		return
	}
	if err := run(site); err != nil {
		logx.From(r.Context()).Warn("admin action failed", "op", op, "site", site, "err", err)
		http.Error(w, op+" failed: "+err.Error(), http.StatusBadRequest)
		return
	}
	a.auditf(op, "", note+": "+site)
	logx.From(r.Context()).Info("admin action", "op", op, "site", site)
	http.Redirect(w, r, "/?t="+a.tokenVal, http.StatusSeeOther)
}

func redirectTarget(op, id, token string) string {
	if op == "delete" || op == "approve" || op == "revoke" {
		return "/?t=" + token
	}
	return "/macro?id=" + id + "&t=" + token
}

// fail re-renders the macro view with an error message.
func (a *Admin) fail(w http.ResponseWriter, id, msg string, _ error) {
	m, err := a.store.Get(id)
	if err != nil {
		http.Error(w, msg, http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = macroTmpl.Execute(w, macroViewData{Token: a.tokenVal, Macro: m, Error: msg})
}
