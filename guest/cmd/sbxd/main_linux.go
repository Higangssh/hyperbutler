//go:build linux

// sbxd is the guest agent of a sandbox session VM (ADR 0009). It takes the project the
// host copies in, once, on wire.CopyPort, records what it unpacked, and answers the
// host's requests for the session's changes on wire.Port. Every connection must come
// from the host (CID 2) and begin with the session's VM secret from the kernel command
// line; anything else is closed without a byte written.
package main

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Higangssh/hyperbutler/internal/sandbox/wire"
	"github.com/Higangssh/hyperbutler/internal/sandbox/workspace"
	"github.com/Higangssh/hyperbutler/internal/vmsecret"
)

const (
	workspaceDir = "/workspace"
	baselineFile = "/var/lib/hyperbutler/baseline.json"
)

func main() {
	if err := run(); err != nil {
		slog.Error("sbxd", "err", err)
		os.Exit(1)
	}
}

type state struct {
	secret vmsecret.Secret
	root   *os.Root

	mu   sync.Mutex
	base workspace.Baseline // nil until the project is copied in
}

func run() error {
	secret, err := vmsecret.Load("/proc/cmdline")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		return err
	}
	root, err := os.OpenRoot(workspaceDir)
	if err != nil {
		return err
	}
	s := &state{secret: secret, root: root}
	// A kept session that was copied into before keeps its baseline.
	if b, err := workspace.Load(baselineFile); err == nil {
		s.base = b
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("baseline: %w", err)
	}
	errc := make(chan error, 2)
	go func() { errc <- s.listen(wire.CopyPort, s.copyIn) }()
	go func() { errc <- s.listen(wire.Port, s.changes) }()
	return <-errc
}

// listen serves one connection at a time on port: the host is the only client, and
// serving one at a time bounds what a misbehaving one costs.
func (s *state) listen(port uint32, serve func(*vsockConn) error) error {
	l, err := listenVsock(port)
	if err != nil {
		return err
	}
	for {
		c, cid, err := l.accept()
		if err != nil {
			if errors.Is(err, unix.ECONNABORTED) || errors.Is(err, unix.EINTR) {
				continue
			}
			return err
		}
		if cid != unix.VMADDR_CID_HOST {
			_ = c.Close()
			continue
		}
		if err := vmsecret.Admit(c, s.secret, 5*time.Second); err != nil {
			_ = c.Close()
			continue
		}
		if err := serve(c); err != nil {
			slog.Warn("sbxd", "port", port, "err", err)
		}
		_ = c.Close()
	}
}

func (s *state) copyIn(c *vsockConn) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.base != nil {
		return errors.New("the project was already copied in")
	}
	b, err := workspace.Extract(s.root, c)
	if err != nil {
		return err
	}
	if err := os.MkdirAll("/var/lib/hyperbutler", 0o700); err != nil {
		return err
	}
	if err := b.Save(baselineFile); err != nil {
		return err
	}
	s.base = b
	// The host reads this byte as the copy's end: everything before it is on disk.
	_, err = c.Write([]byte{1})
	return err
}

func (s *state) changes(c *vsockConn) error {
	s.mu.Lock()
	base := s.base
	s.mu.Unlock()
	if base == nil {
		base = workspace.Baseline{}
	}
	return workspace.Serve(c, s.root, base)
}
