// Package index stores searchable work metadata in a single SQLite file with
// an FTS5 full-text index, and records where each EPUB lives in the zips.
package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mihai-max/ao3/internal/archive"
	_ "modernc.org/sqlite"
)

// Record is one row of the works table.
type Record struct {
	ID            int64
	Title         string
	Author        string
	Fandom        string
	Rating        string
	Warnings      string
	Category      string
	Relationships string
	Characters    string
	Freeforms     string
	Language      string
	Series        string
	Summary       string
	Words         int64
	Chapters      string
	Published     string
	Updated       string
	Container     string // file holding the zip; empty means the zip itself
	Zip           string
	Member        string
	DataOffset    int64
	CompSize      int64
	Size          int64
	Method        int
	ZipRank       int
}

// Loc returns where the EPUB lives inside the archives.
func (r *Record) Loc() archive.Member {
	return archive.Member{Container: r.Container, Zip: r.Zip, Name: r.Member, DataOffset: r.DataOffset, CompSize: r.CompSize, Size: r.Size, Method: uint16(r.Method)}
}

// DB wraps the SQLite handle.
type DB struct {
	SQL        *sql.DB
	Path       string
	FTSSummary bool
}

const schema = `
CREATE TABLE IF NOT EXISTS meta(key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS works(
  id            INTEGER PRIMARY KEY,
  title         TEXT NOT NULL DEFAULT '',
  author        TEXT NOT NULL DEFAULT '',
  fandom        TEXT NOT NULL DEFAULT '',
  rating        TEXT NOT NULL DEFAULT '',
  warnings      TEXT NOT NULL DEFAULT '',
  category      TEXT NOT NULL DEFAULT '',
  relationships TEXT NOT NULL DEFAULT '',
  characters    TEXT NOT NULL DEFAULT '',
  freeforms     TEXT NOT NULL DEFAULT '',
  language      TEXT NOT NULL DEFAULT '',
  series        TEXT NOT NULL DEFAULT '',
  summary       TEXT NOT NULL DEFAULT '',
  words         INTEGER NOT NULL DEFAULT 0,
  chapters      TEXT NOT NULL DEFAULT '',
  published     TEXT NOT NULL DEFAULT '',
  updated       TEXT NOT NULL DEFAULT '',
  container     TEXT NOT NULL DEFAULT '',
  zip           TEXT NOT NULL,
  member        TEXT NOT NULL,
  data_offset   INTEGER NOT NULL,
  comp_size     INTEGER NOT NULL,
  size          INTEGER NOT NULL,
  method        INTEGER NOT NULL,
  zip_rank      INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS zip_progress(
  zip           TEXT PRIMARY KEY,
  size          INTEGER NOT NULL,
  members_total INTEGER NOT NULL DEFAULT 0,
  members_done  INTEGER NOT NULL DEFAULT 0,
  epubs         INTEGER NOT NULL DEFAULT 0,
  errors        INTEGER NOT NULL DEFAULT 0,
  done_at       TEXT
);
-- Row counts kept up to date by triggers, so pages never run count(*).
CREATE TABLE IF NOT EXISTS stats(key TEXT PRIMARY KEY, n INTEGER NOT NULL);
-- Contentless FTS: the text lives only in works, the FTS index holds tokens.
CREATE VIRTUAL TABLE IF NOT EXISTS works_fts USING fts5(
  title, author, fandom, tags, summary,
  content='', contentless_delete=1,
  tokenize='unicode61 remove_diacritics 2'
);
`

// ftsColumns is the trigger body that feeds the FTS index from a works row.
func ftsColumns(summary bool) string {
	s := "''"
	if summary {
		s = "new.summary"
	}
	return `new.title, new.author, new.fandom,
    new.relationships || ' ' || new.characters || ' ' || new.freeforms || ' ' || new.warnings || ' ' || new.category || ' ' || new.rating || ' ' || new.series,
    ` + s
}

// schemaVersion 2 added the works counter and the sort indexes.
const schemaVersion = 2

// sortIndexes let date, length and title sorts walk an index instead of
// sorting every match. They hold only the key and the rowid.
const sortIndexes = `
CREATE INDEX IF NOT EXISTS works_updated ON works(updated);
CREATE INDEX IF NOT EXISTS works_words ON works(words);
CREATE INDEX IF NOT EXISTS works_title ON works(title COLLATE NOCASE);`

func triggers(summary bool) string {
	cols := ftsColumns(summary)
	return `
CREATE TRIGGER IF NOT EXISTS works_ai AFTER INSERT ON works BEGIN
  INSERT INTO works_fts(rowid, title, author, fandom, tags, summary) VALUES (new.id, ` + cols + `);
  UPDATE stats SET n = n + 1 WHERE key = 'works';
END;
CREATE TRIGGER IF NOT EXISTS works_ad AFTER DELETE ON works BEGIN
  DELETE FROM works_fts WHERE rowid = old.id;
  UPDATE stats SET n = n - 1 WHERE key = 'works';
END;
CREATE TRIGGER IF NOT EXISTS works_au AFTER UPDATE ON works BEGIN
  DELETE FROM works_fts WHERE rowid = old.id;
  INSERT INTO works_fts(rowid, title, author, fandom, tags, summary) VALUES (new.id, ` + cols + `);
END;`
}

// Open opens (creating if needed) the index database. ftsSummary only takes
// effect when the database is first created; it is remembered afterwards.
func Open(path string, ftsSummary bool) (*DB, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("index directory %s: %w", dir, err)
	}
	probe := filepath.Join(dir, ".ao3-write-test")
	if err := os.WriteFile(probe, nil, 0o644); err != nil {
		return nil, fmt.Errorf("index directory %s is not writable by uid %d gid %d (fix the host folder's owner or set PUID/PGID): %w",
			dir, os.Getuid(), os.Getgid(), err)
	}
	os.Remove(probe)

	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)" +
		"&_pragma=busy_timeout(30000)&_pragma=cache_size(-32768)&_pragma=temp_store(FILE)"
	sqldb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	sqldb.SetMaxOpenConns(8)
	db := &DB{SQL: sqldb, Path: path}

	if _, err := sqldb.Exec(schema); err != nil {
		sqldb.Close()
		return nil, fmt.Errorf("creating schema: %w", err)
	}
	if err := migrate(sqldb); err != nil {
		sqldb.Close()
		return nil, err
	}
	var v string
	err = sqldb.QueryRow(`SELECT value FROM meta WHERE key='fts_summary'`).Scan(&v)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Brand new index.
		v = strconv.FormatBool(ftsSummary)
		_, err := sqldb.Exec(`INSERT INTO meta(key,value) VALUES('fts_summary',?),('schema',?);
			INSERT OR IGNORE INTO stats(key,n) VALUES('works',0);`+sortIndexes, v, strconv.Itoa(schemaVersion))
		if err != nil {
			sqldb.Close()
			return nil, err
		}
	case err != nil:
		sqldb.Close()
		return nil, err
	}
	db.FTSSummary = v == "true"
	// An index from an older version keeps its old triggers until Upgrade
	// replaces them; CREATE TRIGGER IF NOT EXISTS leaves them alone.
	if _, err := sqldb.Exec(triggers(db.FTSSummary)); err != nil {
		sqldb.Close()
		return nil, fmt.Errorf("creating triggers: %w", err)
	}
	return db, nil
}

// migrate brings an index created by an older version up to date.
func migrate(sqldb *sql.DB) error {
	rows, err := sqldb.Query(`PRAGMA table_info(works)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	hasContainer := false
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == "container" {
			hasContainer = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !hasContainer {
		if _, err := sqldb.Exec(`ALTER TABLE works ADD COLUMN container TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("adding container column: %w", err)
		}
	}
	return nil
}

// Close closes the database.
func (db *DB) Close() error { return db.SQL.Close() }

// upsertSQL keeps the copy with the newest AO3 "updated" date; on a tie the
// copy from the later zip (higher rank) wins.
const upsertSQL = `
INSERT INTO works(id,title,author,fandom,rating,warnings,category,relationships,characters,freeforms,
  language,series,summary,words,chapters,published,updated,container,zip,member,data_offset,comp_size,size,method,zip_rank)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET
  title=excluded.title, author=excluded.author, fandom=excluded.fandom, rating=excluded.rating,
  warnings=excluded.warnings, category=excluded.category, relationships=excluded.relationships,
  characters=excluded.characters, freeforms=excluded.freeforms, language=excluded.language,
  series=excluded.series, summary=excluded.summary, words=excluded.words, chapters=excluded.chapters,
  published=excluded.published, updated=excluded.updated, container=excluded.container, zip=excluded.zip, member=excluded.member,
  data_offset=excluded.data_offset, comp_size=excluded.comp_size, size=excluded.size,
  method=excluded.method, zip_rank=excluded.zip_rank
WHERE excluded.updated > works.updated
   OR (excluded.updated = works.updated AND excluded.zip_rank >= works.zip_rank)`

func upsert(ctx context.Context, stmt *sql.Stmt, r *Record) error {
	_, err := stmt.ExecContext(ctx, r.ID, r.Title, r.Author, r.Fandom, r.Rating, r.Warnings, r.Category,
		r.Relationships, r.Characters, r.Freeforms, r.Language, r.Series, r.Summary, r.Words, r.Chapters,
		r.Published, r.Updated, r.Container, r.Zip, r.Member, r.DataOffset, r.CompSize, r.Size, r.Method, r.ZipRank)
	return err
}

const recordCols = `id,title,author,fandom,rating,warnings,category,relationships,characters,freeforms,
  language,series,summary,words,chapters,published,updated,container,zip,member,data_offset,comp_size,size,method,zip_rank`

func scanRecord(row interface{ Scan(...any) error }) (*Record, error) {
	r := &Record{}
	err := row.Scan(&r.ID, &r.Title, &r.Author, &r.Fandom, &r.Rating, &r.Warnings, &r.Category,
		&r.Relationships, &r.Characters, &r.Freeforms, &r.Language, &r.Series, &r.Summary, &r.Words,
		&r.Chapters, &r.Published, &r.Updated, &r.Container, &r.Zip, &r.Member, &r.DataOffset, &r.CompSize, &r.Size,
		&r.Method, &r.ZipRank)
	return r, err
}

// Get returns one work by id.
func (db *DB) Get(ctx context.Context, id int64) (*Record, error) {
	return scanRecord(db.SQL.QueryRowContext(ctx, `SELECT `+recordCols+` FROM works WHERE id=?`, id))
}

// Count returns the number of indexed works from the trigger-maintained
// counter; it never scans the table.
func (db *DB) Count(ctx context.Context) int64 {
	var n int64
	db.SQL.QueryRowContext(ctx, `SELECT n FROM stats WHERE key='works'`).Scan(&n)
	return n
}

// SizeOnDisk returns the bytes used by the database and its WAL.
func (db *DB) SizeOnDisk() int64 {
	var total int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if fi, err := os.Stat(db.Path + suffix); err == nil {
			total += fi.Size()
		}
	}
	return total
}

// Progress is one row of zip_progress.
type Progress struct {
	Zip          string
	Size         int64
	MembersTotal int64
	MembersDone  int64
	EPUBs        int64
	Errors       int64
	Done         bool
}

// AllProgress returns progress rows keyed by zip name.
func (db *DB) AllProgress(ctx context.Context) (map[string]Progress, error) {
	rows, err := db.SQL.QueryContext(ctx, `SELECT zip,size,members_total,members_done,epubs,errors,done_at IS NOT NULL FROM zip_progress`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Progress{}
	for rows.Next() {
		var p Progress
		if err := rows.Scan(&p.Zip, &p.Size, &p.MembersTotal, &p.MembersDone, &p.EPUBs, &p.Errors, &p.Done); err != nil {
			return nil, err
		}
		out[p.Zip] = p
	}
	return out, rows.Err()
}

// SearchParams describes a search request.
type SearchParams struct {
	Query  string
	Field  string // any, title, author, fandom, tags, summary
	Sort   string // relevance, words, updated, title
	Limit  int
	Offset int
}

var fields = map[string]bool{"title": true, "author": true, "fandom": true, "tags": true, "summary": true}

// BuildMatch turns user input into a safe FTS5 MATCH expression.
// Bare words are ANDed, "quoted phrases" stay phrases, -word excludes and
// word* is a prefix search. Every term is quoted, so FTS5 syntax characters
// in user input cannot break the query.
func BuildMatch(q, field string) string {
	var pos, neg []string
	for _, tok := range tokenize(q) {
		negate := strings.HasPrefix(tok.text, "-") && !tok.quoted && len(tok.text) > 1
		text := tok.text
		if negate {
			text = text[1:]
		}
		prefix := !tok.quoted && strings.HasSuffix(text, "*") && len(text) > 1
		text = strings.TrimRight(text, "*")
		if strings.TrimSpace(text) == "" {
			continue
		}
		term := `"` + strings.ReplaceAll(text, `"`, `""`) + `"`
		if prefix {
			term += "*"
		}
		if fields[field] {
			term = field + " : " + term
		}
		if negate {
			neg = append(neg, term)
		} else {
			pos = append(pos, term)
		}
	}
	if len(pos) == 0 {
		return ""
	}
	expr := strings.Join(pos, " AND ")
	for _, n := range neg {
		expr += " NOT " + n
	}
	return expr
}

type token struct {
	text   string
	quoted bool
}

func tokenize(q string) []token {
	var out []token
	for {
		q = strings.TrimSpace(q)
		if q == "" {
			return out
		}
		if q[0] == '"' {
			end := strings.IndexByte(q[1:], '"')
			if end < 0 {
				out = append(out, token{text: q[1:], quoted: true})
				return out
			}
			out = append(out, token{text: q[1 : end+1], quoted: true})
			q = q[end+2:]
			continue
		}
		end := strings.IndexAny(q, " \t\n")
		if end < 0 {
			end = len(q)
		}
		out = append(out, token{text: q[:end]})
		q = q[end:]
	}
}

// SearchResult holds one page of matches.
type SearchResult struct {
	Works  []*Record
	Total  int64 // exact up to CountCap
	Capped bool  // more than CountCap matches
	Sort   string
	// SortFallback is set when relevance was asked for but there were too
	// many matches to rank, so the newest works are shown instead.
	SortFallback bool
	// MaxOffset is how deep paging may go for this result (0 = no limit).
	MaxOffset int
	Elapsed   time.Duration
}

// Search cost tiers, measured on a 1M-work index and chosen so that no page
// costs more than about a second on the full ~20M-work archive:
//
//   - up to RankLimit matches: rank by relevance, or join all matches and sort;
//   - up to CountCap matches: collect the matching ids, then walk a sort
//     index and keep the ids that are in the set;
//   - more: walk the sort index and test each row against the query. The
//     matches are dense enough (>10% of works) that a page fills quickly,
//     but deep pages get slower, so paging is limited to HugeMaxOffset.
var (
	RankLimit     int64 = 100000
	CountCap      int64 = 2000000
	HugeMaxOffset       = 1000
)

var sortIndex = map[string]struct{ index, order string }{
	"updated": {"works_updated", "updated DESC"},
	"words":   {"works_words", "words DESC"},
	"title":   {"works_title", "title COLLATE NOCASE"},
}

const bm25 = "bm25(works_fts, 10.0, 4.0, 3.0, 1.0, 0.5)"

// Search runs a full-text search. Every path is bounded: no query here sorts
// or joins more than CountCap matches.
func (db *DB) Search(ctx context.Context, p SearchParams) (*SearchResult, error) {
	start := time.Now()
	if p.Limit <= 0 || p.Limit > 200 {
		p.Limit = 50
	}
	match := BuildMatch(p.Query, p.Field)
	res := &SearchResult{Sort: p.Sort}
	if res.Sort == "" {
		res.Sort = "relevance"
	}
	if match == "" {
		return res, nil
	}
	err := db.SQL.QueryRowContext(ctx,
		`SELECT count(*) FROM (SELECT rowid FROM works_fts WHERE works_fts MATCH ? LIMIT ?)`, match, CountCap+1).Scan(&res.Total)
	if err != nil {
		return nil, fmt.Errorf("search %q: %w", match, err)
	}
	if res.Total > CountCap {
		res.Total, res.Capped = CountCap, true
	}
	if _, ok := sortIndex[res.Sort]; !ok && res.Sort != "relevance" {
		res.Sort = "relevance"
	}
	if res.Sort == "relevance" && res.Total > RankLimit {
		res.Sort, res.SortFallback = "updated", true
	}

	var q string
	si := sortIndex[res.Sort]
	switch {
	case res.Sort == "relevance":
		q = `SELECT rowid FROM works_fts WHERE works_fts MATCH ? ORDER BY ` + bm25 + ` LIMIT ? OFFSET ?`
	case res.Total <= RankLimit:
		q = `SELECT w.id FROM works_fts JOIN works w ON w.id = works_fts.rowid WHERE works_fts MATCH ?
		     ORDER BY w.` + si.order + ` LIMIT ? OFFSET ?`
	case !res.Capped:
		q = `SELECT w.id FROM works w INDEXED BY ` + si.index + `
		     WHERE w.id IN (SELECT rowid FROM works_fts WHERE works_fts MATCH ?)
		     ORDER BY w.` + si.order + ` LIMIT ? OFFSET ?`
	default:
		res.MaxOffset = HugeMaxOffset
		if p.Offset > HugeMaxOffset {
			p.Offset = HugeMaxOffset
		}
		q = `SELECT w.id FROM works w INDEXED BY ` + si.index + `
		     WHERE EXISTS (SELECT 1 FROM works_fts WHERE works_fts MATCH ? AND works_fts.rowid = w.id)
		     ORDER BY w.` + si.order + ` LIMIT ? OFFSET ?`
	}
	ids, err := db.queryIDs(ctx, q, match, p.Limit, p.Offset)
	if err != nil {
		return nil, fmt.Errorf("search %q: %w", match, err)
	}
	if res.Works, err = db.getMany(ctx, ids); err != nil {
		return nil, err
	}
	res.Elapsed = time.Since(start).Round(100 * time.Microsecond)
	return res, nil
}

func (db *DB) queryIDs(ctx context.Context, q string, args ...any) ([]int64, error) {
	rows, err := db.SQL.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// getMany loads records by id, keeping the order of ids.
func (db *DB) getMany(ctx context.Context, ids []int64) ([]*Record, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	args := make([]any, len(ids))
	marks := make([]string, len(ids))
	for i, id := range ids {
		args[i], marks[i] = id, "?"
	}
	rows, err := db.SQL.QueryContext(ctx, `SELECT `+recordCols+` FROM works WHERE id IN (`+strings.Join(marks, ",")+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byID := make(map[int64]*Record, len(ids))
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		byID[r.ID] = r
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]*Record, 0, len(ids))
	for _, id := range ids {
		if r, ok := byID[id]; ok {
			out = append(out, r)
		}
	}
	return out, nil
}

func prefixed(p, cols string) string {
	parts := strings.Split(cols, ",")
	for i, c := range parts {
		parts[i] = p + strings.TrimSpace(c)
	}
	return strings.Join(parts, ",")
}

// Recent returns the most recently updated works, for the empty search page.
func (db *DB) Recent(ctx context.Context, limit int) ([]*Record, error) {
	rows, err := db.SQL.QueryContext(ctx, `SELECT `+recordCols+` FROM works ORDER BY rowid DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Record
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
