//go:build windows

package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/Higangssh/hyperbutler/internal/engine"
	"github.com/Higangssh/hyperbutler/internal/updatecheck"
)

// The update check (REQ-UX-022): at most once a day `start` and `status` ask GitHub
// whether a newer release exists, and print one line with its URL when there is one.
// It is on by default (REQ-UX-005 allows this one request, with an off switch:
// `config set updateCheck false`); a development build checks but never prints.

// updateRecordName is the file in the state directory that holds the last check.
const updateRecordName = "update-check.json"

func updateRecordPath(l engine.Layout) string { return filepath.Join(l.Home, updateRecordName) }

// Seams for the tests: the checker (its URL and client) and the clock.
var (
	newUpdateChecker = func(version string, s *engine.Settings) *updatecheck.Checker {
		return updatecheck.New(version, updateProxy(s), nil)
	}
	updateNow = time.Now
)

// updateProxy is the check's proxy: proxy.https when it is set, else the HTTPS_PROXY
// and NO_PROXY variables of this shell.
func updateProxy(s *engine.Settings) func(*http.Request) (*url.URL, error) {
	if s != nil && s.HTTPSProxy != "" {
		if u, err := url.Parse(s.HTTPSProxy); err == nil {
			return http.ProxyURL(u)
		}
	}
	return http.ProxyFromEnvironment
}

// debugf writes a debug line when HYPERBUTLER_DEBUG is set; the update check's
// failures go only there.
func debugf(w io.Writer, format string, a ...any) {
	if os.Getenv("HYPERBUTLER_DEBUG") == "" {
		return
	}
	_, _ = fmt.Fprintf(w, "hyperbutler: debug: "+format+"\n", a...)
}

// checkUpdate makes the check when it is on and due, and returns the record (nil when
// it is off or hyperbutler is not set up). It never fails: a failure is a debug line.
func (in *install) checkUpdate(stderr io.Writer) *updatecheck.Record {
	if in.settings == nil || !in.settings.UpdateChecked() {
		return nil
	}
	path := updateRecordPath(in.l)
	b, _ := engine.ReadFileLimit(path, 4<<10)
	r := updatecheck.ParseRecord(b)
	next, changed, err := newUpdateChecker(versionInfo().Version, in.settings).Run(context.Background(), r, updateNow())
	if err != nil {
		debugf(stderr, "update check: %v", err)
	}
	if changed {
		if err := engine.WriteFileAtomic(path, next.Encode()); err != nil {
			debugf(stderr, "update check: %v", err)
		}
	}
	return &next
}

// lastUpdate is the last check's record without making one (nil when the check is off,
// hyperbutler is not set up, or no check was made yet).
func (in *install) lastUpdate() *updatecheck.Record {
	if in.settings == nil || !in.settings.UpdateChecked() {
		return nil
	}
	b, err := engine.ReadFileLimit(updateRecordPath(in.l), 4<<10)
	if err != nil {
		return nil
	}
	r := updatecheck.ParseRecord(b)
	if r.Checked.IsZero() {
		return nil
	}
	return &r
}

// noteUpdate prints the notice for r, if any.
func noteUpdate(w io.Writer, r *updatecheck.Record) {
	if r == nil {
		return
	}
	if n := updatecheck.Notice(*r, versionInfo().Version); n != "" {
		_, _ = fmt.Fprintln(w, n)
	}
}

// updateJSON is status --json's view of r.
func updateJSON(r *updatecheck.Record) *UpdateJSON {
	if r == nil {
		return nil
	}
	return &UpdateJSON{Checked: r.Checked, Latest: r.Latest, URL: r.URL}
}
