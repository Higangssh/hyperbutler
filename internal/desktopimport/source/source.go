// Package source is the only way the importer reaches Docker Desktop's engine. Its
// Client can send a GET to a fixed list of read-only endpoints, create a helper
// container that mounts one volume read-only (never started), and remove a helper it
// created (or one an earlier import left, after checking it is one). Nothing else: no
// tag, delete, prune, stop, start or exec of anything of the user's, and no POST, PUT or
// DELETE beyond those two. The importer lives in another package, so it cannot reach
// the underlying HTTP client to send anything else.
package source

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
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

// HelperMount is where a helper container mounts the volume it reads or writes.
const HelperMount = "/v"

// HelperEntrypoint is the helper's command. A helper is never started; if anything ever
// did start one, this program does not exist and it exits at once.
const HelperEntrypoint = "/hyperbutler-import-helper-never-started"

// ErrNotAllowed is a request the source client never sends.
var ErrNotAllowed = errors.New("not a read-only request: the importer never changes Docker Desktop's engine")

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
	case len(segs) == 2 && segs[0] == "volumes":
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
// exist. The target side uses it with readOnly false.
func HelperBody(image, volume string, readOnly bool) map[string]any {
	return map[string]any{
		"Image":           image,
		"Entrypoint":      []string{HelperEntrypoint},
		"Cmd":             []string{},
		"Labels":          map[string]string{HelperLabel: HelperLabelValue},
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
// present, that mounts volume read-only. It first checks the volume exists, so the
// create cannot make a new one. It returns the container id; RemoveHelper removes it.
func (s *Client) CreateHelper(ctx context.Context, image, volume string) (string, error) {
	if err := dockerapi.ValidImageRef(image); err != nil {
		return "", err
	}
	if err := dockerapi.ValidName("volume", volume); err != nil {
		return "", err
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
	if err := s.c.JSON(ctx, http.MethodPost, "/containers/create", q, HelperBody(image, volume, true), &created); err != nil {
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

// RemoveHelper removes a helper container this client created. Its anonymous volumes
// are kept (v=0; a helper has none) and it is not forced: a helper never runs.
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
	q := url.Values{"v": {"0"}, "force": {"0"}}
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

// RemoveStaleHelper removes a helper an earlier import left behind (it was killed before
// it could): only after inspecting it shows the helper label, the helper's entrypoint,
// state "created" and that it was never started.
func (s *Client) RemoveStaleHelper(ctx context.Context, id string) error {
	if err := dockerapi.ValidName("container id", id); err != nil {
		return err
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
	if err := s.c.JSON(ctx, http.MethodGet, "/containers/"+id+"/json", nil, nil, &in); err != nil {
		return err
	}
	if in.Config.Labels[HelperLabel] != HelperLabelValue || len(in.Config.Entrypoint) != 1 ||
		in.Config.Entrypoint[0] != HelperEntrypoint || in.State.Status != "created" ||
		(in.State.StartedAt != "" && in.State.StartedAt != neverStarted) {
		return fmt.Errorf("container %s: %w (it is not an unused import helper)", id, ErrNotAllowed)
	}
	return s.remove(ctx, id)
}
