package broker

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"testing"
)

// testVHDX builds the smallest file checkVHDX reads as a VHDX: the identifier, two
// region tables naming a metadata region, and a metadata table with File Parameters
// (HasParent if parent) and, if locator, a Parent Locator item.
func testVHDX(parent, locator bool) []byte {
	const meta = 320 << 10
	b := make([]byte, meta+64<<10)
	copy(b, vhdxSignature)
	for i, at := range []int{vhdxHeader1, vhdxHeader2} {
		setTestHeader(b, at, uint64(i+1), false)
	}
	rt := make([]byte, vhdxRegionTableLen)
	copy(rt, "regi")
	binary.LittleEndian.PutUint32(rt[8:12], 1)
	copy(rt[16:32], vhdxMetadataRegion[:])
	binary.LittleEndian.PutUint64(rt[32:40], meta)
	binary.LittleEndian.PutUint32(rt[40:44], 64<<10)
	binary.LittleEndian.PutUint32(rt[44:48], 1)
	binary.LittleEndian.PutUint32(rt[4:8], crc32.Checksum(rt, crc32.MakeTable(crc32.Castagnoli)))
	copy(b[vhdxRegionTable1:], rt)
	copy(b[vhdxRegionTable2:], rt)
	mt := b[meta:]
	copy(mt, "metadata")
	count := 1
	copy(mt[32:48], vhdxFileParameters[:])
	binary.LittleEndian.PutUint32(mt[48:52], 4096)
	binary.LittleEndian.PutUint32(mt[52:56], 8)
	binary.LittleEndian.PutUint32(mt[4096:4100], 32<<20)
	if parent {
		binary.LittleEndian.PutUint32(mt[4100:4104], vhdxHasParent)
	}
	if locator {
		copy(mt[64:80], vhdxParentLocator[:])
		binary.LittleEndian.PutUint32(mt[80:84], 8192)
		binary.LittleEndian.PutUint32(mt[84:88], 64)
		count++
	}
	binary.LittleEndian.PutUint16(mt[10:12], uint16(count))
	return b
}

// setTestHeader writes a valid VHDX header with sequence number seq at at, with a
// nonzero LogGuid if log.
func setTestHeader(b []byte, at int, seq uint64, log bool) {
	h := b[at : at+vhdxHeaderLen]
	clear(h)
	copy(h, "head")
	binary.LittleEndian.PutUint64(h[8:16], seq)
	if log {
		h[48] = 1
	}
	binary.LittleEndian.PutUint16(h[66:68], 1)
	binary.LittleEndian.PutUint32(h[4:8], crc32.Checksum(h, crc32.MakeTable(crc32.Castagnoli)))
}

// setTestRegionEntry points region table at's metadata entry at off and refreshes its
// checksum.
func setTestRegionEntry(b []byte, at int, off uint64) {
	rt := b[at : at+vhdxRegionTableLen]
	binary.LittleEndian.PutUint64(rt[32:40], off)
	binary.LittleEndian.PutUint32(rt[4:8], 0)
	binary.LittleEndian.PutUint32(rt[4:8], crc32.Checksum(rt, crc32.MakeTable(crc32.Castagnoli)))
}

func TestCheckVHDX(t *testing.T) {
	if err := checkVHDX(bytes.NewReader(testVHDX(false, false))); err != nil {
		t.Fatalf("a plain VHDX: %v", err)
	}
	for name, b := range map[string][]byte{
		"HasParent":      testVHDX(true, false),
		"parent locator": testVHDX(false, true),
		"both":           testVHDX(true, true),
	} {
		if err := checkVHDX(bytes.NewReader(b)); !errors.Is(err, errDifferencing) {
			t.Errorf("%s: %v, want a differencing disk", name, err)
		}
	}
	// The current header (the higher sequence number) decides; an older one with a log
	// does not matter, a current one with a log is refused (the log would be replayed
	// before the opener reads the metadata).
	b := testVHDX(false, false)
	setTestHeader(b, vhdxHeader1, 1, true)
	setTestHeader(b, vhdxHeader2, 2, false)
	if err := checkVHDX(bytes.NewReader(b)); err != nil {
		t.Errorf("an old header with a log: %v", err)
	}
	for name, b := range map[string][]byte{
		"pending log": func() []byte { b := testVHDX(false, false); setTestHeader(b, vhdxHeader2, 9, true); return b }(),
		"one damaged table": func() []byte {
			b := testVHDX(false, false)
			b[vhdxRegionTable1+20] ^= 1
			return b
		}(),
		"tables disagree": func() []byte {
			b := testVHDX(false, false)
			setTestRegionEntry(b, vhdxRegionTable2, 256<<10)
			return b
		}(),
		"no valid header": func() []byte {
			b := testVHDX(false, false)
			b[vhdxHeader1+20] ^= 1
			b[vhdxHeader2+20] ^= 1
			return b
		}(),
	} {
		if err := checkVHDX(bytes.NewReader(b)); err == nil || errors.Is(err, errDifferencing) {
			t.Errorf("%s: %v, want a refused disk", name, err)
		}
	}
	for name, b := range map[string][]byte{
		"empty":        nil,
		"four bytes":   []byte("vhdx"),
		"a VHD footer": append(make([]byte, 512), []byte("conectix")...),
		"no tables":    append([]byte("vhdxfile"), make([]byte, 400<<10)...),
		"both damaged": func() []byte {
			b := testVHDX(false, false)
			b[vhdxRegionTable1+20] ^= 1
			b[vhdxRegionTable2+20] ^= 1
			return b
		}(),
		"truncated": testVHDX(false, false)[:300<<10],
		"no parameters": func() []byte {
			b := testVHDX(false, false)
			binary.LittleEndian.PutUint16(b[(320<<10)+10:], 0)
			return b
		}(),
	} {
		if err := checkVHDX(bytes.NewReader(b)); err == nil || errors.Is(err, errDifferencing) {
			t.Errorf("%s: %v, want a malformed disk", name, err)
		}
	}
}

func FuzzCheckVHDX(f *testing.F) {
	f.Add(testVHDX(false, false)[:4096], int64(0))
	f.Fuzz(func(t *testing.T, patch []byte, at int64) {
		// Patch a valid differencing disk anywhere: the check must never accept it
		// while the HasParent bit and the locator are intact, and must not panic.
		b := testVHDX(true, true)
		if at < 0 {
			at = -at
		}
		at %= int64(len(b))
		copy(b[at:], patch)
		err := checkVHDX(bytes.NewReader(b))
		meta := int64(320 << 10)
		intact := !(at < meta+4104 && at+int64(len(patch)) > meta+4100) && // HasParent flags
			!(at < meta+80 && at+int64(len(patch)) > meta+64) // the locator's GUID
		if err == nil && intact {
			t.Fatalf("a differencing disk patched at %d with %x was accepted", at, patch)
		}
	})
}
