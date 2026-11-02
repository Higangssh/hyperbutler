//go:build windows

package broker

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/broker/proto"
	"github.com/Higangssh/hyperbutler/internal/transport"
)

// DefaultSharedPipe is the broker's shared endpoint, which interactive users may connect
// to (ADR 0004 rule 5).
const DefaultSharedPipe = `\\.\pipe\hyperbutler-broker`

// UserPipeName is the name of sid's own endpoint next to the shared endpoint shared:
// shared, a dash, and the hex of the first 16 bytes of SHA-256 over sid's canonical
// string form (S-1-5-21-...). Both the broker and the client derive it, and the client
// connects only to the name it derived itself.
func UserPipeName(shared, sid string) string {
	h := sha256.Sum256([]byte(sid))
	return shared + "-" + hex.EncodeToString(h[:16])
}

// EndpointsConfig configures ListenEndpoints. Zero values take the defaults.
type EndpointsConfig struct {
	// Name is the shared endpoint (default DefaultSharedPipe).
	Name string
	// Instances is the shared endpoint's instance count, UserInstances each per-user
	// endpoint's (1..16, default 4 each).
	Instances, UserInstances int
	// MaxUsers caps the per-user endpoints (default 64); a user beyond it gets "busy".
	MaxUsers int
}

// Endpoints is the broker's shared endpoint and the per-user endpoints it creates on
// request (ADR 0004 rule 5). The shared endpoint grants connect to interactive users and
// serves only the version and open-endpoint verbs; open-endpoint creates the caller's
// own pipe, UserPipeName, with FILE_FLAG_FIRST_PIPE_INSTANCE and a protected DACL (the
// broker's account full access, the caller read and write without
// FILE_CREATE_PIPE_INSTANCE, nobody else), serves every VM verb there for that caller
// only, and replies with its name. A per-user name that already exists fails the request
// with CodeEndpointSquatted; the broker never falls back to another name, and tries again
// on the next request.
type Endpoints struct {
	srv    *Server
	cfg    EndpointsConfig
	shared *PipeListener

	mu     sync.Mutex
	closed bool
	users  map[string]*PipeListener // by SID
	wg     sync.WaitGroup
}

// ListenEndpoints creates the shared endpoint for srv and makes srv answer the
// open-endpoint verb with per-user endpoints. It fails with ErrPipeExists if the shared
// name is taken. A Server takes at most one Endpoints.
func ListenEndpoints(srv *Server, cfg EndpointsConfig) (*Endpoints, error) {
	if cfg.Name == "" {
		cfg.Name = DefaultSharedPipe
	}
	if cfg.UserInstances == 0 {
		cfg.UserInstances = 4
	}
	if cfg.MaxUsers == 0 {
		cfg.MaxUsers = 64
	}
	if cfg.UserInstances < 1 || cfg.UserInstances > maxPipeInstances || cfg.MaxUsers < 1 {
		return nil, errors.New("broker: endpoint limits out of range")
	}
	// The longest per-user name must still be a valid pipe name and fit an
	// open-endpoint reply.
	if long := UserPipeName(cfg.Name, ""); len(long) > proto.MaxPipeNameLen || transport.ValidatePipeName(long) != nil {
		return nil, fmt.Errorf("broker: shared endpoint name %q is too long for per-user endpoints", cfg.Name)
	}
	e := &Endpoints{srv: srv, cfg: cfg, users: make(map[string]*PipeListener)}
	srv.mu.Lock()
	taken := srv.opener != nil
	if !taken {
		srv.opener = e.open
	}
	srv.mu.Unlock()
	if taken {
		return nil, errors.New("broker: the server already has endpoints")
	}
	l, err := ListenPipe(PipeConfig{Name: cfg.Name, Instances: cfg.Instances})
	if err != nil {
		srv.mu.Lock()
		srv.opener = nil
		srv.mu.Unlock()
		return nil, err
	}
	e.shared = l
	return e, nil
}

// Name returns the shared endpoint's name.
func (e *Endpoints) Name() string { return e.cfg.Name }

// open returns sid's own endpoint, creating it on first use.
func (e *Endpoints) open(sid string) (string, error) {
	if _, err := windows.StringToSid(sid); err != nil {
		return "", fmt.Errorf("broker: caller SID %q: %w", sid, err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return "", ErrListenerClosed
	}
	if l := e.users[sid]; l != nil {
		return l.Name(), nil
	}
	if len(e.users) >= e.cfg.MaxUsers {
		return "", errTooManyEndpoints
	}
	l, err := ListenPipe(PipeConfig{Name: UserPipeName(e.cfg.Name, sid), ClientSID: sid, Instances: e.cfg.UserInstances})
	if err != nil {
		return "", err // ErrPipeExists when squatted: reported, and retried next time
	}
	e.users[sid] = l
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		if err := e.srv.Serve(l); err != nil && !errors.Is(err, ErrListenerClosed) {
			e.srv.cfg.Logf("broker: endpoint of %s: %v", sid, err)
		}
	}()
	return l.Name(), nil
}

// Serve serves the shared endpoint until Close, then waits for the per-user endpoints'
// connections to finish. It returns nil after Close.
func (e *Endpoints) Serve() error {
	err := e.srv.Serve(e.shared)
	e.wg.Wait()
	if errors.Is(err, ErrListenerClosed) {
		return nil
	}
	return err
}

// Close closes the shared endpoint and every per-user endpoint. A connection's close
// ends its lease, as always.
func (e *Endpoints) Close() error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	users := e.users
	e.users = nil
	e.mu.Unlock()
	_ = e.shared.Close()
	for _, l := range users {
		_ = l.Close()
	}
	e.wg.Wait()
	return nil
}
