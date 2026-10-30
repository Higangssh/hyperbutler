package image

import (
	"bytes"
	"testing"
	"time"
)

// FuzzParseFooter: an accepted footer re-encodes to the same bytes, so the engine only
// ever attaches disks whose footer EncodeFooter could have written.
func FuzzParseFooter(f *testing.F) {
	for _, size := range []uint64{1 << 20, 96 << 20, 1 << 30, MaxDiskSize} {
		b, _ := EncodeFooter(size, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), [16]byte{byte(size)})
		f.Add(b[:])
	}
	f.Add(make([]byte, FooterSize))
	f.Fuzz(func(t *testing.T, b []byte) {
		ft, err := ParseFooter(b)
		if err != nil {
			return
		}
		again, err := EncodeFooter(ft.Size, ft.Timestamp, ft.UniqueID)
		if err != nil {
			t.Fatalf("accepted footer does not re-encode: %v", err)
		}
		// The creator fields are not checked, so compare everything else.
		want := append([]byte(nil), b...)
		copy(want[28:40], again[28:40])
		copy(want[64:68], again[64:68])
		if !bytes.Equal(again[:], want) {
			t.Fatalf("re-encode differs:\n% x\n% x", again, want)
		}
	})
}

// FuzzParseManifest: an accepted manifest validates and survives a re-encode.
func FuzzParseManifest(f *testing.F) {
	good, _ := testManifest().Encode()
	f.Add(good)
	f.Add([]byte(`{"version":1}`))
	f.Add([]byte(`{"version":1,"kernel":{"name":"a","size":1,"sha256":"00"}}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		m, err := ParseManifest(b)
		if err != nil {
			return
		}
		if err := m.Validate(); err != nil {
			t.Fatalf("accepted an invalid manifest: %v", err)
		}
		for _, x := range m.Files() {
			if bytes.ContainsAny([]byte(x.Name), `/\:`) || x.Name[0] == '.' {
				t.Fatalf("accepted a name that can leave the directory: %q", x.Name)
			}
		}
		again, err := m.Encode()
		if err != nil {
			t.Fatal(err)
		}
		m2, err := ParseManifest(again)
		if err != nil || *m2 != *m {
			t.Fatalf("re-encode changed the manifest: %v", err)
		}
	})
}
