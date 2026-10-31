//go:build windows

package hostmon

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestSampleNTSelf(t *testing.T) {
	self := uint32(os.Getpid())
	m, err := SampleNT(context.Background(), []uint32{self, 0xfffffff0})
	if err != nil {
		t.Fatal(err)
	}
	s, ok := m[self]
	if !ok || len(m) != 1 {
		t.Fatalf("got %v", m)
	}
	if s.WorkingSetPriv == 0 || s.PrivateBytes < s.WorkingSetPriv/2 || s.ThreadCount == 0 || s.HandleCount == 0 {
		t.Fatalf("implausible sample %+v", s)
	}
	if m, err := SampleNT(context.Background(), nil); err != nil || len(m) != 0 {
		t.Fatalf("no PIDs: %v %v", m, err)
	}
}

func TestSamplerByName(t *testing.T) {
	for _, n := range []string{"nt", "cim"} {
		if s, err := SamplerByName(n); err != nil || s == nil {
			t.Fatalf("%s: %v", n, err)
		}
	}
	if _, err := SamplerByName("pdh"); err == nil {
		t.Fatal("unknown sampler accepted")
	}
}

// big is a demand-zero data symbol that the test touches only after the first walk,
// like the FIPS scratch buffer in the engine.
var big [8 << 20]byte

func symbolBytes(r Residency, suffix string) Bytes {
	for k, v := range r.Kinds {
		if strings.HasPrefix(k, "exe symbol ") && strings.HasSuffix(k, suffix) {
			return v
		}
	}
	return Bytes{}
}

func TestResidencySelf(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	r, err := ResidencyOf(uint32(os.Getpid()), exe)
	if err != nil {
		t.Fatal(err)
	}
	// `go test` links without the symbol table; `go test -c` keeps it.
	lay, err := readExeLayout(exe)
	if err != nil {
		t.Fatal(err)
	}
	syms := len(lay.symbols) > 0
	if sym := symbolBytes(r, ".big"); syms && (sym.Committed < 8<<20 || sym.Resident > 1<<20) {
		t.Fatalf("untouched symbol: %+v in %s", sym, r)
	}
	before := r.Kinds["exe .data"].Resident
	for i := 0; i < len(big); i += 4096 {
		big[i] = 1
	}
	r, err = ResidencyOf(uint32(os.Getpid()), exe)
	if err != nil {
		t.Fatal(err)
	}
	if sym := symbolBytes(r, ".big"); syms && sym.Resident < 8<<20-8192 {
		t.Fatalf("touched symbol: %+v in %s", sym, r)
	}
	if got := r.Kinds["exe .data"].Resident - before; got < 8<<20-8192 {
		t.Fatalf(".data grew by %d bytes after touching 8 MiB: %s", got, r)
	}
	if r.Kinds["go heap"].Resident == 0 {
		t.Fatalf("no Go heap: %s", r)
	}
	t.Log(r)
}
