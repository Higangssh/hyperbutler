package updatecheck

import (
	"context"
	"crypto/x509"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// releaseServer is a stand-in for GitHub's release API: it answers with status and
// body, records each request's headers, and sets a cookie that must never come back.
type releaseServer struct {
	*httptest.Server
	mu      sync.Mutex
	status  int
	body    string
	headers []http.Header
	hosts   []string
}

func newReleaseServer(t *testing.T) *releaseServer {
	t.Helper()
	rs := &releaseServer{status: http.StatusOK, body: `{"tag_name":"v0.3.0","html_url":"https://github.com/Higangssh/hyperbutler/releases/tag/v0.3.0","draft":false,"prerelease":false}`}
	rs.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rs.mu.Lock()
		rs.headers = append(rs.headers, r.Header.Clone())
		rs.hosts = append(rs.hosts, r.Host)
		status, body := rs.status, rs.body
		rs.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "tracker", Value: "1"})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(rs.Close)
	return rs
}

func (rs *releaseServer) requests() []http.Header {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return append([]http.Header(nil), rs.headers...)
}

// checker is a Checker for rs through the client the check really uses (NewClient),
// trusting only rs's certificate.
func (rs *releaseServer) checker(version string, proxy func(*http.Request) (*url.URL, error)) *Checker {
	pool := x509.NewCertPool()
	pool.AddCert(rs.Certificate())
	return &Checker{URL: rs.URL + "/repos/Higangssh/hyperbutler/releases/latest", Version: version, Client: NewClient(proxy, pool)}
}

// TestRequestHeaders: the request carries the User-Agent and nothing else: no
// Accept-Encoding, no cookie (the server's is never sent back), no identifier.
func TestRequestHeaders(t *testing.T) {
	rs := newReleaseServer(t)
	c := rs.checker("v0.2.0", nil)
	t0 := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	r, changed, err := c.Run(context.Background(), Record{}, t0)
	if err != nil || !changed || r.Latest != "v0.3.0" || r.URL != "https://github.com/Higangssh/hyperbutler/releases/tag/v0.3.0" || !r.Checked.Equal(t0) {
		t.Fatalf("run: %+v %v %v", r, changed, err)
	}
	if _, _, err := c.Run(context.Background(), r, t0.Add(Interval)); err != nil {
		t.Fatal(err)
	}
	reqs := rs.requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests, want 2", len(reqs))
	}
	for i, h := range reqs {
		var names []string
		for k := range h {
			names = append(names, k)
		}
		sort.Strings(names)
		if strings.Join(names, ",") != "User-Agent" || h.Get("User-Agent") != "hyperbutler/v0.2.0" {
			t.Errorf("request %d headers: %v", i, h)
		}
	}
}

// TestOnceADay: a check is made only when a day has passed since the last one, which
// counts failed checks too; a clock set back makes it due.
func TestOnceADay(t *testing.T) {
	rs := newReleaseServer(t)
	c := rs.checker("v0.2.0", nil)
	t0 := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	r, _, _ := c.Run(context.Background(), Record{}, t0)
	for _, d := range []time.Duration{time.Minute, time.Hour, Interval - time.Second} {
		if _, changed, _ := c.Run(context.Background(), r, t0.Add(d)); changed {
			t.Errorf("checked again after %v", d)
		}
	}
	if n := len(rs.requests()); n != 1 {
		t.Fatalf("%d requests within a day", n)
	}
	// A failed check keeps the last answer and is not retried within the day.
	rs.mu.Lock()
	rs.status = http.StatusInternalServerError
	rs.mu.Unlock()
	r2, changed, err := c.Run(context.Background(), r, t0.Add(Interval))
	if err == nil || !changed || r2.Latest != "v0.3.0" || !r2.Checked.Equal(t0.Add(Interval)) {
		t.Fatalf("failed check: %+v %v %v", r2, changed, err)
	}
	if _, changed, _ := c.Run(context.Background(), r2, t0.Add(Interval+time.Hour)); changed {
		t.Error("a failed check was retried within the day")
	}
	if !r2.Due(t0) {
		t.Error("a record from the future is not due")
	}
	if n := len(rs.requests()); n != 2 {
		t.Fatalf("%d requests", n)
	}
}

// TestNoRelease: GitHub's 404 (no release, or a repository it does not show) is
// ErrNoRelease and clears what an earlier check found; no notice is printed.
func TestNoRelease(t *testing.T) {
	rs := newReleaseServer(t)
	rs.status, rs.body = http.StatusNotFound, `{"message":"Not Found"}`
	c := rs.checker("v0.2.0", nil)
	r, changed, err := c.Run(context.Background(), Record{Latest: "v0.3.0", URL: releasePagePrefix + "v0.3.0"}, time.Now())
	if !errors.Is(err, ErrNoRelease) || !changed || r.Latest != "" || r.URL != "" || r.Checked.IsZero() {
		t.Fatalf("%+v %v %v", r, changed, err)
	}
	if n := Notice(r, "v0.2.0"); n != "" {
		t.Fatalf("notice %q", n)
	}
}

// TestProxyUsed: the request goes through the proxy function it was given.
func TestProxyUsed(t *testing.T) {
	rs := newReleaseServer(t)
	var asked []string
	c := rs.checker("v0.2.0", func(r *http.Request) (*url.URL, error) {
		asked = append(asked, r.URL.Host)
		return nil, nil // direct, after all
	})
	if _, _, err := c.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 {
		t.Fatalf("proxy asked %v", asked)
	}
}

// TestUntrustedCertificate: a server the roots do not vouch for is a failure.
func TestUntrustedCertificate(t *testing.T) {
	rs := newReleaseServer(t)
	c := &Checker{URL: rs.URL, Version: "v0.2.0", Client: NewClient(nil, x509.NewCertPool())}
	if _, _, err := c.Check(context.Background()); err == nil {
		t.Fatal("an untrusted certificate was accepted")
	}
	if len(rs.requests()) != 0 {
		t.Fatal("the request was sent")
	}
}

// TestTimeout: a server that does not answer is given up after Timeout.
func TestTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	c := &Checker{URL: srv.URL, Version: "v0.2.0", Client: NewClient(nil, pool)}
	t0 := time.Now()
	if _, _, err := c.Check(context.Background()); err == nil {
		t.Fatal("no error")
	}
	if d := time.Since(t0); d > Timeout+2*time.Second {
		t.Fatalf("took %v", d)
	}
}

func TestParseRelease(t *testing.T) {
	for _, tc := range []struct {
		body, latest, page string
		err                bool
	}{
		{`{"tag_name":"v0.3.0","html_url":"https://github.com/Higangssh/hyperbutler/releases/tag/v0.3.0"}`, "v0.3.0", releasePagePrefix + "v0.3.0", false},
		// A page elsewhere is replaced by the tag's own.
		{`{"tag_name":"v0.3.0","html_url":"https://evil.example/x"}`, "v0.3.0", releasePagePrefix + "v0.3.0", false},
		{`{"tag_name":"v0.3.0"}`, "v0.3.0", releasePagePrefix + "v0.3.0", false},
		{`{"tag_name":"nightly"}`, "", "", true},
		{`{"tag_name":"v0.3.0\u001b[2J"}`, "", "", true},
		// Build metadata reaches the page URL, so it must follow the SemVer grammar.
		{`{"tag_name":"v0.3.0-rc.1+build.7"}`, "v0.3.0-rc.1+build.7", releasePagePrefix + "v0.3.0-rc.1+build.7", false},
		{`{"tag_name":"0.0.0+\u007f"}`, "", "", true},
		{`{"tag_name":"v0.3.0+"}`, "", "", true},
		{`{"tag_name":"v0.3.0+a..b"}`, "", "", true},
		{`{"tag_name":"v0.3.0+a/b"}`, "", "", true},
		{`{"tag_name":"v0.3.0+a?b"}`, "", "", true},
		{`{"tag_name":"v0.3.0","prerelease":true}`, "", "", true},
		{`not json`, "", "", true},
	} {
		l, p, err := ParseRelease([]byte(tc.body))
		if l != tc.latest || p != tc.page || (err != nil) != tc.err {
			t.Errorf("%s: %q %q %v", tc.body, l, p, err)
		}
	}
}

func TestNotice(t *testing.T) {
	r := Record{Checked: time.Now(), Latest: "v0.3.0", URL: releasePagePrefix + "v0.3.0"}
	if n := Notice(r, "v0.2.0"); n != "hyperbutler v0.3.0 is available (this is v0.2.0): https://github.com/Higangssh/hyperbutler/releases/tag/v0.3.0" {
		t.Errorf("notice %q", n)
	}
	for _, v := range []string{"v0.3.0", "v0.4.0", "(devel)", "", "v0.2.1-0.20261101120000-0123456789ab", "v0.2.0+dirty", "garbage"} {
		if n := Notice(r, v); n != "" {
			t.Errorf("%q: notice %q", v, n)
		}
	}
}

func TestNewer(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"v0.3.0", "v0.2.0", true},
		{"v0.2.10", "v0.2.9", true},
		{"v1.0.0", "v0.99.99", true},
		{"v0.2.0", "v0.2.0-rc.1", true},
		{"v0.2.0-rc.2", "v0.2.0-rc.1", true},
		{"v0.2.0-rc.10", "v0.2.0-rc.9", true},
		{"v0.2.0-rc.1", "v0.2.0-beta", true},
		{"v0.2.0-rc.1.1", "v0.2.0-rc.1", true},
		{"0.3.0", "v0.2.0", true},
		{"v0.2.0", "v0.2.0", false},
		{"v0.2.0+b", "v0.2.0", false},
		{"v0.3.0+dirty", "v0.3.0", false},
		{"v0.2.0-rc.1", "v0.2.0", false},
		{"v01.2.0", "v0.2.0", false},
		{"v0.2", "v0.1.0", false},
	} {
		if got := Newer(tc.a, tc.b); got != tc.want {
			t.Errorf("Newer(%s, %s) = %v", tc.a, tc.b, got)
		}
	}
}

func TestRecordRoundTrip(t *testing.T) {
	r := Record{Checked: time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC), Latest: "v0.3.0", URL: releasePagePrefix + "v0.3.0"}
	if got := ParseRecord(r.Encode()); got != r {
		t.Fatalf("%+v", got)
	}
	if got := ParseRecord([]byte(`{"checked":"2026-11-01T09:00:00Z","latest":"v9","url":"https://evil.example"}`)); got.Latest != "" || got.URL != "" || got.Checked.IsZero() {
		t.Fatalf("bad record kept: %+v", got)
	}
	if got := ParseRecord([]byte("{")); !got.Checked.IsZero() {
		t.Fatal("broken record")
	}
}

func TestUserAgent(t *testing.T) {
	for v, want := range map[string]string{"v0.2.0": "hyperbutler/v0.2.0", "(devel)": "hyperbutler/devel", "": "hyperbutler/unknown", "v1\r\nX: y": "hyperbutler/v1Xy"} {
		if got := UserAgent(v); got != want {
			t.Errorf("%q: %q", v, got)
		}
	}
}

func TestIsDevBuild(t *testing.T) {
	for _, tc := range []struct {
		version string
		want    bool
	}{
		{"v0.2.0", false},
		{"v0.2.0+build.7", false},
		{"v0.2.0+dirty", true},
		{"v0.2.1-0.20261110120000-0123456789ab", true},
		{"v0.2.1-0.20261110120000-0123456789ab+dirty", true},
		{"(devel)", true},
		{"", true},
		{"v0.2.0+a/b", true},
	} {
		if got := IsDevBuild(tc.version); got != tc.want {
			t.Errorf("IsDevBuild(%q) = %v", tc.version, got)
		}
	}
}
