package main

import (
	"io"
	"sync"
)

// stream is a byte stream whose send side can be shut down on its own.
type stream interface {
	io.ReadWriteCloser
	CloseWrite() error
}

const relayBufferSize = 32 << 10

// relay copies a to b and b to a. A clean EOF in one direction is passed on as
// CloseWrite on the other side, and the reverse direction keeps running: docker attach
// and exec close stdin while they still read output. relay returns once both directions
// have finished, or as soon as either fails. It always closes a and b before returning,
// and returns the first error (nil when both directions ended in EOF).
func relay(a, b stream) error {
	var once sync.Once
	closeBoth := func() {
		once.Do(func() {
			_ = a.Close()
			_ = b.Close()
		})
	}
	errc := make(chan error, 2)
	go func() { errc <- pipe(b, a) }()
	go func() { errc <- pipe(a, b) }()
	err := <-errc
	if err != nil {
		closeBoth() // unblocks the other direction
		<-errc      //nolint:errcheck // fails only because closeBoth closed it; err is the cause
		return err
	}
	err = <-errc
	closeBoth()
	return err
}

// pipe copies src into dst until EOF, then shuts down dst's send side.
func pipe(dst, src stream) error {
	buf := make([]byte, relayBufferSize)
	// Hide ReadFrom/WriteTo so the copy always goes through buf. *os.File's ReadFrom
	// may try splice or sendfile, which AF_VSOCK does not support.
	if _, err := io.CopyBuffer(writerOnly{dst}, readerOnly{src}, buf); err != nil {
		return err
	}
	return dst.CloseWrite()
}

type readerOnly struct{ io.Reader }

type writerOnly struct{ io.Writer }
