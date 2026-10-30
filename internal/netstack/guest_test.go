package netstack

import (
	"bufio"
	"context"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/link/ethernet"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"

	"github.com/Higangssh/winbox/internal/netstack/frame"
)

// fakeGuest is a second gVisor stack that plays the guest kernel: it sends and receives
// Ethernet frames over a framed stream, exactly as guest/cmd/netfwd does with a TAP.
type fakeGuest struct {
	s    *stack.Stack
	ch   *channel.Endpoint
	conn net.Conn
	wg   sync.WaitGroup
	stop context.CancelFunc
}

var guestMAC = tcpip.LinkAddress("\x02\x00\x00\x00\x00\x02")

func newFakeGuest(t *testing.T, n *Stack) *fakeGuest {
	t.Helper()
	cfg := n.Config()
	hostEnd, guestEnd := net.Pipe()
	ch := channel.New(512, uint32(cfg.MTU+header.EthernetMinimumSize), guestMAC)
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
	go func() { defer g.wg.Done(); _ = n.ServeConn(hostEnd) }()
	go func() { // guest -> host
		defer g.wg.Done()
		for {
			pkt := ch.ReadContext(ctx)
			if pkt == nil {
				return
			}
			var b []byte
			for _, v := range pkt.AsSlices() {
				b = append(b, v...)
			}
			pkt.DecRef()
			if _, err := guestEnd.Write(frame.Append(nil, b)); err != nil {
				return
			}
		}
	}()
	go func() { // host -> guest
		defer g.wg.Done()
		fr, _ := frame.NewReader(bufio.NewReader(guestEnd), cfg.MTU)
		for {
			b, err := fr.Next()
			if err != nil {
				return
			}
			pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(append([]byte(nil), b...))})
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
