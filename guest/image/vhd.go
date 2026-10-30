// Package image holds the pieces of the guest image that both the build and the engine
// need: the fixed-VHD footer that turns the raw erofs system image into a disk HCS can
// attach, and the image manifest (image.json) that ties the kernel, the initramfs and
// the system disk of one build together (ADR 0005).
package image

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

// A fixed VHD (the original VHD format, not VHDX) is the raw disk content followed by
// a 512-byte footer. HCS attaches .vhd files as VirtualDisk, so the erofs image only
// needs this footer appended; the guest sees the raw bytes and never the footer.
// Layout and checksum: "Virtual Hard Disk Image Format Specification" v1.0 (Microsoft,
// 2006), section "Hard Disk Footer Format".

// FooterSize is the size of a VHD footer.
const FooterSize = 512

// DiskAlign is the alignment of the disk content: whole MiB, which every VHD consumer
// accepts.
const DiskAlign = 1 << 20

// MaxDiskSize bounds a system disk: far more than any system image, and below the
// 2040 GiB limit of the VHD format.
const MaxDiskSize = 64 << 30

const (
	vhdCookie     = "conectix"
	vhdFeatures   = 0x00000002 // "reserved", must be set
	vhdVersion    = 0x00010000
	vhdFixedType  = 2
	vhdNoOffset   = ^uint64(0) // fixed disks have no dynamic header
	vhdCreatorApp = "wbox"
	vhdCreatorVer = 0x00010000
	vhdCreatorOS  = "Wi2k"
)

// vhdEpoch is the VHD timestamp origin.
var vhdEpoch = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// Footer is the decoded part of a fixed-VHD footer that matters here.
type Footer struct {
	Size      uint64 // disk size in bytes (original and current size are equal)
	Timestamp time.Time
	UniqueID  [16]byte
	Cylinders uint16
	Heads     uint8
	Sectors   uint8
}

// Geometry returns the CHS geometry the VHD specification derives from a size
// (appendix "CHS Calculation").
func Geometry(size uint64) (cyl uint16, heads, spt uint8) {
	ts := size / 512
	if ts > 65535*16*255 {
		ts = 65535 * 16 * 255
	}
	var h, s, cth uint64
	if ts >= 65535*16*63 {
		s, h = 255, 16
		cth = ts / s
	} else {
		s = 17
		cth = ts / s
		h = (cth + 1023) / 1024
		if h < 4 {
			h = 4
		}
		if cth >= h*1024 || h > 16 {
			s, h = 31, 16
			cth = ts / s
		}
		if cth >= h*1024 {
			s, h = 63, 16
			cth = ts / s
		}
	}
	return uint16(cth / h), uint8(h), uint8(s)
}

// EncodeFooter builds the footer of a fixed VHD whose content is size bytes. size must
// be a positive multiple of DiskAlign and at most MaxDiskSize. The timestamp is
// clamped to what the format can hold, so a given input always gives the same footer.
func EncodeFooter(size uint64, ts time.Time, id [16]byte) ([FooterSize]byte, error) {
	var b [FooterSize]byte
	if size == 0 || size%DiskAlign != 0 || size > MaxDiskSize {
		return b, fmt.Errorf("vhd: size %d is not a positive multiple of %d up to %d", size, DiskAlign, uint64(MaxDiskSize))
	}
	secs := ts.Sub(vhdEpoch) / time.Second
	if ts.Before(vhdEpoch) {
		secs = 0
	}
	if secs > 1<<32-1 {
		secs = 1<<32 - 1
	}
	be := binary.BigEndian
	copy(b[0:8], vhdCookie)
	be.PutUint32(b[8:], vhdFeatures)
	be.PutUint32(b[12:], vhdVersion)
	be.PutUint64(b[16:], vhdNoOffset)
	be.PutUint32(b[24:], uint32(secs))
	copy(b[28:32], vhdCreatorApp)
	be.PutUint32(b[32:], vhdCreatorVer)
	copy(b[36:40], vhdCreatorOS)
	be.PutUint64(b[40:], size)
	be.PutUint64(b[48:], size)
	c, h, s := Geometry(size)
	be.PutUint16(b[56:], c)
	b[58], b[59] = h, s
	be.PutUint32(b[60:], vhdFixedType)
	copy(b[68:84], id[:])
	be.PutUint32(b[64:], checksum(&b))
	return b, nil
}

// checksum is the one's complement of the byte sum, with the checksum field as zero.
func checksum(b *[FooterSize]byte) uint32 {
	var sum uint32
	for i, c := range b {
		if i >= 64 && i < 68 {
			continue
		}
		sum += uint32(c)
	}
	return ^sum
}

// ParseFooter decodes and checks a fixed-VHD footer: cookie, version, type, checksum,
// sizes, geometry and the zeroed reserved area. Anything else is refused: the engine
// attaches only disks shaped exactly like the ones EncodeFooter writes.
func ParseFooter(p []byte) (*Footer, error) {
	if len(p) != FooterSize {
		return nil, fmt.Errorf("vhd: footer is %d bytes, want %d", len(p), FooterSize)
	}
	var b [FooterSize]byte
	copy(b[:], p)
	be := binary.BigEndian
	switch {
	case string(b[0:8]) != vhdCookie:
		return nil, errors.New("vhd: no footer cookie (not a VHD)")
	case be.Uint32(b[8:]) != vhdFeatures:
		return nil, fmt.Errorf("vhd: features %#x, want %#x", be.Uint32(b[8:]), vhdFeatures)
	case be.Uint32(b[12:]) != vhdVersion:
		return nil, fmt.Errorf("vhd: version %#x", be.Uint32(b[12:]))
	case be.Uint32(b[60:]) != vhdFixedType:
		return nil, fmt.Errorf("vhd: disk type %d, want fixed (2)", be.Uint32(b[60:]))
	case be.Uint64(b[16:]) != vhdNoOffset:
		return nil, errors.New("vhd: a fixed disk with a data offset")
	case be.Uint32(b[64:]) != checksum(&b):
		return nil, errors.New("vhd: footer checksum mismatch")
	case !bytes.Equal(b[85:], make([]byte, FooterSize-85)):
		return nil, errors.New("vhd: reserved footer bytes are not zero")
	case b[84] != 0:
		return nil, errors.New("vhd: saved-state flag set")
	}
	f := &Footer{Size: be.Uint64(b[48:])}
	if be.Uint64(b[40:]) != f.Size {
		return nil, errors.New("vhd: original and current size differ")
	}
	if f.Size == 0 || f.Size%DiskAlign != 0 || f.Size > MaxDiskSize {
		return nil, fmt.Errorf("vhd: size %d out of range", f.Size)
	}
	f.Cylinders, f.Heads, f.Sectors = be.Uint16(b[56:]), b[58], b[59]
	if c, h, s := Geometry(f.Size); c != f.Cylinders || h != f.Heads || s != f.Sectors {
		return nil, errors.New("vhd: geometry does not match the size")
	}
	f.Timestamp = vhdEpoch.Add(time.Duration(be.Uint32(b[24:])) * time.Second)
	copy(f.UniqueID[:], b[68:84])
	return f, nil
}
