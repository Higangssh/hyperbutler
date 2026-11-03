//go:build windows

package winipc

import (
	"bytes"
	"errors"
	"io"
	"net"
	"testing"
	"time"
	"unsafe"
)

// FuzzHvsockAddrBytes: decoding a SOCKADDR_HV from arbitrary bytes never panics, and
// what decodes re-encodes to the same address.
func FuzzHvsockAddrBytes(f *testing.F) {
	r := randLoopbackAddr().raw()
	f.Add((*[unsafe.Sizeof(rawHvsockAddr{})]byte)(unsafe.Pointer(&r))[:])
	f.Add([]byte{34, 0})
	f.Add(make([]byte, 40))
	f.Fuzz(func(t *testing.T, b []byte) {
		a, err := rawHvsockAddrFromBytes(b)
		if err != nil {
			return
		}
		r := a.raw()
		if r.Family != afHyperV || r.addr() != a {
			t.Fatalf("round trip of %x: %v", b, a)
		}
		if got := a.String(); len(got) != 73 {
			t.Fatalf("String %q", got)
		}
	})
}

// FuzzDeadline drives a deadline through arbitrary sequences of moves and checks that
// its channel is closed exactly when the last move put it in the past.
func FuzzDeadline(f *testing.F) {
	f.Add([]byte{0, 1, 2, 1, 0, 2, 2, 1})
	f.Add([]byte{1, 1, 1})
	f.Add([]byte{2, 0, 3, 3, 1})
	f.Fuzz(func(t *testing.T, ops []byte) {
		if len(ops) > 64 {
			ops = ops[:64]
		}
		var d deadline
		for i, op := range ops {
			var want bool
			switch op % 4 {
			case 0:
				d.set(time.Time{})
			case 1:
				d.set(time.Now().Add(-time.Second))
				want = true
			case 2:
				d.set(time.Now().Add(time.Hour))
			case 3:
				// A deadline that passes almost at once.
				d.set(time.Now().Add(time.Microsecond))
				select {
				case <-d.wait():
				case <-time.After(5 * time.Second):
					t.Fatalf("op %d: short deadline never fired", i)
				}
				want = true
			}
			select {
			case <-d.wait():
				if !want {
					t.Fatalf("op %d (%d): deadline fired", i, op)
				}
			default:
				if want {
					t.Fatalf("op %d (%d): deadline did not fire", i, op)
				}
			}
		}
		d.set(time.Time{}) // stops any timer
	})
}

// transfer writes data to w in chunks of wchunk bytes, half-closes, and reads it back
// from r with a buffer of rchunk bytes.
func transfer(t *testing.T, w, r net.Conn, data []byte, wchunk, rchunk int) {
	t.Helper()
	errc := make(chan error, 1)
	go func() {
		rest := data
		for len(rest) > 0 {
			n := min(wchunk, len(rest))
			if _, err := w.Write(rest[:n]); err != nil {
				errc <- err
				return
			}
			rest = rest[n:]
		}
		errc <- w.(interface{ CloseWrite() error }).CloseWrite()
	}()
	_ = r.SetReadDeadline(time.Now().Add(10 * time.Second))
	var got bytes.Buffer
	buf := make([]byte, rchunk)
	for {
		n, err := r.Read(buf)
		got.Write(buf[:n])
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read after %d bytes: %v", got.Len(), err)
		}
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), data) {
		t.Fatalf("got %d bytes, want %d", got.Len(), len(data))
	}
}

func fuzzSizes(data []byte, wchunk, rchunk uint16, repeat uint8) ([]byte, int, int) {
	if r := int(repeat % 16); r > 1 {
		data = bytes.Repeat(data, r*1024)
	}
	if len(data) > 4<<20 {
		data = data[:4<<20]
	}
	return data, int(wchunk) + 1, int(rchunk) + 1
}

// FuzzPipeTransfer: any data, written and read in any chunk sizes over a message-type
// pipe in either direction, arrives intact and is followed by EOF.
func FuzzPipeTransfer(f *testing.F) {
	f.Add([]byte("hello"), uint16(1), uint16(1), uint8(0), false)
	f.Add([]byte{0}, uint16(65535), uint16(7), uint8(15), true)
	f.Add([]byte("x"), uint16(4096), uint16(65535), uint8(9), false)
	f.Fuzz(func(t *testing.T, data []byte, wchunk, rchunk uint16, repeat uint8, fromClient bool) {
		data, wc, rc := fuzzSizes(data, wchunk, rchunk, repeat)
		client, server := pipePair(t, true)
		if fromClient {
			transfer(t, client, server, data, wc, rc)
		} else {
			transfer(t, server, client, data, wc, rc)
		}
	})
}

// FuzzHvsockTransfer is FuzzPipeTransfer over a loopback Hyper-V socket.
func FuzzHvsockTransfer(f *testing.F) {
	f.Add([]byte("hello"), uint16(1), uint16(1), uint8(0), false)
	f.Add([]byte{0}, uint16(65535), uint16(7), uint8(15), true)
	f.Fuzz(func(t *testing.T, data []byte, wchunk, rchunk uint16, repeat uint8, fromClient bool) {
		data, wc, rc := fuzzSizes(data, wchunk, rchunk, repeat)
		client, server := hvPair(t)
		if fromClient {
			transfer(t, client, server, data, wc, rc)
		} else {
			transfer(t, server, client, data, wc, rc)
		}
	})
}
