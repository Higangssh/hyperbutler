package engine

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Higangssh/hyperbutler/internal/dockerenv"
	"github.com/Higangssh/hyperbutler/internal/guestctl"
)

var testEnvNet = EnvNet{Subnet: netip.MustParsePrefix("192.168.127.0/24"), HostGW: netip.MustParseAddr("192.168.127.254")}

func testCAFile(t *testing.T) string {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "hb test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		BasicConstraintsValid: true, IsCA: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// noStores keeps BuildDockerdEnv away from this machine's certificate stores; disallowed
// are the Untrusted Certificates.
func noStores(t testing.TB, disallowed ...[]byte) {
	oldD, oldR := loadDisallowed, importRoots
	loadDisallowed = func(set *dockerenv.CertSet) error {
		set.Disallowed = map[[32]byte]bool{}
		for _, d := range disallowed {
			set.Disallowed[sha256.Sum256(d)] = true
		}
		return nil
	}
	importRoots = func(*dockerenv.CertSet) error { return nil }
	t.Cleanup(func() { loadDisallowed, importRoots = oldD, oldR })
}

// caSettings returns settings whose ca.extraFile is pemFile, with the engine's copy of
// it made as `config set` does, and the copy's path.
func caSettings(t *testing.T, pemFile string) (*Settings, string) {
	t.Helper()
	b, err := os.ReadFile(pemFile)
	if err != nil {
		t.Fatal(err)
	}
	l := Layout{Home: t.TempDir()}
	if err := SaveCAExtra(l, b); err != nil {
		t.Fatal(err)
	}
	off := false
	return &Settings{CAImport: &off, CAExtraFile: pemFile, CAExtraSHA256: digest(b)}, l.CAExtraPath()
}

// longNoProxy is a valid NO_PROXY list of exactly n bytes.
func longNoProxy(n int) string {
	var b strings.Builder
	for i := 0; b.Len() < n-40; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "h%d.corp.example", i)
	}
	b.WriteByte(',')
	b.WriteString(strings.Repeat("a", n-b.Len()))
	return b.String()
}

func TestBuildDockerdEnv(t *testing.T) {
	noStores(t)
	s, caCopy := caSettings(t, testCAFile(t))
	s.HTTPProxy, s.HTTPSProxy, s.NoProxy = "http://user:s3cret@127.0.0.1:3128", "http://proxy.corp:8080", "corp.example,10.0.0.0/8"
	daemonJSON := []byte(`{"insecure-registries":["192.168.1.5:5000","10.1.0.0/16"],"Registry-Mirrors":["https://mirror.lan:5000"]}`)
	doc, st := BuildDockerdEnv(s, testEnvNet, daemonJSON, caCopy)
	if st.Error != "" {
		t.Fatal(st.Error)
	}
	if doc.HTTPProxy != "http://user:s3cret@192.168.127.254:3128" || st.HTTPProxy != "http://user:***@192.168.127.254:3128" {
		t.Fatalf("loopback proxy %q (state %q)", doc.HTTPProxy, st.HTTPProxy)
	}
	defaults := "localhost,127.0.0.1,::1,192.168.127.0/24,host.docker.internal,gateway.docker.internal,192.168.127.254"
	want := defaults + ",corp.example,10.0.0.0/8,10.1.0.0/16,192.168.1.5,mirror.lan"
	if doc.NoProxy != want {
		t.Fatalf("NO_PROXY %q\nwant     %q", doc.NoProxy, want)
	}
	if st.CACerts != 1 || st.CAImport || len(doc.Certs()) != 1 {
		t.Fatalf("CA: %+v", st)
	}

	// Review M-A: the longest list `config set` takes, with the defaults and the
	// registries added, is still sent whole, with the certificates.
	s.NoProxy = longNoProxy(4090)
	if dockerenv.ValidateNoProxy(s.NoProxy) != nil || len(s.NoProxy) != 4090 {
		t.Fatalf("test list of %d bytes is invalid", len(s.NoProxy))
	}
	doc, st = BuildDockerdEnv(s, testEnvNet, daemonJSON, caCopy)
	if err := doc.Validate(); err != nil || st.Error != "" {
		t.Fatalf("4090-byte NO_PROXY: %v; state error %q", err, st.Error)
	}
	if doc.NoProxy != defaults+","+s.NoProxy+",10.1.0.0/16,192.168.1.5,mirror.lan" || st.NoProxy != doc.NoProxy || st.HTTPSProxy == "" || len(doc.Certs()) != 1 || st.CACerts != 1 {
		t.Fatalf("4090-byte NO_PROXY: %d bytes sent, state %+v", len(doc.NoProxy), st)
	}
	// Registries that do not fit are left out and counted; the rest is unchanged.
	var regs []string
	for i := 0; i < 100; i++ {
		regs = append(regs, fmt.Sprintf(`"registry-%03d.lan.example:5000"`, i))
	}
	doc, st = BuildDockerdEnv(s, testEnvNet, []byte(`{"insecure-registries":[`+strings.Join(regs, ",")+`]}`), caCopy)
	if err := doc.Validate(); err != nil || !strings.Contains(st.Error, "NO_PROXY entries (daemon.json registries) left out") ||
		!strings.HasPrefix(doc.NoProxy, defaults+","+s.NoProxy+",registry-000") || len(doc.Certs()) != 1 {
		t.Fatalf("many registries: %v; %d bytes; state error %q", err, len(doc.NoProxy), st.Error)
	}

	// No proxy: no NO_PROXY either, and nothing at all without certificates.
	off := false
	doc, st = BuildDockerdEnv(&Settings{CAImport: &off, NoProxy: "x"}, testEnvNet, nil, caCopy)
	if !doc.IsZero() || st.Error != "" {
		t.Fatalf("%+v %+v", doc, st)
	}
}

// TestBuildDockerdEnvCACopy (review M-C): only the engine's copy of ca.extraFile is read,
// never the file the settings name, and a missing or different copy is reported.
func TestBuildDockerdEnvCACopy(t *testing.T) {
	noStores(t)
	src := testCAFile(t)
	s, caCopy := caSettings(t, src)
	// The original is gone (or on a share that hangs): the copy is used.
	if err := os.Remove(src); err != nil {
		t.Fatal(err)
	}
	doc, st := BuildDockerdEnv(s, testEnvNet, nil, caCopy)
	if st.Error != "" || len(doc.Certs()) != 1 {
		t.Fatalf("copy not used: %q", st.Error)
	}
	// settings.json edited to name another file: the copy is still what is used, and
	// the state says it is not the one recorded.
	s2 := *s
	s2.CAExtraFile, s2.CAExtraSHA256 = `\\server\share\other.pem`, ""
	doc, st = BuildDockerdEnv(&s2, testEnvNet, nil, caCopy)
	if len(doc.Certs()) != 1 || !strings.Contains(st.Error, "not the one `config set` made") {
		t.Fatalf("edited settings: %d certs, %q", len(doc.Certs()), st.Error)
	}
	// No copy at all: reported, nothing read from the named path.
	_ = os.Remove(caCopy)
	doc, st = BuildDockerdEnv(s, testEnvNet, nil, caCopy)
	if !doc.IsZero() || !strings.Contains(st.Error, "has no copy of it") {
		t.Fatalf("no copy: %+v, %q", doc, st.Error)
	}
}

// TestBuildDockerdEnvDisallowedExtraFile (review L-A): a certificate of ca.extraFile
// that is in an Untrusted Certificates store is left out and counted as disallowed.
func TestBuildDockerdEnvDisallowedExtraFile(t *testing.T) {
	src := testCAFile(t)
	b, _ := os.ReadFile(src)
	blk, _ := pem.Decode(b)
	noStores(t, blk.Bytes)
	s, caCopy := caSettings(t, src)
	doc, st := BuildDockerdEnv(s, testEnvNet, nil, caCopy)
	if len(doc.Certs()) != 0 || st.CACerts != 0 || st.CAStats.Disallowed != 1 || st.CASkipped != 1 {
		t.Fatalf("disallowed certificate sent: %+v", st)
	}
}

// TestSettleDoc (review M-A): a document that does not validate, which BuildDockerdEnv
// cannot produce, drops the proxy settings but keeps the certificates, and the state
// says so instead of reporting the proxy in effect.
func TestSettleDoc(t *testing.T) {
	pemB, _ := os.ReadFile(testCAFile(t))
	var set dockerenv.CertSet
	blk, _ := pem.Decode(pemB)
	set.Add(blk.Bytes, false)
	doc := &dockerenv.Doc{HTTPSProxy: "http://proxy:3128", NoProxy: "bad entry!", CACerts: set.PEM()}
	st := EnvState{HTTPSProxy: "http://proxy:3128", NoProxy: "bad entry!", CACerts: 1, CAStats: set.Stats}
	got, err := settleDoc(doc, &st)
	if err == nil || got.HTTPSProxy != "" || got.NoProxy != "" || got.CACerts != doc.CACerts || got.Validate() != nil {
		t.Fatalf("settle: %+v, %v", got, err)
	}
	if st.HTTPSProxy != "" || st.NoProxy != "" || st.CACerts != 1 || st.CAStats.Added != 1 {
		t.Fatalf("state %+v", st)
	}
	// Invalid certificates too: nothing.
	st = EnvState{HTTPProxy: "x", CACerts: 1}
	got, err = settleDoc(&dockerenv.Doc{HTTPProxy: "ftp://x", CACerts: "junk"}, &st)
	if err == nil || !got.IsZero() || st.CACerts != 0 || st.HTTPProxy != "" {
		t.Fatalf("all invalid: %+v %+v %v", got, st, err)
	}
	// A valid one is returned as it is.
	ok := &dockerenv.Doc{HTTPProxy: "http://proxy:1"}
	if got, err := settleDoc(ok, &EnvState{}); err != nil || got != ok {
		t.Fatal(err)
	}
}

func TestSaveCAExtra(t *testing.T) {
	l := Layout{Home: t.TempDir()}
	if err := SaveCAExtra(l, []byte("one")); err != nil {
		t.Fatal(err)
	}
	if err := SaveCAExtra(l, []byte("two")); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(l.CAExtraPath()); err != nil || string(b) != "two" {
		t.Fatalf("%q %v", b, err)
	}
	if m, _ := filepath.Glob(l.CAExtraPath() + ".tmp-*"); len(m) != 0 {
		t.Fatalf("left behind: %v", m)
	}
	checkUserOnly(t, l.CAExtraPath())
	if err := SaveCAExtra(l, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(l.CAExtraPath()); !os.IsNotExist(err) {
		t.Fatal("not removed")
	}
	if err := SaveCAExtra(l, nil); err != nil {
		t.Fatal("removing nothing:", err)
	}
}

func TestCheckProxyReach(t *testing.T) {
	off := false
	for _, c := range []struct {
		s  Settings
		ok bool
	}{
		{Settings{HTTPProxy: "http://127.0.0.1:3128"}, true},
		{Settings{HTTPProxy: "http://localhost:3128", HostGatewayEnabled: &off}, false},
		{Settings{HTTPSProxy: "http://[::1]:3128", HostGatewayPorts: "5432"}, false},
		{Settings{HTTPSProxy: "http://127.0.0.1", HostGatewayPorts: "80"}, true},
		{Settings{HTTPProxy: "http://proxy.corp:3128", HostGatewayEnabled: &off}, true},
	} {
		if err := c.s.CheckProxyReach(); (err == nil) != c.ok {
			t.Errorf("%+v: %v", c.s, err)
		}
	}
}

// TestCheckProxyReachOtherLoopback (review L-E): the host gateway connects to 127.0.0.1
// (and to [::1] only when that refuses), so a proxy given as [::1] or another 127.x
// address would get whatever listens on 127.0.0.1, credentials included. It is refused
// with a way out, and not rewritten to the gateway.
func TestCheckProxyReachOtherLoopback(t *testing.T) {
	for _, p := range []string{"http://u:pw@[::1]:3128", "http://127.0.0.2:3128", "socks5://[0:0:0:0:0:0:0:1]:1080"} {
		err := (&Settings{HTTPSProxy: p}).CheckProxyReach()
		if err == nil || !strings.Contains(err.Error(), "bind the relay to 127.0.0.1") || strings.Contains(err.Error(), "pw") {
			t.Errorf("%s: %v", p, err)
		}
		noStores(t)
		off := false
		doc, st := BuildDockerdEnv(&Settings{CAImport: &off, HTTPSProxy: p}, testEnvNet, nil, "")
		if strings.Contains(doc.HTTPSProxy, "192.168.127.254") || !strings.Contains(st.Error, "bind the relay") {
			t.Errorf("%s: sent %q, state error %q", p, doc.HTTPSProxy, st.Error)
		}
	}
	for _, p := range []string{"http://localhost:3128", "http://127.0.0.1:3128", "http://[::ffff:127.0.0.1]:3128"} {
		if err := (&Settings{HTTPSProxy: p}).CheckProxyReach(); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
}

// envTest is a DaemonConfig whose environment comes from *doc.
func envTest(t *testing.T) (*DaemonConfig, *fakeGuest, *dockerenv.Doc, *bytes.Buffer) {
	dc, g, _ := newDaemonTest(t)
	g.features |= guestctl.FeatureDockerdEnv
	doc := &dockerenv.Doc{HTTPProxy: "http://user:s3cret@proxy:3128"}
	dc.Env = func() (*dockerenv.Doc, EnvState) {
		d := *doc
		return &d, EnvState{HTTPProxy: dockerenv.Redact(d.HTTPProxy)}
	}
	var logBuf bytes.Buffer
	dc.Log = slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return dc, g, doc, &logBuf
}

// TestDockerdEnvBootOrder: the environment goes before daemon.json, so the guest's one
// readiness gate (daemon.ready) covers both.
func TestDockerdEnvBootOrder(t *testing.T) {
	dc, g, _, _ := envTest(t)
	writeUser(t, dc, `{"debug":true}`)
	dc.Connected(context.Background(), g)
	if !reflect.DeepEqual(g.order, []string{"env", "daemon"}) {
		t.Fatalf("order %v", g.order)
	}
	if err := dc.WaitDelivered(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st := dc.State(); st.Env == nil || st.Env.SHA256 == "" || st.Env.Error != "" {
		t.Fatalf("state %+v", st.Env)
	}
}

// TestDockerdEnvReconnectKeepsInEffect: the certificate stores change between boot and
// a reconnect; the reconnect sends what is in effect, so dockerd is not restarted.
// Only Apply takes the new set.
func TestDockerdEnvReconnectKeepsInEffect(t *testing.T) {
	dc, g, doc, _ := envTest(t)
	ctx := context.Background()
	dc.Connected(ctx, g)
	first := g.env
	doc.HTTPProxy = "http://other:3128" // what a new import would give now
	dc.Connected(ctx, g)
	if g.dockerdPID != 0 || !bytes.Equal(g.env, first) {
		t.Fatalf("reconnect restarted dockerd (%d) or sent a new environment", g.dockerdPID)
	}
	// Apply: one restart for the new environment; daemon.json, unchanged, is sent
	// without another.
	if err := dc.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if g.dockerdPID != 1 || bytes.Equal(g.env, first) || g.restarts[len(g.restarts)-1] {
		t.Fatalf("apply: pid %d, restarts %v", g.dockerdPID, g.restarts)
	}
	if st := dc.State(); !st.Restarted || st.Env == nil || !st.Env.Restarted {
		t.Fatalf("state %+v", st)
	}
	// Apply with nothing changed: daemon.json's restart, nothing else.
	if err := dc.Apply(ctx); err != nil || g.dockerdPID != 2 {
		t.Fatalf("%v, pid %d", err, g.dockerdPID)
	}
}

// TestDockerdEnvReconnectKeepsBuildError (review L-B): an environment built with a
// problem (a certificate file that could not be read) still has it after a reconnect,
// which re-sends that environment; the next build, by an apply, clears it.
func TestDockerdEnvReconnectKeepsBuildError(t *testing.T) {
	dc, g, doc, _ := envTest(t)
	builds := 0
	buildErr := "ca.extraFile D:\\ca.pem: hyperbutler has no copy of it"
	dc.Env = func() (*dockerenv.Doc, EnvState) {
		builds++
		d := *doc
		return &d, EnvState{HTTPProxy: dockerenv.Redact(d.HTTPProxy), Error: buildErr}
	}
	ctx := context.Background()
	dc.Connected(ctx, g)
	if st := dc.State(); st.Env == nil || st.Env.Error != buildErr {
		t.Fatalf("boot: %+v", st.Env)
	}
	dc.Connected(ctx, g)
	if st := dc.State(); builds != 1 || st.Env == nil || st.Env.Error != buildErr || st.Env.SHA256 == "" {
		t.Fatalf("reconnect: %d builds, %+v", builds, st.Env)
	}
	buildErr = ""
	_ = dc.Apply(ctx)
	dc.Connected(ctx, g)
	if st := dc.State(); builds != 2 || st.Env.Error != "" {
		t.Fatalf("after apply: %d builds, %+v", builds, st.Env)
	}
}

// TestDockerdEnvNoSecrets: a proxy password reaches the guest and nowhere else: not
// the engine log, not the state (state.json, status --json).
func TestDockerdEnvNoSecrets(t *testing.T) {
	dc, g, _, logBuf := envTest(t)
	var states []DaemonState
	dc.OnState = func(s DaemonState) { states = append(states, s) }
	dc.Connected(context.Background(), g)
	_ = dc.Apply(context.Background())
	if !bytes.Contains(g.env, []byte("s3cret")) {
		t.Fatal("the guest did not get the credentials")
	}
	b, _ := json.Marshal(states)
	if bytes.Contains(b, []byte("s3cret")) || strings.Contains(logBuf.String(), "s3cret") {
		t.Fatalf("the password leaked:\n%s\n%s", b, logBuf)
	}
	if !strings.Contains(logBuf.String(), "user:***@proxy:3128") {
		t.Fatalf("log %s", logBuf)
	}
	// The full path: settings to state, through BuildDockerdEnv.
	off := false
	_, st := BuildDockerdEnv(&Settings{CAImport: &off, HTTPProxy: "http://user:s3cret@proxy:1", HTTPSProxy: "socks5://tok3n@proxy:2"}, testEnvNet, nil, "")
	if b, _ := json.Marshal(st); bytes.Contains(b, []byte("s3cret")) || bytes.Contains(b, []byte("tok3n")) {
		t.Fatalf("state %s", b)
	}
}

// TestDockerdEnvOldGuest: a guest image without the feature gets nothing, and the state
// says why when there was something to send.
func TestDockerdEnvOldGuest(t *testing.T) {
	dc, g, _, _ := envTest(t)
	g.features &^= guestctl.FeatureDockerdEnv
	dc.Connected(context.Background(), g)
	if len(g.envs) != 0 {
		t.Fatal("sent to a guest without the feature")
	}
	if st := dc.State(); st.Env == nil || !strings.Contains(st.Env.Error, "update it") {
		t.Fatalf("state %+v", st.Env)
	}
}
