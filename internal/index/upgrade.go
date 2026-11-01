package index

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// An index built by an older version is upgraded in resumable steps, each
// committed on its own, so a restart only redoes the step that was running.
// Progress is measured, not guessed: bytes read from the index file while a
// step scans the works table, bytes appended to the WAL while it writes the
// new index, and bytes written while the WAL is copied back at the end.

// UpgradeStep is one step of the upgrade as shown to the user.
type UpgradeStep struct {
	Name     string
	State    string  // pending, running, done, skipped
	Fraction float64 // progress within the step, 0..1
	Detail   string
}

// UpgradeProgress is a snapshot of a running upgrade.
type UpgradeProgress struct {
	Steps    []UpgradeStep
	Current  int     // index into Steps
	Fraction float64 // overall, 0..1
	Rows     int64
	Started  time.Time
	ETA      time.Duration // 0 when not yet known
	Done     bool
	Err      string
}

// UpgradeState is shared between the upgrade and the status page.
type UpgradeState struct {
	mu sync.Mutex
	p  UpgradeProgress
}

// Snapshot returns a copy of the current progress.
func (s *UpgradeState) Snapshot() UpgradeProgress {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.p
	p.Steps = append([]UpgradeStep(nil), s.p.Steps...)
	return p
}

func (s *UpgradeState) update(f func(p *UpgradeProgress)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(&s.p)
	// Overall progress, never moving backwards.
	var total, done float64
	for i, st := range s.p.Steps {
		w := upgradeWeights[i]
		total += w
		switch st.State {
		case "done", "skipped":
			done += w
		case "running":
			done += w * st.Fraction
		}
	}
	if f := done / total; f > s.p.Fraction {
		s.p.Fraction = min(f, 0.999)
	}
	if s.p.Done {
		s.p.Fraction = 1
	}
	if el := time.Since(s.p.Started); s.p.Fraction > 0.02 && el > 20*time.Second && !s.p.Done {
		s.p.ETA = time.Duration(float64(el) * (1 - s.p.Fraction) / s.p.Fraction).Round(time.Minute)
	}
}

// sortIndexDefs are the indexes added by schema version 2, with the size
// each one adds per work (measured on a 1M-work index) for progress.
var sortIndexDefs = []struct {
	name, label, sql string
	bytesPerRow      float64
}{
	{"works_updated", "date sort index", `CREATE INDEX IF NOT EXISTS works_updated ON works(updated)`, 19.5},
	{"works_words", "length sort index", `CREATE INDEX IF NOT EXISTS works_words ON works(words)`, 12.2},
	{"works_title", "title sort index", `CREATE INDEX IF NOT EXISTS works_title ON works(title COLLATE NOCASE)`, 38.5},
}

// upgradeCacheKiB is the page cache for the upgrade connection. It also
// sets the sorter's run size, so a large value merges in a single pass.
var upgradeCacheKiB = 262144

// Step weights for the overall bar. Each index build reads the whole works
// table, which dominates on spinning disks, so they weigh the same.
var upgradeWeights = []float64{0.02, 0.29, 0.29, 0.29, 0.11}

// NeedsUpgrade reports whether the index must be upgraded before use.
func (db *DB) NeedsUpgrade() bool {
	var v string
	db.SQL.QueryRow(`SELECT value FROM meta WHERE key='schema'`).Scan(&v)
	n, _ := strconv.Atoi(v)
	return n < schemaVersion
}

// Upgrade brings an index built by an older version to schemaVersion.
// Nothing else may write while it runs. It can be cancelled; completed steps
// are kept and the next run continues from there.
func (db *DB) Upgrade(ctx context.Context, st *UpgradeState, logger *log.Logger) error {
	if logger == nil {
		logger = log.Default()
	}
	steps := []UpgradeStep{{Name: "Counting works", State: "pending"}}
	for _, d := range sortIndexDefs {
		steps = append(steps, UpgradeStep{Name: "Building the " + d.label, State: "pending"})
	}
	steps = append(steps, UpgradeStep{Name: "Saving", State: "pending"})
	st.update(func(p *UpgradeProgress) {
		*p = UpgradeProgress{Steps: steps, Started: time.Now()}
	})

	err := db.upgrade(ctx, st, logger)
	st.update(func(p *UpgradeProgress) {
		if err != nil {
			p.Err = err.Error()
		} else {
			p.Done = true
		}
	})
	return err
}

func (db *DB) upgrade(ctx context.Context, st *UpgradeState, logger *log.Logger) error {
	conn, err := db.SQL.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	setStep := func(i int, state string) {
		st.update(func(p *UpgradeProgress) {
			p.Current = i
			p.Steps[i].State = state
			if state == "done" || state == "skipped" {
				p.Steps[i].Fraction = 1
			}
		})
	}
	setStep(0, "running")
	st.update(func(p *UpgradeProgress) { p.Steps[0].Detail = "preparing" })

	// Sorting tens of millions of keys spills to temporary files. Keep them
	// next to the index rather than inside the container image, and give the
	// sorter enough memory to merge in one pass.
	tmp := filepath.Join(filepath.Dir(db.Path), ".tmp")
	os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	for _, q := range []string{
		`PRAGMA temp_store_directory = '` + strings.ReplaceAll(tmp, "'", "''") + `'`,
		`PRAGMA cache_size = -` + strconv.Itoa(upgradeCacheKiB),
		`PRAGMA wal_autocheckpoint = 0`,
		// Start with an empty WAL so its size measures this upgrade's writes.
		`PRAGMA wal_checkpoint(TRUNCATE)`,
	} {
		if _, err := conn.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("preparing upgrade (%s): %w", q, err)
		}
	}
	defer conn.ExecContext(context.Background(), `PRAGMA wal_autocheckpoint = 1000`)

	stop := make(chan struct{})
	defer close(stop)
	go logUpgrade(st, logger, stop)

	// Step 0: count works through the FTS size table (about 20 bytes per
	// work), which is far cheaper than the works table.
	st.update(func(p *UpgradeProgress) { p.Steps[0].Detail = "counting" })
	var rows int64
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM works_fts_docsize`).Scan(&rows); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rows = 0
	}
	st.update(func(p *UpgradeProgress) {
		p.Rows = rows
		p.Steps[0].Detail = fmt.Sprintf("%s works", commas(rows))
	})
	setStep(0, "done")
	logger.Printf("index: upgrading the index file: %s works, 3 sort indexes to build", commas(rows))

	existing := map[string]bool{}
	if r, err := conn.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='index'`); err == nil {
		for r.Next() {
			var n string
			r.Scan(&n)
			existing[n] = true
		}
		r.Close()
	}

	// Steps 1-3: build the indexes. Each scans the works table (bytes read
	// by this process), sorts, then writes the index into the WAL.
	tableBytes := estimateTableBytes(db.Path)
	for i, d := range sortIndexDefs {
		step := i + 1
		if existing[d.name] {
			setStep(step, "skipped")
			st.update(func(p *UpgradeProgress) { p.Steps[step].Detail = "already built" })
			continue
		}
		setStep(step, "running")
		start := time.Now()
		read0 := procIO()
		wal0 := fileSize(db.Path + "-wal")
		expect := float64(rows) * d.bytesPerRow
		done := make(chan struct{})
		var measuredScan int64
		go func() {
			t := time.NewTicker(time.Second)
			defer t.Stop()
			for {
				select {
				case <-done:
					return
				case <-t.C:
				}
				readNow := procIO()
				read := readNow.rchar - read0.rchar
				wrote := fileSize(db.Path+"-wal") - wal0
				scan := clamp01(float64(read) / float64(max(tableBytes, 1)))
				write := 0.0
				if expect > 0 {
					write = clamp01(float64(wrote) / expect)
				}
				var detail string
				if wrote <= 0 {
					if readNow.ok {
						detail = fmt.Sprintf("reading works: %s of about %s", human(read), human(tableBytes))
					} else {
						detail = "reading works"
					}
				} else {
					scan = 1
					if measuredScan == 0 {
						measuredScan = read
					}
					detail = fmt.Sprintf("writing index: %s of about %s", human(wrote), human(int64(expect)))
				}
				st.update(func(p *UpgradeProgress) {
					p.Steps[step].Fraction = max(p.Steps[step].Fraction, 0.6*scan+0.4*write)
					p.Steps[step].Detail = detail
				})
			}
		}()
		_, err := conn.ExecContext(ctx, d.sql)
		close(done)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("building %s: %w", d.name, err)
		}
		// Later steps read the same table: use what this one really read.
		if measuredScan > 0 {
			tableBytes = measuredScan
		}
		readTotal := procIO().rchar - read0.rchar
		wroteTotal := fileSize(db.Path+"-wal") - wal0
		st.update(func(p *UpgradeProgress) {
			p.Steps[step].Detail = fmt.Sprintf("built in %s (read %s, wrote %s)",
				time.Since(start).Round(time.Second), human(readTotal), human(wroteTotal))
		})
		setStep(step, "done")
		logger.Printf("index: %s built in %s (read %s, wrote %s)", d.label, time.Since(start).Round(time.Second), human(readTotal), human(wroteTotal))
	}

	// Step 4: switch to the new triggers with an exact counter, then copy
	// the WAL back into the index file.
	setStep(4, "running")
	st.update(func(p *UpgradeProgress) { p.Steps[4].Detail = "updating triggers" })
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for _, q := range []string{
		`DROP TRIGGER IF EXISTS works_ai`,
		`DROP TRIGGER IF EXISTS works_ad`,
		`DROP TRIGGER IF EXISTS works_au`,
		`INSERT OR REPLACE INTO stats(key,n) SELECT 'works', count(*) FROM works`,
		triggers(db.FTSSummary),
		`INSERT OR REPLACE INTO meta(key,value) VALUES('schema','` + strconv.Itoa(schemaVersion) + `')`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			tx.Rollback()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("upgrading index: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	walBytes := fileSize(db.Path + "-wal")
	write0 := procIO()
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
			}
			w := procIO()
			wrote := w.wchar - write0.wchar
			st.update(func(p *UpgradeProgress) {
				p.Steps[4].Fraction = max(p.Steps[4].Fraction, 0.1+0.9*clamp01(float64(wrote)/float64(max(walBytes, 1))))
				p.Steps[4].Detail = fmt.Sprintf("copying into the index file: %s of %s", human(wrote), human(walBytes))
			})
		}
	}()
	// The upgrade is committed at this point; this only moves pages from the
	// WAL into the main file, and SQLite would do it later anyway.
	_, err = conn.ExecContext(context.Background(), `PRAGMA wal_checkpoint(TRUNCATE)`)
	close(done)
	if err != nil {
		logger.Printf("index: checkpoint after upgrade: %v (harmless; it will be retried)", err)
	}
	st.update(func(p *UpgradeProgress) { p.Steps[4].Detail = "" })
	setStep(4, "done")
	logger.Printf("index: upgrade finished in %s", time.Since(st.Snapshot().Started).Round(time.Second))
	return nil
}

// logUpgrade prints a text progress bar to the log every 15 seconds.
func logUpgrade(st *UpgradeState, logger *log.Logger, stop <-chan struct{}) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		logger.Print(UpgradeLine(st.Snapshot()))
	}
}

// UpgradeLine renders progress as one log line with a text bar.
func UpgradeLine(p UpgradeProgress) string {
	const width = 30
	filled := int(p.Fraction * width)
	bar := strings.Repeat("#", filled) + strings.Repeat("-", width-filled)
	line := fmt.Sprintf("index: upgrade [%s] %3.0f%%", bar, p.Fraction*100)
	if p.Current < len(p.Steps) {
		s := p.Steps[p.Current]
		line += fmt.Sprintf("  step %d/%d %s", p.Current+1, len(p.Steps), strings.ToLower(s.Name))
		if s.Detail != "" {
			line += " (" + s.Detail + ")"
		}
	}
	if p.ETA > 0 {
		line += fmt.Sprintf("  about %s left", p.ETA)
	}
	return line
}

type ioCounters struct {
	rchar, wchar int64
	ok           bool
}

// procIO returns bytes this process has read and written through read and
// write system calls, which is what SQLite uses for the index file.
func procIO() ioCounters {
	var c ioCounters
	f, err := os.Open("/proc/self/io")
	if err != nil {
		return c
	}
	c.ok = true
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		switch k {
		case "rchar":
			c.rchar = n
		case "wchar":
			c.wchar = n
		}
	}
	return c
}

// estimateTableBytes guesses the size of the works table before the first
// scan has measured it: about 80% of the file, the rest being the full-text
// index.
func estimateTableBytes(path string) int64 {
	return fileSize(path) * 8 / 10
}

func fileSize(p string) int64 {
	fi, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return fi.Size()
}

func clamp01(f float64) float64 {
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

func human(n int64) string {
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

func commas(n int64) string {
	s := strconv.FormatInt(n, 10)
	var out []byte
	for i := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, s[i])
	}
	return string(out)
}
