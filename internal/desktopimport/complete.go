package desktopimport

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"sync"

	"github.com/Higangssh/hyperbutler/internal/desktopimport/source"
	"github.com/Higangssh/hyperbutler/internal/dockerapi"
	"github.com/Higangssh/hyperbutler/internal/engine"
)

// completeFile records which hyperbutler volumes an import copied and verified:
// imports-complete.json in the state directory, a volume name and the id of the run
// that created it (the volume's source.RunLabel). Docker volume labels cannot be changed
// after the create, so "verified" cannot be a label. The recovery of an interrupted
// import never removes a volume recorded here; an entry is written right after the
// verification and dropped when the volume is gone or is another one of that name.
//
// Run writes it while holding the installation's import lock, so one process writes it
// at a time.
type completeFile struct {
	path string // "" keeps it in memory only

	mu sync.Mutex
	m  map[string]string // volume -> run id
}

// completeJSON is the file.
type completeJSON struct {
	Schema  int               `json:"schema"`
	Volumes map[string]string `json:"volumes"`
}

// completeMax bounds the file: tens of bytes per volume.
const completeMax = 4 << 20

// loadComplete reads the file (none: empty). An unreadable or invalid file is an error,
// so that it is not overwritten with less than it held.
func loadComplete(path string) (*completeFile, error) {
	c := &completeFile{path: path, m: map[string]string{}}
	if path == "" {
		return c, nil
	}
	b, err := engine.ReadFileLimit(path, completeMax)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var j completeJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return nil, fmt.Errorf("read %s: %w (move it away to start a new one)", path, err)
	}
	for name, run := range j.Volumes {
		if dockerapi.ValidName("volume", name) == nil && source.ValidRunID(run) {
			c.m[name] = run
		}
	}
	return c, nil
}

// has reports whether volume name, created by run, was verified.
func (c *completeFile) has(name, run string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return run != "" && c.m[name] == run
}

// set records volume name as verified, created by run; run "" drops the entry.
func (c *completeFile) set(name, run string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	old, had := c.m[name]
	if run == "" {
		if !had {
			return nil
		}
		delete(c.m, name)
	} else {
		if had && old == run {
			return nil
		}
		c.m[name] = run
	}
	if err := c.writeLocked(); err != nil {
		if had {
			c.m[name] = old
		} else {
			delete(c.m, name)
		}
		return err
	}
	return nil
}

// prune drops the entries of volumes that are gone or are another volume of the same
// name (labeled with another run): current maps every hyperbutler volume to its run
// label.
func (c *completeFile) prune(current map[string]string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	before := maps.Clone(c.m)
	for name, run := range c.m {
		if r, ok := current[name]; !ok || r != run {
			delete(c.m, name)
		}
	}
	if len(c.m) == len(before) {
		return nil
	}
	if err := c.writeLocked(); err != nil {
		c.m = before
		return err
	}
	return nil
}

func (c *completeFile) writeLocked() error {
	if c.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(completeJSON{Schema: 1, Volumes: c.m}, "", "  ")
	if err != nil {
		return err
	}
	if err := engine.WriteFileAtomic(c.path, append(b, '\n')); err != nil {
		return fmt.Errorf("record the verified volume in %s: %w", c.path, err)
	}
	return nil
}
