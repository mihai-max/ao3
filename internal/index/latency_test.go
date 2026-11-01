package index

import (
	"context"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fillSynthetic writes rows with AO3-like field lengths and a Zipf word
// distribution, so that common words match a large share of works the way
// "love" does in the real archive. It returns the most common word.
func fillSynthetic(t testing.TB, db *DB, rows int) string {
	t.Helper()
	rng := rand.New(rand.NewSource(1))
	zipf := rand.NewZipf(rng, 1.1, 1, 80000)
	vocab := make([]string, 80001)
	for i := range vocab {
		vocab[i] = word(rng, i)
	}
	words := func(n int) string {
		ws := make([]string, n)
		for i := range ws {
			ws[i] = vocab[zipf.Uint64()]
		}
		return strings.Join(ws, " ")
	}
	ctx := context.Background()
	tx, _ := db.SQL.Begin()
	stmt, _ := tx.Prepare(upsertSQL)
	for i := 0; i < rows; i++ {
		r := &Record{
			ID: int64(i*7 + 1), Title: words(2 + rng.Intn(5)), Author: vocab[rng.Intn(80000)] + strconv.Itoa(rng.Intn(999)),
			Fandom: words(3), Rating: "Teen And Up Audiences", Warnings: "No Archive Warnings Apply", Category: "M/M",
			Relationships: words(4), Characters: words(2 + rng.Intn(12)), Freeforms: words(5 + rng.Intn(25)),
			Language: "English", Summary: words(30 + rng.Intn(90)), Words: int64(rng.Intn(200000)), Chapters: "3/10",
			Published: "2019-04-01", Updated: "2021-05-" + strconv.Itoa(10+rng.Intn(18)),
			Container: "things.rar", Zip: "queue17.zip", Member: strconv.Itoa(i*7+1) + ".epub",
			DataOffset: int64(i) * 40000, CompSize: 40000, Size: 40000, ZipRank: 3,
		}
		if err := upsert(ctx, stmt, r); err != nil {
			t.Fatal(err)
		}
		if i%5000 == 4999 {
			stmt.Close()
			tx.Commit()
			tx, _ = db.SQL.Begin()
			stmt, _ = tx.Prepare(upsertSQL)
		}
	}
	stmt.Close()
	tx.Commit()
	return vocab[0]
}

// TestQueryLatencyAtScale times the queries behind the home and search pages
// on a large index. Slow, so it only runs with AO3_LATENCY_ROWS=<rows>.
//
//	AO3_LATENCY_ROWS=1000000 go test ./internal/index -run QueryLatency -v -timeout 30m
func TestQueryLatencyAtScale(t *testing.T) {
	rows, _ := strconv.Atoi(os.Getenv("AO3_LATENCY_ROWS"))
	if rows <= 0 {
		t.Skip("set AO3_LATENCY_ROWS=<rows> to run")
	}
	path := os.Getenv("AO3_LATENCY_DB")
	if path == "" {
		path = filepath.Join(t.TempDir(), "ao3.db")
	}
	db, err := Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	common := "x"
	var have int64
	db.SQL.QueryRow(`SELECT count(*) FROM works`).Scan(&have)
	if have < int64(rows) {
		start := time.Now()
		common = fillSynthetic(t, db, rows)
		t.Logf("filled %d rows in %s, db %.0f MB", rows, time.Since(start).Round(time.Second), float64(db.SizeOnDisk())/1e6)
	} else {
		rng := rand.New(rand.NewSource(1))
		common = word(rng, 0)
	}
	ctx := context.Background()

	timeIt := func(name string, f func() error) {
		start := time.Now()
		if err := f(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		t.Logf("%-28s %s", name, time.Since(start).Round(time.Millisecond))
	}
	timeIt("home: Count", func() error { db.Count(ctx); return nil })
	timeIt("home: Recent", func() error { _, err := db.Recent(ctx, 20); return err })
	for _, term := range append([]string{common}, strings.Fields(os.Getenv("AO3_LATENCY_TERMS"))...) {
		for _, sort := range []string{"", "updated", "words", "title"} {
			sort, term := sort, term
			timeIt("search "+term+" sort="+sort, func() error {
				res, err := db.Search(ctx, SearchParams{Query: term, Field: "any", Sort: sort})
				if err == nil {
					t.Logf("    %d results (capped=%v) sorted by %s fallback=%v, page %d", res.Total, res.Capped, res.Sort, res.SortFallback, len(res.Works))
				}
				return err
			})
		}
	}
	timeIt("search common word, title", func() error {
		_, err := db.Search(ctx, SearchParams{Query: common, Field: "title"})
		return err
	})
	timeIt("search page 50", func() error {
		_, err := db.Search(ctx, SearchParams{Query: common, Field: "any", Offset: 49 * 50})
		return err
	})
}
