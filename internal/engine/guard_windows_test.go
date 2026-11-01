//go:build windows

package engine

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// REQ-DISK-010: the guard reports a pause with the volume, the free space and the free
// space it resumes at; again while paused when the free space moves by a MiB or more;
// and the resume, once.
func TestHostGuardReportsPause(t *testing.T) {
	cfg := spaceguard.Config{Margin: 4 << 30, Floor: 1 << 30, ResumeGap: 256 << 20, Hysteresis: 1 << 20, Quantum: 1 << 20}
	g, m := newTestGuard(t, cfg)
	free := uint64(10 << 30)
	g.freeBytes = func(string) (uint64, error) { return free, nil }
	clock := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	g.now = func() time.Time { return clock }
	var got []*GuardPauseState
	g.onPause = func(p *GuardPauseState) { got = append(got, p) }
	if g.volume == "" {
		t.Fatal("no volume")
	}

	g.tick()
	if len(got) != 0 {
		t.Fatalf("reported %d pauses with space to spare", len(got))
	}
	free = 900 << 20 // below the floor
	g.tick()
	if p, _, _ := m.counts(); p != 1 || len(got) != 1 || got[0] == nil {
		t.Fatalf("pauses %d, reports %v", p, got)
	}
	first := got[0]
	if first.Volume != g.volume || first.FreeBytes != free || first.ResumeAtBytes != cfg.Floor+cfg.ResumeGap || first.Since.IsZero() {
		t.Fatalf("pause report %+v", first)
	}
	free += 512 << 10 // less than a MiB: not reported again
	g.tick()
	if len(got) != 1 {
		t.Fatalf("reported a %d-byte change", 512<<10)
	}
	free = 1<<30 + 100<<20 // above the floor, below the resume point: still paused
	g.tick()
	if len(got) != 1 {
		t.Fatal("reported again less than pauseReportEvery after the last report")
	}
	clock = clock.Add(pauseReportEvery)
	g.tick()
	if p, r, _ := m.counts(); p != 1 || r != 0 || len(got) != 2 || got[1] == nil || got[1].FreeBytes != free || !got[1].Since.Equal(first.Since) {
		t.Fatalf("update while paused: %v", got)
	}
	free = cfg.Floor + cfg.ResumeGap
	g.tick()
	if _, r, _ := m.counts(); r != 1 || len(got) != 3 || got[2] != nil {
		t.Fatalf("resumes %d, reports %v", r, got)
	}
	g.tick()
	if len(got) != 3 {
		t.Fatalf("resume reported %d times", len(got)-2)
	}
}

// Review M3: while paused, a free space that swings by more than a MiB on every tick
// (100 ms) is reported at most every pauseReportEvery, not on every tick; the pause and
// the resume are still reported on the tick they happen.
func TestHostGuardPauseReportRate(t *testing.T) {
	cfg := spaceguard.Config{Margin: 4 << 30, Floor: 1 << 30, ResumeGap: 256 << 20, Hysteresis: 1 << 20, Quantum: 1 << 20}
	g, m := newTestGuard(t, cfg)
	ticks := 0
	g.freeBytes = func(string) (uint64, error) {
		if ticks%2 == 0 {
			return 500 << 20, nil
		}
		return 700 << 20, nil // 200 MiB more: well above pauseReportStep
	}
	clock := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	g.now = func() time.Time { return clock }
	var got []*GuardPauseState
	g.onPause = func(p *GuardPauseState) { got = append(got, p) }
	step := func() {
		g.tick()
		ticks++
		clock = clock.Add(g.interval)
	}

	step()
	if p, _, _ := m.counts(); p != 1 || len(got) != 1 || got[0] == nil {
		t.Fatalf("the pause was not reported on its tick: pauses %d, reports %d", p, len(got))
	}
	const n = 100 // 10 s of ticks
	for i := 0; i < n; i++ {
		step()
	}
	// At most one report per pauseReportEvery (20 ticks): 4 or 5 in 10 s (a tick
	// whose free space equals the last report's is skipped), not 100.
	most := n / int(pauseReportEvery/g.interval)
	if updates := len(got) - 1; updates > most || updates < most-1 {
		t.Fatalf("%d updates in %d ticks of a swinging free space, want %d or %d", updates, n, most-1, most)
	}
	for _, r := range got {
		if r == nil {
			t.Fatal("a resume was reported while paused")
		}
	}

	g.freeBytes = func(string) (uint64, error) { return cfg.Floor + cfg.ResumeGap, nil }
	before := len(got)
	step()
	if _, r, _ := m.counts(); r != 1 || len(got) != before+1 || got[before] != nil {
		t.Fatalf("the resume was not reported on its tick: resumes %d, reports %v", r, got[before:])
	}
}

// A guard closed while it has the VM paused reports the resume.
func TestHostGuardCloseReportsResume(t *testing.T) {
	cfg := spaceguard.Config{Margin: 4 << 30, Floor: 1 << 30, ResumeGap: 256 << 20, Hysteresis: 1 << 20, Quantum: 1 << 20}
	g, _ := newTestGuard(t, cfg)
	g.freeBytes = func(string) (uint64, error) { return 1 << 20, nil }
	var got []*GuardPauseState
	g.onPause = func(p *GuardPauseState) { got = append(got, p) }
	g.tick()
	g.start()
	g.close()
	if len(got) != 2 || got[0] == nil || got[1] != nil {
		t.Fatalf("reports %v", got)
	}
}

func TestVolumeOf(t *testing.T) {
	dir := testDir(t)
	v := volumeOf(dir)
	if v == "" || strings.HasSuffix(v, `\`) || !strings.HasPrefix(strings.ToLower(dir), strings.ToLower(v)) {
		t.Fatalf("volumeOf(%s) = %q", dir, v)
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
