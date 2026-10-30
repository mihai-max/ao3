package index

import (
	"archive/zip"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mihai-max/ao3/internal/archive"
	"github.com/mihai-max/ao3/internal/epub"
)

// Options configures an indexing run.
type Options struct {
	DataDir string
	Workers int      // 0 = number of CPUs
	Only    []string // optional glob patterns on zip names
	Logger  *log.Logger
}

// ZipStatus is the live state of one zip.
type ZipStatus struct {
	Name   string
	Size   int64
	Total  int64
	Done   int64
	EPUBs  int64
	Errors int64
	State  string // pending, indexing, done, error
}

// Status is a snapshot of the indexer.
type Status struct {
	Running      bool
	StartedAt    time.Time
	FinishedAt   time.Time
	Zips         []ZipStatus
	Indexed      int64 // EPUBs indexed in this run
	RecentErrors []string
	LastError    string
}

// Indexer walks the archives and fills the database.
type Indexer struct {
	db      *DB
	opt     Options
	running atomic.Bool

	mu sync.Mutex
	st Status
}

// NewIndexer returns an idle indexer.
func NewIndexer(db *DB, opt Options) *Indexer {
	if opt.Workers <= 0 {
		opt.Workers = runtime.NumCPU()
	}
	if opt.Logger == nil {
		opt.Logger = log.New(io.Discard, "", 0)
	}
	return &Indexer{db: db, opt: opt}
}

// Status returns a copy of the current state.
func (ix *Indexer) Status() Status {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	s := ix.st
	s.Zips = append([]ZipStatus(nil), ix.st.Zips...)
	s.RecentErrors = append([]string(nil), ix.st.RecentErrors...)
	return s
}

// Running reports whether a run is in progress.
func (ix *Indexer) Running() bool { return ix.running.Load() }

// ErrAlreadyRunning is returned when Run is called during a run.
var ErrAlreadyRunning = errors.New("indexer already running")

type zipState struct {
	archive.Zip
	idx       int
	zr        *zip.ReadCloser
	total     int64
	processed int64
	epubs     int64
	errors    int64
	dirty     bool
}

type job struct {
	z *zipState
	f *zip.File
}

type result struct {
	z     *zipState
	start bool
	rec   *Record
	epub  bool
	err   error
}

// Run indexes every zip that is not already recorded as done. Zips that were
// interrupted part-way are redone from the start, which is safe because
// writes are idempotent upserts.
func (ix *Indexer) Run(ctx context.Context) error {
	if !ix.running.CompareAndSwap(false, true) {
		return ErrAlreadyRunning
	}
	defer ix.running.Store(false)

	err := ix.run(ctx)
	ix.mu.Lock()
	ix.st.Running = false
	ix.st.FinishedAt = time.Now()
	if err != nil {
		ix.st.LastError = err.Error()
	}
	ix.mu.Unlock()
	return err
}

func (ix *Indexer) run(ctx context.Context) error {
	zips, err := archive.Discover(ix.opt.DataDir)
	if err != nil {
		return fmt.Errorf("reading data directory: %w", err)
	}
	zips = filterZips(zips, ix.opt.Only)
	prog, err := ix.db.AllProgress(context.Background())
	if err != nil {
		return err
	}

	var todo []*zipState
	statuses := make([]ZipStatus, len(zips))
	for i, z := range zips {
		statuses[i] = ZipStatus{Name: z.Name, Size: z.Size, State: "pending"}
		if p, ok := prog[z.Name]; ok && p.Done && p.Size == z.Size {
			statuses[i] = ZipStatus{Name: z.Name, Size: z.Size, Total: p.MembersTotal, Done: p.MembersDone,
				EPUBs: p.EPUBs, Errors: p.Errors, State: "done"}
			continue
		}
		todo = append(todo, &zipState{Zip: z, idx: i})
	}
	ix.mu.Lock()
	ix.st = Status{Running: true, StartedAt: time.Now(), Zips: statuses}
	ix.mu.Unlock()

	if len(todo) == 0 {
		ix.opt.Logger.Printf("index: all %d zips already indexed", len(zips))
		return nil
	}
	ix.opt.Logger.Printf("index: %d of %d zips to index with %d workers", len(todo), len(zips), ix.opt.Workers)

	jobs := make(chan job, ix.opt.Workers*64)
	results := make(chan result, ix.opt.Workers*64)

	// Producer: open zips in rank order and hand out their members.
	var opened []*zipState
	var openedMu sync.Mutex
	go func() {
		defer close(jobs)
		for _, zs := range todo {
			if ctx.Err() != nil {
				return
			}
			zr, err := zip.OpenReader(zs.Path)
			if err != nil {
				ix.fail(zs, fmt.Errorf("opening %s: %w", zs.Name, err))
				continue
			}
			openedMu.Lock()
			opened = append(opened, zs)
			openedMu.Unlock()
			zs.zr = zr
			zs.total = int64(len(zr.File))
			results <- result{z: zs, start: true}
			for _, f := range zr.File {
				select {
				case jobs <- job{z: zs, f: f}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	// Workers: inflate and parse EPUBs.
	var wg sync.WaitGroup
	for i := 0; i < ix.opt.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				if ctx.Err() != nil {
					continue
				}
				results <- process(j)
			}
		}()
	}
	go func() { wg.Wait(); close(results) }()

	err = ix.write(results)

	openedMu.Lock()
	for _, zs := range opened {
		if zs.zr != nil {
			zs.zr.Close()
		}
	}
	openedMu.Unlock()
	if err != nil {
		return err
	}
	if ctx.Err() != nil {
		ix.opt.Logger.Printf("index: stopped; unfinished zips will be redone next start")
		return ctx.Err()
	}
	st := ix.Status()
	ix.opt.Logger.Printf("index: finished, %d EPUBs indexed in %s", st.Indexed, time.Since(st.StartedAt).Round(time.Second))
	return nil
}

func filterZips(zips []archive.Zip, only []string) []archive.Zip {
	if len(only) == 0 {
		return zips
	}
	var out []archive.Zip
	for _, z := range zips {
		for _, pat := range only {
			if ok, _ := filepath.Match(pat, z.Name); ok || pat == z.Name {
				out = append(out, z)
				break
			}
		}
	}
	return out
}

func (ix *Indexer) fail(zs *zipState, err error) {
	ix.opt.Logger.Printf("index: %v", err)
	ix.mu.Lock()
	defer ix.mu.Unlock()
	ix.st.Zips[zs.idx].State = "error"
	ix.addErrLocked(err.Error())
}

func (ix *Indexer) addErrLocked(msg string) {
	ix.st.RecentErrors = append(ix.st.RecentErrors, msg)
	if len(ix.st.RecentErrors) > 50 {
		ix.st.RecentErrors = ix.st.RecentErrors[len(ix.st.RecentErrors)-50:]
	}
}

func process(j job) result {
	f := j.f
	r := result{z: j.z}
	if f.FileInfo().IsDir() || !strings.EqualFold(filepath.Ext(f.Name), ".epub") {
		return r
	}
	r.epub = true
	m, err := archive.MemberOf(j.z.Name, f)
	if err != nil {
		r.err = err
		return r
	}
	if m.Size > archive.MaxMemberSize {
		r.err = fmt.Errorf("too large (%d bytes)", m.Size)
		return r
	}
	rc, err := f.Open()
	if err != nil {
		r.err = err
		return r
	}
	data, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		r.err = err
		return r
	}
	book, err := epub.Open(data)
	if err != nil {
		r.err = err
		return r
	}
	r.rec = toRecord(book.WorkMeta(), m, j.z.Rank)
	return r
}

func join(s []string) string { return strings.Join(s, ", ") }

func toRecord(w epub.Work, m archive.Member, rank int) *Record {
	id := w.WorkID
	if id == 0 {
		id = epub.WorkIDFromName(m.Name)
	}
	if id == 0 {
		// No AO3 id anywhere: derive a stable negative id from the location so
		// it can never collide with a real work id.
		h := fnv.New64a()
		h.Write([]byte(m.Zip + "/" + m.Name))
		id = -int64(h.Sum64()>>1) - 1
	}
	title := w.Title
	if title == "" {
		title = strings.TrimSuffix(filepath.Base(m.Name), filepath.Ext(m.Name))
	}
	return &Record{
		ID: id, Title: title, Author: join(w.Authors), Fandom: join(w.Fandoms), Rating: join(w.Rating),
		Warnings: join(w.Warnings), Category: join(w.Categories), Relationships: join(w.Relationships),
		Characters: join(w.Characters), Freeforms: join(w.Freeforms), Language: w.Language,
		Series: join(w.Series), Summary: w.Summary, Words: w.Words, Chapters: w.Chapters,
		Published: w.Published, Updated: w.Updated,
		Zip: m.Zip, Member: m.Name, DataOffset: m.DataOffset, CompSize: m.CompSize, Size: m.Size,
		Method: int(m.Method), ZipRank: rank,
	}
}

const batchSize = 2000

// write is the single writer goroutine. It uses a background context so
// the last batch is still committed when the run is being cancelled.
func (ix *Indexer) write(results <-chan result) error {
	ctx := context.Background()
	db := ix.db.SQL
	var (
		tx      *sql.Tx
		stmt    *sql.Stmt
		pending int
		dirty   = map[*zipState]bool{}
		err     error
	)
	begin := func() error {
		if tx, err = db.BeginTx(ctx, nil); err != nil {
			return err
		}
		stmt, err = tx.PrepareContext(ctx, upsertSQL)
		return err
	}
	saveProgress := func(zs *zipState, done bool) error {
		var doneAt any
		if done {
			doneAt = time.Now().UTC().Format(time.RFC3339)
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO zip_progress(zip,size,members_total,members_done,epubs,errors,done_at)
			VALUES(?,?,?,?,?,?,?) ON CONFLICT(zip) DO UPDATE SET size=excluded.size, members_total=excluded.members_total,
			members_done=excluded.members_done, epubs=excluded.epubs, errors=excluded.errors, done_at=excluded.done_at`,
			zs.Name, zs.Size, zs.total, zs.processed, zs.epubs, zs.errors, doneAt)
		return err
	}
	commit := func() error {
		for zs := range dirty {
			if err := saveProgress(zs, false); err != nil {
				return err
			}
		}
		clear(dirty)
		stmt.Close()
		if err := tx.Commit(); err != nil {
			return err
		}
		pending = 0
		return begin()
	}
	finish := func(zs *zipState) error {
		delete(dirty, zs)
		if err := saveProgress(zs, true); err != nil {
			return err
		}
		if zs.zr != nil {
			zs.zr.Close()
			zs.zr = nil
		}
		ix.mu.Lock()
		ix.st.Zips[zs.idx].State = "done"
		ix.mu.Unlock()
		ix.opt.Logger.Printf("index: %s done: %d EPUBs, %d errors", zs.Name, zs.epubs, zs.errors)
		return nil
	}

	if err := begin(); err != nil {
		return err
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	logTick := time.NewTicker(30 * time.Second)
	defer logTick.Stop()
	var lastIndexed int64

	for {
		select {
		case r, ok := <-results:
			if !ok {
				err := commit()
				if tx != nil {
					stmt.Close()
					tx.Rollback()
				}
				return err
			}
			zs := r.z
			if r.start {
				if err := saveProgress(zs, false); err != nil {
					return err
				}
				ix.mu.Lock()
				st := &ix.st.Zips[zs.idx]
				st.State, st.Total, st.Done, st.EPUBs, st.Errors = "indexing", zs.total, 0, 0, 0
				ix.mu.Unlock()
				if zs.total == 0 {
					if err := finish(zs); err != nil {
						return err
					}
				}
				continue
			}
			zs.processed++
			if r.epub {
				zs.epubs++
			}
			if r.err != nil {
				zs.errors++
			}
			if r.rec != nil {
				if err := upsert(ctx, stmt, r.rec); err != nil {
					return fmt.Errorf("writing %s/%s: %w", zs.Name, r.rec.Member, err)
				}
			}
			ix.mu.Lock()
			st := &ix.st.Zips[zs.idx]
			st.Done, st.EPUBs, st.Errors = zs.processed, zs.epubs, zs.errors
			if r.rec != nil {
				ix.st.Indexed++
			}
			if r.err != nil {
				ix.addErrLocked(fmt.Sprintf("%s: %v", zs.Name, r.err))
			}
			ix.mu.Unlock()
			dirty[zs] = true
			pending++
			if zs.processed == zs.total {
				if err := finish(zs); err != nil {
					return err
				}
			}
			if pending >= batchSize {
				if err := commit(); err != nil {
					return err
				}
			}
		case <-ticker.C:
			if pending > 0 {
				if err := commit(); err != nil {
					return err
				}
			}
		case <-logTick.C:
			st := ix.Status()
			done := 0
			for _, z := range st.Zips {
				if z.State == "done" {
					done++
				}
			}
			rate := float64(st.Indexed-lastIndexed) / 30
			lastIndexed = st.Indexed
			ix.opt.Logger.Printf("index: %d/%d zips done, %d EPUBs indexed this run, %.0f/s", done, len(st.Zips), st.Indexed, rate)
		}
	}
}
