//go:build windows

package transport

import (
	"fmt"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/Higangssh/hyperbutler/internal/vmsecret"
)

// The dialer's preface is exactly the secret, and a target never prints it.
func TestSendSecret(t *testing.T) {
	s, err := vmsecret.New()
	if err != nil {
		t.Fatal(err)
	}
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	errc := make(chan error, 1)
	go func() {
		errc <- sendSecret(a, s)
		_, _ = a.Write([]byte("GET"))
	}()
	if err := vmsecret.Check(b, s); err != nil {
		t.Fatal(err)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	rest := make([]byte, 3)
	if _, err := io.ReadFull(b, rest); err != nil || string(rest) != "GET" {
		t.Fatalf("after the secret: %q, %v", rest, err)
	}

	tgt := HvsockTarget{ServiceID: VsockServiceID(2375), Secret: s}
	for _, out := range []string{tgt.String(), fmt.Sprintf("%v %+v %#v", tgt, tgt, tgt)} {
		if strings.Contains(out, s.Hex()) {
			t.Fatalf("target prints its secret: %s", out)
		}
	}
}
