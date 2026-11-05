// Package apply writes the changes a user accepted in a sandbox review into their
// project directory (ADR 0009, "Bringing changes out", REQ-SBX-002 and REQ-SBX-003).
//
// The bytes written are the ones the review showed, from the review cache; the guest is
// never read here. Every change is checked against the path grammar first, and on
// Windows every file is opened by one name at a time relative to a handle on its
// verified parent, never following a link, and the name the file system found is
// compared with the one asked for, so an 8.3 short name or a case-only variant of an
// existing name is refused rather than written through. A file the host changed since
// the copy into the session is a conflict and is left alone.
package apply

import (
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/Higangssh/hyperbutler/internal/sandbox/review"
)

// MaxChanges bounds one apply.
const MaxChanges = 100000

// Change is one accepted file change.
type Change struct {
	// Path is the project-relative, '/'-separated path (review.CheckPath's grammar).
	Path string
	// Delete removes the file instead of writing Data.
	Delete bool
	// Data is the reviewed content.
	Data []byte
	// Base is the SHA-256 of the file when it was copied into the session, or nil if
	// there was no file at Path then. A host file that no longer matches is a conflict.
	Base *[sha256.Size]byte
}

// Errors an Outcome can carry, besides file system errors.
var (
	// ErrConflict: the host file changed (or appeared, or went away) since the copy.
	ErrConflict = errors.New("changed on this computer since the session started")
	// ErrCollision: two changes name the same file in a case-insensitive directory.
	ErrCollision = errors.New("names the same file as another change")
	// ErrNotApplied: changes under .git come out as a bundle, never as files.
	ErrNotApplied = errors.New("is inside .git; commits are fetched from the session's bundle instead")
	// ErrName: the file system found a different name than the one asked for (an 8.3
	// short name, a different case, or a link).
	ErrName = errors.New("names an existing file by another name")
)

// Outcome is the result of one change, in the order the changes were given.
type Outcome struct {
	Path string
	Err  error
}

// plan checks the change set as a whole and returns, for each change, its segments or
// the reason it will not be applied.
func plan(changes []Change) ([][]string, []error, error) {
	if len(changes) > MaxChanges {
		return nil, nil, fmt.Errorf("%d changes (at most %d)", len(changes), MaxChanges)
	}
	segs := make([][]string, len(changes))
	errs := make([]error, len(changes))
	seen := map[string]int{}
	for i, c := range changes {
		s, err := review.CheckPath(c.Path)
		if err != nil {
			errs[i] = err
			continue
		}
		if review.Classify(c.Path, false) == review.RiskGitInner {
			errs[i] = ErrNotApplied
			continue
		}
		if c.Delete && c.Base == nil {
			errs[i] = ErrConflict // nothing to delete was there at copy time
			continue
		}
		k := review.FoldKey(c.Path)
		if j, ok := seen[k]; ok {
			errs[i], errs[j] = ErrCollision, ErrCollision
			continue
		}
		seen[k] = i
		segs[i] = s
	}
	// A file and a directory of the same name in one set (a/b written and a/b/c written)
	// cannot both be applied.
	for i, s := range segs {
		if s == nil {
			continue
		}
		for n := 1; n < len(s); n++ {
			if j, ok := seen[review.FoldKey(joinSegs(s[:n]))]; ok && errs[j] == nil && !changes[j].Delete {
				errs[i], errs[j] = ErrCollision, ErrCollision
			}
		}
	}
	return segs, errs, nil
}

func joinSegs(s []string) string {
	out := s[0]
	for _, x := range s[1:] {
		out += "/" + x
	}
	return out
}

// Apply writes changes into the directory root and returns one Outcome per change. An
// error is returned only when nothing was attempted.
func Apply(root string, changes []Change) ([]Outcome, error) {
	segs, errs, err := plan(changes)
	if err != nil {
		return nil, err
	}
	return applyPlanned(root, changes, segs, errs)
}
