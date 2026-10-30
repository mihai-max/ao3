// Command ao3 indexes the "things" AO3 archive zips and serves a search,
// reader and download web UI over them.
//
//	ao3 serve                 web UI; indexes new or unfinished zips in the background
//	ao3 index [--only GLOB]   index in the foreground and exit
//	ao3 inspect FILE.zip      show what is inside one archive
//	ao3 healthcheck           exit 0 when the local server answers /healthz
package main

import (
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
	"github.com/mihai-max/ao3/internal/rmount"
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

// mountRemote starts the rclone mount when one is configured through the
// environment. It must run before privileges are dropped.
func mountRemote(ctx context.Context, c *config) (*rmount.Mount, error) {
	cfg := rmount.FromEnv(c.data)
	if !cfg.Enabled() {
		return nil, nil
	}
	cfg.Logger = log.Default()
	log.Printf("rclone: mounting %s at %s", cfg.Describe(), c.data)
	m, err := rmount.Start(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("mounting %s: %w", cfg.Describe(), err)
	}
	return m, nil
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

	mnt, err := mountRemote(ctx, c)
	if err != nil {
		return err
	}
	if mnt != nil {
		defer mnt.Close()
		go func() {
			// If rclone gives up for good, exit so Docker restarts the container.
			select {
			case <-mnt.Done():
				if ctx.Err() == nil {
					log.Printf("rclone: mount is gone and could not be restored, exiting")
					stop()
				}
			case <-ctx.Done():
			}
		}()
	}
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
	if mnt != nil {
		srv.Mount = mnt
	}

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
		logger.Printf("shutting down: stopping web server")
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		hs.Shutdown(shut)
	}()
	logger.Printf("ao3 %s: serving on %s, data %s, index %s (uid %d gid %d)", version, c.addr, c.data, c.index, os.Getuid(), os.Getgid())
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-done // let the indexer commit its last batch
	logger.Printf("shutting down: index closed, web server stopped")
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

	mnt, err := mountRemote(ctx, c)
	if err != nil {
		return err
	}
	if mnt != nil {
		defer mnt.Close()
	}
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
		return errors.New("usage: ao3 inspect [-n 5] DATA_DIR | FILE.zip | FILE.rar | DATA_DIR/name.zip (also inside a RAR)")
	}
	target := fs.Arg(0)
	dataDir := env("AO3_DATA", "/data")
	if cfg := rmount.FromEnv(dataDir); cfg.Enabled() && strings.HasPrefix(filepath.Clean(target)+"/", filepath.Clean(cfg.MountPoint)+"/") {
		cfg.Logger = log.Default()
		m, err := rmount.Start(context.Background(), cfg)
		if err != nil {
			return err
		}
		defer m.Close()
	}
	st, err := os.Stat(target)
	switch {
	case err == nil && st.IsDir():
		return inspectDir(target)
	case err == nil && strings.EqualFold(filepath.Ext(target), ".rar"):
		return inspectRar(target, st.Size())
	case err == nil:
		return inspectZip(archive.Zip{Name: filepath.Base(target), Container: filepath.Base(target), Size: st.Size()}, filepath.Dir(target), *n)
	}
	// Not a file on disk: maybe a zip inside a RAR, named like DATA_DIR/queue85.zip.
	dir, base := filepath.Split(target)
	zips, derr := archive.Discover(filepath.Clean(dir))
	if derr != nil {
		return err
	}
	for _, z := range zips {
		if strings.EqualFold(z.Name, base) {
			return inspectZip(z, filepath.Clean(dir), *n)
		}
	}
	return err
}

func inspectRar(path string, size int64) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	entries, err := archive.ListRar(f, size)
	if err != nil {
		return err
	}
	var stored, zips int
	var total int64
	fmt.Printf("%s: %d entries, %s\n", path, len(entries), human(uint64(size)))
	for _, e := range entries {
		flags := ""
		if !e.Stored {
			flags += " COMPRESSED"
		}
		if e.Split {
			flags += " SPLIT"
		}
		if e.Encrypted {
			flags += " ENCRYPTED"
		}
		if e.Dir {
			flags += " dir"
		}
		if e.Stored {
			stored++
		}
		if strings.EqualFold(filepath.Ext(e.Name), ".zip") {
			zips++
		}
		total += e.Size
		fmt.Printf("  %12s  @%-14d %s%s\n", human(uint64(e.Size)), e.DataOffset, e.Name, flags)
	}
	fmt.Printf("%d zip files, %d of %d entries stored uncompressed, %s of content\n", zips, stored, len(entries), human(uint64(total)))
	if stored != len(entries) {
		fmt.Println("WARNING: compressed entries cannot be read in place; only stored entries are indexed")
	}
	return nil
}

func inspectZip(z archive.Zip, dataDir string, n int) error {
	zr, closer, err := archive.Open(dataDir, z)
	if err != nil {
		return err
	}
	defer closer.Close()

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
	where := z.Name
	if z.Container != z.Name {
		where = fmt.Sprintf("%s (inside %s at offset %d)", z.Name, z.Container, z.Offset)
	}
	fmt.Printf("%s: %d members, %s stored, %s uncompressed\n", where, len(zr.File), human(comp), human(total))
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
		if shown >= n || !strings.EqualFold(filepath.Ext(f.Name), ".epub") {
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
	layout, err := archive.Scan(dir)
	if err != nil {
		return err
	}
	var total int64
	fmt.Printf("%d zip files in %s, in indexing order:\n", len(layout.Zips), dir)
	for _, z := range layout.Zips {
		total += z.Size
		in := ""
		if z.Container != z.Name {
			in = "  in " + z.Container
		}
		fmt.Printf("  %3d  %-28s %10s%s\n", z.Rank, z.Name, human(uint64(z.Size)), in)
	}
	fmt.Printf("total %s\n", human(uint64(total)))
	for _, w := range layout.Warnings {
		fmt.Println("WARNING:", w)
	}
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
