package transport

import (
	"context"
	"errors"
	"net"
)

// TCPDialer dials a TCP address: the tests' stand-in for HvsockDialer. It lives in a
// test file so that production code has no TCP dialer (REQ-UX-005, internal/notelemetry).
type TCPDialer struct {
	// Address is a host:port understood by net.Dialer.
	Address string
}

// DialContext dials d.Address over TCP.
func (d TCPDialer) DialContext(ctx context.Context) (net.Conn, error) {
	if d.Address == "" {
		return nil, errors.New("transport: TCPDialer has no address")
	}
	var nd net.Dialer
	return nd.DialContext(ctx, "tcp", d.Address)
}
