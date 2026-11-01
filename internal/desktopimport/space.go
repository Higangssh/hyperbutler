package desktopimport

import (
	"errors"
	"fmt"

	"github.com/Higangssh/hyperbutler/internal/engine"
)

// CopyBytes is about how much Run would write into hyperbutler for sel, as the plan saw
// the items: the size of each new image, and of each volume that is not already there
// with the same content and labels. Images that are there (only tags added), conflicts
// that are refused, unsupported items and volumes Run refuses (live without
// Selection.AllowLiveVolumes, no helper image) count nothing. A volume of the same name
// whose content was not compared counts whole; one that --overwrite-volumes replaces
// counts twice, because Docker Desktop's content is copied to a temporary volume and
// verified before hyperbutler's is deleted, and then copied from there into the new
// volume of that name (the temporary one goes at the end). unknown is the number of
// counted volumes Docker Desktop gave no size for; they add nothing to bytes.
func (p *Plan) CopyBytes(sel Selection) (bytes int64, unknown int, err error) {
	imgs, vols, _, err := resolve(p, sel)
	if err != nil {
		return 0, 0, err
	}
	for _, ip := range imgs {
		if ip.Status == StatusNew && ip.Size > 0 {
			bytes += ip.Size
		}
	}
	for _, vp := range vols {
		replaced := vp.Status == StatusConflict || vp.Status == StatusDifferentLabels || vp.Status == StatusExists
		switch {
		case vp.Status == StatusUnsupported || vp.Status == StatusAlready || p.HelperImage == "":
			continue
		case (vp.Status == StatusConflict || vp.Status == StatusDifferentLabels) && !sel.OverwriteVolumes:
			continue
		case len(vp.UsedBy) > 0 && !sel.AllowLiveVolumes:
			continue
		case vp.Size < 0:
			unknown++
			continue
		}
		bytes += vp.Size
		if replaced && sel.OverwriteVolumes {
			bytes += vp.Size
		}
	}
	return bytes, unknown, nil
}

// SpaceMargin is the share, in percent, an import needs on top of what it copies.
const SpaceMargin = 10

// ErrNoSpace is wrapped by Run's refusal of a copy that does not fit.
var ErrNoSpace = errors.New("not enough space for the import")

// Space is what an import's copy needs and where it lands: the guest's data file system
// (its guestFs report in state.json, REQ-DISK-009) and the host volume that holds
// data.vhdx, which grows by what the guest writes. A place whose free space is not known
// is not checked.
type Space struct {
	Need    uint64 `json:"needBytes"`      // Plan.CopyBytes
	Unknown int    `json:"unknownVolumes"` // volumes without a size, not in Need

	GuestFree  uint64 `json:"guestFreeBytes"`
	GuestKnown bool   `json:"guestKnown"`

	HostDir   string `json:"hostDir,omitempty"`
	HostFree  uint64 `json:"hostFreeBytes"`
	HostKnown bool   `json:"hostKnown"`
	// HostKeep is the host free space the free-space guard keeps from the guest: its
	// writes get ENOSPC below it.
	HostKeep uint64 `json:"hostKeepBytes"`
}

type spaceError struct{ msg string }

func (e *spaceError) Error() string   { return e.msg }
func (e *spaceError) Is(t error) bool { return t == ErrNoSpace }

// Check refuses a copy that does not fit, with SpaceMargin percent to spare, in either
// place; the error wraps ErrNoSpace.
func (s Space) Check() error {
	want := s.Need + s.Need/100*SpaceMargin
	needs := fmt.Sprintf("the import copies about %s (%s with a %d%% margin)", engine.HumanSize(s.Need), engine.HumanSize(want), SpaceMargin)
	const how = "free space or import fewer items; --force-space starts it anyway"
	if s.GuestKnown && want > s.GuestFree {
		return &spaceError{fmt.Sprintf("import: not enough space in the VM: %s, and its data disk has %s free; docker system prune frees space in hyperbutler, or %s", needs, engine.HumanSize(s.GuestFree), how)}
	}
	if s.HostKnown {
		avail := s.HostFree - min(s.HostKeep, s.HostFree)
		if want > avail {
			return &spaceError{fmt.Sprintf("import: not enough space on the host: %s, and the volume of the data disk (%s) has %s free, %s of it kept by the free-space guard; %s", needs, s.HostDir, engine.HumanSize(s.HostFree), engine.HumanSize(s.HostKeep), how)}
		}
	}
	return nil
}

// Space is the free space an import of sel from plan needs, and what the installation
// has now. Run checks it before it copies anything (Selection.ForceSpace goes on
// anyway); a GUI can show it first.
func (im *Importer) Space(plan *Plan, sel Selection) (Space, error) {
	need, unknown, err := plan.CopyBytes(sel)
	if err != nil {
		return Space{}, err
	}
	var s Space
	if im.spaceFn != nil {
		s = im.spaceFn()
	} else {
		s = installSpace(im.home)
	}
	s.Need, s.Unknown = uint64(max(need, 0)), unknown
	return s, nil
}
