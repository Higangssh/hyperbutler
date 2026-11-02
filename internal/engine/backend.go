package engine

import (
	"github.com/Microsoft/go-winio/pkg/guid"

	"github.com/Higangssh/hyperbutler/internal/vm"
)

// Backend is every privileged call the engine makes. HCSBackend calls HCS directly,
// which works for members of Hyper-V Administrators. The VM document it is given is
// built by internal/vmtemplate, which the HCS access broker of ADR 0004 uses too.
type Backend interface {
	// Create creates (but does not start) a compute system.
	Create(id string, cfg *vm.Config) (Machine, error)
	// Open opens an existing compute system, to clean it up.
	Open(id string) (Machine, error)
	// List lists the compute systems with this HCS owner.
	List(owner string) ([]vm.Summary, error)
	// GrantAccess lets the worker process of compute system id open path.
	GrantAccess(id, path string) error
	// RuntimeIDOf is the runtime id compute system id will have, before it exists: its
	// VM account (vm.AccountSID) is what GrantAccess adds to a file.
	RuntimeIDOf(id string) (guid.GUID, error)
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
