package netstack

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/link/ethernet"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"

	"github.com/Higangssh/hyperbutler/internal/netstack/frame"
)

// fakeGuest is a second gVisor stack that plays the guest kernel: it sends and receives
// Ethernet frames over a framed stream, exactly as guest/cmd/netfwd does with a TAP.
type fakeGuest struct {
	s    *stack.Stack
	ch   *channel.Endpoint
	conn net.Conn
	wg   sync.WaitGroup
	stop context.CancelFunc
	// bigIn and bigOut count offload-mode segments larger than the MTU, from and to
	// the host.
	bigIn, bigOut atomic.Int64
	// csumOK counts frames from the host whose partial checksum completed correctly.
	csumOK atomic.Int64
	// crcBad counts frames from the host that failed their CRC (link version 2) and
	// were dropped, as netfwd drops them.
	crcBad atomic.Int64
}

var guestMAC = tcpip.LinkAddress("\x02\x00\x00\x00\x00\x02")

func newFakeGuest(t *testing.T, n *Stack) *fakeGuest { return newFakeGuestMode(t, n, false) }

// newFakeGuestMode with offload plays netfwd -vnet over a TAP with TSO: the guest stack
// sends and takes segments larger than the MTU, with a virtio_net_hdr on every frame.
func newFakeGuestMode(t *testing.T, n *Stack, offload bool) *fakeGuest {
	t.Helper()
	return newFakeGuestWith(t, n, fakeGuestOpts{offload: offload})
}

type fakeGuestOpts struct {
	offload bool
	// wrap, if set, wraps the host's end of the link (the engine's hvsock connection).
	wrap func(net.Conn) net.Conn
	// inspect, if set, sees every Ethernet frame from the host as it arrives, in the
	// link reader's buffer, before the guest finishes its checksum or copies it.
	inspect func(eth []byte)
	// linkErr, if set, is called when the guest stops reading the host's frames because
	// the stream or a frame is malformed (not when the link closes), or in strict CRC
	// mode (the stack's Config.LinkCRC) because a frame failed its CRC.
	linkErr func(error)
	// sent, if set, sees every frame the guest sends, framed, before it is written; it
	// may change it.
	sent func(framed []byte)
}

func newFakeGuestWith(t *testing.T, n *Stack, o fakeGuestOpts) *fakeGuest {
	t.Helper()
	offload := o.offload
	cfg := n.Config()
	crc := offload && cfg.LinkCRC != CRCOff
	hostEnd, guestEnd := net.Pipe()
	hostConn := hostEnd
	if o.wrap != nil {
		hostConn = o.wrap(hostEnd)
	}
	ch := channel.New(512, uint32(cfg.MTU+header.EthernetMinimumSize), guestMAC)
	if offload {
		ch.SupportedGSOKind = stack.HostGSOSupported
		ch.LinkEPCapabilities = stack.CapabilityRXChecksumOffload
	}
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})
	if err := s.CreateNIC(1, ethernet.New(ch)); err != nil {
		t.Fatal(err)
	}
	ga := tcpipAddr(cfg.GuestIP())
	if err := s.AddProtocolAddress(1, tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{Address: ga, PrefixLen: cfg.Subnet.Bits()},
	}, stack.AddressProperties{}); err != nil {
		t.Fatal(err)
	}
	s.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, Gateway: tcpipAddr(cfg.Gateway()), NIC: 1}})

	ctx, cancel := context.WithCancel(context.Background())
	g := &fakeGuest{s: s, ch: ch, conn: guestEnd, stop: cancel}
	g.wg.Add(3)
	go func() {
		defer g.wg.Done()
		if offload {
			_ = n.ServeOffload(hostConn)
		} else {
			_ = n.ServeConn(hostConn)
		}
	}()
	go func() { // guest -> host
		defer g.wg.Done()
		for {
			pkt := ch.ReadContext(ctx)
			if pkt == nil {
				return
			}
			var b []byte
			if offload {
				var vh frame.VnetHdr
				if gso := pkt.GSOOptions; gso.Type != stack.GSONone {
					vh.Flags = frame.VnetFlagNeedsCsum
					vh.HdrLen = uint16(pkt.HeaderSize())
					vh.CsumStart = header.EthernetMinimumSize + gso.L3HdrLen
					vh.CsumOffset = gso.CsumOffset
					if pkt.Data().Size() > int(gso.MSS) {
						vh.GSOType, vh.GSOSize = frame.VnetGSOTCPv4, gso.MSS
						g.bigOut.Add(1)
					}
				}
				b = make([]byte, frame.VnetHdrLen)
				vh.Put(b)
			}
			for _, v := range pkt.AsSlices() {
				b = append(b, v...)
			}
			pkt.DecRef()
			if crc {
				b = frame.AppendCRC(nil, b)
			} else {
				b = frame.Append(nil, b)
			}
			if o.sent != nil {
				o.sent(b)
			}
			if _, err := guestEnd.Write(b); err != nil {
				return
			}
		}
	}()
	go func() { // host -> guest
		defer g.wg.Done()
		br := bufio.NewReader(guestEnd)
		fr, _ := frame.NewReader(br, cfg.MTU)
		if offload {
			var hello [frame.HeaderLen]byte
			if _, err := io.ReadFull(br, hello[:]); err != nil || hello != frame.HelloVersion(cfg.LinkCRC.linkVersion()) {
				return
			}
			fr = frame.NewVnetReader(br)
			if crc {
				fr.WithCRC()
			}
		}
		for {
			// b is the frame reader's buffer, valid until the next frame. Nothing here
			// copies it before inspect sees it, or before the guest stack takes it
			// (MakeWithData copies, as a kernel copies into an skb), so bytes the host
			// changed after handing them over are seen as they arrived, unless the
			// CRC drops them first.
			b, err := fr.Next()
			if errors.Is(err, frame.ErrCRC) {
				g.crcBad.Add(1)
				if cfg.LinkCRC != CRCStrict {
					continue
				}
				if o.linkErr != nil {
					o.linkErr(err)
				}
				return
			}
			if err != nil {
				if errors.Is(err, frame.ErrFrameSize) {
					t.Errorf("the host's frame stream is out of sync: %v", err)
					if o.linkErr != nil {
						o.linkErr(err)
					}
				}
				return
			}
			if offload {
				h, eth, err := frame.ParseVnet(b, cfg.MTU)
				if err != nil {
					t.Errorf("host sent a bad offload frame: %v", err)
					if o.linkErr != nil {
						o.linkErr(err)
					}
					return
				}
				if o.inspect != nil {
					o.inspect(eth)
				}
				if h.IsGSO() {
					g.bigIn.Add(1)
				}
				if h.Flags&frame.VnetFlagNeedsCsum != 0 {
					// Finish the checksum in place, as the guest kernel would
					// (skb_checksum_help), then check the TCP checksum it gives.
					seg := eth[h.CsumStart:]
					at := int(h.CsumOffset)
					sum := checksum.Checksum(seg, 0)
					seg[at], seg[at+1] = byte(^sum>>8), byte(^sum)
					ip := header.IPv4(eth[header.EthernetMinimumSize:])
					pseudo := header.PseudoHeaderChecksum(tcp.ProtocolNumber, ip.SourceAddress(), ip.DestinationAddress(), uint16(len(seg)))
					if checksum.Checksum(seg, pseudo) != 0xFFFF {
						t.Errorf("host sent a partial checksum that does not complete to a valid one")
						return
					}
					g.csumOK.Add(1)
				}
				b = eth
			} else if o.inspect != nil {
				o.inspect(b)
			}
			pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(b)})
			ch.InjectInbound(header.Ethernet(b).Type(), pkt)
			pkt.DecRef()
		}
	}()
	t.Cleanup(g.close)
	return g
}

func (g *fakeGuest) close() {
	g.stop()
	_ = g.conn.Close()
	g.ch.Close()
	g.s.Close()
	g.wg.Wait()
}

// fakeResolver answers from a fixed table.
type fakeResolver struct {
	a     map[string][]netip.Addr
	delay time.Duration
	calls int
	mu    sync.Mutex
}

func (f *fakeResolver) LookupNetIP(ctx context.Context, _, host string) ([]netip.Addr, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, &net.DNSError{Err: "timeout", Name: host, IsTimeout: true}
		}
	}
	if ips, ok := f.a[host]; ok {
		return ips, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}
func (f *fakeResolver) LookupCNAME(context.Context, string) (string, error) {
	return "", &net.DNSError{IsNotFound: true}
}
func (f *fakeResolver) LookupMX(context.Context, string) ([]*net.MX, error) {
	return []*net.MX{{Host: "mx.example.com.", Pref: 10}, {Host: "bad\x00name", Pref: 1}}, nil
}
func (f *fakeResolver) LookupNS(context.Context, string) ([]*net.NS, error) {
	return nil, &net.DNSError{IsNotFound: true}
}
func (f *fakeResolver) LookupSRV(context.Context, string, string, string) (string, []*net.SRV, error) {
	return "", nil, &net.DNSError{IsNotFound: true}
}
func (f *fakeResolver) LookupTXT(context.Context, string) ([]string, error) {
	return []string{string(make([]byte, 600))}, nil
}
func (f *fakeResolver) LookupAddr(context.Context, string) ([]string, error) {
	return []string{"host.example.com"}, nil
}
