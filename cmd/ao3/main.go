// Command ao3 indexes the "things" AO3 archive zips and serves a search,
// reader and download web UI over them.
//
//	ao3 serve                 web UI; indexes new or unfinished zips in the background
//	ao3 index [--only GLOB]   index in the foreground and exit
//	ao3 inspect FILE.zip      show what is inside one archive
//	ao3 healthcheck           exit 0 when the local server answers /healthz
package main

import (
	"archive/zip"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mihai-max/ao3/internal/archive"
	"github.com/mihai-max/ao3/internal/epub"
	"github.com/mihai-max/ao3/internal/index"
	"github.com/mihai-max/ao3/internal/privdrop"
	"github.com/mihai-max/ao3/internal/web"
)

var version = "dev"

type config struct {
	data       string
	index      string
	addr       string
	workers    int
	autoIndex  bool
	ftsSummary bool
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	if v, err := strconv.ParseBool(os.Getenv(key)); err == nil {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return v
	}
	return def
}

func flags(fs *flag.FlagSet) *config {
	c := &config{}
	fs.StringVar(&c.data, "data", env("AO3_DATA", "/data"), "folder holding the torrent's .zip files (AO3_DATA)")
	fs.StringVar(&c.index, "index", env("AO3_INDEX", "/index/ao3.db"), "SQLite index file (AO3_INDEX)")
	fs.StringVar(&c.addr, "addr", env("AO3_ADDR", ":8080"), "listen address (AO3_ADDR)")
	fs.IntVar(&c.workers, "workers", envInt("AO3_INDEX_WORKERS", 0), "parallel EPUB parsers, 0 = all CPUs (AO3_INDEX_WORKERS)")
	fs.BoolVar(&c.autoIndex, "auto-index", envBool("AO3_AUTO_INDEX", true), "index new zips when the server starts (AO3_AUTO_INDEX)")
	fs.BoolVar(&c.ftsSummary, "fts-summary", envBool("AO3_FTS_SUMMARY", false), "make summaries searchable; bigger index; only applies to a new index (AO3_FTS_SUMMARY)")
	return c
}

func main() {
	log.SetFlags(log.LstdFlags)
	cmd := "serve"
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve(args)
	case "index":
		err = runIndex(args)
	case "inspect":
		err = inspect(args)
	case "healthcheck":
		err = healthcheck()
	case "version":
		fmt.Println(version)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\nusage: ao3 [serve|index|inspect|healthcheck|version] [flags]\n", cmd)
		os.Exit(2)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}

func openDB(c *config) (*index.DB, error) {
	if err := privdrop.Drop(filepath.Dir(c.index)); err != nil {
		return nil, fmt.Errorf("switching to PUID/PGID: %w", err)
	}
	db, err := index.Open(c.index, c.ftsSummary)
	if err != nil {
		return nil, err
	}
	if db.FTSSummary != c.ftsSummary {
		log.Printf("note: this index was created with summary search %v; AO3_FTS_SUMMARY=%v is ignored until you delete %s and reindex",
			db.FTSSummary, c.ftsSummary, c.index)
	}
	return db, nil
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	c := flags(fs)
	fs.Parse(args)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := openDB(c)
	if err != nil {
		return err
	}
	defer db.Close()
	logger := log.Default()
	ix := index.NewIndexer(db, index.Options{DataDir: c.data, Workers: c.workers, Logger: logger})
	srv, err := web.New(db, ix, c.data, logger)
	if err != nil {
		return err
	}
	srv.BaseCtx = ctx

	done := make(chan struct{})
	if c.autoIndex {
		go func() {
			defer close(done)
			if err := ix.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				logger.Printf("index: %v", err)
			}
		}()
	} else {
		close(done)
	}

	hs := &http.Server{Addr: c.addr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		hs.Shutdown(shut)
	}()
	logger.Printf("ao3 %s: serving on %s, data %s, index %s (uid %d gid %d)", version, c.addr, c.data, c.index, os.Getuid(), os.Getgid())
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-done // let the indexer commit its last batch
	return nil
}

func runIndex(args []string) error {
	fs := flag.NewFlagSet("index", flag.ExitOnError)
	c := flags(fs)
	var only string
	fs.StringVar(&only, "only", "", "comma-separated zip names or globs to index, e.g. queue85.zip")
	fs.Parse(args)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := openDB(c)
	if err != nil {
		return err
	}
	defer db.Close()
	var pats []string
	if only != "" {
		pats = strings.Split(only, ",")
	}
	ix := index.NewIndexer(db, index.Options{DataDir: c.data, Workers: c.workers, Only: pats, Logger: log.Default()})
	if err := ix.Run(ctx); err != nil {
		return err
	}
	log.Printf("index now holds %d works, %d bytes on disk", db.Count(context.Background()), db.SizeOnDisk())
	return nil
}

func inspect(args []string) error {
	fs := flag.NewFlagSet("inspect", flag.ExitOnError)
	n := fs.Int("n", 5, "how many EPUBs to parse and print")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: ao3 inspect [-n 5] FILE.zip|DATA_DIR")
	}
	target := fs.Arg(0)
	if st, err := os.Stat(target); err == nil && st.IsDir() {
		return inspectDir(target)
	}
	zr, err := zip.OpenReader(target)
	if err != nil {
		return err
	}
	defer zr.Close()

	exts := map[string]int{}
	methods := map[uint16]int{}
	var total, comp uint64
	for _, f := range zr.File {
		ext := strings.ToLower(filepath.Ext(f.Name))
		if f.FileInfo().IsDir() {
			ext = "(dir)"
		} else if ext == "" {
			ext = "(none)"
		}
		exts[ext]++
		methods[f.Method]++
		total += f.UncompressedSize64
		comp += f.CompressedSize64
	}
	fmt.Printf("%s: %d members, %s stored, %s uncompressed\n", target, len(zr.File), human(comp), human(total))
	fmt.Println("by extension:")
	for _, k := range sortedKeys(exts) {
		fmt.Printf("  %-8s %d\n", k, exts[k])
	}
	fmt.Println("by compression method (0 = store, 8 = deflate):")
	for m, c := range methods {
		fmt.Printf("  %d: %d\n", m, c)
	}
	fmt.Println("first member names:")
	for i, f := range zr.File {
		if i == 10 {
			break
		}
		fmt.Printf("  %s (%s)\n", f.Name, human(f.UncompressedSize64))
	}
	shown := 0
	for _, f := range zr.File {
		if shown >= *n || !strings.EqualFold(filepath.Ext(f.Name), ".epub") {
			continue
		}
		shown++
		rc, err := f.Open()
		if err != nil {
			fmt.Printf("\n%s: %v\n", f.Name, err)
			continue
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			fmt.Printf("\n%s: %v\n", f.Name, err)
			continue
		}
		b, err := epub.Open(data)
		if err != nil {
			fmt.Printf("\n%s: %v\n", f.Name, err)
			continue
		}
		w := b.WorkMeta()
		fmt.Printf("\n%s\n  title:    %s\n  authors:  %s\n  work id:  %d (ao3 preface found: %v)\n  fandoms:  %s\n  rating:   %s\n  words:    %d  chapters: %s  updated: %s\n  spine:    %d items\n",
			f.Name, w.Title, strings.Join(w.Authors, ", "), w.WorkID, w.HasPreface, strings.Join(w.Fandoms, ", "),
			strings.Join(w.Rating, ", "), w.Words, w.Chapters, w.Updated, len(b.Spine))
	}
	return nil
}

func inspectDir(dir string) error {
	zips, err := archive.Discover(dir)
	if err != nil {
		return err
	}
	var total int64
	fmt.Printf("%d zip files in %s, in indexing order:\n", len(zips), dir)
	for _, z := range zips {
		total += z.Size
		fmt.Printf("  %3d  %-28s %s\n", z.Rank, z.Name, human(uint64(z.Size)))
	}
	fmt.Printf("total %s\n", human(uint64(total)))
	for name, text := range archive.Notes(dir) {
		fmt.Printf("\n--- %s ---\n%s\n", name, text)
	}
	return nil
}

func healthcheck() error {
	addr := env("AO3_ADDR", ":8080")
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	client := http.Client{Timeout: 5 * time.Second}
	res, err := client.Get("http://" + addr + "/healthz")
	if err != nil {
		return err
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("healthz returned %d", res.StatusCode)
	}
	return nil
}

func human(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return m[keys[i]] > m[keys[j]] })
	return keys
}
