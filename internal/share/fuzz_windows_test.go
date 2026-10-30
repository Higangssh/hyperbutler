//go:build windows

package share

import (
	"encoding/binary"
	"strings"
	"testing"
)

// FuzzReparse decodes reparse buffers. On the host these come from files in the share,
// which a guest can create (WSL symlinks) and a Windows program can craft.
func FuzzReparse(f *testing.F) {
	lx := binary.LittleEndian.AppendUint32(nil, tagLxSymlink)
	lx = binary.LittleEndian.AppendUint16(lx, 8)
	lx = append(lx, 0, 0, 2, 0, 0, 0, 'a', '/', 'b', 'c')
	f.Add(lx)
	f.Add(make([]byte, 20))
	f.Fuzz(func(t *testing.T, b []byte) {
		s, err := parseReparse(b, []string{"x", "link"}, `\\?\C:\share`)
		if err == nil && strings.IndexByte(s, 0) >= 0 && binary.LittleEndian.Uint32(append(b, 0, 0, 0, 0)) != tagLxSymlink {
			t.Fatalf("NUL in NTFS link target %q", s)
		}
	})
}

// FuzzStatAndRoot covers the FILE_STAT_LX_INFORMATION decoder and the link-target
// containment check.
func FuzzStatAndRoot(f *testing.F) {
	f.Add(make([]byte, statLxLen), `C:\share\a\..\..\x`)
	f.Add(make([]byte, statLxLen), `\\?\C:\SHARE\sub`)
	f.Fuzz(func(t *testing.T, b []byte, target string) {
		if len(b) >= statLxLen {
			a := statLx(b[:statLxLen])
			if typ := a.Mode & sIFMT; typ != sIFREG && typ != sIFDIR && typ != sIFLNK && typ != sIFIFO && typ != sIFSOCK {
				t.Fatalf("statLx produced type %o", typ)
			}
			if a.Mode&sIFMT == sIFREG && b[63]&0x80 != 0 && (binary.LittleEndian.Uint32(b[60:]) == tagLxChr) {
				t.Fatal("device node not neutralized")
			}
		}
		if rel, ok := insideRoot(target, `\\?\C:\share`); ok {
			for _, c := range rel {
				// A Linux component may contain '\' (NTFS U+F05C maps to it); it must
				// never be "..", empty, or contain '/' or NUL.
				if c == ".." || c == "" || strings.ContainsAny(c, "/\x00") {
					t.Fatalf("insideRoot(%q) = %q", target, rel)
				}
			}
		}
	})
}

// FuzzNotify decodes ReadDirectoryChangesW buffers.
func FuzzNotify(f *testing.F) {
	b := notifyRecord(4, `a\old`, false)
	b = append(b, notifyRecord(5, `a\new`, true)...)
	f.Add(b)
	f.Fuzz(func(t *testing.T, b []byte) {
		parseNotify(b, func(k EventKind, p, np []string) {
			for _, c := range append(p, np...) {
				if checkName(c) != nil {
					t.Fatalf("bad component %q", c)
				}
			}
		})
	})
}
