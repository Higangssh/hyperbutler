package desktopimport

// Status is what an item of the plan would be, compared with what hyperbutler has now.
type Status string

const (
	// StatusNew: hyperbutler does not have it; it would be copied.
	StatusNew Status = "new"
	// StatusAlready: hyperbutler has it already (an image with the same id and every
	// tag; a volume of the same name with the same content hash).
	StatusAlready Status = "already"
	// StatusPartial: hyperbutler has the image (same id) but not every tag; only the
	// missing tags would be added, without copying the image again.
	StatusPartial Status = "partial"
	// StatusConflict: hyperbutler has something else under that name (a tag of another
	// image, a volume with other content). It is refused.
	StatusConflict Status = "conflict"
	// StatusExists: a volume of that name exists in hyperbutler and its content was not
	// compared (Plan without CompareVolumes, or no image to compare it with).
	StatusExists Status = "exists"
	// StatusUnsupported: it cannot be imported (an untagged image, a volume that is not
	// a plain local one).
	StatusUnsupported Status = "unsupported"
)

// Kinds of item.
const (
	KindImage  = "image"
	KindVolume = "volume"
)

// ImagePlan is one image in Docker Desktop.
type ImagePlan struct {
	ID   string   `json:"id"`
	Tags []string `json:"tags"`
	Size int64    `json:"sizeBytes"`
	// Status compares it with hyperbutler. MissingTags are the tags a partial import
	// adds; Conflicts are the tags hyperbutler has for another image.
	Status      Status   `json:"status"`
	MissingTags []string `json:"missingTags,omitempty"`
	Conflicts   []string `json:"conflicts,omitempty"`
	Reason      string   `json:"reason,omitempty"`
}

// Name is the image's first tag, or its short id.
func (p ImagePlan) Name() string {
	if len(p.Tags) > 0 {
		return p.Tags[0]
	}
	return shortID(p.ID)
}

// VolumePlan is one volume in Docker Desktop.
type VolumePlan struct {
	Name   string `json:"name"`
	Driver string `json:"driver"`
	// Size is the engine's figure (GET /system/df), -1 when it has none.
	Size   int64  `json:"sizeBytes"`
	Status Status `json:"status"`
	Reason string `json:"reason,omitempty"`
	// UsedBy are the running containers in Docker Desktop that mount it. Run refuses to
	// copy it while there are any, unless Selection.AllowLiveVolumes.
	UsedBy []string `json:"usedBy,omitempty"`
	// SourceHash and TargetHash are the content hashes, when compared.
	SourceHash string `json:"sourceHash,omitempty"`
	TargetHash string `json:"targetHash,omitempty"`

	labels map[string]string
}

// Plan is what Docker Desktop has and how it compares with hyperbutler.
type Plan struct {
	Source  Endpoint     `json:"source"`
	Target  string       `json:"target"`
	Images  []ImagePlan  `json:"images"`
	Volumes []VolumePlan `json:"volumes"`
	// HelperImage is the image in Docker Desktop the volume helper containers would be
	// created from; empty when there is none (volumes cannot be copied then).
	HelperImage string   `json:"helperImage,omitempty"`
	Notes       []string `json:"notes,omitempty"`
}

// Selection is what Run imports. Names are image tags (busybox means busybox:latest),
// image ids (or a prefix of 12 or more hex digits) and volume names; a name that is not
// in the plan is an error before anything is copied.
type Selection struct {
	Images     []string
	AllImages  bool
	Volumes    []string
	AllVolumes bool
	// OverwriteVolumes replaces a hyperbutler volume of the same name whose content
	// differs (its old content is deleted first).
	OverwriteVolumes bool
	// AllowLiveVolumes copies a volume a running Docker Desktop container mounts; the
	// copy may then be inconsistent.
	AllowLiveVolumes bool
}

// Outcome is what Run did with an item.
type Outcome string

const (
	OutcomeImported Outcome = "imported" // copied and verified
	OutcomeTagged   Outcome = "tagged"   // the image was there; missing tags added
	OutcomeAlready  Outcome = "already"  // nothing to do
	OutcomeRefused  Outcome = "refused"  // a conflict, a live volume, unsupported
	OutcomeFailed   Outcome = "failed"   // an error or a failed verification; nothing of it is left in hyperbutler
	OutcomeCanceled Outcome = "canceled" // not done: the import was canceled
)

// ItemResult is one selected item's outcome.
type ItemResult struct {
	Kind string `json:"kind"` // KindImage or KindVolume
	Name string `json:"name"`
	ID   string `json:"id,omitempty"`
	// Tags are an image's tags; AddedTags those Run added to an image already there.
	Tags      []string `json:"tags,omitempty"`
	AddedTags []string `json:"addedTags,omitempty"`
	Size      int64    `json:"sizeBytes"`
	// Bytes is what was streamed from Docker Desktop for it (0 when nothing was copied).
	Bytes   int64   `json:"bytesCopied"`
	Outcome Outcome `json:"outcome"`
	Reason  string  `json:"reason,omitempty"`
	// Hash is a volume's verified content hash.
	Hash string `json:"hash,omitempty"`
}

// SourceCheck compares Docker Desktop before and after the import.
type SourceCheck struct {
	Unchanged  bool `json:"unchanged"`
	Images     int  `json:"images"`
	Volumes    int  `json:"volumes"`
	Containers int  `json:"containers"`
	// HashedVolumes is how many volumes' content hashes were compared.
	HashedVolumes int      `json:"hashedVolumes"`
	Differences   []string `json:"differences,omitempty"`
	// Warnings are changes expected from running containers (a live volume's content).
	Warnings []string `json:"warnings,omitempty"`
}

// Result is what Run did.
type Result struct {
	Items []ItemResult `json:"items"`
	// Skipped are items --all-images or --all-volumes left out: unsupported ones.
	Skipped []ItemResult `json:"skipped,omitempty"`
	Bytes   int64        `json:"bytesCopied"`
	Source  SourceCheck  `json:"sourceCheck"`
	Notes   []string     `json:"notes,omitempty"`
	// Canceled says the context was canceled before every item was done.
	Canceled bool `json:"canceled"`
}

// OK reports whether every item was imported or already there and Docker Desktop is
// unchanged.
func (r *Result) OK() bool {
	if !r.Source.Unchanged || r.Canceled {
		return false
	}
	for _, it := range r.Items {
		switch it.Outcome {
		case OutcomeImported, OutcomeTagged, OutcomeAlready:
		default:
			return false
		}
	}
	return true
}

// EventKind is what an Event reports.
type EventKind string

const (
	EventStart    EventKind = "start"    // work on an item begins
	EventProgress EventKind = "progress" // bytes streamed so far
	EventDone     EventKind = "done"     // an item is finished: Outcome
	EventNote     EventKind = "note"     // something to tell the user
)

// Phases of an EventProgress.
const (
	PhaseSnapshot = "snapshot" // hashing a Docker Desktop volume before or after
	PhaseCopy     = "copy"
	PhaseVerify   = "verify"  // hashing the copy in hyperbutler
	PhaseCompare  = "compare" // hashing a hyperbutler volume of the same name
)

// Event is a progress report from Run. The callback runs on Run's goroutine; it must
// return quickly.
type Event struct {
	Kind    EventKind `json:"kind"`
	Item    string    `json:"item,omitempty"` // KindImage or KindVolume
	Name    string    `json:"name,omitempty"`
	Phase   string    `json:"phase,omitempty"`
	Bytes   int64     `json:"bytes,omitempty"`
	Total   int64     `json:"total,omitempty"` // 0 when unknown
	Outcome Outcome   `json:"outcome,omitempty"`
	Message string    `json:"message,omitempty"`
}
