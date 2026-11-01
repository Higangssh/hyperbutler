package desktopimport

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestPlanCopyBytes(t *testing.T) {
	const mib = 1 << 20
	p := &Plan{
		HelperImage: "sha256:" + hex64('a'),
		Images: []ImagePlan{
			{ID: "sha256:" + hex64('1'), Tags: []string{"new:1"}, Size: 100 * mib, Status: StatusNew},
			{ID: "sha256:" + hex64('2'), Tags: []string{"there:1"}, Size: 200 * mib, Status: StatusAlready},
			{ID: "sha256:" + hex64('3'), Tags: []string{"tags:1"}, Size: 300 * mib, Status: StatusPartial},
			{ID: "sha256:" + hex64('4'), Tags: []string{"clash:1"}, Size: 400 * mib, Status: StatusConflict},
			{ID: "sha256:" + hex64('5'), Size: 500 * mib, Status: StatusUnsupported},
		},
		Volumes: []VolumePlan{
			{Name: "fresh", Size: 10 * mib, Status: StatusNew},
			{Name: "same", Size: 20 * mib, Status: StatusAlready},
			{Name: "uncompared", Size: 30 * mib, Status: StatusExists},
			{Name: "other", Size: 40 * mib, Status: StatusConflict},
			{Name: "live", Size: 50 * mib, Status: StatusNew, UsedBy: []string{"db"}},
			{Name: "nosize", Size: -1, Status: StatusNew},
			{Name: "nfs", Size: 70 * mib, Status: StatusUnsupported},
		},
	}
	for _, c := range []struct {
		name    string
		sel     Selection
		want    int64
		unknown int
	}{
		{"everything", Selection{AllImages: true, AllVolumes: true}, (100 + 10 + 30) * mib, 1},
		// A replaced volume counts twice: the temporary copy and the new volume (M5).
		{"overwrite and live", Selection{AllImages: true, AllVolumes: true, OverwriteVolumes: true, AllowLiveVolumes: true}, (100 + 10 + 2*30 + 2*40 + 50) * mib, 1},
		{"one image by tag", Selection{Images: []string{"new:1"}}, 100 * mib, 0},
		{"an image that is there", Selection{Images: []string{"there:1", "tags:1"}}, 0, 0},
		{"named twice", Selection{Images: []string{"new:1", "new:1"}, Volumes: []string{"fresh", "fresh"}}, 110 * mib, 0},
	} {
		got, unknown, err := p.CopyBytes(c.sel)
		if err != nil || got != c.want || unknown != c.unknown {
			t.Errorf("%s: %d MiB, %d unknown, %v; want %d MiB, %d unknown", c.name, got/mib, unknown, err, c.want/mib, c.unknown)
		}
	}
	if _, _, err := p.CopyBytes(Selection{Volumes: []string{"missing"}}); err == nil {
		t.Error("a volume that is not in the plan was counted")
	}
	// Without a helper image no volume is copied.
	p.HelperImage = ""
	if got, _, _ := p.CopyBytes(Selection{AllVolumes: true}); got != 0 {
		t.Errorf("volumes without a helper image: %d bytes", got)
	}
}

func TestSpaceCheck(t *testing.T) {
	const gib = 1 << 30
	ok := Space{Need: 10 * gib, GuestFree: 20 * gib, GuestKnown: true, HostDir: `D:\`, HostFree: 100 * gib, HostKnown: true, HostKeep: 5 * gib}
	if err := ok.Check(); err != nil {
		t.Fatalf("fits: %v", err)
	}
	for _, c := range []struct {
		name string
		edit func(*Space)
		want string // "" for no refusal
	}{
		{"guest just fits with the margin", func(s *Space) { s.GuestFree = 11 * gib }, ""},
		{"guest fits without the margin only", func(s *Space) { s.GuestFree = 10*gib + gib/2 }, "not enough space in the VM"},
		{"guest unknown", func(s *Space) { s.GuestFree, s.GuestKnown = 0, false }, ""},
		{"host short above the guard's share", func(s *Space) { s.HostFree = 15 * gib }, "not enough space on the host"},
		{"host just fits", func(s *Space) { s.HostFree = 16 * gib }, ""},
		{"host below the guard's share", func(s *Space) { s.HostFree = 2 * gib }, "not enough space on the host"},
		{"host unknown", func(s *Space) { s.HostFree, s.HostKnown = 0, false }, ""},
		{"nothing to copy, full disk", func(s *Space) { s.Need, s.GuestFree = 0, 0 }, ""},
	} {
		s := ok
		c.edit(&s)
		err := s.Check()
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: refused: %v", c.name, err)
		case c.want != "" && (!errors.Is(err, ErrNoSpace) || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "--force-space")):
			t.Errorf("%s: %v, want %q and --force-space", c.name, err, c.want)
		}
	}
	err := Space{Need: 10 * gib, GuestFree: gib, GuestKnown: true}.Check()
	if err == nil || !strings.Contains(err.Error(), "10.0 GiB (11.0 GiB with a 10% margin)") || !strings.Contains(err.Error(), "1.0 GiB free") {
		t.Errorf("message: %v", err)
	}
}

// Run checks the space itself (the GUI gets it with the CLI): a copy that does not fit
// is refused before anything is touched, and ForceSpace goes on with a note.
func TestRunChecksSpace(t *testing.T) {
	p := newPair(t)
	p.src.addImage("busybox:latest")
	im := p.open(t)
	im.spaceFn = func() Space { return Space{GuestKnown: true, GuestFree: 10} }
	plan, err := im.Plan(context.Background(), PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	n := len(p.src.reqs())
	if _, err := im.Run(context.Background(), plan, Selection{AllImages: true}, nil); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("run: %v", err)
	}
	if len(p.dst.images) != 0 || len(p.src.reqs()) != n {
		t.Error("a refused import touched an engine")
	}
	var notes []string
	res, err := im.Run(context.Background(), plan, Selection{AllImages: true, ForceSpace: true}, func(e Event) {
		if e.Kind == EventNote {
			notes = append(notes, e.Message)
		}
	})
	if err != nil || !res.OK() || !strings.Contains(strings.Join(notes, ";"), "going on") {
		t.Fatalf("forced: %v %+v %v", err, res, notes)
	}
}

func hex64(c byte) string {
	b := make([]byte, 64)
	for i := range b {
		b[i] = c
	}
	return string(b)
}
