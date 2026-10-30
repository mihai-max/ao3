package index_test

import (
	"archive/zip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mihai-max/ao3/internal/archive"
	"github.com/mihai-max/ao3/internal/epub"
	"github.com/mihai-max/ao3/internal/index"
	"github.com/mihai-max/ao3/internal/testutil"
)

func setup(t *testing.T) (string, *index.DB) {
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
	return data, db
}

func TestIndexSearchAndRead(t *testing.T) {
	data, db := setup(t)
	ctx := context.Background()
	ix := index.NewIndexer(db, index.Options{DataDir: data, Workers: 4})
	if err := ix.Run(ctx); err != nil {
		t.Fatal(err)
	}
	st := ix.Status()
	if st.Indexed != 51 { // 30 + 20 + 1 updated duplicate
		t.Errorf("indexed %d, want 51", st.Indexed)
	}
	for _, z := range st.Zips {
		if z.State != "done" || z.Errors != 0 {
			t.Errorf("zip %+v", z)
		}
	}
	if n := db.Count(ctx); n != 50 {
		t.Errorf("count = %d, want 50 (duplicate should be merged)", n)
	}

	// The newer copy of work 1000 from the later zip wins.
	r, err := db.Get(ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if r.Title != "The Long Way Home (Revised)" || r.Zip != "jan_may_2023.zip" {
		t.Errorf("dedupe picked %q from %s", r.Title, r.Zip)
	}

	// The stored offset lets us read the EPUB back without the central directory.
	raw, err := archive.ReadMember(data, r.Loc())
	if err != nil {
		t.Fatal(err)
	}
	b, err := epub.Open(raw)
	if err != nil {
		t.Fatal(err)
	}
	if b.WorkMeta().Title != r.Title {
		t.Errorf("read back %q", b.WorkMeta().Title)
	}
	// Stored (method 0) member too.
	r2, _ := db.Get(ctx, 1001)
	if r2.Method != 0 {
		t.Errorf("work 1001 method = %d, want store", r2.Method)
	}
	if _, err := archive.ReadMember(data, r2.Loc()); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		q, field string
		min, max int64
	}{
		{"long way home", "title", 5, 5},
		{"cafe", "title", 5, 5},              // diacritics folded: Café
		{"déjà", "any", 5, 5},                // and the other way round
		{"\"Good Omens\"", "fandom", 10, 10}, // phrase
		{"hurt", "tags", 16, 17},             // Hurt/Comfort
		{"writer03", "author", 7, 7},
		{"ineff*", "title", 5, 5},                 // prefix
		{"home -revised", "title", 4, 4},          // exclusion
		{"somebody", "summary", 0, 0},             // summaries not in FTS by default
		{`"); DROP TABLE works; --`, "any", 0, 0}, // hostile input is just text
	}
	for _, c := range cases {
		res, err := db.Search(ctx, index.SearchParams{Query: c.q, Field: c.field})
		if err != nil {
			t.Errorf("%q: %v", c.q, err)
			continue
		}
		if res.Total < c.min || res.Total > c.max {
			t.Errorf("search %q in %s: %d results, want %d..%d", c.q, c.field, res.Total, c.min, c.max)
		}
	}

	// A second run is a no-op because every zip is recorded as done.
	ix2 := index.NewIndexer(db, index.Options{DataDir: data})
	if err := ix2.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if ix2.Status().Indexed != 0 {
		t.Errorf("second run re-indexed %d", ix2.Status().Indexed)
	}
}

func TestFTSSummaryOption(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	testutil.WriteSampleDataDir(data)
	db, err := index.Open(filepath.Join(dir, "ao3.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := index.NewIndexer(db, index.Options{DataDir: data}).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	res, err := db.Search(context.Background(), index.SearchParams{Query: "somebody", Field: "summary"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 50 {
		t.Errorf("summary search: %d", res.Total)
	}
}

func TestBuildMatch(t *testing.T) {
	cases := map[string]string{
		`harry potter`:   `"harry" AND "potter"`,
		`"harry potter"`: `"harry potter"`,
		`drarry -angst`:  `"drarry" NOT "angst"`,
		`-angst`:         ``,
		`fix*`:           `"fix"*`,
		`a"b`:            `"a""b"`,
		`   `:            ``,
	}
	for in, want := range cases {
		if got := index.BuildMatch(in, "any"); got != want {
			t.Errorf("BuildMatch(%q) = %q, want %q", in, got, want)
		}
	}
	if got := index.BuildMatch("x", "title"); got != `title : "x"` {
		t.Errorf("column filter: %q", got)
	}
	if got := index.BuildMatch("x", "bogus"); got != `"x"` {
		t.Errorf("unknown field must not be passed through: %q", got)
	}
}

func TestInterruptAndResume(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	os.MkdirAll(data, 0o755)
	var works []testutil.FakeWork
	for i := 0; i < 40; i++ {
		works = append(works, testutil.Sample()...)
		for j := len(works) - 50; j < len(works); j++ {
			works[j].ID = int64(10000 + j)
		}
	}
	if err := testutil.WriteArchive(filepath.Join(data, "queue01.zip"), works, zip.Store, nil); err != nil {
		t.Fatal(err)
	}
	db, err := index.Open(filepath.Join(dir, "ao3.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ctx, cancel := context.WithCancel(context.Background())
	ix := index.NewIndexer(db, index.Options{DataDir: data, Workers: 2})
	go func() {
		for ix.Status().Indexed < 200 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	if err := ix.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("first run: %v", err)
	}
	prog, _ := db.AllProgress(context.Background())
	if p := prog["queue01.zip"]; p.Done || p.MembersDone == 0 {
		t.Fatalf("after interrupt progress = %+v, want partial", p)
	}

	ix2 := index.NewIndexer(db, index.Options{DataDir: data})
	if err := ix2.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	prog, _ = db.AllProgress(context.Background())
	if p := prog["queue01.zip"]; !p.Done || p.EPUBs != 2000 {
		t.Fatalf("after resume progress = %+v", p)
	}
	var n int64
	db.SQL.QueryRow(`SELECT count(*) FROM works`).Scan(&n)
	if n != 2000 {
		t.Errorf("works = %d, want 2000", n)
	}
	// The FTS index must match the table exactly (no orphans from re-upserts).
	var fts int64
	db.SQL.QueryRow(`SELECT count(*) FROM works_fts WHERE works_fts MATCH 'author : "writer"*'`).Scan(&fts)
	if fts != 2000 {
		t.Errorf("fts rows matching = %d, want 2000", fts)
	}
}
