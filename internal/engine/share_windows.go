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
	"sync/atomic"
	"time"

	"github.com/Higangssh/hyperbutler/internal/guid"
	"github.com/Higangssh/hyperbutler/internal/share"
	"github.com/Higangssh/hyperbutler/internal/transport"
	"github.com/Higangssh/hyperbutler/internal/vmsecret"
	"github.com/Higangssh/hyperbutler/internal/vmtemplate"
	"github.com/Higangssh/hyperbutler/internal/winipc"
)

// SharePort is the AF_VSOCK port the guest's share relay connects to (ADR 0007).
const SharePort = vmtemplate.SharePort

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
	// neverWarn is how long run waits for a first session before it warns (tests
	// shorten it).
	neverWarn time.Duration
	// silent logs connections the guest closed without a byte (a refused secret).
	silent silentGuests

	mu     sync.Mutex
	cancel context.CancelFunc // ends run's dialing
	conns  map[net.Conn]struct{}
	closed bool
	wg     sync.WaitGroup
}

// setupShares builds the share server. It returns nil when bind mounts are off. The
// VM's template adds the share's service and kernel argument (vmtemplate.Spec.Share).
func setupShares(sc ShareConfig, log *slog.Logger) (s *shareService) {
	if sc.Off {
		return nil
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

		grace: pruneGrace, debounce: pruneDebounce, neverWarn: shareNeverWarn,
	}
	return s
}

// shareQueues is how many connections the engine opens to the guest's share relay,
// which mounts once it has that many (hyperbutlerd -share-queues, 4 by default).
const shareQueues = 4

// connect keeps the guest relay's connections open on the VM's hvsock address. Each
// begins with secret, the VM's (ADR 0004 rule 6), which the guest checks before it
// mounts; the VM's share service admits connections from the owner only.
//
// The host dials, as for the network link: Linux hv_sock gives the connections a guest
// listener accepts VMBus rings sized from net.core.[rw]mem_default (the relay asks for
// hv_sock's 256 KiB maximum), against 24 KiB per direction for a connection the guest
// dials, round which every 1 MiB write went 43 times (ADR 0006, ADR 0007).
func (s *shareService) connect(vmid guid.GUID, secret vmsecret.Secret) {
	d := &winipc.HvsockDialer{}
	addr := winipc.HvsockAddr{VMID: vmid, ServiceID: s.svc}
	s.run(func(ctx context.Context) (net.Conn, error) {
		// A connect to a port nobody listens on yet stays pending for about 30 s; bound
		// each attempt, as dialGuest does, so the relay's listener is found soon after
		// it appears.
		actx, cancel := context.WithTimeout(ctx, shareDialTimeout)
		defer cancel()
		return d.Dial(actx, addr)
	}, secret)
}

// shareDialTimeout bounds one connect to the guest's relay.
const shareDialTimeout = 250 * time.Millisecond

// run dials shareQueues connections with dial, sends secret on each and serves it with
// share.Server.Serve, and when they have all ended (the guest's relay session ended, or
// it refused them) dials again, until close. The secret goes first, before the guest's
// HELLO: the guest reads it before it writes anything.
func (s *shareService) run(dial func(context.Context) (net.Conn, error), secret vmsecret.Secret) {
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		cancel()
		return
	}
	s.cancel = cancel
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		sleep := func(d time.Duration) bool {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-t.C:
				return true
			case <-ctx.Done():
				return false
			}
		}
		start := time.Now()
		var dialBackoff, sessionBackoff time.Duration
		var everSpoke, warned bool
		// notYet warns once when the relay has never started a session long after the
		// engine began dialing: a guest image without the listening relay (older than
		// the engine) or with bind mounts off. Until then a failure is debug only.
		notYet := func(err error) {
			if everSpoke || warned || time.Since(start) < s.neverWarn {
				s.log.Debug("guest share relay not reachable yet", "err", err)
				return
			}
			warned = true
			s.log.Warn("the guest's share relay has not connected; bind mounts of Windows paths will fail (is the guest image older than the engine?)",
				"after", time.Since(start).Round(time.Second), "err", err)
		}
		for ctx.Err() == nil {
			var session sync.WaitGroup
			var spoke atomic.Bool
			for n := 0; n < shareQueues && ctx.Err() == nil; {
				c, err := dial(ctx)
				if err != nil {
					if ctx.Err() != nil {
						break
					}
					// The guest is still booting, or its relay is between sessions.
					notYet(err)
					dialBackoff = nextAcceptBackoff(dialBackoff)
					if !sleep(dialBackoff) {
						break
					}
					continue
				}
				dialBackoff = 0
				if !s.track(c) {
					break
				}
				n++
				session.Add(1)
				go func() {
					defer session.Done()
					if s.serveConn(c, secret) {
						spoke.Store(true)
					}
				}()
			}
			session.Wait()
			// A session in which the guest spoke resets the wait; one it refused or
			// dropped at once (an image that does not take the secret, or wants another
			// number of connections) backs off up to shareMaxBackoff, as the relay did
			// when it dialed.
			if spoke.Load() {
				everSpoke, sessionBackoff = true, 0
			} else {
				notYet(errors.New("the guest closed every connection before its HELLO"))
			}
			sessionBackoff = min(max(2*sessionBackoff, shareDialTimeout), shareMaxBackoff)
			if !sleep(sessionBackoff) {
				return
			}
		}
	}()
}

// shareMaxBackoff bounds the wait between sessions the guest refused.
const shareMaxBackoff = 5 * time.Second

// track registers c for close, or closes it and reports false when the service is
// closed.
func (s *shareService) track(c net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		_ = c.Close()
		return false
	}
	s.conns[c] = struct{}{}
	return true
}

// serveConn sends secret on c and serves it until it ends. It reports whether the guest
// spoke (sent its HELLO).
func (s *shareService) serveConn(c net.Conn, secret vmsecret.Secret) bool {
	gc := &guestConn{Conn: c}
	err := transport.SendSecret(c, secret)
	sent := err == nil
	if sent {
		err = s.srv.Serve(gc)
	}
	s.mu.Lock()
	delete(s.conns, c)
	closed := s.closed
	s.mu.Unlock()
	_ = c.Close()
	switch {
	case closed || errors.Is(err, net.ErrClosed):
	case !sent:
		s.log.Warn("guest share connection refused", "err", err)
	case !gc.spoke.Load():
		// Before the HELLO: Serve reports a clean EOF as nil.
		s.silent.log(s.log, "guest share connection ended before the guest's HELLO", err)
	case err != nil:
		s.log.Warn("guest share connection ended", "err", err)
	}
	return gc.spoke.Load()
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
	if s.cancel != nil {
		s.cancel()
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
		defer func() { _ = resp.Body.Close() }()
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
		// A stream that keeps failing at once (each (re)open kicks a full container
		// list) backs off to a minute.
		var backoff time.Duration
		for ctx.Err() == nil {
			t0 := time.Now()
			if err := events(); err != nil && ctx.Err() == nil {
				s.log.Debug("shares: event stream ended", "err", err)
			}
			if time.Since(t0) > time.Minute {
				backoff = 0
			}
			backoff = min(max(2*backoff, time.Second), time.Minute)
			select {
			case <-ctx.Done():
			case <-time.After(backoff):
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
	defer func() { _ = resp.Body.Close() }()
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

// shareNeverWarn is how long the engine dials without a session before it warns that
// the guest's relay never connected.
const shareNeverWarn = 30 * time.Second
