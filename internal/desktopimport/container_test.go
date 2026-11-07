package desktopimport

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/Higangssh/hyperbutler/internal/desktopimport/source"
	"github.com/Higangssh/hyperbutler/internal/share"
)

// addRun adds a container made with docker run to the fake, with an inspect answer as
// dockerd gives it; hc and edit change it from a plain one.
func (f *fakeDocker) addRun(name string, img *fImage, hc map[string]any, edit ...func(in map[string]any)) *fContainer {
	c := f.addContainer(name, "exited", "")
	if img != nil {
		c.Image = img.ID
	}
	host := map[string]any{"NetworkMode": "bridge", "RestartPolicy": map[string]any{"Name": "no"}}
	for k, v := range hc {
		host[k] = v
	}
	in := map[string]any{
		"Id": c.ID, "Name": "/" + name, "Image": c.Image, "Platform": "linux", "SizeRw": 0,
		"State":           map[string]any{"Status": "exited"},
		"Config":          map[string]any{"Hostname": c.ID[:12], "Image": imageName(img), "Env": []string{"SECRET=hunter2"}, "Cmd": []string{"serve"}, "Labels": map[string]string{}},
		"HostConfig":      host,
		"NetworkSettings": map[string]any{"Networks": map[string]any{"bridge": map[string]any{}}},
	}
	for _, e := range edit {
		e(in)
	}
	c.Inspect = in
	if cfg, ok := in["Config"].(map[string]any); ok {
		if l, ok := cfg["Labels"].(map[string]string); ok {
			c.Labels = l
		}
	}
	return c
}

func imageName(img *fImage) string {
	if img == nil || len(img.Tags) == 0 {
		return "sha256:gone"
	}
	return img.Tags[0]
}

func planAll(t *testing.T, im *Importer) *Plan {
	t.Helper()
	plan, err := im.Plan(context.Background(), PlanOptions{CompareVolumes: true, Containers: true})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func planned(t *testing.T, p *Plan, name string) ContainerPlan {
	t.Helper()
	i := slices.IndexFunc(p.Containers, func(c ContainerPlan) bool { return c.Name == name })
	if i < 0 {
		t.Fatalf("%s is not in the plan: %+v", name, p.Containers)
	}
	return p.Containers[i]
}

func runPlan(t *testing.T, im *Importer, plan *Plan, sel Selection) *Result {
	t.Helper()
	res, err := im.Run(context.Background(), plan, sel, nil)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// assertNoContainerStarted fails if hyperbutler's fake got a start of any container.
func assertNoContainerStarted(t *testing.T, dst *fakeDocker) {
	t.Helper()
	for _, r := range dst.reqs() {
		if strings.HasPrefix(r, "POST /containers/") && !strings.HasSuffix(r, "/create") {
			t.Errorf("hyperbutler got %s", r)
		}
	}
}

// The whole path: everything the requirement names arrives in the create, the image and
// volume come along, the network is made first, nothing starts, Docker Desktop is only
// read and its secrets are not shown.
func TestContainerRecreated(t *testing.T) {
	p := newPair(t)
	img := p.src.addImage("myapp:1")
	p.src.addImage("busybox:latest")
	p.src.addVolume("pgdata", files("x")...)
	p.src.networks["backend"] = map[string]any{"Name": "backend", "Driver": "bridge", "Labels": map[string]string{"team": "a"},
		"Options": map[string]string{"com.docker.network.bridge.name": "eth0", "com.docker.network.driver.mtu": "1400"},
		"IPAM":    map[string]any{"Config": []map[string]string{{"Subnet": "172.30.0.0/16"}}}}
	c := p.src.addRun("web", img, map[string]any{
		"Binds":         []string{"pgdata:/data:ro", `C:\src:/src`},
		"NetworkMode":   "backend",
		"PortBindings":  map[string]any{"80/tcp": []map[string]string{{"HostIp": "", "HostPort": "8080"}}},
		"RestartPolicy": map[string]any{"Name": "unless-stopped"},
		"Memory":        1 << 30, "NanoCpus": 2000000000, "PidsLimit": 100, "CapAdd": []string{"NET_ADMIN"},
	}, func(in map[string]any) {
		cfg := in["Config"].(map[string]any)
		cfg["Entrypoint"] = []string{"/entry"}
		cfg["Labels"] = map[string]string{"app": "web", "dev.hyperbutler.import": "helper-no", "dev.hyperbutler.import.container": "forged"}
		in["NetworkSettings"] = map[string]any{"Networks": map[string]any{"backend": map[string]any{"Aliases": []string{in["Id"].(string)[:12], "web"}}}}
	})
	im := p.open(t)
	plan := planAll(t, im)

	cp := planned(t, plan, "web")
	if cp.Status != StatusNew || cp.Image != "myapp:1" || !slices.Equal(cp.Volumes, []string{"pgdata"}) || !slices.Equal(cp.Networks, []string{"backend"}) ||
		!slices.Equal(cp.CapAdd, []string{"NET_ADMIN"}) || !slices.Equal(cp.Ports, []string{"8080->80/tcp"}) || !slices.Equal(cp.Binds, []string{`C:\src -> /src`}) {
		t.Fatalf("plan %+v", cp)
	}
	shown, _ := json.Marshal(plan.Containers)
	if strings.Contains(string(shown), "hunter2") {
		t.Errorf("the plan shows the environment: %s", shown)
	}

	var notes []string
	var firstCopy int
	n := 0
	res, err := im.Run(context.Background(), plan, Selection{Containers: []string{"web"}}, func(e Event) {
		n++
		if e.Kind == EventStart && firstCopy == 0 {
			firstCopy = n
		}
		if e.Kind == EventNote && firstCopy == 0 {
			notes = append(notes, e.Message)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK() {
		t.Fatalf("not ok: %+v", res)
	}
	got := outcomes(res)
	if got["container:web"] != OutcomeImported || got["image:myapp:1"] != OutcomeImported || got["volume:pgdata"] != OutcomeImported {
		t.Fatalf("outcomes %v", got)
	}
	if _, ok := got["image:busybox:latest"]; ok {
		t.Errorf("an image no one asked for came along: %v", got)
	}
	if !strings.Contains(strings.Join(notes, "|"), "NET_ADMIN") {
		t.Errorf("the capabilities were not shown before the copy: %q", notes)
	}

	// The network came before the container, with only the options that mean something
	// here and no subnet, which inspect shows even when the engine picked it.
	reqs := p.dst.reqs()
	nw, cr := slices.Index(reqs, "POST /networks/create"), slices.Index(reqs, "POST /containers/create")
	if nw < 0 || cr < 0 {
		t.Fatalf("requests %v", reqs)
	}
	n0 := p.dst.networks["backend"]
	if n0["Driver"] != "bridge" || n0["IPAM"] != nil || n0["Options"].(map[string]any)["com.docker.network.bridge.name"] != nil ||
		n0["Options"].(map[string]any)["com.docker.network.driver.mtu"] != "1400" || n0["Labels"].(map[string]any)["team"] != "a" {
		t.Errorf("network %v", n0)
	}

	if len(p.dst.createdBody) != 1 {
		t.Fatalf("creates %v", p.dst.createdBody)
	}
	b := p.dst.createdBody[0]
	hc := b["HostConfig"].(map[string]any)
	if b["Image"] != "myapp:1" || b["Hostname"] != nil {
		t.Errorf("body %v", b)
	}
	if !slices.Equal(strs(b["Entrypoint"]), []string{"/entry"}) || !slices.Equal(strs(b["Cmd"]), []string{"serve"}) || !slices.Equal(strs(b["Env"]), []string{"SECRET=hunter2"}) {
		t.Errorf("command or environment: %v", b)
	}
	labels := b["Labels"].(map[string]any)
	if labels["app"] != "web" || labels[importLabel] != "docker-desktop" || labels[containerFromLabel] != c.ID || labels["dev.hyperbutler.import"] != nil {
		t.Errorf("labels %v", labels)
	}
	if !slices.Equal(strs(hc["Binds"]), []string{"pgdata:/data:ro", `C:\src:/src`}) || hc["NetworkMode"] != "backend" ||
		hc["Memory"] != float64(1<<30) || hc["NanoCpus"] != float64(2000000000) || hc["PidsLimit"] != float64(100) ||
		hc["RestartPolicy"].(map[string]any)["Name"] != "unless-stopped" || !slices.Equal(strs(hc["CapAdd"]), []string{"NET_ADMIN"}) {
		t.Errorf("host config %v", hc)
	}
	if pb := hc["PortBindings"].(map[string]any)["80/tcp"].([]any)[0].(map[string]any); pb["HostPort"] != "8080" {
		t.Errorf("ports %v", pb)
	}
	ep := b["NetworkingConfig"].(map[string]any)["EndpointsConfig"].(map[string]any)["backend"].(map[string]any)
	if !slices.Equal(strs(ep["Aliases"]), []string{"web"}) {
		t.Errorf("aliases %v (the old short id must go)", ep)
	}

	assertNoContainerStarted(t, p.dst)
	assertReadOnly(t, p.src)
	if !res.Source.Unchanged {
		t.Errorf("source check %+v", res.Source)
	}

	// A second run finds it there and creates nothing.
	plan = planAll(t, im)
	if s := planned(t, plan, "web").Status; s != StatusAlready {
		t.Errorf("second plan: %s", s)
	}
	res = runPlan(t, im, plan, Selection{AllContainers: true})
	if !res.OK() || outcomes(res)["container:web"] != OutcomeAlready || len(p.dst.createdBody) != 1 {
		t.Errorf("second run: %+v, %d creates", res, len(p.dst.createdBody))
	}
}

func strs(v any) []string {
	l, _ := v.([]any)
	out := make([]string, 0, len(l))
	for _, x := range l {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}

// A plan without PlanOptions.Containers, the window's, reads no container of Docker
// Desktop and has none; a dry run with them creates nothing.
func TestPlanCreatesNoContainer(t *testing.T) {
	p := newPair(t)
	img := p.src.addImage("myapp:1")
	p.src.addRun("web", img, nil)
	im := p.open(t)
	plan, err := im.Plan(context.Background(), PlanOptions{CompareVolumes: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Containers) != 0 {
		t.Errorf("containers in a plan that did not ask: %+v", plan.Containers)
	}
	for _, r := range p.src.reqs() {
		if strings.HasPrefix(r, "GET /containers/") && strings.HasSuffix(r, "/json") && r != "GET /containers/json" {
			t.Errorf("read %s", r)
		}
	}
	plan = planAll(t, im)
	sel, err := plan.Select(Selection{Containers: []string{"web"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(sel.Containers) != 1 || len(sel.Images) != 1 || sel.Images[0].ID != img.ID {
		t.Errorf("selected %+v", sel)
	}
	if _, err := plan.Select(Selection{Containers: []string{"nope"}}); err == nil {
		t.Error("an unknown container was accepted")
	}
	if _, err := plan.Select(Selection{ConfirmContainers: []string{"../x"}}); err == nil {
		t.Error("a bad name was accepted")
	}
	for _, r := range p.dst.reqs() {
		if !strings.HasPrefix(r, "GET ") {
			t.Errorf("a plan wrote to hyperbutler: %s", r)
		}
	}
	assertReadOnly(t, p.src)
}

// Each thing the requirement names is listed and skipped with the reason, refused when
// named without a confirmation, and created when confirmed.
func TestContainerNeedsConfirm(t *testing.T) {
	devices := []map[string]string{{"PathOnHost": "/dev/kvm", "PathInContainer": "/dev/kvm", "CgroupPermissions": "rwm"}}
	for _, tc := range []struct {
		name, want string
		hc         map[string]any
		check      func(t *testing.T, hc map[string]any)
	}{
		{name: "priv", want: "--privileged", hc: map[string]any{"Privileged": true},
			check: func(t *testing.T, hc map[string]any) {
				if hc["Privileged"] != true {
					t.Errorf("not carried: %v", hc)
				}
			}},
		{name: "hostnet", want: "host networking", hc: map[string]any{"NetworkMode": "host"},
			check: func(t *testing.T, hc map[string]any) {
				if hc["NetworkMode"] != "host" {
					t.Errorf("not carried: %v", hc)
				}
			}},
		{name: "hostpid", want: "PID namespace", hc: map[string]any{"PidMode": "host"}},
		{name: "hostipc", want: "IPC namespace", hc: map[string]any{"IpcMode": "host"}},
		{name: "dev", want: "/dev/kvm", hc: map[string]any{"Devices": devices}},
		{name: "allcaps", want: "SYS_ADMIN", hc: map[string]any{"CapAdd": []string{"cap_sys_admin"}}},
		{name: "seccomp", want: "security options", hc: map[string]any{"SecurityOpt": []string{"seccomp=unconfined"}}},
		{name: "unc", want: "network path", hc: map[string]any{"Binds": []string{`\\server\share\x:/data`, "/run/desktop/mnt/host/c/keep:/keep"}},
			check: func(t *testing.T, hc map[string]any) {
				// hyperbutler's engine refuses that source: the mount is left out, the other stays.
				if !slices.Equal(strs(hc["Binds"]), []string{`C:\keep:/keep`}) {
					t.Errorf("binds %v", hc["Binds"])
				}
			}},
		{name: "wsl", want: "WSL", hc: map[string]any{"Binds": []string{"/run/desktop/mnt/host/wsl/Ubuntu/home:/data"}}},
		{name: "wslbind", want: "WSL", hc: map[string]any{"Mounts": []map[string]any{{"Type": "bind", "Source": "/mnt/wsl/docker-desktop-bind-mounts/Ubuntu/abc", "Target": "/data"}}}},
		{name: "vmpath", want: "inside Docker Desktop's VM", hc: map[string]any{"Binds": []string{"/var/run/docker.sock:/var/run/docker.sock"}},
			check: func(t *testing.T, hc map[string]any) {
				if !slices.Equal(strs(hc["Binds"]), []string{"/var/run/docker.sock:/var/run/docker.sock"}) {
					t.Errorf("binds %v", hc["Binds"])
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPair(t)
			p.src.addRun(tc.name, p.src.addImage("myapp:1"), tc.hc)
			im := p.open(t)
			plan := planAll(t, im)
			cp := planned(t, plan, tc.name)
			if cp.Status != StatusConfirm || !strings.Contains(cp.Reason, tc.want) || !strings.Contains(cp.Reason, "--confirm-container "+tc.name) {
				t.Fatalf("plan %+v", cp)
			}
			// --all-containers skips it, and it brings nothing along.
			res := runPlan(t, im, plan, Selection{AllContainers: true})
			if !res.OK() || len(res.Items) != 0 || len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0].Reason, tc.want) {
				t.Fatalf("all: %+v", res)
			}
			// Named but not confirmed: refused, and the command says so.
			res = runPlan(t, im, plan, Selection{Containers: []string{tc.name}})
			if res.OK() || outcomes(res)["container:"+tc.name] != OutcomeRefused || len(p.dst.createdBody) != 0 {
				t.Fatalf("named: %+v", res)
			}
			res = runPlan(t, im, plan, Selection{ConfirmContainers: []string{tc.name}})
			if !res.OK() || outcomes(res)["container:"+tc.name] != OutcomeImported || len(p.dst.createdBody) != 1 {
				t.Fatalf("confirmed: %+v", res)
			}
			if tc.check != nil {
				tc.check(t, p.dst.createdBody[0]["HostConfig"].(map[string]any))
			}
			assertNoContainerStarted(t, p.dst)
			assertReadOnly(t, p.src)
		})
	}
}

// What names another container of Docker Desktop's cannot be recreated, confirmed or not.
func TestContainerUnsupported(t *testing.T) {
	for name, hc := range map[string]map[string]any{
		"netof":   {"NetworkMode": "container:abc"},
		"pidof":   {"PidMode": "container:abc"},
		"links":   {"Links": []string{"/db:/web/db"}},
		"from":    {"VolumesFrom": []string{"db"}},
		"badport": {"PortBindings": map[string]any{"80/tcp;rm": []map[string]string{{"HostPort": "1"}}}},
		"badbind": {"Binds": []string{"nope"}},
		"badmnt":  {"Mounts": []map[string]any{{"Type": "npipe", "Source": "x", "Target": "/x"}}},
	} {
		t.Run(name, func(t *testing.T) {
			p := newPair(t)
			p.src.addRun(name, p.src.addImage("myapp:1"), hc)
			im := p.open(t)
			plan := planAll(t, im)
			if cp := planned(t, plan, name); cp.Status != StatusUnsupported || cp.Reason == "" {
				t.Fatalf("plan %+v", cp)
			}
			res := runPlan(t, im, plan, Selection{ConfirmContainers: []string{name}})
			if outcomes(res)["container:"+name] != OutcomeRefused || len(p.dst.createdBody) != 0 {
				t.Errorf("%+v", res)
			}
		})
	}
}

// A Compose project's container is named with its project and folder, never recreated.
func TestContainerOfComposeProject(t *testing.T) {
	p := newPair(t)
	img := p.src.addImage("myapp:1")
	withLabels := func(l map[string]string) func(map[string]any) {
		return func(in map[string]any) { in["Config"].(map[string]any)["Labels"] = l }
	}
	p.src.addRun("shop-web-1", img, nil, withLabels(map[string]string{composeProjectLabel: "shop", composeWorkingDirLabel: `C:\work\shop`}))
	p.src.addRun("other-1", img, nil, withLabels(map[string]string{composeProjectLabel: "other"}))
	im := p.open(t)
	plan := planAll(t, im)
	cp := planned(t, plan, "shop-web-1")
	if cp.Status != StatusCompose || cp.Project != "shop" || cp.ProjectDir != `C:\work\shop` ||
		!strings.Contains(cp.Reason, "docker compose up in C:\\work\\shop") || !strings.Contains(cp.Reason, "shop") {
		t.Fatalf("plan %+v", cp)
	}
	if cp := planned(t, plan, "other-1"); !strings.Contains(cp.Reason, "docker compose up in its folder") {
		t.Errorf("without a folder: %+v", cp)
	}
	res := runPlan(t, im, plan, Selection{AllContainers: true})
	if len(res.Skipped) != 2 || len(p.dst.createdBody) != 0 || !res.OK() {
		t.Errorf("all: %+v", res)
	}
	res = runPlan(t, im, plan, Selection{ConfirmContainers: []string{"shop-web-1"}})
	if outcomes(res)["container:shop-web-1"] != OutcomeRefused || len(p.dst.createdBody) != 0 {
		t.Errorf("confirming a Compose container must not recreate it: %+v", res)
	}
}

// The writable layer is not copied: said for each container with one, not for the others.
func TestContainerWritableLayer(t *testing.T) {
	p := newPair(t)
	img := p.src.addImage("myapp:1")
	p.src.addRun("clean", img, nil)
	p.src.addRun("wrote", img, nil, func(in map[string]any) {
		in["SizeRw"] = 3 << 20
		in["Mounts"] = []map[string]any{{"Type": "volume", "Destination": "/anon"}}
	})
	p.src.addRun("unknown", img, nil, func(in map[string]any) { delete(in, "SizeRw") })
	im := p.open(t)
	plan := planAll(t, im)
	if cp := planned(t, plan, "clean"); cp.WritableBytes != 0 || len(cp.Notes) != 0 {
		t.Errorf("clean: %+v", cp)
	}
	cp := planned(t, plan, "wrote")
	text := strings.Join(cp.Notes, "|")
	if cp.WritableBytes != 3<<20 || !strings.Contains(text, "writable layer holds") || !strings.Contains(text, "not copied") || !strings.Contains(text, "anonymous volume at /anon") {
		t.Errorf("wrote: %+v", cp)
	}
	if cp := planned(t, plan, "unknown"); cp.WritableBytes != -1 || !strings.Contains(strings.Join(cp.Notes, "|"), "may hold") {
		t.Errorf("unknown: %+v", cp)
	}
	res := runPlan(t, im, plan, Selection{Containers: []string{"wrote"}})
	for _, it := range res.Items {
		if it.Kind == KindContainer && !strings.Contains(it.Reason, "not copied") {
			t.Errorf("the result does not say it: %+v", it)
		}
	}
}

// An image that cannot come along is flagged: it lost its tag, it is not listed, or its
// tag names another image in hyperbutler. One hyperbutler has by id needs no tag.
func TestContainerImageFlagged(t *testing.T) {
	p := newPair(t)
	untagged := p.src.addImage()
	p.src.addRun("moved", untagged, nil)
	p.src.addRun("gone", nil, nil, func(in map[string]any) { in["Image"] = "sha256:0123456789ab" })
	clash := p.src.addImage("clash:1")
	p.dst.addImage("clash:1")
	p.src.addRun("clashing", clash, nil)
	there := p.src.addImage()
	p.dst.images[there.ID] = &fImage{ID: there.ID, Tags: []string{"local:1"}, Layers: there.Layers}
	p.src.addRun("byid", there, nil)
	im := p.open(t)
	plan := planAll(t, im)
	if cp := planned(t, plan, "moved"); cp.Status != StatusUnsupported || !strings.Contains(cp.Reason, "has no tag") {
		t.Errorf("moved: %+v", cp)
	}
	if cp := planned(t, plan, "gone"); cp.Status != StatusUnsupported || !strings.Contains(cp.Reason, "not in Docker Desktop's list") {
		t.Errorf("gone: %+v", cp)
	}
	if cp := planned(t, plan, "clashing"); cp.Status != StatusConflict || !strings.Contains(cp.Reason, "clash:1") {
		t.Errorf("clashing: %+v", cp)
	}
	if cp := planned(t, plan, "byid"); cp.Status != StatusNew || cp.Image != there.ID {
		t.Errorf("byid: %+v", cp)
	}
	res := runPlan(t, im, plan, Selection{Containers: []string{"moved", "gone", "clashing"}})
	got := outcomes(res)
	if got["container:moved"] != OutcomeRefused || got["container:gone"] != OutcomeRefused || got["container:clashing"] != OutcomeRefused || len(p.dst.createdBody) != 0 {
		t.Errorf("%v", got)
	}
}

// A name clash in hyperbutler is reported and never overwritten, also when it appears
// between the plan and the run.
func TestContainerNameClash(t *testing.T) {
	p := newPair(t)
	img := p.src.addImage("myapp:1")
	p.src.addRun("web", img, nil)
	p.src.addRun("late", img, nil)
	theirs := p.dst.addContainer("web", "running", "")
	im := p.open(t)
	plan := planAll(t, im)
	if cp := planned(t, plan, "web"); cp.Status != StatusConflict || !strings.Contains(cp.Reason, "not overwritten") {
		t.Fatalf("plan %+v", cp)
	}
	p.dst.mu.Lock()
	p.dst.addContainer("late", "running", "")
	p.dst.mu.Unlock()
	res := runPlan(t, im, plan, Selection{Containers: []string{"web", "late"}})
	got := outcomes(res)
	if got["container:web"] != OutcomeRefused || got["container:late"] != OutcomeRefused || res.OK() {
		t.Errorf("%v", got)
	}
	if p.dst.containers[theirs.ID] == nil || theirs.State != "running" || len(p.dst.createdBody) != 0 {
		t.Errorf("hyperbutler's container was touched: %+v", p.dst.containers)
	}
	for _, r := range p.dst.reqs() {
		if strings.HasPrefix(r, "DELETE /containers/"+theirs.ID) {
			t.Errorf("sent %s", r)
		}
	}
}

// A volume a running container holds is refused as before, and the container with it:
// no empty volume of that name is made, which would stand in the way of the real one.
func TestContainerVolumeNotImported(t *testing.T) {
	p := newPair(t)
	img := p.src.addImage("myapp:1")
	p.src.addImage("busybox:latest")
	p.src.addVolume("pgdata", files("x")...)
	p.src.addContainer("holder", "running", "pgdata")
	p.src.addRun("web", img, map[string]any{"Binds": []string{"pgdata:/data"}})
	im := p.open(t)
	plan := planAll(t, im)
	if !strings.Contains(strings.Join(planned(t, plan, "web").Notes, "|"), "stop holder") {
		t.Errorf("the plan does not warn: %+v", planned(t, plan, "web"))
	}
	res := runPlan(t, im, plan, Selection{Containers: []string{"web"}})
	got := outcomes(res)
	if got["volume:pgdata"] != OutcomeRefused || got["container:web"] != OutcomeRefused || res.OK() {
		t.Fatalf("%v", got)
	}
	if len(p.dst.createdBody) != 0 || slices.Contains(p.dst.volumeNames(), "pgdata") {
		t.Errorf("creates %v, volumes %v", p.dst.createdBody, p.dst.volumeNames())
	}
	// With --allow-live-volumes the copy goes on, as for a volume alone.
	res = runPlan(t, im, plan, Selection{Containers: []string{"web"}, AllowLiveVolumes: true})
	if outcomes(res)["container:web"] != OutcomeImported {
		t.Errorf("%+v", res)
	}
}

// An existing network is used as it is; only a bridge network is made; a subnet goes
// along only for a fixed address; a failed create takes the network it made with it.
func TestContainerNetworks(t *testing.T) {
	p := newPair(t)
	img := p.src.addImage("myapp:1")
	onNet := func(net string, ep map[string]any) func(map[string]any) {
		return func(in map[string]any) {
			in["NetworkSettings"] = map[string]any{"Networks": map[string]any{net: ep}}
		}
	}
	p.src.networks["shared"] = map[string]any{"Name": "shared", "Driver": "bridge"}
	p.dst.networks["shared"] = map[string]any{"Name": "shared", "Driver": "bridge", "Labels": map[string]any{"mine": "1"}}
	p.src.addRun("a", img, map[string]any{"NetworkMode": "shared"}, onNet("shared", map[string]any{}))

	p.src.networks["over"] = map[string]any{"Name": "over", "Driver": "overlay"}
	p.src.addRun("b", img, map[string]any{"NetworkMode": "over"}, onNet("over", map[string]any{}))

	p.src.addRun("c", img, map[string]any{"NetworkMode": "vanished"}, onNet("vanished", map[string]any{}))

	p.src.networks["fixed"] = map[string]any{"Name": "fixed", "Driver": "bridge", "IPAM": map[string]any{"Config": []map[string]string{{"Subnet": "10.9.0.0/24"}}}}
	p.src.addRun("d", img, map[string]any{"NetworkMode": "fixed"}, onNet("fixed", map[string]any{"IPAMConfig": map[string]any{"IPv4Address": "10.9.0.5"}}))

	p.src.networks["doomed"] = map[string]any{"Name": "doomed", "Driver": "bridge"}
	p.src.addRun("e", img, map[string]any{"NetworkMode": "doomed"}, onNet("doomed", map[string]any{}))
	p.dst.failCreate = "e"

	im := p.open(t)
	plan := planAll(t, im)
	if cp := planned(t, plan, "b"); cp.Status != StatusUnsupported || !strings.Contains(cp.Reason, "overlay") {
		t.Errorf("b: %+v", cp)
	}
	if cp := planned(t, plan, "c"); cp.Status != StatusUnsupported || !strings.Contains(cp.Reason, "vanished") {
		t.Errorf("c: %+v", cp)
	}
	res := runPlan(t, im, plan, Selection{AllContainers: true})
	got := outcomes(res)
	if got["container:a"] != OutcomeImported || got["container:d"] != OutcomeImported || got["container:e"] != OutcomeFailed {
		t.Fatalf("%v", got)
	}
	if l := p.dst.networks["shared"]["Labels"].(map[string]any); l["mine"] != "1" {
		t.Errorf("the existing network changed: %v", p.dst.networks["shared"])
	}
	if p.dst.networks["fixed"]["IPAM"] == nil {
		t.Errorf("the fixed address lost its subnet: %v", p.dst.networks["fixed"])
	}
	if p.dst.networks["doomed"] != nil {
		t.Errorf("the failed container's network stayed")
	}
	for _, it := range res.Items {
		if it.Name == "e" && !strings.Contains(it.Reason, "nothing of it is left") {
			t.Errorf("reason %q", it.Reason)
		}
	}
	assertReadOnly(t, p.src)
}

// A verification that fails removes what the create made.
func TestContainerVerifyFails(t *testing.T) {
	p := newPair(t)
	img := p.src.addImage("myapp:1")
	src := p.src.addRun("web", img, nil, func(in map[string]any) {
		in["Config"].(map[string]any)["Labels"] = map[string]string{}
	})
	im := p.open(t)
	plan := planAll(t, im)
	cp := planned(t, plan, "web")
	// The source's id, which the label must say, is not what the plan holds.
	plan.Containers[0].ID = strings.Repeat("a", len(src.ID))
	res := runPlan(t, im, plan, Selection{Containers: []string{cp.Name}})
	if outcomes(res)["container:web"] != OutcomeFailed {
		t.Fatalf("%+v", res)
	}
	p.dst.mu.Lock()
	defer p.dst.mu.Unlock()
	for _, c := range p.dst.containers {
		if c.Name == "web" {
			t.Errorf("left behind: %+v", c)
		}
	}
}

func TestContainerCanceled(t *testing.T) {
	p := newPair(t)
	img := p.src.addImage("myapp:1")
	p.src.addRun("web", img, nil)
	im := p.open(t)
	plan := planAll(t, im)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := im.Run(ctx, plan, Selection{Containers: []string{"web"}}, nil)
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err == nil && outcomes(res)["container:web"] != OutcomeCanceled {
		t.Errorf("%+v", res)
	}
	if len(p.dst.createdBody) != 0 {
		t.Errorf("created after a cancel: %v", p.dst.createdBody)
	}
}

// An inspect answer that cannot be read lists that container as unsupported: the others
// are still offered.
func TestContainerBadInspect(t *testing.T) {
	p := newPair(t)
	img := p.src.addImage("myapp:1")
	p.src.addRun("good", img, nil)
	bad := p.src.addRun("bad", img, nil)
	bad.Inspect = map[string]any{"Id": bad.ID, "HostConfig": "not an object"}
	other := p.src.addRun("other", img, nil)
	other.Inspect = map[string]any{"Id": "someone-else"}
	im := p.open(t)
	plan := planAll(t, im)
	if cp := planned(t, plan, "bad"); cp.Status != StatusUnsupported || !strings.Contains(cp.Reason, "could not be read") {
		t.Errorf("bad: %+v", cp)
	}
	if cp := planned(t, plan, "other"); cp.Status != StatusUnsupported || !strings.Contains(cp.Reason, "another container") {
		t.Errorf("other: %+v", cp)
	}
	if cp := planned(t, plan, "good"); cp.Status != StatusNew {
		t.Errorf("good: %+v", cp)
	}
}

func TestHostPath(t *testing.T) {
	for in, want := range map[string]string{
		"/run/desktop/mnt/host/c/Users/me":               `C:\Users\me`,
		"/host_mnt/d/src":                                `D:\src`,
		"/mnt/host/c":                                    `C:\`,
		"/run/desktop/mnt/host/wsl/Ubuntu":               `\\wsl$\Ubuntu`,
		"/mnt/wsl/docker-desktop-bind-mounts/Ubuntu/abc": `\\wsl$\Ubuntu\abc`,
		`/host_mnt/c/Users\..\..\Windows`:                `/host_mnt/c/Users\..\..\Windows`,
		`/host_mnt/c/Users/.. /x`:                        `/host_mnt/c/Users/.. /x`,
		"/var/lib/x":                                     "/var/lib/x",
		"/run/desktop/mnt/host/c/../etc":                 "/run/desktop/mnt/host/c/../etc",
		"/run/desktop/mnt/host/ab/x":                     "/run/desktop/mnt/host/ab/x",
		"/run/desktop/mnt/host/1/x":                      "/run/desktop/mnt/host/1/x",
		`C:\already`:                                     `C:\already`,
	} {
		if got := hostPath(in); got != want {
			t.Errorf("%s: %q, want %q", in, got, want)
		}
	}
}

func TestParseInspectMaps(t *testing.T) {
	in, err := parseInspect([]byte(`{"Id":"abcdef012345aaaa","Name":"/web","Config":{"Hostname":"chosen","Image":"a"},"HostConfig":{"NetworkMode":"default","LogConfig":{"Type":"splunk"},"Runtime":"kata","CgroupParent":"/x","SecurityOpt":["no-new-privileges"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	m := mapContainer(in)
	if len(m.requires) != 0 || m.unsupported != "" {
		t.Errorf("%+v", m)
	}
	if m.body.Hostname != "chosen" || m.body.HostConfig.NetworkMode != "bridge" || m.body.HostConfig.LogConfig != nil {
		t.Errorf("body %+v", m.body)
	}
	text := strings.Join(m.notes, "|")
	for _, want := range []string{"splunk", "kata", "cgroup parent"} {
		if !strings.Contains(text, want) {
			t.Errorf("no note about %s: %q", want, text)
		}
	}
}

// FuzzMapContainer feeds mapContainer what any engine could answer to docker inspect: it
// must not panic; what it shows is one clean line; and nothing that needs a confirmation
// goes into a create without being listed.
func FuzzMapContainer(f *testing.F) {
	for _, s := range []string{
		`{"Id":"a","Name":"/web","Config":{"Image":"x","Labels":{"com.docker.compose.project":"p"}}}`,
		`{"Id":"abc","Name":"/w\u001b[2J","Image":"sha256:1","SizeRw":-5,"HostConfig":{"Privileged":true,"NetworkMode":"host","PidMode":"host","IpcMode":"container:z"}}`,
		`{"Name":"web","HostConfig":{"Binds":["\\\\server\\share:/x","C:\\src:/y:ro","/etc:/z","pg:/d:rw","nope","C:/a"],"Mounts":[{"Type":"bind","Source":"/run/desktop/mnt/host/c/u","Target":"/t"},{"Type":"volume","Target":"/a"},{"Type":"tmpfs","Target":"/b"},{"Type":"cluster"}]}}`,
		`{"Name":"web","HostConfig":{"PortBindings":{"80/tcp":[{"HostIp":"127.0.0.1","HostPort":"8080"}],"x":[{"HostPort":";"}]},"Devices":[{"PathOnHost":"/dev/x"}],"CapAdd":["ALL"],"SecurityOpt":["seccomp=unconfined"]}}`,
		`{"Name":"web","HostConfig":{"NetworkMode":"net"},"NetworkSettings":{"Networks":{"net":{"Aliases":["a"],"IPAMConfig":{"IPv4Address":"10.0.0.1"}},"bad/name":{}}},"Mounts":[{"Type":"volume","Destination":"/anon"}]}`,
		`{"Name":"web","HostConfig":{"RestartPolicy":{"Name":"sometimes"},"Links":["/a:/b"],"LogConfig":{"Type":"x"}}}`,
		`{"Name":"web","HostConfig":{"SecurityOpt":["seccomp={\"a\":\"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\"}","apparmor=unconfined","systempaths:unconfined","no-new-privileges","label"]}}`,
		`{"Name":"web","HostConfig":{"CapAdd":["cap_sys_admin","net_admin"],"DeviceRequests":[{"Driver":"nvidia","DeviceIDs":["0"],"Evil":1}],"Binds":["/host_mnt/c/a\\..\\b:/x","C:\\a\\..\\b:/y"]}}`,
		`{"Name":"web","Config":{"Labels":{"com.docker.compose.project":""}},"HostConfig":{"Privileged":true,"Mounts":[{"Type":"bind","Source":"C:\\a","Target":"/a","BindOptions":{"Propagation":"rshared"}},{"Type":"volume","Target":"/v","VolumeOptions":{"DriverConfig":{"Name":"nfs"}}}]}}`,
		`[]`, `null`, `{"HostConfig":7}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		in, err := parseInspect(raw)
		if err != nil {
			if strings.ContainsAny(clean(err.Error()), "\x1b\r\n") {
				t.Fatalf("unclean error %q", err)
			}
			return
		}
		m := mapContainer(in)
		var shown []string
		shown = append(shown, m.name, m.state, m.project, m.projectDir, m.unsupported)
		for _, l := range [][]string{listed(m.requires), m.capAdd, listed(m.binds), m.ports, m.notes} {
			shown = append(shown, l...)
		}
		for _, s := range shown {
			if strings.ContainsAny(s, "\x1b\r\n\x00") {
				t.Fatalf("unclean text %q", s)
			}
		}
		if m.project != "" {
			return // never created
		}
		// Whatever else it has, every security option's key is shown, however long the
		// one before it: what is confirmed must be what is created.
		joined := strings.Join(m.requires, "\n")
		for _, o := range in.HostConfig.SecurityOpt {
			if k := securityKey(o); k != "no-new-privileges" && !strings.Contains(joined, k) {
				t.Fatalf("security option %q is not in %q", k, joined)
			}
		}
		for _, d := range in.HostConfig.Devices {
			if !strings.Contains(joined, cleanPath(d.PathOnHost)) {
				t.Fatalf("device %q is not in %q", d.PathOnHost, joined)
			}
		}
		if m.unsupported != "" {
			return // never created
		}
		if _, err := json.Marshal(m.body); err != nil {
			t.Fatalf("body does not encode: %v", err)
		}
		hc := m.body.HostConfig
		asked := len(m.requires) > 0
		if !asked && (hc.Privileged || hc.NetworkMode == "host" || hc.PidMode == "host" || hc.IpcMode == "host" || hc.UTSMode == "host" ||
			hc.UsernsMode == "host" || hc.CgroupnsMode == "host" || len(hc.Devices)+len(hc.DeviceCgroupRules)+len(hc.DeviceRequests) > 0) {
			t.Fatalf("host access without a confirmation: %s", raw)
		}
		if !asked {
			for _, c := range hc.CapAdd {
				if dangerousCaps[capName(c)] {
					t.Fatalf("capability %q without a confirmation", c)
				}
			}
			for _, b := range hc.Binds {
				if src, _, _, err := share.ParseBind(b); err != nil || hasDotDot(src) {
					t.Fatalf("bind %q without a confirmation", b)
				}
			}
			for _, ms := range hc.Mounts {
				if ms.Type == "bind" && hasDotDot(ms.Source) {
					t.Fatalf("mount source %q without a confirmation", ms.Source)
				}
			}
		}
		for _, ms := range hc.Mounts {
			if ms.BindOptions != nil || (ms.VolumeOptions != nil && ms.VolumeOptions.DriverConfig != nil) {
				t.Fatalf("mount options went into the create: %+v", ms)
			}
		}
		for _, n := range append(slices.Clone(m.volumes), m.networks...) {
			if strings.ContainsAny(n, `/\:`) {
				t.Fatalf("name %q would leave its path segment", n)
			}
		}
		if m.body.Labels[source.HelperLabel] != "" {
			t.Fatalf("a source's helper label was carried over: %v", m.body.Labels)
		}
	})
}

func mapped(t *testing.T, raw string) *mappedContainer {
	t.Helper()
	in, err := parseInspect([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return mapContainer(in)
}

// What is confirmed is what is created: nothing of the list or of an item is cut.
func TestRequiresShowEverything(t *testing.T) {
	profile := strings.Repeat("x", 5000)
	in := map[string]any{"Name": "/web", "HostConfig": map[string]any{"SecurityOpt": []string{
		"seccomp=" + profile, "apparmor=unconfined", "systempaths=unconfined", "label:disable", "no-new-privileges"}}}
	raw, _ := json.Marshal(in)
	m := mapped(t, string(raw))
	text := strings.Join(m.requires, "|")
	for _, want := range []string{"seccomp=(5000 bytes)", "apparmor=unconfined", "systempaths=unconfined", "label=disable"} {
		if !strings.Contains(text, want) {
			t.Errorf("%q is not in %q", want, text)
		}
	}
	if strings.Contains(text, "xxx") || strings.Contains(text, "no-new-privileges") {
		t.Errorf("shown: %q", text)
	}
	if len(m.body.HostConfig.SecurityOpt) != 5 {
		t.Errorf("the create lost options: %d", len(m.body.HostConfig.SecurityOpt))
	}

	// More options than any bound, and the sentence after a long source, and a long target.
	long := strings.Repeat("d", 300)
	var opts []string
	for i := range 40 {
		opts = append(opts, "key"+string(rune('a'+i%26))+string(rune('a'+i/26))+"=1")
	}
	raw, _ = json.Marshal(map[string]any{"Name": "/web", "HostConfig": map[string]any{"SecurityOpt": opts, "Binds": []string{
		`\\server\` + long + `:/a`, `C:\` + long + `:/` + long}}})
	m = mapped(t, string(raw))
	text = strings.Join(listed(m.requires), "|")
	for _, o := range opts {
		if !strings.Contains(text, o) {
			t.Errorf("%s is not shown", o)
		}
	}
	if !strings.Contains(text, "created without this mount") || !strings.Contains(text, "Copy the files") || !strings.Contains(text, long) {
		t.Errorf("the refused bind's sentence was cut: %q", text)
	}
	if b := m.binds[0]; !strings.HasSuffix(b, "-> /"+long) || !strings.Contains(b, `C:\`+long) {
		t.Errorf("the bind was cut: %q", b)
	}
}

func TestDevicesAreDecoded(t *testing.T) {
	m := mapped(t, `{"Name":"/web","HostConfig":{
		"Devices":[{"PathOnHost":"/dev/kvm","PathInContainer":"/dev/kvm","CgroupPermissions":"rwm","Evil":"x"},{"PathOnHost":"/dev/fuse\u001b[2J"}],
		"DeviceRequests":[{"Driver":"nvidia","Count":1,"DeviceIDs":["GPU-0"],"Evil":"y"}],
		"DeviceCgroupRules":["c 1:3 rwm"]}}`)
	text := strings.Join(m.requires, "|")
	for _, want := range []string{"/dev/kvm", "/dev/fuse", "nvidia", "GPU-0", "c 1:3 rwm"} {
		if !strings.Contains(text, want) {
			t.Errorf("%q is not in %q", want, text)
		}
	}
	if strings.Contains(text, "\x1b") {
		t.Errorf("unclean %q", text)
	}
	b, err := json.Marshal(m.body.HostConfig)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "Evil") || !strings.Contains(string(b), `"PathOnHost":"/dev/kvm"`) || !strings.Contains(string(b), `"DeviceIDs":["GPU-0"]`) {
		t.Errorf("create %s", b)
	}
	if m := mapped(t, `{"Name":"/web","HostConfig":{}}`); len(m.requires) != 0 {
		t.Errorf("none: %q", m.requires)
	}
}

func TestDangerousCapsNeedConfirm(t *testing.T) {
	p := newPair(t)
	img := p.src.addImage("myapp:1")
	for name, caps := range map[string][]string{
		"all": {"ALL"}, "lower": {"sys_admin"}, "prefix": {"CAP_SYS_MODULE"}, "mixed": {"Cap_Sys_Rawio"}, "ptrace": {"SYS_PTRACE"},
		"dac": {"DAC_READ_SEARCH"}, "bpf": {"BPF"}, "perf": {"PERFMON"},
	} {
		p.src.addRun(name, img, map[string]any{"CapAdd": caps})
	}
	p.src.addRun("fine", img, map[string]any{"CapAdd": []string{"NET_ADMIN"}})
	im := p.open(t)
	plan := planAll(t, im)
	res := runPlan(t, im, plan, Selection{AllContainers: true})
	got := outcomes(res)
	if got["container:fine"] != OutcomeImported || len(res.Skipped) != 8 || len(p.dst.createdBody) != 1 {
		t.Fatalf("%v, skipped %+v", got, res.Skipped)
	}
	if cp := planned(t, plan, "all"); cp.Status != StatusConfirm || !strings.Contains(cp.Reason, "capability ALL") || !slices.Equal(cp.CapAdd, []string{"ALL"}) {
		t.Errorf("all: %+v", cp)
	}
	if cp := planned(t, plan, "prefix"); !strings.Contains(cp.Reason, "capability SYS_MODULE") {
		t.Errorf("prefix: %+v", cp)
	}
}

// A create that was canceled may still have made the container: it is removed when it is
// this run's, and otherwise the result says so and its network stays.
func TestCreateCanceled(t *testing.T) {
	for _, tc := range []struct {
		name    string
		hook    func(p *pair, c *fContainer)
		remains bool
	}{
		{name: "removed"},
		{name: "not this run's", remains: true, hook: func(_ *pair, c *fContainer) { delete(c.Labels, source.RunLabel) }},
		{name: "removal fails", remains: true, hook: func(p *pair, _ *fContainer) {
			p.dst.failRemove = func(*fContainer) bool { return true }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPair(t)
			img := p.src.addImage("myapp:1")
			p.src.networks["backend"] = map[string]any{"Name": "backend", "Driver": "bridge"}
			p.src.addRun("web", img, map[string]any{"NetworkMode": "backend"}, func(in map[string]any) {
				in["NetworkSettings"] = map[string]any{"Networks": map[string]any{"backend": map[string]any{}}}
			})
			im := p.open(t)
			plan := planAll(t, im)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			p.dst.onCreate = func(c *fContainer) {
				if tc.hook != nil {
					tc.hook(p, c)
				}
				cancel() // and the handler answers 201
			}
			res, err := im.Run(ctx, plan, Selection{Containers: []string{"web"}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			var it ItemResult
			for _, x := range res.Items {
				if x.Kind == KindContainer {
					it = x
				}
			}
			if it.Outcome != OutcomeCanceled {
				t.Fatalf("%+v", it)
			}
			p.dst.mu.Lock()
			defer p.dst.mu.Unlock()
			there := false
			for _, c := range p.dst.containers {
				there = there || c.Name == "web"
			}
			if there != tc.remains || (p.dst.networks["backend"] != nil) != tc.remains {
				t.Errorf("container there %v, network %v; want %v", there, p.dst.networks["backend"], tc.remains)
			}
			if said := strings.Contains(it.Reason, "may remain"); said != tc.remains || strings.Contains(it.Reason, "nothing of it is left") == tc.remains {
				t.Errorf("reason %q", it.Reason)
			}
		})
	}
}

// A failed create dockerd answered also looks: it may have made the container first.
func TestCreateFailsAfterMaking(t *testing.T) {
	p := newPair(t)
	p.src.addRun("web", p.src.addImage("myapp:1"), nil)
	p.dst.failAfterCreate = "web"
	stray := p.dst.addContainer("stray", "created", "")
	im := p.open(t)
	plan := planAll(t, im)
	res := runPlan(t, im, plan, Selection{Containers: []string{"web"}})
	if outcomes(res)["container:web"] != OutcomeFailed || p.dst.containers[stray.ID] == nil {
		t.Errorf("%+v", res)
	}
	for _, c := range p.dst.containers {
		if c.Name == "web" {
			t.Errorf("left behind: %+v", c)
		}
	}
}

// An empty project label names no project: the container is checked like any other.
func TestEmptyComposeLabel(t *testing.T) {
	p := newPair(t)
	p.src.addRun("web", p.src.addImage("myapp:1"), map[string]any{"Privileged": true}, func(in map[string]any) {
		in["Config"].(map[string]any)["Labels"] = map[string]string{composeProjectLabel: ""}
	})
	im := p.open(t)
	plan := planAll(t, im)
	cp := planned(t, plan, "web")
	if cp.Status != StatusConfirm || cp.Project != "" || !strings.Contains(cp.Reason, "privileged") {
		t.Fatalf("plan %+v", cp)
	}
	res := runPlan(t, im, plan, Selection{ConfirmContainers: []string{"web"}})
	if !res.OK() || p.dst.createdBody[0]["HostConfig"].(map[string]any)["Privileged"] != true {
		t.Errorf("%+v", res)
	}
}

// A network to create is shown before the copy, escaped, with the subnet it takes along;
// an IPAM driver hyperbutler does not have stays behind, and the plan says so.
func TestNewNetworkIsShown(t *testing.T) {
	p := newPair(t)
	img := p.src.addImage("myapp:1")
	p.src.networks["fixed"] = map[string]any{"Name": "fixed", "Driver": "bridge",
		"IPAM": map[string]any{"Driver": "evil", "Config": []map[string]string{{"Subnet": "10.9.0.0/24\x1b[2J"}}}}
	p.src.addRun("web", img, map[string]any{"NetworkMode": "fixed"}, func(in map[string]any) {
		in["NetworkSettings"] = map[string]any{"Networks": map[string]any{"fixed": map[string]any{"IPAMConfig": map[string]any{"IPv4Address": "10.9.0.5"}}}}
	})
	im := p.open(t)
	plan := planAll(t, im)
	cp := planned(t, plan, "web")
	notes := strings.Join(cp.Notes, "|")
	if !strings.Contains(notes, "network fixed will be created (subnet 10.9.0.0/24") || !strings.Contains(notes, "IPAM driver evil") || strings.Contains(notes, "\x1b") {
		t.Fatalf("notes %q", notes)
	}
	var said []string
	if _, err := im.Run(context.Background(), plan, Selection{Containers: []string{"web"}}, func(e Event) {
		if e.Kind == EventNote {
			said = append(said, e.Message)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if text := strings.Join(said, "|"); !strings.Contains(text, "network fixed will be created (subnet 10.9.0.0/24") || strings.Contains(text, "\x1b") {
		t.Errorf("announced %q", text)
	}
	ipam := p.dst.networks["fixed"]["IPAM"].(map[string]any)
	if ipam["Driver"] != nil || ipam["Config"] == nil {
		t.Errorf("IPAM %v", ipam)
	}

	// One hyperbutler has is used as it is: nothing to say.
	plan = planAll(t, im)
	if cp := planned(t, plan, "web"); strings.Contains(strings.Join(cp.Notes, "|"), "will be created") {
		t.Errorf("notes %q", cp.Notes)
	}
}

func TestDotDotNeedsConfirm(t *testing.T) {
	for _, bind := range []string{`/host_mnt/c/Users\..\..\Windows:/x`, `/run/desktop/mnt/host/c/a/../b:/x`, `C:\a\..\b:/x`} {
		raw, _ := json.Marshal(map[string]any{"Name": "/web", "HostConfig": map[string]any{"Binds": []string{bind}}})
		m := mapped(t, string(raw))
		if len(m.requires) != 1 || len(m.body.HostConfig.Binds) != 1 {
			t.Errorf("%s: requires %q, binds %q", bind, m.requires, m.body.HostConfig.Binds)
		}
		if strings.HasPrefix(bind, "/") && !strings.HasPrefix(m.body.HostConfig.Binds[0], "/") {
			t.Errorf("%s became a Windows path: %q", bind, m.body.HostConfig.Binds)
		}
	}
}

func TestMountOptionsAreNoted(t *testing.T) {
	m := mapped(t, `{"Name":"/web","HostConfig":{"Mounts":[
		{"Type":"bind","Source":"C:\\a","Target":"/a","BindOptions":{"Propagation":"rshared"}},
		{"Type":"bind","Source":"C:\\b","Target":"/b","BindOptions":{"NonRecursive":true}},
		{"Type":"bind","Source":"C:\\c","Target":"/c","BindOptions":{"CreateMountpoint":true}},
		{"Type":"bind","Source":"C:\\d","Target":"/d","BindOptions":{}},
		{"Type":"volume","Source":"pg","Target":"/v","VolumeOptions":{"NoCopy":true,"DriverConfig":{"Name":"nfs","Options":{"o":"addr=x"}}}}]}}`)
	text := strings.Join(m.notes, "|")
	for _, want := range []string{"mount at /a", "mount at /b", "mount at /c", "volume driver settings of the mount at /v"} {
		if !strings.Contains(text, want) {
			t.Errorf("%q is not in %q", want, text)
		}
	}
	if strings.Contains(text, "mount at /d") {
		t.Errorf("empty options noted: %q", text)
	}
	b, _ := json.Marshal(m.body.HostConfig.Mounts)
	if strings.Contains(string(b), "BindOptions") || strings.Contains(string(b), "DriverConfig") || !strings.Contains(string(b), `"NoCopy":true`) {
		t.Errorf("create %s", b)
	}
}

// A source that lists one name twice cannot say which a name selects: both are refused.
func TestDuplicateContainerNames(t *testing.T) {
	p := newPair(t)
	img := p.src.addImage("myapp:1")
	a := p.src.addRun("web", img, nil)
	b := p.src.addRun("web", img, nil)
	p.src.addRun("other", img, nil)
	im := p.open(t)
	plan := planAll(t, im)
	n := 0
	for _, cp := range plan.Containers {
		if cp.Name == "web" {
			n++
			if cp.Status != StatusUnsupported || !strings.Contains(cp.Reason, "more than one") || (cp.ID != a.ID && cp.ID != b.ID) {
				t.Errorf("%+v", cp)
			}
		}
	}
	if n != 2 {
		t.Fatalf("%d in the plan", n)
	}
	res := runPlan(t, im, plan, Selection{ConfirmContainers: []string{"web", a.ID}})
	for _, it := range res.Items {
		if it.Kind == KindContainer && it.Outcome != OutcomeRefused {
			t.Errorf("%+v", it)
		}
	}
	res = runPlan(t, im, plan, Selection{AllContainers: true})
	if len(res.Skipped) != 2 || outcomes(res)["container:other"] != OutcomeImported {
		t.Errorf("%+v", res)
	}
	for _, c := range p.dst.createdBody {
		if c["Labels"].(map[string]any)[containerFromLabel] == a.ID || c["Labels"].(map[string]any)[containerFromLabel] == b.ID {
			t.Errorf("created %v", c)
		}
	}
}
