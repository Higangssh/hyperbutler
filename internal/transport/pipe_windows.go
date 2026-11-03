//go:build windows

package transport

import (
	"errors"
	"fmt"
	"net"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/winipc"
)

// pipeBufferSize matches what dockerd uses for its own named pipe.
const pipeBufferSize = 64 << 10

// CurrentUserSDDL returns the security descriptor applied to the listening pipe: owned by
// the current user, with a protected DACL that grants GENERIC_ALL to that user and to no
// one else. SYSTEM, Administrators, and other users get no access; remote clients are
// already rejected by the pipe type (FILE_PIPE_REJECT_REMOTE_CLIENTS, set by winipc).
func CurrentUserSDDL() (string, error) {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("transport: query token user: %w", err)
	}
	sid := tu.User.Sid.String()
	if sid == "" {
		return "", errors.New("transport: current user has no SID")
	}
	// O: pins the owner to the user even when the process is elevated, where the default
	// owner would be BUILTIN\Administrators (which would grant every admin WRITE_DAC).
	// D:P makes the DACL protected so no inheritable ACE can widen it.
	return "O:" + sid + "D:P(A;;GA;;;" + sid + ")", nil
}

// ListenPipe listens on the local named pipe name (see ValidatePipeName) with a security
// descriptor from CurrentUserSDDL. The pipe is created in message mode so that clients
// (the Docker CLI and SDKs, and winipc.DialPipe) get a connection that supports
// CloseWrite: a zero-byte message they read as EOF.
//
// Creation fails if a pipe with the same name already exists, including one created by
// another process to squat on the name.
func ListenPipe(name string) (net.Listener, error) {
	if err := ValidatePipeName(name); err != nil {
		return nil, err
	}
	sddl, err := CurrentUserSDDL()
	if err != nil {
		return nil, err
	}
	ln, err := winipc.ListenPipe(name, &winipc.PipeConfig{
		SecurityDescriptor: sddl,
		MessageMode:        true,
		InputBufferSize:    pipeBufferSize,
		OutputBufferSize:   pipeBufferSize,
	})
	if err != nil {
		return nil, fmt.Errorf("transport: listen on %s: %w", name, err)
	}
	return &pipeListener{Listener: ln}, nil
}

type pipeListener struct {
	net.Listener
}

func (l *pipeListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	pc, ok := c.(*winipc.PipeConn)
	if !ok {
		_ = c.Close()
		return nil, errors.New("transport: accepted connection is not a pipe")
	}
	return &pipeConn{PipeConn: pc}, nil
}

// pipeConn is the server end of one named-pipe connection. Its CloseWrite (winipc's)
// waits until the client has read everything written so far, and Close interrupts that
// wait.
type pipeConn struct {
	*winipc.PipeConn
}

// closeWriteWithoutLock marks pipeConn's CloseWrite as safe to run concurrently with
// Close (Close interrupts it), so halfConn must not hold its lock around it.
func (c *pipeConn) closeWriteWithoutLock() {}

// peerClosed reports whether the client has closed its end of the pipe. It does not
// read, so it is safe after a half-close.
func (c *pipeConn) peerClosed() bool { return c.PeerClosed() }

// Compile-time interface checks.
var (
	_ closeWriter         = (*pipeConn)(nil)
	_ unlockedCloseWriter = (*pipeConn)(nil)
	_ peerProber          = (*pipeConn)(nil)
)
