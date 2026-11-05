//go:build linux

package main

import (
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Higangssh/hyperbutler/internal/vmsecret"
)

func shareTestSecret() vmsecret.Secret {
	var s vmsecret.Secret
	for i := range s {
		s[i] = byte(0xa0 + i)
	}
	return s
}

// socketpair returns a non-blocking socket, as shareAcceptor.accept has it before the check, and
// its peer, which plays the share server.
func socketpair(t *testing.T) (int, *os.File) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(fds[0]) })
	peer := os.NewFile(uintptr(fds[1]), "share-server")
	t.Cleanup(func() { _ = peer.Close() })
	return fds[0], peer
}

// The share relay's check of the listener (ADR 0004, two-account run): a listener that
// sends nothing, too little or the wrong bytes, or closes, is refused, the silent ones
// within the timeout and the others at once; the host's secret passes, and the relay's
// next read starts right after it.
func TestShareVerifyListener(t *testing.T) {
	s := shareTestSecret()
	wrong := s
	wrong[3] ^= 1
	for _, tc := range []struct {
		name     string
		send     []byte
		close    bool
		min, max time.Duration
	}{
		{"nothing", nil, false, 900 * time.Millisecond, 1500 * time.Millisecond},
		{"31 bytes", s[:vmsecret.Size-1], false, 900 * time.Millisecond, 1500 * time.Millisecond},
		{"wrong secret", wrong[:], false, 0, 500 * time.Millisecond},
		{"closed at once", nil, true, 0, 500 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fd, peer := socketpair(t)
			if tc.send != nil {
				if _, err := peer.Write(tc.send); err != nil {
					t.Fatal(err)
				}
			}
			if tc.close {
				_ = peer.Close()
			}
			start := time.Now()
			err := vmsecret.Verify(&pollReader{fd: fd}, s)
			d := time.Since(start)
			if !errors.Is(err, vmsecret.ErrMismatch) {
				t.Fatalf("Verify: %v, want ErrMismatch", err)
			}
			if d < tc.min || d > tc.max {
				t.Fatalf("refused after %v, want %v to %v", d, tc.min, tc.max)
			}
		})
	}

	t.Run("host", func(t *testing.T) {
		fd, peer := socketpair(t)
		if _, err := peer.Write(append(s[:], "HELLO"...)); err != nil {
			t.Fatal(err)
		}
		r := &pollReader{fd: fd}
		if err := vmsecret.Verify(r, s); err != nil {
			t.Fatal(err)
		}
		got := make([]byte, 5)
		if _, err := io.ReadFull(r, got); err != nil || string(got) != "HELLO" {
			t.Fatalf("after the secret: %q, %v", got, err)
		}
	})
}
