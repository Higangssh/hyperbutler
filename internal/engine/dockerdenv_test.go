package engine

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
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

func TestBuildDockerdEnv(t *testing.T) {
	off := false
	s := &Settings{
		CAImport: &off, CAExtraFile: testCAFile(t),
		HTTPProxy: "http://user:s3cret@127.0.0.1:3128", HTTPSProxy: "http://proxy.corp:8080", NoProxy: "corp.example,10.0.0.0/8",
	}
	daemonJSON := []byte(`{"insecure-registries":["192.168.1.5:5000","10.1.0.0/16"],"Registry-Mirrors":["https://mirror.lan:5000"]}`)
	doc, st := BuildDockerdEnv(s, testEnvNet, daemonJSON)
	if st.Error != "" {
		t.Fatal(st.Error)
	}
	if doc.HTTPProxy != "http://user:s3cret@192.168.127.254:3128" || st.HTTPProxy != "http://user:***@192.168.127.254:3128" {
		t.Fatalf("loopback proxy %q (state %q)", doc.HTTPProxy, st.HTTPProxy)
	}
	want := "localhost,127.0.0.1,::1,192.168.127.0/24,host.docker.internal,gateway.docker.internal,192.168.127.254,corp.example,10.0.0.0/8,10.1.0.0/16,192.168.1.5,mirror.lan"
	if doc.NoProxy != want {
		t.Fatalf("NO_PROXY %q\nwant     %q", doc.NoProxy, want)
	}
	if st.CACerts != 1 || st.CAImport || len(doc.Certs()) != 1 {
		t.Fatalf("CA: %+v", st)
	}
	// No proxy: no NO_PROXY either, and nothing at all without certificates.
	doc, st = BuildDockerdEnv(&Settings{CAImport: &off, NoProxy: "x"}, testEnvNet, nil)
	if !doc.IsZero() || st.Error != "" {
		t.Fatalf("%+v %+v", doc, st)
	}
	// A missing extra file is reported, not fatal.
	_, st = BuildDockerdEnv(&Settings{CAImport: &off, CAExtraFile: filepath.Join(t.TempDir(), "none.pem")}, testEnvNet, nil)
	if !strings.Contains(st.Error, "ca.extraFile") {
		t.Fatalf("error %q", st.Error)
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
	_, st := BuildDockerdEnv(&Settings{CAImport: &off, HTTPProxy: "http://user:s3cret@proxy:1", HTTPSProxy: "socks5://tok3n@proxy:2"}, testEnvNet, nil)
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
