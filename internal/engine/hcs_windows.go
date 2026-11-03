//go:build windows

package engine

import (
	"github.com/Higangssh/hyperbutler/internal/guid"
	"github.com/Higangssh/hyperbutler/internal/vm"
)

// HCSBackend calls HCS (computecore.dll) directly from this process.
type HCSBackend struct{}

func (HCSBackend) Create(id string, cfg *vm.Config) (Machine, error) {
	s, err := vm.Create(id, cfg)
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (HCSBackend) Open(id string) (Machine, error) {
	s, err := vm.Open(id)
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (HCSBackend) List(owner string) ([]vm.Summary, error)  { return vm.List(owner) }
func (HCSBackend) GrantAccess(id, path string) error        { return vm.GrantVMAccess(id, path) }
func (HCSBackend) RevokeAccess(id, path string) error       { return vm.RevokeVMAccess(id, path) }
func (HCSBackend) RuntimeIDOf(id string) (guid.GUID, error) { return vm.ProbeRuntimeID(id) }
