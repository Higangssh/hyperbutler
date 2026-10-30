package main

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

var (
	modKernel32  = syscall.NewLazyDLL("kernel32.dll")
	modVirtdisk  = syscall.NewLazyDLL("virtdisk.dll")
	procGetCFS   = modKernel32.NewProc("GetCompressedFileSizeW")
	procSetLastE = modKernel32.NewProc("SetLastError")
	procCreateVD = modVirtdisk.NewProc("CreateVirtualDisk")
	procOpenVD   = modVirtdisk.NewProc("OpenVirtualDisk")
	procCompact  = modVirtdisk.NewProc("CompactVirtualDisk")
	procAttachVD = modVirtdisk.NewProc("AttachVirtualDisk")
	procDetachVD = modVirtdisk.NewProc("DetachVirtualDisk")
)

const (
	fsctlSetSparse             = 0x000900C4
	fsctlSetZeroData           = 0x000980C8
	fsctlQueryAllocatedRanges  = 0x000940CF
	fileAttributeSparseFile    = 0x200
	errorMoreData              = syscall.Errno(234)
	virtualStorageTypeVHDX     = 3
	createFlagNone             = 0x0
	createFlagSparseFile       = 0x80
	createFlagSparseAnyFS      = 0x400 // CREATE_VIRTUAL_DISK_FLAG_SUPPORT_SPARSE_FILES_ANY_FS
	openFlagSparseAnyFS        = 0x400 // OPEN_VIRTUAL_DISK_FLAG_SUPPORT_SPARSE_FILES_ANY_FS
	accessNone                 = 0x0
	accessAttachRO             = 0x00010000
	accessMetaOps              = 0x00200000
	attachFlagReadOnly         = 0x1
	attachFlagNoDriveLetter    = 0x2
	defaultVHDXLogicalSector   = 512
	defaultVHDXPhysicalSector  = 4096
	openVirtualDiskVersion1    = 1
	createVirtualDiskVersion2  = 2
	compactVirtualDiskVersion1 = 1
	attachVirtualDiskVersion1  = 1
)

// vhdxBlockSize is the VHDX block size for createVHDX; 0 means the provider default (32 MiB).
// The hcs subcommand sets it with -block. WSL uses 1 MiB.
var vhdxBlockSize uint32

type guid struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

// VIRTUAL_STORAGE_TYPE_VENDOR_MICROSOFT {EC984AEC-A0F9-47e9-901F-71415A66345B}
var vendorMicrosoft = guid{0xEC984AEC, 0xA0F9, 0x47e9, [8]byte{0x90, 0x1F, 0x71, 0x41, 0x5A, 0x66, 0x34, 0x5B}}

type virtualStorageType struct {
	DeviceID uint32
	VendorID guid
}

// createVersion2 mirrors CREATE_VIRTUAL_DISK_PARAMETERS.Version2. It contains a uint64, so
// Go aligns it to 8 inside createParams, which matches the C union layout on amd64.
type createVersion2 struct {
	UniqueID                  guid
	MaximumSize               uint64
	BlockSizeInBytes          uint32
	SectorSizeInBytes         uint32
	PhysicalSectorSizeInBytes uint32
	ParentPath                *uint16
	SourcePath                *uint16
	OpenFlags                 uint32
	ParentVirtualStorageType  virtualStorageType
	SourceVirtualStorageType  virtualStorageType
	ResiliencyGUID            guid
}

type createParams struct {
	Version uint32
	V2      createVersion2
}

type openParamsV1 struct {
	Version uint32
	RWDepth uint32
}

type compactParams struct {
	Version  uint32
	Reserved uint32
}

type attachParams struct {
	Version  uint32
	Reserved uint32
}

func errno(r uintptr) error {
	if r == 0 {
		return nil
	}
	return syscall.Errno(r)
}

func createVHDX(path string, size uint64, flags uint32) error {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	vst := virtualStorageType{DeviceID: virtualStorageTypeVHDX, VendorID: vendorMicrosoft}
	params := createParams{Version: createVirtualDiskVersion2}
	params.V2.MaximumSize = size
	params.V2.BlockSizeInBytes = vhdxBlockSize
	params.V2.SectorSizeInBytes = defaultVHDXLogicalSector
	params.V2.PhysicalSectorSizeInBytes = defaultVHDXPhysicalSector
	var h syscall.Handle
	r, _, _ := procCreateVD.Call(
		uintptr(unsafe.Pointer(&vst)),
		uintptr(unsafe.Pointer(p)),
		accessNone,
		0, // security descriptor
		uintptr(flags),
		0, // provider-specific flags
		uintptr(unsafe.Pointer(&params)),
		0, // overlapped
		uintptr(unsafe.Pointer(&h)),
	)
	if err := errno(r); err != nil {
		return fmt.Errorf("CreateVirtualDisk(flags=0x%x): %w", flags, err)
	}
	return syscall.CloseHandle(h)
}

func openVHDX(path string, access, flags uint32) (syscall.Handle, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	vst := virtualStorageType{DeviceID: virtualStorageTypeVHDX, VendorID: vendorMicrosoft}
	params := openParamsV1{Version: openVirtualDiskVersion1, RWDepth: 1}
	var h syscall.Handle
	r, _, _ := procOpenVD.Call(
		uintptr(unsafe.Pointer(&vst)),
		uintptr(unsafe.Pointer(p)),
		uintptr(access),
		uintptr(flags),
		uintptr(unsafe.Pointer(&params)),
		uintptr(unsafe.Pointer(&h)),
	)
	if err := errno(r); err != nil {
		return 0, fmt.Errorf("OpenVirtualDisk(access=0x%x, flags=0x%x): %w", access, flags, err)
	}
	return h, nil
}

func compactVHDX(h syscall.Handle) error {
	params := compactParams{Version: compactVirtualDiskVersion1}
	r, _, _ := procCompact.Call(uintptr(h), 0, uintptr(unsafe.Pointer(&params)), 0)
	if err := errno(r); err != nil {
		return fmt.Errorf("CompactVirtualDisk: %w", err)
	}
	return nil
}

func attachVHDX(h syscall.Handle, flags uint32) error {
	params := attachParams{Version: attachVirtualDiskVersion1}
	r, _, _ := procAttachVD.Call(uintptr(h), 0, uintptr(flags), 0, uintptr(unsafe.Pointer(&params)), 0)
	if err := errno(r); err != nil {
		return fmt.Errorf("AttachVirtualDisk(flags=0x%x): %w", flags, err)
	}
	return nil
}

func detachVHDX(h syscall.Handle) error {
	r, _, _ := procDetachVD.Call(uintptr(h), 0, 0)
	return errno(r)
}

// usage is what the host volume actually pays for a file.
type usage struct {
	Size      int64 // end of file
	Allocated int64 // GetCompressedFileSizeW: bytes allocated on disk for sparse/compressed files
	Sparse    bool
}

func (u usage) String() string {
	return fmt.Sprintf("size=%s allocated=%s sparse=%v", mib(u.Size), mib(u.Allocated), u.Sparse)
}

func mib(n int64) string { return fmt.Sprintf("%.1fMiB", float64(n)/(1<<20)) }

func measure(path string) (usage, error) {
	st, err := os.Stat(path)
	if err != nil {
		return usage{}, err
	}
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return usage{}, err
	}
	// GetCompressedFileSizeW does not clear the last error on success, and a low DWORD of
	// 0xFFFFFFFF is a valid size, so clear it first and check it only in that case.
	var high uint32
	procSetLastE.Call(0)
	low, _, callErr := procGetCFS.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&high)))
	if uint32(low) == 0xFFFFFFFF && callErr != nil && callErr != syscall.Errno(0) {
		return usage{}, fmt.Errorf("GetCompressedFileSizeW: %w", callErr)
	}
	attrs, err := syscall.GetFileAttributes(p)
	if err != nil {
		return usage{}, err
	}
	return usage{
		Size:      st.Size(),
		Allocated: int64(high)<<32 | int64(uint32(low)),
		Sparse:    attrs&fileAttributeSparseFile != 0,
	}, nil
}

func setSparse(f *os.File) error {
	var ret uint32
	err := syscall.DeviceIoControl(syscall.Handle(f.Fd()), fsctlSetSparse, nil, 0, nil, 0, &ret, nil)
	if err != nil {
		return fmt.Errorf("FSCTL_SET_SPARSE: %w", err)
	}
	return nil
}

type fileZeroDataInformation struct {
	FileOffset      int64
	BeyondFinalZero int64
}

// zeroData asks NTFS to zero [off, off+n). On a sparse file NTFS deallocates the whole
// allocation units inside the range and writes zeros into the partial ones at the edges.
func zeroData(f *os.File, off, n int64) error {
	in := fileZeroDataInformation{FileOffset: off, BeyondFinalZero: off + n}
	var ret uint32
	err := syscall.DeviceIoControl(syscall.Handle(f.Fd()), fsctlSetZeroData,
		(*byte)(unsafe.Pointer(&in)), uint32(unsafe.Sizeof(in)), nil, 0, &ret, nil)
	if err != nil {
		return fmt.Errorf("FSCTL_SET_ZERO_DATA(%d,%d): %w", off, n, err)
	}
	return nil
}

type allocatedRange struct {
	FileOffset int64
	Length     int64
}

func allocatedRanges(f *os.File, size int64) ([]allocatedRange, error) {
	in := allocatedRange{0, size}
	out := make([]allocatedRange, 1024)
	var ret uint32
	err := syscall.DeviceIoControl(syscall.Handle(f.Fd()), fsctlQueryAllocatedRanges,
		(*byte)(unsafe.Pointer(&in)), uint32(unsafe.Sizeof(in)),
		(*byte)(unsafe.Pointer(&out[0])), uint32(len(out))*uint32(unsafe.Sizeof(out[0])), &ret, nil)
	if err != nil && !errors.Is(err, errorMoreData) {
		return nil, fmt.Errorf("FSCTL_QUERY_ALLOCATED_RANGES: %w", err)
	}
	return out[:ret/uint32(unsafe.Sizeof(out[0]))], nil
}
