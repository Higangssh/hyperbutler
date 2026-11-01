package netbench

import (
	"bytes"
	"context"
	"encoding/binary"
	"hash/crc32"
	"net"
	"testing"
	"time"
)

// TestPatternWriter: the writer's buffers are the blocks of the pattern, in order,
// across the table's wrap.
func TestPatternWriter(t *testing.T) {
	p := NewPattern(42)
	w := newPatternWriter(p)
	var want []byte
	for k := int64(0); k < 3*patternBlocks; k++ {
		want = p.Block(want, k)
	}
	var got []byte
	for len(got) < len(want) {
		got = append(got, w.next()...)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("the writer's stream differs from the pattern's blocks")
	}
	if bytes.Equal(p.Block(nil, 1)[8:], NewPattern(43).Block(nil, 1)[8:]) {
		t.Fatal("two seeds give the same pattern")
	}
}

// TestVerifier: a stream read in any pieces checks clean; a changed byte, a block
// that arrives at another block's place and a block of zeros are each one corrupt
// block, and with every > 1 only every-th block is checked.
func TestVerifier(t *testing.T) {
	p := NewPattern(7)
	var stream []byte
	for k := int64(0); k < 2*patternBlocks+5; k++ {
		stream = p.Block(stream, k)
	}
	feed := func(b []byte, every int64) int64 {
		v := newVerifier(p, every)
		for i := 0; len(b) > 0; i++ {
			n := min(len(b), 1+(i*7919)%(3*BlockSize))
			v.check(b[:n])
			b = b[n:]
		}
		return v.corrupt
	}
	if c := feed(stream, 1); c != 0 {
		t.Fatalf("clean stream: %d corrupt blocks", c)
	}
	bad := bytes.Clone(stream)
	bad[3*BlockSize+100] ^= 1                                                                     // block 3
	copy(bad[10*BlockSize:11*BlockSize], stream[(10+patternBlocks)*BlockSize:])                   // block 10: block 26's bytes
	copy(bad[20*BlockSize+4:21*BlockSize], make([]byte, BlockSize-4))                             // block 20: zeros
	binary.BigEndian.PutUint64(bad[30*BlockSize:], binary.BigEndian.Uint64(bad[30*BlockSize:])+1) // block 30: wrong number
	if c := feed(bad, 1); c != 4 {
		t.Fatalf("corrupted stream: %d corrupt blocks, want 4", c)
	}
	if c := feed(bad, 10); c != 3 { // blocks 10, 20 and 30
		t.Fatalf("every 10th block: %d corrupt blocks, want 3", c)
	}
}

// TestUploadDetectsCorruption: a sink that reads other bytes than were sent reports a
// CRC that makes the upload corrupt.
func TestUploadDetectsCorruption(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		// A sink whose link flips one bit of the first byte.
		buf := make([]byte, BufSize)
		var n int64
		var sum uint32
		for {
			k, err := c.Read(buf)
			if n == 0 && k > 0 {
				buf[0] ^= 1
			}
			n += int64(k)
			sum = crc32.Update(sum, castagnoli, buf[:k])
			if err != nil {
				break
			}
		}
		_, _ = c.Write(sinkReport(n, sum))
	}()
	r, err := Upload(context.Background(), ln.Addr().String(), 50*time.Millisecond)
	<-done
	if err != nil || r.Corrupt != 1 || r.FirstError == "" {
		t.Fatalf("upload through a corrupting sink: %+v %v", r, err)
	}
}

// TestDownloadDetectsCorruption: a source whose stream has a changed byte is caught.
func TestDownloadDetectsCorruption(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		var hdr [8]byte
		binary.BigEndian.PutUint64(hdr[:], 99)
		if _, err := c.Write(hdr[:]); err != nil {
			return
		}
		w := newPatternWriter(NewPattern(99))
		for i := 0; ; i++ {
			b := w.next()
			if i == 2 {
				b = bytes.Clone(b)
				b[9] ^= 0x80 // block 8, which VerifyEvery checks
			}
			if _, err := c.Write(b); err != nil {
				return
			}
		}
	}()
	r, err := Download(context.Background(), ln.Addr().String(), 50*time.Millisecond)
	ln.Close()
	<-done
	if err != nil || r.Corrupt != 1 {
		t.Fatalf("download from a corrupting source: %+v %v", r, err)
	}
}
