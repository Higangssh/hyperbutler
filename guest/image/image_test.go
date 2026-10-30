package image

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
	"time"
)

func TestGeometry(t *testing.T) {
	for _, c := range []struct {
		size uint64
		c    uint16
		h, s uint8
	}{
		{1 << 30, 2080, 16, 63}, // what Hyper-V's own 1 GiB fixed VHD carries
		{1 << 20, 30, 4, 17},
	} {
		if c2, h, s := Geometry(c.size); c2 != c.c || h != c.h || s != c.s {
			t.Errorf("Geometry(%d) = %d/%d/%d, want %d/%d/%d", c.size, c2, h, s, c.c, c.h, c.s)
		}
	}
	// Above 65535*16*63 sectors the geometry saturates at 255 sectors per track.
	if _, h, s := Geometry(MaxDiskSize); h != 16 || s != 255 {
		t.Errorf("Geometry(MaxDiskSize) = %d heads, %d sectors", h, s)
	}
}

func TestFooterRoundTrip(t *testing.T) {
	id := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	ts := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	b, err := EncodeFooter(96<<20, ts, id)
	if err != nil {
		t.Fatal(err)
	}
	if string(b[:8]) != "conectix" || binary.BigEndian.Uint32(b[60:]) != 2 {
		t.Fatalf("not a fixed VHD footer: % x", b[:64])
	}
	f, err := ParseFooter(b[:])
	if err != nil {
		t.Fatal(err)
	}
	if f.Size != 96<<20 || f.UniqueID != id || !f.Timestamp.Equal(ts) {
		t.Fatalf("round trip: %+v", f)
	}
	// Deterministic: the same input gives the same bytes.
	b2, _ := EncodeFooter(96<<20, ts, id)
	if b != b2 {
		t.Fatal("footer is not deterministic")
	}
	// Timestamps outside the format are clamped, not wrapped.
	if f, _ := EncodeFooter(1<<20, time.Unix(0, 0), id); binary.BigEndian.Uint32(f[24:]) != 0 {
		t.Error("a pre-2000 timestamp was not clamped to 0")
	}
}

func TestFooterRejects(t *testing.T) {
	for _, size := range []uint64{0, 512, 1<<20 + 512, MaxDiskSize + 1<<20} {
		if _, err := EncodeFooter(size, time.Time{}, [16]byte{}); err == nil {
			t.Errorf("EncodeFooter(%d) accepted", size)
		}
	}
	good, _ := EncodeFooter(4<<20, time.Time{}, [16]byte{9})
	mut := func(f func(b []byte)) []byte {
		b := append([]byte(nil), good[:]...)
		f(b)
		return b
	}
	fix := func(b []byte) { // recompute the checksum so only the named defect remains
		var a [FooterSize]byte
		copy(a[:], b)
		binary.BigEndian.PutUint32(b[64:], checksum(&a))
	}
	for name, b := range map[string][]byte{
		"short":    good[:511],
		"cookie":   mut(func(b []byte) { b[0] = 'x'; fix(b) }),
		"checksum": mut(func(b []byte) { b[64]++ }),
		"dynamic":  mut(func(b []byte) { binary.BigEndian.PutUint32(b[60:], 3); fix(b) }),
		"offset":   mut(func(b []byte) { binary.BigEndian.PutUint64(b[16:], 512); fix(b) }),
		"sizes":    mut(func(b []byte) { binary.BigEndian.PutUint64(b[40:], 8<<20); fix(b) }),
		"unaligned": mut(func(b []byte) {
			binary.BigEndian.PutUint64(b[40:], 4<<20+512)
			binary.BigEndian.PutUint64(b[48:], 4<<20+512)
			fix(b)
		}),
		"geometry": mut(func(b []byte) { b[58]++; fix(b) }),
		"reserved": mut(func(b []byte) { b[300] = 1; fix(b) }),
		"saved":    mut(func(b []byte) { b[84] = 1; fix(b) }),
		"version":  mut(func(b []byte) { b[13] = 2; fix(b) }),
	} {
		if _, err := ParseFooter(b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func testManifest() *Manifest {
	h := strings.Repeat("ab", 32)
	return &Manifest{
		Version: 1,
		Kernel:  File{Name: "vmlinuz-virt", Size: 12641280, SHA256: h},
		Initrd:  File{Name: "initramfs-winbox.gz", Size: 3 << 20, SHA256: h},
		System:  File{Name: "system.vhd", Size: 80<<20 + FooterSize, SHA256: h},
	}
}

func TestManifestRoundTrip(t *testing.T) {
	m := testManifest()
	b, err := m.Encode()
	if err != nil {
		t.Fatal(err)
	}
	m2, err := ParseManifest(b)
	if err != nil {
		t.Fatal(err)
	}
	if *m2 != *m {
		t.Fatalf("round trip: %+v", m2)
	}
}

func TestManifestRejects(t *testing.T) {
	for name, f := range map[string]func(m *Manifest){
		"version":    func(m *Manifest) { m.Version = 2 },
		"path":       func(m *Manifest) { m.Kernel.Name = `..\vmlinuz` },
		"slash":      func(m *Manifest) { m.Initrd.Name = "a/b" },
		"device":     func(m *Manifest) { m.Initrd.Name = "CON:" },
		"dot":        func(m *Manifest) { m.Initrd.Name = ".." },
		"empty":      func(m *Manifest) { m.Kernel.Name = "" },
		"dup":        func(m *Manifest) { m.Initrd.Name = "VMLINUZ-virt" },
		"zero size":  func(m *Manifest) { m.Kernel.Size = 0 },
		"big initrd": func(m *Manifest) { m.Initrd.Size = 1 << 40 },
		"hash case":  func(m *Manifest) { m.System.SHA256 = strings.Repeat("AB", 32) },
		"hash short": func(m *Manifest) { m.System.SHA256 = "ab" },
		"not vhd":    func(m *Manifest) { m.System.Name = "system.img" },
		"vhd size":   func(m *Manifest) { m.System.Size = 80 << 20 },
		"vhd footer": func(m *Manifest) { m.System.Size = FooterSize },
	} {
		m := testManifest()
		f(m)
		if err := m.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	good, _ := testManifest().Encode()
	for name, b := range map[string][]byte{
		"unknown field": bytes.Replace(good, []byte(`"version"`), []byte(`"extra":1,"version"`), 1),
		"trailing":      append(append([]byte(nil), good...), []byte("{}")...),
		"not utf8":      append([]byte{0xff}, good...),
		"too large":     bytes.Repeat([]byte(" "), maxManifestSize+1),
	} {
		if _, err := ParseManifest(b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestFileCheck(t *testing.T) {
	data := []byte("system image bytes")
	f, err := Describe("system.vhd", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Check(bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if err := f.Check(bytes.NewReader(append(data, 'x'))); err == nil {
		t.Error("a longer file passed")
	}
	if err := f.Check(bytes.NewReader(data[1:])); err == nil {
		t.Error("a shorter file passed")
	}
	bad := append([]byte(nil), data...)
	bad[0] ^= 1
	if err := f.Check(bytes.NewReader(bad)); err == nil {
		t.Error("a changed file passed")
	}
}
