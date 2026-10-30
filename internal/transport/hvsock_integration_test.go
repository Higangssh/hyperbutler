//go:build integration && windows

// Integration test against a real guest. It needs a running hyperbutler utility VM whose
// dockerd listens on AF_VSOCK, and an elevated shell (HCS and AF_HYPERV connects to a
// utility VM require it on Windows 11 Home; see docs/adr/0002-primary-vm-backend.md).
//
// Run it with:
//
//	$env:HYPERBUTLER_TEST_HVSOCK = "<vm-guid>:vsock:2375"   # or <vm-guid>:<service-guid>
//	go test -tags integration -run Integration -v ./internal/transport/
//
// Windows edition: not yet run; record the edition and build here when it is.
package transport

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
)

func integrationTarget(t *testing.T) HvsockTarget {
	t.Helper()
	spec := os.Getenv("HYPERBUTLER_TEST_HVSOCK")
	if spec == "" {
		t.Skip("HYPERBUTLER_TEST_HVSOCK is not set")
	}
	target, err := ParseHvsockTarget(spec)
	if err != nil {
		t.Fatal(err)
	}
	return target
}

// TestIntegrationHvsockPing sends GET /_ping to the guest dockerd through the named pipe
// proxy and the hvsock dialer, the same path the Docker CLI takes.
func TestIntegrationHvsockPing(t *testing.T) {
	target := integrationTarget(t)
	name := testPipeName(t)
	ln, err := ListenPipe(name)
	if err != nil {
		t.Fatal(err)
	}
	runProxy(t, &Proxy{Upstream: &HvsockDialer{Target: target, Retries: 3, RetryWait: time.Second}}, ln)

	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return winio.DialPipeContext(ctx, name)
		},
	}}
	defer client.CloseIdleConnections()
	resp, err := client.Get("http://docker/_ping")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "OK" {
		t.Fatalf("/_ping = %d %q", resp.StatusCode, body)
	}
}

// TestIntegrationHvsockHalfClose checks that CloseWrite on an hvsock connection leaves
// the read side usable, which attach and exec depend on.
func TestIntegrationHvsockHalfClose(t *testing.T) {
	target := integrationTarget(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := (&HvsockDialer{Target: target, Retries: 3, RetryWait: time.Second}).DialContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	if _, err := io.WriteString(c, "GET /_ping HTTP/1.0\r\nHost: docker\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	if err := c.(closeWriter).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(c)
	if err != nil {
		t.Fatalf("read after CloseWrite: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("no response after CloseWrite")
	}
}
