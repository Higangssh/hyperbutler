package desktopimport

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// snapshot is Docker Desktop's state as the import checks it: images with their tags,
// volume names, the content hash of each volume the import reads, and containers with
// their states.
type snapshot struct {
	images     map[string]string // id -> its tags, sorted and joined
	volumes    map[string]bool
	hashes     map[string]string // volume -> content hash
	containers map[string]string // id -> "name state image"
	helpers    []string          // import helper containers present
}

// snapshot records Docker Desktop's state, hashing the volumes in hash.
func (im *Importer) snapshot(ctx context.Context, helperImage string, hash []VolumePlan, progress func(Event)) (*snapshot, error) {
	s := &snapshot{images: map[string]string{}, volumes: map[string]bool{}, hashes: map[string]string{}, containers: map[string]string{}}
	var imgs []apiImage
	if err := im.src.GetJSON(ctx, "/images/json", nil, &imgs); err != nil {
		return nil, fmt.Errorf("list images: %w", err)
	}
	for _, i := range imgs {
		s.images[i.ID] = strings.Join(realTags(i.RepoTags), ",")
	}
	vols, err := im.sourceVolumes(ctx)
	if err != nil {
		return nil, err
	}
	for _, v := range vols {
		s.volumes[v.Name] = true
	}
	var cs []apiContainer
	if err := im.src.GetJSON(ctx, "/containers/json", url.Values{"all": {"1"}}, &cs); err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	for _, c := range cs {
		if c.helper() {
			s.helpers = append(s.helpers, c.ID)
			continue
		}
		s.containers[c.ID] = c.name() + " " + c.State + " " + shortID(c.ImageID)
	}
	for _, vp := range hash {
		if !s.volumes[vp.Name] {
			continue // gone: the comparison says so
		}
		sum, err := im.hashSource(ctx, helperImage, vp.Name, vp.Size, PhaseSnapshot, progress)
		if err != nil {
			return nil, err
		}
		s.hashes[vp.Name] = sum.Sum()
	}
	return s, nil
}

// compareSnapshots lists what differs. A volume in live (mounted by running containers)
// whose content changed is a warning, not a difference: its containers wrote it.
func compareSnapshots(b, a *snapshot, live map[string]bool) SourceCheck {
	c := SourceCheck{Images: len(a.images), Volumes: len(a.volumes), Containers: len(a.containers), HashedVolumes: len(b.hashes)}
	diff := func(format string, x ...any) { c.Differences = append(c.Differences, fmt.Sprintf(format, x...)) }
	for _, id := range sortedKeys(b.images) {
		tags, ok := a.images[id]
		switch {
		case !ok:
			diff("image %s (%s) is gone", shortID(id), b.images[id])
		case tags != b.images[id]:
			diff("image %s's tags changed from %q to %q", shortID(id), b.images[id], tags)
		}
	}
	for _, id := range sortedKeys(a.images) {
		if _, ok := b.images[id]; !ok {
			diff("image %s (%s) is new", shortID(id), a.images[id])
		}
	}
	for _, n := range sortedKeys(b.volumes) {
		if !a.volumes[n] {
			diff("volume %s is gone", n)
		}
	}
	for _, n := range sortedKeys(a.volumes) {
		if !b.volumes[n] {
			diff("volume %s is new", n)
		}
	}
	for _, n := range sortedKeys(b.hashes) {
		h, ok := a.hashes[n]
		switch {
		case !ok:
			// gone: said above
		case h != b.hashes[n] && live[n]:
			c.Warnings = append(c.Warnings, "volume "+n+"'s content changed during the import: running containers in Docker Desktop write to it")
		case h != b.hashes[n]:
			diff("volume %s's content changed", n)
		}
	}
	for _, id := range sortedKeys(b.containers) {
		s, ok := a.containers[id]
		switch {
		case !ok:
			diff("container %s is gone", b.containers[id])
		case s != b.containers[id]:
			diff("container %q is now %q", b.containers[id], s)
		}
	}
	for _, id := range sortedKeys(a.containers) {
		if _, ok := b.containers[id]; !ok {
			diff("container %s is new", a.containers[id])
		}
	}
	for _, id := range a.helpers {
		diff("an import helper container is still there: %s (remove it with docker --context desktop-linux rm %s)", shortID(id), id)
	}
	c.Unchanged = len(c.Differences) == 0
	return c
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
