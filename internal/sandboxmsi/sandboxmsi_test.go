package sandboxmsi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func hashOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestParsePin(t *testing.T) {
	sum := strings.Repeat("a", 64)
	good := "hyperbutler-v0.2.0-rc.1-sandbox-windows-amd64.msi"
	if _, err := ParsePin("", "", ""); !errors.Is(err, ErrNoPin) {
		t.Errorf("empty: %v", err)
	}
	p, err := ParsePin(sum, good, "270000000")
	if err != nil || p.Bytes != 270000000 || p.Name != good || p.SHA256 != sum {
		t.Fatalf("%+v %v", p, err)
	}
	for _, c := range [][3]string{
		{"", good, "1"},
		{sum, "", "1"},
		{sum, good, ""},
		{strings.Repeat("A", 64), good, "1"},
		{sum[:63], good, "1"},
		{sum + "a", good, "1"},
		{strings.Repeat("g", 64), good, "1"},
		{sum, "a/b.msi", "1"},
		{sum, `a\b.msi`, "1"},
		{sum, "a..b.msi", "1"},
		{sum, "a.exe", "1"},
		{sum, ".msi", "1"},
		{sum, "a b.msi", "1"},
		{sum, "a%2fb.msi", "1"},
		{sum, strings.Repeat("a", 200) + ".msi", "1"},
		{sum, good, "0"},
		{sum, good, "-1"},
		{sum, good, "+1"},
		{sum, good, "01"},
		{sum, good, "1e9"},
		{sum, good, fmt.Sprint(int64(MaxPinBytes) + 1)},
		{sum, good, "99999999999999999999"},
	} {
		if _, err := ParsePin(c[0], c[1], c[2]); err == nil || errors.Is(err, ErrNoPin) {
			t.Errorf("%q: %v", c, err)
		}
	}
}

func TestReleaseURL(t *testing.T) {
	u, err := ReleaseURL("v0.2.0-rc.1", "x.msi")
	if err != nil || u != "https://github.com/Higangssh/hyperbutler/releases/download/v0.2.0-rc.1/x.msi" {
		t.Fatalf("%q %v", u, err)
	}
	for _, c := range [][2]string{
		{"", "x.msi"},
		{"(devel)", "x.msi"},
		{"v1.2", "x.msi"},
		{"v1.2.3+dirty", "x.msi"},
		{"v1.2.3/../../evil", "x.msi"},
		{"v1.2.3?a=b", "x.msi"},
		{"v1.2.3", "../x.msi"},
		{"v1.2.3", "x.msi?a=b"},
		{"v1.2.3", "x"},
	} {
		if u, err := ReleaseURL(c[0], c[1]); err == nil {
			t.Errorf("%q: %q", c, u)
		}
	}
}

// FuzzReleaseURL: whatever goes in, what comes out stays below this project's releases.
func FuzzReleaseURL(f *testing.F) {
	f.Add("v1.2.3", "x.msi")
	f.Add("v1.2.3-rc.1", "hyperbutler-v1.2.3-rc.1-sandbox-windows-amd64.msi")
	f.Add("v1.2.3/../..", "a..msi")
	f.Add("v1.2.3\n", "a.msi\n")
	f.Fuzz(func(t *testing.T, tag, name string) {
		s, err := ReleaseURL(tag, name)
		if err != nil {
			return
		}
		u, err := url.Parse(s)
		if err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		rest, ok := strings.CutPrefix(s, ReleaseBase)
		if !ok || u.Host != "github.com" || u.Scheme != "https" || u.RawQuery != "" || u.Fragment != "" ||
			strings.Count(rest, "/") != 1 || strings.Contains(s, "..") || strings.ContainsAny(rest, "\\%?#\r\n ") {
			t.Fatalf("%q from %q, %q", s, tag, name)
		}
	})
}

// FuzzParsePin: a pin that passes is usable as a file name and a size.
func FuzzParsePin(f *testing.F) {
	f.Add(strings.Repeat("a", 64), "x.msi", "1")
	f.Add("", "", "")
	f.Fuzz(func(t *testing.T, sum, name, size string) {
		p, err := ParsePin(sum, name, size)
		if err != nil {
			return
		}
		if p.Bytes <= 0 || p.Bytes > MaxPinBytes || len(p.SHA256) != 64 || filepath.Base(p.Name) != p.Name || strings.ContainsAny(p.Name, `/\:*?"<>| `) {
			t.Fatalf("%+v", p)
		}
	})
}

func TestOutcome(t *testing.T) {
	for _, c := range []struct {
		code    uint32
		restart bool
		is      error
		exit    bool
	}{
		{code: 0},
		{code: 3010, restart: true},
		{code: 1641, restart: true},
		{code: 1602, is: ErrCanceled},
		{code: 1618, is: ErrBusy},
		{code: 1603, exit: true},
		{code: 1619, exit: true},
		{code: 1638, exit: true},
		{code: 1, exit: true},
		{code: 4294967295, exit: true},
	} {
		r, err := outcome(c.code)
		if r.RestartNeeded != c.restart {
			t.Errorf("%d: restart %v", c.code, r.RestartNeeded)
		}
		var ee *ExitError
		switch {
		case c.is != nil:
			if !errors.Is(err, c.is) {
				t.Errorf("%d: %v", c.code, err)
			}
		case c.exit:
			if !errors.As(err, &ee) || ee.Code != c.code || !strings.Contains(err.Error(), fmt.Sprint(c.code)) {
				t.Errorf("%d: %v", c.code, err)
			}
		case err != nil:
			t.Errorf("%d: %v", c.code, err)
		}
	}
}

// fakeLock hashes what the path holds at lock time, as the real one does, and records the
// sequence.
type fakeLock struct {
	path string
	sum  string
	log  *[]string
}

func (l *fakeLock) Path() string            { return l.path }
func (l *fakeLock) SHA256() (string, error) { return l.sum, nil }
func (l *fakeLock) Close() error            { *l.log = append(*l.log, "unlock"); return nil }

type installerFixture struct {
	*Installer
	log []string
}

func newInstaller(t *testing.T, payload []byte, exit uint32, execErr, verifyErr error) *installerFixture {
	t.Helper()
	fx := &installerFixture{}
	fx.Installer = &Installer{
		Pin: Pin{SHA256: hashOf(payload), Name: "x.msi", Bytes: int64(len(payload))},
		Dir: t.TempDir(),
		Fetch: func(_ context.Context, path string, _ int64) error {
			fx.log = append(fx.log, "fetch")
			return os.WriteFile(path, payload, 0o600)
		},
		Lock: func(path string) (Locked, error) {
			fx.log = append(fx.log, "lock")
			b, err := os.ReadFile(path)
			if err != nil {
				return nil, err
			}
			return &fakeLock{path: path, sum: hashOf(b), log: &fx.log}, nil
		},
		Exec: func(_ context.Context, path string, _ bool) (uint32, error) {
			fx.log = append(fx.log, "exec")
			if _, err := os.Stat(path); err != nil {
				t.Errorf("exec: %v", err)
			}
			return exit, execErr
		},
		Verify: func() error {
			fx.log = append(fx.log, "verify")
			return verifyErr
		},
	}
	return fx
}

func (fx *installerFixture) noTemp(t *testing.T) {
	t.Helper()
	ents, err := os.ReadDir(fx.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		t.Errorf("left behind: %v", ents)
	}
}

func TestInstallerSequence(t *testing.T) {
	fx := newInstaller(t, []byte("msi"), 0, nil, nil)
	r, err := fx.Run(context.Background())
	if err != nil || r.RestartNeeded {
		t.Fatalf("%+v %v", r, err)
	}
	if got := strings.Join(fx.log, " "); got != "fetch lock exec verify unlock" {
		t.Errorf("sequence: %s", got)
	}
	fx.noTemp(t)
}

func TestInstallerWrongHashNeverExecs(t *testing.T) {
	fx := newInstaller(t, []byte("msi"), 0, nil, nil)
	fx.Pin.SHA256 = hashOf([]byte("other"))
	_, err := fx.Run(context.Background())
	if !errors.Is(err, ErrHash) {
		t.Fatalf("%v", err)
	}
	if got := strings.Join(fx.log, " "); got != "fetch lock unlock" {
		t.Errorf("sequence: %s", got)
	}
	fx.noTemp(t)
}

func TestInstallerFailures(t *testing.T) {
	boom := errors.New("boom")
	for name, c := range map[string]struct {
		exit    uint32
		execErr error
		verify  error
		is      error
		restart bool
		ok      bool
	}{
		"declined":          {execErr: ErrDeclined, is: ErrDeclined},
		"canceled":          {exit: 1602, is: ErrCanceled},
		"busy":              {exit: 1618, is: ErrBusy},
		"restart":           {exit: 3010, restart: true, ok: true},
		"restart, not yet":  {exit: 3010, verify: boom, is: boom, restart: true},
		"exit 0, bad image": {verify: boom, is: boom},
	} {
		t.Run(name, func(t *testing.T) {
			fx := newInstaller(t, []byte("msi"), c.exit, c.execErr, c.verify)
			r, err := fx.Run(context.Background())
			if c.ok != (err == nil) || (c.is != nil && !errors.Is(err, c.is)) || r.RestartNeeded != c.restart {
				t.Errorf("%+v %v", r, err)
			}
			fx.noTemp(t)
		})
	}
}

func TestInstallerRemovesLeftovers(t *testing.T) {
	fx := newInstaller(t, []byte("msi"), 0, nil, nil)
	old := filepath.Join(fx.Dir, "dead-x.msi")
	other := filepath.Join(fx.Dir, "not-ours.txt")
	for _, p := range []string{old, other} {
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fx.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("old download: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("another file: %v", err)
	}
}

func TestCopy(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.msi")
	if err := os.WriteFile(src, []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dst.msi")
	if err := Copy(src)(context.Background(), dst, 5); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "12345" {
		t.Errorf("%q", b)
	}
	// It must create the file: an existing one might be somebody else's.
	if err := Copy(src)(context.Background(), dst, 5); !errors.Is(err, os.ErrExist) {
		t.Errorf("over an existing file: %v", err)
	}
	if err := Copy(src)(context.Background(), filepath.Join(dir, "small.msi"), 4); !errors.Is(err, ErrTooLarge) {
		t.Errorf("too large: %v", err)
	}
}

// tlsServer serves h on 127.0.0.1 (httptest binds loopback only) and returns a client that
// trusts it and nothing else.
func tlsServer(t *testing.T, h http.Handler) (*httptest.Server, *http.Client) {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	if !strings.HasPrefix(srv.URL, "https://127.0.0.1:") {
		t.Fatalf("server on %s", srv.URL)
	}
	c := NewClient(nil, srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs)
	t.Cleanup(c.CloseIdleConnections)
	return srv, c
}

func TestDownload(t *testing.T) {
	payload := []byte(strings.Repeat("x", 1<<20))
	var ua, gotHeaders string
	srv, c := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua = r.UserAgent()
		gotHeaders = fmt.Sprint(len(r.Header))
		_, _ = w.Write(payload)
	}))
	path := filepath.Join(t.TempDir(), "a.msi")
	var last, total int64
	err := Download(c, srv.URL, "hyperbutler/test", 0, func(d, n int64) { last, total = d, n })(context.Background(), path, int64(len(payload)))
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); hashOf(b) != hashOf(payload) {
		t.Error("the bytes differ")
	}
	if ua != "hyperbutler/test" || gotHeaders != "1" {
		t.Errorf("user agent %q, %s headers", ua, gotHeaders)
	}
	if last != int64(len(payload)) || total != last {
		t.Errorf("progress %d of %d", last, total)
	}
}

func TestDownloadRefuses(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("x"))
	}))
	defer plain.Close()
	if !strings.HasPrefix(plain.URL, "http://127.0.0.1:") {
		t.Fatalf("server on %s", plain.URL)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/toplain", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, plain.URL+"/x", http.StatusFound) })
	mux.HandleFunc("/loop", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/loop", http.StatusFound) })
	mux.HandleFunc("/ok", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("12345")) })
	mux.HandleFunc("/hop", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/ok", http.StatusFound) })
	mux.HandleFunc("/missing", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	mux.HandleFunc("/toolarge", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "6")
		_, _ = w.Write([]byte("123456"))
	})
	mux.HandleFunc("/chunked", func(w http.ResponseWriter, _ *http.Request) {
		// No Content-Length, so only the count of what arrives can stop it.
		for range 3 {
			_, _ = w.Write([]byte("123"))
			w.(http.Flusher).Flush()
		}
	})
	mux.HandleFunc("/short", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "5")
		_, _ = w.Write([]byte("12"))
	})
	srv, c := tlsServer(t, mux)

	dir := t.TempDir()
	fetch := func(address string) error {
		name, err := tempName("x.msi")
		if err != nil {
			t.Fatal(err)
		}
		return Download(c, address, "t", 0, nil)(context.Background(), filepath.Join(dir, name), 5)
	}
	if err := fetch(srv.URL + "/ok"); err != nil {
		t.Errorf("plain https: %v", err)
	}
	if err := fetch(srv.URL + "/hop"); err != nil {
		t.Errorf("a redirect to https: %v", err)
	}
	for _, path := range []string{"/toplain", "/loop", "/missing", "/toolarge", "/chunked", "/short"} {
		if err := fetch(srv.URL + path); err == nil {
			t.Errorf("%s: no error", path)
		}
	}
	if err := fetch(srv.URL + "/toolarge"); !errors.Is(err, ErrTooLarge) {
		t.Errorf("Content-Length over the pin: %v", err)
	}
	if err := fetch(srv.URL + "/chunked"); !errors.Is(err, ErrTooLarge) {
		t.Errorf("chunked over the pin: %v", err)
	}
	if err := fetch(srv.URL + "/toplain"); err == nil || !strings.Contains(err.Error(), "not https") {
		t.Errorf("redirect to http: %v", err)
	}
	if err := fetch(plain.URL); err == nil || !strings.Contains(err.Error(), "not an https URL") {
		t.Errorf("an http URL: %v", err)
	}
	if err := fetch("ftp://127.0.0.1/x"); err == nil {
		t.Error("an ftp URL")
	}
}

func TestDownloadTrustsOnlyItsRoots(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	c := NewClient(nil, nil) // the system's roots do not know the test server
	defer c.CloseIdleConnections()
	path := filepath.Join(t.TempDir(), "a.msi")
	if err := Download(c, srv.URL, "t", 0, nil)(context.Background(), path, 5); err == nil {
		t.Error("an untrusted certificate was accepted")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a file was created: %v", err)
	}
}

func TestDownloadStall(t *testing.T) {
	release := make(chan struct{})
	srv, c := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("1"))
		w.(http.Flusher).Flush()
		<-release
	}))
	defer close(release)
	path := filepath.Join(t.TempDir(), "a.msi")
	err := Download(c, srv.URL, "t", 100*time.Millisecond, nil)(context.Background(), path, 5)
	if err == nil || !strings.Contains(err.Error(), "stalled") {
		t.Errorf("%v", err)
	}
}

func TestDownloadCanceled(t *testing.T) {
	release := make(chan struct{})
	srv, c := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("1"))
		w.(http.Flusher).Flush()
		<-release
	}))
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	path := filepath.Join(t.TempDir(), "a.msi")
	err := Download(c, srv.URL, "t", 0, func(int64, int64) { cancel() })(ctx, path, 5)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("%v", err)
	}
}

func TestCopyBoundedExact(t *testing.T) {
	var out strings.Builder
	if err := copyBounded(context.Background(), &out, strings.NewReader("12345"), 5, nil); err != nil || out.String() != "12345" {
		t.Errorf("%q %v", out.String(), err)
	}
	out.Reset()
	if err := copyBounded(context.Background(), &out, strings.NewReader("123456"), 5, nil); !errors.Is(err, ErrTooLarge) || out.Len() != 0 {
		t.Errorf("%q %v", out.String(), err)
	}
	if err := copyBounded(context.Background(), io.Discard, iotestErr{}, 5, nil); err == nil || !strings.Contains(err.Error(), "read") {
		t.Errorf("%v", err)
	}
}

type iotestErr struct{}

func (iotestErr) Read([]byte) (int, error) { return 0, errors.New("read failed") }
