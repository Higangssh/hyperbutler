package desktopimport

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/Higangssh/hyperbutler/internal/desktopimport/source"
)

// assertReadOnly fails unless every request Docker Desktop's fake saw was a GET, a
// helper create or a helper removal, and no helper is left.
func assertReadOnly(t *testing.T, src *fakeDocker) {
	t.Helper()
	for _, r := range src.reqs() {
		method, path, _ := strings.Cut(r, " ")
		switch {
		case method == "GET":
		case method == "POST" && path == "/containers/create":
		case method == "DELETE" && strings.HasPrefix(path, "/containers/"):
		default:
			t.Errorf("Docker Desktop got a write: %s", r)
		}
	}
	if n := src.helperCount(); n != 0 {
		t.Errorf("%d helper containers left in Docker Desktop", n)
	}
}

func run(t *testing.T, im *Importer, sel Selection) (*Plan, *Result) {
	t.Helper()
	plan, err := im.Plan(context.Background(), PlanOptions{CompareVolumes: true})
	if err != nil {
		t.Fatal(err)
	}
	res, err := im.Run(context.Background(), plan, sel, nil)
	if err != nil {
		t.Fatal(err)
	}
	return plan, res
}

func outcomes(res *Result) map[string]Outcome {
	m := map[string]Outcome{}
	for _, it := range res.Items {
		m[it.Kind+":"+it.Name] = it.Outcome
	}
	return m
}

func files(content ...string) []fFile {
	var out []fFile
	for i, c := range content {
		out = append(out, fFile{Name: string(rune('a'+i)) + ".txt", Mode: 0o644, Uid: 999, Content: c})
	}
	return append(out, fFile{Name: "sub", Dir: true, Mode: 0o700, Uid: 999})
}

func TestImportImagesThenAgain(t *testing.T) {
	p := newPair(t)
	bb := p.src.addImage("busybox:latest", "busybox:1.37")
	al := p.src.addImage("alpine:3.22")
	im := p.open(t)

	_, res := run(t, im, Selection{AllImages: true})
	if !res.OK() {
		t.Fatalf("not ok: %+v", res)
	}
	got := outcomes(res)
	if got["image:busybox:1.37"] != OutcomeImported || got["image:alpine:3.22"] != OutcomeImported {
		t.Fatalf("outcomes %v", got)
	}
	if res.Bytes == 0 {
		t.Error("no bytes counted")
	}
	for _, src := range []*fImage{bb, al} {
		d := p.dst.images[src.ID]
		if d == nil {
			t.Fatalf("%v not in hyperbutler", src.Tags)
		}
		dt, st := slices.Sorted(slices.Values(d.Tags)), slices.Sorted(slices.Values(src.Tags))
		if !slices.Equal(dt, st) {
			t.Errorf("tags %v, want %v", dt, st)
		}
	}
	if !res.Source.Unchanged || res.Source.Images != 2 {
		t.Errorf("source check %+v", res.Source)
	}

	// The second run copies nothing.
	plan, res := run(t, im, Selection{AllImages: true})
	for _, ip := range plan.Images {
		if ip.Status != StatusAlready {
			t.Errorf("plan %s: %s", ip.Name(), ip.Status)
		}
	}
	if !res.OK() || res.Bytes != 0 {
		t.Fatalf("second run: %+v", res)
	}
	for _, it := range res.Items {
		if it.Outcome != OutcomeAlready {
			t.Errorf("second run %s: %s", it.Name, it.Outcome)
		}
	}
	assertReadOnly(t, p.src)
}

func TestImagePartialTagsAndConflict(t *testing.T) {
	p := newPair(t)
	bb := p.src.addImage("busybox:latest", "busybox:1.37")
	al := p.src.addImage("alpine:3.22")
	// hyperbutler has busybox under one tag, and another image as alpine:3.22.
	p.dst.images[bb.ID] = &fImage{ID: bb.ID, Tags: []string{"busybox:latest"}, Layers: bb.Layers, Created: bb.Created}
	other := p.dst.addImage("alpine:3.22")
	im := p.open(t)

	plan, res := run(t, im, Selection{Images: []string{"busybox", "alpine:3.22"}})
	st := map[string]Status{}
	for _, ip := range plan.Images {
		st[ip.Name()] = ip.Status
	}
	if st["busybox:1.37"] != StatusPartial || st["alpine:3.22"] != StatusConflict {
		t.Fatalf("plan %v", st)
	}
	got := outcomes(res)
	if got["image:busybox:1.37"] != OutcomeTagged || got["image:alpine:3.22"] != OutcomeRefused {
		t.Fatalf("outcomes %v", got)
	}
	if !slices.Contains(p.dst.images[bb.ID].Tags, "busybox:1.37") {
		t.Error("missing tag not added")
	}
	if p.dst.images[other.ID] == nil || p.dst.images[al.ID] != nil {
		t.Error("the conflicting image was replaced")
	}
	if res.OK() {
		t.Error("a refused image is not OK")
	}
	if res.Bytes != 0 {
		t.Errorf("tagging copied %d bytes", res.Bytes)
	}
	assertReadOnly(t, p.src)
}

func TestImageLoadFailureLeavesNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(f *fakeDocker)
		want string
	}{
		{"load error", func(f *fakeDocker) { f.failLoad = "unexpected EOF" }, "unexpected EOF"},
		{"other id", func(f *fakeDocker) { f.otherLoadID = true }, "verification failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPair(t)
			p.src.addImage("busybox:latest")
			keep := p.dst.addImage("nginx:1.27")
			tc.set(p.dst)
			im := p.open(t)
			_, res := run(t, im, Selection{AllImages: true})
			if len(res.Items) != 1 || res.Items[0].Outcome != OutcomeFailed || !strings.Contains(res.Items[0].Reason, tc.want) {
				t.Fatalf("items %+v", res.Items)
			}
			if len(p.dst.images) != 1 || p.dst.images[keep.ID] == nil {
				t.Errorf("hyperbutler's images after the failure: %v", p.dst.images)
			}
			if !res.Source.Unchanged {
				t.Errorf("source %+v", res.Source)
			}
			assertReadOnly(t, p.src)
		})
	}
}

func TestVolumeCopyThenAgain(t *testing.T) {
	p := newPair(t)
	p.src.addImage("busybox:latest")
	p.src.addVolume("pgdata", files("hello", "world")...)
	p.src.addContainer("db", "exited", "pgdata")
	p.dst.addImage("busybox:latest")
	im := p.open(t)

	_, res := run(t, im, Selection{AllVolumes: true})
	if !res.OK() {
		t.Fatalf("not ok: %+v", res)
	}
	it := res.Items[0]
	if it.Outcome != OutcomeImported || !strings.HasPrefix(it.Hash, "sha256:") {
		t.Fatalf("item %+v", it)
	}
	dv := p.dst.volumes["pgdata"]
	if dv == nil || len(dv.Files) != 3 || dv.Files[0].Content != "hello" || dv.Files[0].Uid != 999 {
		t.Fatalf("copy %+v", dv)
	}
	if dv.Labels["com.docker.compose.volume"] != "pgdata" || dv.Labels[importLabel] == "" {
		t.Errorf("labels %v", dv.Labels)
	}
	if p.dst.helperCount() != 0 {
		t.Error("helper left in hyperbutler")
	}
	if !res.Source.Unchanged || res.Source.HashedVolumes != 1 || res.Source.Containers != 1 {
		t.Errorf("source %+v", res.Source)
	}

	plan, res := run(t, im, Selection{AllVolumes: true})
	if plan.Volumes[0].Status != StatusAlready {
		t.Errorf("plan %+v", plan.Volumes[0])
	}
	if !res.OK() || res.Bytes != 0 || res.Items[0].Outcome != OutcomeAlready {
		t.Fatalf("second run %+v", res)
	}
	assertReadOnly(t, p.src)
}

func TestVolumeHashMismatchRemovesTargetOnly(t *testing.T) {
	p := newPair(t)
	p.src.addImage("busybox:latest")
	p.src.addVolume("data", files("abc")...)
	p.dst.addImage("busybox:latest")
	p.dst.corruptPut = true
	im := p.open(t)
	_, res := run(t, im, Selection{Volumes: []string{"data"}})
	it := res.Items[0]
	if it.Outcome != OutcomeFailed || !strings.Contains(it.Reason, "verification failed") {
		t.Fatalf("item %+v", it)
	}
	if _, ok := p.dst.volumes["data"]; ok {
		t.Error("the bad copy is still in hyperbutler")
	}
	if v := p.src.volumes["data"]; v == nil || v.Files[0].Content != "abc" {
		t.Error("Docker Desktop's volume changed")
	}
	if !res.Source.Unchanged || res.OK() {
		t.Errorf("source %+v ok %v", res.Source, res.OK())
	}
	assertReadOnly(t, p.src)
}

func TestVolumeExistsWithOtherContent(t *testing.T) {
	p := newPair(t)
	p.src.addImage("busybox:latest")
	p.src.addVolume("data", files("new")...)
	p.dst.addImage("busybox:latest")
	p.dst.addVolume("data", files("old")...)
	im := p.open(t)
	plan, res := run(t, im, Selection{Volumes: []string{"data"}})
	if plan.Volumes[0].Status != StatusConflict {
		t.Errorf("plan %+v", plan.Volumes[0])
	}
	if res.Items[0].Outcome != OutcomeRefused || !strings.Contains(res.Items[0].Reason, "different content") {
		t.Fatalf("item %+v", res.Items[0])
	}
	if p.dst.volumes["data"].Files[0].Content != "old" {
		t.Error("refused, but hyperbutler's volume changed")
	}
	_, res = run(t, im, Selection{Volumes: []string{"data"}, OverwriteVolumes: true})
	if res.Items[0].Outcome != OutcomeImported || p.dst.volumes["data"].Files[0].Content != "new" {
		t.Fatalf("overwrite: %+v", res.Items[0])
	}
	assertReadOnly(t, p.src)
}

func TestLiveVolume(t *testing.T) {
	p := newPair(t)
	p.src.addImage("busybox:latest")
	p.src.addVolume("data", files("x")...)
	p.src.addContainer("web-1", "running", "data")
	p.dst.addImage("busybox:latest")
	im := p.open(t)
	plan, res := run(t, im, Selection{AllVolumes: true})
	if !slices.Equal(plan.Volumes[0].UsedBy, []string{"web-1"}) {
		t.Errorf("used by %v", plan.Volumes[0].UsedBy)
	}
	it := res.Items[0]
	if it.Outcome != OutcomeRefused || !strings.Contains(it.Reason, "web-1") || !strings.Contains(it.Reason, "stop them in Docker Desktop") {
		t.Fatalf("item %+v", it)
	}
	if _, ok := p.dst.volumes["data"]; ok {
		t.Error("a refused volume was created")
	}
	_, res = run(t, im, Selection{AllVolumes: true, AllowLiveVolumes: true})
	it = res.Items[0]
	if it.Outcome != OutcomeImported || !strings.Contains(it.Reason, "may be inconsistent") {
		t.Fatalf("allowed: %+v", it)
	}
	if p.src.containers[firstID(p.src)].State != "running" {
		t.Error("the running container was touched")
	}
	assertReadOnly(t, p.src)
}

func firstID(f *fakeDocker) string {
	for id := range f.containers {
		return id
	}
	return ""
}

func TestVolumeChangedDuringCopy(t *testing.T) {
	p := newPair(t)
	p.src.addImage("busybox:latest")
	p.src.addVolume("data", files("one")...)
	p.dst.addImage("busybox:latest")
	im := p.open(t)
	plan, err := im.Plan(context.Background(), PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	reads := 0
	p.src.mutateRead = func(v *fVolume) bool {
		reads++
		if reads == 2 { // the copy, after the snapshot's read
			v.Files[0].Content = "two"
			return true
		}
		return false
	}
	res, err := im.Run(context.Background(), plan, Selection{AllVolumes: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if it := res.Items[0]; it.Outcome != OutcomeFailed || !strings.Contains(it.Reason, "changed during the copy") {
		t.Fatalf("item %+v", it)
	}
	if _, ok := p.dst.volumes["data"]; ok {
		t.Error("the inconsistent copy is still in hyperbutler")
	}
	// The fake changed the source: the check says so.
	if res.Source.Unchanged || !strings.Contains(strings.Join(res.Source.Differences, ";"), "volume data's content changed") {
		t.Errorf("source %+v", res.Source)
	}
}

func TestCancelDuringVolumeCopy(t *testing.T) {
	p := newPair(t)
	p.src.addImage("busybox:latest")
	p.src.addVolume("data", files("abc", "def")...)
	p.dst.addImage("busybox:latest")
	p.dst.blockPut = make(chan struct{})
	im := p.open(t)
	plan, err := im.Plan(context.Background(), PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res, err := im.Run(ctx, plan, Selection{AllVolumes: true}, func(e Event) {
		if e.Kind == EventProgress && e.Phase == PhaseCopy {
			cancel()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Canceled || res.Items[0].Outcome != OutcomeCanceled {
		t.Fatalf("result %+v", res)
	}
	if _, ok := p.dst.volumes["data"]; ok {
		t.Error("the canceled copy is still in hyperbutler")
	}
	if p.dst.helperCount() != 0 {
		t.Error("helper left in hyperbutler")
	}
	if !res.Source.Unchanged {
		t.Errorf("source %+v", res.Source)
	}
	assertReadOnly(t, p.src)
}

func TestHelperRemovedOnError(t *testing.T) {
	p := newPair(t)
	p.src.addImage("busybox:latest")
	p.src.addVolume("data", files("abc")...)
	p.dst.addImage("busybox:latest")
	im := p.open(t)
	plan, err := im.Plan(context.Background(), PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// The target's archive API fails: both helpers must still go.
	p.dst.mu.Lock()
	p.dst.blockPut = nil
	p.dst.mu.Unlock()
	reads := 0
	p.src.onArchive = func(f *fakeDocker) {
		reads++
		if reads == 2 {
			p.dst.mu.Lock()
			for _, c := range p.dst.containers {
				c.ReadOnly = true // the PUT is refused
			}
			p.dst.mu.Unlock()
		}
	}
	res, err := im.Run(context.Background(), plan, Selection{AllVolumes: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Items[0].Outcome != OutcomeFailed {
		t.Fatalf("item %+v", res.Items[0])
	}
	if p.dst.helperCount() != 0 {
		t.Error("helper left in hyperbutler")
	}
	if _, ok := p.dst.volumes["data"]; ok {
		t.Error("the failed copy is still in hyperbutler")
	}
	if !res.Source.Unchanged {
		t.Errorf("source %+v", res.Source)
	}
	assertReadOnly(t, p.src)
}

func TestTemporaryHelperImage(t *testing.T) {
	p := newPair(t)
	p.src.addImage("busybox:latest")
	p.src.addVolume("data", files("abc")...)
	im := p.open(t)
	_, res := run(t, im, Selection{AllVolumes: true})
	if !res.OK() {
		t.Fatalf("%+v", res)
	}
	if len(p.dst.images) != 0 {
		t.Errorf("the temporary helper image is still in hyperbutler: %v", p.dst.images)
	}
	if p.dst.volumes["data"] == nil {
		t.Error("no volume")
	}
	assertReadOnly(t, p.src)
}

func TestStaleHelpers(t *testing.T) {
	p := newPair(t)
	p.src.addImage("busybox:latest")
	stale := p.src.addContainer("hyperbutler-import-old", "created", "")
	stale.Labels[source.HelperLabel] = source.HelperLabelValue
	stale.Entrypoint = []string{source.HelperEntrypoint}
	// Labeled, but it ran: not ours to remove.
	ran := p.src.addContainer("lookalike", "exited", "")
	ran.Labels[source.HelperLabel] = source.HelperLabelValue
	ran.Entrypoint = []string{source.HelperEntrypoint}
	ran.Started = true
	im := p.open(t)
	_, res := run(t, im, Selection{AllImages: true})
	if _, ok := p.src.containers[stale.ID]; ok {
		t.Error("the stale helper is still there")
	}
	if _, ok := p.src.containers[ran.ID]; !ok {
		t.Error("a container that ran was removed")
	}
	if !strings.Contains(strings.Join(res.Notes, ";"), "left alone") {
		t.Errorf("notes %v", res.Notes)
	}
}

// A killed import leaves a half-filled volume and its helper in hyperbutler: the next
// import removes both and copies the volume again. A volume the import did not create is
// never removed, even with a helper that claims it.
func TestInterruptedCopyIsCleanedUp(t *testing.T) {
	p := newPair(t)
	p.src.addImage("busybox:latest")
	p.src.addVolume("data", files("full")...)
	p.src.addVolume("mine", files("src")...)
	p.dst.addImage("busybox:latest")
	half := p.dst.addVolume("data", files("fu")...)
	half.Labels = map[string]string{importLabel: "docker-desktop"}
	own := p.dst.addVolume("mine", files("dst")...)
	for _, v := range []string{"data", "mine"} {
		h := p.dst.addContainer("hyperbutler-import-x"+v, "created", v)
		h.Labels = map[string]string{source.HelperLabel: source.HelperLabelValue, copyingLabel: v}
	}
	im := p.open(t)
	plan, res := run(t, im, Selection{AllVolumes: true})
	st := map[string]Status{}
	for _, vp := range plan.Volumes {
		st[vp.Name] = vp.Status
	}
	if st["data"] != StatusNew || st["mine"] != StatusConflict {
		t.Fatalf("plan %v (notes %v)", st, plan.Notes)
	}
	got := outcomes(res)
	if got["volume:data"] != OutcomeImported || got["volume:mine"] != OutcomeRefused {
		t.Fatalf("outcomes %v", got)
	}
	if p.dst.volumes["data"].Files[0].Content != "full" || p.dst.volumes["mine"] != own || own.Files[0].Content != "dst" {
		t.Error("wrong volumes in hyperbutler")
	}
	if p.dst.helperCount() != 0 {
		t.Error("helpers left in hyperbutler")
	}
	assertReadOnly(t, p.src)
}

func TestSourceChangeIsReported(t *testing.T) {
	p := newPair(t)
	p.src.addImage("busybox:latest")
	c := p.src.addContainer("web", "running", "")
	im := p.open(t)
	plan, err := im.Plan(context.Background(), PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	p.dst.onLoadRead = func() {
		p.src.mu.Lock()
		c.State = "exited"
		p.src.mu.Unlock()
	}
	res, err := im.Run(context.Background(), plan, Selection{AllImages: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Source.Unchanged || res.OK() || !strings.Contains(strings.Join(res.Source.Differences, ";"), "is now") {
		t.Errorf("source %+v", res.Source)
	}
}

func TestDesktopNotRunning(t *testing.T) {
	p := newPair(t)
	p.src = nil
	_, err := Discover(context.Background(), p.o)
	if !errors.Is(err, ErrDesktopNotRunning) || !strings.Contains(err.Error(), "start Docker Desktop first") {
		t.Fatalf("err %v", err)
	}
	if _, err := Open(context.Background(), p.o); !errors.Is(err, ErrDesktopNotRunning) {
		t.Fatalf("open: %v", err)
	}
}

func TestTargetNotRunningAndSameEngine(t *testing.T) {
	p := newPair(t)
	o := p.o
	o.TargetPipe = `\\.\pipe\nothing`
	if _, err := Open(context.Background(), o); !errors.Is(err, ErrTargetNotRunning) {
		t.Fatalf("err %v", err)
	}
	o = p.o
	o.TargetPipe = srcPipe
	if _, err := Open(context.Background(), o); !errors.Is(err, ErrSameEngine) {
		t.Fatalf("same pipe: %v", err)
	}
	p.dst.id = "desktop"
	if _, err := Open(context.Background(), p.o); !errors.Is(err, ErrSameEngine) {
		t.Fatalf("same id: %v", err)
	}
}

func TestSelection(t *testing.T) {
	p := &Plan{Images: []ImagePlan{
		{ID: "sha256:aaaaaaaaaaaaaaaa1111", Tags: []string{"busybox:latest"}},
		{ID: "sha256:aaaaaaaaaaaaaaaa2222", Tags: []string{"ghcr.io/o/app:1"}},
		{ID: "sha256:bbbb", Status: StatusUnsupported, Reason: "untagged"},
	}, Volumes: []VolumePlan{{Name: "pg"}, {Name: "nfs", Status: StatusUnsupported}}}
	imgs, vols, skipped, err := resolve(p, Selection{Images: []string{"docker.io/library/busybox", "ghcr.io/o/app:1", "busybox:latest"}, Volumes: []string{"pg"}})
	if err != nil || len(imgs) != 2 || len(vols) != 1 || len(skipped) != 0 {
		t.Fatalf("%v %v %v %v", imgs, vols, skipped, err)
	}
	for _, bad := range []string{"aaaaaaaaaaaa", "nginx", "../x", "a/../b"} {
		if _, _, _, err := resolve(p, Selection{Images: []string{bad}}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, _, _, err := resolve(p, Selection{Volumes: []string{"../x"}}); err == nil {
		t.Error("bad volume name accepted")
	}
	if _, _, _, err := resolve(p, Selection{Volumes: []string{"missing"}}); err == nil {
		t.Error("missing volume accepted")
	}
	imgs, vols, skipped, _ = resolve(p, Selection{AllImages: true, AllVolumes: true})
	if len(imgs) != 2 || len(vols) != 1 || len(skipped) != 2 {
		t.Errorf("all: %d %d %d", len(imgs), len(vols), len(skipped))
	}
	if ip, err := findImage(p, "aaaaaaaaaaaaaaaa2"); err != nil || ip.Tags[0] != "ghcr.io/o/app:1" {
		t.Errorf("prefix: %v %v", ip, err)
	}
}

func TestPipeFromHost(t *testing.T) {
	for host, want := range map[string]string{
		"npipe:////./pipe/dockerDesktopLinuxEngine": `\\.\pipe\dockerDesktopLinuxEngine`,
		"npipe://./pipe/x":                          "",
		"tcp://127.0.0.1:2375":                      "",
		"npipe:////server/pipe/x":                   "",
		"unix:///var/run/docker.sock":               "",
	} {
		got, err := pipeFromHost(host)
		if got != want || (want == "") != (err != nil) {
			t.Errorf("%q: %q, %v", host, got, err)
		}
	}
}

func TestEndpointFromContext(t *testing.T) {
	dir := t.TempDir()
	ep, err := findEndpoint(Options{DockerConfig: dir})
	if err != nil || ep.Pipe != DesktopPipe || ep.Via != ViaDefault {
		t.Fatalf("no context: %+v %v", ep, err)
	}
	writeContext(t, dir, `{"Name":"desktop-linux","Metadata":{},"Endpoints":{"docker":{"Host":"npipe:////./pipe/otherDesktop"}}}`)
	ep, err = findEndpoint(Options{DockerConfig: dir})
	if err != nil || ep.Pipe != `\\.\pipe\otherDesktop` || ep.Via != ViaContext {
		t.Fatalf("context: %+v %v", ep, err)
	}
	writeContext(t, dir, `{"Name":"desktop-linux","Metadata":{},"Endpoints":{"docker":{"Host":"tcp://10.0.0.1:2375"}}}`)
	ep, _ = findEndpoint(Options{DockerConfig: dir})
	if ep.Pipe != DesktopPipe || ep.Note == "" {
		t.Fatalf("tcp context: %+v", ep)
	}
}

func TestCompareSnapshots(t *testing.T) {
	b := &snapshot{images: map[string]string{"i1": "a:1", "i2": "b:1"}, volumes: map[string]bool{"v1": true, "v2": true},
		hashes: map[string]string{"v1": "h1", "v2": "h2"}, containers: map[string]string{"c1": "web running i1"}}
	same := compareSnapshots(b, b, nil)
	if !same.Unchanged || same.Images != 2 || same.Volumes != 2 || same.Containers != 1 {
		t.Fatalf("same %+v", same)
	}
	a := &snapshot{images: map[string]string{"i1": "a:1,a:2", "i3": "c:1"}, volumes: map[string]bool{"v1": true, "v2": true},
		hashes: map[string]string{"v1": "x", "v2": "y"}, containers: map[string]string{"c1": "web exited i1"}, helpers: []string{"h"}}
	c := compareSnapshots(b, a, map[string]bool{"v2": true})
	if c.Unchanged || len(c.Differences) != 6 || len(c.Warnings) != 1 {
		t.Fatalf("%+v", c)
	}
}
