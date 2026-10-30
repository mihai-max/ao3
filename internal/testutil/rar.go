package testutil

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"os"
)

// RarFile is one file to put into a test RAR archive.
type RarFile struct {
	Name string
	Data []byte
}

// WriteRar4 writes a RAR 4.x archive with stored (uncompressed) entries,
// the way `rar a -m0` would. It exists so the parser can be tested without
// the proprietary rar tool.
func WriteRar4(path string, files []RarFile) error {
	var buf bytes.Buffer
	buf.Write([]byte{0x52, 0x61, 0x72, 0x21, 0x1a, 0x07, 0x00})
	// MAIN_HEAD: type 0x73, flags 0, size 13, HighPosAV(2) PosAV(4).
	main := make([]byte, 13)
	main[2] = 0x73
	binary.LittleEndian.PutUint16(main[5:7], 13)
	binary.LittleEndian.PutUint16(main[0:2], uint16(crc32.ChecksumIEEE(main[2:])))
	buf.Write(main)
	for _, f := range files {
		name := []byte(f.Name)
		head := make([]byte, 32+len(name))
		head[2] = 0x74
		flags := uint16(0x8000) // long block (pack size present)
		binary.LittleEndian.PutUint16(head[3:5], flags)
		binary.LittleEndian.PutUint16(head[5:7], uint16(len(head)))
		binary.LittleEndian.PutUint32(head[7:11], uint32(len(f.Data)))  // PACK_SIZE
		binary.LittleEndian.PutUint32(head[11:15], uint32(len(f.Data))) // UNP_SIZE
		head[15] = 3                                                    // HOST_OS unix
		binary.LittleEndian.PutUint32(head[16:20], crc32.ChecksumIEEE(f.Data))
		binary.LittleEndian.PutUint32(head[20:24], 0x5a000000) // FTIME
		head[24] = 29                                          // UNP_VER
		head[25] = 0x30                                        // METHOD store
		binary.LittleEndian.PutUint16(head[26:28], uint16(len(name)))
		binary.LittleEndian.PutUint32(head[28:32], 0x81a4) // ATTR
		copy(head[32:], name)
		binary.LittleEndian.PutUint16(head[0:2], uint16(crc32.ChecksumIEEE(head[2:])))
		buf.Write(head)
		buf.Write(f.Data)
	}
	end := make([]byte, 7)
	end[2] = 0x7b
	binary.LittleEndian.PutUint16(end[3:5], 0x4000)
	binary.LittleEndian.PutUint16(end[5:7], 7)
	binary.LittleEndian.PutUint16(end[0:2], uint16(crc32.ChecksumIEEE(end[2:])))
	buf.Write(end)
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

func putVint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

// WriteRar5 writes a RAR 5.0 archive with stored entries.
func WriteRar5(path string, files []RarFile) error {
	var buf bytes.Buffer
	buf.Write([]byte{0x52, 0x61, 0x72, 0x21, 0x1a, 0x07, 0x01, 0x00})
	block := func(body []byte) {
		var hdr []byte
		hdr = putVint(hdr, uint64(len(body)))
		hdr = append(hdr, body...)
		var crc [4]byte
		binary.LittleEndian.PutUint32(crc[:], crc32.ChecksumIEEE(hdr))
		buf.Write(crc[:])
		buf.Write(hdr)
	}
	// Main header: type 1, flags 0, archive flags 0.
	block([]byte{1, 0, 0})
	for _, f := range files {
		var body []byte
		body = putVint(body, 2)                   // type file
		body = putVint(body, 0x0002)              // header flags: data area present
		body = putVint(body, uint64(len(f.Data))) // data size
		body = putVint(body, 0x0004)              // file flags: CRC present
		body = putVint(body, uint64(len(f.Data))) // unpacked size
		body = putVint(body, 0x81a4)              // attributes
		var crc [4]byte
		binary.LittleEndian.PutUint32(crc[:], crc32.ChecksumIEEE(f.Data))
		body = append(body, crc[:]...)
		body = putVint(body, 0) // compression: version 0, method 0 (store)
		body = putVint(body, 1) // host OS unix
		body = putVint(body, uint64(len(f.Name)))
		body = append(body, f.Name...)
		block(body)
		buf.Write(f.Data)
	}
	block([]byte{5, 0, 0}) // end of archive
	return os.WriteFile(path, buf.Bytes(), 0o644)
}
