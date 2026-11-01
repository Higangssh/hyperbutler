package dockerenv

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"sort"
	"time"
)

// Bounds of the imported CA certificates.
const (
	// MaxCAPEM bounds the PEM of the certificates in a Doc. A Windows root store holds
	// tens to a few hundred roots of about 2 KiB each; certificates past the limit are
	// dropped (Stats.OverLimit) rather than failing the whole import.
	MaxCAPEM = 960 << 10
	// MaxCerts bounds their number.
	MaxCerts = 2000
	// MaxCertDER bounds one certificate. Real CA certificates are 1-3 KiB.
	MaxCertDER = 16 << 10
	// MaxExtraFile bounds the ca.extraFile PEM file.
	MaxExtraFile = 1 << 20
)

// Stats count what CertSet.Add did with the certificates it was given.
type Stats struct {
	Added     int `json:"added"`
	Duplicate int `json:"duplicate,omitempty"`
	NotCA     int `json:"notCA,omitempty"`
	Expired   int `json:"expired,omitempty"`
	// NotYetValid have a NotBefore in the future (or a wrong clock in this host).
	NotYetValid int `json:"notYetValid,omitempty"`
	Invalid     int `json:"invalid,omitempty"`
	Disallowed  int `json:"disallowed,omitempty"`
	OverLimit   int `json:"overLimit,omitempty"`
}

// Skipped is the number of certificates not added.
func (s Stats) Skipped() int {
	return s.Duplicate + s.NotCA + s.Expired + s.NotYetValid + s.Invalid + s.Disallowed + s.OverLimit
}

// CertSet collects CA certificates, deduplicated by the SHA-256 of their DER, within
// MaxCAPEM and MaxCerts.
type CertSet struct {
	// Now is the time certificates must be valid at (time.Now if zero).
	Now time.Time
	// Disallowed are certificates (by SHA-256 of the DER) that must never be added:
	// the Windows "Untrusted Certificates" store.
	Disallowed map[[32]byte]bool

	Stats Stats
	seen  map[[32]byte][]byte
	size  int
}

// pemSize is the length of der's PEM encoding.
func pemSize(der []byte) int {
	n := (len(der) + 2) / 3 * 4
	return len("-----BEGIN CERTIFICATE-----\n") + n + (n+63)/64 + len("-----END CERTIFICATE-----\n")
}

// Add adds one certificate. With requireCA it must be a CA certificate (basic
// constraints CA:TRUE, or a self-issued version 1 root without them), which is what a
// root store should hold; the
// ca.extraFile certificates, which the user chose, need only parse. Every certificate
// must be within its validity period.
func (s *CertSet) Add(der []byte, requireCA bool) {
	if s.seen == nil {
		s.seen = make(map[[32]byte][]byte)
	}
	if len(der) == 0 || len(der) > MaxCertDER {
		s.Stats.Invalid++
		return
	}
	h := sha256.Sum256(der)
	if _, dup := s.seen[h]; dup {
		s.Stats.Duplicate++
		return
	}
	if s.Disallowed[h] {
		s.Stats.Disallowed++
		return
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		s.Stats.Invalid++
		return
	}
	// A version 1 root has no basic constraints; it counts as a CA when it is
	// self-issued. Anything that says CA:FALSE does not.
	isCA := c.IsCA || !c.BasicConstraintsValid && bytes.Equal(c.RawSubject, c.RawIssuer)
	if requireCA && !isCA {
		s.Stats.NotCA++
		return
	}
	now := s.Now
	if now.IsZero() {
		now = time.Now()
	}
	if now.Before(c.NotBefore) {
		s.Stats.NotYetValid++
		return
	}
	if now.After(c.NotAfter) {
		s.Stats.Expired++
		return
	}
	n := pemSize(der)
	if len(s.seen) >= MaxCerts || s.size+n > MaxCAPEM {
		s.Stats.OverLimit++
		return
	}
	s.seen[h] = bytes.Clone(der)
	s.size += n
	s.Stats.Added++
}

// Len is the number of certificates in the set.
func (s *CertSet) Len() int { return len(s.seen) }

// PEM encodes the set, ordered by SHA-256, so the same certificates always give the
// same bytes (and an unchanged set never restarts dockerd).
func (s *CertSet) PEM() string {
	keys := make([][32]byte, 0, len(s.seen))
	for h := range s.seen {
		keys = append(keys, h)
	}
	sort.Slice(keys, func(i, j int) bool { return bytes.Compare(keys[i][:], keys[j][:]) < 0 })
	var b bytes.Buffer
	for _, h := range keys {
		_ = pem.Encode(&b, &pem.Block{Type: "CERTIFICATE", Bytes: s.seen[h]})
	}
	return b.String()
}

// ParsePEMFile returns the DER of every CERTIFICATE block in a PEM file such as a CA
// bundle. Text between blocks (comments) is ignored; any other block type, a block
// with headers, or a file without certificates is an error.
func ParsePEMFile(b []byte) ([][]byte, error) {
	if len(b) > MaxExtraFile {
		return nil, fmt.Errorf("the file is larger than %d bytes", MaxExtraFile)
	}
	var out [][]byte
	rest := b
	for {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			break
		}
		if blk.Type != "CERTIFICATE" || len(blk.Headers) != 0 {
			return nil, fmt.Errorf("a %q block: want only CERTIFICATE blocks", blk.Type)
		}
		if _, err := x509.ParseCertificate(blk.Bytes); err != nil {
			return nil, fmt.Errorf("certificate %d: %w", len(out)+1, err)
		}
		out = append(out, blk.Bytes)
	}
	if len(out) == 0 {
		return nil, errors.New("no PEM CERTIFICATE block found")
	}
	return out, nil
}

// parseCanonicalPEM returns the certificates of a Doc's CACerts, which must be exactly
// the PEM encoding of its blocks, nothing else.
func parseCanonicalPEM(s string) ([][]byte, error) {
	var out [][]byte
	rest := []byte(s)
	var re bytes.Buffer
	for len(rest) > 0 {
		blk, r := pem.Decode(rest)
		if blk == nil {
			return nil, errors.New("CA certificates: trailing data")
		}
		rest = r
		if blk.Type != "CERTIFICATE" || len(blk.Headers) != 0 || len(blk.Bytes) == 0 || len(blk.Bytes) > MaxCertDER {
			return nil, errors.New("CA certificates: not a certificate block")
		}
		if _, err := x509.ParseCertificate(blk.Bytes); err != nil {
			return nil, fmt.Errorf("CA certificate %d: %w", len(out)+1, err)
		}
		_ = pem.Encode(&re, blk)
		out = append(out, blk.Bytes)
		if len(out) > MaxCerts {
			return nil, errors.New("CA certificates: too many")
		}
	}
	if re.String() != s {
		return nil, errors.New("CA certificates: not in canonical PEM form")
	}
	return out, nil
}

// Bundle returns the guest's combined CA bundle: the stock bundle as it is, then every
// certificate of certs that is not in it (by SHA-256 of the DER). added is how many
// were appended; with none, the stock bundle is enough and bundle is nil.
func Bundle(stock []byte, certs [][]byte) (bundle []byte, added int) {
	have := make(map[[32]byte]bool)
	rest := stock
	for {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			break
		}
		if blk.Type == "CERTIFICATE" {
			have[sha256.Sum256(blk.Bytes)] = true
		}
	}
	var extra bytes.Buffer
	for _, der := range certs {
		h := sha256.Sum256(der)
		if have[h] {
			continue
		}
		have[h] = true
		_ = pem.Encode(&extra, &pem.Block{Type: "CERTIFICATE", Bytes: der})
		added++
	}
	if added == 0 {
		return nil, 0
	}
	var b bytes.Buffer
	b.Grow(len(stock) + extra.Len() + 128)
	b.Write(stock)
	if len(stock) > 0 && stock[len(stock)-1] != '\n' {
		b.WriteByte('\n')
	}
	b.WriteString("\n# Added by hyperbutler: CA certificates from the Windows certificate store (ca.import)\n# and ca.extraFile.\n")
	b.Write(extra.Bytes())
	return b.Bytes(), added
}
