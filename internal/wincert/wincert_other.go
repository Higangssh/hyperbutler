//go:build !windows

// Package wincert reads CA certificates from the Windows certificate stores
// (REQ-UX-009). Elsewhere there are none.
package wincert

import (
	"errors"

	"github.com/Higangssh/hyperbutler/internal/dockerenv"
)

// ImportRoots is unsupported off Windows.
func ImportRoots(*dockerenv.CertSet) error {
	return errors.New("the Windows certificate store is only available on Windows")
}
