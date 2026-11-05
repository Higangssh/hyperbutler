package broker

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
)

// The VHDX layout the data disk check reads (MS-VHDX 2.x): the file type identifier at
// 0, two region tables at 192 KiB and 256 KiB (each 64 KiB, CRC-32C), and in the
// metadata region a table of items, among them File Parameters, whose HasParent bit
// marks a differencing disk, and the Parent Locator of such a disk.
const (
	vhdxRegionTable1   = 192 << 10
	vhdxRegionTable2   = 256 << 10
	vhdxRegionTableLen = 64 << 10
	vhdxMetaTableLen   = 64 << 10
	vhdxMaxEntries     = 2047
	vhdxHasParent      = 1 << 1
)

var (
	vhdxSignature = []byte("vhdxfile")
	// GUIDs as stored on disk (little-endian fields).
	vhdxMetadataRegion = [16]byte{0x06, 0xa2, 0x7c, 0x8b, 0x90, 0x47, 0x9a, 0x4b, 0xb8, 0xfe, 0x57, 0x5f, 0x05, 0x0f, 0x88, 0x6e}
	vhdxFileParameters = [16]byte{0x37, 0x67, 0xa1, 0xca, 0x36, 0xfa, 0x43, 0x4d, 0xb3, 0xb6, 0x33, 0xf0, 0xaa, 0x44, 0xe7, 0x6b}
	vhdxParentLocator  = [16]byte{0x2d, 0x5f, 0xd3, 0xa8, 0x0b, 0xb3, 0x4d, 0x45, 0xab, 0xf7, 0xd3, 0xd8, 0x48, 0x34, 0xab, 0x0c}

	crc32c = crc32.MakeTable(crc32.Castagnoli)
)

// errDifferencing is the caller-safe reason for a differencing disk.
var errDifferencing = errors.New("is a differencing disk (it names a parent disk)")

// checkVHDX checks that r holds a VHDX that is not a differencing disk. HCS opens a
// differencing disk's parent by the path its metadata names, as the VM worker, so a
// data disk the caller wrote could make the guest read a disk the caller cannot (one
// the Virtual Machines group may read, or a share reached as the computer account).
// The error is caller-safe text.
func checkVHDX(r io.ReaderAt) error {
	sig := make([]byte, len(vhdxSignature))
	if _, err := r.ReadAt(sig, 0); err != nil || !bytes.Equal(sig, vhdxSignature) {
		return errors.New("is not a VHDX file")
	}
	if err := vhdxNoPendingLog(r); err != nil {
		return err
	}
	meta, metaLen, err := vhdxMetadataRegionOf(r)
	if err != nil {
		return err
	}
	if metaLen < 32 {
		return errors.New("has a malformed VHDX metadata region")
	}
	n := min(int64(metaLen), vhdxMetaTableLen)
	tab := make([]byte, n)
	if _, err := r.ReadAt(tab, int64(meta)); err != nil {
		return errors.New("has an unreadable VHDX metadata table")
	}
	if !bytes.Equal(tab[:8], []byte("metadata")) {
		return errors.New("has a malformed VHDX metadata table")
	}
	count := int(binary.LittleEndian.Uint16(tab[10:12]))
	if count > vhdxMaxEntries || 32+32*count > len(tab) {
		return errors.New("has a malformed VHDX metadata table")
	}
	params := false
	for i := 0; i < count; i++ {
		e := tab[32+32*i : 64+32*i]
		var id [16]byte
		copy(id[:], e[:16])
		off, length := binary.LittleEndian.Uint32(e[16:20]), binary.LittleEndian.Uint32(e[20:24])
		switch id {
		case vhdxParentLocator:
			return errDifferencing
		case vhdxFileParameters:
			if length < 8 || uint64(off)+8 > uint64(metaLen) {
				return errors.New("has malformed VHDX file parameters")
			}
			var fp [8]byte
			if _, err := r.ReadAt(fp[:], int64(meta)+int64(off)); err != nil {
				return errors.New("has unreadable VHDX file parameters")
			}
			if binary.LittleEndian.Uint32(fp[4:8])&vhdxHasParent != 0 {
				return errDifferencing
			}
			params = true
		}
	}
	if !params {
		return errors.New("has no VHDX file parameters")
	}
	return nil
}

// The two VHDX headers, each 4 KiB with a CRC-32C, and the fields read from them.
const (
	vhdxHeader1   = 64 << 10
	vhdxHeader2   = 128 << 10
	vhdxHeaderLen = 4 << 10
)

// vhdxNoPendingLog requires a valid current header (the valid one with the higher
// sequence number) whose LogGuid is zero. A nonzero LogGuid means a log the opener
// replays first, which can rewrite the metadata this check reads, so such a disk is
// refused rather than judged by its contents before replay. A disk the engine created
// and a VM shut down cleanly has none.
func vhdxNoPendingLog(r io.ReaderAt) error {
	var cur []byte
	var curSeq uint64
	for _, at := range []int64{vhdxHeader1, vhdxHeader2} {
		h := make([]byte, vhdxHeaderLen)
		if _, err := r.ReadAt(h, at); err != nil || !bytes.Equal(h[:4], []byte("head")) {
			continue
		}
		want := binary.LittleEndian.Uint32(h[4:8])
		binary.LittleEndian.PutUint32(h[4:8], 0)
		if crc32.Checksum(h, crc32c) != want {
			continue
		}
		if seq := binary.LittleEndian.Uint64(h[8:16]); cur == nil || seq > curSeq {
			cur, curSeq = h, seq
		}
	}
	if cur == nil {
		return errors.New("has no valid VHDX header")
	}
	if !bytes.Equal(cur[48:64], make([]byte, 16)) {
		return errors.New("has a pending VHDX log (it was not closed cleanly)")
	}
	return nil
}

// vhdxMetadataRegionOf finds the metadata region. Both region tables must be valid and
// name the same metadata region: an opener may use either, so a disk whose copies
// disagree is refused rather than judged by one of them.
func vhdxMetadataRegionOf(r io.ReaderAt) (offset uint64, length uint32, err error) {
	var found [2]struct {
		off uint64
		n   uint32
	}
	buf := make([]byte, vhdxRegionTableLen)
	for i, at := range []int64{vhdxRegionTable1, vhdxRegionTable2} {
		if _, err := r.ReadAt(buf, at); err != nil || !bytes.Equal(buf[:4], []byte("regi")) {
			return 0, 0, errors.New("has a damaged VHDX region table")
		}
		want := binary.LittleEndian.Uint32(buf[4:8])
		binary.LittleEndian.PutUint32(buf[4:8], 0)
		if crc32.Checksum(buf, crc32c) != want {
			return 0, 0, errors.New("has a damaged VHDX region table")
		}
		count := int(binary.LittleEndian.Uint32(buf[8:12]))
		if count > vhdxMaxEntries || 16+32*count > len(buf) {
			return 0, 0, errors.New("has a malformed VHDX region table")
		}
		ok := false
		for j := 0; j < count; j++ {
			e := buf[16+32*j : 48+32*j]
			var id [16]byte
			copy(id[:], e[:16])
			if id == vhdxMetadataRegion {
				if ok {
					return 0, 0, errors.New("names two VHDX metadata regions")
				}
				found[i].off, found[i].n, ok = binary.LittleEndian.Uint64(e[16:24]), binary.LittleEndian.Uint32(e[24:28]), true
			}
		}
		if !ok {
			return 0, 0, errors.New("has no VHDX metadata region")
		}
	}
	if found[0] != found[1] {
		return 0, 0, fmt.Errorf("has VHDX region tables that disagree")
	}
	if found[0].off > 1<<50 {
		return 0, 0, errors.New("has a malformed VHDX metadata region")
	}
	return found[0].off, found[0].n, nil
}
