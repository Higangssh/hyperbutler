//go:build windows

package main

import (
	"archive/zip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A tail that starts inside a line drops that line: half a "password=..." would no
// longer be recognized as one (L-4).
func TestReadTail(t *testing.T) {
	p := filepath.Join(t.TempDir(), "log")
	writeFile(t, p, "first line password=abcdef\nsecond\nthird\n")
	for _, c := range []struct {
		n    int64
		want string
	}{
		{100, "first line password=abcdef\nsecond\nthird\n"},
		{int64(len("rd=abcdef\nsecond\nthird\n")), "second\nthird\n"},
		{int64(len("second\nthird\n")), "second\nthird\n"}, // starts exactly at a line
		{int64(len("econd\nthird\n")), "third\n"},
		{3, ""}, // inside the last line
	} {
		b, err := readTail(p, c.n)
		if err != nil || string(b) != c.want {
			t.Errorf("readTail(%d) = %q, %v; want %q", c.n, b, err, c.want)
		}
	}
}

// A docker CLI config.json over the state files' 64 KiB bound is still included, and
// one over diagnose's 1 MiB is listed as omitted, with its size (L-5).
func TestDiagnoseLargeConfig(t *testing.T) {
	home, dockerCfg := testHome(t)
	fakeInstall(t, home, home, dockerCfg)
	pad := strings.Repeat("x", 100<<10)
	writeFile(t, filepath.Join(dockerCfg, "config.json"), `{"auths":{"r":{"auth":"`+testSecret+`"}},"pad":"`+pad+`"}`)
	writeFile(t, filepath.Join(home, "daemon.json"), `{"pad":"`+strings.Repeat("y", 2<<20)+`"}`)
	out := filepath.Join(t.TempDir(), "d.zip")
	code, so, e := runCLI(t, "diagnose", "--out", out, "--json")
	if code != 0 {
		t.Fatalf("diagnose: %d %s %s", code, so, e)
	}
	var r DiagnoseJSON
	if err := json.Unmarshal([]byte(so), &r); err != nil {
		t.Fatal(err)
	}
	if !contains(r.Files, "docker-config.json") {
		t.Errorf("docker-config.json of 100 KiB not included: %v", r.Files)
	}
	found := false
	for _, m := range r.Missing {
		if m.Name == "daemon.json" {
			found = strings.Contains(m.Reason, "omitted") && strings.Contains(m.Reason, "bytes")
		}
	}
	if !found {
		t.Errorf("an oversized daemon.json is not listed as omitted: %+v", r.Missing)
	}
	zr, err := zip.OpenReader(out)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = zr.Close() }()
	for _, f := range zr.File {
		if f.Name != "summary.txt" {
			continue
		}
		rc, _ := f.Open()
		b := make([]byte, 64<<10)
		n, _ := rc.Read(b)
		_ = rc.Close()
		if !strings.Contains(string(b[:n]), "daemon.json: omitted") {
			t.Errorf("summary.txt does not say daemon.json was omitted:\n%s", b[:n])
		}
	}
}

// persistentACLs is true on the NTFS volume the tests run on.
func TestPersistentACLs(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "f"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if !persistentACLs(f) {
		t.Errorf("%s: no FILE_PERSISTENT_ACLS on the test volume (is TEMP on FAT32 or exFAT?)", f.Name())
	}
}
