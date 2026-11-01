package bridge

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"time"
)

// The window's "Import from Docker Desktop" (REQ-UX-023). The import itself lives in
// its own package (`hyperbutler import --from-desktop`); the window drives it through
// Importer, and says "not available yet" while Options.Importer is nil.

// Importer is what the window needs from the Docker Desktop import.
type Importer interface {
	// Plan lists what could be imported and the state of each item.
	Plan(ctx context.Context) ([]ImportItem, error)
	// Check says whether the items with these ids fit (the free-space check); an error
	// with CodeImportSpace says they may not. Run does not check again.
	Check(ctx context.Context, ids []string) error
	// Run imports the items with these ids, reporting progress, until done or ctx ends.
	Run(ctx context.Context, ids []string, progress func(ImportProgress)) (ImportResult, error)
}

// Import item states. New, partial and exists can be chosen; the others are shown only.
const (
	ImportNew         = "new"
	ImportDone        = "imported" // already imported
	ImportPartial     = "partial"  // the image is there, some tags are missing
	ImportConflict    = "conflict" // something else has the name in hyperbutler
	ImportExists      = "exists"   // a volume of that name is there, content not compared
	ImportUnsupported = "unsupported"
	ImportInUse       = "inuse" // a volume a running Docker Desktop container mounts
	// ImportAttention is any state this window does not know yet: shown, not chosen.
	ImportAttention = "attention"
)

// ImportStatuses are every state, for the catalog test.
var ImportStatuses = []string{ImportNew, ImportDone, ImportPartial, ImportConflict, ImportExists, ImportUnsupported, ImportInUse, ImportAttention}

// importSelectable are the states an item can be imported from.
var importSelectable = map[string]bool{ImportNew: true, ImportPartial: true, ImportExists: true}

// ImportItem is one thing the import can bring over.
type ImportItem struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"` // image, volume, container, context, ...
	Name   string `json:"name"`
	Size   int64  `json:"size"` // bytes; -1 unknown
	Status string `json:"status"`
	// Note is the source's reason or detail (English): the containers using a volume,
	// why an item is refused.
	Note string `json:"note,omitempty"`
}

// ImportResult is what an import did, for its last progress event.
type ImportResult struct {
	Imported int   `json:"imported"`
	Already  int   `json:"already"`
	Refused  int   `json:"refused"`
	Failed   int   `json:"failed"`
	Bytes    int64 `json:"bytes"`
	// SourceUnchanged is the check that Docker Desktop is as it was before; Differences
	// say what changed when it is not.
	SourceUnchanged bool     `json:"sourceUnchanged"`
	Differences     []string `json:"differences,omitempty"`
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
	// Result is set on the last event.
	Result *ImportResult `json:"result,omitempty"`
}

// CodeImportUnavailable: the import is not in this build yet.
const CodeImportUnavailable = "HB-IMP-001"

// CodeImportFailed: the import stopped with an error.
const CodeImportFailed = "HB-IMP-002"

// CodeDesktopNotRunning: nothing answers on Docker Desktop's engine pipe.
const CodeDesktopNotRunning = "HB-IMP-003"

// CodeImportSpace: the copy may not fit; import.run with force starts it anyway.
const CodeImportSpace = "HB-IMP-004"

// CodeImportBusy: another import runs in this installation.
const CodeImportBusy = "HB-IMP-005"

// ImportArgs are the ids import.run takes.
type ImportArgs struct {
	IDs   []string `json:"ids"`
	Token string   `json:"token"`
	// Force starts the copy even when the free-space check says it may not fit
	// (import --force-space).
	Force bool `json:"force,omitempty"`
}

const maxImportItems = 500

var reImportID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@-]{0,127}$`)

func checkImportArgs(a *ImportArgs) error {
	if err := checkToken(a.Token); err != nil {
		return err
	}
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
	run     tracker           // the running import's Run
}

// WaitImport waits up to timeout for a running import's Run to return (after
// CancelImport, its cleanup takes up to about 30 s), and reports whether it did.
func (d *Dispatcher) WaitImport(timeout time.Duration) bool { return d.imp.run.wait(timeout) }

func (d *Dispatcher) importPlan(ctx context.Context) (any, error) {
	if d.o.Importer == nil {
		return nil, Codedf(CodeImportUnavailable, "importing from Docker Desktop is not available in this build yet")
	}
	if d.ImportRunning() {
		// A new plan replaces the importer the running import uses.
		return nil, Codedf(CodeBusy, "an import is running; plan again when it is done")
	}
	items, err := d.o.Importer.Plan(ctx)
	if err != nil {
		var e *Error
		if errors.As(err, &e) {
			return nil, err // the importer's own code
		}
		return nil, Coded(CodeImportFailed, err)
	}
	out := make([]ImportItem, 0, min(len(items), maxImportItems))
	planned := map[string]string{}
	for i, it := range items {
		if i == maxImportItems {
			break
		}
		known := false
		for _, s := range ImportStatuses {
			known = known || it.Status == s
		}
		if !known {
			it.Status = ImportAttention
		}
		it.ID, it.Kind, it.Name, it.Note = field(it.ID), field(it.Kind), field(it.Name), field(it.Note)
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
func (d *Dispatcher) importRun(ids []string, force bool, token string) (any, error) {
	if d.o.Importer == nil {
		return nil, Codedf(CodeImportUnavailable, "importing from Docker Desktop is not available in this build yet")
	}
	d.imp.mu.Lock()
	defer d.imp.mu.Unlock()
	if d.imp.cancel != nil {
		return nil, Codedf(CodeBusy, "an import is already running")
	}
	// Busy first, then the token: a request refused as busy keeps it.
	if err := d.tok.use(token, OpImportRun, ""); err != nil {
		return nil, err
	}
	var take []string
	for _, id := range ids {
		if st, ok := d.imp.planned[id]; ok && importSelectable[st] {
			take = append(take, id)
		}
	}
	if len(take) == 0 {
		return nil, badf("import.run: none of the ids is in the plan, or all are imported")
	}
	if !force {
		c, cancel := context.WithTimeout(d.base, readTimeout)
		err := d.o.Importer.Check(c, take)
		cancel()
		if err != nil {
			var e *Error
			if !errors.As(err, &e) {
				err = Coded(CodeImportFailed, err)
			}
			return nil, err
		}
	}
	// Not the window's context: closing the window must not cancel the import under its
	// own cleanup. The window cancels it (CancelImport) and waits for Run to return
	// (Wait) before the process ends.
	ctx, cancel := context.WithCancel(context.WithoutCancel(d.base))
	d.imp.cancel = cancel
	d.bg.add()
	d.imp.run.add()
	go func() {
		defer d.bg.done()
		defer d.imp.run.done()
		notify := func(p ImportProgress) {
			if d.o.Notify != nil {
				d.o.Notify("import", p)
			}
		}
		res, err := d.o.Importer.Run(ctx, take, func(p ImportProgress) {
			p.Current = field(p.Current) // a name someone else wrote
			notify(p)
		})
		res.Differences = cleanList(res.Differences)
		final := ImportProgress{Total: len(take), Done: res.Imported + res.Already + res.Refused + res.Failed, Bytes: res.Bytes, Result: &res}
		switch {
		case errors.Is(err, context.Canceled):
			final.Canceled = true
		case err != nil:
			r := d.fail(0, Coded(CodeImportFailed, err), CodeImportFailed)
			final.Error = r.Error
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
	d.CancelImport()
	return struct{}{}, nil
}

// ImportRunning reports whether an import runs (the window asks before it closes).
func (d *Dispatcher) ImportRunning() bool {
	d.imp.mu.Lock()
	defer d.imp.mu.Unlock()
	return d.imp.cancel != nil
}

// CancelImport cancels the running import, if any. Its Run still undoes the item in
// progress and checks Docker Desktop; Wait waits for that.
func (d *Dispatcher) CancelImport() {
	d.imp.mu.Lock()
	defer d.imp.mu.Unlock()
	if d.imp.cancel != nil {
		d.imp.cancel()
	}
}

// cleanList bounds a list of English lines from the import for the page.
func cleanList(l []string) []string {
	if len(l) > 20 {
		l = l[:20]
	}
	out := make([]string, 0, len(l))
	for _, s := range l {
		out = append(out, field(s))
	}
	return out
}
