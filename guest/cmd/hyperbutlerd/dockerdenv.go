package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/Higangssh/hyperbutler/internal/dockerenv"
	"github.com/Higangssh/hyperbutler/internal/guestctl"
)

// dockerdEnv are the files of dockerd's environment (REQ-UX-009) on the /run tmpfs.
// The init exports the KEY=VALUE lines of env (only dockerenv.EnvNames) into each
// dockerd it starts; SSL_CERT_FILE names bundle, the stock CA bundle plus the host's
// extra CA certificates. Containers are not changed: like Docker Desktop, the
// certificates are for dockerd's own pulls and pushes (and BuildKit's), not for what
// runs in a container.
type dockerdEnv struct {
	env    string // KEY=VALUE lines; absent: none
	bundle string // the combined CA bundle; absent: the stock bundle is used
	stock  string // the image's stock bundle (/etc/ssl/certs/ca-certificates.crt)
}

// readOpt reads path; a missing file is nil, not an error.
func readOpt(path string) ([]byte, error) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: fixed paths from the command line, not host input
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return b, err
}

// writeOpt replaces path with data atomically, or removes it for nil data.
func writeOpt(path string, data []byte) error {
	if data == nil {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	tmp := path + ".new"
	_ = os.Remove(tmp) // a leftover would keep its mode
	// 0600: the environment can hold proxy credentials; only root (dockerd) reads it.
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// files renders doc as the contents of env and bundle (nil: no file).
func (e *dockerdEnv) files(doc *dockerenv.Doc) (env, bundle []byte, added int, err error) {
	if certs := doc.Certs(); len(certs) > 0 {
		stock, err := readOpt(e.stock)
		if err != nil {
			return nil, nil, 0, fmt.Errorf("read the stock CA bundle: %w", err)
		}
		bundle, added = dockerenv.Bundle(stock, certs)
	}
	path := ""
	if bundle != nil {
		path = e.bundle
	}
	if kv := doc.Environ(path); len(kv) > 0 {
		env = []byte(strings.Join(kv, "\n") + "\n")
	}
	return env, bundle, added, nil
}

// applyEnv installs the host's dockerd environment (data: an encoded dockerenv.Doc,
// empty for none) and restarts dockerd when it changed while dockerd runs, or when
// asked. It shares the daemon.json lock and restart protocol: one change to dockerd at
// a time, and a dockerd that does not come back with the new environment is restarted
// with the previous one (DaemonRolledBack). At boot, before the init has started
// dockerd, the files are only installed; the init starts dockerd once daemon.json is
// settled, which the host sends after the environment.
func (d *dockerdConfig) applyEnv(ctx context.Context, data []byte, restart bool) (res guestctl.EnvResult) {
	d.mu.Lock()
	defer d.mu.Unlock()
	t0 := time.Now()
	defer func() { res.Duration = time.Since(t0) }()
	fail := func(code guestctl.DaemonCode, format string, a ...any) guestctl.EnvResult {
		r := guestctl.EnvResult{Code: code, Message: clip(fmt.Sprintf(format, a...))}
		logf("dockerd environment: %s", r.Message)
		return r
	}
	e := d.denv
	if e == nil {
		return fail(guestctl.DaemonFailed, "not configured")
	}
	// The messages of Decode never contain a proxy URL.
	doc, err := dockerenv.Decode(data)
	if err != nil {
		return fail(guestctl.DaemonRejected, "%v", err)
	}
	env, bundle, added, err := e.files(doc)
	if err != nil {
		return fail(guestctl.DaemonFailed, "%v", err)
	}
	prevEnv, err1 := readOpt(e.env)
	prevBundle, err2 := readOpt(e.bundle)
	if err := errors.Join(err1, err2); err != nil {
		return fail(guestctl.DaemonFailed, "read the installed environment: %v", err)
	}
	changed := !sameOpt(prevEnv, env) || !sameOpt(prevBundle, bundle)
	if changed {
		// The bundle first: an installed env never names a bundle that is not there.
		if err := errors.Join(writeOpt(e.bundle, bundle), writeOpt(e.env, env)); err != nil {
			_ = writeOpt(e.bundle, prevBundle)
			_ = writeOpt(e.env, prevEnv)
			return fail(guestctl.DaemonFailed, "install the environment: %v", err)
		}
		res.Flags |= guestctl.DaemonChanged
		logf("dockerd environment: installed (%s; %d CA certificates beyond the stock bundle)", describeEnv(doc), added)
	}
	gen, _, err := readGen(d.gen)
	boot := errors.Is(err, fs.ErrNotExist)
	if err != nil && !boot {
		return fail(guestctl.DaemonFailed, "%v", err)
	}
	if boot {
		return res
	}
	if changed {
		restart = true // never report a changed environment that dockerd does not have
	}
	if !restart {
		return res
	}
	ctx = context.WithoutCancel(ctx) // see apply: a reconnect is no reason to roll back
	if err := os.WriteFile(d.hold, nil, 0o600); err != nil {
		return fail(guestctl.DaemonFailed, "hold file: %v", err)
	}
	defer func() { _ = os.Remove(d.hold) }()
	gen, err = d.restartDockerd(ctx, gen)
	if err == nil {
		res.Flags |= guestctl.DaemonRestarted
		logf("dockerd environment: dockerd restarted")
		return res
	}
	if !changed {
		return fail(guestctl.DaemonFailed, "dockerd did not come back after a restart: %v", err)
	}
	logf("dockerd environment: dockerd did not start with the new environment (%v); restoring the previous one", err)
	if ierr := errors.Join(writeOpt(e.bundle, prevBundle), writeOpt(e.env, prevEnv)); ierr != nil {
		return fail(guestctl.DaemonFailed, "dockerd did not start with the new environment (%v), and the previous one could not be restored: %v", err, ierr)
	}
	if _, rerr := d.restartDockerd(ctx, gen); rerr != nil {
		return fail(guestctl.DaemonFailed, "dockerd did not start with the new environment (%v), nor with the previous one (%v)", err, rerr)
	}
	r := fail(guestctl.DaemonRolledBack, "dockerd did not start with the new environment (%v); the previous one is back in effect", err)
	r.Flags = guestctl.DaemonChanged | guestctl.DaemonRestarted
	return r
}

func sameOpt(a, b []byte) bool { return (a == nil) == (b == nil) && bytes.Equal(a, b) }

// describeEnv says which variables doc sets, never their values: proxy URLs can carry
// credentials, and this goes to the console log.
func describeEnv(doc *dockerenv.Doc) string {
	var set []string
	for _, kv := range doc.Environ("") {
		set = append(set, kv[:strings.IndexByte(kv, '=')])
	}
	if len(set) == 0 {
		return "no proxy"
	}
	return "sets " + strings.Join(set, ", ")
}
