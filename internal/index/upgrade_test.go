package index

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mihai-max/ao3/internal/testutil"
)

// downgrade turns a current index into the schema-1 layout that shipped
// before the sort indexes and counter existed.
func downgrade(t testing.TB, db *DB) {
	t.Helper()
	v1 := `
CREATE TRIGGER works_ai AFTER INSERT ON works BEGIN
  INSERT INTO works_fts(rowid, title, author, fandom, tags, summary) VALUES (new.id, ` + ftsColumns(db.FTSSummary) + `);
END;
CREATE TRIGGER works_ad AFTER DELETE ON works BEGIN
  DELETE FROM works_fts WHERE rowid = old.id;
END;`
	for _, q := range []string{
		`DROP INDEX IF EXISTS works_updated`, `DROP INDEX IF EXISTS works_words`, `DROP INDEX IF EXISTS works_title`,
		`DROP TRIGGER IF EXISTS works_ai`, `DROP TRIGGER IF EXISTS works_ad`,
		v1,
		`UPDATE stats SET n = 0`,
		`UPDATE meta SET value='1' WHERE key='schema'`,
	} {
		if _, err := db.SQL.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

func indexedSample(t *testing.T) *DB {
	t.Helper()
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	if err := testutil.WriteSampleDataDir(data); err != nil {
		t.Fatal(err)
	}
	db, err := Open(filepath.Join(dir, "index", "ao3.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := NewIndexer(db, Options{DataDir: data}).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	return db
}

func quiet() *log.Logger { return log.New(io.Discard, "", 0) }

func TestUpgradeFromV1(t *testing.T) {
	db := indexedSample(t)
	if db.NeedsUpgrade() {
		t.Fatal("a fresh index must not need an upgrade")
	}
	downgrade(t, db)
	if !db.NeedsUpgrade() {
		t.Fatal("downgraded index should need an upgrade")
	}
	// Reopening must not run anything slow or change the old triggers.
	db2, err := Open(db.Path, false)
	if err != nil {
		t.Fatal(err)
	}
	db2.Close()

	st := &UpgradeState{}
	if err := db.Upgrade(context.Background(), st, quiet()); err != nil {
		t.Fatal(err)
	}
	if db.NeedsUpgrade() {
		t.Error("still needs upgrade")
	}
	p := st.Snapshot()
	if !p.Done || p.Fraction != 1 || p.Rows != 50 {
		t.Errorf("progress = %+v", p)
	}
	for _, s := range p.Steps {
		if s.State != "done" {
			t.Errorf("step %q state %s", s.Name, s.State)
		}
	}
	for _, name := range []string{"works_updated", "works_words", "works_title"} {
		var n int
		db.SQL.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='index' AND name=?`, name).Scan(&n)
		if n != 1 {
			t.Errorf("index %s missing", name)
		}
	}
	if got := db.Count(context.Background()); got != 50 {
		t.Errorf("counter = %d, want 50", got)
	}
	// The new triggers keep the counter right.
	if _, err := db.SQL.Exec(`DELETE FROM works WHERE id = 1000`); err != nil {
		t.Fatal(err)
	}
	if got := db.Count(context.Background()); got != 49 {
		t.Errorf("counter after delete = %d, want 49", got)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(db.Path), ".tmp")); !os.IsNotExist(err) {
		t.Errorf("temporary folder left behind: %v", err)
	}
	if line := UpgradeLine(p); line == "" {
		t.Error("empty log line")
	}
}

func TestUpgradeResumes(t *testing.T) {
	db := indexedSample(t)
	downgrade(t, db)

	// A cancelled upgrade keeps the old layout and reports the cancel.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := db.Upgrade(ctx, &UpgradeState{}, quiet()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled upgrade returned %v", err)
	}
	if !db.NeedsUpgrade() {
		t.Fatal("cancelled upgrade must not mark the index upgraded")
	}

	// An index finished before a restart is skipped, not rebuilt.
	if _, err := db.SQL.Exec(`CREATE INDEX works_updated ON works(updated)`); err != nil {
		t.Fatal(err)
	}
	st := &UpgradeState{}
	if err := db.Upgrade(context.Background(), st, quiet()); err != nil {
		t.Fatal(err)
	}
	p := st.Snapshot()
	if p.Steps[1].State != "skipped" || p.Steps[2].State != "done" {
		t.Errorf("steps = %+v", p.Steps)
	}
	if db.NeedsUpgrade() || db.Count(context.Background()) != 50 {
		t.Error("upgrade did not finish")
	}
}

func TestProgressNeverGoesBackwards(t *testing.T) {
	st := &UpgradeState{}
	st.update(func(p *UpgradeProgress) {
		*p = UpgradeProgress{Started: time.Now(), Steps: make([]UpgradeStep, len(upgradeWeights))}
		p.Steps[1].State = "running"
		p.Steps[1].Fraction = 0.8
	})
	before := st.Snapshot().Fraction
	st.update(func(p *UpgradeProgress) { p.Steps[1].Fraction = 0.1 })
	if after := st.Snapshot().Fraction; after < before {
		t.Errorf("fraction went from %f to %f", before, after)
	}
}

// TestUpgradeAtScale downgrades a large index and times the upgrade, printing
// the progress line every second. Point AO3_UPGRADE_DB at a copy of an index.
//
//	AO3_UPGRADE_DB=/tmp/copy.db go test ./internal/index -run UpgradeAtScale -v -timeout 60m
func TestUpgradeAtScale(t *testing.T) {
	path := os.Getenv("AO3_UPGRADE_DB")
	if path == "" {
		t.Skip("set AO3_UPGRADE_DB to a copy of an index to run")
	}
	db, err := Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if kb, _ := strconv.Atoi(os.Getenv("AO3_UPGRADE_CACHE_KB")); kb > 0 {
		upgradeCacheKiB = kb
		t.Logf("upgrade cache set to %d KiB so the sort spills to disk", kb)
	}
	tmpDir := filepath.Join(filepath.Dir(path), ".tmp")
	var maxTemp int64
	var tempSeen []string
	downgrade(t, db)
	db.SQL.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	st := &UpgradeState{}
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- db.Upgrade(context.Background(), st, quiet()) }()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			p := st.Snapshot()
			t.Logf("%s", UpgradeLine(p))
			for _, s := range p.Steps {
				t.Logf("  %-30s %s", s.Name, s.Detail)
			}
			t.Logf("total %s; peak sort temp in index folder %s %v", time.Since(start).Round(time.Second), human(maxTemp), tempSeen)
			return
		case <-tick.C:
			t.Logf("%s", UpgradeLine(st.Snapshot()))
			var sum int64
			ents, _ := os.ReadDir("/proc/self/fd")
			for _, e := range ents {
				target, err := os.Readlink(filepath.Join("/proc/self/fd", e.Name()))
				if err != nil {
					continue
				}
				if strings.HasPrefix(target, "/tmp/etilqs") || strings.HasPrefix(target, "/var/tmp/etilqs") {
					t.Errorf("sort temp file outside the index folder: %s", target)
				}
				if strings.HasPrefix(target, tmpDir) {
					if fi, err := os.Stat(filepath.Join("/proc/self/fd", e.Name())); err == nil {
						sum += fi.Size()
					}
					if len(tempSeen) < 3 {
						tempSeen = append(tempSeen, target)
					}
				}
			}
			maxTemp = max(maxTemp, sum)
		}
	}
}
