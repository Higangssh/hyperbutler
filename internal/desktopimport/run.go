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
// is ambiguous, is an error. Unsupported items picked by --all-* go to skipped.
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
	sel  Selection
	plan *Plan
	// dstHelperImage is the image hyperbutler's helper containers are created from;
	// tempTags are the tags of a copy of Docker Desktop's helper image made for that
	// (hyperbutler had no image), removed at the end.
	dstHelperImage string
	tempTags       []string
	tempID         string
	tempErr        error
}

// Run imports the selected items of plan into hyperbutler, then checks that Docker
// Desktop is unchanged (Result.Source). It returns an error only when nothing could be
// done (an unknown name in the selection, Docker Desktop unreadable before the
// import); per-item problems are in Result.Items. When ctx is canceled the item in
// progress is undone in hyperbutler, the rest are OutcomeCanceled, and the check of
// Docker Desktop still runs (on a context that is not canceled).
func (im *Importer) Run(ctx context.Context, plan *Plan, sel Selection, progress func(Event)) (*Result, error) {
	if progress == nil {
		progress = func(Event) {}
	}
	imgs, vols, skipped, err := resolve(plan, sel)
	if err != nil {
		return nil, err
	}
	res := &Result{Skipped: skipped}
	st := &runState{sel: sel, plan: plan}

	im.removeStaleHelpers(ctx, progress)

	// The volumes this run reads in Docker Desktop are hashed before and after.
	var hashed []VolumePlan
	live := map[string]bool{}
	for _, vp := range vols {
		if vp.Status == StatusUnsupported || plan.HelperImage == "" || (len(vp.UsedBy) > 0 && !sel.AllowLiveVolumes) {
			continue
		}
		hashed = append(hashed, vp)
		if len(vp.UsedBy) > 0 {
			live[vp.Name] = true
		}
	}
	before, err := im.snapshot(ctx, plan.HelperImage, hashed, progress)
	if err != nil {
		return nil, fmt.Errorf("record Docker Desktop's state before the import: %w", err)
	}

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
		r := im.importVolume(ctx, st, vp, before.hashes[vp.Name], progress)
		if live[vp.Name] && (r.Outcome == OutcomeImported || r.Outcome == OutcomeAlready) {
			r.Reason = strings.TrimPrefix(r.Reason+"; ", "; ") + "copied while running containers mount it in Docker Desktop (" + strings.Join(vp.UsedBy, ", ") + "): the copy may be inconsistent"
		}
		res.add(r, progress)
	}
	im.removeTempImage(ctx, st)
	res.Canceled = ctx.Err() != nil

	after, err := im.snapshot(context.WithoutCancel(ctx), plan.HelperImage, hashed, progress)
	if err != nil {
		res.Source = SourceCheck{Differences: []string{"could not check Docker Desktop after the import: " + err.Error()}}
	} else {
		res.Source = compareSnapshots(before, after, live)
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
// it was not), so nothing half-imported stays tagged.
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

// targetHelper returns an image in hyperbutler to create helper containers from. When
// hyperbutler has none, it copies Docker Desktop's helper image for the time of the
// import (removed at the end).
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
	if d.smallest != "" {
		st.dstHelperImage = d.smallest
		return d.smallest, nil
	}
	si, err := im.inspectSource(ctx, st.plan.HelperImage)
	if err == nil && len(realTags(si.RepoTags)) == 0 {
		err = errors.New("it has no tag")
	}
	if err == nil {
		tags := realTags(si.RepoTags)
		progress(Event{Kind: EventNote, Message: "hyperbutler has no image for the helper container: copying " + tags[0] + " from Docker Desktop for the time of the import"})
		if _, err = im.loadImage(ctx, si, tags, d, 0, KindImage, tags[0], progress); err == nil {
			st.tempTags, st.tempID = tags, si.ID
			st.dstHelperImage = tags[0]
			return tags[0], nil
		}
	}
	st.tempErr = fmt.Errorf("hyperbutler has no image to create the helper container from, and Docker Desktop's %s could not be copied for it (%w); import an image too, such as busybox", shortID(st.plan.HelperImage), err)
	return "", st.tempErr
}

func (im *Importer) removeTempImage(ctx context.Context, st *runState) {
	if len(st.tempTags) > 0 {
		im.untag(ctx, st.tempTags)
	}
}

func (im *Importer) importVolume(ctx context.Context, st *runState, vp VolumePlan, before string, progress func(Event)) ItemResult {
	r := ItemResult{Kind: KindVolume, Name: vp.Name, Size: vp.Size}
	switch {
	case vp.Status == StatusUnsupported:
		r.Outcome, r.Reason = OutcomeRefused, vp.Reason
		return r
	case st.plan.HelperImage == "":
		r.Outcome, r.Reason = OutcomeRefused, "Docker Desktop has no image to create the helper container from; pull a small one there (docker --context desktop-linux pull busybox)"
		return r
	}
	users, err := im.liveUsers(ctx)
	if err != nil {
		return failed(ctx, r, err)
	}
	if u := users[vp.Name]; len(u) > 0 && !st.sel.AllowLiveVolumes {
		r.Outcome = OutcomeRefused
		r.Reason = "mounted by running containers in Docker Desktop (" + strings.Join(u, ", ") + "): stop them in Docker Desktop first, so the copy is consistent (--allow-live-volumes copies anyway)"
		return r
	}
	live := len(vp.UsedBy) > 0 || len(users[vp.Name]) > 0
	if before == "" {
		return failed(ctx, r, errors.New("no content hash was recorded before the import"))
	}
	img, err := im.targetHelper(ctx, st, progress)
	if err != nil {
		return failed(ctx, r, err)
	}
	exists, err := im.dstVolumeExists(ctx, vp.Name)
	if err != nil {
		return failed(ctx, r, err)
	}
	if exists {
		th, err := im.hashTarget(ctx, img, vp.Name, PhaseCompare, progress)
		if err != nil {
			return failed(ctx, r, err)
		}
		switch {
		case th.Sum() == before:
			r.Outcome, r.Reason, r.Hash = OutcomeAlready, "in hyperbutler with the same content", before
			return r
		case !st.sel.OverwriteVolumes:
			r.Outcome, r.Reason = OutcomeRefused, "a volume of this name in hyperbutler has different content; --overwrite-volumes replaces it"
			return r
		}
		cctx, cancel := context.WithTimeout(ctx, cleanupTimeout)
		err = im.dst.JSON(cctx, http.MethodDelete, "/volumes/"+vp.Name, nil, nil, nil)
		cancel()
		if err != nil {
			return failed(ctx, r, fmt.Errorf("replace hyperbutler's volume (is a container there using it?): %w", err))
		}
	}
	labels := maps.Clone(vp.labels)
	if labels == nil {
		labels = map[string]string{}
	}
	labels[importLabel] = "docker-desktop"
	body := map[string]any{"Name": vp.Name, "Driver": "local", "Labels": labels}
	if err := im.dst.JSON(ctx, http.MethodPost, "/volumes/create", nil, body, nil); err != nil {
		return failed(ctx, r, fmt.Errorf("create the volume in hyperbutler: %w", err))
	}
	hash, n, err := im.fillVolume(ctx, img, st.plan.HelperImage, vp, before, live, progress)
	r.Bytes = n
	if err != nil {
		im.removeDstVolume(ctx, vp.Name)
		return failed(ctx, r, err)
	}
	r.Outcome, r.Hash = OutcomeImported, hash
	return r
}

// fillVolume copies the Docker Desktop volume into the new hyperbutler volume of the
// same name and verifies it: the stream's hash must equal the one recorded before the
// import (unless containers write it), and a fresh export of the copy must hash the
// same. The helpers are removed before it returns.
func (im *Importer) fillVolume(ctx context.Context, img, srcImg string, vp VolumePlan, before string, live bool, progress func(Event)) (string, int64, error) {
	dh, err := im.createDstHelper(ctx, img, vp.Name, true)
	if err != nil {
		return "", 0, err
	}
	defer im.removeDstHelper(ctx, dh)
	sh, err := im.src.CreateHelper(ctx, srcImg, vp.Name)
	if err != nil {
		return "", 0, fmt.Errorf("Docker Desktop's helper container: %w", err) //nolint:staticcheck // ST1005: a product name
	}
	defer im.removeSrcHelper(ctx, sh)
	sum, n, err := im.copyArchive(ctx, sh, dh, vp.Name, vp.Size, progress)
	if err != nil {
		return "", n, err
	}
	if !live && sum.Sum() != before {
		return "", n, errors.New("Docker Desktop's volume changed during the copy (is a container writing to it?)") //nolint:staticcheck // ST1005: a product name
	}
	got, err := im.hashDstHelper(ctx, dh, vp.Name, PhaseVerify, progress)
	if err != nil {
		return "", n, fmt.Errorf("verify: %w", err)
	}
	if got.Sum() != sum.Sum() {
		return "", n, fmt.Errorf("verification failed: the copy in hyperbutler hashes %s, Docker Desktop's volume %s", got.Sum(), sum.Sum())
	}
	return sum.Sum(), n, nil
}

// removeStaleHelpers removes helper containers an earlier, interrupted import left in
// either engine (only never-started ones with the helper label and entrypoint).
func (im *Importer) removeStaleHelpers(ctx context.Context, progress func(Event)) {
	q := url.Values{"all": {"1"}, "filters": {`{"label":["` + source.HelperLabel + `=` + source.HelperLabelValue + `"]}`}}
	var list []apiContainer
	if im.src.GetJSON(ctx, "/containers/json", q, &list) == nil {
		for _, c := range list {
			if !c.helper() || dockerapi.ValidName("container id", c.ID) != nil {
				continue
			}
			if err := im.src.RemoveStaleHelper(ctx, c.ID); err != nil {
				im.note("a container labeled as an import helper in Docker Desktop was left alone: %v", err)
				continue
			}
			progress(Event{Kind: EventNote, Message: "removed a helper container an earlier import left in Docker Desktop (" + shortID(c.ID) + ")"})
		}
	}
	list = nil
	if im.dst.JSON(ctx, http.MethodGet, "/containers/json", q, nil, &list) == nil {
		for _, c := range list {
			if !c.helper() || c.State != "created" || dockerapi.ValidName("container id", c.ID) != nil {
				continue
			}
			im.removeDstHelper(ctx, c.ID)
			v := c.Labels[copyingLabel]
			if v == "" || dockerapi.ValidName("volume", v) != nil {
				continue
			}
			// An interrupted copy: remove the volume if this import made it.
			var vi struct {
				Labels map[string]string `json:"Labels"`
			}
			if im.dst.JSON(ctx, http.MethodGet, "/volumes/"+v, nil, nil, &vi) == nil && vi.Labels[importLabel] != "" {
				im.removeDstVolume(ctx, v)
				progress(Event{Kind: EventNote, Message: "removed the incomplete copy of volume " + v + " an interrupted import left in hyperbutler"})
			}
		}
	}
}
