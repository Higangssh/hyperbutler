//go:build windows

package vm

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The data disk of ADR 0002: a dynamic VHDX with 1 MiB blocks that is an NTFS sparse
// file, so blocks the guest trims are handed back to the host volume while it runs
// (docs/research/disk-reclaim.md). Creating one needs no elevation.

var (
	modVirtdisk             = windows.NewLazySystemDLL("virtdisk.dll")
	procCreateVirtualDisk   = modVirtdisk.NewProc("CreateVirtualDisk")
	modKernel32             = windows.NewLazySystemDLL("kernel32.dll")
	procGetCompressedFileSz = modKernel32.NewProc("GetCompressedFileSizeW")
	procSetLastError        = modKernel32.NewProc("SetLastError")
)

const (
	virtualStorageTypeVHDX    = 3
	createVirtualDiskV2       = 2
	createFlagNone            = 0
	vhdxLogicalSector         = 512
	vhdxPhysicalSector        = 4096
	fsctlSetSparse            = 0x000900C4
	fileAttributeSparseFile   = 0x200
	fileAttributeReparsePoint = 0x400

	// DataDiskBlockSize is the VHDX block size of the data disk. With the 32 MiB
	// default, free runs shorter than a block are never returned (ADR 0002).
	DataDiskBlockSize = 1 << 20
)

// VIRTUAL_STORAGE_TYPE_VENDOR_MICROSOFT {EC984AEC-A0F9-47e9-901F-71415A66345B}
var vendorMicrosoft = windows.GUID{Data1: 0xEC984AEC, Data2: 0xA0F9, Data3: 0x47e9, Data4: [8]byte{0x90, 0x1F, 0x71, 0x41, 0x5A, 0x66, 0x34, 0x5B}}

type virtualStorageType struct {
	DeviceID uint32
	VendorID windows.GUID
}

// createVersion2 mirrors CREATE_VIRTUAL_DISK_PARAMETERS.Version2. It holds a uint64, so
// Go aligns it to 8 inside createParams, matching the C union on amd64 and arm64.
type createVersion2 struct {
	UniqueID                  windows.GUID
	MaximumSize               uint64
	BlockSizeInBytes          uint32
	SectorSizeInBytes         uint32
	PhysicalSectorSizeInBytes uint32
	ParentPath                *uint16
	SourcePath                *uint16
	OpenFlags                 uint32
	ParentVirtualStorageType  virtualStorageType
	SourceVirtualStorageType  virtualStorageType
	ResiliencyGUID            windows.GUID
}

type createParams struct {
	Version uint32
	V2      createVersion2
}

// CreateSparseVHDX creates a new dynamic VHDX of the given virtual size with 1 MiB
// blocks at path, and marks it an NTFS sparse file. It fails if path exists. Attach it
// with Disk{Sparse: true}.
func CreateSparseVHDX(path string, size uint64) error {
	if size < 64<<20 || size%DataDiskBlockSize != 0 {
		return fmt.Errorf("vm: VHDX size %d must be a multiple of 1 MiB and at least 64 MiB", size)
	}
	if err := modVirtdisk.Load(); err != nil {
		return fmt.Errorf("vm: load virtdisk.dll: %w", err)
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	vst := virtualStorageType{DeviceID: virtualStorageTypeVHDX, VendorID: vendorMicrosoft}
	params := createParams{Version: createVirtualDiskV2}
	params.V2.MaximumSize = size
	params.V2.BlockSizeInBytes = DataDiskBlockSize
	params.V2.SectorSizeInBytes = vhdxLogicalSector
	params.V2.PhysicalSectorSizeInBytes = vhdxPhysicalSector
	var h windows.Handle
	r, _, _ := procCreateVirtualDisk.Call(
		uintptr(unsafe.Pointer(&vst)),
		uintptr(unsafe.Pointer(p)),
		0, // VIRTUAL_DISK_ACCESS_NONE: required with version 2 parameters
		0, // security descriptor: inherit from the directory
		createFlagNone,
		0, // provider-specific flags
		uintptr(unsafe.Pointer(&params)),
		0, // overlapped
		uintptr(unsafe.Pointer(&h)),
	)
	if r != 0 {
		return fmt.Errorf("vm: CreateVirtualDisk %s: %w", path, windows.Errno(r))
	}
	if err := windows.CloseHandle(h); err != nil {
		return err
	}
	// FSCTL_SET_SPARSE works without admin on an existing VHDX; CreateVirtualDisk's own
	// sparse flag needs SUPPORT_SPARSE_FILES_ANY_FS as well (disk-reclaim.md, Q1). This
	// is the path the HCS measurements used.
	f, err := os.OpenFile(path, os.O_RDWR, 0) //nolint:gosec // G304: the VHDX this function just created
	if err != nil {
		return err
	}
	var ret uint32
	err = windows.DeviceIoControl(windows.Handle(f.Fd()), fsctlSetSparse, nil, 0, nil, 0, &ret, nil)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("vm: FSCTL_SET_SPARSE %s: %w", path, err)
	}
	return nil
}

// FileUsage is what a file costs the host volume.
type FileUsage struct {
	// Size is the end of file.
	Size uint64
	// Allocated is the bytes allocated on disk (GetCompressedFileSizeW): for a sparse
	// VHDX, what the guest's data actually takes.
	Allocated uint64
	Sparse    bool
}

// Usage measures path. It refuses reparse points, so a link planted in place of the
// disk is not measured (or used) as if it were the disk.
func Usage(path string) (FileUsage, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return FileUsage{}, err
	}
	var fa windows.Win32FileAttributeData
	if err := windows.GetFileAttributesEx(p, windows.GetFileExInfoStandard, (*byte)(unsafe.Pointer(&fa))); err != nil {
		return FileUsage{}, &os.PathError{Op: "GetFileAttributesEx", Path: path, Err: err}
	}
	if fa.FileAttributes&fileAttributeReparsePoint != 0 {
		return FileUsage{}, fmt.Errorf("vm: %s is a reparse point", path)
	}
	if fa.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		return FileUsage{}, fmt.Errorf("vm: %s is a directory", path)
	}
	// GetCompressedFileSizeW does not clear the last error on success, and a low DWORD of
	// 0xFFFFFFFF is a valid size, so check the error only in that case.
	var high uint32
	procSetLastError.Call(0)
	low, _, callErr := procGetCompressedFileSz.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&high)))
	if dword(low) == 0xFFFFFFFF && callErr != nil && !errors.Is(callErr, windows.Errno(0)) {
		return FileUsage{}, fmt.Errorf("vm: GetCompressedFileSizeW %s: %w", path, callErr)
	}
	return FileUsage{
		Size:      uint64(fa.FileSizeHigh)<<32 | uint64(fa.FileSizeLow),
		Allocated: uint64(high)<<32 | uint64(dword(low)),
		Sparse:    fa.FileAttributes&fileAttributeSparseFile != 0,
	}, nil
}

// FreeBytes is the free space of the volume holding dir that is available to the caller
// (GetDiskFreeSpaceEx, which honors quotas).
func FreeBytes(dir string) (uint64, error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, err
	}
	var avail, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &total, &free); err != nil {
		return 0, fmt.Errorf("vm: GetDiskFreeSpaceEx %s: %w", dir, err)
	}
	return avail, nil
}
