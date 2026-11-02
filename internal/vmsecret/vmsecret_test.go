package vmsecret

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testSecret() Secret {
	var s Secret
	for i := range s {
		s[i] = byte(i + 1)
	}
	return s
}

func TestNew(t *testing.T) {
	a, err := New()
	if err != nil {
		t.Fatal(err)
	}
	b, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if a.IsZero() || b.IsZero() || a == b {
		t.Fatal("New returned a zero or repeated secret")
	}
}

// A secret never reaches text through fmt, whatever the verb or nesting.
func TestNeverPrints(t *testing.T) {
	s := testSecret()
	h := s.Hex()
	holder := struct {
		Name   string
		Secret Secret
		P      *Secret
	}{"x", s, &s}
	for _, out := range []string{
		fmt.Sprint(s), fmt.Sprintf("%v %+v %#v %s %q %x %X %d", s, s, s, s, s, s, s, s),
		fmt.Sprintf("%v %+v %#v", holder, holder, holder), fmt.Sprintf("%x", s[:0]),
	} {
		if strings.Contains(out, h) || strings.Contains(strings.ToLower(out), h[:16]) || strings.Contains(out, "1 2 3 4") {
			t.Fatalf("secret printed: %s", out)
		}
	}
	if len(h) != 2*Size || h != strings.ToLower(h) {
		t.Fatalf("Hex = %q", h)
	}
}

func TestParse(t *testing.T) {
	s := testSecret()
	got, err := Parse(s.Hex())
	if err != nil || got != s {
		t.Fatalf("Parse(Hex) = %v, %v", got == s, err)
	}
	for _, bad := range []string{
		"", strings.Repeat("0", 64), strings.ToUpper(s.Hex()), s.Hex()[:63], s.Hex() + "0",
		"+" + s.Hex()[1:], strings.Repeat("g", 64), " " + s.Hex()[1:],
	} {
		if _, err := Parse(bad); !errors.Is(err, ErrMalformed) {
			t.Errorf("Parse(%q) = %v, want ErrMalformed", bad, err)
		}
	}
}

func TestFromCmdline(t *testing.T) {
	s := testSecret()
	arg := CmdlineKey + "=" + s.Hex()
	for _, line := range []string{
		arg,
		"console=ttyS0 quiet " + arg + " hyperbutler.net=1024\n",
		"\t" + arg + "\n",
	} {
		got, err := FromCmdline(line)
		if err != nil || got != s {
			t.Fatalf("FromCmdline(%q): ok %v, %v", line, got == s, err)
		}
	}
	if _, err := FromCmdline("console=ttyS0 hyperbutler.secrets=1 xhyperbutler.secret=" + s.Hex()); !errors.Is(err, ErrMissing) {
		t.Fatalf("no key: %v", err)
	}
	for _, line := range []string{
		arg + " " + arg,         // a second copy, even an equal one
		arg + " " + CmdlineKey,  // a bare key
		CmdlineKey + "=",        // empty
		CmdlineKey + "=" + "00", // short
		strings.Repeat("a", maxCmdline+1),
	} {
		if _, err := FromCmdline(line); !errors.Is(err, ErrMalformed) {
			t.Errorf("FromCmdline(%.80q) = %v, want ErrMalformed", line, err)
		}
	}
}

func TestLoad(t *testing.T) {
	s := testSecret()
	p := filepath.Join(t.TempDir(), "cmdline")
	if err := os.WriteFile(p, []byte("console=ttyS0 "+CmdlineKey+"="+s.Hex()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := Load(p); err != nil || got != s {
		t.Fatalf("Load: ok %v, %v", got == s, err)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("Load of a missing file")
	}
	if err := os.WriteFile(p, []byte("quiet"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); !errors.Is(err, ErrMissing) {
		t.Fatalf("no secret: %v", err)
	}
}

func TestCheck(t *testing.T) {
	s := testSecret()
	rest := []byte("GET /_ping")
	r := bytes.NewReader(append(s[:], rest...))
	if err := Check(r, s); err != nil {
		t.Fatal(err)
	}
	// Nothing past the secret was consumed.
	if b, _ := io.ReadAll(r); !bytes.Equal(b, rest) {
		t.Fatalf("left %q", b)
	}
	wrong := s
	wrong[Size-1] ^= 1
	for name, tc := range map[string]struct {
		in   []byte
		want Secret
	}{
		"wrong":       {wrong[:], s},
		"short":       {s[:Size-1], s},
		"empty":       {nil, s},
		"zero want":   {make([]byte, Size), Secret{}},
		"zero wanted": {s[:], Secret{}},
	} {
		if err := Check(bytes.NewReader(tc.in), tc.want); !errors.Is(err, ErrMismatch) {
			t.Errorf("%s: %v, want ErrMismatch", name, err)
		}
	}
	if err := Send(io.Discard, Secret{}); err == nil {
		t.Fatal("Send of a zero secret")
	}
	var b bytes.Buffer
	if err := Send(&b, s); err != nil || !bytes.Equal(b.Bytes(), s[:]) {
		t.Fatalf("Send: %x, %v", b.Bytes(), err)
	}
}

// Admit bounds the wait: a peer that sends nothing, or too little, is refused once the
// timeout passes, and an admitted connection has no deadline left.
func TestAdmit(t *testing.T) {
	s := testSecret()
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	go func() { _, _ = a.Write(s[:5]) }()
	start := time.Now()
	if err := Admit(b, s, 100*time.Millisecond); !errors.Is(err, ErrMismatch) {
		t.Fatalf("slow peer: %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("took %v", d)
	}

	c, d := net.Pipe()
	defer c.Close()
	defer d.Close()
	go func() {
		_ = Send(c, s)
		time.Sleep(200 * time.Millisecond) // longer than the timeout below
		_, _ = c.Write([]byte("x"))
	}()
	if err := Admit(d, s, 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	var one [1]byte
	if _, err := io.ReadFull(d, one[:]); err != nil || one[0] != 'x' {
		t.Fatalf("read after Admit: %q, %v (deadline not cleared?)", one, err)
	}
}

func FuzzFromCmdline(f *testing.F) {
	s := testSecret()
	f.Add(CmdlineKey + "=" + s.Hex())
	f.Add("quiet " + CmdlineKey + "=" + s.Hex() + " " + CmdlineKey + "=" + s.Hex())
	f.Add(CmdlineKey + "=" + strings.ToUpper(s.Hex()))
	f.Add("")
	f.Fuzz(func(t *testing.T, line string) {
		got, err := FromCmdline(line)
		if err != nil {
			if got != (Secret{}) || (!errors.Is(err, ErrMissing) && !errors.Is(err, ErrMalformed)) {
				t.Fatalf("FromCmdline(%q) = %v", line, err)
			}
			return
		}
		if got.IsZero() || !strings.Contains(line, CmdlineKey+"="+got.Hex()) {
			t.Fatalf("FromCmdline(%q) accepted a secret not in the line", line)
		}
	})
}

// Verify is the guest's check of a host listener: a listener that sends the wrong
// secret, or nothing, is refused (the latter after DefaultTimeout), and one that sends
// the right one leaves the connection with no deadline and nothing past it consumed.
func TestVerify(t *testing.T) {
	s := testSecret()
	wrong := s
	wrong[0] ^= 1
	for name, send := range map[string][]byte{"wrong": wrong[:], "zero": make([]byte, Size), "nothing": nil} {
		a, b := net.Pipe()
		go func() {
			if send != nil {
				_, _ = a.Write(send)
			}
		}()
		start := time.Now()
		err := Verify(b, s)
		if !errors.Is(err, ErrMismatch) {
			t.Errorf("%s: %v, want ErrMismatch", name, err)
		}
		if d := time.Since(start); d > DefaultTimeout+2*time.Second {
			t.Errorf("%s: took %v", name, d)
		}
		_ = a.Close()
		_ = b.Close()
	}

	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	go func() {
		_ = Send(a, s)
		_, _ = a.Write([]byte("hello"))
	}()
	if err := Verify(b, s); err != nil {
		t.Fatal(err)
	}
	rest := make([]byte, 5)
	if _, err := io.ReadFull(b, rest); err != nil || string(rest) != "hello" {
		t.Fatalf("after Verify: %q, %v", rest, err)
	}
}

// Check is the parser both directions share: it accepts exactly the inputs that begin
// with the wanted secret, and never reads past its 32 bytes.
func FuzzCheck(f *testing.F) {
	s := testSecret()
	f.Add(s[:])
	f.Add(append(s[:], "GET /"...))
	f.Add(s[:Size-1])
	f.Add(make([]byte, Size))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, in []byte) {
		for _, want := range []Secret{s, {}} {
			r := bytes.NewReader(in)
			err := Check(r, want)
			ok := !want.IsZero() && len(in) >= Size && bytes.Equal(in[:Size], want[:])
			if (err == nil) != ok {
				t.Fatalf("Check(%x, zero=%v) = %v, want ok=%v", in, want.IsZero(), err, ok)
			}
			if err != nil && !errors.Is(err, ErrMismatch) {
				t.Fatalf("Check error %v is not ErrMismatch", err)
			}
			if read := len(in) - r.Len(); read > Size {
				t.Fatalf("Check read %d bytes", read)
			}
		}
	})
}
