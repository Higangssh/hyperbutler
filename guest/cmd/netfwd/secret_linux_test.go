//go:build linux

package main

import (
	"errors"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Higangssh/hyperbutler/internal/vmsecret"
)

func linkTestSecret() vmsecret.Secret {
	var s vmsecret.Secret
	for i := range s {
		s[i] = byte(0x50 + i)
	}
	return s
}

// linkPair returns a non-blocking, poller-driven file, as dialHost returns it, and its
// peer, which plays the host's listener.
func linkPair(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	conn := os.NewFile(uintptr(fds[0]), "vsock")
	peer := os.NewFile(uintptr(fds[1]), "host")
	t.Cleanup(func() {
		_ = conn.Close()
		_ = peer.Close()
	})
	return conn, peer
}

// netfwd's check of a plain-mode listener (ADR 0004, two-account run): a listener that
// sends nothing, too little or the wrong bytes, or closes, is refused, the silent ones
// within the timeout and the others at once; the host's secret passes, and the first
// frame read starts right after it.
func TestVerifyPlainListener(t *testing.T) {
	s := linkTestSecret()
	wrong := s
	wrong[0] ^= 1
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
			conn, peer := linkPair(t)
			if tc.send != nil {
				if _, err := peer.Write(tc.send); err != nil {
					t.Fatal(err)
				}
			}
			if tc.close {
				_ = peer.Close()
			}
			start := time.Now()
			err := vmsecret.Verify(conn, s)
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
		conn, peer := linkPair(t)
		if _, err := peer.Write(append(s[:], "HELLO"...)); err != nil {
			t.Fatal(err)
		}
		if err := vmsecret.Verify(conn, s); err != nil {
			t.Fatal(err)
		}
		got := make([]byte, 5)
		if _, err := io.ReadFull(conn, got); err != nil || string(got) != "HELLO" {
			t.Fatalf("after the secret: %q, %v", got, err)
		}
	})
}

// dialLoop closes a listener that does not send the secret, without writing to it, and
// dials again after its backoff until the host's listener answers.
func TestDialLoopRefusesImpostor(t *testing.T) {
	s := linkTestSecret()
	var (
		mu    sync.Mutex
		dials int
		peers []*os.File
	)
	dial := func() (*os.File, error) {
		mu.Lock()
		defer mu.Unlock()
		dials++
		conn, peer := linkPair(t)
		peers = append(peers, peer)
		send := make([]byte, vmsecret.Size) // two impostors with a zero secret
		if dials > 2 {
			send = append(s[:], "FRAME"...)
		}
		if _, err := peer.Write(send); err != nil {
			return nil, err
		}
		return conn, nil
	}
	start := time.Now()
	conn := dialLoop(dial, s)()
	if d := time.Since(start); d < 100*time.Millisecond {
		t.Fatalf("redialled without a backoff (%v)", d)
	}
	mu.Lock()
	defer mu.Unlock()
	if dials != 3 {
		t.Fatalf("dials = %d, want 3", dials)
	}
	got := make([]byte, 5)
	if _, err := io.ReadFull(conn, got); err != nil || string(got) != "FRAME" {
		t.Fatalf("after the secret: %q, %v", got, err)
	}
	for i, p := range peers[:2] {
		_ = p.SetReadDeadline(time.Now().Add(2 * time.Second))
		if b, err := io.ReadAll(p); len(b) != 0 || err != nil {
			t.Fatalf("impostor %d: netfwd wrote %d byte(s) or did not close (%v)", i, len(b), err)
		}
	}
}
