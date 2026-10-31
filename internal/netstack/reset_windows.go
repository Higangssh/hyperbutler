//go:build windows

package netstack

import (
	"errors"
	"syscall"
)

// hostReset reports whether err, from a Windows socket, is a reset or an abort.
func hostReset(err error) bool {
	return errors.Is(err, syscall.WSAECONNRESET) || errors.Is(err, syscall.WSAECONNABORTED)
}

// wsaeconnrefused is WSAECONNREFUSED, which package syscall does not name.
const wsaeconnrefused = syscall.Errno(10061)

// connRefused reports whether err, from a connect on a Windows socket, is a refusal
// (nothing listens on the port).
func connRefused(err error) bool { return errors.Is(err, wsaeconnrefused) }
