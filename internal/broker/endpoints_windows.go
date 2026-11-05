//go:build windows

package broker

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/broker/proto"
	"github.com/Higangssh/hyperbutler/internal/hvaddr"
)

// DefaultSharedPipe is the broker's shared endpoint, which interactive users may connect
// to (ADR 0004 rule 5).
const DefaultSharedPipe = `\\.\pipe\hyperbutler-broker`

// userPipeSuffixLen is the length of the random hex suffix of a per-user pipe name.
const userPipeSuffixLen = 16

// userPipePrefix is the fixed part of sid's own endpoint next to the shared endpoint
// shared: shared, a dash, and the hex of the first 16 bytes of SHA-256 over sid's
// canonical string form (S-1-5-21-...), then a dash.
func userPipePrefix(shared, sid string) string {
	h := sha256.Sum256([]byte(sid))
	return shared + "-" + hex.EncodeToString(h[:16]) + "-"
}

// newUserPipeName is a fresh name for sid's own endpoint: userPipePrefix and 16 random
// lowercase hex digits, new for each endpoint the broker creates. Another user can
// compute the prefix but not the suffix, so cannot create the name before the broker
// does (ADR 0004 rule 5).
func newUserPipeName(shared, sid string) (string, error) {
	var b [userPipeSuffixLen / 2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return userPipePrefix(shared, sid) + hex.EncodeToString(b[:]), nil
}

// ValidUserPipeName reports whether name has the form of sid's own endpoint next to
// shared: userPipePrefix and exactly 16 lowercase hex digits. The client connects to
// nothing else.
func ValidUserPipeName(shared, sid, name string) bool {
	suffix, ok := strings.CutPrefix(name, userPipePrefix(shared, sid))
	if !ok || len(suffix) != userPipeSuffixLen {
		return false
	}
	for i := 0; i < len(suffix); i++ {
		if c := suffix[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// EndpointsConfig configures ListenEndpoints. Zero values take the defaults.
type EndpointsConfig struct {
	// Name is the shared endpoint (default DefaultSharedPipe).
	Name string
	// Instances is the shared endpoint's instance count (1..16, default 16), and
	// UserInstances each per-user endpoint's (default 4).
	Instances, UserInstances int
	// MaxUsers caps the per-user endpoints (default 64). At the cap, endpoints with no
	// connection and no running VM are retired; if none is, a new user gets "busy".
	MaxUsers int
}

// Endpoints is the broker's shared endpoint and the per-user endpoints it creates on
// request (ADR 0004 rule 5). The shared endpoint grants connect to interactive users and
// serves only the version and open-endpoint verbs; open-endpoint creates the caller's
// own pipe, under a new name with a random part (newUserPipeName), with
// FILE_FLAG_FIRST_PIPE_INSTANCE and a protected DACL (the broker's account full access,
// the caller read and write without FILE_CREATE_PIPE_INSTANCE, nobody else), serves
// every VM verb there for that caller only, and replies with its name. A name that
// already exists fails the request with CodeEndpointSquatted; the broker never falls
// back to another name, and the next request tries a new one.
type Endpoints struct {
	srv    *Server
	cfg    EndpointsConfig
	shared *PipeListener
	// newName replaces newUserPipeName in tests.
	newName func(shared, sid string) (string, error)

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
	if cfg.Instances == 0 {
		cfg.Instances = maxPipeInstances
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
	if long := userPipePrefix(cfg.Name, "") + strings.Repeat("0", userPipeSuffixLen); len(long) > proto.MaxPipeNameLen || hvaddr.ValidatePipeName(long) != nil {
		return nil, fmt.Errorf("broker: shared endpoint name %q is too long for per-user endpoints", cfg.Name)
	}
	e := &Endpoints{srv: srv, cfg: cfg, newName: newUserPipeName, users: make(map[string]*PipeListener)}
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
		e.retireIdleLocked()
		if len(e.users) >= e.cfg.MaxUsers {
			return "", errTooManyEndpoints
		}
	}
	name, err := e.newName(e.cfg.Name, sid)
	if err != nil {
		return "", err
	}
	l, err := ListenPipe(PipeConfig{Name: name, ClientSID: sid, Instances: e.cfg.UserInstances})
	if err != nil {
		return "", err // ErrPipeExists when squatted: reported, and a new name next time
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

// retireIdleLocked closes the per-user endpoints that have no identified connection and
// whose owner has no running VM (a VM's lease is a connection, so this is a second
// check). Their users get a new endpoint, under a new name, when they ask again; a client
// that got the old name just before may fail to connect once. e.mu is held.
func (e *Endpoints) retireIdleLocked() {
	for sid, l := range e.users {
		e.srv.mu.Lock()
		busy := e.srv.conns[sid] > 0
		e.srv.mu.Unlock()
		if busy {
			continue
		}
		if e.anyRunning(sid) {
			continue
		}
		_ = l.Close()
		delete(e.users, sid)
		e.srv.cfg.Logf("broker: retired the idle endpoint of %s", sid)
	}
}

// anyRunning reports whether sid has a running VM in any slot, or whether that cannot
// be told (an endpoint is kept rather than retired under a VM).
func (e *Endpoints) anyRunning(sid string) bool {
	for slot := 0; slot <= proto.MaxSlot; slot++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		st, err := e.srv.cfg.Backend.Status(ctx, VMKey{Owner: sid, Slot: uint8(slot)}) //nolint:gosec // G115: slot <= proto.MaxSlot
		cancel()
		if err != nil || st.Running {
			return true
		}
	}
	return false
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
