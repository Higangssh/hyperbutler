//go:build windows

package engine

import (
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/Microsoft/go-winio/pkg/guid"
)

// The runtime id is on record (OnGrant) before the first grant, and learned once.
func TestGranterRecordsBeforeGrant(t *testing.T) {
	b := &fakeBackend{}
	var got []string
	c := &Config{VMID: "hyperbutler-1-2", Backend: b, Logger: slog.New(slog.DiscardHandler),
		OnGrant: func(id string, rid guid.GUID) {
			b.calls = append(b.calls, "on grant "+id)
			got = append(got, rid.String())
		}}
	grant := c.granter()
	for _, p := range []string{`C:\d\data.vhdx`, `C:\i\system.vhd`} {
		if err := grant(p); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"runtime id hyperbutler-1-2", "on grant hyperbutler-1-2", `grant hyperbutler-1-2 C:\d\data.vhdx`, `grant hyperbutler-1-2 C:\i\system.vhd`}
	if !reflect.DeepEqual(b.calls, want) {
		t.Fatalf("calls %q\nwant %q", b.calls, want)
	}
	if rid, _ := b.RuntimeIDOf("hyperbutler-1-2"); len(got) != 1 || got[0] != rid.String() {
		t.Fatalf("OnGrant got %q", got)
	}
}

func TestRecordRuntimeID(t *testing.T) {
	var s State
	for i := range MaxPastRuntimeIDs + 3 {
		s.RecordRuntimeID(fmt.Sprintf("0000000%d-AAAA", i%10) + fmt.Sprint(i))
	}
	s.RecordRuntimeID("")
	s.RecordRuntimeID("{bad}")
	if len(s.PastRuntimeIDs) != MaxPastRuntimeIDs || s.PastRuntimeIDs[0] != "00000003-aaaa3" {
		t.Fatalf("%q", s.PastRuntimeIDs)
	}
	s.RecordRuntimeID("00000003-AAAA3") // moved to the end, not duplicated
	if n := len(s.PastRuntimeIDs); n != MaxPastRuntimeIDs || s.PastRuntimeIDs[n-1] != "00000003-aaaa3" || s.PastRuntimeIDs[0] != "00000004-aaaa4" {
		t.Fatalf("%q", s.PastRuntimeIDs)
	}
	s.Phase, s.Version = PhaseStopped, stateVersion
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	s.PastRuntimeIDs = append(s.PastRuntimeIDs, "x")
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "past runtime ids") {
		t.Fatalf("17 ids: %v", err)
	}
	s.PastRuntimeIDs = []string{"Upper"}
	if err := s.Validate(); err == nil {
		t.Fatal("accepted an uppercase id")
	}
}
