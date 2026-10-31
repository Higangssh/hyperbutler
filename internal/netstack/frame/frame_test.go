package frame

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	frames := [][]byte{
		bytes.Repeat([]byte{1}, EthernetHeaderLen),
		bytes.Repeat([]byte{2}, 1514),
		bytes.Repeat([]byte{3}, MaxFrameLen(1500)),
	}
	var stream []byte
	for _, f := range frames {
		stream = Append(stream, f)
	}
	r, err := NewReader(bufio.NewReader(bytes.NewReader(stream)), 1500)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range frames {
		got, err := r.Next()
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("frame %d differs", i)
		}
	}
	if _, err := r.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("after last frame: %v, want EOF", err)
	}
}

func TestBadLengths(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []byte
		want error
	}{
		{"zero", []byte{0, 0, 0, 0}, ErrFrameSize},
		{"runt", []byte{0, 0, 0, 13}, ErrFrameSize},
		{"one over mtu", []byte{0, 0, 0x05, 0xEB}, ErrFrameSize}, // 1515 at MTU 1500
		{"huge", []byte{0xFF, 0xFF, 0xFF, 0xFF}, ErrFrameSize},
		{"half header", []byte{0, 0}, io.ErrUnexpectedEOF},
		{"truncated body", append([]byte{0, 0, 0, 20}, make([]byte, 10)...), io.ErrUnexpectedEOF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := NewReader(bytes.NewReader(tc.in), 1500)
			_, err := r.Next()
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestNewReaderMTU(t *testing.T) {
	for _, mtu := range []int{0, MinMTU - 1, MaxMTU + 1, -1} {
		if _, err := NewReader(bytes.NewReader(nil), mtu); err == nil {
			t.Errorf("MTU %d accepted", mtu)
		}
	}
}

// FuzzReader feeds arbitrary streams to the frame reader. Every frame it returns must be
// within bounds, and it must never panic or return more bytes than the stream holds.
func FuzzReader(f *testing.F) {
	f.Add(Append(nil, make([]byte, 60)), uint16(1500))
	f.Add([]byte{0xFF, 0xFF, 0xFF, 0xFF, 0}, uint16(1500))
	f.Add([]byte{0, 0, 0, 14}, uint16(576))
	f.Add(Append(Append(nil, make([]byte, 14)), make([]byte, 9000)), uint16(9000))
	f.Fuzz(func(t *testing.T, in []byte, mtu uint16) {
		r, err := NewReader(bytes.NewReader(in), int(mtu))
		if err != nil {
			if ValidMTU(int(mtu)) {
				t.Fatalf("valid MTU %d rejected: %v", mtu, err)
			}
			return
		}
		consumed := 0
		for {
			b, err := r.Next()
			if err != nil {
				return
			}
			if len(b) < EthernetHeaderLen || len(b) > MaxFrameLen(int(mtu)) {
				t.Fatalf("frame of %d bytes at MTU %d", len(b), mtu)
			}
			consumed += HeaderLen + len(b)
			if consumed > len(in) {
				t.Fatalf("consumed %d of %d bytes", consumed, len(in))
			}
			if !bytes.Equal(b, in[consumed-len(b):consumed]) {
				t.Fatal("frame bytes do not match the stream")
			}
		}
	})
}
