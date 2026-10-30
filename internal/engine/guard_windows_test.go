//go:build windows

package engine

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/Higangssh/hyperbutler/internal/guestctl"
	"github.com/Higangssh/hyperbutler/internal/spaceguard"
	"github.com/Higangssh/hyperbutler/internal/vm"
)

func newTestGuard(t *testing.T, cfg spaceguard.Config) (*hostGuard, *fakeMachine) {
	t.Helper()
	dir := testDir(t)
	vhdx := filepath.Join(dir, "data.vhdx")
	if err := os.WriteFile(vhdx, make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	m := &fakeMachine{}
	disk := &dataDisk{path: vhdx, meta: &DiskMeta{SizeBytes: MinDiskSize, BlockSize: 1 << 20, MetadataBytes: 1 << 20}}
	g, err := newHostGuard(cfg, m, &guestctl.Supervisor{}, disk, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return g, m
}

// Review finding 7a: before the guest's first status (it is booting, and its guard
// reserves everything), only the floor applies; the margin rule would pause a booting VM.
func TestHostGuardNoMarginPauseBeforeFirstStatus(t *testing.T) {
	free, err := vm.FreeBytes(testDir(t))
	if err != nil {
		t.Fatal(err)
	}
	g, m := newTestGuard(t, spaceguard.Config{Margin: free + 1<<40, Floor: 1 << 20, ResumeGap: 1 << 20, Hysteresis: 1 << 20, Quantum: 1 << 20})
	for i := 0; i < 3; i++ {
		g.tick()
	}
	if p, _, _ := m.counts(); p != 0 {
		t.Fatal("paused at the margin before the guest guard ever reported")
	}
}

// Review finding 7b: a VM the guard paused is resumed when the guard closes, so the
// shutdown request can reach the guest.
func TestHostGuardCloseResumes(t *testing.T) {
	free, err := vm.FreeBytes(testDir(t))
	if err != nil {
		t.Fatal(err)
	}
	g, m := newTestGuard(t, spaceguard.Config{Margin: free + 1<<40, Floor: free + 1<<30, ResumeGap: 1 << 20, Hysteresis: 1 << 20, Quantum: 1 << 20})
	g.tick()
	if p, _, _ := m.counts(); p != 1 {
		t.Fatal("not paused")
	}
	g.start()
	g.close()
	if _, r, _ := m.counts(); r != 1 || g.paused {
		t.Fatalf("close did not resume: resumes %d", r)
	}
}

// REQ-DISK-005: guest writes are refused before the host volume drops below 5 GB.
func TestGuardConfigMeetsReserve(t *testing.T) {
	if GuardConfig.Margin < 5<<30 || GuardConfig.Validate() != nil {
		t.Fatalf("GuardConfig %+v", GuardConfig)
	}
	var c Config
	c.defaults()
	if c.Guard != GuardConfig {
		t.Fatalf("Run's default guard %+v", c.Guard)
	}
}
