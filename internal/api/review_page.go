package api

import (
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/eordano/cccp/internal/syncclass"
)

var reviewFuncs = template.FuncMap{
	"when": func(ns int64) string {
		if ns == 0 {
			return ""
		}
		return time.Unix(0, ns).UTC().Format("2006-01-02 15:04 UTC")
	},
	"short": func(s string) string {
		if len(s) > 12 {
			return s[:12]
		}
		return s
	},
	"showURL": func(path string, v int64) string {
		return "/review/show?path=" + url.QueryEscape(path) + "&version=" + strconv.FormatInt(v, 10)
	},
	"diffClass": func(line string) string {
		switch {
		case strings.HasPrefix(line, "@@"):
			return "hunk"
		case strings.HasPrefix(line, "+"):
			return "add"
		case strings.HasPrefix(line, "-"):
			return "del"
		}
		return ""
	},
	"lines": func(s string) []string { return strings.Split(strings.TrimSuffix(s, "\n"), "\n") },
}

const reviewCSS = `
:root { color-scheme: dark; --bg:#0a0a0b; --panel:#141416; --line:#2a2a2e; --fg:#e0e0e0; --dim:#8a8a90; --add:#16351f; --del:#3a1618; --accent:#2563eb; --warn:#d97706; }
body { background:var(--bg); color:var(--fg); font:14px/1.45 system-ui, sans-serif; margin:0; padding:1rem; }
main { max-width:1100px; margin:0 auto; }
a { color:#7aa2ff; }
h1 { font-size:1.2rem; } h2 { font-size:1rem; margin-top:1.5rem; }
table { width:100%; border-collapse:collapse; } td, th { text-align:left; padding:.35rem .5rem; border-bottom:1px solid var(--line); vertical-align:top; }
th { color:var(--dim); font-weight:500; } code, pre { font-family:ui-monospace, monospace; font-size:12.5px; }
.badge { display:inline-block; padding:0 .4rem; border-radius:3px; border:1px solid var(--line); color:var(--dim); font-size:12px; }
.pending { color:var(--warn); border-color:var(--warn); } .approved { color:#4ade80; border-color:#4ade80; } .rejected { color:#f87171; border-color:#f87171; }
pre.diff { background:var(--panel); border:1px solid var(--line); padding:.5rem; overflow-x:auto; }
pre.diff span { display:block; white-space:pre; } .add { background:var(--add); } .del { background:var(--del); } .hunk { color:var(--dim); }
form.decide { display:flex; gap:.5rem; flex-wrap:wrap; margin:1rem 0; } input[type=text] { flex:1; min-width:12rem; background:#1a1a1e; color:var(--fg); border:1px solid #3a3a3e; border-radius:4px; padding:.4rem; }
button { padding:.4rem .9rem; border:none; border-radius:4px; color:#fff; cursor:pointer; } .approve { background:#15803d; } .reject { background:#b91c1c; } .rollback { background:var(--accent); }
.dim { color:var(--dim); } .wrap { overflow-x:auto; }
`

var reviewListTmpl = template.Must(template.New("list").Funcs(reviewFuncs).Parse(`<!DOCTYPE html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>cccp review</title><style>` + reviewCSS + `</style></head><body><main>
<h1>Review queue</h1>
<p class="dim">Agent-written knowledge files (memory, CLAUDE.md, AGENTS.md, skill and command prose) reach other hosts only after approval here or with <code>cccp review approve</code>. Privileged files (hooks, settings, plugin/MCP config, rules, scripts) are archived and never applied from cccp, and so is knowledge in an agent's own space (<code>agent/&lt;agent&gt;-&lt;space&gt;/</code>), which only its own home pulls. <a href="/">sessions</a> · <a href="/review{{if not .All}}?all=1{{end}}">{{if .All}}pending only{{else}}show everything since the gate{{end}}</a></p>
<div class="wrap"><table>
<tr><th>path</th><th>v</th><th>status</th><th>class</th><th>host / agent</th><th>pushed</th><th>pre-check</th></tr>
{{range .Items}}<tr>
<td><a href="{{showURL .Path .Version}}"><code>{{.Path}}</code></a>{{if .Deleted}} <span class="badge">delete</span>{{end}}{{if .Superseded}} <span class="badge">superseded</span>{{end}}</td>
<td>{{.Version}}</td>
<td><span class="badge {{.Status}}">{{.Status}}</span></td>
<td>{{.Class}}</td>
<td>{{if .Host}}{{.Host}}{{else}}{{short .HostID}}{{end}}{{if .Agent}} / {{.Agent}}{{end}}{{if .Sessions}}<br><span class="dim">{{.Sessions}}</span>{{end}}</td>
<td>{{when .IngestedAt}}</td>
<td>{{with .Precheck}}<span class="badge">{{.Verdict}}</span>{{end}}</td>
</tr>{{else}}<tr><td colspan="7" class="dim">Nothing waiting.</td></tr>{{end}}
</table></div>
</main></body></html>`))

var reviewShowTmpl = template.Must(template.New("show").Funcs(reviewFuncs).Parse(`<!DOCTYPE html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>cccp review</title><style>` + reviewCSS + `</style></head><body><main>
<p><a href="/review">&larr; review queue</a></p>
{{with .D.Item}}<h1><code>{{.Path}}</code> v{{.Version}} <span class="badge {{.Status}}">{{.Status}}</span></h1>
<p class="dim">{{.Class}} · from {{if .Host}}{{.Host}}{{else}}{{.HostID}}{{end}}{{if .Agent}} ({{.Agent}}){{end}} · pushed {{when .IngestedAt}}{{if .Sessions}} · sessions {{.Sessions}}{{end}}{{if .Effective}} · serving v{{.Effective}}{{else}} · nothing approved yet{{end}}</p>
{{with .Precheck}}<p>Pre-check: <span class="badge">{{.Verdict}}</span></p><pre class="diff">{{.Detail}}</pre>{{end}}{{end}}
{{if .Reviewable}}<form class="decide" method="POST" action="/review/decide">
<input type="hidden" name="path" value="{{.D.Item.Path}}"><input type="hidden" name="version" value="{{.D.Item.Version}}">
<input type="text" name="note" placeholder="note (optional)">
<button class="approve" name="decision" value="approved">{{if lt .D.Item.Version .D.Item.Effective}}Approve (roll back to v{{.D.Item.Version}}){{else}}Approve v{{.D.Item.Version}}{{end}}</button>
<button class="reject" name="decision" value="rejected">Reject</button>
</form>{{else if .OwnSpace}}<p class="dim">Knowledge in an agent's own space is archived, not reviewed: only the home that writes the space pulls it.</p>{{else}}<p class="dim">Privileged files are never applied from cccp; change them in the repo.</p>{{end}}
<h2>Diff against v{{.D.DiffBase}}</h2>
{{if .D.Binary}}<p class="dim">binary content</p>{{else if .D.Diff}}<pre class="diff">{{range lines .D.Diff}}<span class="{{diffClass .}}">{{.}}</span>{{end}}</pre>{{else}}<p class="dim">no change</p>{{end}}
<h2>Content</h2>
{{if .D.Item.Deleted}}<p class="dim">This version deletes the file.</p>{{else}}<pre class="diff">{{.D.Content}}</pre>{{end}}
<h2>History</h2>
<div class="wrap"><table><tr><th>v</th><th>status</th><th>host</th><th>pushed</th><th>sha</th></tr>
{{range .D.History}}<tr><td><a href="{{showURL .Path .Version}}">v{{.Version}}</a></td><td><span class="badge {{.Status}}">{{.Status}}</span>{{if eq .Version .Effective}} <span class="badge approved">serving</span>{{end}}</td><td>{{if .Host}}{{.Host}}{{else}}{{short .HostID}}{{end}}</td><td>{{when .IngestedAt}}</td><td><code>{{short .SHA}}</code></td></tr>{{end}}
</table></div>
{{if .D.Decisions}}<h2>Decisions</h2><div class="wrap"><table><tr><th>v</th><th>decision</th><th>by</th><th>when</th><th>note</th></tr>
{{range .D.Decisions}}<tr><td>v{{.Version}}</td><td>{{.Decision}}</td><td>{{.Reviewer}}</td><td>{{when .DecidedAt}}</td><td>{{.Note}}</td></tr>{{end}}</table></div>{{end}}
</main></body></html>`))

func (s *Server) handleReviewPage(w http.ResponseWriter, r *http.Request) {
	all := r.URL.Query().Get("all") == "1"
	items, err := s.reviewQueue(r.Context(), all)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := reviewListTmpl.Execute(w, map[string]any{"Items": items, "All": all}); err != nil {
		s.Log.Warn("review page", "err", err)
	}
}

func (s *Server) handleReviewShowPage(w http.ResponseWriter, r *http.Request) {
	path, v, err := parsePathVersion(r)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	d, err := s.reviewDetail(r.Context(), path, v)
	if err != nil {
		httpError(w, reviewStatus(err), err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := reviewShowTmpl.Execute(w, map[string]any{
		"D":          d,
		"Reviewable": syncclass.Reviewable(d.Item.Path, d.Item.Mode) && !readOnly(r.Context()),
		"OwnSpace":   d.Item.Class == "knowledge" && syncclass.OwnSpace(d.Item.Path),
	}); err != nil {
		s.Log.Warn("review page", "err", err)
	}
}

func (s *Server) handleReviewDecideForm(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	v, err := strconv.ParseInt(r.FormValue("version"), 10, 64)
	if err != nil {
		httpError(w, http.StatusBadRequest, "version must be an integer")
		return
	}
	req := decideRequest{
		Path: r.FormValue("path"), Version: v, Decision: r.FormValue("decision"),
		Note: r.FormValue("note"), Reviewer: "pravda@" + clientAddr(r),
	}
	if err := s.decide(r.Context(), req); err != nil {
		httpError(w, reviewStatus(err), err.Error())
		return
	}
	http.Redirect(w, r, "/review/show?path="+url.QueryEscape(req.Path)+"&version="+strconv.FormatInt(v, 10), http.StatusSeeOther)
}

func clientAddr(r *http.Request) string {
	if f := r.Header.Get("X-Forwarded-For"); f != "" {
		return strings.TrimSpace(strings.Split(f, ",")[0])
	}
	return r.RemoteAddr
}
