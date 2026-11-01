package dockerenv

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// testCert makes a self-signed certificate valid from nb to na.
func testCert(t testing.TB, name string, ca bool, nb, na time.Time) []byte {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name},
		NotBefore: nb, NotAfter: na, BasicConstraintsValid: true, IsCA: ca,
	}
	if ca {
		tmpl.KeyUsage = x509.KeyUsageCertSign
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func validCA(t testing.TB, name string) []byte {
	return testCert(t, name, true, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour))
}

func TestParseProxyURL(t *testing.T) {
	for _, v := range []string{
		"http://proxy:3128", "http://proxy.corp.example.com:8080", "https://10.0.0.1",
		"http://user:pa%40ss@proxy:3128", "socks5://127.0.0.1:1080", "http://[fd00::1]:3128",
		"http://proxy:3128/", "http://u@proxy",
	} {
		if err := ValidateProxyURL(v); err != nil {
			t.Errorf("%q: %v", v, err)
		}
	}
	for _, v := range []string{
		"", "proxy:3128", "ftp://proxy", "http://", "http://proxy:0", "http://proxy:65536",
		"http://proxy:3128/path", "http://proxy?x=1", "http://proxy#f", "http://pro xy",
		"http://proxy:", "http://pr\x00oxy", "http://proxy:080", "http://a..b", "http://[fe80::1%25eth0]:1",
		"socks4://proxy", "http://:pw@proxy", "http://proxy\n", "http://ünï", strings.Repeat("a", 3000),
		"http://user:secret@pro%zzxy",
	} {
		err := ValidateProxyURL(v)
		if err == nil {
			t.Errorf("%q accepted", v)
			continue
		}
		if strings.Contains(err.Error(), "secret") {
			t.Errorf("%q: the error leaks the password: %v", v, err)
		}
	}
}

func TestRedact(t *testing.T) {
	for in, want := range map[string]string{
		"":                               "",
		"http://proxy:3128":              "http://proxy:3128",
		"http://user:secret@proxy:3128":  "http://user:***@proxy:3128",
		"http://do%5Cuser:s%3Ap@proxy:1": "http://do%5Cuser:***@proxy:1",
		"http://tokenvalue@proxy":        "http://***@proxy",
		"http://user:@proxy":             "http://user:***@proxy",
		"not a url secret":               "(invalid proxy URL, not shown)",
	} {
		if got := Redact(in); got != want {
			t.Errorf("Redact(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNoProxy(t *testing.T) {
	for _, v := range []string{"localhost", "*", ".corp.example.com, example.org", "10.0.0.0/8,192.168.1.1", "registry:5000", "[::1]:443", "*.corp"} {
		if err := ValidateNoProxy(v); err != nil {
			t.Errorf("%q: %v", v, err)
		}
	}
	for _, v := range []string{"", " ", "a,,b", "a b", "host:99999", "a/b", strings.Repeat("a,", 3000), "x\ny"} {
		if ValidateNoProxy(v) == nil {
			t.Errorf("%q accepted", v)
		}
	}
	if got := NormalizeNoProxy(" a , b,c "); got != "a,b,c" {
		t.Errorf("normalized to %q", got)
	}
}

func TestRewriteLoopback(t *testing.T) {
	gw := netip.MustParseAddr("192.168.127.254")
	for in, want := range map[string]string{
		"http://127.0.0.1:3128":      "http://192.168.127.254:3128",
		"http://u:p@localhost:3128":  "http://u:p@192.168.127.254:3128",
		"http://[::1]:8080":          "http://192.168.127.254:8080",
		"http://127.1.2.3":           "http://192.168.127.254",
		"http://proxy.corp:3128":     "http://proxy.corp:3128",
		"http://localhost.corp:3128": "http://localhost.corp:3128",
		"http://10.0.0.1:3128":       "http://10.0.0.1:3128",
	} {
		got, _ := RewriteLoopback(in, gw)
		if got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
		if ValidateProxyURL(got) != nil {
			t.Errorf("%q: rewrite %q is not valid", in, got)
		}
	}
}

func TestCertSet(t *testing.T) {
	ca1, ca2 := validCA(t, "one"), validCA(t, "two")
	leaf := testCert(t, "leaf", false, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	expired := testCert(t, "old", true, time.Now().Add(-48*time.Hour), time.Now().Add(-24*time.Hour))
	future := testCert(t, "new", true, time.Now().Add(24*time.Hour), time.Now().Add(48*time.Hour))
	bad := validCA(t, "bad")
	s := CertSet{Disallowed: map[[32]byte]bool{sha256.Sum256(bad): true}}
	for _, d := range [][]byte{ca1, ca2, ca1, leaf, expired, future, bad, []byte("junk"), nil} {
		s.Add(d, true)
	}
	s.Add(leaf, false) // ca.extraFile: any certificate
	want := Stats{Added: 3, Duplicate: 1, NotCA: 1, Expired: 1, NotYetValid: 1, Invalid: 2, Disallowed: 1}
	if s.Stats != want {
		t.Fatalf("stats %+v, want %+v", s.Stats, want)
	}
	p := s.PEM()
	// Stable: the same set in another order gives the same bytes.
	var r CertSet
	for _, d := range [][]byte{leaf, ca2, ca1} {
		r.Add(d, false)
	}
	if r.PEM() != p {
		t.Fatal("PEM depends on the insertion order")
	}
	d := Doc{CACerts: p}
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(d.Certs()) != 3 {
		t.Fatalf("%d certs", len(d.Certs()))
	}
	if got := len(p); got != s.size {
		t.Fatalf("pem is %d bytes, accounted %d", got, s.size)
	}
}

func TestCertSetLimit(t *testing.T) {
	ca := validCA(t, "x")
	s := CertSet{}
	s.size = MaxCAPEM - pemSize(ca) + 1
	s.Add(ca, true)
	if s.Stats.OverLimit != 1 || s.Len() != 0 {
		t.Fatalf("%+v", s.Stats)
	}
	s.Add(make([]byte, MaxCertDER+1), true)
	if s.Stats.Invalid != 1 {
		t.Fatalf("%+v", s.Stats)
	}
}

func TestDocRoundTrip(t *testing.T) {
	var s CertSet
	s.Add(validCA(t, "corp"), true)
	d := Doc{HTTPProxy: "http://u:p@proxy:3128", HTTPSProxy: "http://proxy:3128", NoProxy: "localhost,.corp", CACerts: s.PEM()}
	b, err := d.Encode()
	if err != nil {
		t.Fatal(err)
	}
	back, err := Decode(b)
	if err != nil || *back != d {
		t.Fatalf("%v %+v", err, back)
	}
	if b, err := (&Doc{}).Encode(); err != nil || b != nil {
		t.Fatalf("zero doc: %q %v", b, err)
	}
	if d, err := Decode(nil); err != nil || !d.IsZero() {
		t.Fatal("decode nil")
	}
	for _, bad := range []string{
		`{}`, `{"httpProxy":"ftp://x"}`, `{"x":1}`, `{"noProxy":" a"}`, `{"httpProxy":"http://p:1"} `,
		`{"caCerts":"-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"}`,
		`{"httpsProxy":"http://p:1","httpProxy":"http://p:1"}`, `[]`, `{"httpProxy":"http://p:1"}{}`,
	} {
		if _, err := Decode([]byte(bad)); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	env := d.Environ("/run/hyperbutler/ca-certificates.crt")
	if len(env) != 4 || env[0] != "HTTP_PROXY=http://u:p@proxy:3128" || env[3] != "SSL_CERT_FILE=/run/hyperbutler/ca-certificates.crt" {
		t.Fatalf("%q", env)
	}
	if env := (&Doc{}).Environ(""); len(env) != 0 {
		t.Fatalf("%q", env)
	}
}

func TestBundle(t *testing.T) {
	stockCA, corp := validCA(t, "stock"), validCA(t, "corp")
	stock := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: stockCA})
	stock = append([]byte("# stock bundle\n"), stock...)
	if b, n := Bundle(stock, [][]byte{stockCA}); b != nil || n != 0 {
		t.Fatal("a certificate already in the stock bundle was added")
	}
	b, n := Bundle(stock, [][]byte{corp, stockCA, corp})
	if n != 1 || !strings.HasPrefix(string(b), string(stock)) {
		t.Fatalf("added %d", n)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(b) {
		t.Fatal("bundle does not parse")
	}
	certs, err := ParsePEMFile(b)
	if err != nil || len(certs) != 2 {
		t.Fatalf("%d certs, %v", len(certs), err)
	}
}

func TestParsePEMFile(t *testing.T) {
	ca := validCA(t, "x")
	good := "comment\n" + string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca}))
	if c, err := ParsePEMFile([]byte(good)); err != nil || len(c) != 1 {
		t.Fatal(err)
	}
	for _, bad := range []string{
		"", "no pem here",
		string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte{1}})),
		string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte{1, 2}})),
	} {
		if _, err := ParsePEMFile([]byte(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
