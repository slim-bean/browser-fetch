package server

import (
	"html/template"
	"net/http"
	"time"

	"github.com/slim-bean/browser-fetch/internal/logx"
)

// handleDebug renders live state: config, Chrome health, scheduler snapshot and
// the recent-request ring. `?format=json` returns the same data as JSON, which
// is what you want from a script; the HTML view is for a human on the VM.
func (s *Server) handleDebug(w http.ResponseWriter, r *http.Request) {
	limit := 100
	data := debugData{
		Uptime:    time.Since(s.start).Round(time.Second).String(),
		Config:    s.redactedConfig(),
		Chrome:    s.mgr.Health(),
		Scheduler: s.sched.Snapshot(),
		Recent:    s.ring.Recent(limit),
		Now:       time.Now(),
	}

	if r.URL.Query().Get("format") == "json" {
		writeJSON(w, http.StatusOK, data)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := debugTmpl.Execute(w, data); err != nil {
		logx.From(r.Context()).Error("debug template failed", "err", err)
	}
}

type debugData struct {
	Uptime    string `json:"uptime"`
	Now       time.Time
	Config    map[string]any `json:"config"`
	Chrome    any            `json:"chrome"`
	Scheduler any            `json:"scheduler"`
	Recent    any            `json:"recent"`
}

// redactedConfig never includes the token.
func (s *Server) redactedConfig() map[string]any {
	c := s.cfg
	tokenState := "set"
	if c.Token == "" {
		tokenState = "NOT SET (unauthenticated)"
	}
	return map[string]any{
		"addr":                  c.Addr,
		"token":                 tokenState,
		"chrome_url":            c.ChromeURL,
		"max_tabs":              c.MaxTabs,
		"host_gap":              c.HostGap.String(),
		"host_jitter":           c.HostJitter.String(),
		"request_timeout":       c.RequestTimeout.String(),
		"nav_timeout":           c.NavTimeout.String(),
		"challenge_wait":        c.ChallengeWait.String(),
		"assist_timeout":        c.AssistTimeout.String(),
		"challenge_retries":     c.ChallengeRetries,
		"challenge_retry_delay": c.ChallengeRetryDelay.String(),
		"block_media":           c.BlockMedia,
		"allow_private":         c.AllowPrivate,
		"debug":                 c.Debug,
		"debug_ring":            c.DebugRing,
		"metrics_auth":          c.MetricsAuth,
		"log_format":            c.LogFormat,
		"log_level":             c.LogLevel,
	}
}

var debugTmpl = template.Must(template.New("debug").Funcs(template.FuncMap{
	"short": func(s string, n int) string {
		if len(s) <= n {
			return s
		}
		return s[:n] + "…"
	},
}).Parse(`<!doctype html>
<meta charset="utf-8">
<title>pi browser gateway — debug</title>
<style>
 body{font:13px/1.5 ui-monospace,SFMono-Regular,Menlo,monospace;margin:1.5rem;color:#111}
 h1{font-size:1.1rem} h2{font-size:.95rem;margin:1.4rem 0 .4rem}
 table{border-collapse:collapse;width:100%;margin-bottom:.5rem}
 th,td{text-align:left;padding:.25rem .5rem;border-bottom:1px solid #e3e3e3;vertical-align:top}
 th{background:#f6f6f6;font-weight:600}
 .ok{color:#0a7d28}.bad{color:#b30000}.warn{color:#a86200}
 .kv{display:grid;grid-template-columns:max-content 1fr;gap:0 1rem}
 code{background:#f3f3f3;padding:0 .2rem}
 a{color:#06c}
</style>
<h1>pi browser gateway <span class="warn">debug</span></h1>
<p>uptime {{.Uptime}} · {{.Now.Format "2006-01-02 15:04:05 MST"}} ·
   <a href="?format=json">json</a> · <a href="/stats">/stats</a> ·
   <a href="/healthz">/healthz</a> · <a href="/metrics">/metrics</a> ·
   <a href="/debug/pprof/">pprof</a></p>

<h2>chrome</h2>
<pre>{{printf "%+v" .Chrome}}</pre>

<h2>scheduler</h2>
<pre>{{printf "%+v" .Scheduler}}</pre>

<h2>config</h2>
<div class="kv">{{range $k, $v := .Config}}<div>{{$k}}</div><div><code>{{$v}}</code></div>{{end}}</div>

<h2>recent requests</h2>
<table>
<tr><th>at<th>id<th>host<th>outcome<th>tgt<th>ms<th>queue<th>bytes<th>page<th>url<th>note</tr>
{{range .Recent}}
<tr>
 <td>{{.At.Format "15:04:05"}}
 <td>{{.ID}}
 <td>{{.Host}}
 <td class="{{if eq .Outcome "ok"}}ok{{else}}bad{{end}}">{{.Outcome}}
 <td>{{if .Status}}{{.Status}}{{end}}
 <td>{{.DurationMS}}
 <td>{{.QueueWaitMS}}
 <td>{{if .Bytes}}{{.Bytes}}{{end}}
 <td>{{if .PageID}}{{.PageID}}{{end}}
 <td title="{{.URL}}">{{short .URL 70}}
 <td>{{if .Vendor}}{{.Vendor}} {{end}}{{if .Deduped}}deduped {{end}}{{if .AssistedMS}}assist {{.AssistedMS}}ms {{end}}{{short .Error 80}}
</tr>
{{else}}
<tr><td colspan="11">no requests yet</tr>
{{end}}
</table>
`))
