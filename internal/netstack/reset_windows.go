//go:build windows

package netstack

import (
	"errors"
	"syscall"
)

// hostReset reports whether err, from a Windows socket, is a reset by its peer.
func hostReset(err error) bool { return errors.Is(err, syscall.WSAECONNRESET) }
