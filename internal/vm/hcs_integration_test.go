//go:build integration && windows

// Integration test against real HCS. It boots the prototype guest (guest/prototype,
// with the agent listening on vsock port 2375) and needs the user to be in Hyper-V
// Administrators. Run it non-elevated with:
//
//	$env:HYPERBUTLER_TEST_KERNEL = "...\vmlinuz-virt"
//	$env:HYPERBUTLER_TEST_INITRD = "...\initramfs-hyperbutler.gz"
//	go test -tags integration -run Integration -v ./internal/vm/
//
// Windows edition: ran on Windows 11 Home 10.0.26200 (see docs/research/hvsock-path.md).
package vm

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/transport"
)

func TestIntegrationHvSocketServiceTable(t *testing.T) {
	kernel, initrd := os.Getenv("HYPERBUTLER_TEST_KERNEL"), os.Getenv("HYPERBUTLER_TEST_INITRD")
	if kernel == "" || initrd == "" {
		t.Skip("HYPERBUTLER_TEST_KERNEL and HYPERBUTLER_TEST_INITRD are not set")
	}
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sddl, err := UserOnlySDDL(tu.User.Sid.String())
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	id := "hyperbutler-test-" + hex.EncodeToString(b)
	consolePipe := `\\.\pipe\` + id + "-com1"
	sys, err := Create(id, &Config{
		// The prototype init writes to /dev/console, so it gets a COM port.
		ConsolePipe:    consolePipe,
		KernelPath:     kernel,
		InitrdPath:     initrd,
		KernelCmdLine:  "console=ttyS0 quiet",
		MemoryMB:       1024,
		ProcessorCount: 2,
		Services: []Service{
			{ID: transport.VsockServiceID(2375), ConnectSDDL: sddl}, // the agent
			{ID: transport.VsockServiceID(2377), ConnectSDDL: sddl}, // allowed, nobody listens
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sys.Close()
	defer sys.Terminate()
	if err := sys.Start(); err != nil {
		t.Fatal(err)
	}

	// An unread COM port stalls every guest write to /dev/console, so drain it.
	cp, err := windows.UTF16PtrFromString(consolePipe)
	if err != nil {
		t.Fatal(err)
	}
	var ch windows.Handle
	for i := 0; ; i++ {
		ch, err = windows.CreateFile(cp, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING,
			windows.SECURITY_SQOS_PRESENT|windows.SECURITY_IDENTIFICATION, 0)
		if err == nil {
			break
		}
		if i == 500 {
			t.Fatalf("open console: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	console := os.NewFile(uintptr(ch), "com1")
	defer console.Close()
	go io.Copy(io.Discard, console)

	vmid, err := sys.RuntimeID()
	if err != nil {
		t.Fatal(err)
	}
	dialOnce := func(port uint32, timeout time.Duration) (net.Conn, error) {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		d := &transport.HvsockDialer{Target: transport.HvsockTarget{VMID: vmid, ServiceID: transport.VsockServiceID(port)}}
		return d.DialContext(ctx)
	}

	// The listed service is reachable once the guest agent listens.
	start := time.Now()
	var c net.Conn
	for time.Since(start) < 30*time.Second {
		if c, err = dialOnce(2375, 500*time.Millisecond); err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	t.Logf("agent accepted after %v", time.Since(start))
	if _, err := c.Write([]byte("GET /_ping HTTP/1.0\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	resp, err := io.ReadAll(io.LimitReader(c, 1<<10))
	c.Close()
	if err != nil || len(resp) < 12 || string(resp[9:12]) != "200" {
		t.Fatalf("/_ping: %v %q", err, resp)
	}

	// A connect to an allowed port with no listener stays pending in go-winio until the
	// hvsock connect timeout; HvsockDialer must still return when its context ends.
	t0 := time.Now()
	if _, err := dialOnce(2377, 500*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pending connect: got %v, want context.DeadlineExceeded", err)
	}
	if d := time.Since(t0); d > 2*time.Second {
		t.Fatalf("pending connect returned after %v", d)
	}

	// A port that is not in the service table falls under the deny-all default. Nothing
	// listens there either, but that would show as a pending connect, not WSAEACCES.
	if _, err := dialOnce(2376, 5*time.Second); !errors.Is(err, windows.WSAEACCES) {
		t.Fatalf("unlisted service: got %v, want WSAEACCES", err)
	}

	// Terminate is idempotent.
	if err := sys.Terminate(); err != nil {
		t.Fatalf("terminate: %v", err)
	}
	if err := sys.Terminate(); err != nil {
		t.Fatalf("second terminate: %v", err)
	}
}
