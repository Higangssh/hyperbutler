package engine

import (
	"github.com/Microsoft/go-winio/pkg/guid"

	"github.com/Higangssh/winbox/internal/vm"
)

// Backend is every privileged call the engine makes. HCSBackend calls HCS directly,
// which works for members of Hyper-V Administrators. The HCS access broker of ADR 0004
// is meant to implement it later: it would build the VM document from its own template
// instead of trusting cfg's paths, and hand back a Machine that forwards over its pipe.
type Backend interface {
	// Create creates (but does not start) a compute system.
	Create(id string, cfg *vm.Config) (Machine, error)
	// Open opens an existing compute system, to clean it up.
	Open(id string) (Machine, error)
	// List lists the compute systems with this HCS owner.
	List(owner string) ([]vm.Summary, error)
	// GrantAccess lets the worker process of compute system id open path.
	GrantAccess(id, path string) error
	// RevokeAccess undoes GrantAccess.
	RevokeAccess(id, path string) error
}

// Machine is one compute system.
type Machine interface {
	Start() error
	RuntimeID() (guid.GUID, error)
	// State is the HCS state: "Running", "Paused", "Stopped", ...
	State() (string, error)
	Pause() error
	Resume() error
	// ShutDown asks the guest to power off; the VM stops when it has.
	ShutDown() error
	// Terminate stops the VM at once.
	Terminate() error
	Close() error
}
