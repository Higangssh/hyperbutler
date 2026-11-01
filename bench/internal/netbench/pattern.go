package netbench

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"sync/atomic"
	"time"
)

// The bulk streams carry a pattern instead of zeros, so that the benchmark notices data
// that arrives different from what was sent: the guest link trusts its frames without a
// checksum (ADR 0006, "Integrity"), and a stream of zeros stays zeros when one frame's
// bytes land in another's place.
//
// A stream is a sequence of BlockSize blocks. Block k is block k%patternBlocks of a
// 1 MiB table of xorshift64 output from the stream's seed, with its first 8 bytes
// replaced by k, so a block that arrives at another block's place does not match
// either. Writing one costs no copy: the writer keeps the table as BufSize write
// buffers and stamps the four block numbers before each write. The table is small so
// that the reader compares against it in cache.
const (
	// BlockSize is the unit of the pattern and of Result.Corrupt.
	BlockSize     = 64 << 10
	patternBlocks = 16
	blocksPerBuf  = BufSize / BlockSize
)

// VerifyEvery is how many blocks of a download are checked: one in VerifyEvery, the
// first included. On the host's loopback (about 27 Gbit/s, 2026-11-01) checking every
// block cost the download about a third of its rate, every 4th about a fifth and every
// 8th about 4%, so the host baseline would have measured the check more than the
// path. The bug that made it worth having changed many frames in a burst, which one
// block in 8 still sees. Upload checks every byte (CRC-32C, about 30 GB/s).
var VerifyEvery int64 = 8

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// Pattern is the table of one seed.
type Pattern struct {
	table []byte // patternBlocks blocks
}

// NewPattern builds the table for seed.
func NewPattern(seed uint64) *Pattern {
	t := make([]byte, patternBlocks*BlockSize)
	x := seed ^ 0x9e3779b97f4a7c15
	if x == 0 {
		x = 1 // xorshift needs a nonzero state
	}
	for i := 0; i < len(t); i += 8 {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		binary.LittleEndian.PutUint64(t[i:], x)
	}
	return &Pattern{table: t}
}

// Block appends block k to dst.
func (p *Pattern) Block(dst []byte, k int64) []byte {
	var s [8]byte
	binary.BigEndian.PutUint64(s[:], uint64(k)) //nolint:gosec // G115: block numbers are not negative
	dst = append(dst, s[:]...)
	return append(dst, p.tableBlock(k)[8:]...)
}

func (p *Pattern) tableBlock(k int64) []byte {
	i := k % patternBlocks
	return p.table[i*BlockSize : (i+1)*BlockSize]
}

// patternWriter hands out the stream's write buffers in order.
type patternWriter struct {
	bufs [patternBlocks / blocksPerBuf][]byte
	k    int64 // the next block
}

func newPatternWriter(p *Pattern) *patternWriter {
	w := &patternWriter{}
	for j := range w.bufs {
		// Buffer j holds table blocks j*blocksPerBuf.., which is where block k lands
		// whenever (k/blocksPerBuf) % len(bufs) == j.
		w.bufs[j] = append([]byte(nil), p.table[j*BufSize:(j+1)*BufSize]...)
	}
	return w
}

// next returns the next BufSize bytes of the stream. The buffer is the writer's: it is
// valid until the call that returns it again, len(bufs) calls later.
func (w *patternWriter) next() []byte {
	b := w.bufs[(w.k/blocksPerBuf)%int64(len(w.bufs))]
	for i := int64(0); i < blocksPerBuf; i++ {
		binary.BigEndian.PutUint64(b[i*BlockSize:], uint64(w.k+i)) //nolint:gosec // G115: block numbers are not negative
	}
	w.k += blocksPerBuf
	return b
}

// verifier checks a stream as it is read, one block in every.
type verifier struct {
	p       *Pattern
	every   int64
	off     int64 // stream offset of the next byte
	bad     int64 // the last block counted as corrupt, plus 1
	corrupt int64
}

func newVerifier(p *Pattern, every int64) *verifier {
	return &verifier{p: p, every: max(1, every)}
}

func (v *verifier) check(b []byte) {
	for len(b) > 0 {
		k, o := v.off/BlockSize, int(v.off%BlockSize)
		n := min(len(b), BlockSize-o)
		if k%v.every == 0 && v.bad != k+1 && !v.blockMatches(k, o, b[:n]) {
			v.corrupt++
			v.bad = k + 1
		}
		b = b[n:]
		v.off += int64(n)
	}
}

// blockMatches reports whether b is block k's bytes from offset o.
func (v *verifier) blockMatches(k int64, o int, b []byte) bool {
	if o < 8 {
		var s [8]byte
		binary.BigEndian.PutUint64(s[:], uint64(k)) //nolint:gosec // G115: block numbers are not negative
		m := min(8-o, len(b))
		if !bytes.Equal(b[:m], s[o:o+m]) {
			return false
		}
		b, o = b[m:], o+m
	}
	return bytes.Equal(b, v.p.tableBlock(k)[o:o+len(b)])
}

const sinkReportLen = 12

// sinkReport is a Sink's answer: the byte count and the CRC-32C of what it read.
func sinkReport(n int64, sum uint32) []byte {
	b := make([]byte, sinkReportLen)
	binary.BigEndian.PutUint64(b, uint64(n)) //nolint:gosec // G115: a byte count, never negative
	binary.BigEndian.PutUint32(b[8:], sum)
	return b
}

var seedCount atomic.Uint64

// newSeed returns a seed that differs between streams.
func newSeed() uint64 {
	return uint64(time.Now().UnixNano())*0x9e3779b97f4a7c15 + seedCount.Add(1)
}
