package guestctl

import (
	"bytes"
	"errors"
	"io"
	"math"
	"testing"
	"time"

	"github.com/Higangssh/hyperbutler/internal/spaceguard"
)

func sampleMsgs() []Msg {
	return []Msg{
		Hello{Min: 1, Max: 1},
		Hello{Min: 1, Max: 7},
		Budget{spaceguard.Budget{Bytes: 64 << 20, Backed: 1 << 30}},
		Budget{spaceguard.Budget{Unlimited: true}},
		Budget{spaceguard.Budget{Bytes: math.MaxUint64, Backed: math.MaxUint64}},
		TrimRequest{},
		CacheTrimRequest{},
		Ping{Nonce: 0xdeadbeefcafe},
		HelloReply{Version: 1, Features: FeatureGuard | FeatureTrim | FeatureCacheTrim, Instance: 42},
		Status{Guard: GuardOK, GuardErrno: 28, Device: 2 << 30, FSSize: 1 << 30, FSFree: 1 << 29, Reserved: 1, Target: 2, Need: 3, Short: 4,
			Trims: 5, Trimmed: 6, LastTrimAge: math.MaxUint64, MemTotal: 7, MemFree: 8, FileCache: 9, Shmem: 10, CacheTrims: 11, CacheFreed: 12},
		TrimResult{Errno: 0, Trimmed: 1 << 30, Duration: 1500 * time.Millisecond},
		CacheResult{Method: CacheReclaim, Before: 2 << 30, After: 1 << 20, Duration: 30 * time.Millisecond},
		Pong{Nonce: 1},
		ErrorReply{Code: ErrBusy, Request: TTrim},
	}
}

func dirOf(m Msg) Direction {
	if m.Type() >= 0x80 {
		return FromGuest
	}
	return FromHost
}

func TestRoundTrip(t *testing.T) {
	for _, m := range sampleMsgs() {
		f, err := Encode(77, m)
		if err != nil {
			t.Fatalf("%v: %v", m.Type(), err)
		}
		var buf [MaxBody]byte
		body, err := ReadFrame(bytes.NewReader(f), &buf)
		if err != nil {
			t.Fatalf("%v: %v", m.Type(), err)
		}
		seq, got, err := Decode(body, dirOf(m))
		if err != nil {
			t.Fatalf("%v: %v", m.Type(), err)
		}
		if seq != 77 || got != m {
			t.Errorf("%v: got seq %d %#v, want %#v", m.Type(), seq, got, m)
		}
		// The other direction must refuse it.
		if _, _, err := Decode(body, 1-dirOf(m)); err == nil {
			t.Errorf("%v decoded in the wrong direction", m.Type())
		}
	}
}

func TestEncodeRejects(t *testing.T) {
	for _, m := range []Msg{
		nil,
		Hello{Min: 0, Max: 1},
		Hello{Min: 2, Max: 1},
		Budget{spaceguard.Budget{Unlimited: true, Bytes: 1}},
		HelloReply{Version: 2},
		HelloReply{Version: 1, Features: 1 << 9},
		Status{Guard: 9},
		Status{GuardErrno: 4096},
		TrimResult{Errno: 5000},
		TrimResult{Duration: -1},
		CacheResult{Method: 3},
		ErrorReply{Code: 0, Request: TTrim},
		ErrorReply{Code: ErrBusy, Request: THello},
		ErrorReply{Code: 3, Request: TTrim},
	} {
		if _, err := Encode(1, m); err == nil {
			t.Errorf("Encode(%#v) succeeded", m)
		}
	}
}

func TestDecodeStrict(t *testing.T) {
	good, _ := Encode(1, Budget{spaceguard.Budget{Bytes: 5, Backed: 6}})
	body := good[4:]
	mut := func(i int, v byte) []byte {
		b := bytes.Clone(body)
		b[i] = v
		return b
	}
	cases := map[string][]byte{
		"version 0":         mut(0, 0),
		"version 2":         mut(0, 2),
		"reserved":          mut(3, 1),
		"unknown type":      mut(1, 0x7f),
		"guest type":        mut(1, byte(RStatus)),
		"flag 2":            mut(HeaderLen, 2),
		"padding":           mut(HeaderLen+3, 1),
		"short":             body[:len(body)-1],
		"long":              append(bytes.Clone(body), 0),
		"header only":       body[:HeaderLen],
		"truncated header":  body[:3],
		"unlimited + bytes": mut(HeaderLen, 1),
	}
	for name, b := range cases {
		if _, _, err := Decode(b, FromHost); err == nil {
			t.Errorf("%s: decoded", name)
		}
	}
	tr, _ := Encode(1, TrimResult{})
	b := bytes.Clone(tr[4:])
	be.PutUint64(b[HeaderLen+16:], maxDurationMS+1)
	if _, _, err := Decode(b, FromGuest); err == nil {
		t.Error("overflowing duration decoded")
	}
}

func TestReadFrameLimits(t *testing.T) {
	var buf [MaxBody]byte
	for _, n := range []uint32{0, HeaderLen - 1, MaxBody + 1, math.MaxUint32} {
		var hdr [4]byte
		be.PutUint32(hdr[:], n)
		if _, err := ReadFrame(bytes.NewReader(hdr[:]), &buf); !errors.Is(err, ErrFrameTooLarge) {
			t.Errorf("length %d: %v", n, err)
		}
	}
	var hdr [4]byte
	be.PutUint32(hdr[:], 20)
	if _, err := ReadFrame(bytes.NewReader(append(hdr[:], 1, 2)), &buf); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("truncated body: %v", err)
	}
	if _, err := ReadFrame(bytes.NewReader(nil), &buf); !errors.Is(err, io.EOF) {
		t.Errorf("empty: %v", err)
	}
}
