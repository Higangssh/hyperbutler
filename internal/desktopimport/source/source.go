// Package source is the only way the importer reaches Docker Desktop's engine. Its
// Client can send a GET to a fixed list of read-only endpoints, create a helper
// container that mounts one volume read-only (never started), and remove a helper it
// created (or one an earlier import left, after checking it is one and that the import
// run that made it is gone). Nothing else: no tag, delete, prune, stop, start or exec of
// anything of the user's, and no POST, PUT or DELETE beyond those two. The importer
// lives in another package, so it cannot reach the underlying HTTP client to send
// anything else.
package source

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/Higangssh/hyperbutler/internal/dockerapi"
)

// HelperLabel marks a helper container: key and value.
const (
	HelperLabel      = "dev.hyperbutler.import"
	HelperLabelValue = "helper"
)

// RunLabel holds the id of the import run that created a helper (NewRunID), so another
// import can tell a helper of a run still going on from one a killed run left.
const RunLabel = "dev.hyperbutler.import.run"

// HelperMount is where a helper container mounts the volume it reads or writes.
const HelperMount = "/v"

// HelperEntrypoint is the helper's command. A helper is never started; if anything ever
// did start one, this program does not exist and it exits at once.
const HelperEntrypoint = "/hyperbutler-import-helper-never-started"

// ErrNotAllowed is a request the source client never sends.
var ErrNotAllowed = errors.New("not a read-only request: the importer never changes Docker Desktop's engine")

// ErrNotHelper is a container labeled as an import helper that is not an unused one
// (it ran, or has another entrypoint): it is never removed.
var ErrNotHelper = errors.New("it is not an unused import helper")

// ErrRunAlive is a left-over looking helper whose import run is still going on (another
// import, in this or another installation): it is not removed.
var ErrRunAlive = errors.New("its import is still running")

// NewRunID is a new import run id: 16 lowercase hex digits.
func NewRunID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// ValidRunID reports whether s is a run id as NewRunID makes them.
func ValidRunID(s string) bool {
	if len(s) != 16 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// Client is a read-only view of an engine, plus helper containers.
type Client struct {
	c *dockerapi.Client

	mu      sync.Mutex
	helpers map[string]bool // ids of the helpers this client created and has not removed
}

// New wraps c. The caller must not use c for anything else.
func New(c *dockerapi.Client) *Client { return &Client{c: c, helpers: map[string]bool{}} }

// Close drops idle connections.
func (s *Client) Close() { s.c.Close() }

// allowedGET reports whether path (already built from validated names) is one of the
// read-only endpoints the importer uses. Everything else, including GETs that attach
// to a container (attach/ws) or stream its events, is refused.
func allowedGET(path string) bool {
	switch path {
	case "/_ping", "/info", "/version", "/images/json", "/images/get", "/volumes", "/containers/json", "/system/df":
		return true
	}
	segs := strings.Split(strings.TrimPrefix(path, "/"), "/")
	switch {
	case len(segs) == 2 && (segs[0] == "volumes" || segs[0] == "networks"):
		return segs[1] != ""
	case len(segs) == 3 && segs[0] == "containers" && (segs[2] == "json" || segs[2] == "archive"):
		return segs[1] != ""
	case len(segs) >= 3 && segs[0] == "images" && segs[len(segs)-1] == "json":
		// /images/{name}/json; the name may hold '/' (registry/owner/name).
		for _, s := range segs[1 : len(segs)-1] {
			if s == "" || s == "." || s == ".." {
				return false
			}
		}
		return true
	}
	return false
}

// Get sends a GET to one of the read-only endpoints; the caller closes the body.
func (s *Client) Get(ctx context.Context, path string, q url.Values) (*http.Response, error) {
	if !allowedGET(path) {
		return nil, fmt.Errorf("GET %s: %w", path, ErrNotAllowed)
	}
	return s.c.Do(ctx, http.MethodGet, path, q, nil, "")
}

// GetJSON GETs path and decodes the answer into out.
func (s *Client) GetJSON(ctx context.Context, path string, q url.Values, out any) error {
	if !allowedGET(path) {
		return fmt.Errorf("GET %s: %w", path, ErrNotAllowed)
	}
	return s.c.JSON(ctx, http.MethodGet, path, q, nil, out)
}

// HelperBody is the container create request of a helper that mounts volume at
// HelperMount: read-only for the source side, never with the image's content copied
// into the volume (NoCopy), without network, and with an entrypoint that does not
// exist. It is labeled HelperLabel and RunLabel=run, plus labels. The target side uses
// it too, writable only for the helper that fills a new volume.
//
// The image must declare no VOLUME: a container created from one that does gets an
// anonymous volume for each. The importer picks such images, and removes its helpers
// with their anonymous volumes in case.
func HelperBody(image, volume, run string, readOnly bool, labels map[string]string) map[string]any {
	l := maps.Clone(labels)
	if l == nil {
		l = map[string]string{}
	}
	l[HelperLabel], l[RunLabel] = HelperLabelValue, run
	return map[string]any{
		"Image":           image,
		"Entrypoint":      []string{HelperEntrypoint},
		"Cmd":             []string{},
		"Labels":          l,
		"NetworkDisabled": true,
		"HostConfig": map[string]any{
			"NetworkMode": "none",
			"Mounts": []map[string]any{{
				"Type": "volume", "Source": volume, "Target": HelperMount, "ReadOnly": readOnly,
				"VolumeOptions": map[string]any{"NoCopy": true},
			}},
		},
	}
}

// HelperName is a new helper container name.
func HelperName() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return "hyperbutler-import-" + hex.EncodeToString(b[:])
}

// CreateHelper creates (never starts) a helper container from image, an image already
// present that declares no VOLUME, that mounts volume read-only and is labeled with
// the import run's id. It first checks the volume exists, so the create cannot make a
// new one. It returns the container id; RemoveHelper removes it.
func (s *Client) CreateHelper(ctx context.Context, image, volume, run string) (string, error) {
	if err := dockerapi.ValidImageRef(image); err != nil {
		return "", err
	}
	if err := dockerapi.ValidName("volume", volume); err != nil {
		return "", err
	}
	if !ValidRunID(run) {
		return "", fmt.Errorf("import run id %q is not valid", dockerapi.Clean(run, 40))
	}
	// A mount of a volume that does not exist creates it: make sure it exists (it can
	// still be removed in between; the check after the import would show that).
	if err := s.c.JSON(ctx, http.MethodGet, "/volumes/"+volume, nil, nil, nil); err != nil {
		return "", err
	}
	var created struct {
		ID string `json:"Id"`
	}
	q := url.Values{"name": {HelperName()}}
	if err := s.c.JSON(ctx, http.MethodPost, "/containers/create", q, HelperBody(image, volume, run, true, nil), &created); err != nil {
		return "", fmt.Errorf("create the helper container: %w", err)
	}
	if err := dockerapi.ValidName("container id", created.ID); err != nil {
		return "", err
	}
	s.mu.Lock()
	s.helpers[created.ID] = true
	s.mu.Unlock()
	return created.ID, nil
}

// RemoveHelper removes a helper container this client created, with its anonymous
// volumes (v=1). A helper is created from an image without VOLUME and has none, but if
// it ever had, its create made them and they go with it; a named volume is never
// removed by it. It is not forced: a helper never runs.
func (s *Client) RemoveHelper(ctx context.Context, id string) error {
	s.mu.Lock()
	ok := s.helpers[id]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("remove container %s: %w (not a helper this import created)", dockerapi.Clean(id, 80), ErrNotAllowed)
	}
	if err := s.remove(ctx, id); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.helpers, id)
	s.mu.Unlock()
	return nil
}

func (s *Client) remove(ctx context.Context, id string) error {
	q := url.Values{"v": {"1"}, "force": {"0"}}
	err := s.c.JSON(ctx, http.MethodDelete, "/containers/"+id, q, nil, nil)
	if dockerapi.IsNotFound(err) {
		return nil
	}
	return err
}

// Helpers returns the ids of helpers created and not removed yet.
func (s *Client) Helpers() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.helpers))
	for id := range s.helpers {
		out = append(out, id)
	}
	return out
}

// neverStarted is Docker's zero time, the StartedAt of a container never started.
const neverStarted = "0001-01-01T00:00:00Z"

// CheckUnusedHelper inspects container id on engine c and returns its labels if it is an
// unused import helper: the helper label, exactly the helper's entrypoint, state
// "created" and never started. Anything else is an error wrapping ErrNotHelper. The
// left-over helpers of both engines are checked with it before they are removed.
func CheckUnusedHelper(ctx context.Context, c *dockerapi.Client, id string) (map[string]string, error) {
	if err := dockerapi.ValidName("container id", id); err != nil {
		return nil, err
	}
	var in struct {
		Config struct {
			Labels     map[string]string `json:"Labels"`
			Entrypoint []string          `json:"Entrypoint"`
		} `json:"Config"`
		State struct {
			Status    string `json:"Status"`
			StartedAt string `json:"StartedAt"`
		} `json:"State"`
	}
	if err := c.JSON(ctx, http.MethodGet, "/containers/"+id+"/json", nil, nil, &in); err != nil {
		return nil, err
	}
	if in.Config.Labels[HelperLabel] != HelperLabelValue || len(in.Config.Entrypoint) != 1 ||
		in.Config.Entrypoint[0] != HelperEntrypoint || in.State.Status != "created" ||
		(in.State.StartedAt != "" && in.State.StartedAt != neverStarted) {
		return nil, fmt.Errorf("container %s: %w", id, ErrNotHelper)
	}
	return in.Config.Labels, nil
}

// RemoveStaleHelper removes a helper an earlier import left behind (it was killed before
// it could): only after CheckUnusedHelper accepts it, and only when the run that made it
// is gone. alive reports whether a run is still going on; a helper without a valid run
// id (from an older version) counts as left behind. A helper of a live run is kept, with
// an error wrapping ErrRunAlive.
func (s *Client) RemoveStaleHelper(ctx context.Context, id string, alive func(run string) bool) error {
	labels, err := CheckUnusedHelper(ctx, s.c, id)
	if errors.Is(err, ErrNotHelper) {
		return fmt.Errorf("%w: %w", ErrNotAllowed, err)
	}
	if err != nil {
		return err
	}
	if run := labels[RunLabel]; ValidRunID(run) && alive(run) {
		return fmt.Errorf("helper %s: %w", dockerapi.Clean(id, 80), ErrRunAlive)
	}
	return s.remove(ctx, id)
}
