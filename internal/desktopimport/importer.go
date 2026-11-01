package desktopimport

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/Higangssh/hyperbutler/internal/desktopimport/source"
	"github.com/Higangssh/hyperbutler/internal/dockerapi"
)

// Importer holds the two engines: Docker Desktop's, reached only through the read-only
// source client, and hyperbutler's.
type Importer struct {
	src    *source.Client
	dst    *dockerapi.Client
	ep     Endpoint
	target string
	// mixed: one engine keeps images in containerd's store and the other does not, so
	// the same image has different ids on the two.
	mixed bool

	mu    sync.Mutex
	notes []string // problems found while cleaning up
}

// Open discovers Docker Desktop (Discover) and checks that hyperbutler's engine on
// o.TargetPipe answers and is another engine. Close releases it.
func Open(ctx context.Context, o Options) (*Importer, error) {
	if o.TargetPipe == "" {
		return nil, errors.New("desktopimport: no target pipe")
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
	}
	pctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	var si, di engineInfo
	if err := im.src.GetJSON(pctx, "/info", nil, &si); err != nil {
		im.Close()
		return nil, fmt.Errorf("Docker Desktop's engine: %w", err) //nolint:staticcheck // ST1005: a product name
	}
	if err := im.dst.JSON(pctx, http.MethodGet, "/info", nil, nil, &di); err != nil {
		im.Close()
		if dockerapi.StatusOf(err) == 0 {
			return nil, fmt.Errorf("%w (nothing answers on %s)", ErrTargetNotRunning, o.TargetPipe)
		}
		return nil, fmt.Errorf("hyperbutler's engine: %w", err)
	}
	if si.ID != "" && si.ID == di.ID {
		im.Close()
		return nil, ErrSameEngine
	}
	im.mixed = si.containerdStore() != di.containerdStore()
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

type apiImageInspect struct {
	ID           string   `json:"Id"`
	RepoTags     []string `json:"RepoTags"`
	Created      string   `json:"Created"`
	Architecture string   `json:"Architecture"`
	Os           string   `json:"Os"`
	RootFS       struct {
		Layers []string `json:"Layers"`
	} `json:"RootFS"`
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
	// smallest is the smallest image's id, for the helper container ("" for none).
	smallest string
}

func (im *Importer) loadDstImages(ctx context.Context) (dstImages, error) {
	var list []apiImage
	if err := im.dst.JSON(ctx, http.MethodGet, "/images/json", nil, nil, &list); err != nil {
		return dstImages{}, fmt.Errorf("list hyperbutler's images: %w", err)
	}
	d := dstImages{ids: map[string]bool{}, tags: map[string]string{}}
	var small int64 = -1
	for _, i := range list {
		if dockerapi.ValidImageRef(i.ID) != nil {
			continue
		}
		d.ids[i.ID] = true
		for _, t := range realTags(i.RepoTags) {
			d.tags[t] = i.ID
		}
		if small < 0 || i.Size < small {
			small, d.smallest = i.Size, i.ID
		}
	}
	return d, nil
}

// sameImage reports whether an image in hyperbutler (inspected by ref) is the source
// image: the same id, or, when the two engines use different image stores, the same
// layers, creation time and platform.
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
		d.Architecture == src.Architecture && d.Os == src.Os
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
	// Progress, when set, gets EventProgress while volumes are compared.
	Progress func(Event)
}

// Plan lists Docker Desktop's images and volumes and compares each with hyperbutler.
// It only reads (and, with CompareVolumes, creates helper containers, never started,
// and removes them).
func (im *Importer) Plan(ctx context.Context, po PlanOptions) (*Plan, error) {
	p := &Plan{Source: im.ep, Target: im.target}
	if im.ep.Note != "" {
		p.Notes = append(p.Notes, im.ep.Note)
	}
	// Helpers (and half-filled volumes in hyperbutler) a killed import left go first, so
	// the plan shows the state they hid.
	im.removeStaleHelpers(ctx, func(e Event) {
		if e.Kind == EventNote {
			p.Notes = append(p.Notes, e.Message)
		}
	})
	defer func() {
		im.mu.Lock()
		p.Notes = append(p.Notes, im.notes...)
		im.notes = nil
		im.mu.Unlock()
	}()
	var list []apiImage
	if err := im.src.GetJSON(ctx, "/images/json", nil, &list); err != nil {
		return nil, fmt.Errorf("list Docker Desktop's images: %w", err)
	}
	d, err := im.loadDstImages(ctx)
	if err != nil {
		return nil, err
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Size < list[j].Size })
	helperTagged := false
	for _, i := range list {
		if dockerapi.ValidImageRef(i.ID) != nil {
			p.Notes = append(p.Notes, fmt.Sprintf("skipped an image with an unexpected id %q", dockerapi.Clean(i.ID, 80)))
			continue
		}
		ip := ImagePlan{ID: i.ID, Tags: realTags(i.RepoTags), Size: i.Size}
		if p.HelperImage == "" || (len(realTags(i.RepoTags)) > 0 && !helperTagged) {
			// The smallest image, preferring one with a tag (which can be copied to
			// hyperbutler for its own helper when hyperbutler has no image).
			p.HelperImage, helperTagged = i.ID, len(realTags(i.RepoTags)) > 0
		}
		if len(ip.Tags) == 0 {
			ip.Status, ip.Reason = StatusUnsupported, "untagged: give it a tag in Docker Desktop to import it"
		} else {
			si, err := im.inspectSource(ctx, i.ID)
			if err != nil {
				return nil, fmt.Errorf("inspect Docker Desktop's image %s: %w", shortID(i.ID), err)
			}
			ip.Status, ip.MissingTags, ip.Conflicts, _, err = im.imageStatus(ctx, si, ip.Tags, d)
			if err != nil {
				return nil, err
			}
			ip.Reason = imageReason(ip)
		}
		p.Images = append(p.Images, ip)
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
		exists, err := im.dstVolumeExists(ctx, v.Name)
		if err != nil {
			return nil, err
		}
		switch {
		case !exists:
			vp.Status = StatusNew
		case !po.CompareVolumes:
			vp.Status, vp.Reason = StatusExists, "a volume of this name is in hyperbutler; its content is compared when it is imported"
		case p.HelperImage == "" || d.smallest == "":
			vp.Status, vp.Reason = StatusExists, "a volume of this name is in hyperbutler; there is no image on both sides to compare the content with"
		default:
			sh, err := im.hashSource(ctx, p.HelperImage, v.Name, vp.Size, PhaseCompare, po.Progress)
			if err != nil {
				return nil, err
			}
			th, err := im.hashTarget(ctx, d.smallest, v.Name, PhaseCompare, po.Progress)
			if err != nil {
				return nil, err
			}
			vp.SourceHash, vp.TargetHash = sh.Sum(), th.Sum()
			if vp.SourceHash == vp.TargetHash {
				vp.Status, vp.Reason = StatusAlready, "in hyperbutler with the same content"
			} else {
				vp.Status, vp.Reason = StatusConflict, "a volume of this name in hyperbutler has different content"
			}
		}
		p.Volumes = append(p.Volumes, vp)
	}
	if p.HelperImage == "" && len(p.Volumes) > 0 {
		p.Notes = append(p.Notes, "Docker Desktop has no image to create the volume helper container from, so volumes cannot be copied; pull a small image there (docker --context desktop-linux pull busybox) or copy them by hand (docs/tools.md)")
	}
	return p, nil
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

func (im *Importer) dstVolumeExists(ctx context.Context, name string) (bool, error) {
	err := im.dst.JSON(ctx, http.MethodGet, "/volumes/"+name, nil, nil, nil)
	switch {
	case err == nil:
		return true, nil
	case dockerapi.IsNotFound(err):
		return false, nil
	}
	return false, fmt.Errorf("hyperbutler's volume %s: %w", name, err)
}
