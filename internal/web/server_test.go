package web_test

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mihai-max/ao3/internal/epub"
	"github.com/mihai-max/ao3/internal/index"
	"github.com/mihai-max/ao3/internal/testutil"
	"github.com/mihai-max/ao3/internal/web"
)

func newServer(t *testing.T) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	if err := testutil.WriteSampleDataDir(data); err != nil {
		t.Fatal(err)
	}
	db, err := index.Open(filepath.Join(dir, "index", "ao3.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ix := index.NewIndexer(db, index.Options{DataDir: data})
	if err := ix.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	s, err := web.New(db, ix, data, log.New(os.Stderr, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func get(t *testing.T, ts *httptest.Server, path string) (*http.Response, string) {
	t.Helper()
	res, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, string(b)
}

func expect(t *testing.T, body string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(body, w) {
			t.Errorf("body missing %q", w)
		}
	}
}

func TestPages(t *testing.T) {
	ts := newServer(t)

	res, body := get(t, ts, "/")
	if res.StatusCode != 200 {
		t.Fatalf("home: %d", res.StatusCode)
	}
	expect(t, body, "50 works indexed", "2 of 2 archives indexed")
	if csp := res.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'none'") {
		t.Errorf("CSP = %q", csp)
	}

	_, body = get(t, ts, "/search?q=coffee+shop&field=title")
	expect(t, body, "5 results", "Coffee Shop Encounters 1")

	_, body = get(t, ts, "/search?q=1000")
	expect(t, body, "Work id 1000", "The Long Way Home (Revised)")

	_, body = get(t, ts, "/search?q=%22unterminated")
	expect(t, body, "results for")

	_, body = get(t, ts, "/work/1010")
	expect(t, body, "The Long Way Home 10", "writer03", "Harry Potter - J. K. Rowling", "Download EPUB", "Chapter 1",
		`/search?field=tags&q=%22Fluff%22`)

	res, body = get(t, ts, "/work/1010/ch/1")
	if res.StatusCode != 200 {
		t.Fatalf("chapter: %d %s", res.StatusCode, body)
	}
	expect(t, body, "Chapter 1 of work 1010", `src="/work/1010/res/OEBPS/images/pic.png"`, "Chapter 2 →")
	if strings.Contains(body, "alert(") || strings.Contains(body, "onclick") || strings.Contains(body, "evil()") {
		t.Error("chapter HTML was not sanitized")
	}

	// Internal links between chapters are rewritten to reader URLs.
	_, body = get(t, ts, "/work/1011/ch/1")
	expect(t, body, `href="/work/1011/ch/2#top"`)

	res, _ = get(t, ts, "/work/1010/res/OEBPS/images/pic.png")
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "image/png" {
		t.Errorf("image: %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	res, _ = get(t, ts, "/work/1010/res/OEBPS/chapter_001.xhtml")
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("raw xhtml resource should be refused, got %d", res.StatusCode)
	}

	res, body = get(t, ts, "/work/1000/download")
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "application/epub+zip" {
		t.Fatalf("download: %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	cd := res.Header.Get("Content-Disposition")
	if !strings.Contains(cd, "attachment") || !strings.Contains(cd, "The Long Way Home (Revised) - writer00.epub") {
		t.Errorf("Content-Disposition = %q", cd)
	}
	if b, err := epub.Open([]byte(body)); err != nil || b.WorkMeta().WorkID != 1000 {
		t.Errorf("downloaded file is not the right EPUB: %v", err)
	}

	_, body = get(t, ts, "/status")
	expect(t, body, "queue01.zip", "jan_may_2023.zip", "zip_order.txt", "Scan for new")

	for _, p := range []string{"/work/999999", "/work/abc", "/work/1000/ch/99", "/nope"} {
		if res, _ := get(t, ts, p); res.StatusCode != 404 {
			t.Errorf("%s: %d, want 404", p, res.StatusCode)
		}
	}
	if res, body := get(t, ts, "/healthz"); res.StatusCode != 200 || body != "ok\n" {
		t.Errorf("healthz: %d %q", res.StatusCode, body)
	}
}
