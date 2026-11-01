//go:build windows

package socktable

import (
	"net"
	"net/netip"
	"os"
	"testing"
)

// TestReaderSeesLoopbackListener: a listener on 127.0.0.1 only (never a wildcard)
// shows up in the listener table with this process as its owner.
func TestReaderSeesLoopbackListener(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	ap := netip.MustParseAddrPort(ln.Addr().String())

	var r Reader
	ss, err := r.TCPListeners()
	if err != nil {
		t.Fatal(err)
	}
	s, ok := Holder(ss, ap.Port())
	if !ok {
		t.Fatalf("port %d not in the listener table (%d rows)", ap.Port(), len(ss))
	}
	if s.Local != ap || s.State != "LISTEN" || s.PID != uint32(os.Getpid()) {
		t.Errorf("row %+v, want %s LISTEN pid %d", s, ap, os.Getpid())
	}
	for _, s := range ss {
		if s.State != "LISTEN" {
			t.Fatalf("listener table has a %s row: %+v", s.State, s)
		}
	}

	// The full table has the listener too, read again with the same buffer.
	all, err := r.All()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := Holder(all, ap.Port()); !ok {
		t.Error("port not in the full table")
	}
	if _, err := r.UDP(); err != nil {
		t.Error(err)
	}

	_ = ln.Close()
	ss, err = r.TCPListeners()
	if err != nil {
		t.Fatal(err)
	}
	if s, ok := Holder(ss, ap.Port()); ok {
		t.Errorf("closed listener still listed: %+v", s)
	}
}
