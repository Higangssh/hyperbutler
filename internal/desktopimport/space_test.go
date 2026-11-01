package desktopimport

import "testing"

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
		{"overwrite and live", Selection{AllImages: true, AllVolumes: true, OverwriteVolumes: true, AllowLiveVolumes: true}, (100 + 10 + 30 + 40 + 50) * mib, 1},
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

func hex64(c byte) string {
	b := make([]byte, 64)
	for i := range b {
		b[i] = c
	}
	return string(b)
}
