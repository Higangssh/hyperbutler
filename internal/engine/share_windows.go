//go:build windows

package engine

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"

	"github.com/Microsoft/go-winio"
	"github.com/Microsoft/go-winio/pkg/guid"

	"github.com/Higangssh/hyperbutler/internal/share"
	"github.com/Higangssh/hyperbutler/internal/transport"
	"github.com/Higangssh/hyperbutler/internal/vm"
)

// SharePort is the AF_VSOCK port the guest's share relay connects to (ADR 0007).
const SharePort = 2385

// ShareMount is where the guest mounts the share file system.
const ShareMount = "/mnt/hyperbutler"

// ShareConfig is the bind-mount file server (ADR 0007).
type ShareConfig struct {
	// Off turns Windows bind mounts off: no service, no relay, no create rewriting.
	Off bool
	// Options tune the server (share.Options defaults if zero).
	Options share.Options
}

// shareService serves the guest's FUSE connections and resolves Windows bind sources
// for the Docker API proxy.
type shareService struct {
	srv *share.Server
	reg *share.Registry
	svc guid.GUID
	log *slog.Logger

	mu     sync.Mutex
	ln     net.Listener
	conns  map[net.Conn]struct{}
	closed bool
	wg     sync.WaitGroup
}

// setupShares builds the share server. It returns nil when bind mounts are off. The
// caller adds svcs to the VM document and args to the kernel command line.
func setupShares(sc ShareConfig, sddl string, log *slog.Logger) (s *shareService, svcs []vm.Service, args string) {
	if sc.Off {
		return nil, nil, ""
	}
	opt := sc.Options
	if opt.Logger == nil {
		opt.Logger = slog.New(levelFilter{h: log.Handler(), min: slog.LevelWarn})
	}
	srv := share.NewServer(opt)
	s = &shareService{
		srv:   srv,
		reg:   &share.Registry{Server: srv, MountPoint: ShareMount},
		svc:   transport.VsockServiceID(SharePort),
		log:   log,
		conns: map[net.Conn]struct{}{},
	}
	// The guest connects to the host on this service; only this user may listen on it.
	svcs = []vm.Service{{ID: s.svc, BindSDDL: sddl}}
	return s, svcs, fmt.Sprintf(" hyperbutler.share=%d", SharePort)
}

// listen starts accepting the guest relay's connections on the VM's hvsock address.
func (s *shareService) listen(vmid guid.GUID) error {
	ln, err := winio.ListenHvsock(&winio.HvsockAddr{VMID: vmid, ServiceID: s.svc})
	if err != nil {
		return fmt.Errorf("listen hvsock %s: %w", s.svc, err)
	}
	s.serve(ln)
	return nil
}

// serve accepts on ln until close and runs share.Server.Serve for each connection.
func (s *shareService) serve(ln net.Listener) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = ln.Close()
		return
	}
	s.ln = ln
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			if s.closed {
				s.mu.Unlock()
				_ = c.Close()
				return
			}
			s.conns[c] = struct{}{}
			s.wg.Add(1)
			s.mu.Unlock()
			go func() {
				defer s.wg.Done()
				err := s.srv.Serve(c)
				s.mu.Lock()
				delete(s.conns, c)
				closed := s.closed
				s.mu.Unlock()
				_ = c.Close()
				if err != nil && !closed && !errors.Is(err, net.ErrClosed) {
					s.log.Warn("guest share connection ended", "err", err)
				}
			}()
		}
	}()
}

// rewriteCreate is the proxy's container create rewriter.
func (s *shareService) rewriteCreate(body []byte) ([]byte, error) {
	return share.RewriteCreate(body, func(p string) (string, error) {
		if !s.srv.Mounted() {
			return "", errors.New("bind mounts of Windows paths are not available: the guest has not mounted the share file system (see hyperbutler logs --console)")
		}
		return s.reg.Resolve(p)
	})
}

// close stops accepting, closes every guest connection, waits for their goroutines and
// removes the shares (closing their handles and watchers).
func (s *shareService) close() {
	s.mu.Lock()
	s.closed = true
	if s.ln != nil {
		_ = s.ln.Close()
	}
	for c := range s.conns {
		_ = c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
	_ = s.srv.Close()
}
