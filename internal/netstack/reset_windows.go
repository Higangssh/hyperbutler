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
