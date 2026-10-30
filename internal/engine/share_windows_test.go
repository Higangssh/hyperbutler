//go:build windows

package engine

import (
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"
)

func TestSetupSharesOff(t *testing.T) {
	s, svcs, args := setupShares(ShareConfig{Off: true}, "D:P", slog.Default())
	if s != nil || svcs != nil || args != "" {
		t.Fatalf("off: got %v %v %q", s, svcs, args)
	}
	s, svcs, args = setupShares(ShareConfig{}, "D:P(A;;GA;;;S-1-5-21-1)", slog.Default())
	defer s.close()
	if len(svcs) != 1 || svcs[0].BindSDDL != "D:P(A;;GA;;;S-1-5-21-1)" || svcs[0].ConnectSDDL != "" || args != " hyperbutler.share=2385" {
		t.Fatalf("got %+v %q", svcs, args)
	}
}

// helloFrame is the relay's first frame on a connection (relay.helloFrame).
func helloFrame(queue uint32) []byte {
	b := make([]byte, 48)
	binary.LittleEndian.PutUint32(b[0:], 48)
	binary.LittleEndian.PutUint32(b[4:], 0x77620001)
	binary.LittleEndian.PutUint32(b[44:], queue)
	return b
}

// TestShareServiceCloseEndsConnections: close stops accepting, closes live guest
// connections (an idle session and a half-sent frame) and leaves no goroutine behind
// (checked by TestMain).
func TestShareServiceCloseEndsConnections(t *testing.T) {
	s, _, _ := setupShares(ShareConfig{}, "D:P", slog.Default())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.serve(ln)
	if _, err := s.rewriteCreate([]byte(`{"HostConfig":{"Binds":["C:\\x:/x"]}}`)); err == nil || !strings.Contains(err.Error(), "not mounted") {
		t.Fatalf("rewrite before the guest mounted: %v", err)
	}
	var clients []net.Conn
	for i, first := range [][]byte{helloFrame(0), helloFrame(0)[:20]} {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if _, err := c.Write(first); err != nil {
			t.Fatal(err)
		}
		clients = append(clients, c)
		if i == 0 {
			deadline := time.Now().Add(5 * time.Second)
			for !s.srv.Mounted() {
				if time.Now().After(deadline) {
					t.Fatal("the session never started")
				}
				time.Sleep(5 * time.Millisecond)
			}
		}
	}
	// Names are passed through untouched without Windows sources.
	body := []byte(`{"HostConfig":{"Binds":["vol:/x"]}}`)
	if out, err := s.rewriteCreate(body); err != nil || string(out) != string(body) {
		t.Fatalf("named volume: %s %v", out, err)
	}
	done := make(chan struct{})
	go func() { s.close(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("close did not return")
	}
	for _, c := range clients {
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		// EOF or a reset (Windows resets a socket closed with unread data); a timeout
		// means the host never closed it.
		var ne net.Error
		if _, err := io.ReadAll(c); errors.As(err, &ne) && ne.Timeout() {
			t.Fatalf("connection not closed by the host: %v", err)
		}
	}
	if s.srv.Mounted() {
		t.Fatal("still mounted after close")
	}
	// A late serve after close is refused at once.
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.serve(ln2)
	if _, err := ln2.Accept(); err == nil {
		t.Fatal("listener still open")
	}
}
