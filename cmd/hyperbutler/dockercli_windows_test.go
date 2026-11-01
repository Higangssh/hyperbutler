//go:build windows

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseCLIVersion(t *testing.T) {
	for in, want := range map[string][3]int{
		"Docker version 29.9.2, build a82e528\n":      {29, 9, 1},
		"Docker version 19.03.12, build 48a66213fe\n": {19, 3, 1},
		"Docker version 18.09.7, build 2d0083d\n":     {18, 9, 1},
		"Docker version v25.0.0-rc.1, build x":        {25, 0, 1},
		"docker-compose version 1.29.2":               {0, 0, 0},
		"":                                            {0, 0, 0},
	} {
		major, minor, ok := parseCLIVersion(in)
		if got := [3]int{major, minor, map[bool]int{true: 1}[ok]}; got != want {
			t.Errorf("%q: got %v, want %v", in, got, want)
		}
	}
	for _, c := range []struct {
		major, minor int
		old          bool
	}{{18, 9, true}, {19, 2, true}, {19, 3, false}, {20, 10, false}, {29, 9, false}} {
		if got := cliTooOld(c.major, c.minor); got != c.old {
			t.Errorf("%d.%d: tooOld %v", c.major, c.minor, got)
		}
	}
}

// fakeDocker writes a docker.cmd that prints version, and puts only its folder on PATH.
func fakeDocker(t *testing.T, version string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "docker.cmd")
	if err := os.WriteFile(p, []byte("@echo Docker version "+version+", build test\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("PATHEXT", ".CMD;.EXE")
	return p
}

func TestCheckDockerCLI(t *testing.T) {
	bundled := filepath.Join(t.TempDir(), "docker.exe")

	p := fakeDocker(t, "18.09.7")
	c := findDockerCLI()
	if !strings.EqualFold(c.Path, p) {
		t.Fatalf("found %q, want %q", c.Path, p)
	}
	c.Bundled = bundled
	var out, errb bytes.Buffer
	checkDockerCLI(c, &out, &errb)
	if s := errb.String(); !strings.Contains(s, "too old") || !strings.Contains(s, bundled) || !strings.Contains(s, "$env:Path") {
		t.Errorf("old CLI: %q", s)
	}

	fakeDocker(t, "29.9.2")
	c = findDockerCLI()
	c.Bundled = bundled
	out.Reset()
	errb.Reset()
	checkDockerCLI(c, &out, &errb)
	if errb.Len() != 0 || !strings.Contains(out.String(), "found on PATH") {
		t.Errorf("current CLI: out %q err %q", out.String(), errb.String())
	}

	// None on PATH: point at the bundled one.
	t.Setenv("PATH", t.TempDir())
	c = findDockerCLI()
	if c.Path != "" {
		t.Fatalf("found %q on an empty PATH", c.Path)
	}
	c.Bundled = bundled
	errb.Reset()
	checkDockerCLI(c, &out, &errb)
	if !strings.Contains(errb.String(), bundled) {
		t.Errorf("no CLI: %q", errb.String())
	}
}
