// Package frame is the Ethernet framing used on the hvsock link between the guest's TAP
// device (guest/cmd/netfwd) and the host's user-mode network stack (internal/netstack).
//
// Each frame is a 4-byte big-endian length followed by that many bytes of Ethernet frame,
// which is gvisor-tap-vsock's "qemu" stream protocol. The package has no dependencies so
// the guest binary stays small.
//
// The reader treats its input as untrusted: the guest controls one side of the link, and a
// guest that sends a bad length must not make the host allocate or panic.
package frame

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	// HeaderLen is the size of the length prefix.
	HeaderLen = 4
	// EthernetHeaderLen is the minimum frame: destination MAC, source MAC, EtherType.
	EthernetHeaderLen = 14
	// MaxMTU is the largest MTU either side accepts. A frame is at most MaxMTU plus the
	// Ethernet header.
	MaxMTU = 65520
	// MinMTU is the IPv4 minimum (RFC 791) the link is allowed to run at.
	MinMTU = 576
)

// ErrFrameSize reports a length prefix outside [EthernetHeaderLen, MaxFrameLen(mtu)].
var ErrFrameSize = errors.New("frame: length out of range")

// MaxFrameLen is the largest frame accepted on a link with the given MTU.
func MaxFrameLen(mtu int) int { return mtu + EthernetHeaderLen }

// ValidMTU reports whether mtu can be used on the link.
func ValidMTU(mtu int) bool { return mtu >= MinMTU && mtu <= MaxMTU }

// Reader reads length-prefixed frames. It is not safe for concurrent use.
type Reader struct {
	r   io.Reader
	min int
	max int
	hdr [HeaderLen]byte
	buf []byte
}

// NewReader returns a Reader for a link with the given MTU. r should be buffered.
func NewReader(r io.Reader, mtu int) (*Reader, error) {
	if !ValidMTU(mtu) {
		return nil, fmt.Errorf("frame: MTU %d is outside %d..%d", mtu, MinMTU, MaxMTU)
	}
	max := MaxFrameLen(mtu)
	return &Reader{r: r, min: EthernetHeaderLen, max: max, buf: make([]byte, max)}, nil
}

// Next returns the next frame. The slice is valid until the next call. A length prefix
// outside the allowed range returns ErrFrameSize, after which the stream is out of sync
// and must be closed.
func (fr *Reader) Next() ([]byte, error) {
	if _, err := io.ReadFull(fr.r, fr.hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(fr.hdr[:])
	if int64(n) < int64(fr.min) || int64(n) > int64(fr.max) {
		return nil, fmt.Errorf("%w: %d (allowed %d..%d)", ErrFrameSize, n, fr.min, fr.max)
	}
	b := fr.buf[:n]
	if _, err := io.ReadFull(fr.r, b); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return b, nil
}

// PutHeader writes the length prefix for a frame of n bytes into b[:HeaderLen].
func PutHeader(b []byte, n int) {
	binary.BigEndian.PutUint32(b[:HeaderLen], uint32(n)) //nolint:gosec // G115: callers pass frame lengths, at most MaxFrameLen
}

// Append appends the encoding of frame to dst.
func Append(dst, frame []byte) []byte {
	var h [HeaderLen]byte
	PutHeader(h[:], len(frame))
	dst = append(dst, h[:]...)
	return append(dst, frame...)
}
