package bridge

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sync"
)

// The window's "Import from Docker Desktop" (REQ-UX-023). The import itself lives in
// its own package (`hyperbutler import --from-desktop`); the window drives it through
// Importer, and says "not available yet" while Options.Importer is nil.

// Importer is what the window needs from the Docker Desktop import.
type Importer interface {
	// Plan lists what could be imported and the state of each item.
	Plan(ctx context.Context) ([]ImportItem, error)
	// Run imports the items with these ids, reporting progress, until done or ctx ends.
	Run(ctx context.Context, ids []string, progress func(ImportProgress)) error
}

// Import item states.
const (
	ImportNew      = "new"
	ImportDone     = "imported" // already imported: shown, not selectable
	ImportPartial  = "partial"
	ImportConflict = "conflict"
)

// ImportItem is one thing the import can bring over.
type ImportItem struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"` // image, volume, container, context, ...
	Name   string `json:"name"`
	Size   int64  `json:"size"` // bytes; -1 unknown
	Status string `json:"status"`
}

// ImportProgress is the import's progress, sent to the page as {"event":"import"}.
type ImportProgress struct {
	Running bool   `json:"running"`
	Done    int    `json:"done"`
	Total   int    `json:"total"`
	Current string `json:"current,omitempty"`
	Bytes   int64  `json:"bytes"`
	// Error is set when the import ended with an error; Canceled when the user
	// stopped it.
	Error    *ErrorView `json:"error,omitempty"`
	Canceled bool       `json:"canceled,omitempty"`
}

// CodeImportUnavailable: the import is not in this build yet.
const CodeImportUnavailable = "HB-IMP-001"

// CodeImportFailed: the import stopped with an error.
const CodeImportFailed = "HB-IMP-002"

// ImportArgs are the ids import.run takes.
type ImportArgs struct {
	IDs []string `json:"ids"`
}

const maxImportItems = 500

var reImportID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@-]{0,127}$`)

func checkImportArgs(a *ImportArgs) error {
	if len(a.IDs) == 0 || len(a.IDs) > maxImportItems {
		return fmt.Errorf("ids: want 1 to %d", maxImportItems)
	}
	for _, id := range a.IDs {
		if !reImportID.MatchString(id) {
			return errors.New("ids: not an item id")
		}
	}
	return nil
}

// importState is the one import a window runs at a time.
type importState struct {
	mu      sync.Mutex
	cancel  context.CancelFunc
	planned map[string]string // id -> status, from the last plan
}

func (d *Dispatcher) importPlan(ctx context.Context) (any, error) {
	if d.o.Importer == nil {
		return nil, Codedf(CodeImportUnavailable, "importing from Docker Desktop is not available in this build yet")
	}
	items, err := d.o.Importer.Plan(ctx)
	if err != nil {
		return nil, Coded(CodeImportFailed, err)
	}
	out := make([]ImportItem, 0, min(len(items), maxImportItems))
	planned := map[string]string{}
	for i, it := range items {
		if i == maxImportItems {
			break
		}
		switch it.Status {
		case ImportNew, ImportDone, ImportPartial, ImportConflict:
		default:
			it.Status = ImportConflict
		}
		it.ID, it.Kind, it.Name = field(it.ID), field(it.Kind), field(it.Name)
		planned[it.ID] = it.Status
		out = append(out, it)
	}
	d.imp.mu.Lock()
	d.imp.planned = planned
	d.imp.mu.Unlock()
	return struct {
		Items []ImportItem `json:"items"`
	}{out}, nil
}

// importRun starts the import of ids in the background; progress goes to the page as
// events. Only items of the last plan that are not already imported are taken.
func (d *Dispatcher) importRun(ids []string) (any, error) {
	if d.o.Importer == nil {
		return nil, Codedf(CodeImportUnavailable, "importing from Docker Desktop is not available in this build yet")
	}
	d.imp.mu.Lock()
	defer d.imp.mu.Unlock()
	if d.imp.cancel != nil {
		return nil, Codedf(CodeBusy, "an import is already running")
	}
	var take []string
	for _, id := range ids {
		if st, ok := d.imp.planned[id]; ok && st != ImportDone {
			take = append(take, id)
		}
	}
	if len(take) == 0 {
		return nil, badf("import.run: none of the ids is in the plan, or all are imported")
	}
	ctx, cancel := context.WithCancel(d.base)
	d.imp.cancel = cancel
	go func() {
		notify := func(p ImportProgress) {
			if d.o.Notify != nil {
				d.o.Notify("import", p)
			}
		}
		err := d.o.Importer.Run(ctx, take, notify)
		final := ImportProgress{Total: len(take)}
		switch {
		case errors.Is(err, context.Canceled):
			final.Canceled = true
		case err != nil:
			r := d.fail(0, Coded(CodeImportFailed, err), CodeImportFailed)
			final.Error = r.Error
		default:
			final.Done = len(take)
		}
		d.imp.mu.Lock()
		d.imp.cancel = nil
		d.imp.mu.Unlock()
		cancel()
		notify(final)
	}()
	return struct{}{}, nil
}

func (d *Dispatcher) importCancel() (any, error) {
	d.imp.mu.Lock()
	defer d.imp.mu.Unlock()
	if d.imp.cancel != nil {
		d.imp.cancel()
	}
	return struct{}{}, nil
}
