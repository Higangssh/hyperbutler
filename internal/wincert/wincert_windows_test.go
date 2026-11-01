//go:build windows

package wincert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"math/big"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/dockerenv"
)

// TestImportRoots reads this machine's stores (read-only). Every Windows installation
// has trusted roots, so the set must not be empty, and every certificate in it must be
// a CA certificate that is valid now.
func TestImportRoots(t *testing.T) {
	t0 := time.Now()
	var set dockerenv.CertSet
	if err := LoadDisallowed(&set); err != nil {
		t.Fatal(err)
	}
	if err := ImportRoots(&set); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d roots in %v, stats %+v", set.Len(), time.Since(t0), set.Stats)
	if set.Len() == 0 {
		t.Fatal("no trusted roots found")
	}
	d := dockerenv.Doc{CACerts: set.PEM()}
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, der := range d.Certs() {
		c, err := x509.ParseCertificate(der)
		if err != nil || c.BasicConstraintsValid && !c.IsCA || time.Now().After(c.NotAfter) {
			t.Fatalf("bad certificate %v: %v", c.Subject, err)
		}
	}
	if lm, err := LocalMachineRoots(); err != nil || len(lm) == 0 {
		t.Fatalf("LocalMachine\\Root: %d, %v", len(lm), err)
	}
	// The same stores give the same bytes.
	var again dockerenv.CertSet
	if err := ImportRoots(&again); err != nil || again.PEM() != set.PEM() {
		t.Fatal("a second import differs")
	}
}

// newCert makes a self-signed CA certificate; pad adds an extension of that many bytes.
func newCert(t *testing.T, name string, pad int) []byte {
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
	if pad > 0 {
		tmpl.ExtraExtensions = []pkix.Extension{{Id: asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 99999, 1}, Value: make([]byte, pad)}}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

// memStore is an in-memory certificate store (CERT_STORE_PROV_MEMORY) holding certs:
// it lives in this process only, so no system store is touched.
func memStore(t *testing.T, certs ...[]byte) windows.Handle {
	t.Helper()
	h, err := windows.CertOpenStore(windows.CERT_STORE_PROV_MEMORY, 0, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = windows.CertCloseStore(h, 0) })
	for _, der := range certs {
		c, err := windows.CertCreateCertificateContext(windows.X509_ASN_ENCODING|windows.PKCS_7_ASN_ENCODING, &der[0], uint32(len(der)))
		if err != nil {
			t.Fatal(err)
		}
		err = windows.CertAddCertificateContextToStore(h, c, windows.CERT_STORE_ADD_ALWAYS, nil)
		_ = windows.CertFreeCertificateContext(c)
		if err != nil {
			t.Fatal(err)
		}
	}
	return h
}

// TestEnumStoreLimit: a store with more entries than the limit is read up to it, and
// says so.
func TestEnumStoreLimit(t *testing.T) {
	h := memStore(t, newCert(t, "a", 0), newCert(t, "b", 0), newCert(t, "c", 0), newCert(t, "d", 0), newCert(t, "e", 0))
	var n int
	err := enumStore(h, "test", 3, func([]byte) { n++ })
	if err == nil || !strings.Contains(err.Error(), "more than 3") || n != 3 {
		t.Fatalf("limit 3: %d read, %v", n, err)
	}
	n = 0
	if err := enumStore(h, "test", 5, func([]byte) { n++ }); err != nil || n != 5 {
		t.Fatalf("limit 5: %d read, %v", n, err)
	}
	// An empty store.
	if err := enumStore(memStore(t), "empty", 3, func([]byte) { t.Error("called") }); err != nil {
		t.Fatal(err)
	}
}

// TestEnumStoreOversized: a certificate over dockerenv.MaxCertDER is passed truncated,
// and the CertSet counts it as invalid.
func TestEnumStoreOversized(t *testing.T) {
	big, ok := newCert(t, "big", dockerenv.MaxCertDER+100), newCert(t, "ok", 0)
	h := memStore(t, big, ok)
	var set dockerenv.CertSet
	var lens []int
	if err := enumStore(h, "test", 10, func(der []byte) { lens = append(lens, len(der)); set.Add(der, true) }); err != nil {
		t.Fatal(err)
	}
	if set.Len() != 1 || set.Stats.Invalid != 1 {
		t.Fatalf("stats %+v", set.Stats)
	}
	for _, l := range lens {
		if l > dockerenv.MaxCertDER+1 {
			t.Fatalf("passed %d bytes", l)
		}
	}
}

// TestEnumStoreErrors: a system store that does not exist is skipped, not an error
// (CERT_STORE_OPEN_EXISTING_FLAG: it is not created either); one that cannot be opened
// is an error that names it.
func TestEnumStoreErrors(t *testing.T) {
	s := store{windows.CERT_SYSTEM_STORE_CURRENT_USER, "hyperbutler-test-no-such-store", "missing"}
	if err := enum(s, func([]byte) { t.Error("called") }); err != nil {
		t.Fatal(err)
	}
	// An empty store name: CertOpenStore says the parameter is incorrect.
	bad := store{windows.CERT_SYSTEM_STORE_CURRENT_USER, "", `CurrentUser\(empty)`}
	if err := enum(bad, func([]byte) {}); err == nil || !strings.Contains(err.Error(), `open the CurrentUser\(empty) certificate store`) {
		t.Fatalf("bad store name: %v", err)
	}
}
