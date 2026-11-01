//go:build windows

package wincert

import (
	"crypto/x509"
	"testing"
	"time"

	"github.com/Higangssh/hyperbutler/internal/dockerenv"
)

// TestImportRoots reads this machine's stores (read-only). Every Windows installation
// has trusted roots, so the set must not be empty, and every certificate in it must be
// a CA certificate that is valid now.
func TestImportRoots(t *testing.T) {
	t0 := time.Now()
	var set dockerenv.CertSet
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
	// The same stores give the same bytes.
	var again dockerenv.CertSet
	if err := ImportRoots(&again); err != nil || again.PEM() != set.PEM() {
		t.Fatal("a second import differs")
	}
}
