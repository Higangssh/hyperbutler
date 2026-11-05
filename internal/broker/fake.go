//go:build windows

package broker

import (
	"context"
	"crypto/rand"
	"sync"

	"github.com/Higangssh/hyperbutler/internal/vmsecret"
)

// FakeBackend is an in-memory Backend for tests and for running the broker without HCS.
// It starts no VMs. It keys every VM by owner SID and slot, like a real backend must.
type FakeBackend struct {
	mu  sync.Mutex
	vms map[VMKey]*VMStatus

	// Starts counts successful Start calls by owner (of any slot), and Specs holds the last spec each
	// owner started.
	Starts map[string]int
	Specs  map[string]VMSpec
	// Stops counts Stop calls that stopped a VM, by owner (of any slot).
	Stops map[string]int
	// Err, if set, is returned by every call.
	Err error
}

// NewFakeBackend returns an empty FakeBackend.
func NewFakeBackend() *FakeBackend {
	return &FakeBackend{vms: make(map[VMKey]*VMStatus), Starts: make(map[string]int), Specs: make(map[string]VMSpec), Stops: make(map[string]int)}
}

// StartCount and StopCount read Starts and Stops for owner under the lock, for tests that
// read them while the server is still serving.
func (f *FakeBackend) StartCount(owner string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Starts[owner]
}

func (f *FakeBackend) StopCount(owner string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Stops[owner]
}

func (f *FakeBackend) Start(_ context.Context, spec VMSpec) (VMStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return VMStatus{}, f.Err
	}
	if vm, ok := f.vms[spec.Key()]; ok {
		return *vm, nil
	}
	secret, err := vmsecret.New()
	if err != nil {
		return VMStatus{}, err
	}
	vm := &VMStatus{Running: true, CPUs: spec.CPUs, MemoryMB: spec.MemoryMB, Secret: secret}
	_, _ = rand.Read(vm.ID[:])
	vm.ID[0] |= 1 // never all zero
	f.vms[spec.Key()] = vm
	f.Starts[spec.Owner]++
	spec.DataDir = nil // not the backend's to keep
	f.Specs[spec.Owner] = spec
	return *vm, nil
}

func (f *FakeBackend) Stop(_ context.Context, key VMKey, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	if _, ok := f.vms[key]; !ok {
		return ErrNotRunning
	}
	delete(f.vms, key)
	f.Stops[key.Owner]++
	return nil
}

func (f *FakeBackend) Status(_ context.Context, key VMKey) (VMStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return VMStatus{}, f.Err
	}
	if vm, ok := f.vms[key]; ok {
		return *vm, nil
	}
	return VMStatus{}, nil
}

func (f *FakeBackend) Resize(_ context.Context, key VMKey, cpus, memoryMB uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	vm, ok := f.vms[key]
	if !ok {
		return ErrNotRunning
	}
	if cpus != 0 {
		vm.CPUs = cpus
	}
	if memoryMB != 0 {
		vm.MemoryMB = memoryMB
	}
	return nil
}

func (f *FakeBackend) Pause(_ context.Context, key VMKey, resume bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	vm, ok := f.vms[key]
	if !ok {
		return ErrNotRunning
	}
	vm.Paused = !resume
	return nil
}
