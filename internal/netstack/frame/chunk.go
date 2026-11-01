package frame

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// ChunkReader reads a framed stream in large chunks that each hold whole frames only.
// Unlike Reader it does not copy frames out of its buffer, and with Pump one goroutine
// can keep the link drained while another one handles the frames. Every length prefix
// is checked before the chunk is returned, and with WithCRC every frame's CRC. It is
// not safe for concurrent use.
type ChunkReader struct {
	r        io.Reader
	min, max int
	tail     []byte // the start of an incomplete frame, carried into the next chunk
	err      error  // a read error seen after the last whole frame, returned next
	crc      *CRCCheck
}

// NewChunkReader returns a ChunkReader for frames of min..max bytes (length prefix not
// included).
func NewChunkReader(r io.Reader, min, max int) *ChunkReader {
	return &ChunkReader{r: r, min: min, max: max, tail: make([]byte, 0, HeaderLen+max)}
}

// WithCRC makes c read a version 2 stream: every frame of min..max bytes has a CRC in
// front, which ReadChunk checks. A frame that fails the check is left out of the chunk
// (chk.Bad is told), or with chk.Strict ends the stream with ErrCRC after the frames
// before it. EachFrame then strips the CRC. Call it before the first ReadChunk.
func (c *ChunkReader) WithCRC(chk CRCCheck) *ChunkReader {
	c.crc = &chk
	c.min += CRCLen
	c.max += CRCLen
	c.tail = make([]byte, 0, HeaderLen+c.max)
	return c
}

// MinChunk is the smallest buffer ReadChunk accepts: one frame of the maximum size.
func (c *ChunkReader) MinChunk() int { return HeaderLen + c.max }

// ReadChunk reads into buf (all of its capacity) until it holds at least one whole
// frame, and returns the whole frames, length prefixes included; walk them with
// c.EachFrame. It returns after the first read that completes a frame, so a chunk is
// whatever the link had ready, not a full buffer. An out-of-range length returns
// ErrFrameSize, after which the stream is out of sync. At the end of the stream it
// returns io.EOF, or io.ErrUnexpectedEOF if a frame was cut off.
func (c *ChunkReader) ReadChunk(buf []byte) ([]byte, error) {
	if c.err != nil {
		return nil, c.err
	}
	buf = buf[:cap(buf)]
	if len(buf) < c.MinChunk() {
		return nil, fmt.Errorf("frame: chunk buffer of %d bytes is smaller than %d", len(buf), c.MinChunk())
	}
	n := copy(buf, c.tail)
	empty := 0 // reads that returned nothing and no error
	for {
		end, err := c.scan(buf[:n])
		if end == 0 && err == nil {
			k, rerr := c.r.Read(buf[n:])
			n += k
			if k == 0 && rerr == nil {
				if empty++; empty >= 100 {
					rerr = io.ErrNoProgress
				}
			}
			if rerr == nil {
				continue
			}
			if end, err = c.scan(buf[:n]); err == nil {
				err = c.endErr(rerr, n-end)
			}
		}
		keep := end
		if c.crc != nil && end > 0 {
			var cerr error
			if keep, cerr = c.check(buf[:end]); cerr != nil {
				err = cerr
			}
		}
		// Whole frames before a bad length, a bad CRC in strict mode or the end of the
		// stream are still returned; the error comes with the next call.
		c.err = err
		c.tail = c.tail[:0]
		if err == nil {
			if keep == 0 { // every frame failed its CRC: read on
				n = copy(buf, buf[end:n])
				continue
			}
			c.tail = append(c.tail, buf[end:n]...)
		}
		if keep == 0 {
			return nil, err
		}
		return buf[:keep], nil
	}
}

// check verifies the CRC of every frame in b, whole frames that scan accepted, and
// moves the good ones together at the start of b. It returns their length, and in
// strict mode ErrCRC at the first bad frame (keeping only the frames before it).
func (c *ChunkReader) check(b []byte) (int, error) {
	keep := 0
	for off := 0; off < len(b); {
		n := HeaderLen + int(binary.BigEndian.Uint32(b[off:]))
		f := b[off : off+n]
		off += n
		if !CheckCRC(f[HeaderLen:]) {
			if c.crc.Bad != nil {
				c.crc.Bad(n - HeaderLen)
			}
			if c.crc.Strict {
				return keep, fmt.Errorf("%w: frame of %d bytes", ErrCRC, n-HeaderLen)
			}
			continue
		}
		if keep != off-n {
			copy(b[keep:], f)
		}
		keep += n
	}
	return keep, nil
}

// endErr is the error for a stream that ended with rest bytes of an incomplete frame.
func (c *ChunkReader) endErr(err error, rest int) error {
	if errors.Is(err, io.EOF) && rest > 0 {
		return io.ErrUnexpectedEOF
	}
	return err
}

// scan returns the length of the whole frames at the start of b, checking each prefix,
// and an error if a bad prefix follows them.
func (c *ChunkReader) scan(b []byte) (int, error) {
	end := 0
	for len(b)-end >= HeaderLen {
		n := binary.BigEndian.Uint32(b[end:])
		if int64(n) < int64(c.min) || int64(n) > int64(c.max) {
			return end, fmt.Errorf("%w: %d (allowed %d..%d)", ErrFrameSize, n, c.min, c.max)
		}
		if len(b)-end-HeaderLen < int(n) {
			break
		}
		end += HeaderLen + int(n)
	}
	return end, nil
}

// EachFrame calls fn for every frame of a chunk from c.ReadChunk, in order, without
// its CRC, and stops at the first error.
func (c *ChunkReader) EachFrame(chunk []byte, fn func([]byte) error) error {
	skip := 0
	if c.crc != nil {
		skip = CRCLen
	}
	return eachFrame(chunk, skip, fn)
}

// EachFrame calls fn for every frame of a chunk from ReadChunk, in order, and stops at
// the first error. A version 2 chunk's frames start with their CRC; ChunkReader's
// EachFrame strips it.
func EachFrame(chunk []byte, fn func([]byte) error) error { return eachFrame(chunk, 0, fn) }

func eachFrame(chunk []byte, skip int, fn func([]byte) error) error {
	for len(chunk) >= HeaderLen {
		n := int(binary.BigEndian.Uint32(chunk))
		if HeaderLen+n > len(chunk) || n < skip {
			return io.ErrUnexpectedEOF // not a chunk from ReadChunk
		}
		if err := fn(chunk[HeaderLen+skip : HeaderLen+n]); err != nil {
			return err
		}
		chunk = chunk[HeaderLen+n:]
	}
	return nil
}

// Pump reads chunks of up to size bytes from c on its own goroutine, nbuf buffers
// ahead, and calls handle for every frame on the calling goroutine, in order. Reading
// and handling then overlap: a slow handler no longer stops the reader from emptying
// the link's buffers, and with WithCRC the CRCs are checked on the reader's goroutine,
// while the handler works on the chunk before. It returns the first error of either
// side. When handle fails, Pump calls stop, which must make the reader's pending Read
// return (close the stream or set a deadline), and waits for the reader to finish.
func Pump(c *ChunkReader, nbuf, size int, handle func([]byte) error, stop func()) error {
	size = max(size, c.MinChunk())
	free := make(chan []byte, nbuf)
	for i := 0; i < nbuf; i++ {
		free <- make([]byte, size)
	}
	full := make(chan []byte, nbuf)
	quit := make(chan struct{})
	var rerr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer close(full)
		for {
			var buf []byte
			select {
			case buf = <-free:
			case <-quit:
				return
			}
			chunk, err := c.ReadChunk(buf)
			if err != nil {
				rerr = err
				return
			}
			full <- chunk
		}
	}()
	var herr error
	for chunk := range full {
		if herr = c.EachFrame(chunk, handle); herr != nil {
			break
		}
		free <- chunk[:0]
	}
	if herr != nil {
		close(quit)
		stop()
		for range full { // the reader may be blocked sending one more chunk
		}
		<-done
		return herr
	}
	<-done
	return rerr
}
