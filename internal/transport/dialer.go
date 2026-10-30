package transport

import (
	"context"
	"errors"
	"net"
)

// Dialer opens one connection to the Docker API in the guest.
//
// Implementations must honor ctx cancellation and must be safe for concurrent use. The
// returned connection should implement CloseWrite() error, and CloseWrite must not block
// indefinitely; without it the proxy cannot forward a half-close and tears the whole
// connection down instead.
type Dialer interface {
	DialContext(ctx context.Context) (net.Conn, error)
}

// DialerFunc adapts a function to the Dialer interface.
type DialerFunc func(ctx context.Context) (net.Conn, error)

// DialContext calls f(ctx).
func (f DialerFunc) DialContext(ctx context.Context) (net.Conn, error) { return f(ctx) }

// TCPDialer dials a TCP address. It exists for tests and for debugging against a dockerd
// that listens on TCP; production traffic uses HvsockDialer.
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
