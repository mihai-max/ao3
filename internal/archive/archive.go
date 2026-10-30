// Package archive deals with the torrent's outer .zip files. Nothing is ever
// extracted: the indexer walks each zip's central directory once, and the
// reader later pulls a single EPUB straight out by byte offset.
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
)

// Zip is one outer archive in the data directory.
type Zip struct {
	Name string // base name, e.g. "queue17.zip"
	Path string
	Size int64
	Rank int // higher rank = later in zip_order.txt = preferred on ties
}

// Member locates one file inside an outer zip without its central directory.
type Member struct {
	Zip        string
	Name       string
	DataOffset int64
	CompSize   int64
	Size       int64
	Method     uint16
}

var reZipName = regexp.MustCompile(`(?i)[\w.\-]+\.zip`)
var reQueue = regexp.MustCompile(`(?i)^queue(\d+)\.zip$`)

// Discover lists the *.zip files at the top of dataDir and ranks them. The
// order comes from zip_order.txt when it names them; otherwise queueNN.zip
// files come first in numeric order, followed by the rest alphabetically.
func Discover(dataDir string) ([]Zip, error) {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return nil, err
	}
	var zips []Zip
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".zip") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		zips = append(zips, Zip{Name: e.Name(), Path: filepath.Join(dataDir, e.Name()), Size: info.Size()})
	}

	order := map[string]int{}
	if data, err := os.ReadFile(filepath.Join(dataDir, "zip_order.txt")); err == nil {
		for i, m := range reZipName.FindAllString(string(data), -1) {
			if _, seen := order[strings.ToLower(m)]; !seen {
				order[strings.ToLower(m)] = i
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
	sort.Slice(zips, func(i, j int) bool {
		a1, a2, a3 := key(zips[i])
		b1, b2, b3 := key(zips[j])
		if a1 != b1 {
			return a1 < b1
		}
		if a2 != b2 {
			return a2 < b2
		}
		return a3 < b3
	})
	for i := range zips {
		zips[i].Rank = i
	}
	return zips, nil
}

// Notes returns the small .txt files shipped next to the zips
// (extract_from_zips.txt, zip_order.txt), capped at 64 KiB each.
func Notes(dataDir string) map[string]string {
	out := map[string]string{}
	matches, _ := filepath.Glob(filepath.Join(dataDir, "*.txt"))
	for _, p := range matches {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(f, 64<<10))
		f.Close()
		out[filepath.Base(p)] = string(b)
	}
	return out
}

// MemberOf records where f lives inside its zip.
func MemberOf(zipName string, f *zip.File) (Member, error) {
	off, err := f.DataOffset()
	if err != nil {
		return Member{}, err
	}
	return Member{
		Zip:        zipName,
		Name:       f.Name,
		DataOffset: off,
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
	if strings.ContainsAny(m.Zip, `/\`) {
		return nil, fmt.Errorf("archive: bad zip name %q", m.Zip)
	}
	f, err := os.Open(filepath.Join(dataDir, m.Zip))
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
