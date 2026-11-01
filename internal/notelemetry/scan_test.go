package notelemetry

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
	_ = exec.Command(ctx.Value("bin").(string)).Run()
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
		"pkg/p.go:30: exec:computed",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("findings:\n%q\nwant:\n%q", got, want)
	}
}

// scanSource scans one synthetic file and returns its findings as strings.
func scanSource(t *testing.T, src string) []string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pkg", "p.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	fs, err := ScanGo(root, []string{"pkg"})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range fs {
		got = append(got, f.String())
	}
	return got
}

// The evasions of the first version of the scan (L-6), one rule each.
func TestScanGoEvasions(t *testing.T) {
	for name, c := range map[string]struct {
		src  string
		want []string
	}{
		"dot import of net": {"package p\n\nimport . \"net\"\n\nvar _, _ = Dial(\"tcp\", \"x:1\")\n",
			[]string{"pkg/p.go:3: dotimport:net"}},
		"dot import of net/http": {"package p\n\nimport (\n\t. \"net/http\"\n)\n\nvar _, _ = Get(\"\")\n",
			[]string{"pkg/p.go:4: dotimport:net/http"}},
		"dot import of os/exec": {"package p\n\nimport . \"os/exec\"\n\nvar _ = Command(\"curl\")\n",
			[]string{"pkg/p.go:3: dotimport:os/exec"}},
		"cmd /c curl": {"package p\n\nimport \"os/exec\"\n\nvar _ = exec.Command(\"cmd\", \"/c\", \"curl -s x\")\n",
			[]string{"pkg/p.go:5: exec:curl"}},
		"powershell Invoke-WebRequest": {"package p\n\nimport (\n\t\"context\"\n\t\"os/exec\"\n)\n\nvar _ = exec.CommandContext(context.TODO(), \"powershell.exe\", \"-NoProfile\", \"-c\", \"Invoke-WebRequest x | Out-Null\")\n",
			[]string{"pkg/p.go:8: exec:invoke-webrequest"}},
		"sh -c wget, computed program": {"package p\n\nimport \"os/exec\"\n\nfunc f(sh string) { _ = exec.Command(sh, \"-c\", \"wget -q x; ping -c1 y\") }\n",
			[]string{"pkg/p.go:5: exec:computed", "pkg/p.go:5: exec:wget", "pkg/p.go:5: exec:ping"}},
		"UDP and IP listeners, resolvers": {"package p\n\nimport \"net\"\n\nfunc f() {\n\t_, _ = net.ListenUDP(\"udp\", nil)\n\t_, _ = net.ListenPacket(\"udp\", \":0\")\n\t_, _ = net.ListenIP(\"ip4:icmp\", nil)\n\t_, _ = net.ResolveTCPAddr(\"tcp\", \"x:1\")\n\t_, _ = net.ResolveUDPAddr(\"udp\", \"x:1\")\n\t_, _ = net.ResolveIPAddr(\"ip\", \"x\")\n}\n",
			[]string{"pkg/p.go:6: net.ListenUDP", "pkg/p.go:7: net.ListenPacket", "pkg/p.go:8: net.ListenIP",
				"pkg/p.go:9: net.ResolveTCPAddr", "pkg/p.go:10: net.ResolveUDPAddr", "pkg/p.go:11: net.ResolveIPAddr"}},
		"Winsock and DNS client DLLs": {"package p\n\nimport (\n\t\"syscall\"\n\n\t\"golang.org/x/sys/windows\"\n)\n\nvar a = windows.NewLazySystemDLL(\"ws2_32.dll\")\nvar b, _ = syscall.LoadDLL(\"DNSAPI.DLL\")\n",
			[]string{"pkg/p.go:9: dll:ws2_32.dll", "pkg/p.go:10: dll:dnsapi.dll"}},
		"cgo": {"package p\n\n// #include <sys/socket.h>\nimport \"C\"\n",
			[]string{"pkg/p.go:4: cgo"}},
		"no finding": {"package p\n\nimport \"os/exec\"\n\nvar _ = exec.Command(\"dockerd\", \"--validate\", \"--config-file\", \"/etc/docker/daemon.json\")\n",
			nil},
	} {
		if got := scanSource(t, c.src); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got %q\nwant %q", name, got, c.want)
		}
	}
}

// An allowlist entry covers as many uses as it counts: a second net.Dialer in a file
// with one reviewed fails, and so does a count that is too high.
func TestAllowlistCounts(t *testing.T) {
	fs := []Finding{{File: "a.go", Line: 3, Rule: "net.Dialer"}, {File: "a.go", Line: 9, Rule: "net.Dialer"}, {File: "b.go", Line: 1, Rule: "net.Dialer"}}
	for _, c := range []struct {
		allow []allowEntry
		want  string // a substring of the only problem, or "" for none
	}{
		{[]allowEntry{{file: "a.go", rule: "net.Dialer", count: 2}, {file: "b.go", rule: "net.Dialer", count: 1}}, ""},
		{[]allowEntry{{file: "a.go", rule: "net.Dialer", count: 1}, {file: "b.go", rule: "net.Dialer", count: 1}}, "allows 1 uses, the file has 2 (lines 3, 9)"},
		{[]allowEntry{{file: "a.go", rule: "net.Dialer", count: 2}, {file: "b.go", rule: "net.Dialer", count: 2}}, "allows 2 uses, the file has 1"},
		{[]allowEntry{{file: "a.go", rule: "net.Dialer", count: 2}, {file: "b.go", rule: "net.Dialer", count: 1}, {file: "c.go", rule: "net.Dialer", count: 1}}, "matches nothing"},
	} {
		p := checkAllowlist(fs, c.allow)
		switch {
		case c.want == "" && len(p) != 0:
			t.Errorf("%+v: %q", c.allow, p)
		case c.want != "" && (len(p) != 1 || !strings.Contains(p[0], c.want)):
			t.Errorf("%+v: %q, want %q", c.allow, p, c.want)
		}
	}
	if p := checkAllowlist(fs, []allowEntry{{file: "a.go", rule: "net.Dialer", count: 2}}); len(p) != 1 || !strings.Contains(p[0], "b.go:1") {
		t.Errorf("an unlisted use: %q", p)
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
