package desktopimport

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/Higangssh/hyperbutler/internal/desktopimport/source"
	"github.com/Higangssh/hyperbutler/internal/engine"
)

// Regression tests for the second adversarial review of the import (docs/tools.md).

func content(f *fakeDocker, volume string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	v := f.volumes[volume]
	if v == nil {
		return "<none>"
	}
	var b strings.Builder
	for _, fl := range v.Files {
		b.WriteString(fl.Name + "=" + fl.Content + ";")
	}
	return b.String()
}

// H1, L6: a helper is never created from an image that declares VOLUME (or is for
// another platform), on either side, so neither engine gets anonymous volumes.
func TestHelperImageWithoutVolumes(t *testing.T) {
	p := newPair(t)
	p.src.addVolumeImage(100, "redis:alpine") // the smallest, with VOLUME /data
	arm := p.src.addImage("armonly:1")
	arm.Size, arm.Arch = 200, "arm64"
	bb := p.src.addImage("busybox:latest")
	bb.Size = 5000
	p.src.addVolume("data", files("x")...)
	p.dst.addVolumeImage(10, "postgres:16") // hyperbutler's only image has VOLUME too
	srcBefore := p.src.volumeNames()
	im := p.open(t)

	plan, res := run(t, im, Selection{Volumes: []string{"data"}})
	if plan.HelperImage != bb.ID {
		t.Fatalf("helper image %s, want busybox's", plan.HelperImage)
	}
	if !res.OK() || res.Items[0].Outcome != OutcomeImported {
		t.Fatalf("%+v", res)
	}
	if got := p.src.volumeNames(); !slices.Equal(got, srcBefore) {
		t.Errorf("Docker Desktop's volumes %v, before %v", got, srcBefore)
	}
	for _, c := range p.src.created {
		if c.Image != bb.ID || len(c.Anon) != 0 {
			t.Errorf("Docker Desktop helper from %s with anonymous volumes %v", c.Image, c.Anon)
		}
	}
	for _, c := range p.dst.created {
		if len(c.Anon) != 0 {
			t.Errorf("hyperbutler helper from %s made anonymous volumes %v", c.Image, c.Anon)
		}
	}
	if got := p.dst.volumeNames(); !slices.Equal(got, []string{"data"}) {
		t.Errorf("hyperbutler's volumes %v", got)
	}
	assertReadOnly(t, p.src)
}

// H1: with only images that declare VOLUME, volumes are skipped with a note and
// nothing is created in Docker Desktop.
func TestNoHelperImageSkipsVolumes(t *testing.T) {
	p := newPair(t)
	p.src.addVolumeImage(100, "redis:alpine")
	p.src.addVolume("data", files("x")...)
	srcBefore := p.src.volumeNames()
	im := p.open(t)
	plan, res := run(t, im, Selection{Volumes: []string{"data"}})
	if plan.HelperImage != "" || !strings.Contains(strings.Join(plan.Notes, ";"), "VOLUME") {
		t.Fatalf("helper %q notes %v", plan.HelperImage, plan.Notes)
	}
	if it := res.Items[0]; it.Outcome != OutcomeRefused || !strings.Contains(it.Reason, "anonymous volumes") {
		t.Fatalf("item %+v", it)
	}
	if len(p.src.created) != 0 {
		t.Errorf("containers created in Docker Desktop: %v", p.src.created)
	}
	if got := p.src.volumeNames(); !slices.Equal(got, srcBefore) || !res.Source.Unchanged {
		t.Errorf("Docker Desktop's volumes %v, source %+v", got, res.Source)
	}
	assertReadOnly(t, p.src)
}

// H2: the helper that filled a volume fails to go once; the import still succeeds
// (the removal is tried again), and the next import keeps the volume and its content.
func TestCopyingHelperRemovalFailsOnce(t *testing.T) {
	p := newPair(t)
	p.src.addImage("busybox:latest")
	p.src.addVolume("data", files("hello")...)
	p.dst.addImage("busybox:latest")
	fails := 0
	p.dst.failRemove = func(c *fContainer) bool {
		if c.Labels[copyingLabel] != "" && fails == 0 {
			fails++
			return true
		}
		return false
	}
	im := p.open(t)
	_, res := run(t, im, Selection{AllVolumes: true})
	if !res.OK() || fails != 1 {
		t.Fatalf("first: %+v (fails %d)", res, fails)
	}
	want := content(p.dst, "data")
	_, res = run(t, p.open(t), Selection{AllVolumes: true})
	if !res.OK() || res.Items[0].Outcome != OutcomeAlready || content(p.dst, "data") != want {
		t.Fatalf("second: %+v, content %q", res, content(p.dst, "data"))
	}
	c, err := loadComplete(engine.Layout{Home: p.o.Home}.ImportsCompletePath())
	if err != nil || !c.has("data", p.dst.volumes["data"].Labels[source.RunLabel]) {
		t.Errorf("not recorded as verified: %v %v", c, err)
	}
	if p.dst.helperCount() != 0 {
		t.Error("helpers left in hyperbutler")
	}
}

// H2: when the helper that filled a volume cannot be removed at all, the copy fails
// (its volume cannot be removed either while the helper holds it); the next import
// removes both and copies the volume again.
func TestCopyingHelperRemovalFails(t *testing.T) {
	p := newPair(t)
	p.src.addImage("busybox:latest")
	p.src.addVolume("data", files("hello")...)
	p.dst.addImage("busybox:latest")
	p.dst.failRemove = func(c *fContainer) bool { return c.Labels[copyingLabel] != "" }
	_, res := run(t, p.open(t), Selection{AllVolumes: true})
	if res.Items[0].Outcome != OutcomeFailed || !strings.Contains(res.Items[0].Reason, "could not remove the helper") {
		t.Fatalf("first: %+v", res.Items[0])
	}
	p.dst.mu.Lock()
	p.dst.failRemove = nil
	p.dst.mu.Unlock()
	plan, res := run(t, p.open(t), Selection{AllVolumes: true})
	if plan.Volumes[0].Status != StatusNew || !res.OK() || res.Items[0].Outcome != OutcomeImported || !strings.Contains(content(p.dst, "data"), "hello") {
		t.Fatalf("second: plan %+v result %+v", plan.Volumes[0], res)
	}
	if p.dst.helperCount() != 0 {
		t.Error("helpers left in hyperbutler")
	}
}

// H2: a helper that filled a volume left behind (its removal lost) does not make the
// next import remove the verified volume; the helper goes.
func TestStuckCopyingHelperKeepsVerifiedVolume(t *testing.T) {
	p := newPair(t)
	p.src.addImage("busybox:latest")
	p.src.addVolume("data", files("hello", "world")...)
	p.dst.addImage("busybox:latest")
	im := p.open(t)
	im.hooks.keepCopyingHelper = true
	_, res := run(t, im, Selection{AllVolumes: true})
	if !res.OK() || p.dst.helperCount() != 1 {
		t.Fatalf("first: %+v, %d helpers", res, p.dst.helperCount())
	}
	want := content(p.dst, "data")
	// The user writes to it in hyperbutler.
	p.dst.mu.Lock()
	p.dst.volumes["data"].Files = append(p.dst.volumes["data"].Files, fFile{Name: "z.txt", Mode: 0o644, Content: "mine"})
	p.dst.mu.Unlock()
	plan, res := run(t, p.open(t), Selection{AllVolumes: true})
	if plan.Volumes[0].Status != StatusConflict {
		t.Errorf("plan %+v", plan.Volumes[0])
	}
	if got := content(p.dst, "data"); got != want+"z.txt=mine;" {
		t.Fatalf("hyperbutler's volume is now %q", got)
	}
	if res.Items[0].Outcome != OutcomeRefused || p.dst.helperCount() != 0 || !strings.Contains(strings.Join(res.Notes, ";"), "verified") {
		t.Fatalf("second: %+v, %d helpers", res, p.dst.helperCount())
	}
}

// H2, L5: recovery removes a volume only for a left-over helper of a gone run that was
// filling it, made by that run and not verified; and a helper only after the checks.
func TestRecoveryRules(t *testing.T) {
	p := newPair(t)
	p.src.addImage("busybox:latest")
	p.dst.addImage("busybox:latest")
	dead, other := source.NewRunID(), source.NewRunID()
	labels := func(run string) map[string]string {
		return map[string]string{importLabel: "docker-desktop", source.RunLabel: run}
	}
	// Made by another run than the helper's.
	p.dst.addVolume("other-run", files("a")...).Labels = labels(other)
	p.dst.addHelper("other-run", dead, true)
	// Not made by an import at all.
	p.dst.addVolume("users", files("b")...).Labels = map[string]string{source.RunLabel: dead}
	p.dst.addHelper("users", dead, true)
	// A helper that was started is not a helper; nor one with another entrypoint.
	p.dst.addVolume("started", files("c")...).Labels = labels(dead)
	p.dst.addHelper("started", dead, true).Started = true
	p.dst.addVolume("entry", files("d")...).Labels = labels(dead)
	p.dst.addHelper("entry", dead, true).Entrypoint = []string{"/bin/sh"}
	// A helper of a run that is still going on.
	alive := source.NewRunID()
	rel, err := holdRun(alive)
	if err != nil {
		t.Fatal(err)
	}
	defer rel()
	p.dst.addVolume("alive", files("e")...).Labels = labels(alive)
	aliveHelper := p.dst.addHelper("alive", alive, true)
	// The only one to go: a half copy.
	p.dst.addVolume("half", files("f")...).Labels = labels(dead)
	p.dst.addHelper("half", dead, true)

	_, res, err := runErr(p.open(t), Selection{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK() {
		t.Fatalf("%+v", res)
	}
	got := p.dst.volumeNames()
	if want := []string{"alive", "entry", "other-run", "started", "users"}; !slices.Equal(got, want) {
		t.Errorf("volumes %v, want %v", got, want)
	}
	p.dst.mu.Lock()
	defer p.dst.mu.Unlock()
	var left []string
	for _, c := range p.dst.containers {
		left = append(left, c.Volume)
	}
	slices.Sort(left)
	if !slices.Equal(left, []string{"alive", "entry", "started"}) || p.dst.containers[aliveHelper.ID] == nil {
		t.Errorf("helpers left %v", left)
	}
}

func runErr(im *Importer, sel Selection) (*Plan, *Result, error) {
	plan, err := im.Plan(context.Background(), PlanOptions{CompareVolumes: true})
	if err != nil {
		return nil, nil, err
	}
	res, err := im.Run(context.Background(), plan, sel, nil)
	return plan, res, err
}

// M1: two imports at once from two installations on the same engines leave each
// other's helpers and volumes alone; a second one in the same installation refuses.
func TestParallelImports(t *testing.T) {
	p := newPair(t)
	p.src.addImage("busybox:latest")
	p.src.addVolume("a", files("aaa")...)
	p.src.addVolume("b", files("bbb")...)
	p.dst.addImage("busybox:latest")
	release := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	p.dst.blockPut, p.dst.blockPutVol = release, "a"
	p.dst.onPut = func(v string) {
		if v == "a" {
			once.Do(func() { close(started) })
		}
	}
	imA := p.open(t)
	planA, err := imA.Plan(context.Background(), PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	type out struct {
		res *Result
		err error
	}
	done := make(chan out, 1)
	go func() {
		res, err := imA.Run(context.Background(), planA, Selection{Volumes: []string{"a"}}, nil)
		done <- out{res, err}
	}()
	<-started

	// The same installation: refused at once.
	same := p.open(t)
	if _, _, err := runErr(same, Selection{Volumes: []string{"b"}}); !errors.Is(err, ErrImportRunning) {
		t.Fatalf("same installation: %v", err)
	}
	// Another installation (a GUI's and a CLI's of two installs, say).
	o := p.o
	o.Home = t.TempDir()
	imB, err := Open(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer imB.Close()
	_, resB, err := runErr(imB, Selection{Volumes: []string{"b"}})
	if err != nil || !resB.OK() {
		t.Fatalf("B: %v %+v", err, resB)
	}
	if p.src.helperCount() != 1 || p.dst.helperCount() != 1 {
		t.Errorf("A's helpers were removed: %d in Docker Desktop, %d in hyperbutler", p.src.helperCount(), p.dst.helperCount())
	}
	close(release)
	a := <-done
	if a.err != nil || !a.res.OK() {
		t.Fatalf("A: %v %+v", a.err, a.res)
	}
	if !strings.Contains(content(p.dst, "a"), "aaa") || !strings.Contains(content(p.dst, "b"), "bbb") {
		t.Errorf("contents %q %q", content(p.dst, "a"), content(p.dst, "b"))
	}
	if p.src.helperCount() != 0 || p.dst.helperCount() != 0 {
		t.Error("helpers left")
	}
	assertReadOnly(t, p.src)
}

// M2: a volume that cannot be hashed before the import fails alone; images and the
// other volumes are imported.
func TestHashFailureIsPerVolume(t *testing.T) {
	p := newPair(t)
	p.src.addImage("busybox:latest")
	p.src.addImage("alpine:3.22")
	p.src.addVolume("bad", files("x")...)
	p.src.addVolume("good", files("y")...)
	p.dst.addImage("busybox:latest")
	p.src.failArchive = "bad"
	im := p.open(t)
	plan, err := im.Plan(context.Background(), PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := im.Run(context.Background(), plan, Selection{AllImages: true, AllVolumes: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := outcomes(res)
	if got["image:alpine:3.22"] != OutcomeImported || got["volume:good"] != OutcomeImported || got["volume:bad"] != OutcomeFailed {
		t.Fatalf("outcomes %v", got)
	}
	if !res.Source.Unchanged {
		t.Errorf("source %+v", res.Source)
	}
	if _, ok := p.dst.volumes["bad"]; ok {
		t.Error("the volume that failed is in hyperbutler")
	}
	assertReadOnly(t, p.src)
}

// M3: a volume with the same content but other labels is not "already imported".
func TestVolumeLabelsCompared(t *testing.T) {
	t.Run("refused, then replaced", func(t *testing.T) {
		p := newPair(t)
		p.src.addImage("busybox:latest")
		p.src.addVolume("data", files("same")...)
		p.dst.addImage("busybox:latest")
		p.dst.addVolume("data", files("same")...).Labels = map[string]string{"com.docker.compose.volume": "data", "com.docker.compose.project": "other"}
		im := p.open(t)
		plan, res := run(t, im, Selection{Volumes: []string{"data"}})
		if plan.Volumes[0].Status != StatusDifferentLabels || res.Items[0].Outcome != OutcomeRefused || !strings.Contains(res.Items[0].Reason, "other labels") {
			t.Fatalf("plan %+v item %+v", plan.Volumes[0], res.Items[0])
		}
		_, res = run(t, im, Selection{Volumes: []string{"data"}, OverwriteVolumes: true})
		if res.Items[0].Outcome != OutcomeImported || !maps.Equal(withoutOwnLabels(p.dst.volumes["data"].Labels), p.src.volumes["data"].Labels) {
			t.Fatalf("overwrite: %+v labels %v", res.Items[0], p.dst.volumes["data"].Labels)
		}
		if _, res = run(t, im, Selection{Volumes: []string{"data"}}); res.Items[0].Outcome != OutcomeAlready {
			t.Fatalf("again: %+v", res.Items[0])
		}
	})
	t.Run("empty and unlabeled: created again", func(t *testing.T) {
		p := newPair(t)
		p.src.addImage("busybox:latest")
		p.src.addVolume("data")
		p.dst.addImage("busybox:latest")
		p.dst.addVolume("data").Labels = nil
		plan, res := run(t, p.open(t), Selection{Volumes: []string{"data"}})
		if plan.Volumes[0].Status != StatusDifferentLabels || !strings.Contains(plan.Volumes[0].Reason, "empty") {
			t.Fatalf("plan %+v", plan.Volumes[0])
		}
		if res.Items[0].Outcome != OutcomeImported || p.dst.volumes["data"].Labels["com.docker.compose.volume"] != "data" {
			t.Fatalf("item %+v labels %v", res.Items[0], p.dst.volumes["data"].Labels)
		}
	})
	for _, c := range []struct {
		name string
		set  func(p *pair)
		want string
	}{
		{"empty, another project's", func(p *pair) {
			p.dst.volumes["data"].Labels = map[string]string{"com.docker.compose.project": "other"}
		}, "other labels"},
		{"empty, in use in hyperbutler", func(p *pair) {
			p.dst.volumes["data"].Labels = nil
			p.dst.addContainer("app", "exited", "data")
		}, "app"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := newPair(t)
			p.src.addImage("busybox:latest")
			p.src.addVolume("data")
			p.dst.addImage("busybox:latest")
			p.dst.addVolume("data")
			c.set(p)
			before := p.dst.volumes["data"]
			_, res := run(t, p.open(t), Selection{Volumes: []string{"data"}})
			if res.Items[0].Outcome != OutcomeRefused || !strings.Contains(res.Items[0].Reason, c.want) || p.dst.volumes["data"] != before {
				t.Fatalf("item %+v", res.Items[0])
			}
		})
	}
}

// M4: with different image stores, an image with the same layers, time and platform
// but another config is another image.
func TestMixedStoreComparesConfig(t *testing.T) {
	for _, c := range []struct {
		name string
		cmd  []string
		want Status
	}{{"same config", []string{"sh"}, StatusAlready}, {"other command", []string{"evil"}, StatusConflict}} {
		t.Run(c.name, func(t *testing.T) {
			p := newPair(t)
			p.dst.classic = true
			bb := p.src.addImage("busybox:latest")
			bb.Config.Cmd = []string{"sh"}
			d := p.dst.addImage("busybox:latest")
			d.Layers, d.Created, d.Config.Cmd = bb.Layers, bb.Created, c.cmd
			plan, err := p.open(t).Plan(context.Background(), PlanOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if plan.Images[0].Status != c.want {
				t.Fatalf("status %s, want %s", plan.Images[0].Status, c.want)
			}
		})
	}
}

// M5: --overwrite-volumes copies into a temporary volume and verifies it before
// hyperbutler's volume is deleted; no failure loses both.
func TestOverwriteKeepsOldUntilVerified(t *testing.T) {
	setup := func(t *testing.T) *pair {
		p := newPair(t)
		p.src.addImage("busybox:latest")
		p.src.addVolume("data", files("new")...)
		p.dst.addImage("busybox:latest")
		p.dst.addVolume("data", files("old")...)
		return p
	}
	sel := Selection{Volumes: []string{"data"}, OverwriteVolumes: true}
	t.Run("replaced", func(t *testing.T) {
		p := setup(t)
		_, res := run(t, p.open(t), sel)
		if !res.OK() || !strings.Contains(content(p.dst, "data"), "a.txt=new") {
			t.Fatalf("%+v %q", res, content(p.dst, "data"))
		}
		if got := p.dst.volumeNames(); !slices.Equal(got, []string{"data"}) || p.dst.helperCount() != 0 {
			t.Errorf("volumes %v, %d helpers", got, p.dst.helperCount())
		}
		if p.dst.puts != 2 {
			t.Errorf("%d archive writes, want 2 (temporary, then the volume)", p.dst.puts)
		}
	})
	t.Run("the temporary copy fails", func(t *testing.T) {
		p := setup(t)
		p.dst.corruptPut, p.dst.corruptNth = true, 1
		_, res := run(t, p.open(t), sel)
		if res.Items[0].Outcome != OutcomeFailed || !strings.Contains(res.Items[0].Reason, "unchanged") {
			t.Fatalf("%+v", res.Items[0])
		}
		if !strings.Contains(content(p.dst, "data"), "a.txt=old") || len(p.dst.volumeNames()) != 1 {
			t.Errorf("hyperbutler: %v %q", p.dst.volumeNames(), content(p.dst, "data"))
		}
	})
	t.Run("canceled during the temporary copy", func(t *testing.T) {
		p := setup(t)
		im := p.open(t)
		plan, err := im.Plan(context.Background(), PlanOptions{})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		block := make(chan struct{})
		t.Cleanup(func() { close(block) }) // before the fake's server closes
		p.dst.blockPut = block
		p.dst.onPut = func(string) { cancel() }
		res, err := im.Run(ctx, plan, sel, nil)
		if err != nil || res.Items[0].Outcome != OutcomeCanceled {
			t.Fatalf("%v %+v", err, res)
		}
		if !strings.Contains(content(p.dst, "data"), "a.txt=old") || len(p.dst.volumeNames()) != 1 {
			t.Errorf("hyperbutler: %v %q", p.dst.volumeNames(), content(p.dst, "data"))
		}
	})
	t.Run("in use in hyperbutler", func(t *testing.T) {
		p := setup(t)
		p.dst.addContainer("app", "exited", "data")
		_, res := run(t, p.open(t), sel)
		if res.Items[0].Outcome != OutcomeRefused || p.dst.puts != 0 || !strings.Contains(content(p.dst, "data"), "a.txt=old") {
			t.Fatalf("%+v, %d writes", res.Items[0], p.dst.puts)
		}
	})
	t.Run("the copy from the temporary volume fails", func(t *testing.T) {
		p := setup(t)
		p.dst.corruptPut, p.dst.corruptNth = true, 2
		_, res := run(t, p.open(t), sel)
		it := res.Items[0]
		if it.Outcome != OutcomeFailed || !strings.Contains(it.Reason, "verified, is in hyperbutler's volume hyperbutler-import-") {
			t.Fatalf("%+v", it)
		}
		names := p.dst.volumeNames()
		if len(names) != 1 || !strings.HasPrefix(names[0], "hyperbutler-import-") || !strings.Contains(content(p.dst, names[0]), "a.txt=new") {
			t.Fatalf("hyperbutler: %v", names)
		}
		tmp := names[0]
		// The next import keeps the verified copy while data is not there again ...
		p.dst.corruptPut = false
		plan, res := run(t, p.open(t), Selection{Volumes: []string{"data"}})
		if !res.OK() || content(p.dst, tmp) == "<none>" || !strings.Contains(strings.Join(plan.Recovery, ";"), tmp) {
			t.Fatalf("second: %+v, notes %v", res, plan.Recovery)
		}
		// ... and the one after removes it.
		if _, res = run(t, p.open(t), Selection{Volumes: []string{"data"}}); !res.OK() || content(p.dst, tmp) != "<none>" {
			t.Fatalf("third: %+v, %v", res, p.dst.volumeNames())
		}
	})
}

// L2: a plan (a dry run) removes nothing in either engine, not even what a killed
// import left; it says what Run will remove.
func TestPlanRemovesNothing(t *testing.T) {
	p := newPair(t)
	p.src.addImage("busybox:latest")
	p.src.addVolume("data", files("full")...)
	dead := source.NewRunID()
	p.src.addHelper("data", dead, false)
	p.dst.addImage("busybox:latest")
	p.dst.addVolume("data", files("fu")...).Labels = map[string]string{importLabel: "docker-desktop", source.RunLabel: dead}
	p.dst.addHelper("data", dead, true)
	im := p.open(t)
	plan, err := im.Plan(context.Background(), PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []*fakeDocker{p.src, p.dst} {
		for _, r := range f.reqs() {
			if !strings.HasPrefix(r, "GET ") {
				t.Errorf("the plan sent %s", r)
			}
		}
	}
	if p.src.helperCount() != 1 || p.dst.helperCount() != 1 || p.dst.volumes["data"] == nil {
		t.Error("the plan removed something")
	}
	if plan.Volumes[0].Status != StatusNew || !strings.Contains(strings.Join(plan.Recovery, ";"), "incomplete copy of volume data") {
		t.Errorf("plan %+v notes %v", plan.Volumes[0], plan.Recovery)
	}
	res, err := im.Run(context.Background(), plan, Selection{AllVolumes: true}, nil)
	if err != nil || !res.OK() || !strings.Contains(content(p.dst, "data"), "full") || p.src.helperCount() != 0 {
		t.Fatalf("run: %v %+v", err, res)
	}
}

// L3: a dry run's selection resolves names as Run does.
func TestPlanSelect(t *testing.T) {
	p := &Plan{Images: []ImagePlan{
		{ID: "sha256:aaaaaaaaaaaaaaaa1111", Tags: []string{"busybox:latest"}},
		{ID: "sha256:bbbbbbbbbbbbbbbb2222", Tags: []string{"ghcr.io/o/app:1"}},
		{ID: "sha256:cccc", Status: StatusUnsupported},
	}, Volumes: []VolumePlan{{Name: "pg"}, {Name: "cache"}}}
	n, err := p.Select(Selection{Images: []string{"docker.io/library/busybox", "bbbbbbbbbbbbbbbb"}, Volumes: []string{"pg"}})
	if err != nil || len(n.Images) != 2 || len(n.Volumes) != 1 || n.Volumes[0].Name != "pg" {
		t.Fatalf("%+v %v", n, err)
	}
	for _, sel := range []Selection{{Images: []string{"nginx"}}, {Volumes: []string{"nope"}}, {Images: []string{"dddddddddddd"}}} {
		if _, err := p.Select(sel); err == nil {
			t.Errorf("%+v accepted", sel)
		}
	}
	if n, _ := p.Select(Selection{AllImages: true}); len(n.Images) != 3 || len(n.Volumes) != 0 {
		t.Errorf("all images: %+v", n)
	}
}

// L4: hyperbutler's volumes are read through read-only helpers; only the helper that
// fills a new volume is writable.
func TestTargetReadsAreReadOnly(t *testing.T) {
	p := newPair(t)
	p.src.addImage("busybox:latest")
	p.src.addVolume("data", files("x")...)
	p.src.addVolume("same", files("y")...)
	p.dst.addImage("busybox:latest")
	p.dst.addVolume("same", files("y")...).Labels = p.src.volumes["same"].Labels
	_, res := run(t, p.open(t), Selection{AllVolumes: true})
	if !res.OK() {
		t.Fatalf("%+v", res)
	}
	writable := 0
	for _, c := range p.dst.created {
		copying := c.Labels[copyingLabel] != ""
		if c.ReadOnly == copying {
			t.Errorf("helper on %s: read-only %v, filling %v", c.Volume, c.ReadOnly, copying)
		}
		if !c.ReadOnly {
			writable++
		}
	}
	if writable != 1 {
		t.Errorf("%d writable helpers", writable)
	}
}

// L8: a volume in use when the plan was made but no longer when Run starts is copied.
func TestVolumeNoLongerLive(t *testing.T) {
	p := newPair(t)
	p.src.addImage("busybox:latest")
	p.src.addVolume("data", files("x")...)
	c := p.src.addContainer("db", "running", "data")
	p.dst.addImage("busybox:latest")
	im := p.open(t)
	plan, err := im.Plan(context.Background(), PlanOptions{})
	if err != nil || len(plan.Volumes[0].UsedBy) != 1 {
		t.Fatalf("%v %+v", err, plan)
	}
	p.src.mu.Lock()
	c.State = "exited"
	p.src.mu.Unlock()
	res, err := im.Run(context.Background(), plan, Selection{AllVolumes: true}, nil)
	if err != nil || !res.OK() || res.Items[0].Outcome != OutcomeImported || res.Source.HashedVolumes != 1 {
		t.Fatalf("%v %+v", err, res)
	}
}
