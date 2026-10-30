//go:build windows

package diag

import (
	"github.com/Higangssh/winbox/internal/transport"
)

// Serve starts the debug endpoint on the local named pipe name. The pipe gets
// transport.ListenPipe's descriptor: owned by the current user, protected DACL granting
// only that user access, remote clients rejected, and creation fails if the name is
// already taken. The caller must Close the returned Server.
//
// The endpoint is opt-in: call Serve only when Config.DebugPipe is set.
func Serve(name string) (*Server, error) {
	ln, err := transport.ListenPipe(name)
	if err != nil {
		return nil, err
	}
	return serve(ln), nil
}
