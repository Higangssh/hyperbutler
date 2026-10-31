package main

import (
	"errors"
	"fmt"
	"io"
	"syscall"

	"github.com/Higangssh/hyperbutler/internal/netstack/frame"
)

// The two directions each run as two goroutines, one per device, so that neither
// device waits while the other is busy, and frames are handled where they were read
// (no copy out of the read buffer). On its own this did not change the measured
// throughput (ADR 0006): the limits were elsewhere. It is kept so that the vsock is
// drained while the TAP is written.
const (
	// hostChunk is the largest vsock read; hostChunks are read ahead of the TAP writer.
	hostChunk  = 1 << 20
	hostChunks = 4
	// tapBatches are filled from the TAP ahead of the vsock writer.
	tapBatches = 3
)

var errTapWrite = errors.New("tap write")

// tapToHost reads batches of frames from the TAP and writes each batch to the host in
// one write, with reading and writing overlapped.
func tapToHost(read readBatchFunc, conn io.Writer, lk linkFormat) error {
	free := make(chan []byte, tapBatches)
	for i := 0; i < tapBatches; i++ {
		free <- make([]byte, 0, batchBytes+frame.HeaderLen+lk.maxFrame())
	}
	full := make(chan []byte, tapBatches)
	werr := make(chan error, 1)
	go func() {
		for b := range full {
			if _, err := conn.Write(b); err != nil {
				werr <- fmt.Errorf("vsock write: %w", err)
				for range full {
				}
				return
			}
			free <- b[:0]
		}
		werr <- nil
	}()
	for {
		var buf []byte
		select {
		case buf = <-free:
		case err := <-werr:
			close(full)
			return err
		}
		b, err := read(buf)
		if len(b) > 0 {
			full <- b
		} else {
			free <- buf
		}
		if err != nil {
			close(full)
			if e := <-werr; e != nil {
				return e
			}
			return fmt.Errorf("tap read: %w", err)
		}
	}
}

// hostToTap writes each frame from the host to the TAP, reading the vsock ahead on
// another goroutine. In offload mode the frame starts with the virtio_net_hdr, which
// the kernel checks. stop must make a pending vsock read return.
func hostToTap(conn io.Reader, tap io.Writer, lk linkFormat, stop func()) error {
	cr := frame.NewChunkReader(conn, lk.minFrame(), lk.maxFrame())
	err := frame.Pump(cr, hostChunks, hostChunk, func(b []byte) error {
		if _, err := tap.Write(b); err != nil {
			// A frame the kernel refuses (for example while the device is down) is dropped.
			if errors.Is(err, syscall.EIO) || errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.EAGAIN) {
				return nil
			}
			return fmt.Errorf("%w: %w", errTapWrite, err)
		}
		return nil
	}, stop)
	if err != nil && !errors.Is(err, errTapWrite) {
		return fmt.Errorf("vsock read: %w", err)
	}
	return err
}
