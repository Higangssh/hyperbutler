package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Higangssh/hyperbutler/internal/dockerenv"
	"github.com/Higangssh/hyperbutler/internal/guestctl"
)

func newCA(t *testing.T, name string) []byte {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		BasicConstraintsValid: true, IsCA: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

// envInit is a fakeInit whose dockerdConfig also installs environments, with a stock
// bundle that holds stock.
func envInit(t *testing.T, started bool, stock []byte) *fakeInit {
	f := newFakeInit(t, started)
	dir := filepath.Dir(f.d.path)
	f.d.denv = &dockerdEnv{env: filepath.Join(dir, "dockerd.env"), bundle: filepath.Join(dir, "ca.crt"), stock: filepath.Join(dir, "stock.crt")}
	if err := os.WriteFile(f.d.denv.stock, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: stock}), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

func encodeDoc(t *testing.T, d dockerenv.Doc) []byte {
	t.Helper()
	b, err := d.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func readOr(t *testing.T, p string) string {
	b, err := readOpt(p)
	if err != nil {
		t.Fatal(err)
	}
	if b == nil {
		return "<none>"
	}
	return string(b)
}

func TestDockerdEnvInstall(t *testing.T) {
	stock, corp := newCA(t, "stock"), newCA(t, "corp")
	f := envInit(t, false, stock)
	d, e, ctx := f.d, f.d.denv, context.Background()
	var cs dockerenv.CertSet
	cs.Add(corp, true)
	cs.Add(stock, true)
	doc := dockerenv.Doc{HTTPSProxy: "http://user:secret@proxy:3128", NoProxy: "localhost", CACerts: cs.PEM()}
	var logged []string
	old := logf
	logf = func(format string, a ...any) { logged = append(logged, fmt.Sprintf(format, a...)) }
	defer func() { logf = old }()
	// Boot: installed, no restart, no watch (the init has not started dockerd).
	r := d.applyEnv(ctx, encodeDoc(t, doc), false)
	if r.Code != guestctl.DaemonOK || r.Flags != guestctl.DaemonChanged || f.restarts != 0 {
		t.Fatalf("boot: %#v", r)
	}
	env := readOr(t, e.env)
	want := "HTTPS_PROXY=http://user:secret@proxy:3128\nNO_PROXY=localhost\nSSL_CERT_FILE=" + e.bundle + "\n"
	if env != want {
		t.Fatalf("env %q, want %q", env, want)
	}
	// The bundle is the stock bundle plus corp only (stock is already in it).
	certs, err := dockerenv.ParsePEMFile([]byte(readOr(t, e.bundle)))
	if err != nil || len(certs) != 2 {
		t.Fatalf("bundle: %d certs, %v", len(certs), err)
	}
	f.start()
	// The same again: nothing changes, nothing restarts.
	if r := d.applyEnv(ctx, encodeDoc(t, doc), false); r.Flags != 0 || f.restarts != 0 {
		t.Fatalf("same: %#v", r)
	}
	// A change on a running dockerd restarts it, even without the flag.
	doc.CACerts = ""
	r = d.applyEnv(ctx, encodeDoc(t, doc), false)
	if r.Code != guestctl.DaemonOK || r.Flags != guestctl.DaemonChanged|guestctl.DaemonRestarted || f.restarts != 1 {
		t.Fatalf("change: %#v, %d restarts", r, f.restarts)
	}
	if readOr(t, e.bundle) != "<none>" || strings.Contains(readOr(t, e.env), "SSL_CERT_FILE") {
		t.Fatal("bundle not removed with the certificates")
	}
	// Removal of everything.
	if r := d.applyEnv(ctx, nil, false); r.Code != guestctl.DaemonOK || readOr(t, e.env) != "<none>" || f.restarts != 2 {
		t.Fatalf("removal: %#v", r)
	}
	// A document that does not decode is refused, and the message has no URL in it.
	r = d.applyEnv(ctx, []byte(`{"httpProxy":"ftp://user:secret@x"}`), true)
	if r.Code != guestctl.DaemonRejected || strings.Contains(r.Message, "secret") || f.restarts != 2 {
		t.Fatalf("rejected: %#v", r)
	}
	for _, l := range logged {
		if strings.Contains(l, "secret") {
			t.Fatalf("logged %q", l)
		}
	}
}

func TestDockerdEnvRollback(t *testing.T) {
	f := envInit(t, true, newCA(t, "stock"))
	d, e, ctx := f.d, f.d.denv, context.Background()
	good := encodeDoc(t, dockerenv.Doc{HTTPProxy: "http://proxy:3128"})
	if r := d.applyEnv(ctx, good, false); r.Code != guestctl.DaemonOK || f.restarts != 1 {
		t.Fatalf("%#v", r)
	}
	// dockerd does not come back with this one: the previous environment returns.
	r := d.applyEnv(ctx, encodeDoc(t, dockerenv.Doc{HTTPProxy: "http://fail.invalid:3128"}), false)
	if r.Code != guestctl.DaemonRolledBack || !f.running || f.restarts != 3 {
		t.Fatalf("rollback: %#v, %d restarts", r, f.restarts)
	}
	if readOr(t, e.env) != "HTTP_PROXY=http://proxy:3128\n" {
		t.Fatalf("env after rollback %q", readOr(t, e.env))
	}
	if _, err := os.Stat(d.hold); err == nil {
		t.Fatal("hold file left behind")
	}
}

// TestDockerdEnvAndDaemonConfigSerialized applies both at once: they take turns, and
// both files end up installed with every restart watched.
func TestDockerdEnvAndDaemonConfigSerialized(t *testing.T) {
	f := envInit(t, true, newCA(t, "stock"))
	d, ctx := f.d, context.Background()
	var wg sync.WaitGroup
	var r1 guestctl.EnvResult
	var r2 guestctl.DaemonResult
	wg.Add(2)
	go func() {
		defer wg.Done()
		r1 = d.applyEnv(ctx, encodeDoc(t, dockerenv.Doc{HTTPProxy: "http://proxy:1"}), false)
	}()
	go func() { defer wg.Done(); r2 = d.apply(ctx, []byte(`{"debug":true}`), true) }()
	wg.Wait()
	if r1.Code != guestctl.DaemonOK || r2.Code != guestctl.DaemonOK || f.restarts != 2 || !f.running {
		t.Fatalf("%#v %#v, %d restarts", r1, r2, f.restarts)
	}
}

// startingInit is a fakeInit whose init timed out waiting for daemon.ready: it has
// created its starting marker and starts dockerd after delay.
func startingInit(t *testing.T, delay time.Duration) *fakeInit {
	f := envInit(t, false, newCA(t, "stock"))
	f.d.starting = filepath.Join(filepath.Dir(f.d.path), "dockerd.starting")
	if err := os.WriteFile(f.d.starting, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if delay > 0 {
		done := make(chan struct{})
		t.Cleanup(func() { <-done })
		go func() {
			defer close(done)
			time.Sleep(delay)
			f.start()
		}()
	}
	return f
}

// TestDockerdStartingIsNotBoot (review L-C): once the init has passed its boot wait and
// is starting dockerd, what arrives is not installed for a dockerd that has yet to
// start (it may have read the files already): both the environment and daemon.json wait
// for that dockerd and restart it.
func TestDockerdStartingIsNotBoot(t *testing.T) {
	ctx := context.Background()
	f := startingInit(t, 30*time.Millisecond)
	r := f.d.applyEnv(ctx, encodeDoc(t, dockerenv.Doc{HTTPProxy: "http://proxy:3128"}), false)
	if r.Code != guestctl.DaemonOK || r.Flags != guestctl.DaemonChanged|guestctl.DaemonRestarted || f.restarts != 1 || f.gen != 2 {
		t.Fatalf("environment: %#v, %d restarts, gen %d", r, f.restarts, f.gen)
	}
	f2 := startingInit(t, 30*time.Millisecond)
	r2 := f2.d.apply(ctx, []byte(`{"debug":true}`), false)
	if r2.Code != guestctl.DaemonOK || r2.Flags != guestctl.DaemonChanged|guestctl.DaemonRestarted || f2.restarts != 1 {
		t.Fatalf("daemon.json: %#v, %d restarts", r2, f2.restarts)
	}
	// Nothing changed: nothing to restart for, but it still waited for the dockerd.
	if r := f2.d.apply(ctx, []byte(`{"debug":true}`), false); r.Code != guestctl.DaemonOK || r.Flags != 0 || f2.restarts != 1 {
		t.Fatalf("unchanged: %#v", r)
	}
	// The init never gets dockerd going: a failure after upTimeout, not a boot install.
	f3 := startingInit(t, 0)
	f3.d.upTimeout = 20 * time.Millisecond
	if r := f3.d.applyEnv(ctx, encodeDoc(t, dockerenv.Doc{HTTPProxy: "http://proxy:1"}), false); r.Code != guestctl.DaemonFailed || !strings.Contains(r.Message, "starting dockerd") {
		t.Fatalf("never started: %#v", r)
	}
	// Without the marker it is boot, as before: installed, no restart.
	f4 := envInit(t, false, newCA(t, "stock"))
	f4.d.starting = filepath.Join(filepath.Dir(f4.d.path), "dockerd.starting")
	if r := f4.d.applyEnv(ctx, encodeDoc(t, dockerenv.Doc{HTTPProxy: "http://proxy:1"}), false); r.Code != guestctl.DaemonOK || r.Flags != guestctl.DaemonChanged || f4.restarts != 0 {
		t.Fatalf("boot: %#v", r)
	}
}

// TestDockerdEnvAfterBoot: an environment that arrives after daemon.json settled (an
// engine that sent it late, or the init's boot wait timed out) is never reported as in
// effect while dockerd runs without it: dockerd is restarted with it. The engine sends
// the environment first, so this is the fallback, not the boot path.
func TestDockerdEnvAfterBoot(t *testing.T) {
	f := envInit(t, false, newCA(t, "stock"))
	d, ctx := f.d, context.Background()
	booted := f.bootInit(t)
	if r := d.apply(ctx, []byte(`{"debug":true}`), false); r.Code != guestctl.DaemonOK {
		t.Fatal(r)
	}
	<-booted
	r := d.applyEnv(ctx, encodeDoc(t, dockerenv.Doc{HTTPProxy: "http://proxy:3128"}), false)
	if r.Code != guestctl.DaemonOK || r.Flags != guestctl.DaemonChanged|guestctl.DaemonRestarted || f.restarts != 1 {
		t.Fatalf("late environment: %#v, %d restarts", r, f.restarts)
	}
	// Files are private: they can hold proxy credentials.
	if fi, err := os.Stat(d.denv.env); err != nil || runtime.GOOS == "linux" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("env file mode %v, %v", fi.Mode(), err)
	}
}
