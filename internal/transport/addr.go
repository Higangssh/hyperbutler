package transport

import (
	"github.com/Higangssh/hyperbutler/internal/guid"
	"github.com/Higangssh/hyperbutler/internal/hvaddr"
)

// The address forms live in internal/hvaddr, which the broker service can import
// without this package's I/O; these names keep the callers of transport as they were.

// DefaultPipeName is hvaddr.DefaultPipeName.
const DefaultPipeName = hvaddr.DefaultPipeName

// HvsockTarget is hvaddr.HvsockTarget.
type HvsockTarget = hvaddr.HvsockTarget

// VsockServiceID is hvaddr.VsockServiceID.
func VsockServiceID(port uint32) guid.GUID { return hvaddr.VsockServiceID(port) }

// ParseHvsockTarget is hvaddr.ParseHvsockTarget.
func ParseHvsockTarget(s string) (HvsockTarget, error) { return hvaddr.ParseHvsockTarget(s) }

// ValidatePipeName is hvaddr.ValidatePipeName.
func ValidatePipeName(name string) error { return hvaddr.ValidatePipeName(name) }
