package web

import (
	"bytes"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/mihai-max/ao3/internal/index"
)

// Startup is what the server is doing before the site is ready.
type Startup struct {
	Stage   string // e.g. "Mounting remote storage"
	Upgrade *index.UpgradeProgress
	Err     string
}

// Switch is an http.Handler whose target can be replaced while serving, so
// the port answers from the first second and moves from the startup page to
// the full site when it is ready.
type Switch struct{ h atomic.Value }

// Set replaces the handler.
func (s *Switch) Set(h http.Handler) { s.h.Store(&h) }

func (s *Switch) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h, ok := s.h.Load().(*http.Handler); ok {
		(*h).ServeHTTP(w, r)
		return
	}
	http.Error(w, "starting", http.StatusServiceUnavailable)
}

var startupTmpl = template.Must(template.New("startup").Funcs(template.FuncMap{
	"pct": func(f float64) string { return fmt.Sprintf("%.0f", f*100) },
	"dur": func(d time.Duration) string {
		d = d.Round(time.Minute)
		if d < time.Minute {
			return "less than a minute"
		}
		h, m := int(d.Hours()), int(d.Minutes())%60
		if h == 0 {
			return fmt.Sprintf("%d min", m)
		}
		return fmt.Sprintf("%d h %d min", h, m)
	},
	"since": func(t time.Time) time.Duration { return time.Since(t) },
	"lower": strings.ToLower,
	"add":   func(a, b int) int { return a + b },
	"mulf":  func(f float64) float64 { return f * 1000 },
	"num":   formatNum,
}).Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="refresh" content="5">
<title>{{if .Upgrade}}{{pct .Upgrade.Fraction}}% · {{end}}Starting · AO3 Archive</title>
<link rel="stylesheet" href="/static/style.css">
</head><body>
<header class="top"><span class="brand">AO3 Archive</span></header>
<main class="startup">
{{if .Err}}
  <h1>Startup failed</h1>
  <p class="warn">{{.Err}}</p>
  <p class="muted">The container will exit and Docker restarts it. Check the container log for details.</p>
{{else if .Upgrade}}{{with .Upgrade}}
  <h1>Upgrading the index</h1>
  <p class="muted">This happens once after an update. Search opens automatically when it is done.
  It is safe to stop the container; finished steps are kept.</p>
  <progress max="1000" value="{{printf "%.0f" (mulf .Fraction)}}"></progress>
  <p class="bigpct"><strong>{{pct .Fraction}}%</strong>
    {{if .ETA}} · about {{dur .ETA}} left{{end}} · running for {{dur (since .Started)}}</p>
  <ol class="steps">
  {{range $i, $s := .Steps}}
    <li class="step-{{$s.State}}">
      <span class="mark">{{if eq $s.State "done"}}✓{{else if eq $s.State "skipped"}}✓{{else if eq $s.State "running"}}▸{{else}}·{{end}}</span>
      {{$s.Name}}{{if eq $s.State "running"}} — {{pct $s.Fraction}}%{{end}}
      {{if $s.Detail}}<span class="detail muted">{{$s.Detail}}</span>{{end}}
    </li>
  {{end}}
  </ol>
  {{if .Rows}}<p class="muted">{{num .Rows}} works in the index.</p>{{end}}
{{end}}{{else}}
  <h1>Starting</h1>
  <p>{{.Stage}}…</p>
  <p class="muted">This page refreshes by itself.</p>
{{end}}
</main></body></html>`))

// StartupHandler serves the startup page for every path, plus static files
// and a health check that reports healthy so Docker does not flag a long
// upgrade as a failure.
func StartupHandler(state func() Startup) http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		st := state()
		if st.Err != "" {
			http.Error(w, st.Err, http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintf(w, "starting: %s\n", st.Stage)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		if err := startupTmpl.Execute(&buf, state()); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(buf.Bytes())
	})
	return securityHeaders(mux)
}
