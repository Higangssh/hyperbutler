package config

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Higangssh/hyperbutler/internal/engine"
	"github.com/Higangssh/hyperbutler/internal/leaktest"
)

func TestMain(m *testing.M) { leaktest.VerifyTestMain(m) }

func testEnv(t *testing.T) (Env, *bool) {
	home, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	disk := new(bool)
	return Env{
		Home: home, HostCPUs: 12,
		Getenv:     func(string) string { return "" },
		DiskExists: func(string) bool { return *disk },
	}, disk
}

func item(t *testing.T, items []Item, key string) Item {
	t.Helper()
	for _, it := range items {
		if it.Key == key {
			return it
		}
	}
	t.Fatalf("no key %q", key)
	return Item{}
}

func TestListDefaults(t *testing.T) {
	e, _ := testEnv(t)
	items := List(nil, e)
	if len(items) != len(Keys) {
		t.Fatalf("%d items", len(items))
	}
	want := map[string]struct {
		value string
		apply ApplyTime
	}{
		"memory":              {"4GiB", ApplyVM},
		"cpus":                {"6", ApplyVM},
		"disk.size":           {"64GiB", ApplyFirstStart},
		"data.dir":            {e.Home, ApplyFirstStart},
		"pipe":                {`\\.\pipe\docker_engine_hyperbutler`, ApplyVM},
		"context.manage":      {"true", ApplyLive},
		"engine.memoryLimit":  {"256MiB", ApplyVM},
		"hostGateway.enabled": {"true", ApplyLive},
		"hostGateway.ports":   {"all", ApplyLive},
	}
	for k, w := range want {
		it := item(t, items, k)
		if it.Value != w.value || it.Default != w.value || it.Source != SourceDefault || it.Apply != w.apply || it.Description == "" || it.Type == "" {
			t.Errorf("%s: %+v", k, it)
		}
	}
}

func TestSetUnset(t *testing.T) {
	e, disk := testEnv(t)
	var s *engine.Settings
	set := func(k, v string) {
		t.Helper()
		key, err := Lookup(k)
		if err != nil {
			t.Fatal(err)
		}
		n, err := key.Set(s, v, e)
		if err != nil {
			t.Fatalf("set %s %s: %v", k, v, err)
		}
		s = n
	}
	set("memory", "8GiB")
	set("CPUS", "3") // names are case-insensitive
	set("context.manage", "off")
	set("pipe", `\\.\pipe\docker_engine_hb_test`)
	set("engine.memoryLimit", "off")
	set("disk.size", "128g")
	set("data.dir", filepath.Join(e.Home, "data"))
	set("hostGateway.enabled", "no")
	set("hostGateway.ports", " 8001, 5432,8000 ")
	if hg := s.HostGateway(); hg.Enabled || s.HostGatewayPorts != "5432,8000-8001" || hg.Ports.String() != "5432,8000-8001" {
		t.Fatalf("host gateway %+v %q", hg, s.HostGatewayPorts)
	}
	if s.MemoryMB != 8192 || s.CPUs != 3 || s.ContextManaged() || s.Pipe != `\\.\pipe\docker_engine_hb_test` ||
		s.MemoryLimit != "off" || s.DiskSize != 128<<30 || !engine.SamePath(s.DataDir, filepath.Join(e.Home, "data")) {
		t.Fatalf("settings %+v", s)
	}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	items := List(s, e)
	for _, k := range []string{"memory", "cpus", "context.manage", "pipe", "engine.memoryLimit", "disk.size", "data.dir", "hostGateway.enabled", "hostGateway.ports"} {
		if it := item(t, items, k); it.Source != SourceFile {
			t.Errorf("%s source %s", k, it.Source)
		}
	}
	// The default pipe and memory limit are stored as "unset".
	set("pipe", `\\.\pipe\docker_engine_hyperbutler`)
	set("engine.memoryLimit", "256MiB")
	set("hostGateway.ports", "1-65535")
	if s.Pipe != "" || s.MemoryLimit != "" || s.HostGatewayPorts != "" {
		t.Fatalf("defaults stored: %+v", s)
	}
	for _, k := range Keys {
		n, err := k.Unset(s, e)
		if err != nil {
			t.Fatalf("unset %s: %v", k.Name, err)
		}
		s = n
	}
	if s.MemoryMB != 0 || s.CPUs != 0 || s.ManageContext != nil || s.HostGatewayEnabled != nil || s.HostGatewayPorts != "" || s.DiskSize != engine.DefaultDiskSize || !engine.SamePath(s.DataDir, e.Home) {
		t.Fatalf("after unset: %+v", s)
	}
	// Once the disk exists, its location and size are fixed.
	*disk = true
	for _, kv := range [][2]string{{"disk.size", "32GiB"}, {"data.dir", filepath.Join(e.Home, "x")}} {
		k, _ := Lookup(kv[0])
		if _, err := k.Set(s, kv[1], e); err == nil {
			t.Errorf("%s changed with a disk", kv[0])
		}
	}
	k, _ := Lookup("disk.size")
	if _, err := k.Set(s, "64GiB", e); err != nil {
		t.Errorf("the same disk size refused: %v", err)
	}
}

func TestSetRefuses(t *testing.T) {
	e, _ := testEnv(t)
	for _, c := range [][2]string{
		{"memory", "512MiB"}, {"memory", "4.5GiB"}, {"memory", "lots"}, {"memory", ""}, {"memory", "2000000MiB"},
		{"cpus", "0"}, {"cpus", "257"}, {"cpus", "13"}, {"cpus", "two"}, {"cpus", "1.5"},
		{"disk.size", "1GiB"}, {"disk.size", "65GiB1"}, {"disk.size", "17TiB"}, {"disk.size", "4294967297"},
		{"data.dir", ""}, {"data.dir", `\\server\share`}, {"data.dir", "a\x00b"},
		{"pipe", `C:\pipe`}, {"pipe", `\\.\pipe\`}, {"pipe", `\\server\pipe\x`},
		{"context.manage", "maybe"},
		{"hostGateway.enabled", "maybe"}, {"hostGateway.ports", "0"}, {"hostGateway.ports", "65536"}, {"hostGateway.ports", "9-8"}, {"hostGateway.ports", "80;443"},
		{"engine.memoryLimit", "1KiB"}, {"engine.memoryLimit", "lots"},
	} {
		k, err := Lookup(c[0])
		if err != nil {
			t.Fatal(err)
		}
		if n, err := k.Set(nil, c[1], e); err == nil {
			t.Errorf("set %s %q accepted: %+v", c[0], c[1], n)
		} else if !strings.HasPrefix(err.Error(), k.Name+": ") {
			t.Errorf("error %q does not name the key", err)
		}
	}
}

func TestSetDoesNotChangeInput(t *testing.T) {
	e, _ := testEnv(t)
	no := false
	s := &engine.Settings{Version: 1, DataDir: e.Home, DiskSize: engine.DefaultDiskSize, ManageContext: &no}
	k, _ := Lookup("context.manage")
	n, err := k.Set(s, "true", e)
	if err != nil || !n.ContextManaged() || s.ContextManaged() {
		t.Fatalf("input changed: %+v %+v %v", s, n, err)
	}
	if Equal(s, n) || !Equal(s, s) || Equal(nil, s) || !Equal(nil, nil) {
		t.Fatal("Equal")
	}
}

func TestLookup(t *testing.T) {
	if _, err := Lookup("proxy.mode"); !errors.Is(err, ErrReserved) {
		t.Errorf("reserved: %v", err)
	}
	if _, err := Lookup("nope"); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("unknown: %v", err)
	}
	seen := map[string]bool{}
	for _, k := range Keys {
		if seen[strings.ToLower(k.Name)] {
			t.Errorf("duplicate key %s", k.Name)
		}
		seen[strings.ToLower(k.Name)] = true
		switch k.Apply {
		case ApplyLive, ApplyDockerd, ApplyVM, ApplyFirstStart:
		default:
			t.Errorf("%s: apply time %q", k.Name, k.Apply)
		}
	}
	for _, r := range Reserved {
		if seen[strings.ToLower(r)] {
			t.Errorf("%s is both a key and reserved", r)
		}
	}
}

func TestEnvOverride(t *testing.T) {
	e, _ := testEnv(t)
	e.Getenv = func(k string) string {
		if k == "GOMEMLIMIT" {
			return "1GiB"
		}
		return ""
	}
	k, _ := Lookup("engine.memoryLimit")
	if it := k.Get(nil, e); it.Value != "1GiB" || it.Source != SourceEnv || it.Default != "256MiB" {
		t.Fatalf("%+v", it)
	}
}

func TestParseBool(t *testing.T) {
	for v, want := range map[string]bool{"true": true, "YES": true, " on ": true, "1": true, "false": false, "No": false, "off": false, "0": false} {
		if got, err := ParseBool(v); err != nil || got != want {
			t.Errorf("ParseBool(%q) = %v %v", v, got, err)
		}
	}
	for _, v := range []string{"", "t", "2", "truee"} {
		if _, err := ParseBool(v); err == nil {
			t.Errorf("ParseBool(%q) accepted", v)
		}
	}
}

func TestProxyKeys(t *testing.T) {
	e, _ := testEnv(t)
	set := func(s *engine.Settings, k, v string) (*engine.Settings, error) {
		key, err := Lookup(k)
		if err != nil {
			t.Fatal(err)
		}
		return key.Set(s, v, e)
	}
	s, err := set(nil, "proxy.https", "http://corp%5Cuser:s3cret@proxy.corp:3128")
	if err != nil {
		t.Fatal(err)
	}
	k, _ := Lookup("proxy.https")
	it := k.Get(s, e)
	if it.Value != "http://corp%5Cuser:***@proxy.corp:3128" || it.Source != SourceFile {
		t.Fatalf("shown as %+v", it)
	}
	for _, it := range List(s, e) {
		if strings.Contains(it.Value, "s3cret") {
			t.Fatalf("list shows the password: %+v", it)
		}
	}
	e.ShowSecrets = true
	if it := k.Get(s, e); it.Value != "http://corp%5Cuser:s3cret@proxy.corp:3128" {
		t.Fatalf("--show-secrets: %q", it.Value)
	}
	e.ShowSecrets = false
	// Refusals never quote the value.
	for _, v := range []string{"ftp://u:s3cret@x", "http://u:s3cret@x/path", "http://u:s3cret@x:99999", "u:s3cret@x"} {
		_, err := set(s, "proxy.http", v)
		if err == nil || strings.Contains(err.Error(), "s3cret") {
			t.Errorf("%q: %v", v, err)
		}
	}
	// NO_PROXY is stored normalized.
	if s, err = set(s, "proxy.noProxy", " .corp , 10.0.0.0/8 "); err != nil || s.NoProxy != ".corp,10.0.0.0/8" {
		t.Fatalf("%v %q", err, s.NoProxy)
	}
	// A proxy on this host's loopback needs the host gateway, for its port.
	if s, err = set(s, "proxy.http", "http://127.0.0.1:3128"); err != nil {
		t.Fatal(err)
	}
	if _, err := set(s, "hostGateway.enabled", "false"); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("turning the gateway off under a loopback proxy: %v", err)
	}
	if _, err := set(s, "hostGateway.ports", "5432"); err == nil {
		t.Fatal("a port list without the proxy's port was accepted")
	}
	if _, err := set(s, "hostGateway.ports", "3128,5432"); err != nil {
		t.Fatal(err)
	}
	// Review L-E: the gateway connects to 127.0.0.1, not [::1].
	if _, err := set(s, "proxy.https", "http://u:s3cret@[::1]:3128"); err == nil || !strings.Contains(err.Error(), "bind the relay to 127.0.0.1") || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("[::1] proxy: %v", err)
	}
	// An unrelated key is not refused for it.
	off := false
	bad := *s
	bad.HostGatewayEnabled = &off
	if _, err := set(&bad, "memory", "2GiB"); err != nil {
		t.Fatal(err)
	}
}

func TestCAKeys(t *testing.T) {
	e, _ := testEnv(t)
	k, _ := Lookup("ca.import")
	if it := k.Get(nil, e); it.Value != "true" || it.Apply != ApplyDockerd {
		t.Fatalf("%+v", it)
	}
	s, err := k.Set(nil, "off", e)
	if err != nil || s.CAImported() {
		t.Fatal(err)
	}
	if again, _ := k.Set(s, "off", e); !Equal(again, s) {
		t.Fatal("not equal")
	}
	x, _ := Lookup("ca.extraFile")
	p := filepath.Join(t.TempDir(), "ca.pem")
	if _, err := x.Set(s, p, e); err == nil {
		t.Fatal("a missing file was accepted")
	}
	if err := os.WriteFile(p, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := x.Set(s, p, e); err == nil || !strings.Contains(err.Error(), "CERTIFICATE") {
		t.Fatalf("not PEM: %v", err)
	}
	if _, err := x.Set(s, `\\server\share\ca.pem`, e); err == nil {
		t.Fatal("a network path was accepted")
	}
}

func testCAPEM(t *testing.T) []byte {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "hb config test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		BasicConstraintsValid: true, IsCA: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// TestCAExtraFileCopy (review M-C): set copies the file it checked (the engine reads
// only the copy), records the copy's hash and the path as given; setting it again
// copies the file again; unset removes the copy.
func TestCAExtraFileCopy(t *testing.T) {
	e, _ := testEnv(t)
	var saved [][]byte
	e.SaveCAFile = func(b []byte) error { saved = append(saved, b); return nil }
	x, _ := Lookup("ca.extraFile")
	p := filepath.Join(t.TempDir(), "ca.pem")
	one := testCAPEM(t)
	if err := os.WriteFile(p, one, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := x.Set(nil, p, e)
	sum := sha256.Sum256(one)
	if err != nil || len(saved) != 1 || !bytes.Equal(saved[0], one) || s.CAExtraFile != p || s.CAExtraSHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("set: %v, %d saved, %+v", err, len(saved), s)
	}
	if it := x.Get(s, e); it.Value != p {
		t.Fatalf("shown as %q", it.Value)
	}
	two := append(one, testCAPEM(t)...)
	_ = os.WriteFile(p, two, 0o600)
	s2, err := x.Set(s, p, e)
	if err != nil || len(saved) != 2 || !bytes.Equal(saved[1], two) || s2.CAExtraSHA256 == s.CAExtraSHA256 || Equal(s, s2) {
		t.Fatalf("set again: %v, %+v", err, s2)
	}
	s3, err := x.Unset(s2, e)
	if err != nil || len(saved) != 3 || saved[2] != nil || s3.CAExtraFile != "" || s3.CAExtraSHA256 != "" {
		t.Fatalf("unset: %v, %+v", err, s3)
	}
	// A refused file saves nothing.
	_ = os.WriteFile(p, []byte("junk"), 0o600)
	if _, err := x.Set(s3, p, e); err == nil || len(saved) != 3 {
		t.Fatalf("junk: %v, %d saved", err, len(saved))
	}
}

// TestGUIKeys checks the GUI's own settings (REQ-UX-013, REQ-UX-016): the language is
// auto or a shipped one, and opening the GUI starts nothing unless asked.
func TestGUIKeys(t *testing.T) {
	e, _ := testEnv(t)
	lang, err := Lookup("gui.language")
	if err != nil {
		t.Fatal(err)
	}
	if it := lang.Get(nil, e); it.Value != "auto" || it.Source != SourceDefault {
		t.Fatalf("default language %+v", it)
	}
	s, err := lang.Set(nil, "KO", e)
	if err != nil || s.GUILanguage != "ko" {
		t.Fatalf("set ko: %v %+v", err, s)
	}
	if s, err = lang.Set(s, "auto", e); err != nil || s.GUILanguage != "" {
		t.Fatalf("set auto: %v %+v", err, s)
	}
	for _, bad := range []string{"fr", "ko-KR", "../x", "<script>"} {
		if _, err := lang.Set(nil, bad, e); err == nil {
			t.Errorf("gui.language %q accepted", bad)
		}
	}
	open, err := Lookup("gui.startOnOpen")
	if err != nil {
		t.Fatal(err)
	}
	if it := open.Get(nil, e); it.Value != "false" {
		t.Fatalf("gui.startOnOpen defaults to %s", it.Value)
	}
	if s, err := open.Set(nil, "true", e); err != nil || !s.GUIStartOnOpen {
		t.Fatalf("set: %v", err)
	}
}
