//go:build windows

package main

import (
	"strings"
	"testing"
)

func TestImportSpaceCheck(t *testing.T) {
	const gib = 1 << 30
	ok := importSpace{need: 10 * gib, guestFree: 20 * gib, guestKnown: true, hostDir: `D:\`, hostFree: 100 * gib, hostKnown: true, hostKeep: 5 * gib}
	if err := ok.check(); err != nil {
		t.Fatalf("fits: %v", err)
	}
	for _, c := range []struct {
		name string
		edit func(*importSpace)
		want string // "" for no refusal
	}{
		{"guest just fits with the margin", func(s *importSpace) { s.guestFree = 11 * gib }, ""},
		{"guest fits without the margin only", func(s *importSpace) { s.guestFree = 10*gib + gib/2 }, "not enough space in the VM"},
		{"guest unknown", func(s *importSpace) { s.guestFree, s.guestKnown = 0, false }, ""},
		{"host short above the guard's share", func(s *importSpace) { s.hostFree = 15 * gib }, "not enough space on the host"},
		{"host just fits", func(s *importSpace) { s.hostFree = 16 * gib }, ""},
		{"host below the guard's share", func(s *importSpace) { s.hostFree = 2 * gib }, "not enough space on the host"},
		{"host unknown", func(s *importSpace) { s.hostFree, s.hostKnown = 0, false }, ""},
		{"nothing to copy, full disk", func(s *importSpace) { s.need, s.guestFree = 0, 0 }, ""},
	} {
		s := ok
		c.edit(&s)
		err := s.check()
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: refused: %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "--force-space")):
			t.Errorf("%s: %v, want %q and --force-space", c.name, err, c.want)
		}
	}
	err := importSpace{need: 10 * gib, guestFree: gib, guestKnown: true}.check()
	if err == nil || !strings.Contains(err.Error(), "10.0 GiB (11.0 GiB with a 10% margin)") || !strings.Contains(err.Error(), "1.0 GiB free") {
		t.Errorf("message: %v", err)
	}
}
