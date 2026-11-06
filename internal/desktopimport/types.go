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
	// StatusDifferentLabels: a volume of that name in hyperbutler has the same content
	// but other labels or driver options (labels docker compose relies on, say). It is
	// refused unless Selection.OverwriteVolumes, except that an empty, unused volume
	// without compose labels is created again with Docker Desktop's labels.
	StatusDifferentLabels Status = "different-labels"
	// StatusExists: a volume of that name exists in hyperbutler and its content was not
	// compared (Plan without CompareVolumes, or no image to compare it with).
	StatusExists Status = "exists"
	// StatusUnsupported: it cannot be imported (an untagged image, a volume that is not
	// a plain local one).
	StatusUnsupported Status = "unsupported"
	// StatusCompose: a container of a Compose project. It is never recreated: docker
	// compose up in the project's folder does that.
	StatusCompose Status = "compose"
	// StatusConfirm: a container that uses something a copy should not bring along
	// unasked (ContainerPlan.Requires). It is skipped unless the user confirms it by name.
	StatusConfirm Status = "needs-confirm"
)

// Kinds of item.
const (
	KindImage     = "image"
	KindVolume    = "volume"
	KindContainer = "container"
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

// ContainerPlan is one container in Docker Desktop (REQ-UX-024). Nothing of it says what
// its environment holds: that goes to hyperbutler's create and is never shown.
type ContainerPlan struct {
	Name string `json:"name"`
	ID   string `json:"id"`
	// Image is the reference the container is created from in hyperbutler; ImageID the
	// image it runs in Docker Desktop.
	Image   string `json:"image,omitempty"`
	ImageID string `json:"imageId,omitempty"`
	State   string `json:"state,omitempty"` // in Docker Desktop: running, exited, ...
	Status  Status `json:"status"`
	Reason  string `json:"reason,omitempty"`
	// Project and ProjectDir are its Compose project and that project's folder, if known.
	Project    string `json:"project,omitempty"`
	ProjectDir string `json:"projectDir,omitempty"`
	// Requires is what needs the user's confirmation, one sentence each.
	Requires []string `json:"requires,omitempty"`
	// CapAdd are the capabilities it adds: shown before the copy, not asked about.
	CapAdd []string `json:"capAdd,omitempty"`
	// Binds are the bind mounts it gets in hyperbutler ("C:\src -> /src").
	Binds []string `json:"binds,omitempty"`
	Ports []string `json:"ports,omitempty"`
	// Volumes are its named volumes and Networks its user-defined networks; selecting the
	// container brings the volumes and its image along.
	Volumes  []string `json:"volumes,omitempty"`
	Networks []string `json:"networks,omitempty"`
	// WritableBytes is the size of its writable layer, which is not copied; -1: unknown.
	WritableBytes int64 `json:"writableBytes"`
	// Notes are what the copy leaves behind: the writable layer, anonymous volumes, a
	// setting that does not carry over.
	Notes []string `json:"notes,omitempty"`

	spec      *containerSpec
	confirmed bool // the selection names it in ConfirmContainers
}

// Plan is what Docker Desktop has and how it compares with hyperbutler.
type Plan struct {
	Source  Endpoint     `json:"source"`
	Target  string       `json:"target"`
	Images  []ImagePlan  `json:"images"`
	Volumes []VolumePlan `json:"volumes"`
	// Containers is empty unless PlanOptions.Containers asked for them.
	Containers []ContainerPlan `json:"containers,omitempty"`
	// HelperImage is the image in Docker Desktop the volume helper containers would be
	// created from; empty when there is none (volumes cannot be copied then).
	HelperImage string   `json:"helperImage,omitempty"`
	Notes       []string `json:"notes,omitempty"`
	// Recovery is what an interrupted import left behind and Run removes first (or
	// keeps, and why). Plan removes none of it.
	Recovery []string `json:"recovery,omitempty"`
}

// Selection is what Run imports. Names are image tags (busybox means busybox:latest),
// image ids (or a prefix of 12 or more hex digits) and volume names; a name that is not
// in the plan is an error before anything is copied.
type Selection struct {
	Images     []string
	AllImages  bool
	Volumes    []string
	AllVolumes bool
	// Containers are container names (or ids) to create again in hyperbutler, and
	// AllContainers every one that needs no confirmation. Either brings the container's
	// image and named volumes along. ConfirmContainers selects the containers it names
	// and confirms what they require (ContainerPlan.Requires).
	Containers        []string
	AllContainers     bool
	ConfirmContainers []string
	// OverwriteVolumes replaces a hyperbutler volume of the same name whose content or
	// labels differ. Docker Desktop's content is first copied into a temporary volume
	// and verified; only then is hyperbutler's deleted, and the new one filled from the
	// temporary one and verified again. It needs twice the volume's size while it runs.
	OverwriteVolumes bool
	// AllowLiveVolumes copies a volume a running Docker Desktop container mounts; the
	// copy may then be inconsistent.
	AllowLiveVolumes bool
	// ForceSpace starts the copy even when Importer.Space says it does not fit.
	ForceSpace bool
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
	Kind string `json:"kind"` // KindImage, KindVolume or KindContainer
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
	HashedVolumes int `json:"hashedVolumes"`
	// Differences are changes the import could have caused: images, tags, volumes, the
	// content of a volume it read, a helper container left. Any makes Unchanged false.
	Differences []string `json:"differences,omitempty"`
	// Warnings are changes the import cannot cause: a container's state (the user's own
	// actions in Docker Desktop, or its restart policy), a container added or removed,
	// and the content of a volume running containers write.
	Warnings []string `json:"warnings,omitempty"`
}

// Result is what Run did.
type Result struct {
	Items []ItemResult `json:"items"`
	// Skipped are items --all-images, --all-volumes or --all-containers left out:
	// unsupported ones, and containers of a Compose project or that need a confirmation.
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
	Item    string    `json:"item,omitempty"` // KindImage, KindVolume or KindContainer
	Name    string    `json:"name,omitempty"`
	Phase   string    `json:"phase,omitempty"`
	Bytes   int64     `json:"bytes,omitempty"`
	Total   int64     `json:"total,omitempty"` // 0 when unknown
	Outcome Outcome   `json:"outcome,omitempty"`
	Message string    `json:"message,omitempty"`
}
