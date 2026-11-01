//go:build windows

package main

import (
	"fmt"

	"github.com/Higangssh/hyperbutler/internal/desktopimport"
	"github.com/Higangssh/hyperbutler/internal/engine"
	"github.com/Higangssh/hyperbutler/internal/vm"
)

// importSpaceMargin is the share, in percent, an import needs on top of what it copies.
const importSpaceMargin = 10

// importSpace is what an import's copy needs and where it lands: the guest's data file
// system (its guestFs report, REQ-DISK-009) and the host volume that holds data.vhdx,
// which grows by what the guest writes.
type importSpace struct {
	need    uint64 // bytes the plan copies (desktopimport.Plan.CopyBytes)
	unknown int    // volumes without a size, not in need

	guestFree  uint64
	guestKnown bool

	hostDir   string
	hostFree  uint64
	hostKnown bool
	// hostKeep is the host free space the free-space guard keeps from the guest: its
	// writes get ENOSPC below it (engine.GuardConfig.Margin).
	hostKeep uint64
}

// check refuses a copy that does not fit, with importSpaceMargin percent to spare, in
// either place. A place whose free space is unknown is not checked.
func (s importSpace) check() error {
	want := s.need + s.need/100*importSpaceMargin
	needs := fmt.Sprintf("the import copies about %s (%s with a %d%% margin)", engine.HumanSize(s.need), engine.HumanSize(want), importSpaceMargin)
	const how = "free space or import fewer items; --force-space starts it anyway"
	if s.guestKnown && want > s.guestFree {
		return fmt.Errorf("import: not enough space in the VM: %s, and its data disk has %s free; docker system prune frees space in hyperbutler, or %s", needs, engine.HumanSize(s.guestFree), how)
	}
	if s.hostKnown {
		avail := s.hostFree - min(s.hostKeep, s.hostFree)
		if want > avail {
			return fmt.Errorf("import: not enough space on the host: %s, and the volume of the data disk (%s) has %s free, %s of it kept by the free-space guard; %s", needs, s.hostDir, engine.HumanSize(s.hostFree), engine.HumanSize(s.hostKeep), how)
		}
	}
	return nil
}

// importSpaceFor gathers the free space an import of sel from plan needs and has now.
func (in *install) importSpaceFor(plan *desktopimport.Plan, sel desktopimport.Selection) (importSpace, error) {
	need, unknown, err := plan.CopyBytes(sel)
	if err != nil {
		return importSpace{}, err
	}
	s := importSpace{need: uint64(max(need, 0)), unknown: unknown, hostKeep: engine.GuardConfig.Margin}
	if st, err := engine.LoadState(in.l); err == nil && st.Phase.Live() && st.GuestFS != nil {
		s.guestFree, s.guestKnown = st.GuestFS.FreeBytes, true
	}
	if in.settings != nil {
		s.hostDir = nearestDir(in.settings.DataDir)
		if f, err := vm.FreeBytes(s.hostDir); err == nil {
			s.hostFree, s.hostKnown = f, true
		}
	}
	return s, nil
}
