package desktopimport

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"

	"github.com/Higangssh/hyperbutler/internal/desktopimport/source"
	"github.com/Higangssh/hyperbutler/internal/dockerapi"
)

// staleState is what an interrupted import left behind, as findStale found it. Only Run
// removes it (applyStale); Plan only lists it.
//
// The rules, so that recovery never removes another import's work or a good copy:
//   - a helper container is a candidate only if it is labeled as one and its run
//     (source.RunLabel) is not alive; it is removed only after the checks of
//     source.CheckUnusedHelper (the label, exactly the helper's entrypoint, state
//     "created", never started), in both engines;
//   - a hyperbutler volume is removed only when a left-over helper that was filling it
//     (copyingLabel) names it, the volume was created by that helper's run (the same
//     RunLabel, and importLabel), and imports-complete.json does not record it as
//     verified;
//   - a temporary volume of --overwrite-volumes (tempForLabel) of a run that is not alive
//     is removed when it was not verified, or when the volume it was for is there; a
//     verified one whose volume is gone is kept, with a note, until that volume is
//     imported again.
type staleState struct {
	srcHelpers []string
	dstHelpers []string
	volumes    []string // half-filled copies
	temps      []string // temporary volumes to remove
	kept       []string // notes on what is kept
	current    map[string]string
}

// findStale lists what killed imports left; it only reads.
func (im *Importer) findStale(ctx context.Context, complete *completeFile) *staleState {
	s := &staleState{current: map[string]string{}}
	q := url.Values{"all": {"1"}, "filters": {`{"label":["` + source.HelperLabel + `=` + source.HelperLabelValue + `"]}`}}
	var list []apiContainer
	if im.src.GetJSON(ctx, "/containers/json", q, &list) == nil {
		for _, c := range list {
			if !c.helper() || dockerapi.ValidName("container id", c.ID) != nil || im.runLive(c.Labels[source.RunLabel]) {
				continue
			}
			s.srcHelpers = append(s.srcHelpers, c.ID)
		}
	}
	list = nil
	copying := map[string]string{} // volume -> the run of the helper that was filling it
	if im.dst.JSON(ctx, http.MethodGet, "/containers/json", q, nil, &list) == nil {
		for _, c := range list {
			if !c.helper() || dockerapi.ValidName("container id", c.ID) != nil {
				continue
			}
			labels, err := source.CheckUnusedHelper(ctx, im.dst, c.ID)
			if err != nil {
				if !dockerapi.IsNotFound(err) {
					im.note("a container labeled as an import helper in hyperbutler was left alone: %v", err)
				}
				continue
			}
			run := labels[source.RunLabel]
			if im.runLive(run) {
				continue
			}
			s.dstHelpers = append(s.dstHelpers, c.ID)
			if v := labels[copyingLabel]; v != "" && dockerapi.ValidName("volume", v) == nil && source.ValidRunID(run) {
				copying[v] = run
			}
		}
	}
	var vl struct {
		Volumes []apiVolume `json:"Volumes"`
	}
	if im.dst.JSON(ctx, http.MethodGet, "/volumes", nil, nil, &vl) != nil {
		s.current = nil // unknown: nothing is pruned
		return s
	}
	vols := map[string]apiVolume{}
	for _, v := range vl.Volumes {
		if dockerapi.ValidName("volume", v.Name) == nil {
			vols[v.Name] = v
			s.current[v.Name] = v.Labels[source.RunLabel]
		}
	}
	for _, name := range slices.Sorted(maps.Keys(copying)) {
		run := copying[name]
		v, ok := vols[name]
		if !ok || v.Labels[importLabel] == "" || v.Labels[source.RunLabel] != run {
			continue
		}
		if complete.has(name, run) {
			s.kept = append(s.kept, "volume "+name+" was verified by the import that made it, so it is kept although the helper that filled it was left behind")
			continue
		}
		s.volumes = append(s.volumes, name)
	}
	for _, name := range sortedKeys(vols) {
		v := vols[name]
		of, run := v.Labels[tempForLabel], v.Labels[source.RunLabel]
		if of == "" || v.Labels[importLabel] == "" || !source.ValidRunID(run) || im.runLive(run) || slices.Contains(s.volumes, name) {
			continue
		}
		if !complete.has(name, run) {
			s.temps = append(s.temps, name)
			continue
		}
		x, there := vols[of]
		partial := there && x.Labels[source.RunLabel] == run && !complete.has(of, run)
		if !there || partial || slices.Contains(s.volumes, of) {
			s.kept = append(s.kept, "an interrupted --overwrite-volumes of "+of+" left Docker Desktop's content, verified, in hyperbutler's volume "+name+
				"; import "+of+" again and a later import removes "+name+", or remove it with docker --context hyperbutler volume rm "+name)
			continue
		}
		s.temps = append(s.temps, name)
	}
	return s
}

// runLive reports whether run is a valid run id of an import still going on.
func (im *Importer) runLive(run string) bool { return source.ValidRunID(run) && im.alive(run) }

// planNotes are Plan's notes on what Run would remove first.
func (s *staleState) planNotes() []string {
	var out []string
	if n := len(s.srcHelpers); n > 0 {
		out = append(out, countWord(n, "helper container")+" an interrupted import left in Docker Desktop: the import removes them first (after checking each is an unused helper)")
	}
	if n := len(s.dstHelpers); n > 0 {
		out = append(out, countWord(n, "helper container")+" an interrupted import left in hyperbutler: the import removes them first")
	}
	for _, v := range s.volumes {
		out = append(out, "an interrupted import left an incomplete copy of volume "+v+" in hyperbutler: the import removes it first")
	}
	for _, v := range s.temps {
		out = append(out, "an interrupted import left the temporary volume "+v+" in hyperbutler: the import removes it first")
	}
	return append(out, s.kept...)
}

func countWord(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return strconv.Itoa(n) + " " + what + "s"
}

// applyStale removes what findStale found, checking each helper again first. It runs
// at the start of Run, which holds the installation's import lock.
func (im *Importer) applyStale(ctx context.Context, st *runState, s *staleState, progress func(Event)) {
	for _, id := range s.srcHelpers {
		err := im.src.RemoveStaleHelper(ctx, id, im.alive)
		switch {
		case errors.Is(err, source.ErrRunAlive):
		case err != nil:
			im.note("a container labeled as an import helper in Docker Desktop was left alone: %v", err)
		default:
			progress(Event{Kind: EventNote, Message: "removed a helper container an earlier import left in Docker Desktop (" + shortID(id) + ")"})
		}
	}
	removed := map[string]bool{}
	for _, id := range s.dstHelpers {
		labels, err := source.CheckUnusedHelper(ctx, im.dst, id)
		if err != nil || im.runLive(labels[source.RunLabel]) {
			continue
		}
		cctx, cancel := cleanupCtx(ctx)
		err = im.dst.JSON(cctx, http.MethodDelete, "/containers/"+id, url.Values{"v": {"1"}, "force": {"0"}}, nil, nil)
		cancel()
		if err != nil && !dockerapi.IsNotFound(err) {
			im.note("could not remove the helper container %s an earlier import left in hyperbutler (%v); remove it with: docker --context hyperbutler rm %s", shortID(id), err, id)
			if v := labels[copyingLabel]; v != "" {
				removed[v] = false
			}
			continue
		}
		progress(Event{Kind: EventNote, Message: "removed a helper container an earlier import left in hyperbutler (" + shortID(id) + ")"})
	}
	for _, v := range s.volumes {
		if ok, seen := removed[v]; seen && !ok {
			continue // its helper is still there
		}
		if im.removeDstVolume(ctx, v) {
			delete(s.current, v)
			progress(Event{Kind: EventNote, Message: "removed the incomplete copy of volume " + v + " an interrupted import left in hyperbutler"})
		}
	}
	for _, v := range s.temps {
		if im.removeDstVolume(ctx, v) {
			delete(s.current, v)
			progress(Event{Kind: EventNote, Message: "removed the temporary volume " + v + " an interrupted import left in hyperbutler"})
		}
	}
	for _, n := range s.kept {
		im.note("%s", n)
	}
	if s.current != nil {
		if err := st.complete.prune(s.current); err != nil {
			im.note("%v", err)
		}
	}
}
