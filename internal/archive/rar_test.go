package archive

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/nwaples/rardecode/v2"

	"github.com/mihai-max/ao3/internal/testutil"
)

func sampleFiles(t *testing.T) []testutil.RarFile {
	t.Helper()
	works := testutil.Sample()
	dir := t.TempDir()
	var files []testutil.RarFile
	for i, chunk := range [][]testutil.FakeWork{works[:20], works[20:40], works[40:]} {
		p := filepath.Join(dir, "x.zip")
		method := uint16(zip.Store)
		if i == 1 {
			method = zip.Deflate
		}
		if err := testutil.WriteArchive(p, chunk, method, nil); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(p)
		files = append(files, testutil.RarFile{Name: []string{"things/queue01.zip", "things/jan_may_2023.zip", "things/queue85.zip"}[i], Data: data})
	}
	files = append(files, testutil.RarFile{Name: "things/zip_order.txt", Data: []byte("queue01.zip\njan_may_2023.zip\nqueue85.zip\n")})
	return files
}

// oracle reads the archive with an independent library so a bug shared by
// our writer and our parser cannot hide.
func oracle(t *testing.T, path string, want []testutil.RarFile) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rr, err := rardecode.NewReader(f)
	if err != nil {
		t.Fatalf("oracle cannot open %s: %v", path, err)
	}
	for i := 0; ; i++ {
		h, err := rr.Next()
		if err == io.EOF {
			if i != len(want) {
				t.Fatalf("oracle saw %d entries, want %d", i, len(want))
			}
			return
		}
		if err != nil {
			t.Fatalf("oracle entry %d: %v", i, err)
		}
		if h.Name != want[i].Name || h.UnPackedSize != int64(len(want[i].Data)) {
			t.Errorf("oracle entry %d = %s %d, want %s %d", i, h.Name, h.UnPackedSize, want[i].Name, len(want[i].Data))
		}
		got, err := io.ReadAll(rr)
		if err != nil || !bytes.Equal(got, want[i].Data) {
			t.Errorf("oracle entry %d content mismatch (%v)", i, err)
		}
	}
}

func checkList(t *testing.T, path string, want []testutil.RarFile) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, _ := f.Stat()
	entries, err := ListRar(f, st.Size())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(want) {
		t.Fatalf("got %d entries, want %d", len(entries), len(want))
	}
	for i, e := range entries {
		w := want[i]
		if e.Name != w.Name || e.Size != int64(len(w.Data)) || e.PackedSize != int64(len(w.Data)) || !e.Stored || e.Dir || e.Split || e.Encrypted {
			t.Errorf("entry %d = %+v", i, e)
		}
		got := make([]byte, e.PackedSize)
		if _, err := f.ReadAt(got, e.DataOffset); err != nil || !bytes.Equal(got, w.Data) {
			t.Errorf("entry %d: bytes at offset %d do not match", i, e.DataOffset)
		}
	}
	// The zips inside must open as zips from their offsets.
	zips, warnings := zipsInRar(filepath.Base(path), st.Size(), entries)
	if len(warnings) != 0 || len(zips) != 3 {
		t.Fatalf("zipsInRar: %d zips, warnings %v", len(zips), warnings)
	}
	for _, z := range zips {
		zr, err := zip.NewReader(io.NewSectionReader(f, z.Offset, z.Size), z.Size)
		if err != nil {
			t.Fatalf("%s inside rar: %v", z.Name, err)
		}
		if len(zr.File) < 10 {
			t.Errorf("%s inside rar has %d members", z.Name, len(zr.File))
		}
	}
}

func TestRar4(t *testing.T) {
	files := sampleFiles(t)
	p := filepath.Join(t.TempDir(), "things.rar")
	if err := testutil.WriteRar4(p, files); err != nil {
		t.Fatal(err)
	}
	oracle(t, p, files)
	checkList(t, p, files)
}

func TestRar5(t *testing.T) {
	files := sampleFiles(t)
	p := filepath.Join(t.TempDir(), "things.rar")
	if err := testutil.WriteRar5(p, files); err != nil {
		t.Fatal(err)
	}
	oracle(t, p, files)
	checkList(t, p, files)
}

// A RAR4 header for a file over 4 GiB uses the LARGE flag; build one by hand
// since we cannot write a real 5 GiB fixture.
func TestRar4LargeFlag(t *testing.T) {
	name := []byte("queue17.zip")
	head := make([]byte, 40+len(name))
	head[2] = 0x74
	binary.LittleEndian.PutUint16(head[3:5], 0x8000|0x0100)
	binary.LittleEndian.PutUint16(head[5:7], uint16(len(head)))
	const size = int64(28_086_000_000) // 26 GiB
	binary.LittleEndian.PutUint32(head[7:11], uint32(size&0xffffffff))
	binary.LittleEndian.PutUint32(head[11:15], uint32(size&0xffffffff))
	head[25] = 0x30
	binary.LittleEndian.PutUint16(head[26:28], uint16(len(name)))
	binary.LittleEndian.PutUint32(head[32:36], uint32(size>>32))
	binary.LittleEndian.PutUint32(head[36:40], uint32(size>>32))
	copy(head[40:], name)
	var ar bytes.Buffer
	ar.Write(rar4Sig)
	ar.Write(head)
	// Pretend the data follows by lying about the total size.
	entries, err := ListRar(bytes.NewReader(ar.Bytes()), int64(ar.Len())+size)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Size != size || entries[0].PackedSize != size || entries[0].Name != "queue17.zip" ||
		entries[0].DataOffset != int64(len(rar4Sig)+len(head)) {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestVint(t *testing.T) {
	for _, c := range []struct {
		b []byte
		v uint64
		n int
	}{{[]byte{0x05}, 5, 1}, {[]byte{0x80, 0x01}, 128, 2}, {[]byte{0xff, 0xff, 0x7f}, 0x1fffff, 3}} {
		v, n, ok := vint(c.b)
		if !ok || v != c.v || n != c.n {
			t.Errorf("vint(%x) = %d,%d,%v", c.b, v, n, ok)
		}
	}
	if _, _, ok := vint([]byte{0x80}); ok {
		t.Error("truncated vint accepted")
	}
}

func TestNotRar(t *testing.T) {
	if _, err := ListRar(bytes.NewReader([]byte("PK\x03\x04junk")), 8); err != ErrNotRAR {
		t.Errorf("err = %v", err)
	}
}
