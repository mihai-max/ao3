// Package archive deals with the torrent's outer .zip files. Nothing is ever
// extracted: the indexer walks each zip's central directory once, and the
// reader later pulls a single EPUB straight out by byte offset.
//
// A zip may sit directly in the data folder, or inside a RAR archive the way
// Real-Debrid delivers multi-file torrents (one uncompressed things.rar). In
// both cases a Zip is "Container file, Offset, Size", and Member offsets are
// absolute positions inside the container.
package archive

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Zip is one archive of EPUBs.
type Zip struct {
	Name      string // display name, e.g. "queue17.zip"
	Container string // file in the data folder that holds it: itself, or e.g. "things.rar"
	Offset    int64  // where the zip starts inside Container
	Size      int64
	Rank      int // higher rank = later in zip_order.txt = preferred on ties
}

// Layout is everything found in the data folder.
type Layout struct {
	Zips     []Zip
	Warnings []string // things that were found but cannot be used
}

// Member locates one file inside a zip. DataOffset is absolute within the
// container file, so a member can be read with one positioned read.
type Member struct {
	Container  string
	Zip        string
	Name       string
	DataOffset int64
	CompSize   int64
	Size       int64
	Method     uint16
}

var reZipName = regexp.MustCompile(`(?i)[\w.\-]+\.zip`)
var reQueue = regexp.MustCompile(`(?i)^queue(\d+)\.zip$`)

// Discover lists the usable zips in dataDir in indexing order.
func Discover(dataDir string) ([]Zip, error) {
	l, err := Scan(dataDir)
	return l.Zips, err
}

// Scan lists the zips at the top of dataDir, plus the zips inside any RAR
// archives there, and ranks them. The order comes from zip_order.txt when it
// names them; otherwise queueNN.zip files come first in numeric order,
// followed by the rest alphabetically.
func Scan(dataDir string) (Layout, error) {
	var l Layout
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return l, err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		switch strings.ToLower(filepath.Ext(e.Name())) {
		case ".zip":
			l.Zips = append(l.Zips, Zip{Name: e.Name(), Container: e.Name(), Size: info.Size()})
		case ".rar":
			rl, err := rarLayout(filepath.Join(dataDir, e.Name()), info)
			if err != nil {
				l.Warnings = append(l.Warnings, fmt.Sprintf("%s: %v", e.Name(), err))
				continue
			}
			zips, warnings := zipsInRar(e.Name(), info.Size(), rl.entries)
			if len(zips) == 0 && len(warnings) == 0 {
				warnings = append(warnings, fmt.Sprintf("%s: contains no .zip files", e.Name()))
			}
			l.Zips = append(l.Zips, zips...)
			l.Warnings = append(l.Warnings, warnings...)
		}
	}

	order := map[string]int{}
	for _, text := range Notes(dataDir) {
		for _, m := range reZipName.FindAllString(text, -1) {
			if _, seen := order[strings.ToLower(m)]; !seen {
				order[strings.ToLower(m)] = len(order)
			}
		}
	}
	key := func(z Zip) (int, int, string) {
		if i, ok := order[strings.ToLower(z.Name)]; ok {
			return 0, i, ""
		}
		if m := reQueue.FindStringSubmatch(z.Name); m != nil {
			n, _ := strconv.Atoi(m[1])
			return 1, n, ""
		}
		return 2, 0, strings.ToLower(z.Name)
	}
	sort.SliceStable(l.Zips, func(i, j int) bool {
		a1, a2, a3 := key(l.Zips[i])
		b1, b2, b3 := key(l.Zips[j])
		if a1 != b1 {
			return a1 < b1
		}
		if a2 != b2 {
			return a2 < b2
		}
		return a3 < b3
	})
	for i := range l.Zips {
		l.Zips[i].Rank = i
	}
	return l, nil
}

// rarInfo caches a RAR's header walk, which over a network mount costs one
// small read per entry.
type rarInfo struct {
	size    int64
	modTime int64
	entries []RarEntry
}

var (
	rarCacheMu sync.Mutex
	rarCache   = map[string]rarInfo{}
)

func rarLayout(path string, info os.FileInfo) (rarInfo, error) {
	rarCacheMu.Lock()
	cached, ok := rarCache[path]
	rarCacheMu.Unlock()
	if ok && cached.size == info.Size() && cached.modTime == info.ModTime().UnixNano() {
		return cached, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return rarInfo{}, err
	}
	defer f.Close()
	entries, err := ListRar(f, info.Size())
	if err != nil {
		return rarInfo{}, err
	}
	ri := rarInfo{size: info.Size(), modTime: info.ModTime().UnixNano(), entries: entries}
	rarCacheMu.Lock()
	rarCache[path] = ri
	rarCacheMu.Unlock()
	return ri, nil
}

// Notes returns the small .txt files shipped next to the zips
// (extract_from_zips.txt, zip_order.txt), including ones inside a RAR,
// capped at 64 KiB each.
func Notes(dataDir string) map[string]string {
	const maxNote = 64 << 10
	out := map[string]string{}
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		p := filepath.Join(dataDir, e.Name())
		switch strings.ToLower(filepath.Ext(e.Name())) {
		case ".txt":
			f, err := os.Open(p)
			if err != nil {
				continue
			}
			b, _ := io.ReadAll(io.LimitReader(f, maxNote))
			f.Close()
			out[e.Name()] = string(b)
		case ".rar":
			info, err := e.Info()
			if err != nil {
				continue
			}
			rl, err := rarLayout(p, info)
			if err != nil {
				continue
			}
			var f *os.File
			for _, re := range rl.entries {
				if re.Dir || !re.Stored || re.Size > maxNote || !strings.EqualFold(filepath.Ext(re.Name), ".txt") {
					continue
				}
				if f == nil {
					if f, err = os.Open(p); err != nil {
						break
					}
					defer f.Close()
				}
				b := make([]byte, re.PackedSize)
				if _, err := f.ReadAt(b, re.DataOffset); err == nil {
					out[e.Name()+"/"+filepath.Base(re.Name)] = string(b)
				}
			}
		}
	}
	return out
}

// Open opens a zip for reading its directory and members.
func Open(dataDir string, z Zip) (*zip.Reader, io.Closer, error) {
	f, err := os.Open(filepath.Join(dataDir, z.Container))
	if err != nil {
		return nil, nil, err
	}
	zr, err := zip.NewReader(io.NewSectionReader(f, z.Offset, z.Size), z.Size)
	if err != nil {
		f.Close()
		return nil, nil, fmt.Errorf("%s: %w", z.Name, err)
	}
	return zr, f, nil
}

// MemberOf records where f lives inside its container file.
func MemberOf(z Zip, f *zip.File) (Member, error) {
	off, err := f.DataOffset()
	if err != nil {
		return Member{}, err
	}
	return Member{
		Container:  z.Container,
		Zip:        z.Name,
		Name:       f.Name,
		DataOffset: z.Offset + off,
		CompSize:   int64(f.CompressedSize64),
		Size:       int64(f.UncompressedSize64),
		Method:     f.Method,
	}, nil
}

// MaxMemberSize is the largest EPUB we will load into memory.
const MaxMemberSize = 512 << 20

// ReadMember reads one member directly at its recorded offset.
func ReadMember(dataDir string, m Member) ([]byte, error) {
	if m.Size > MaxMemberSize {
		return nil, fmt.Errorf("archive: %s is too large (%d bytes)", m.Name, m.Size)
	}
	container := m.Container
	if container == "" {
		container = m.Zip
	}
	if strings.ContainsAny(container, `/\`) {
		return nil, fmt.Errorf("archive: bad container name %q", container)
	}
	f, err := os.Open(filepath.Join(dataDir, container))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// One read of exactly the compressed bytes: on a network mount this is a
	// single range request.
	raw := make([]byte, m.CompSize)
	if _, err := f.ReadAt(raw, m.DataOffset); err != nil && !(err == io.EOF && m.CompSize == 0) {
		return nil, fmt.Errorf("archive: reading %s from %s: %w", m.Name, m.Zip, err)
	}
	data, err := Decompress(raw, m.Method, m.Size)
	if err != nil {
		return nil, fmt.Errorf("archive: %s from %s: %w", m.Name, m.Zip, err)
	}
	return data, nil
}

// Decompress turns a member's stored bytes into its contents.
func Decompress(raw []byte, method uint16, size int64) ([]byte, error) {
	switch method {
	case zip.Store:
		if int64(len(raw)) != size {
			return nil, fmt.Errorf("stored member is %d bytes, expected %d", len(raw), size)
		}
		return raw, nil
	case zip.Deflate:
		fr := flate.NewReader(bytes.NewReader(raw))
		defer fr.Close()
		buf := make([]byte, size)
		if _, err := io.ReadFull(fr, buf); err != nil {
			return nil, fmt.Errorf("inflating: %w", err)
		}
		return buf, nil
	default:
		return nil, fmt.Errorf("unsupported compression method %d", method)
	}
}
