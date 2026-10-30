//go:build windows

package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

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

	// grace and debounce are pruneGrace and pruneDebounce (tests shorten them).
	grace, debounce time.Duration

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

		grace: pruneGrace, debounce: pruneDebounce,
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
		var backoff time.Duration
		for {
			c, err := ln.Accept()
			if err != nil {
				s.mu.Lock()
				closed := s.closed
				s.mu.Unlock()
				if closed || errors.Is(err, net.ErrClosed) {
					return
				}
				// A failed accept (a guest connection reset while pending) must not end
				// bind mounts for the rest of the run: the relay redials.
				backoff = min(max(2*backoff, 5*time.Millisecond), time.Second)
				s.log.Warn("guest share accept failed; retrying", "err", err, "backoff", backoff)
				time.Sleep(backoff)
				continue
			}
			backoff = 0
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
	mounted := func(resolve share.Resolver) share.Resolver {
		return func(p string) (string, error) {
			if !s.srv.Mounted() {
				return "", errors.New("bind mounts of Windows paths are not available: the guest has not mounted the share file system (see hyperbutler logs --console)")
			}
			return resolve(p)
		}
	}
	// -v creates a missing source (as dockerd does); --mount refuses it.
	return share.RewriteCreateMounts(body, mounted(s.reg.Resolve), mounted(s.reg.ResolveExisting))
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

// Share lifetime (ADR 0007, integration step 5). A share is removed once no container,
// in any state, has a bind mount from it. The guest dockerd's container list says which
// shares are in use; it is read again after a container is destroyed (debounced),
// whenever the event stream (re)opens, and every pruneEvery in case events were lost.
const (
	// pruneGrace keeps a share that a create resolved this recently: dockerd may not
	// list the container yet.
	pruneGrace = time.Minute
	// pruneDebounce batches the prunes of a burst of destroys (docker rm $(docker ps -aq)).
	pruneDebounce = 2 * time.Second
	pruneEvery    = 10 * time.Minute
)

// startLifetime follows the guest dockerd until ctx ends, pruning unused shares.
func (s *shareService) startLifetime(ctx context.Context, target transport.HvsockTarget) {
	d := &transport.HvsockDialer{Target: target, Retries: 2, RetryWait: 100 * time.Millisecond}
	client := &http.Client{Transport: &http.Transport{
		DialContext:       func(ctx context.Context, _, _ string) (net.Conn, error) { return d.DialContext(ctx) },
		DisableKeepAlives: true,
	}}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		s.lifetime(ctx, client)
	}()
}

func (s *shareService) lifetime(ctx context.Context, client *http.Client) {
	kick := make(chan struct{}, 1)
	events := func() error {
		filters := url.QueryEscape(`{"type":["container"],"event":["destroy"]}`)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/events?filters="+filters, nil)
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("events: %s", resp.Status)
		}
		// Containers destroyed while the stream was down.
		select {
		case kick <- struct{}{}:
		default:
		}
		br := bufio.NewReaderSize(resp.Body, maxEventLine)
		for {
			line, err := readEventLine(br)
			if err != nil {
				return err
			}
			var ev dockerEvent
			if json.Unmarshal(line, &ev) == nil && ev.Type == "container" && ev.Action == "destroy" {
				select {
				case kick <- struct{}{}:
				default:
				}
			}
		}
	}
	var follow sync.WaitGroup
	defer follow.Wait()
	follow.Add(1)
	go func() {
		defer follow.Done()
		for ctx.Err() == nil {
			if err := events(); err != nil && ctx.Err() == nil {
				s.log.Debug("shares: event stream ended", "err", err)
			}
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
		}
	}()
	tick := time.NewTicker(pruneEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-kick:
			select {
			case <-ctx.Done():
				return
			case <-time.After(s.debounce):
			}
		}
		if err := s.prune(ctx, client); err != nil && ctx.Err() == nil {
			s.log.Debug("shares: prune skipped", "err", err)
		}
	}
}

// prune lists every container and removes the shares none of them uses.
func (s *shareService) prune(ctx context.Context, client *http.Client) error {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/containers/json?all=1", nil)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("containers: %s", resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, share.MaxContainerList+1))
	if err != nil {
		return err
	}
	used, err := share.SharesInUse(b, ShareMount)
	if err != nil {
		return err
	}
	if gone := s.reg.Prune(used, start.Add(-s.grace)); len(gone) > 0 {
		s.log.Info("shares: removed unused", "shares", gone, "left", s.reg.Len())
	}
	return nil
}
