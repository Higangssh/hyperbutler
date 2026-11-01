package main

import "github.com/Higangssh/hyperbutler/internal/desktopimport"

// ImportJSON is `hyperbutler import --from-desktop --json` (REQ-UX-023): with --dry-run
// what Docker Desktop has and how each item compares with hyperbutler, otherwise what was
// imported and the check that Docker Desktop is unchanged. OK is false, and the command
// exits 1, when an item failed or was refused, the import was canceled, or Docker
// Desktop changed.
type ImportJSON struct {
	Schema int              `json:"schema"`
	DryRun bool             `json:"dryRun"`
	Source ImportSourceJSON `json:"source"`
	Target string           `json:"target"` // hyperbutler's pipe
	// Items are the selected items (every one, in a dry run) with their status and,
	// after a run, outcome; Skipped are those --all-* left out, with the reason.
	Items       []ImportItemJSON `json:"items"`
	Skipped     []ImportItemJSON `json:"skipped"`
	BytesCopied int64            `json:"bytesCopied"`
	// SourceCheck compares Docker Desktop before and after (not in a dry run).
	SourceCheck *desktopimport.SourceCheck `json:"sourceCheck,omitempty"`
	Canceled    bool                       `json:"canceled"`
	OK          bool                       `json:"ok"`
	Notes       []string                   `json:"notes,omitempty"`
}

// ImportSourceJSON is Docker Desktop's engine: its pipe, how it was found (context,
// default or override) and its version.
type ImportSourceJSON struct {
	Pipe    string `json:"pipe"`
	Via     string `json:"via"`
	Version string `json:"version,omitempty"`
}

// ImportItemJSON is one image or volume. Status is the comparison with hyperbutler
// before the run (new, already, partial, conflict, different-labels, exists,
// unsupported); Outcome what
// the run did (imported, tagged, already, refused, failed, canceled; empty in a dry
// run).
type ImportItemJSON struct {
	Kind        string   `json:"kind"` // image or volume
	Name        string   `json:"name"`
	ID          string   `json:"id,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	SizeBytes   int64    `json:"sizeBytes"` // -1: unknown
	Status      string   `json:"status"`
	Outcome     string   `json:"outcome,omitempty"`
	BytesCopied int64    `json:"bytesCopied"`
	Reason      string   `json:"reason,omitempty"`
	// MissingTags are the tags a partial image lacks in hyperbutler; AddedTags those
	// the run added.
	MissingTags []string `json:"missingTags,omitempty"`
	AddedTags   []string `json:"addedTags,omitempty"`
	// UsedBy are running Docker Desktop containers that mount the volume.
	UsedBy []string `json:"usedBy,omitempty"`
	// Hash is a volume's verified content hash.
	Hash string `json:"hash,omitempty"`
}
