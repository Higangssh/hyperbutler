//go:build windows

package broker

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/vm"
)

// The CREATE_VIRTUAL_DISK_PARAMETERS version 2 layout (as internal/vm has it), for a
// differencing child of an existing VHDX.
type testStorageType struct {
	DeviceID uint32
	VendorID windows.GUID
}

type testCreateV2 struct {
	UniqueID                  windows.GUID
	MaximumSize               uint64
	BlockSizeInBytes          uint32
	SectorSizeInBytes         uint32
	PhysicalSectorSizeInBytes uint32
	ParentPath                *uint16
	SourcePath                *uint16
	OpenFlags                 uint32
	ParentVirtualStorageType  testStorageType
	SourceVirtualStorageType  testStorageType
	ResiliencyGUID            windows.GUID
}

type testCreateParams struct {
	Version uint32
	V2      testCreateV2
}

func createVHDX(t *testing.T, path, parent string) {
	t.Helper()
	vendor := windows.GUID{Data1: 0xEC984AEC, Data2: 0xA0F9, Data3: 0x47e9, Data4: [8]byte{0x90, 0x1F, 0x71, 0x41, 0x5A, 0x66, 0x34, 0x5B}}
	vst := testStorageType{DeviceID: 3, VendorID: vendor}
	pc, _ := windows.UTF16PtrFromString(path)
	params := testCreateParams{Version: 2}
	if parent != "" {
		params.V2.ParentPath, _ = windows.UTF16PtrFromString(parent)
	} else {
		params.V2.MaximumSize = 64 << 20
	}
	var h windows.Handle
	r, _, _ := windows.NewLazySystemDLL("virtdisk.dll").NewProc("CreateVirtualDisk").Call(
		uintptr(unsafe.Pointer(&vst)), uintptr(unsafe.Pointer(pc)), 0, 0, 0, 0,
		uintptr(unsafe.Pointer(&params)), 0, uintptr(unsafe.Pointer(&h)))
	if r != 0 {
		t.Skipf("CreateVirtualDisk %s: %v", path, windows.Errno(r))
	}
	_ = windows.CloseHandle(h)
}

// Disks Windows itself writes: the sparse dynamic VHDX the engine creates and a plain one
// pass, and a differencing child of the plain one is refused (read through a
// file handle, as the broker reads the data disk).
func TestCheckVHDXOnRealDisks(t *testing.T) {
	dir := t.TempDir()
	parent := filepath.Join(dir, "parent.vhdx")
	if err := vm.CreateSparseVHDX(parent, 64<<20); err != nil {
		t.Skipf("create a VHDX: %v", err)
	}
	plain := filepath.Join(dir, "plain.vhdx")
	createVHDX(t, plain, "")
	child := filepath.Join(dir, "child.vhdx")
	createVHDX(t, child, plain)
	check := func(p string) error {
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		return checkVHDX(handleReader{windows.Handle(f.Fd())})
	}
	if err := check(parent); err != nil {
		t.Fatalf("the engine's own kind of disk: %v", err)
	}
	if err := check(plain); err != nil {
		t.Fatalf("a plain dynamic VHDX: %v", err)
	}
	if err := check(child); !errors.Is(err, errDifferencing) {
		t.Fatalf("a differencing disk: %v", err)
	}
}
