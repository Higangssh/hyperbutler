package engine

import (
	"encoding/binary"
	"testing"
	"unicode/utf16"
)

// fullDirRecord builds one FILE_FULL_DIR_INFO record for name, with next as its
// NextEntryOffset (0 for the last).
func fullDirRecord(name string, next uint32) []byte {
	u := utf16.Encode([]rune(name))
	b := make([]byte, 68+2*len(u))
	binary.LittleEndian.PutUint32(b[0:], next)
	binary.LittleEndian.PutUint32(b[60:], uint32(2*len(u)))
	for i, c := range u {
		binary.LittleEndian.PutUint16(b[68+2*i:], c)
	}
	return b
}

// FuzzParseFullDirInfo: the parser never reads outside the buffer or loops, whatever
// the offsets and lengths say.
func FuzzParseFullDirInfo(f *testing.F) {
	a := fullDirRecord("data.vhdx", 0)
	two := append(fullDirRecord(".", 72), fullDirRecord("disk.json", 0)...)
	f.Add(a)
	f.Add(two)
	f.Add([]byte{})
	f.Add(make([]byte, 68))
	f.Fuzz(func(t *testing.T, b []byte) {
		names, err := parseFullDirInfo(b)
		if err == nil && len(names) > len(b)/68 {
			t.Fatalf("%d names from %d bytes", len(names), len(b))
		}
	})
}

func TestParseFullDirInfo(t *testing.T) {
	first := fullDirRecord(".", 0)
	binary.LittleEndian.PutUint32(first, uint32(len(first)+4)) // padded, as Windows aligns records
	buf := append(append(first, 0, 0, 0, 0), fullDirRecord("데이터.json", 0)...)
	names, err := parseFullDirInfo(buf)
	if err != nil || len(names) != 1 || names[0] != "데이터.json" {
		t.Fatalf("%q %v", names, err)
	}
	bad := fullDirRecord("x", 0)
	binary.LittleEndian.PutUint32(bad[60:], 1<<20)
	if _, err := parseFullDirInfo(bad); err == nil {
		t.Error("a name length past the buffer was accepted")
	}
	loop := fullDirRecord("x", 0)
	binary.LittleEndian.PutUint32(loop, 1) // next smaller than a record
	if _, err := parseFullDirInfo(loop); err == nil {
		t.Error("an offset inside the record was accepted")
	}
}
