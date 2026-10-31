package frame

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Offload mode (version 1 of the link). netfwd -vnet opens the TAP with IFF_VNET_HDR
// and TCP segmentation offload and listens; the host connects and sends Hello. Every
// frame, both ways, then begins with a virtio_net_hdr (VnetHdrLen bytes, little-endian,
// as the TAP device reads and writes it), so both sides pass TCP segments of up to
// 64 KiB with a partial checksum instead of MTU-sized frames with a full one:
//
//	host to guest: Hello, then frames; guest to host: frames
//	frame: length (4 bytes, big-endian, counting what follows) | virtio_net_hdr | Ethernet frame
//
// A plain stream (the guest dials, no Hello) carries Ethernet frames only. A plain
// frame length is below 2^16, so its first byte is 0 and can never be Hello.
const (
	// VnetHdrLen is sizeof(struct virtio_net_hdr), the TAP device's default vnet header.
	VnetHdrLen = 10
	// MaxIPv4Packet is the largest IPv4 packet (the total length field is 16 bits),
	// which bounds a segmentation-offload frame.
	MaxIPv4Packet = 0xFFFF
	// MaxVnetFrameLen is the largest offload-mode frame: header, Ethernet header and
	// the largest IPv4 packet.
	MaxVnetFrameLen = VnetHdrLen + EthernetHeaderLen + MaxIPv4Packet
	// MinVnetFrameLen is the smallest offload-mode frame.
	MinVnetFrameLen = VnetHdrLen + EthernetHeaderLen
	// GSOMaxSize is the largest segment either side builds: what Linux uses for a TAP
	// device, and what keeps the IPv4 total length within 16 bits.
	GSOMaxSize = 1 << 16
)

// Hello opens an offload-mode stream. Its first byte is 0xFF; the last one is the
// link version.
var Hello = [HeaderLen]byte{0xFF, 'h', 'b', 1}

// virtio_net_hdr flags and GSO types (linux/virtio_net.h).
const (
	VnetFlagNeedsCsum = 1 // csum_start and csum_offset say where the checksum goes
	VnetFlagDataValid = 2 // the checksum is known to be good

	VnetGSONone  = 0
	VnetGSOTCPv4 = 1
	VnetGSOECN   = 0x80 // ORed into a TCP GSO type when the segment has CWR set
)

// ErrVnetHdr reports a virtio_net_hdr that this link does not accept.
var ErrVnetHdr = errors.New("frame: bad virtio_net_hdr")

// VnetHdr is a virtio_net_hdr.
type VnetHdr struct {
	Flags      uint8
	GSOType    uint8
	HdrLen     uint16 // Ethernet, IP and TCP headers of a GSO segment
	GSOSize    uint16 // the MSS a GSO segment would be cut into
	CsumStart  uint16 // where checksumming starts, from the Ethernet header
	CsumOffset uint16 // where the checksum goes, from CsumStart
}

// Put writes h into b[:VnetHdrLen].
func (h VnetHdr) Put(b []byte) {
	_ = b[VnetHdrLen-1]
	b[0] = h.Flags
	b[1] = h.GSOType
	binary.LittleEndian.PutUint16(b[2:], h.HdrLen)
	binary.LittleEndian.PutUint16(b[4:], h.GSOSize)
	binary.LittleEndian.PutUint16(b[6:], h.CsumStart)
	binary.LittleEndian.PutUint16(b[8:], h.CsumOffset)
}

// IsGSO reports whether h describes a TCP segment larger than the MTU.
func (h VnetHdr) IsGSO() bool { return h.GSOType&^VnetGSOECN != VnetGSONone }

// ParseVnet splits an offload-mode frame into its header and Ethernet frame, and
// checks the header against the frame. It accepts what the guest kernel produces for
// an IPv4-only TAP with TUN_F_CSUM, TUN_F_TSO4 and TUN_F_TSO_ECN: no GSO or TCPv4 GSO
// (optionally with ECN), and the NEEDS_CSUM and DATA_VALID flags. A frame without GSO
// must fit the MTU; a GSO frame may be up to MaxVnetFrameLen. The header's offsets
// must lie inside the frame. The stack checks nothing else from it: the receiving
// gVisor stack does not verify checksums on this link, and it parses the packet's own
// headers.
func ParseVnet(b []byte, mtu int) (VnetHdr, []byte, error) {
	if len(b) < MinVnetFrameLen {
		return VnetHdr{}, nil, fmt.Errorf("%w: frame of %d bytes", ErrVnetHdr, len(b))
	}
	h := VnetHdr{
		Flags:      b[0],
		GSOType:    b[1],
		HdrLen:     binary.LittleEndian.Uint16(b[2:]),
		GSOSize:    binary.LittleEndian.Uint16(b[4:]),
		CsumStart:  binary.LittleEndian.Uint16(b[6:]),
		CsumOffset: binary.LittleEndian.Uint16(b[8:]),
	}
	eth := b[VnetHdrLen:]
	if h.Flags&^(VnetFlagNeedsCsum|VnetFlagDataValid) != 0 {
		return h, nil, fmt.Errorf("%w: flags %#x", ErrVnetHdr, h.Flags)
	}
	if h.Flags&VnetFlagNeedsCsum != 0 && int(h.CsumStart)+int(h.CsumOffset)+2 > len(eth) {
		return h, nil, fmt.Errorf("%w: checksum at %d+%d outside a frame of %d bytes", ErrVnetHdr, h.CsumStart, h.CsumOffset, len(eth))
	}
	switch h.GSOType &^ VnetGSOECN {
	case VnetGSONone:
		if h.GSOType != VnetGSONone {
			return h, nil, fmt.Errorf("%w: ECN without GSO", ErrVnetHdr)
		}
		if len(eth) > MaxFrameLen(mtu) {
			return h, nil, fmt.Errorf("%w: %d-byte frame without GSO over MTU %d", ErrVnetHdr, len(eth), mtu)
		}
	case VnetGSOTCPv4:
		if h.GSOSize == 0 || int(h.HdrLen) > len(eth) {
			return h, nil, fmt.Errorf("%w: GSO size %d, header length %d, frame %d", ErrVnetHdr, h.GSOSize, h.HdrLen, len(eth))
		}
		if len(eth) > EthernetHeaderLen+MaxIPv4Packet {
			return h, nil, fmt.Errorf("%w: GSO frame of %d bytes", ErrVnetHdr, len(eth))
		}
	default:
		return h, nil, fmt.Errorf("%w: GSO type %#x", ErrVnetHdr, h.GSOType)
	}
	return h, eth, nil
}

// NewVnetReader returns a Reader for an offload-mode stream after its Hello: frames
// of MinVnetFrameLen..MaxVnetFrameLen bytes, header included. The caller checks each
// one with ParseVnet.
func NewVnetReader(r io.Reader) *Reader {
	return &Reader{r: r, min: MinVnetFrameLen, max: MaxVnetFrameLen, buf: make([]byte, MaxVnetFrameLen)}
}
