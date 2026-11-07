package desktopimport

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/Higangssh/hyperbutler/internal/desktopimport/source"
	"github.com/Higangssh/hyperbutler/internal/dockerapi"
	"github.com/Higangssh/hyperbutler/internal/engine"
)

// Importer holds the two engines: Docker Desktop's, reached only through the read-only
// source client, and hyperbutler's.
type Importer struct {
	src    *source.Client
	dst    *dockerapi.Client
	ep     Endpoint
	target string
	home   string // the installation's state directory
	// mixed: one engine keeps images in containerd's store and the other does not, so
	// the same image has different ids on the two.
	mixed            bool
	srcInfo, dstInfo engineInfo

	alive   func(run string) bool // whether an import run is going on: runAlive
	spaceFn func() Space          // the installation's free space: installSpace(home)
	hooks   testHooks

	mu    sync.Mutex
	notes []string // problems found while cleaning up
}

// testHookEnv sets testHooks for a check on a real machine; nothing sets it otherwise.
const testHookEnv = "HYPERBUTLER_TEST_IMPORT_HOOK"

// testHooks make an import misbehave on purpose, to check the next one recovers.
type testHooks struct {
	// keepCopyingHelper leaves the helper container that filled a volume in hyperbutler
	// (as if its removal were lost) while the import goes on and verifies the copy.
	keepCopyingHelper bool
}

// Open discovers Docker Desktop (Discover) and checks that hyperbutler's engine on
// o.TargetPipe answers and is another engine. Close releases it.
func Open(ctx context.Context, o Options) (*Importer, error) {
	if o.TargetPipe == "" {
		return nil, errors.New("desktopimport: no target pipe")
	}
	home := o.Home
	if home == "" {
		var err error
		if home, err = engine.DefaultHome(); err != nil {
			return nil, err
		}
	}
	ep, err := Discover(ctx, o)
	if err != nil {
		return nil, err
	}
	if strings.EqualFold(ep.Pipe, o.TargetPipe) {
		return nil, ErrSameEngine
	}
	im := &Importer{
		src:    source.New(dockerapi.New(dialer(o, ep.Pipe))),
		dst:    dockerapi.New(dialer(o, o.TargetPipe)),
		ep:     ep,
		target: o.TargetPipe,
		home:   home,
		alive:  runAlive,
	}
	if os.Getenv(testHookEnv) == "keep-copying-helper" {
		im.hooks.keepCopyingHelper = true
	}
	pctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	if err := im.src.GetJSON(pctx, "/info", nil, &im.srcInfo); err != nil {
		im.Close()
		return nil, fmt.Errorf("Docker Desktop's engine: %w", err) //nolint:staticcheck // ST1005: a product name
	}
	if err := im.dst.JSON(pctx, http.MethodGet, "/info", nil, nil, &im.dstInfo); err != nil {
		im.Close()
		if dockerapi.StatusOf(err) == 0 {
			return nil, fmt.Errorf("%w (nothing answers on %s)", ErrTargetNotRunning, o.TargetPipe)
		}
		return nil, fmt.Errorf("hyperbutler's engine: %w", err)
	}
	if im.srcInfo.ID != "" && im.srcInfo.ID == im.dstInfo.ID {
		im.Close()
		return nil, ErrSameEngine
	}
	im.mixed = im.srcInfo.containerdStore() != im.dstInfo.containerdStore()
	return im, nil
}

// Close releases the connections.
func (im *Importer) Close() {
	im.src.Close()
	im.dst.Close()
}

// Source is Docker Desktop's endpoint.
func (im *Importer) Source() Endpoint { return im.ep }

// --- the Docker API's types, the fields used ------------------------------------------

type apiImage struct {
	ID       string   `json:"Id"`
	RepoTags []string `json:"RepoTags"`
	Size     int64    `json:"Size"`
}

// imageConfig is the part of an image's config that says what a container of it runs.
type imageConfig struct {
	Cmd          []string            `json:"Cmd"`
	Entrypoint   []string            `json:"Entrypoint"`
	Env          []string            `json:"Env"`
	Labels       map[string]string   `json:"Labels"`
	WorkingDir   string              `json:"WorkingDir"`
	User         string              `json:"User"`
	ExposedPorts map[string]struct{} `json:"ExposedPorts"`
	Volumes      map[string]struct{} `json:"Volumes"`
}

func (c imageConfig) equal(o imageConfig) bool {
	return slices.Equal(c.Cmd, o.Cmd) && slices.Equal(c.Entrypoint, o.Entrypoint) && slices.Equal(c.Env, o.Env) &&
		maps.Equal(c.Labels, o.Labels) && c.WorkingDir == o.WorkingDir && c.User == o.User &&
		maps.Equal(c.ExposedPorts, o.ExposedPorts) && maps.Equal(c.Volumes, o.Volumes)
}

type apiImageInspect struct {
	ID           string      `json:"Id"`
	RepoTags     []string    `json:"RepoTags"`
	Created      string      `json:"Created"`
	Architecture string      `json:"Architecture"`
	Os           string      `json:"Os"`
	Config       imageConfig `json:"Config"`
	RootFS       struct {
		Layers []string `json:"Layers"`
	} `json:"RootFS"`
}

// helperOK reports whether a helper container can be created from the image on an
// engine with info: it declares no VOLUME (a container created from it would get an
// anonymous volume for each, a change to that engine) and is for the engine's platform.
func (i *apiImageInspect) helperOK(info engineInfo) bool {
	if len(i.Config.Volumes) > 0 {
		return false
	}
	goos, arch := info.platform()
	return (goos == "" || i.Os == goos) && (arch == "" || i.Architecture == arch)
}

type apiVolume struct {
	Name    string            `json:"Name"`
	Driver  string            `json:"Driver"`
	Labels  map[string]string `json:"Labels"`
	Options map[string]string `json:"Options"`
}

type apiContainer struct {
	ID      string            `json:"Id"`
	Names   []string          `json:"Names"`
	ImageID string            `json:"ImageID"`
	State   string            `json:"State"`
	Labels  map[string]string `json:"Labels"`
	Mounts  []struct {
		Type string `json:"Type"`
		Name string `json:"Name"`
	} `json:"Mounts"`
}

func (c apiContainer) name() string {
	for _, n := range c.Names {
		if n = strings.TrimPrefix(n, "/"); n != "" {
			return dockerapi.Clean(n, 128)
		}
	}
	return shortID(c.ID)
}

func (c apiContainer) helper() bool { return c.Labels[source.HelperLabel] == source.HelperLabelValue }

func shortID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		id = id[:12]
	}
	return id
}

// realTags drops the "<none>:<none>" placeholder and anything that is not a valid
// reference.
func realTags(tags []string) []string {
	var out []string
	for _, t := range tags {
		if t == "" || strings.HasPrefix(t, "<none>") || dockerapi.ValidImageRef(t) != nil {
			continue
		}
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// splitTag splits name:tag into the tag API's repo and tag.
func splitTag(ref string) (repo, tag string) {
	if i := strings.LastIndexByte(ref, ':'); i > strings.LastIndexByte(ref, '/') {
		return ref[:i], ref[i+1:]
	}
	return ref, "latest"
}

// normalizeRef is a reference as the image list shows it: busybox is busybox:latest,
// docker.io/library/busybox is busybox.
func normalizeRef(ref string) string {
	ref = strings.TrimPrefix(ref, "docker.io/")
	ref = strings.TrimPrefix(ref, "library/")
	if strings.Contains(ref, "@") {
		return ref
	}
	if i := strings.LastIndexByte(ref, ':'); i < 0 || i < strings.LastIndexByte(ref, '/') {
		ref += ":latest"
	}
	return ref
}

// --- hyperbutler's images ----------------------------------------------------------------

// dstImages is hyperbutler's images: ids and which id each tag names.
type dstImages struct {
	ids  map[string]bool
	tags map[string]string
	// bySize are the images, smallest first (candidates for the helper container).
	bySize []apiImage
}

func (im *Importer) loadDstImages(ctx context.Context) (dstImages, error) {
	var list []apiImage
	if err := im.dst.JSON(ctx, http.MethodGet, "/images/json", nil, nil, &list); err != nil {
		return dstImages{}, fmt.Errorf("list hyperbutler's images: %w", err)
	}
	d := dstImages{ids: map[string]bool{}, tags: map[string]string{}}
	for _, i := range list {
		if dockerapi.ValidImageRef(i.ID) != nil {
			continue
		}
		d.ids[i.ID] = true
		for _, t := range realTags(i.RepoTags) {
			d.tags[t] = i.ID
		}
		d.bySize = append(d.bySize, i)
	}
	sort.SliceStable(d.bySize, func(a, b int) bool { return d.bySize[a].Size < d.bySize[b].Size })
	return d, nil
}

// dstHelperImage is the smallest image in hyperbutler a helper container can be created
// from (apiImageInspect.helperOK), or "" when there is none.
func (im *Importer) dstHelperImage(ctx context.Context, d dstImages) (string, error) {
	for _, i := range d.bySize {
		var in apiImageInspect
		if err := im.dst.JSON(ctx, http.MethodGet, "/images/"+i.ID+"/json", nil, nil, &in); err != nil {
			if dockerapi.IsNotFound(err) {
				continue
			}
			return "", fmt.Errorf("inspect hyperbutler's image %s: %w", shortID(i.ID), err)
		}
		if in.helperOK(im.dstInfo) {
			return i.ID, nil
		}
	}
	return "", nil
}

// sameImage reports whether an image in hyperbutler (inspected by ref) is the source
// image: the same id, or, when the two engines use different image stores, the same
// layers, creation time, platform and config (command, entrypoint, environment, labels,
// working directory, user, ports and volumes).
func (im *Importer) sameImage(ctx context.Context, src *apiImageInspect, ref string) (bool, string, error) {
	var d apiImageInspect
	if err := im.dst.JSON(ctx, http.MethodGet, "/images/"+ref+"/json", nil, nil, &d); err != nil {
		return false, "", err
	}
	if err := dockerapi.ValidImageRef(d.ID); err != nil {
		return false, "", fmt.Errorf("hyperbutler's image %s: %w", ref, err)
	}
	if d.ID == src.ID {
		return true, d.ID, nil
	}
	if !im.mixed || len(src.RootFS.Layers) == 0 {
		return false, d.ID, nil
	}
	same := slices.Equal(d.RootFS.Layers, src.RootFS.Layers) && d.Created == src.Created &&
		d.Architecture == src.Architecture && d.Os == src.Os && d.Config.equal(src.Config)
	return same, d.ID, nil
}

// imageStatus compares a source image with hyperbutler's images. It returns the plan's
// status, missing and conflicting tags, and the id the image has in hyperbutler when it
// is there.
func (im *Importer) imageStatus(ctx context.Context, src *apiImageInspect, tags []string, d dstImages) (Status, []string, []string, string, error) {
	if len(tags) == 0 {
		return StatusUnsupported, nil, nil, "", nil
	}
	targetID := ""
	if d.ids[src.ID] {
		targetID = src.ID
	}
	var missing, conflicts []string
	for _, t := range tags {
		id, ok := d.tags[t]
		switch {
		case !ok:
			missing = append(missing, t)
		case id == src.ID:
		default:
			same, tid, err := im.sameImage(ctx, src, t)
			if err != nil {
				return "", nil, nil, "", err
			}
			if same && (targetID == "" || targetID == tid) {
				targetID = tid
			} else {
				conflicts = append(conflicts, t)
			}
		}
	}
	switch {
	case len(conflicts) > 0:
		return StatusConflict, missing, conflicts, targetID, nil
	case targetID == "":
		return StatusNew, missing, nil, "", nil
	case len(missing) > 0:
		return StatusPartial, missing, nil, targetID, nil
	}
	return StatusAlready, nil, nil, targetID, nil
}

func (im *Importer) inspectSource(ctx context.Context, id string) (*apiImageInspect, error) {
	if err := dockerapi.ValidImageRef(id); err != nil {
		return nil, err
	}
	var s apiImageInspect
	if err := im.src.GetJSON(ctx, "/images/"+id+"/json", nil, &s); err != nil {
		return nil, err
	}
	if s.ID != id {
		return nil, fmt.Errorf("image %s: the engine answered for %q", shortID(id), dockerapi.Clean(s.ID, 80))
	}
	return &s, nil
}

// --- the plan -----------------------------------------------------------------------------

// PlanOptions tune Plan.
type PlanOptions struct {
	// CompareVolumes hashes a volume that exists in hyperbutler under the same name, and
	// the Docker Desktop one, to tell "already" from "conflict". It reads both volumes
	// whole; without it such a volume is StatusExists.
	CompareVolumes bool
	// Containers also reads Docker Desktop's containers (Plan.Containers): one inspect
	// with sizes per container, and a look at hyperbutler's for each name.
	Containers bool
	// Progress, when set, gets EventProgress while volumes are compared.
	Progress func(Event)
}

// noHelperNote says why volumes cannot be copied when Docker Desktop has no image for
// the helper container.
const noHelperNote = "Docker Desktop has no image to create the volume helper container from without changing anything there: " +
	"it must have no VOLUME (a container created from an image such as redis, postgres or mysql gets anonymous volumes in Docker Desktop) " +
	"and be for its platform. Volumes are skipped; pull a small image there (docker --context desktop-linux pull busybox) or copy them by hand (docs/tools.md)"

// Plan lists Docker Desktop's images and volumes and compares each with hyperbutler.
// It only reads: with CompareVolumes it creates helper containers, never started, and
// removes them, and it removes nothing else. What an interrupted import left behind is
// listed in the notes (and a half-filled volume shows as new); Run removes it.
func (im *Importer) Plan(ctx context.Context, po PlanOptions) (*Plan, error) {
	p := &Plan{Source: im.ep, Target: im.target}
	if im.ep.Note != "" {
		p.Notes = append(p.Notes, im.ep.Note)
	}
	run, err := newRun()
	if err != nil {
		return nil, err
	}
	defer run.done()
	defer func() {
		im.mu.Lock()
		p.Notes = append(p.Notes, im.notes...)
		im.notes = nil
		im.mu.Unlock()
	}()
	complete, err := loadComplete(im.completePath())
	if err != nil {
		return nil, err
	}
	stale := im.findStale(ctx, complete)
	p.Recovery = stale.planNotes()

	var list []apiImage
	if err := im.src.GetJSON(ctx, "/images/json", nil, &list); err != nil {
		return nil, fmt.Errorf("list Docker Desktop's images: %w", err)
	}
	d, err := im.loadDstImages(ctx)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].Size < list[j].Size })
	// The helper image: the smallest tagged image a helper can be created from (it can
	// be copied to hyperbutler for its own helper when hyperbutler has none), else the
	// smallest untagged one.
	var untagged []string
	for _, i := range list {
		if dockerapi.ValidImageRef(i.ID) != nil {
			p.Notes = append(p.Notes, fmt.Sprintf("skipped an image with an unexpected id %q", dockerapi.Clean(i.ID, 80)))
			continue
		}
		ip := ImagePlan{ID: i.ID, Tags: realTags(i.RepoTags), Size: i.Size}
		if len(ip.Tags) == 0 {
			ip.Status, ip.Reason = StatusUnsupported, "untagged: give it a tag in Docker Desktop to import it"
			untagged = append(untagged, i.ID)
		} else {
			si, err := im.inspectSource(ctx, i.ID)
			if err != nil {
				return nil, fmt.Errorf("inspect Docker Desktop's image %s: %w", shortID(i.ID), err)
			}
			if p.HelperImage == "" && si.helperOK(im.srcInfo) {
				p.HelperImage = i.ID
			}
			ip.Status, ip.MissingTags, ip.Conflicts, _, err = im.imageStatus(ctx, si, ip.Tags, d)
			if err != nil {
				return nil, err
			}
			ip.Reason = imageReason(ip)
		}
		p.Images = append(p.Images, ip)
	}
	for _, id := range untagged {
		if p.HelperImage != "" {
			break
		}
		if si, err := im.inspectSource(ctx, id); err == nil && si.helperOK(im.srcInfo) {
			p.HelperImage = id
		}
	}
	sort.SliceStable(p.Images, func(i, j int) bool { return p.Images[i].Name() < p.Images[j].Name() })

	vols, err := im.sourceVolumes(ctx)
	if err != nil {
		return nil, err
	}
	sizes := im.volumeSizes(ctx)
	users, err := im.liveUsers(ctx)
	if err != nil {
		return nil, err
	}
	dstHelper, dstHelperDone := "", false
	for _, v := range vols {
		vp := VolumePlan{Name: v.Name, Driver: v.Driver, Size: -1, labels: v.Labels, UsedBy: users[v.Name]}
		if s, ok := sizes[v.Name]; ok {
			vp.Size = s
		}
		if why := unsupportedVolume(v); why != "" {
			vp.Status, vp.Reason = StatusUnsupported, why
			p.Volumes = append(p.Volumes, vp)
			continue
		}
		tv, err := im.dstVolume(ctx, v.Name)
		if err != nil {
			return nil, err
		}
		if tv != nil && !dstHelperDone && po.CompareVolumes && p.HelperImage != "" {
			if dstHelper, err = im.dstHelperImage(ctx, d); err != nil {
				return nil, err
			}
			dstHelperDone = true
		}
		switch {
		case tv == nil:
			vp.Status = StatusNew
		case slices.Contains(stale.volumes, v.Name):
			vp.Status, vp.Reason = StatusNew, "an interrupted import left an incomplete copy in hyperbutler; the import removes it first"
		case !po.CompareVolumes:
			vp.Status, vp.Reason = StatusExists, "a volume of this name is in hyperbutler; its content is compared when it is imported"
		case p.HelperImage == "" || dstHelper == "":
			vp.Status, vp.Reason = StatusExists, "a volume of this name is in hyperbutler; there is no image on both sides to compare the content with"
		default:
			sh, err := im.hashSource(ctx, run.id, p.HelperImage, v.Name, vp.Size, PhaseCompare, po.Progress)
			if err != nil {
				return nil, err
			}
			th, err := im.hashTarget(ctx, run.id, dstHelper, v.Name, PhaseCompare, po.Progress)
			if err != nil {
				return nil, err
			}
			vp.SourceHash, vp.TargetHash = sh.Sum(), th.Sum()
			same, why := sameVolumeMeta(v.Labels, tv)
			switch {
			case vp.SourceHash != vp.TargetHash:
				vp.Status, vp.Reason = StatusConflict, "a volume of this name in hyperbutler has different content"
			case same:
				vp.Status, vp.Reason = StatusAlready, "in hyperbutler with the same content and labels"
			default:
				vp.Status, vp.Reason = StatusDifferentLabels, "a volume of this name in hyperbutler has the same content but "+why
				if th.empty() && recreatable(tv) {
					vp.Reason += "; it is empty, so the import creates it again with Docker Desktop's labels"
				}
			}
		}
		p.Volumes = append(p.Volumes, vp)
	}
	if p.HelperImage == "" && len(p.Volumes) > 0 {
		p.Notes = append(p.Notes, noHelperNote)
	}
	if po.Containers {
		if err := im.planContainers(ctx, p, d); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// Select is the plan narrowed to the items sel names, resolved as Run resolves them (a
// tag as busybox, busybox:latest or docker.io/library/busybox, an id or an id prefix
// of 12 or more hex digits, a volume name). A name that is not in the plan is an error.
// AllImages, AllVolumes and AllContainers keep every item of their kind, the unsupported
// ones too. A selected container's image and named volumes are kept with it, as Run brings
// them along.
func (p *Plan) Select(sel Selection) (*Plan, error) {
	imgs, vols, _, err := resolve(p, sel)
	if err != nil {
		return nil, err
	}
	n := *p
	ctrs, _, err := resolveContainers(p, sel)
	if err != nil {
		return nil, err
	}
	n.Images, n.Volumes, n.Containers = nil, nil, nil
	for _, ip := range p.Images {
		if sel.AllImages || slices.ContainsFunc(imgs, func(x ImagePlan) bool { return x.ID == ip.ID }) {
			n.Images = append(n.Images, ip)
		}
	}
	for _, vp := range p.Volumes {
		if sel.AllVolumes || slices.ContainsFunc(vols, func(x VolumePlan) bool { return x.Name == vp.Name }) {
			n.Volumes = append(n.Volumes, vp)
		}
	}
	for _, cp := range p.Containers {
		if sel.AllContainers || slices.ContainsFunc(ctrs, func(x ContainerPlan) bool { return x.ID == cp.ID }) {
			n.Containers = append(n.Containers, cp)
		}
	}
	return &n, nil
}

func imageReason(ip ImagePlan) string {
	switch ip.Status {
	case StatusAlready:
		return "in hyperbutler with the same id and tags"
	case StatusPartial:
		return "in hyperbutler with the same id; adds the tags " + strings.Join(ip.MissingTags, ", ")
	case StatusConflict:
		return "hyperbutler has " + strings.Join(ip.Conflicts, ", ") + " as another image; remove or retag it there first"
	}
	return ""
}

// unsupportedVolume says why a volume is not copied, or "".
func unsupportedVolume(v apiVolume) string {
	if v.Driver != "local" {
		return "volume driver " + dockerapi.Clean(v.Driver, 64) + ": only local volumes are copied"
	}
	if len(v.Options) > 0 {
		return "a local volume with driver options (a bind mount or a network share): its data is not in Docker Desktop"
	}
	return ""
}

// ownLabel reports whether a volume label is one the import adds (the source, the run,
// a temporary volume's target).
func ownLabel(k string) bool {
	return k == source.HelperLabel || strings.HasPrefix(k, source.HelperLabel+".")
}

// withoutOwnLabels is labels without the import's own.
func withoutOwnLabels(labels map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range labels {
		if !ownLabel(k) {
			out[k] = v
		}
	}
	return out
}

// sameVolumeMeta compares a Docker Desktop volume's labels with a hyperbutler volume of
// the same name (ignoring the labels the import adds) and checks the hyperbutler one is
// a plain local volume. It says what differs.
func sameVolumeMeta(srcLabels map[string]string, tv *apiVolume) (bool, string) {
	switch {
	case tv.Driver != "" && tv.Driver != "local":
		return false, "the volume driver " + dockerapi.Clean(tv.Driver, 64)
	case len(tv.Options) > 0:
		return false, "driver options"
	}
	s, t := withoutOwnLabels(srcLabels), withoutOwnLabels(tv.Labels)
	if maps.Equal(s, t) {
		return true, ""
	}
	return false, "other labels (Docker Desktop's: " + labelText(s) + "; hyperbutler's: " + labelText(t) + ")"
}

func labelText(m map[string]string) string {
	if len(m) == 0 {
		return "none"
	}
	keys := slices.Sorted(maps.Keys(m))
	for i, k := range keys {
		keys[i] = dockerapi.Clean(k, 80) + "=" + dockerapi.Clean(m[k], 80)
	}
	return strings.Join(keys, ", ")
}

// recreatable reports whether a hyperbutler volume with no content may be removed and
// created again with Docker Desktop's labels: a plain local one without labels of
// docker compose (which would then take it for another project's).
func recreatable(tv *apiVolume) bool {
	if (tv.Driver != "" && tv.Driver != "local") || len(tv.Options) > 0 {
		return false
	}
	for k := range tv.Labels {
		if strings.HasPrefix(k, "com.docker.compose.") {
			return false
		}
	}
	return true
}

func (im *Importer) sourceVolumes(ctx context.Context) ([]apiVolume, error) {
	var vl struct {
		Volumes []apiVolume `json:"Volumes"`
	}
	if err := im.src.GetJSON(ctx, "/volumes", nil, &vl); err != nil {
		return nil, fmt.Errorf("list Docker Desktop's volumes: %w", err)
	}
	var out []apiVolume
	for _, v := range vl.Volumes {
		if dockerapi.ValidName("volume", v.Name) != nil {
			continue
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// volumeSizes asks the engine's disk use for the volumes' sizes (empty if it fails).
func (im *Importer) volumeSizes(ctx context.Context) map[string]int64 {
	var df struct {
		Volumes []struct {
			Name      string `json:"Name"`
			UsageData *struct {
				Size int64 `json:"Size"`
			} `json:"UsageData"`
		} `json:"Volumes"`
	}
	out := map[string]int64{}
	if im.src.GetJSON(ctx, "/system/df", url.Values{"type": {"volume"}}, &df) != nil {
		return out
	}
	for _, v := range df.Volumes {
		if v.UsageData != nil && v.UsageData.Size >= 0 {
			out[v.Name] = v.UsageData.Size
		}
	}
	return out
}

// liveUsers maps each volume to the running (or paused, or restarting) Docker Desktop
// containers that mount it.
func (im *Importer) liveUsers(ctx context.Context) (map[string][]string, error) {
	var list []apiContainer
	if err := im.src.GetJSON(ctx, "/containers/json", nil, &list); err != nil {
		return nil, fmt.Errorf("list Docker Desktop's running containers: %w", err)
	}
	out := map[string][]string{}
	for _, c := range list {
		// The list without all=1 holds running, paused and restarting containers: any
		// of them can write the volume (a paused one when it resumes).
		for _, m := range c.Mounts {
			if m.Type == "volume" && m.Name != "" {
				out[m.Name] = append(out[m.Name], c.name())
			}
		}
	}
	return out, nil
}

// dstUsers maps each hyperbutler volume to the containers there (in any state) that
// mount it, the import's own helpers left out.
func (im *Importer) dstUsers(ctx context.Context) (map[string][]string, error) {
	var list []apiContainer
	if err := im.dst.JSON(ctx, http.MethodGet, "/containers/json", url.Values{"all": {"1"}}, nil, &list); err != nil {
		return nil, fmt.Errorf("list hyperbutler's containers: %w", err)
	}
	out := map[string][]string{}
	for _, c := range list {
		if c.helper() {
			continue
		}
		for _, m := range c.Mounts {
			if m.Type == "volume" && m.Name != "" {
				out[m.Name] = append(out[m.Name], c.name())
			}
		}
	}
	return out, nil
}

// dstVolume is hyperbutler's volume of that name, nil when there is none.
func (im *Importer) dstVolume(ctx context.Context, name string) (*apiVolume, error) {
	var v apiVolume
	err := im.dst.JSON(ctx, http.MethodGet, "/volumes/"+name, nil, nil, &v)
	switch {
	case err == nil:
		return &v, nil
	case dockerapi.IsNotFound(err):
		return nil, nil
	}
	return nil, fmt.Errorf("hyperbutler's volume %s: %w", name, err)
}

func (im *Importer) completePath() string {
	if im.home == "" {
		return ""
	}
	return engine.Layout{Home: im.home}.ImportsCompletePath()
}
