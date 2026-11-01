package main

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Higangssh/hyperbutler/internal/leaktest"
)

func TestMain(m *testing.M) { leaktest.VerifyTestMain(m) }

// TestMITMProxy: through the proxy, a client that trusts the test CA reaches the site,
// one that does not gets an x509 error, and wrong credentials get a 407. Everything is
// on 127.0.0.1.
func TestMITMProxy(t *testing.T) {
	site := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "hello "+r.URL.Path)
	}))
	defer site.Close()
	ca, err := newTestCA()
	if err != nil {
		t.Fatal(err)
	}
	upstream := site.Client().Transport.(*http.Transport).Clone()
	defer upstream.CloseIdleConnections()
	p := &mitmProxy{ca: ca, user: "hb", pw: "pa55", upstream: upstream}
	if err := p.start("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	defer p.close()

	client := func(user string, trust bool) *http.Client {
		pool := x509.NewCertPool()
		if trust {
			pool.AppendCertsFromPEM(ca.pem)
		}
		pu := &url.URL{Scheme: "http", Host: p.addr(), User: url.UserPassword("hb", user)}
		return &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu), TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, DisableKeepAlives: true}}
	}
	get := func(c *http.Client) (string, error) {
		resp, err := c.Get(site.URL + "/v2/")
		if err != nil {
			return "", err
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return string(b), nil
	}
	if body, err := get(client("pa55", true)); err != nil || body != "hello /v2/" {
		t.Fatalf("with the CA: %q %v", body, err)
	}
	if _, err := get(client("pa55", false)); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("without the CA: %v", err)
	}
	if _, err := get(client("wrong", true)); err == nil {
		t.Fatal("wrong credentials accepted")
	}
	st := p.stats()
	if len(st.Connects) != 2 || st.TLSOK != 1 || st.TLSFailed != 1 || st.AuthFailed != 1 || st.Req != 1 {
		t.Fatalf("stats %+v", st)
	}
	if err := (&mitmProxy{}).start("0.0.0.0:0"); err == nil {
		t.Fatal("listened on the wildcard address")
	}
}
