package web_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mihai-max/ao3/internal/index"
	"github.com/mihai-max/ao3/internal/web"
)

func TestStartupPageShowsUpgradeProgress(t *testing.T) {
	p := index.UpgradeProgress{
		Started:  time.Now().Add(-25 * time.Minute),
		Fraction: 0.437,
		ETA:      32 * time.Minute,
		Current:  2,
		Rows:     80_123_456,
		Steps: []index.UpgradeStep{
			{Name: "Counting works", State: "done", Fraction: 1, Detail: "80,123,456 works"},
			{Name: "Building the date sort index", State: "done", Fraction: 1, Detail: "built in 21m"},
			{Name: "Building the length sort index", State: "running", Fraction: 0.31, Detail: "reading works: 27.9 GiB of about 90.0 GiB"},
			{Name: "Building the title sort index", State: "pending"},
			{Name: "Saving", State: "pending"},
		},
	}
	sw := &web.Switch{}
	sw.Set(web.StartupHandler(func() web.Startup { return web.Startup{Stage: "Upgrading the index", Upgrade: &p} }))
	ts := httptest.NewServer(sw)
	defer ts.Close()

	res, err := http.Get(ts.URL + "/search?q=anything")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Errorf("status %d", res.StatusCode)
	}
	for _, want := range []string{
		`<progress max="1000" value="437">`, "44%", "about 32 min left", "running for 25 min",
		"Building the length sort index — 31%", "reading works: 27.9 GiB of about 90.0 GiB",
		"80,123,456 works in the index", `http-equiv="refresh"`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("startup page missing %q", want)
		}
	}

	res, _ = http.Get(ts.URL + "/healthz")
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || !strings.Contains(string(b), "starting") {
		t.Errorf("healthz during startup: %d %q", res.StatusCode, b)
	}
	res, _ = http.Get(ts.URL + "/static/style.css")
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Errorf("static during startup: %d", res.StatusCode)
	}

	// Switching hands every new request to the real site.
	sw.Set(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "site") }))
	res, _ = http.Get(ts.URL + "/")
	b, _ = io.ReadAll(res.Body)
	res.Body.Close()
	if string(b) != "site" {
		t.Errorf("after switch got %q", b)
	}
}

func TestStartupPageStages(t *testing.T) {
	h := web.StartupHandler(func() web.Startup { return web.Startup{Stage: "Mounting remote storage"} })
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if !strings.Contains(rec.Body.String(), "Mounting remote storage") {
		t.Errorf("stage not shown: %s", rec.Body.String())
	}
}
