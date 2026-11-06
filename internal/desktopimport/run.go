package desktopimport

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/Higangssh/hyperbutler/internal/desktopimport/source"
	"github.com/Higangssh/hyperbutler/internal/dockerapi"
)

// resolve turns a selection into the plan's items. A name that is not in the plan, or
// is ambiguous, is an error. Unsupported items picked by --all-* go to skipped. A selected
// container that will be created brings its image and named volumes along;
// resolveContainers has the containers themselves.
func resolve(p *Plan, sel Selection) (imgs []ImagePlan, vols []VolumePlan, skipped []ItemResult, err error) {
	seenI := map[string]bool{}
	addImage := func(ip ImagePlan) {
		if !seenI[ip.ID] {
			seenI[ip.ID] = true
			imgs = append(imgs, ip)
		}
	}
	if sel.AllImages {
		for _, ip := range p.Images {
			if ip.Status == StatusUnsupported {
				skipped = append(skipped, ItemResult{Kind: KindImage, Name: ip.Name(), ID: ip.ID, Size: ip.Size, Outcome: OutcomeRefused, Reason: ip.Reason})
				continue
			}
			addImage(ip)
		}
	}
	for _, name := range sel.Images {
		ip, err := findImage(p, name)
		if err != nil {
			return nil, nil, nil, err
		}
		addImage(ip)
	}
	seenV := map[string]bool{}
	addVolume := func(vp VolumePlan) {
		if !seenV[vp.Name] {
			seenV[vp.Name] = true
			vols = append(vols, vp)
		}
	}
	if sel.AllVolumes {
		for _, vp := range p.Volumes {
			if vp.Status == StatusUnsupported {
				skipped = append(skipped, ItemResult{Kind: KindVolume, Name: vp.Name, Size: vp.Size, Outcome: OutcomeRefused, Reason: vp.Reason})
				continue
			}
			addVolume(vp)
		}
	}
	for _, name := range sel.Volumes {
		if err := dockerapi.ValidName("volume", name); err != nil {
			return nil, nil, nil, err
		}
		i := slices.IndexFunc(p.Volumes, func(v VolumePlan) bool { return v.Name == name })
		if i < 0 {
			return nil, nil, nil, fmt.Errorf("volume %q is not in Docker Desktop", name)
		}
		addVolume(p.Volumes[i])
	}
	ctrs, _, err := resolveContainers(p, sel)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, cp := range ctrs {
		if !cp.creatable() {
			continue
		}
		if i := slices.IndexFunc(p.Images, func(ip ImagePlan) bool { return ip.ID == cp.ImageID }); i >= 0 && p.Images[i].Status != StatusUnsupported {
			addImage(p.Images[i])
		}
		for _, name := range cp.Volumes {
			if i := slices.IndexFunc(p.Volumes, func(v VolumePlan) bool { return v.Name == name }); i >= 0 {
				addVolume(p.Volumes[i])
			}
		}
	}
	return imgs, vols, skipped, nil
}

// findImage finds a selected image by tag, id or id prefix (12 or more hex digits).
func findImage(p *Plan, name string) (ImagePlan, error) {
	if err := dockerapi.ValidImageRef(name); err != nil {
		return ImagePlan{}, err
	}
	ref := normalizeRef(name)
	hex := strings.TrimPrefix(name, "sha256:")
	var found []ImagePlan
	for _, ip := range p.Images {
		id := strings.TrimPrefix(ip.ID, "sha256:")
		if slices.Contains(ip.Tags, ref) || slices.Contains(ip.Tags, name) || id == hex ||
			(len(hex) >= 12 && isHex(hex) && strings.HasPrefix(id, hex)) {
			found = append(found, ip)
		}
	}
	switch len(found) {
	case 0:
		return ImagePlan{}, fmt.Errorf("image %q is not in Docker Desktop (or has no tag there)", name)
	case 1:
		return found[0], nil
	}
	return ImagePlan{}, fmt.Errorf("image %q names %d images in Docker Desktop; give a tag or a longer id", name, len(found))
}

func isHex(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// runState is what one Run created for itself in hyperbutler.
type runState struct {
	sel      Selection
	plan     *Plan
	run      *runToken
	complete *completeFile
	// dstHelperImage is the image hyperbutler's helper containers are created from;
	// tempTags are the tags of a copy of Docker Desktop's helper image made for that
	// (hyperbutler had no image), removed at the end.
	dstHelperImage string
	tempTags       []string
	tempID         string
	tempErr        error
	// newNetworks are the networks this run created in hyperbutler, true once a container
	// was created on it; the others are removed at the end.
	newNetworks map[string]bool
}

// Run imports the selected items of plan into hyperbutler, then checks that Docker
// Desktop is unchanged (Result.Source). It returns an error only when nothing was done:
// an unknown name in the selection, another import running in the installation
// (ErrImportRunning), a copy that does not fit (ErrNoSpace, unless
// Selection.ForceSpace), Docker Desktop unreadable before the import. Per-item problems
// are in Result.Items. When ctx is canceled the item in progress is undone in
// hyperbutler, the rest are OutcomeCanceled, and the check of Docker Desktop still runs
// (on a context that is not canceled).
//
// First it removes what an interrupted import left behind (findStale's rules).
func (im *Importer) Run(ctx context.Context, plan *Plan, sel Selection, progress func(Event)) (*Result, error) {
	if progress == nil {
		progress = func(Event) {}
	}
	imgs, vols, skipped, err := resolve(plan, sel)
	if err != nil {
		return nil, err
	}
	unlock, err := lockInstall(im.home)
	if err != nil {
		return nil, err
	}
	defer unlock()
	ctrs, skippedCtrs, err := resolveContainers(plan, sel)
	if err != nil {
		return nil, err
	}
	res := &Result{Skipped: append(skipped, skippedCtrs...)}
	space, err := im.Space(plan, sel)
	if err != nil {
		return nil, err
	}
	if err := space.Check(); err != nil {
		if !sel.ForceSpace {
			return nil, err
		}
		progress(Event{Kind: EventNote, Message: err.Error() + " (going on: --force-space)"})
	}
	if space.Unknown > 0 {
		progress(Event{Kind: EventNote, Message: "Docker Desktop gave no size for " + countWord(space.Unknown, "volume") + ", which the space check does not count"})
	}
	run, err := newRun()
	if err != nil {
		return nil, err
	}
	defer run.done()
	complete, err := loadComplete(im.completePath())
	if err != nil {
		return nil, err
	}
	st := &runState{sel: sel, plan: plan, run: run, complete: complete, newNetworks: map[string]bool{}}

	im.applyStale(ctx, st, im.findStale(ctx, complete), progress)

	// The volumes this run reads in Docker Desktop are hashed before and after: every
	// one it does not refuse now (a volume that is no longer in use since the plan is
	// copied).
	users, err := im.liveUsers(ctx)
	if err != nil {
		return nil, fmt.Errorf("record Docker Desktop's state before the import: %w", err)
	}
	var hashed []VolumePlan
	live := map[string]bool{}
	for _, vp := range vols {
		if vp.Status == StatusUnsupported || plan.HelperImage == "" || (len(users[vp.Name]) > 0 && !sel.AllowLiveVolumes) {
			continue
		}
		hashed = append(hashed, vp)
		if len(users[vp.Name]) > 0 {
			live[vp.Name] = true
		}
	}
	before, err := im.snapshot(ctx, run.id, plan.HelperImage, hashed, progress)
	if err != nil {
		return nil, fmt.Errorf("record Docker Desktop's state before the import: %w", err)
	}

	announceContainers(ctrs, progress)
	for _, ip := range imgs {
		if ctx.Err() != nil {
			res.add(ItemResult{Kind: KindImage, Name: ip.Name(), ID: ip.ID, Tags: ip.Tags, Size: ip.Size, Outcome: OutcomeCanceled}, progress)
			continue
		}
		progress(Event{Kind: EventStart, Item: KindImage, Name: ip.Name(), Total: ip.Size})
		res.add(im.importImage(ctx, ip, progress), progress)
	}
	for _, vp := range vols {
		if ctx.Err() != nil {
			res.add(ItemResult{Kind: KindVolume, Name: vp.Name, Size: vp.Size, Outcome: OutcomeCanceled}, progress)
			continue
		}
		progress(Event{Kind: EventStart, Item: KindVolume, Name: vp.Name, Total: max(vp.Size, 0)})
		r, inUse := im.importVolume(ctx, st, vp, before.hashes[vp.Name], before.hashErr[vp.Name], progress)
		if inUse {
			live[vp.Name] = true
		}
		if inUse && (r.Outcome == OutcomeImported || r.Outcome == OutcomeAlready) {
			r.Reason = strings.TrimPrefix(r.Reason+"; ", "; ") + "copied while running containers mount it in Docker Desktop: the copy may be inconsistent"
		}
		res.add(r, progress)
	}
	for _, cp := range ctrs {
		if ctx.Err() != nil {
			res.add(ItemResult{Kind: KindContainer, Name: cp.Name, ID: cp.ID, Outcome: OutcomeCanceled}, progress)
			continue
		}
		progress(Event{Kind: EventStart, Item: KindContainer, Name: cp.Name})
		res.add(im.importContainer(ctx, st, cp, res), progress)
	}
	im.removeUnusedNetworks(ctx, st)
	im.removeTempImage(ctx, st)
	res.Canceled = ctx.Err() != nil

	after, err := im.snapshot(context.WithoutCancel(ctx), run.id, plan.HelperImage, hashed, progress)
	if err != nil {
		res.Source = SourceCheck{Differences: []string{"could not check Docker Desktop after the import: " + err.Error()}}
	} else {
		res.Source = compareSnapshots(before, after, live, run.id)
	}
	im.mu.Lock()
	res.Notes = append(res.Notes, im.notes...)
	im.notes = nil
	im.mu.Unlock()
	return res, nil
}

func (r *Result) add(it ItemResult, progress func(Event)) {
	r.Items = append(r.Items, it)
	r.Bytes += it.Bytes
	progress(Event{Kind: EventDone, Item: it.Kind, Name: it.Name, Bytes: it.Bytes, Outcome: it.Outcome, Message: it.Reason})
}

// failed is an item's failure: canceled when ctx was, else failed with err.
func failed(ctx context.Context, r ItemResult, err error) ItemResult {
	if ctx.Err() != nil {
		r.Outcome, r.Reason = OutcomeCanceled, "canceled; nothing of it is left in hyperbutler"
		return r
	}
	r.Outcome = OutcomeFailed
	r.Reason = err.Error() + "; nothing of it is left in hyperbutler"
	return r
}

// --- images -------------------------------------------------------------------------------

func (im *Importer) importImage(ctx context.Context, ip ImagePlan, progress func(Event)) ItemResult {
	r := ItemResult{Kind: KindImage, Name: ip.Name(), ID: ip.ID, Tags: ip.Tags, Size: ip.Size}
	if ip.Status == StatusUnsupported {
		r.Outcome, r.Reason = OutcomeRefused, ip.Reason
		return r
	}
	si, err := im.inspectSource(ctx, ip.ID)
	if err != nil {
		return failed(ctx, r, fmt.Errorf("Docker Desktop's image: %w", err)) //nolint:staticcheck // ST1005: a product name
	}
	tags := realTags(si.RepoTags)
	r.Tags = tags
	d, err := im.loadDstImages(ctx)
	if err != nil {
		return failed(ctx, r, err)
	}
	status, missing, conflicts, tid, err := im.imageStatus(ctx, si, tags, d)
	if err != nil {
		return failed(ctx, r, err)
	}
	switch status {
	case StatusUnsupported:
		r.Outcome, r.Reason = OutcomeRefused, "untagged: give it a tag in Docker Desktop to import it"
		return r
	case StatusAlready:
		r.Outcome, r.Reason = OutcomeAlready, "in hyperbutler with the same id and tags"
		return r
	case StatusConflict:
		r.Outcome, r.Reason = OutcomeRefused, imageReason(ImagePlan{Status: status, Conflicts: conflicts})
		return r
	case StatusPartial:
		var added []string
		for _, t := range missing {
			repo, tag := splitTag(t)
			err := im.dst.JSON(ctx, http.MethodPost, "/images/"+tid+"/tag", url.Values{"repo": {repo}, "tag": {tag}}, nil, nil)
			if err == nil {
				added = append(added, t)
				var same bool
				if same, _, err = im.sameImage(ctx, si, t); err == nil && !same {
					err = fmt.Errorf("verification failed: %s in hyperbutler is not the image", t)
				}
			}
			if err != nil {
				im.untag(ctx, added)
				return failed(ctx, r, fmt.Errorf("tag %s: %w", t, err))
			}
		}
		r.Outcome, r.AddedTags = OutcomeTagged, added
		r.Reason = "the image was in hyperbutler; added the tags " + strings.Join(added, ", ")
		return r
	}
	n, err := im.loadImage(ctx, si, tags, d, ip.Size, KindImage, ip.Name(), progress)
	r.Bytes = n
	if err != nil {
		return failed(ctx, r, err)
	}
	r.Outcome = OutcomeImported
	return r
}

// loadImage streams the image's export from Docker Desktop into hyperbutler's image
// load API and verifies every tag names the image there. On any failure it removes what
// the load added to hyperbutler (the tags that were not there before, and the image if
// it was not), so nothing half-imported stays tagged. Layers an aborted load had
// already unpacked, and that no image refers to, stay in dockerd's store until its own
// garbage collection removes them.
func (im *Importer) loadImage(ctx context.Context, si *apiImageInspect, tags []string, before dstImages, total int64, kind, name string, progress func(Event)) (int64, error) {
	resp, err := im.src.Get(ctx, "/images/get", url.Values{"names": tags})
	if err != nil {
		return 0, fmt.Errorf("export from Docker Desktop: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	c := &counter{r: resp.Body, report: progressFn(progress, kind, name, PhaseCopy, total)}
	lr, err := im.dst.Do(ctx, http.MethodPost, "/images/load", url.Values{"quiet": {"1"}}, c, "application/x-tar")
	if err == nil {
		_, err = parseLoadAnswer(lr.Body)
		_ = lr.Body.Close()
	}
	if err == nil {
		for _, t := range tags {
			same, got, verr := im.sameImage(ctx, si, t)
			if verr != nil {
				err = fmt.Errorf("verify %s in hyperbutler: %w", t, verr)
				break
			}
			if !same {
				err = fmt.Errorf("verification failed: %s in hyperbutler is %s, not %s", t, shortID(got), shortID(si.ID))
				break
			}
		}
	}
	if err != nil {
		var added []string
		for _, t := range tags {
			if _, ok := before.tags[t]; !ok {
				added = append(added, t)
			}
		}
		im.untag(ctx, added)
		if !before.ids[si.ID] {
			cctx, cancel := cleanupCtx(ctx)
			derr := im.dst.JSON(cctx, http.MethodDelete, "/images/"+si.ID, nil, nil, nil)
			cancel()
			if derr != nil && !dockerapi.IsNotFound(derr) && dockerapi.StatusOf(derr) != http.StatusConflict {
				im.note("could not remove the incomplete image %s from hyperbutler (%v)", shortID(si.ID), derr)
			}
		}
		return c.n, err
	}
	return c.n, nil
}

// untag removes tags from hyperbutler (an image whose last tag goes is removed with
// it). It is used only for tags this import added.
func (im *Importer) untag(ctx context.Context, tags []string) {
	cctx, cancel := cleanupCtx(ctx)
	defer cancel()
	for _, t := range tags {
		err := im.dst.JSON(cctx, http.MethodDelete, "/images/"+t, nil, nil, nil)
		if err != nil && !dockerapi.IsNotFound(err) {
			im.note("could not remove the tag %s this import added to hyperbutler (%v)", t, err)
		}
	}
}

// --- volumes ------------------------------------------------------------------------------

// targetHelper returns an image in hyperbutler to create helper containers from: the
// smallest one without VOLUME for its platform. When hyperbutler has none, it copies
// Docker Desktop's helper image for the time of the import (removed at the end).
func (im *Importer) targetHelper(ctx context.Context, st *runState, progress func(Event)) (string, error) {
	if st.dstHelperImage != "" {
		return st.dstHelperImage, nil
	}
	if st.tempErr != nil {
		return "", st.tempErr
	}
	d, err := im.loadDstImages(ctx)
	if err != nil {
		return "", err
	}
	img, err := im.dstHelperImage(ctx, d)
	if err != nil {
		return "", err
	}
	if img != "" {
		st.dstHelperImage = img
		return img, nil
	}
	si, err := im.inspectSource(ctx, st.plan.HelperImage)
	switch {
	case err != nil:
	case len(realTags(si.RepoTags)) == 0:
		err = errors.New("it has no tag")
	case !si.helperOK(im.dstInfo):
		err = fmt.Errorf("it is for %s/%s", dockerapi.Clean(si.Os, 20), dockerapi.Clean(si.Architecture, 20))
	}
	if err == nil {
		tags := realTags(si.RepoTags)
		progress(Event{Kind: EventNote, Message: "hyperbutler has no image for the helper container: copying " + tags[0] + " from Docker Desktop for the time of the import"})
		if _, err = im.loadImage(ctx, si, tags, d, 0, KindImage, tags[0], progress); err == nil {
			st.tempTags, st.tempID = tags, si.ID
			st.dstHelperImage = si.ID
			return si.ID, nil
		}
	}
	st.tempErr = fmt.Errorf("hyperbutler has no image without VOLUME to create the helper container from, and Docker Desktop's %s could not be copied for it (%w); import a small image too, such as busybox", shortID(st.plan.HelperImage), err)
	return "", st.tempErr
}

func (im *Importer) removeTempImage(ctx context.Context, st *runState) {
	if len(st.tempTags) > 0 {
		im.untag(ctx, st.tempTags)
	}
}

// importVolume imports one volume. inUse says running Docker Desktop containers mount
// it now (it is refused, or copied with AllowLiveVolumes).
func (im *Importer) importVolume(ctx context.Context, st *runState, vp VolumePlan, before string, beforeErr error, progress func(Event)) (r ItemResult, inUse bool) {
	r = ItemResult{Kind: KindVolume, Name: vp.Name, Size: vp.Size}
	switch {
	case vp.Status == StatusUnsupported:
		r.Outcome, r.Reason = OutcomeRefused, vp.Reason
		return r, false
	case st.plan.HelperImage == "":
		r.Outcome, r.Reason = OutcomeRefused, noHelperNote
		return r, false
	}
	users, err := im.liveUsers(ctx)
	if err != nil {
		return failed(ctx, r, err), false
	}
	u := users[vp.Name]
	if len(u) > 0 && !st.sel.AllowLiveVolumes {
		r.Outcome = OutcomeRefused
		r.Reason = "mounted by running containers in Docker Desktop (" + strings.Join(u, ", ") + "): stop them in Docker Desktop first, so the copy is consistent (--allow-live-volumes copies anyway)"
		return r, true
	}
	live := len(u) > 0
	switch {
	case beforeErr != nil:
		return failed(ctx, r, fmt.Errorf("could not hash it before the import: %w", beforeErr)), live
	case before == "":
		return failed(ctx, r, errors.New("no content hash was recorded before the import (running containers mounted it then); import it again")), live
	}
	img, err := im.targetHelper(ctx, st, progress)
	if err != nil {
		return failed(ctx, r, err), live
	}
	tv, err := im.dstVolume(ctx, vp.Name)
	if err != nil {
		return failed(ctx, r, err), live
	}
	if tv == nil {
		return im.copyNew(ctx, st, img, vp, before, live, "", progress), live
	}
	th, err := im.hashTarget(ctx, st.run.id, img, vp.Name, PhaseCompare, progress)
	if err != nil {
		return failed(ctx, r, err), live
	}
	sameContent := th.Sum() == before
	sameMeta, why := sameVolumeMeta(vp.labels, tv)
	if sameContent && sameMeta {
		// Verified now: an earlier import's copy whose record was lost is recorded.
		if run := tv.Labels[source.RunLabel]; tv.Labels[importLabel] != "" && source.ValidRunID(run) && !im.runLive(run) {
			if err := st.complete.set(vp.Name, run); err != nil {
				im.note("%v", err)
			}
		}
		r.Outcome, r.Reason, r.Hash = OutcomeAlready, "in hyperbutler with the same content and labels", before
		return r, live
	}
	var dusers map[string][]string
	if dusers, err = im.dstUsers(ctx); err != nil {
		return failed(ctx, r, err), live
	}
	used := dusers[vp.Name]
	if sameContent && th.empty() && recreatable(tv) && len(used) == 0 {
		// An empty, unused volume without compose labels: created again with Docker
		// Desktop's labels, nothing is lost.
		if err := im.deleteDstVolume(ctx, vp.Name); err != nil {
			return failed(ctx, r, fmt.Errorf("create the empty volume again with Docker Desktop's labels: %w", err)), live
		}
		return im.copyNew(ctx, st, img, vp, before, live, "the empty volume hyperbutler had was created again with Docker Desktop's labels", progress), live
	}
	inUseNote := ""
	if len(used) > 0 {
		inUseNote = " (containers in hyperbutler use it: " + strings.Join(used, ", ") + ")"
	}
	if !st.sel.OverwriteVolumes {
		r.Outcome = OutcomeRefused
		if sameContent {
			r.Reason = "a volume of this name in hyperbutler has the same content but " + why + inUseNote + "; --overwrite-volumes replaces it with Docker Desktop's"
		} else {
			r.Reason = "a volume of this name in hyperbutler has different content" + inUseNote + "; --overwrite-volumes replaces it"
		}
		return r, live
	}
	if len(used) > 0 {
		r.Outcome = OutcomeRefused
		r.Reason = "containers in hyperbutler use its volume of this name (" + strings.Join(used, ", ") + "): remove them there first to replace it; hyperbutler's volume is unchanged"
		return r, live
	}
	return im.overwriteVolume(ctx, st, img, vp, before, live, progress), live
}

// newVolumeLabels are the labels of a volume the import creates: Docker Desktop's (so
// docker compose takes it as its own), the source and the run.
func newVolumeLabels(vp VolumePlan, run string) map[string]string {
	labels := maps.Clone(vp.labels)
	if labels == nil {
		labels = map[string]string{}
	}
	labels[importLabel] = "docker-desktop"
	labels[source.RunLabel] = run
	return labels
}

func (im *Importer) createDstVolume(ctx context.Context, name string, labels map[string]string) error {
	body := map[string]any{"Name": name, "Driver": "local", "Labels": labels}
	if err := im.dst.JSON(ctx, http.MethodPost, "/volumes/create", nil, body, nil); err != nil {
		return fmt.Errorf("create the volume %s in hyperbutler: %w", name, err)
	}
	return nil
}

func (im *Importer) deleteDstVolume(ctx context.Context, name string) error {
	cctx, cancel := context.WithTimeout(ctx, cleanupTimeout)
	defer cancel()
	return im.dst.JSON(cctx, http.MethodDelete, "/volumes/"+name, nil, nil, nil)
}

// copyNew creates hyperbutler's volume of vp's name, fills it from Docker Desktop and
// verifies it, then records it in imports-complete.json. A failure removes it.
func (im *Importer) copyNew(ctx context.Context, st *runState, img string, vp VolumePlan, before string, live bool, reason string, progress func(Event)) ItemResult {
	r := ItemResult{Kind: KindVolume, Name: vp.Name, Size: vp.Size}
	if err := im.createDstVolume(ctx, vp.Name, newVolumeLabels(vp, st.run.id)); err != nil {
		return failed(ctx, r, err)
	}
	hash, n, err := im.fillVolume(ctx, st, img, vp, vp.Name, before, live, progress)
	r.Bytes = n
	if err != nil {
		im.removeDstVolume(ctx, vp.Name)
		return failed(ctx, r, err)
	}
	if err := st.complete.set(vp.Name, st.run.id); err != nil {
		im.note("%v", err)
	}
	r.Outcome, r.Hash, r.Reason = OutcomeImported, hash, reason
	return r
}

// overwriteVolume replaces hyperbutler's volume of vp's name (--overwrite-volumes)
// without a moment where neither its old content nor Docker Desktop's verified one is
// in hyperbutler: Docker Desktop's is copied into a temporary volume and verified; only
// then is the old volume deleted, the new one created with Docker Desktop's labels,
// filled from the temporary one inside hyperbutler and verified again, and the
// temporary one removed. A failure before the delete leaves the old volume as it was;
// one after it keeps the temporary volume (findStale keeps it too) and says so.
func (im *Importer) overwriteVolume(ctx context.Context, st *runState, img string, vp VolumePlan, before string, live bool, progress func(Event)) ItemResult {
	r := ItemResult{Kind: KindVolume, Name: vp.Name, Size: vp.Size}
	tmp := tempVolumeName(st.run.id, vp.Name)
	labels := map[string]string{importLabel: "docker-desktop", source.RunLabel: st.run.id, tempForLabel: vp.Name}
	unchanged := func(err error) ItemResult {
		r = failed(ctx, r, err)
		r.Reason = strings.TrimSuffix(r.Reason, "; nothing of it is left in hyperbutler") + "; hyperbutler's volume is unchanged"
		return r
	}
	if err := im.createDstVolume(ctx, tmp, labels); err != nil {
		return unchanged(err)
	}
	hash, n, err := im.fillVolume(ctx, st, img, vp, tmp, before, live, progress)
	r.Bytes = n
	if err != nil {
		im.removeDstVolume(ctx, tmp)
		return unchanged(err)
	}
	if err := st.complete.set(tmp, st.run.id); err != nil {
		im.note("%v", err)
	}
	dropTemp := func() {
		if im.removeDstVolume(ctx, tmp) {
			if err := st.complete.set(tmp, ""); err != nil {
				im.note("%v", err)
			}
		}
	}
	// Docker Desktop's content is verified in hyperbutler: only now does the old go.
	if ctx.Err() != nil {
		dropTemp()
		return unchanged(ctx.Err())
	}
	if err := im.deleteDstVolume(ctx, vp.Name); err != nil {
		dropTemp()
		return unchanged(fmt.Errorf("replace hyperbutler's volume (is a container there using it?): %w", err))
	}
	err = im.createDstVolume(ctx, vp.Name, newVolumeLabels(vp, st.run.id))
	if err == nil {
		err = im.refill(ctx, st, img, tmp, vp.Name, hash, vp.Size, progress)
		if err != nil {
			im.removeDstVolume(ctx, vp.Name)
		}
	}
	if err != nil {
		r.Outcome = OutcomeFailed
		if ctx.Err() != nil {
			r.Outcome, err = OutcomeCanceled, errors.New("canceled")
		}
		r.Reason = fmt.Sprintf("%v; hyperbutler's old volume %s was already deleted: Docker Desktop's content, verified, is in hyperbutler's volume %s. Import %s again (a later import then removes %s), or copy it from there",
			err, vp.Name, tmp, vp.Name, tmp)
		return r
	}
	if err := st.complete.set(vp.Name, st.run.id); err != nil {
		im.note("%v", err)
	}
	dropTemp()
	r.Outcome, r.Hash, r.Reason = OutcomeImported, hash, "replaced hyperbutler's volume of this name (--overwrite-volumes)"
	return r
}
