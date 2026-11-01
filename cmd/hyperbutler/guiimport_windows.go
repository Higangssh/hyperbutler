//go:build windows

package main

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"

	"github.com/Microsoft/go-winio"

	"github.com/Higangssh/hyperbutler/internal/desktopimport"
	"github.com/Higangssh/hyperbutler/internal/gui/bridge"
	"github.com/Higangssh/hyperbutler/internal/mcpserver"
)

// guiImporter is the window's Docker Desktop import (REQ-UX-023): a thin adapter of
// internal/desktopimport to bridge.Importer. The plan and the importer that made it are
// kept until the next plan, so Run imports exactly what the user saw.
type guiImporter struct {
	// sourcePipe stands in for Docker Desktop's pipe (tests); empty: discover it.
	sourcePipe string

	mu   sync.Mutex
	im   *desktopimport.Importer
	plan *desktopimport.Plan
	// running counts Runs in progress: the importer is never closed or replaced under
	// one; a close meanwhile (closed) happens when the last one returns.
	running int
	closed  bool
}

// Item ids: the kind and the name the import takes (an image id, a volume name).
const (
	importImagePrefix  = "image:"
	importVolumePrefix = "volume:"
)

func (g *guiImporter) open(ctx context.Context) (*desktopimport.Importer, error) {
	target, err := (&mcpHost{}).Pipe(ctx)
	if errors.Is(err, mcpserver.ErrEngineStopped) {
		return nil, bridge.ErrEngineStopped
	}
	if err != nil {
		return nil, bridge.Coded(bridge.CodeEngineBusy, err)
	}
	im, err := desktopimport.Open(ctx, desktopimport.Options{
		Dial:       func(ctx context.Context, pipe string) (net.Conn, error) { return winio.DialPipeContext(ctx, pipe) },
		SourcePipe: g.sourcePipe,
		TargetPipe: target,
	})
	if errors.Is(err, desktopimport.ErrDesktopNotRunning) {
		return nil, bridge.Coded(bridge.CodeDesktopNotRunning, err)
	}
	return im, err
}

// importStatus maps a plan status to the window's.
func importStatus(s desktopimport.Status) string {
	switch s {
	case desktopimport.StatusNew:
		return bridge.ImportNew
	case desktopimport.StatusAlready:
		return bridge.ImportDone
	case desktopimport.StatusPartial:
		return bridge.ImportPartial
	case desktopimport.StatusConflict:
		return bridge.ImportConflict
	case desktopimport.StatusExists:
		return bridge.ImportExists
	case desktopimport.StatusUnsupported:
		return bridge.ImportUnsupported
	}
	// A state added to the import later: shown as needing attention, not chosen.
	return bridge.ImportAttention
}

// selection turns the window's ids into the import's selection.
func selection(ids []string) desktopimport.Selection {
	var sel desktopimport.Selection
	for _, id := range ids {
		switch {
		case strings.HasPrefix(id, importImagePrefix):
			sel.Images = append(sel.Images, strings.TrimPrefix(id, importImagePrefix))
		case strings.HasPrefix(id, importVolumePrefix):
			sel.Volumes = append(sel.Volumes, strings.TrimPrefix(id, importVolumePrefix))
		}
	}
	return sel
}

func (g *guiImporter) Plan(ctx context.Context) ([]bridge.ImportItem, error) {
	// A plan removes nothing, so it needs no lock; Run takes the import lock itself.
	im, err := g.open(ctx)
	if err != nil {
		return nil, err
	}
	plan, err := im.Plan(ctx, desktopimport.PlanOptions{CompareVolumes: true})
	if err != nil {
		im.Close()
		return nil, err
	}
	g.mu.Lock()
	if g.running > 0 || g.closed {
		g.mu.Unlock()
		im.Close()
		return nil, bridge.Codedf(bridge.CodeBusy, "an import is running; plan again when it is done")
	}
	if g.im != nil {
		g.im.Close()
	}
	g.im, g.plan = im, plan
	g.mu.Unlock()
	var out []bridge.ImportItem
	for _, ip := range plan.Images {
		out = append(out, bridge.ImportItem{ID: importImagePrefix + ip.ID, Kind: desktopimport.KindImage, Name: ip.Name(),
			Size: ip.Size, Status: importStatus(ip.Status), Note: ip.Reason})
	}
	for _, vp := range plan.Volumes {
		it := bridge.ImportItem{ID: importVolumePrefix + vp.Name, Kind: desktopimport.KindVolume, Name: vp.Name,
			Size: vp.Size, Status: importStatus(vp.Status), Note: vp.Reason}
		if len(vp.UsedBy) > 0 && (vp.Status == desktopimport.StatusNew || vp.Status == desktopimport.StatusExists) {
			// Run refuses a volume a running container mounts; the user stops it first.
			it.Status, it.Note = bridge.ImportInUse, strings.Join(vp.UsedBy, ", ")
		}
		out = append(out, it)
	}
	return out, nil
}

func (g *guiImporter) planned() (*desktopimport.Importer, *desktopimport.Plan, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.im == nil || g.plan == nil {
		return nil, nil, errors.New("no plan: open the import again")
	}
	return g.im, g.plan, nil
}

// Check is import --from-desktop's free-space check for these items.
func (g *guiImporter) Check(_ context.Context, ids []string) error {
	im, plan, err := g.planned()
	if err != nil {
		return err
	}
	space, err := im.Space(plan, selection(ids))
	if err != nil {
		return err
	}
	return bridge.Coded(bridge.CodeImportSpace, space.Check())
}

func (g *guiImporter) Run(ctx context.Context, ids []string, progress func(bridge.ImportProgress)) (bridge.ImportResult, error) {
	im, plan, err := g.planned()
	if err != nil {
		return bridge.ImportResult{}, err
	}
	g.mu.Lock()
	g.running++
	g.mu.Unlock()
	defer g.endRun()
	// The window ran Check first, or the user chose to start anyway, so Run does not
	// refuse on space again. It takes the import lock itself.
	sel := selection(ids)
	sel.ForceSpace = true
	total, done := len(sel.Images)+len(sel.Volumes), 0
	var bytes int64
	res, err := im.Run(ctx, plan, sel, func(e desktopimport.Event) {
		switch e.Kind {
		case desktopimport.EventDone:
			done++
		case desktopimport.EventProgress:
			bytes = e.Bytes
		case desktopimport.EventNote:
			return
		}
		progress(bridge.ImportProgress{Running: true, Done: done, Total: total, Current: e.Name, Bytes: bytes})
	})
	out := bridge.ImportResult{}
	if res != nil {
		out.Bytes, out.SourceUnchanged, out.Differences = res.Bytes, res.Source.Unchanged, res.Source.Differences
		for _, it := range res.Items {
			switch it.Outcome {
			case desktopimport.OutcomeImported, desktopimport.OutcomeTagged:
				out.Imported++
			case desktopimport.OutcomeAlready:
				out.Already++
			case desktopimport.OutcomeRefused:
				out.Refused++
			case desktopimport.OutcomeFailed:
				out.Failed++
			}
		}
		if res.Canceled && err == nil {
			err = context.Canceled
		}
	}
	if errors.Is(err, desktopimport.ErrImportRunning) {
		err = bridge.Coded(bridge.CodeImportBusy, err)
	}
	return out, err
}

// endRun ends a Run; the last one does a close that came meanwhile.
func (g *guiImporter) endRun() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.running--
	if g.running == 0 && g.closed && g.im != nil {
		g.im.Close()
		g.im, g.plan = nil, nil
	}
}

// close drops the importer the last plan used (when the window closes); under a
// running Run (the window gave up waiting for it) the last Run closes it.
func (g *guiImporter) close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closed = true
	if g.running == 0 && g.im != nil {
		g.im.Close()
		g.im, g.plan = nil, nil
	}
}
