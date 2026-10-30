// Package web serves the search UI, the chapter reader and EPUB downloads.
package web

import (
	"bytes"
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/microcosm-cc/bluemonday"

	"github.com/mihai-max/ao3/internal/archive"
	"github.com/mihai-max/ao3/internal/epub"
	"github.com/mihai-max/ao3/internal/index"
	"github.com/mihai-max/ao3/internal/rmount"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// Server is the HTTP front end.
type Server struct {
	DB      *index.DB
	Indexer *index.Indexer
	DataDir string
	Logger  *log.Logger
	// BaseCtx is used for indexing runs started from the status page.
	BaseCtx context.Context
	// Mount is the rclone mount serving DataDir, or nil for a local folder.
	Mount *rmount.Mount

	pages  map[string]*template.Template
	policy *bluemonday.Policy
	cache  *bookCache
}

// New builds a server.
func New(db *index.DB, ix *index.Indexer, dataDir string, logger *log.Logger) (*Server, error) {
	s := &Server{DB: db, Indexer: ix, DataDir: dataDir, Logger: logger, BaseCtx: context.Background(),
		policy: newPolicy(), cache: newBookCache(256 << 20), pages: map[string]*template.Template{}}
	funcs := template.FuncMap{
		"num":     formatNum,
		"bytes":   formatBytes,
		"tags":    splitTags,
		"phrase":  func(s string) string { return `"` + strings.ReplaceAll(s, `"`, " ") + `"` },
		"pct":     pct,
		"list":    func(v ...string) []string { return v },
		"add":     func(a, b int) int { return a + b },
		"sub":     func(a, b int) int { return a - b },
		"summary": summaryHTML,
		"since":   func(t time.Time) string { return time.Since(t).Round(time.Second).String() },
	}
	names, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	for _, n := range names {
		base := path.Base(n)
		if base == "layout.html" {
			continue
		}
		t, err := template.New(base).Funcs(funcs).ParseFS(templateFS, "templates/layout.html", n)
		if err != nil {
			return nil, err
		}
		s.pages[strings.TrimSuffix(base, ".html")] = t
	}
	return s, nil
}

// Handler returns the routed handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheFor(24*time.Hour, http.FileServerFS(static))))
	mux.HandleFunc("GET /{$}", s.home)
	mux.HandleFunc("GET /search", s.search)
	mux.HandleFunc("GET /work/{id}", s.work)
	mux.HandleFunc("GET /work/{id}/ch/{n}", s.chapter)
	mux.HandleFunc("GET /work/{id}/res/{path...}", s.resource)
	mux.HandleFunc("GET /work/{id}/download", s.download)
	mux.HandleFunc("GET /status", s.status)
	mux.HandleFunc("POST /status/scan", s.scan)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.DB.SQL.PingContext(r.Context()); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		if s.Mount != nil && !s.Mount.Mounted() {
			http.Error(w, "remote storage is not mounted", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok\n"))
	})
	return securityHeaders(mux)
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		// Book HTML is untrusted: no scripts at all, nothing loaded off-site.
		hd.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'none'; frame-ancestors 'self'; form-action 'self'; base-uri 'none'")
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "no-referrer")
		h.ServeHTTP(w, r)
	})
}

func cacheFor(d time.Duration, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(d.Seconds())))
		h.ServeHTTP(w, r)
	})
}

type common struct {
	Title    string
	Q        string
	Field    string
	Sort     string
	Indexing bool
}

func (s *Server) common(title string) common {
	return common{Title: title, Field: "any", Indexing: s.Indexer != nil && s.Indexer.Running()}
}

func (s *Server) render(w http.ResponseWriter, status int, page string, data any) {
	t, ok := s.pages[page]
	if !ok {
		http.Error(w, "no template "+page, http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout", data); err != nil {
		s.Logger.Printf("web: render %s: %v", page, err)
		http.Error(w, "page failed to render", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

func (s *Server) fail(w http.ResponseWriter, status int, msg string) {
	s.render(w, status, "error", struct {
		common
		Status  int
		Message string
	}{s.common(http.StatusText(status)), status, msg})
}

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	recent, _ := s.DB.Recent(ctx, 20)
	zips, _ := archive.Discover(s.DataDir)
	prog, _ := s.DB.AllProgress(ctx)
	done := 0
	for _, z := range zips {
		if p, ok := prog[z.Name]; ok && p.Done {
			done++
		}
	}
	s.render(w, http.StatusOK, "home", struct {
		common
		Works    int64
		ZipsDone int
		ZipsAll  int
		DBSize   int64
		Recent   []*index.Record
	}{s.common("Search"), s.DB.Count(ctx), done, len(zips), s.DB.SizeOnDisk(), recent})
}

const pageSize = 50
const maxPage = 200

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	qv := r.URL.Query()
	c := s.common("Search")
	c.Q = strings.TrimSpace(qv.Get("q"))
	c.Field = qv.Get("field")
	if c.Field == "" {
		c.Field = "any"
	}
	c.Sort = qv.Get("sort")
	page, _ := strconv.Atoi(qv.Get("page"))
	page = max(1, min(page, maxPage))
	if c.Q != "" {
		c.Title = c.Q + " · Search"
	}

	// A bare number is probably an AO3 work id.
	var direct *index.Record
	if id, err := strconv.ParseInt(c.Q, 10, 64); err == nil {
		direct, _ = s.DB.Get(r.Context(), id)
	}

	res, err := s.DB.Search(r.Context(), index.SearchParams{Query: c.Q, Field: c.Field, Sort: c.Sort,
		Limit: pageSize, Offset: (page - 1) * pageSize})
	if err != nil {
		s.Logger.Printf("web: search: %v", err)
		s.fail(w, http.StatusBadRequest, "That search could not be run. Try fewer special characters.")
		return
	}
	pages := int((res.Total + pageSize - 1) / pageSize)
	s.render(w, http.StatusOK, "search", struct {
		common
		Res    *index.SearchResult
		Direct *index.Record
		Page   int
		Pages  int
		From   int
	}{c, res, direct, page, min(pages, maxPage), (page-1)*pageSize + 1})
}

func (s *Server) lookup(w http.ResponseWriter, r *http.Request) (*index.Record, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.fail(w, http.StatusNotFound, "No such work.")
		return nil, false
	}
	rec, err := s.DB.Get(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		s.fail(w, http.StatusNotFound, "No such work in the index.")
		return nil, false
	}
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err.Error())
		return nil, false
	}
	return rec, true
}

func (s *Server) loadBook(rec *index.Record) (*epub.Book, error) {
	if b := s.cache.get(rec.ID); b != nil {
		return b, nil
	}
	data, err := archive.ReadMember(s.DataDir, rec.Loc())
	if err != nil {
		return nil, err
	}
	b, err := epub.Open(data)
	if err != nil {
		return nil, err
	}
	s.cache.put(rec.ID, b)
	return b, nil
}

type chapterLink struct {
	N     int
	Title string
}

func (s *Server) work(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.lookup(w, r)
	if !ok {
		return
	}
	var chapters []chapterLink
	var loadErr string
	if b, err := s.loadBook(rec); err != nil {
		loadErr = err.Error()
	} else {
		for i, t := range b.ChapterTitles() {
			chapters = append(chapters, chapterLink{i, t})
		}
	}
	s.render(w, http.StatusOK, "work", struct {
		common
		W        *index.Record
		Chapters []chapterLink
		LoadErr  string
	}{s.common(rec.Title), rec, chapters, loadErr})
}

func (s *Server) chapter(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.lookup(w, r)
	if !ok {
		return
	}
	b, err := s.loadBook(rec)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "Could not open this EPUB: "+err.Error())
		return
	}
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n < 0 || n >= len(b.Spine) {
		s.fail(w, http.StatusNotFound, "No such chapter.")
		return
	}
	body, err := renderChapter(b, rec.ID, n, s.policy)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "Could not render this chapter: "+err.Error())
		return
	}
	titles := b.ChapterTitles()
	var chapters []chapterLink
	for i, t := range titles {
		chapters = append(chapters, chapterLink{i, t})
	}
	s.render(w, http.StatusOK, "chapter", struct {
		common
		W        *index.Record
		N        int
		Last     int
		Heading  string
		Body     template.HTML
		Chapters []chapterLink
	}{s.common(titles[n] + " · " + rec.Title), rec, n, len(b.Spine) - 1, titles[n], body, chapters})
}

func (s *Server) resource(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.lookup(w, r)
	if !ok {
		return
	}
	b, err := s.loadBook(rec)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	p := r.PathValue("path")
	if !b.Has(p) {
		http.NotFound(w, r)
		return
	}
	ct := b.MediaType(p)
	if ct == "" {
		ct = mime.TypeByExtension(path.Ext(p))
	}
	// Only serve images and fonts; book HTML must go through the sanitizer.
	if !strings.HasPrefix(ct, "image/") || strings.Contains(ct, "svg") {
		http.Error(w, "not an image", http.StatusForbidden)
		return
	}
	data, err := b.ReadFile(p)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(data)
}

func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.lookup(w, r)
	if !ok {
		return
	}
	var data []byte
	if b := s.cache.get(rec.ID); b != nil {
		data = b.Data
	} else {
		var err error
		if data, err = archive.ReadMember(s.DataDir, rec.Loc()); err != nil {
			s.fail(w, http.StatusInternalServerError, "Could not read this EPUB: "+err.Error())
			return
		}
	}
	name := downloadName(rec)
	w.Header().Set("Content-Type", "application/epub+zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`,
		asciiName(name), url.PathEscape(name)))
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Write(data)
}

func downloadName(rec *index.Record) string {
	name := rec.Title
	if rec.Author != "" {
		name += " - " + rec.Author
	}
	name = strings.Map(func(r rune) rune {
		if strings.ContainsRune(`/\:*?"<>|`, r) || unicode.IsControl(r) {
			return '_'
		}
		return r
	}, name)
	if len([]rune(name)) > 150 {
		name = string([]rune(name)[:150])
	}
	return strings.TrimSpace(name) + ".epub"
}

func asciiName(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' {
			return '_'
		}
		return r
	}, s)
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st := s.Indexer.Status()
	layout, _ := archive.Scan(s.DataDir)
	if len(st.Zips) == 0 {
		// Not run yet in this process: show what the database knows.
		prog, _ := s.DB.AllProgress(ctx)
		for _, z := range layout.Zips {
			zs := index.ZipStatus{Name: z.Name, Container: z.Container, Size: z.Size, State: "pending"}
			if p, ok := prog[z.Name]; ok {
				zs.Total, zs.Done, zs.EPUBs, zs.Errors = p.MembersTotal, p.MembersDone, p.EPUBs, p.Errors
				if p.Done && p.Size == z.Size {
					zs.State = "done"
				}
			}
			st.Zips = append(st.Zips, zs)
		}
	}
	var totalSize, doneSize int64
	for _, z := range st.Zips {
		totalSize += z.Size
		switch {
		case z.State == "done":
			doneSize += z.Size
		case z.Total > 0:
			doneSize += z.Size * z.Done / z.Total
		}
	}
	s.render(w, http.StatusOK, "status", struct {
		common
		St        index.Status
		TotalSize int64
		DoneSize  int64
		Works     int64
		DBSize    int64
		DBPath    string
		DataDir   string
		Notes     map[string]string
		FTSSum    bool
		Mount     *rmount.State
		Warnings  []string
	}{s.common("Index status"), st, totalSize, doneSize, s.DB.Count(ctx), s.DB.SizeOnDisk(), s.DB.Path,
		s.DataDir, archive.Notes(s.DataDir), s.DB.FTSSummary, mountState(s.Mount), layout.Warnings})
}

func mountState(m *rmount.Mount) *rmount.State {
	if m == nil {
		return nil
	}
	st := m.State()
	return &st
}

func (s *Server) scan(w http.ResponseWriter, r *http.Request) {
	if !s.Indexer.Running() {
		go func() {
			if err := s.Indexer.Run(s.BaseCtx); err != nil && !errors.Is(err, context.Canceled) {
				s.Logger.Printf("index: %v", err)
			}
		}()
	}
	http.Redirect(w, r, "/status", http.StatusSeeOther)
}

func splitTags(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ", ")
}

func summaryHTML(s string) template.HTML {
	var b strings.Builder
	for _, p := range strings.Split(s, "\n\n") {
		if p = strings.TrimSpace(p); p != "" {
			b.WriteString("<p>")
			b.WriteString(template.HTMLEscapeString(p))
			b.WriteString("</p>")
		}
	}
	return template.HTML(b.String())
}

func formatNum(v any) string {
	var n int64
	switch x := v.(type) {
	case int:
		n = int64(x)
	case int64:
		n = x
	default:
		return fmt.Sprint(v)
	}
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var out []byte
	for i := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, s[i])
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func pct(a, b int64) string {
	if b <= 0 {
		return "0"
	}
	return strconv.FormatFloat(float64(a)*100/float64(b), 'f', 1, 64)
}
