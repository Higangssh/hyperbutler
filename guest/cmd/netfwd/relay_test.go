//go:build linux

package main

import (
	"bytes"
	"io"
	"testing"

	"github.com/Higangssh/winbox/internal/netstack/frame"
)

// chunkReader returns at most one queued slice per Read, like a TAP device.
type chunkReader struct{ chunks [][]byte }

func (c *chunkReader) Read(b []byte) (int, error) {
	if len(c.chunks) == 0 {
		return 0, io.EOF
	}
	n := copy(b, c.chunks[0])
	c.chunks = c.chunks[1:]
	return n, nil
}

func TestTapToHostFrames(t *testing.T) {
	f1 := bytes.Repeat([]byte{1}, 60)
	f2 := bytes.Repeat([]byte{2}, 1514)
	var out bytes.Buffer
	cr := &chunkReader{[][]byte{f1, {9, 9}, f2}}
	// One frame per batch, like a TAP with one frame queued at a time.
	read := func(buf []byte) ([]byte, error) {
		for {
			slot := make([]byte, frame.MaxFrameLen(1500))
			n, err := cr.Read(slot)
			if err != nil {
				return buf[:0], err
			}
			if n >= frame.EthernetHeaderLen {
				return frame.Append(buf[:0], slot[:n]), nil
			}
		}
	}
	err := tapToHost(read, &out, 1500)
	if err == nil {
		t.Fatal("want EOF error")
	}
	want := frame.Append(frame.Append(nil, f1), f2) // the runt is skipped
	if !bytes.Equal(out.Bytes(), want) {
		t.Fatal("framed output differs")
	}
}

func TestHostToTapFrames(t *testing.T) {
	f1 := bytes.Repeat([]byte{1}, 60)
	in := frame.Append(frame.Append(nil, f1), make([]byte, 14))
	in = append(in, 0xFF, 0, 0, 0) // bad length ends the relay
	var out bytes.Buffer
	if err := hostToTap(bytes.NewReader(in), &out, 1500); err == nil {
		t.Fatal("bad length accepted")
	}
	if out.Len() != 60+14 {
		t.Fatalf("wrote %d bytes to tap", out.Len())
	}
}
