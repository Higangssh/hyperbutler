package notelemetry

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

const violations = `package p

import (
	"context"
	"net"
	h "net/http"
	"os/exec"

	"golang.org/x/sys/unix"
)

func f(ctx context.Context) {
	_, _ = h.Get("https://telemetry.example.com/v1")
	_, _ = net.Dial("tcp", "8.8.8.8:53")
	_, _ = net.Dial("tcp", "127.0.0.1:2375")
	_, _ = net.Dial("unix", "/run/docker.sock")
	_, _ = net.DialTimeout("tcp", "[::1]:80", 0)
	var d net.Dialer
	_, _ = d.DialContext(ctx, "tcp", "x:1")
	_, _ = net.LookupHost("example.com")
	fd, _ := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
	_, _ = unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	_ = unix.Connect(fd, &unix.SockaddrVM{CID: 2, Port: 1})
	_ = unix.Connect(fd, &unix.SockaddrInet4{Port: 80})
	_ = exec.CommandContext(ctx, "C:\\Windows\\System32\\curl.exe", "x").Run()
	_ = exec.Command("dockerd", "--validate").Run()
	_ = "http://docker/_ping"
	_ = "http://127.0.0.1:8080/"
	_ = "wininet.dll"
	_ = h.MethodGet
}
`

func TestScanGoFindsViolations(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "pkg")
	if err := os.MkdirAll(filepath.Join(dir, "testdata"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, src := range map[string]string{
		"p.go":          violations,
		"p_test.go":     violations, // tests are not scanned
		"testdata/x.go": violations, // nor is testdata
	} {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(name)), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fs, err := ScanGo(root, []string{"pkg"})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range fs {
		got = append(got, f.String())
	}
	want := []string{
		"pkg/p.go:13: net/http.Get",
		"pkg/p.go:13: url:telemetry.example.com",
		"pkg/p.go:14: net.Dial",
		"pkg/p.go:18: net.Dialer",
		"pkg/p.go:20: net.LookupHost",
		"pkg/p.go:21: golang.org/x/sys/unix.Socket",
		"pkg/p.go:24: golang.org/x/sys/unix.Connect",
		"pkg/p.go:25: exec:curl",
		"pkg/p.go:29: dll:wininet.dll",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("findings:\n%q\nwant:\n%q", got, want)
	}
}

func TestScanShell(t *testing.T) {
	root := t.TempDir()
	script := `#!/bin/sh
# ntpd -p pool.ntp.org would set the clock from the internet
mount -t proc proc /proc
echo "$#" # wget is mentioned in a comment only
ntpd -n -p pool.ntp.org &
udhcpc -i eth0
OTEL_EXPORTER_OTLP_ENDPOINT=http://x dockerd --metrics-addr=0.0.0.0:9323
`
	if err := os.WriteFile(filepath.Join(root, "init"), []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	fs, err := ScanShell(root, []string{"init"})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range fs {
		got = append(got, f.String())
	}
	want := []string{
		"init:5: sh:ntpd",
		"init:6: sh:udhcpc",
		"init:7: sh:OTEL_EXPORTER_OTLP_ENDPOINT",
		"init:7: sh:metrics-addr",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("findings:\n%q\nwant:\n%q", got, want)
	}
}
