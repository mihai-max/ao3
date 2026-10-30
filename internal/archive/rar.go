package archive

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
)

// RAR support is limited to what Real-Debrid produces: a single-volume
// archive whose entries are stored without compression. For such entries
// we only need to know where each file's bytes start, and the zips inside
// can then be read in place like any other file.

// RarEntry is one file inside a RAR archive.
type RarEntry struct {
	Name       string
	DataOffset int64
	PackedSize int64
	Size       int64
	Stored     bool // compression method is "store"
	Split      bool // continues in another volume
	Encrypted  bool
	Dir        bool
}

var (
	rar4Sig = []byte{0x52, 0x61, 0x72, 0x21, 0x1a, 0x07, 0x00}
	rar5Sig = []byte{0x52, 0x61, 0x72, 0x21, 0x1a, 0x07, 0x01, 0x00}
)

// ErrNotRAR is returned for files without a RAR signature.
var ErrNotRAR = errors.New("not a RAR archive")

// headerChunk is how much we read at each block start; RAR headers are far
// smaller than this, so one read per entry is enough.
const headerChunk = 64 << 10

// ListRar walks the archive headers in r and returns every file entry.
// It reads only the headers, so a 746 GiB archive costs one small read per
// entry.
func ListRar(r io.ReaderAt, size int64) ([]RarEntry, error) {
	sig := make([]byte, 8)
	n, err := r.ReadAt(sig, 0)
	if err != nil && err != io.EOF {
		return nil, err
	}
	sig = sig[:n]
	switch {
	case bytes.HasPrefix(sig, rar5Sig):
		return listRar5(r, size)
	case bytes.HasPrefix(sig, rar4Sig):
		return listRar4(r, size)
	default:
		return nil, ErrNotRAR
	}
}

func readChunk(r io.ReaderAt, off, size int64) ([]byte, error) {
	want := int64(headerChunk)
	if off+want > size {
		want = size - off
	}
	if want <= 0 {
		return nil, io.EOF
	}
	buf := make([]byte, want)
	n, err := r.ReadAt(buf, off)
	if err != nil && err != io.EOF {
		return nil, err
	}
	return buf[:n], nil
}

// --- RAR 4.x (also called RAR 2.9 / 3.x format) ---

const (
	rar4Main   = 0x73
	rar4File   = 0x74
	rar4EndArc = 0x7b

	rar4FlagSplitBefore = 0x0001
	rar4FlagSplitAfter  = 0x0002
	rar4FlagPassword    = 0x0004
	rar4FlagDirMask     = 0x00e0
	rar4FlagLarge       = 0x0100
	rar4FlagUnicode     = 0x0200
	rar4FlagLongBlock   = 0x8000
)

func listRar4(r io.ReaderAt, size int64) ([]RarEntry, error) {
	var out []RarEntry
	pos := int64(len(rar4Sig))
	for pos < size {
		buf, err := readChunk(r, pos, size)
		if err != nil {
			return out, fmt.Errorf("rar: reading header at %d: %w", pos, err)
		}
		if len(buf) < 7 {
			break
		}
		typ := buf[2]
		flags := binary.LittleEndian.Uint16(buf[3:5])
		headSize := int64(binary.LittleEndian.Uint16(buf[5:7]))
		if headSize < 7 {
			return out, fmt.Errorf("rar: bad header size %d at %d", headSize, pos)
		}
		var addSize int64
		if flags&rar4FlagLongBlock != 0 {
			if len(buf) < 11 {
				break
			}
			addSize = int64(binary.LittleEndian.Uint32(buf[7:11]))
		}
		if typ == rar4EndArc {
			break
		}
		if typ == rar4File {
			if len(buf) < 32 || int64(len(buf)) < headSize {
				return out, fmt.Errorf("rar: truncated file header at %d", pos)
			}
			e := RarEntry{
				PackedSize: int64(binary.LittleEndian.Uint32(buf[7:11])),
				Size:       int64(binary.LittleEndian.Uint32(buf[11:15])),
				Stored:     buf[25] == 0x30,
				Split:      flags&(rar4FlagSplitBefore|rar4FlagSplitAfter) != 0,
				Encrypted:  flags&rar4FlagPassword != 0,
				Dir:        flags&rar4FlagDirMask == rar4FlagDirMask,
			}
			nameSize := int(binary.LittleEndian.Uint16(buf[26:28]))
			p := 32
			if flags&rar4FlagLarge != 0 {
				if len(buf) < p+8 {
					return out, fmt.Errorf("rar: truncated file header at %d", pos)
				}
				e.PackedSize |= int64(binary.LittleEndian.Uint32(buf[p:p+4])) << 32
				e.Size |= int64(binary.LittleEndian.Uint32(buf[p+4:p+8])) << 32
				p += 8
			}
			if len(buf) < p+nameSize {
				return out, fmt.Errorf("rar: truncated file name at %d", pos)
			}
			e.Name = rar4Name(buf[p:p+nameSize], flags&rar4FlagUnicode != 0)
			e.DataOffset = pos + headSize
			addSize = e.PackedSize
			out = append(out, e)
		}
		pos += headSize + addSize
	}
	return out, nil
}

// rar4Name decodes a RAR4 file name. Unicode names carry an 8-bit version,
// a zero byte and then a compressed Unicode form; the archives we care
// about use plain names, so the 8-bit part is good enough.
func rar4Name(b []byte, unicode bool) string {
	if unicode {
		if i := bytes.IndexByte(b, 0); i >= 0 {
			b = b[:i]
		}
	}
	return strings.ReplaceAll(string(b), `\`, "/")
}

// --- RAR 5.0 ---

const (
	rar5TypeFile    = 2
	rar5TypeEncrypt = 4
	rar5TypeEnd     = 5

	rar5FlagExtra      = 0x0001
	rar5FlagData       = 0x0002
	rar5FlagSplitBefor = 0x0008
	rar5FlagSplitAfter = 0x0010

	rar5FileFlagDir   = 0x0001
	rar5FileFlagMtime = 0x0002
	rar5FileFlagCRC   = 0x0004
)

// vint reads a RAR5 variable-length integer.
func vint(b []byte) (v uint64, n int, ok bool) {
	for i := 0; i < len(b) && i < 10; i++ {
		v |= uint64(b[i]&0x7f) << (7 * uint(i))
		if b[i]&0x80 == 0 {
			return v, i + 1, true
		}
	}
	return 0, 0, false
}

func listRar5(r io.ReaderAt, size int64) ([]RarEntry, error) {
	var out []RarEntry
	pos := int64(len(rar5Sig))
	encrypted := false
	for pos < size {
		buf, err := readChunk(r, pos, size)
		if err != nil {
			return out, fmt.Errorf("rar: reading header at %d: %w", pos, err)
		}
		if len(buf) < 7 {
			break
		}
		p := 4 // skip header CRC
		headSize, n, ok := vint(buf[p:])
		if !ok {
			return out, fmt.Errorf("rar: bad header at %d", pos)
		}
		p += n
		headerEnd := int64(p) + int64(headSize)
		if headerEnd > int64(len(buf)) {
			return out, fmt.Errorf("rar: header at %d is %d bytes, too large", pos, headSize)
		}
		h := buf[p:headerEnd]
		typ, n, ok := vint(h)
		if !ok {
			return out, fmt.Errorf("rar: bad header type at %d", pos)
		}
		h = h[n:]
		flags, n, ok := vint(h)
		if !ok {
			return out, fmt.Errorf("rar: bad header flags at %d", pos)
		}
		h = h[n:]
		if flags&rar5FlagExtra != 0 {
			_, n, ok = vint(h)
			if !ok {
				return out, fmt.Errorf("rar: bad extra size at %d", pos)
			}
			h = h[n:]
		}
		var dataSize uint64
		if flags&rar5FlagData != 0 {
			dataSize, n, ok = vint(h)
			if !ok {
				return out, fmt.Errorf("rar: bad data size at %d", pos)
			}
			h = h[n:]
		}
		switch typ {
		case rar5TypeEnd:
			return out, nil
		case rar5TypeEncrypt:
			encrypted = true
		case rar5TypeFile:
			e := RarEntry{
				DataOffset: pos + headerEnd,
				PackedSize: int64(dataSize),
				Split:      flags&(rar5FlagSplitBefor|rar5FlagSplitAfter) != 0,
				Encrypted:  encrypted,
			}
			fflags, n, ok := vint(h)
			if !ok {
				return out, fmt.Errorf("rar: bad file flags at %d", pos)
			}
			h = h[n:]
			e.Dir = fflags&rar5FileFlagDir != 0
			unp, n, ok := vint(h)
			if !ok {
				return out, fmt.Errorf("rar: bad size at %d", pos)
			}
			h = h[n:]
			e.Size = int64(unp)
			_, n, ok = vint(h) // attributes
			if !ok {
				return out, fmt.Errorf("rar: bad attributes at %d", pos)
			}
			h = h[n:]
			if fflags&rar5FileFlagMtime != 0 {
				h = h[4:]
			}
			if fflags&rar5FileFlagCRC != 0 {
				h = h[4:]
			}
			comp, n, ok := vint(h)
			if !ok {
				return out, fmt.Errorf("rar: bad compression info at %d", pos)
			}
			h = h[n:]
			e.Stored = (comp>>7)&0x7 == 0
			_, n, ok = vint(h) // host OS
			if !ok {
				return out, fmt.Errorf("rar: bad host os at %d", pos)
			}
			h = h[n:]
			nameLen, n, ok := vint(h)
			if !ok || uint64(len(h)-n) < nameLen {
				return out, fmt.Errorf("rar: bad name at %d", pos)
			}
			e.Name = strings.ReplaceAll(string(h[n:n+int(nameLen)]), `\`, "/")
			out = append(out, e)
		}
		pos += headerEnd + int64(dataSize)
	}
	return out, nil
}

// zipsInRar turns the usable .zip entries of a RAR into Zip descriptors
// and explains the ones that cannot be used.
func zipsInRar(container string, containerSize int64, entries []RarEntry) ([]Zip, []string) {
	var zips []Zip
	var warnings []string
	for _, e := range entries {
		if e.Dir || !strings.EqualFold(path.Ext(e.Name), ".zip") {
			continue
		}
		base := path.Base(e.Name)
		switch {
		case e.Encrypted:
			warnings = append(warnings, fmt.Sprintf("%s: %s is encrypted and cannot be read", container, base))
		case e.Split:
			warnings = append(warnings, fmt.Sprintf("%s: %s continues in another volume; multi-volume archives are not supported", container, base))
		case !e.Stored:
			warnings = append(warnings, fmt.Sprintf("%s: %s is compressed inside the RAR; only uncompressed (stored) entries can be read in place", container, base))
		case e.DataOffset+e.PackedSize > containerSize:
			warnings = append(warnings, fmt.Sprintf("%s: %s extends past the end of the file (incomplete download?)", container, base))
		default:
			zips = append(zips, Zip{Name: base, Container: container, Offset: e.DataOffset, Size: e.PackedSize})
		}
	}
	return zips, warnings
}
