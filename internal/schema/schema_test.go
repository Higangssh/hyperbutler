package schema

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Higangssh/hyperbutler/internal/leaktest"
)

// TestMain fails the package's tests if any goroutine is still running after
// they finish (ADR 0003, guardrail 1).
func TestMain(m *testing.M) {
	leaktest.VerifyTestMain(m)
}

// TestStatusRoundTrip checks that a status document reads back as written: the GUI
// decodes what the CLI prints, and cmd/hyperbutler's schema_test.go pins the names.
func TestStatusRoundTrip(t *testing.T) {
	in := Status{Schema: Version, Running: true, SetUp: true, Home: `C:\h`, MemoryMB: 2048, CPUs: 2,
		GuestDisk: &GuestDisk{SizeBytes: 10, UsedBytes: 4, FreeBytes: 6, UsedPercent: 40}}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"memoryMB":2048`) || !strings.Contains(string(b), `"dockerContextSelected":false`) {
		t.Fatalf("unexpected encoding: %s", b)
	}
	var out Status
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.MemoryMB != 2048 || out.GuestDisk == nil || out.GuestDisk.UsedBytes != 4 {
		t.Fatalf("round trip: %+v", out)
	}
}
