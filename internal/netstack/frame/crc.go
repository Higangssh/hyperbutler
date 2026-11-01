package frame

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"sync"
	"time"
)

// Version 2 of the offload link adds a CRC-32C (Castagnoli) to every frame, in both
// directions, between the length prefix and the virtio_net_hdr:
//
//	frame: length (4 bytes, big-endian, counting what follows) | CRC-32C (4 bytes,
//	       big-endian) | virtio_net_hdr | Ethernet frame
//
// The CRC covers the virtio_net_hdr and the Ethernet frame. The sender computes it
// from the bytes it frames, before they are queued, and the receiver checks it before
// it hands the frame on, so corruption anywhere in between (a buffer reused while it
// is still being written, on either end) is found instead of trusted: in offload mode
// neither the guest kernel (NEEDS_CSUM) nor gVisor (RX checksum offload) verifies a
// TCP checksum. A frame that fails the check is dropped, which TCP recovers by
// retransmitting, and counted (ADR 0006, "Integrity").
//
// Version 1 is the same framing without the CRC, for comparing the cost. Plain mode
// (no Hello) never has one.
const (
	// VersionNoCRC is the offload link without a CRC.
	VersionNoCRC = 1
	// VersionCRC is the offload link with a CRC-32C on every frame.
	VersionCRC = 2
	// CRCLen is the size of the CRC field.
	CRCLen = 4
)

// castagnoli is the CRC-32C table; hash/crc32 uses SSE4.2 or the ARMv8 CRC
// instructions for it.
var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// UpdateCRC returns crc updated with b; start with 0.
func UpdateCRC(crc uint32, b []byte) uint32 { return crc32.Update(crc, castagnoli, b) }

// PutCRC writes crc into b[:CRCLen].
func PutCRC(b []byte, crc uint32) { binary.BigEndian.PutUint32(b[:CRCLen], crc) }

// SealCRC computes the CRC of b[CRCLen:] and writes it into b[:CRCLen].
func SealCRC(b []byte) { PutCRC(b, UpdateCRC(0, b[CRCLen:])) }

// CheckCRC reports whether b, a frame's CRC field and what follows it, is intact.
func CheckCRC(b []byte) bool {
	return len(b) >= CRCLen && binary.BigEndian.Uint32(b) == UpdateCRC(0, b[CRCLen:])
}

// AppendCRC appends the version 2 encoding of frame to dst.
func AppendCRC(dst, frame []byte) []byte {
	var h [HeaderLen + CRCLen]byte
	PutHeader(h[:], CRCLen+len(frame))
	PutCRC(h[HeaderLen:], UpdateCRC(0, frame))
	dst = append(dst, h[:]...)
	return append(dst, frame...)
}

// ErrCRC reports a frame whose CRC does not match. In counting mode the frame is
// dropped and the stream goes on; in strict mode it ends the stream.
var ErrCRC = errors.New("frame: CRC-32C mismatch")

// CRCCheck turns on the CRC check of a version 2 stream.
type CRCCheck struct {
	// Strict makes a mismatch end the stream with ErrCRC, so that tests and benches
	// fail instead of TCP quietly recovering. Otherwise the frame is dropped.
	Strict bool
	// Bad, if set, is called for every frame that fails the check, with its length
	// (CRC included).
	Bad func(n int)
}

// HelloVersion is the Hello of link version v.
func HelloVersion(v byte) [HeaderLen]byte { return [HeaderLen]byte{Hello[0], Hello[1], Hello[2], v} }

// ParseHello returns the link version of a Hello, and false for anything else.
func ParseHello(b [HeaderLen]byte) (byte, bool) {
	if b[0] != Hello[0] || b[1] != Hello[1] || b[2] != Hello[2] {
		return 0, false
	}
	return b[3], true
}

// Link reports (version 2, guest to host). netfwd sends one when its count of frames
// dropped for a CRC mismatch changed, so the host's counters show both directions.
// It is a frame whose virtio_net_hdr has only VnetFlagLinkReport set, which a TAP
// device never sets, followed by LinkReportLen bytes: the count (8 bytes, big-endian)
// and 6 reserved bytes, zero. Its size is MinVnetFrameLen.
const (
	VnetFlagLinkReport = 0x80
	LinkReportLen      = EthernetHeaderLen
)

// LinkReport is what a link report carries.
type LinkReport struct {
	// CRCDropped is how many frames from the host the guest dropped for a CRC
	// mismatch since netfwd started.
	CRCDropped uint64
}

// AppendLinkReport appends r as a version 2 frame to dst.
func AppendLinkReport(dst []byte, r LinkReport) []byte {
	var b [VnetHdrLen + LinkReportLen]byte
	b[0] = VnetFlagLinkReport
	binary.BigEndian.PutUint64(b[VnetHdrLen:], r.CRCDropped)
	return AppendCRC(dst, b[:])
}

// IsLinkReport reports whether b, an offload-mode frame (header included), is a link
// report rather than an Ethernet frame.
func IsLinkReport(b []byte) bool { return len(b) > 0 && b[0] == VnetFlagLinkReport }

// ParseLinkReport decodes a link report.
func ParseLinkReport(b []byte) (LinkReport, error) {
	if len(b) != VnetHdrLen+LinkReportLen || !IsLinkReport(b) {
		return LinkReport{}, fmt.Errorf("%w: link report of %d bytes", ErrVnetHdr, len(b))
	}
	for _, c := range b[1:VnetHdrLen] {
		if c != 0 {
			return LinkReport{}, fmt.Errorf("%w: link report header", ErrVnetHdr)
		}
	}
	return LinkReport{CRCDropped: binary.BigEndian.Uint64(b[VnetHdrLen:])}, nil
}

// MismatchLog decides which CRC mismatches are logged: the first MismatchLogFirst one
// by one, then at most one line per MismatchLogEvery, with the count since the last.
// It is safe for concurrent use.
type MismatchLog struct {
	mu      sync.Mutex
	n       uint64
	skipped uint64
	last    time.Time
}

const (
	MismatchLogFirst = 5
	MismatchLogEvery = time.Minute
)

// Note records one mismatch. It returns whether to log it, and how many were not
// logged before it.
func (l *MismatchLog) Note(now time.Time) (log bool, skipped uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.n++
	if l.n <= MismatchLogFirst || now.Sub(l.last) >= MismatchLogEvery {
		l.last, skipped, l.skipped = now, l.skipped, 0
		return true, skipped
	}
	l.skipped++
	return false, 0
}
