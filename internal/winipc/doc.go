// Package winipc is hyperbutler's Windows IPC layer: local named pipes and Hyper-V
// sockets (AF_HYPERV) as net.Conn and net.Listener, with only the standard library and
// golang.org/x/sys/windows.
//
// Every handle is opened for overlapped I/O and associated with one process-wide I/O
// completion port, which a single goroutine drains (ioCompletionLoop). A Read or Write
// that cannot complete at once parks its goroutine on a channel instead of an OS
// thread, so thousands of idle connections cost no threads. Operations that complete
// synchronously skip the port entirely (FILE_SKIP_COMPLETION_PORT_ON_SUCCESS).
//
// Connections support read and write deadlines (os.ErrDeadlineExceeded), Close from any
// goroutine (pending operations return net.ErrClosed, and the handle is released only
// after the kernel has finished with every OVERLAPPED), and CloseWrite: shutdown(SD_SEND)
// on a socket, and on a message-type pipe a zero-byte message after the peer has read
// everything written before it, which the Docker CLI and SDKs read as EOF.
//
// On other systems the package is empty.
package winipc
