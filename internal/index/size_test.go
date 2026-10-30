package index

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestIndexSizeEstimate measures index bytes per work with AO3-like field
// lengths and a Zipf word distribution, to extrapolate the size for the full
// archive. It is slow, so it only runs with AO3_SIZE_BENCH=<rows>.
//
//	AO3_SIZE_BENCH=300000 go test ./internal/index -run SizeEstimate -v
func TestIndexSizeEstimate(t *testing.T) {
	rows, _ := strconv.Atoi(os.Getenv("AO3_SIZE_BENCH"))
	if rows <= 0 {
		t.Skip("set AO3_SIZE_BENCH=<rows> to run")
	}
	for _, summary := range []bool{false, true} {
		t.Run(fmt.Sprintf("fts_summary=%v", summary), func(t *testing.T) {
			db, err := Open(filepath.Join(t.TempDir(), "ao3.db"), summary)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
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
			fandoms := make([]string, 3000)
			for i := range fandoms {
				fandoms[i] = words(3) + " (TV)"
			}
			start := time.Now()
			ctx := context.Background()
			tx, _ := db.SQL.Begin()
			stmt, _ := tx.Prepare(upsertSQL)
			for i := 0; i < rows; i++ {
				r := &Record{
					ID: int64(i*7 + 1), Title: words(2 + rng.Intn(5)), Author: vocab[rng.Intn(80000)] + strconv.Itoa(rng.Intn(999)),
					Fandom: fandoms[int(zipf.Uint64())%len(fandoms)], Rating: "Teen And Up Audiences",
					Warnings: "No Archive Warnings Apply", Category: "M/M",
					Relationships: words(4) + "/" + words(2), Characters: words(2 + rng.Intn(12)),
					Freeforms: words(5 + rng.Intn(25)), Language: "English", Summary: words(30 + rng.Intn(90)),
					Words: int64(rng.Intn(200000)), Chapters: "3/10", Published: "2019-04-01", Updated: "2021-05-02",
					Zip: "queue17.zip", Member: strconv.Itoa(i*7+1) + ".epub", DataOffset: int64(i) * 40000,
					CompSize: 40000, Size: 40000, Method: 0, ZipRank: 3,
				}
				if err := upsert(ctx, stmt, r); err != nil {
					t.Fatal(err)
				}
				if i%2000 == 1999 {
					stmt.Close()
					tx.Commit()
					tx, _ = db.SQL.Begin()
					stmt, _ = tx.Prepare(upsertSQL)
				}
			}
			stmt.Close()
			tx.Commit()
			elapsed := time.Since(start)
			db.SQL.Exec(`INSERT INTO works_fts(works_fts) VALUES('optimize')`)
			db.SQL.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
			size := db.SizeOnDisk()
			per := float64(size) / float64(rows)
			t.Logf("%d rows: %.1f MB, %.0f bytes/work, %.0f rows/s insert -> 20M works ≈ %.1f GB",
				rows, float64(size)/1e6, per, float64(rows)/elapsed.Seconds(), per*20e6/1e9)
		})
	}
}

func word(rng *rand.Rand, i int) string {
	const letters = "etaoinshrdlucmfwypvbgkjqxz"
	n := 3 + rng.Intn(7)
	b := make([]byte, n)
	for j := range b {
		b[j] = letters[(i*31+j*7+rng.Intn(26))%26]
	}
	return string(b)
}
