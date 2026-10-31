//go:build !windows

package netstack

import (
	"errors"
	"syscall"
)

// hostReset reports whether err, from a host socket, is a reset by its peer.
func hostReset(err error) bool {
	return errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED)
}

// connRefused reports whether err, from a connect, is a refusal (nothing listens on
// the port).
func connRefused(err error) bool { return errors.Is(err, syscall.ECONNREFUSED) }
