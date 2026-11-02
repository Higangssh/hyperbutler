package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Higangssh/hyperbutler/internal/diag"
)

func TestEngineArgs(t *testing.T) {
	got := strings.Join(engineArgs(`C:\h`, engineImage{manifest: `C:\i\image.json`}, diag.Config{MemoryLimit: "256MiB", DebugPipe: `\\.\pipe\d`}), " ")
	if got != `engine --home C:\h --image C:\i\image.json --memlimit 256MiB --debug-pipe \\.\pipe\d` {
		t.Fatal(got)
	}
	// The debug endpoint is off unless asked for.
	got = strings.Join(engineArgs(`C:\h`, engineImage{kernel: "k", initrd: "i"}, diag.Config{}), " ")
	if got != `engine --home C:\h --kernel k --initrd i` {
		t.Fatal(got)
	}
	// With the broker, the engine boots no image of its own.
	got = strings.Join(engineArgs(`C:\h`, engineImage{manifest: `C:\i\image.json`, broker: `\\.\pipe\b`}, diag.Config{}), " ")
	if got != `engine --home C:\h --broker \\.\pipe\b` {
		t.Fatal(got)
	}
}

func TestCheckBackendFlags(t *testing.T) {
	for _, tc := range []struct {
		args []string
		ok   bool
	}{
		{nil, true},
		{[]string{"--backend", "auto"}, true},
		{[]string{"--backend", "hcs"}, true},
		{[]string{"--backend", "broker"}, true},
		{[]string{"--backend", "wsl"}, false},
		{[]string{"--backend", ""}, false},
		{[]string{"--broker", `\\.\pipe\hyperbutler-broker-dev`}, true},
		{[]string{"--backend", "broker", "--broker", `\\.\pipe\b`}, true},
		{[]string{"--broker", `C:\not-a-pipe`}, false},
		{[]string{"--backend", "hcs", "--broker", `\\.\pipe\b`}, false},
	} {
		fs := newFlagSet("start", io.Discard)
		backend := fs.String("backend", backendAuto, "")
		broker := fs.String("broker", "", "")
		if err := fs.Parse(tc.args); err != nil {
			t.Fatal(err)
		}
		if err := checkBackendFlags(*backend, *broker); (err == nil) != tc.ok {
			t.Errorf("%v: %v", tc.args, err)
		}
	}
}

func TestParseMemoryMB(t *testing.T) {
	for in, want := range map[string]int{"4096": 4096, "4GiB": 4096, "4g": 4096, "2048MiB": 2048, " 8192 ": 8192, "1T": 1 << 20} {
		if got, err := parseMemoryMB(in); err != nil || got != want {
			t.Errorf("parseMemoryMB(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "512", "4097KiB", "2T", "-4096", "4.5G", "lots"} {
		if got, err := parseMemoryMB(in); err == nil {
			t.Errorf("parseMemoryMB(%q) = %d, want an error", in, got)
		}
	}
}

func TestSizeFlag(t *testing.T) {
	var s sizeFlag
	if s.String() != "" {
		t.Fatal("unset flag prints a value")
	}
	if err := s.Set("64GiB"); err != nil || !s.set || s.n != 64<<30 || s.String() != "64GiB" {
		t.Fatalf("%+v %v", s, err)
	}
	if err := s.Set("64 apples"); err == nil {
		t.Fatal("accepted a bad size")
	}
}

func TestParseArgsRejectsPositional(t *testing.T) {
	var out bytes.Buffer
	fs := newFlagSet("stop", &out)
	fs.Bool("yes", false, "")
	if err := parseArgs(fs, []string{"--yes", "now"}); err == nil || !strings.Contains(err.Error(), `"now"`) {
		t.Fatalf("positional argument: %v", err)
	}
}

func TestLastLines(t *testing.T) {
	b := []byte("a\nb\nc\n")
	for n, want := range map[int]string{0: "a\nb\nc\n", 1: "c\n", 2: "b\nc\n", 3: "a\nb\nc\n", 9: "a\nb\nc\n"} {
		if got := string(lastLines(b, n)); got != want {
			t.Errorf("lastLines(%d) = %q, want %q", n, got, want)
		}
	}
	if got := string(lastLines([]byte("x\ny"), 1)); got != "y" {
		t.Errorf("no trailing newline: %q", got)
	}
}

func TestNpipeHost(t *testing.T) {
	if got := npipeHost(`\\.\pipe\docker_engine_hyperbutler`); got != "npipe:////./pipe/docker_engine_hyperbutler" {
		t.Fatalf("npipeHost = %q", got)
	}
}

// FuzzPrintable: the output never carries a control character a terminal acts on
// (anything below 0x20 but tab, newline and a CR right before a newline; DEL; the C1
// range) and is valid UTF-8.
func FuzzPrintable(f *testing.F) {
	f.Add([]byte("\x1b]0;title\x07"))
	f.Add([]byte("a\r\nb\rc\xc2\x9b"))
	f.Add([]byte("\xe2\x80\xa8\xff\xfe"))
	f.Fuzz(func(t *testing.T, b []byte) {
		out := printable(b)
		if !utf8.Valid(out) {
			t.Fatalf("invalid UTF-8 from %q: %q", b, out)
		}
		for i, r := range string(out) {
			switch {
			case r == '\t' || r == '\n':
			case r == '\r':
				if i+1 >= len(out) || out[i+1] != '\n' {
					t.Fatalf("lone CR in %q", out)
				}
			case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == 0x2028 || r == 0x2029:
				t.Fatalf("control %U in %q", r, out)
			}
		}
	})
}

// FuzzParseMemoryMB: an accepted value is a whole number of MiB in range.
func FuzzParseMemoryMB(f *testing.F) {
	for _, s := range []string{"4096", "4GiB", "1048576", "1048577", "0", "99999999999999999999"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		mb, err := parseMemoryMB(s)
		if err != nil {
			return
		}
		if mb < 1024 || mb > 1<<20 {
			t.Fatalf("%q -> %d MiB", s, mb)
		}
	})
}

// FuzzLastLines: the result is a suffix of the input with at most n line breaks before
// its final one.
func FuzzLastLines(f *testing.F) {
	f.Add([]byte("a\nb\nc\n"), 2)
	f.Add([]byte("\n\n\n"), 1)
	f.Add([]byte(""), -3)
	f.Fuzz(func(t *testing.T, b []byte, n int) {
		got := lastLines(b, n)
		if !bytes.HasSuffix(b, got) {
			t.Fatalf("not a suffix: %q of %q", got, b)
		}
		if n > 0 && len(got) < len(b) {
			trimmed := bytes.TrimSuffix(got, []byte("\n"))
			if c := bytes.Count(trimmed, []byte("\n")); c > n-1 {
				t.Fatalf("%d lines for n=%d: %q", c+1, n, got)
			}
		}
	})
}
