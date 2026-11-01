package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"syscall"
	"time"

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

// reportEvery is how often tapToHost sends the host a link report when the count of
// frames dropped for their CRC changed. A variable for the tests.
var reportEvery = time.Second

var errTapWrite = errors.New("tap write")

// tapToHost reads batches of frames from the TAP and writes each batch to the host in
// one write, with reading and writing overlapped. In link version 2 the writer also
// sends a link report (frame.LinkReport) between batches when lk.st's count of frames
// dropped for their CRC changed, at most once per reportEvery.
func tapToHost(read readBatchFunc, conn io.Writer, lk linkFormat) error {
	free := make(chan []byte, tapBatches)
	for i := 0; i < tapBatches; i++ {
		free <- make([]byte, 0, batchBytes+lk.prefix()+lk.maxFrame())
	}
	full := make(chan []byte, tapBatches)
	werr := make(chan error, 1)
	go func() {
		var tick <-chan time.Time
		if lk.crc && lk.st != nil {
			t := time.NewTicker(reportEvery)
			defer t.Stop()
			tick = t.C
		}
		var reported uint64
		var report []byte
		for {
			var b []byte
			select {
			case bb, ok := <-full:
				if !ok {
					werr <- nil
					return
				}
				b = bb
			case <-tick:
				n := lk.st.crcDropped.Load()
				if n == reported {
					continue
				}
				report = frame.AppendLinkReport(report[:0], frame.LinkReport{CRCDropped: n})
				if _, err := conn.Write(report); err != nil {
					werr <- fmt.Errorf("vsock write: %w", err)
					for range full {
					}
					return
				}
				reported = n
				continue
			}
			if _, err := conn.Write(b); err != nil {
				werr <- fmt.Errorf("vsock write: %w", err)
				for range full {
				}
				return
			}
			free <- b[:0]
		}
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
// the kernel checks; in link version 2 its CRC is checked first, on the reading
// goroutine, and a frame that fails it is dropped and counted (or, strict, ends the
// link). stop must make a pending vsock read return.
func hostToTap(conn io.Reader, tap io.Writer, lk linkFormat, stop func()) error {
	cr := frame.NewChunkReader(conn, lk.minFrame(), lk.maxFrame())
	if lk.crc {
		cr.WithCRC(frame.CRCCheck{Strict: lk.strict, Bad: func(n int) {
			var total uint64
			if lk.st != nil {
				total = lk.st.crcDropped.Add(1)
				if ok, skipped := lk.st.crcLog.Note(time.Now()); !ok {
					return
				} else if skipped > 0 {
					log.Printf("%d more frames from the host failed their CRC-32C", skipped)
				}
			}
			log.Printf("a %d-byte frame from the host (host to guest) failed its CRC-32C and was dropped (%d so far)", n, total)
		}})
	}
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
