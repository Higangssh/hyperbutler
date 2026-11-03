package transport

import (
	"context"
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
