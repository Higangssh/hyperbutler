package manager

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Higangssh/hyperbutler/internal/sandbox/copyin"
)

// MaxSlot mirrors the broker's: a user runs at most this many sessions. A test holds the
// two equal.
const MaxSlot = 4

// maxRecord bounds a session's record: one line per copied file at worst.
const maxRecord = 256 << 20

// Record is what survives the manager: a session's file in the sandbox folder. A crash or
// the user can leave anything there, so ParseRecord is strict.
type Record struct {
	Name     string    `json:"name"`
	Slot     uint8     `json:"slot"`
	Dir      string    `json:"dir"`
	Kept     bool      `json:"kept"`
	Mode     string    `json:"mode"`
	Allow    []string  `json:"allow,omitempty"`
	CPUs     int       `json:"cpus"`
	MemoryMB int       `json:"memoryMB"`
	DiskMeta uint64    `json:"diskMeta"`
	Created  time.Time `json:"created"`
	// Git says the project's history went in, so commits can come out.
	Git bool `json:"git,omitempty"`
	// Fetched is the commit `sandbox commits` last brought back.
	Fetched string `json:"fetched,omitempty"`
	// Sent is what each path held when copied in, the base of apply's conflict check.
	// A hex SHA-256; a symbolic link has "link:" and its target instead.
	Sent map[string]string `json:"sent"`
}

// ParseRecord reads and checks a record.
func ParseRecord(b []byte) (*Record, error) {
	var r Record
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	if err := CheckName(r.Name); err != nil {
		return nil, err
	}
	if r.Slot < 1 || r.Slot > MaxSlot {
		return nil, fmt.Errorf("slot %d", r.Slot)
	}
	if !filepath.IsAbs(r.Dir) {
		return nil, fmt.Errorf("project %q is not an absolute path", r.Dir)
	}
	for p, s := range r.Sent {
		if strings.HasPrefix(s, "link:") {
			continue
		}
		if b, err := hex.DecodeString(s); err != nil || len(b) != 32 {
			return nil, fmt.Errorf("the hash of %q", p)
		}
	}
	return &r, nil
}

func sentToRecord(sent map[string]copyin.Copied) map[string]string {
	out := make(map[string]string, len(sent))
	for p, c := range sent {
		if c.Link != "" {
			out[p] = "link:" + c.Link
		} else {
			out[p] = hex.EncodeToString(c.Sum[:])
		}
	}
	return out
}

func sentFromRecord(rec map[string]string) map[string]copyin.Copied {
	out := make(map[string]copyin.Copied, len(rec))
	for p, s := range rec {
		if l, ok := strings.CutPrefix(s, "link:"); ok {
			out[p] = copyin.Copied{Link: l}
			continue
		}
		var c copyin.Copied
		if b, err := hex.DecodeString(s); err == nil && len(b) == len(c.Sum) {
			copy(c.Sum[:], b)
		}
		out[p] = c
	}
	return out
}

// store is the sandbox folder, private to the user.
type store struct{ dir string }

func (s store) path(name string) string { return filepath.Join(s.dir, name+".json") }

// save writes r atomically, so a crash leaves the old record or the new one.
func (s store) save(r *Record) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.dir, r.Name+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, s.path(r.Name))
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}

func (s store) remove(name string) error {
	if err := os.Remove(s.path(name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// load reads every record. One that cannot be read is reported and left in place: it
// still owns a disk, which only the user can decide about.
func (s store) load() ([]*Record, []error) {
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, []error{err}
	}
	var out []*Record
	var errs []error
	for _, e := range ents {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		name := filepath.Join(s.dir, e.Name())
		fi, err := e.Info()
		if err == nil && fi.Size() > maxRecord {
			err = errors.New("too large")
		}
		var r *Record
		if err == nil {
			var b []byte
			if b, err = os.ReadFile(name); err == nil { //nolint:gosec // G304: a file in the user's own sandbox folder
				r, err = ParseRecord(b)
			}
		}
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		case r.Name != strings.TrimSuffix(e.Name(), ".json"):
			errs = append(errs, fmt.Errorf("%s: names session %q", name, r.Name))
		default:
			out = append(out, r)
		}
	}
	return out, errs
}
