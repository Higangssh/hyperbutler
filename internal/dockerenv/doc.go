package dockerenv

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

// MaxDoc bounds an encoded Doc on the wire (guestctl.MaxDockerdEnv).
const MaxDoc = 1 << 20

// Doc is what the host sends the guest: dockerd's proxy environment and the extra CA
// certificates. The zero Doc means none of either; it encodes as no bytes at all.
type Doc struct {
	HTTPProxy  string `json:"httpProxy,omitempty"`
	HTTPSProxy string `json:"httpsProxy,omitempty"`
	NoProxy    string `json:"noProxy,omitempty"`
	// CACerts are PEM CERTIFICATE blocks in canonical form (CertSet.PEM).
	CACerts string `json:"caCerts,omitempty"`
}

// IsZero reports a Doc with nothing in it.
func (d *Doc) IsZero() bool { return *d == Doc{} }

// Validate checks every field. Errors never quote a proxy URL.
func (d *Doc) Validate() error {
	if d.HTTPProxy != "" {
		if err := ValidateProxyURL(d.HTTPProxy); err != nil {
			return fmt.Errorf("http proxy: %w", err)
		}
	}
	if d.HTTPSProxy != "" {
		if err := ValidateProxyURL(d.HTTPSProxy); err != nil {
			return fmt.Errorf("https proxy: %w", err)
		}
	}
	if d.NoProxy != "" {
		if err := validateNoProxy(d.NoProxy, MaxDocNoProxy); err != nil {
			return err
		}
		if NormalizeNoProxy(d.NoProxy) != d.NoProxy {
			return errors.New("no-proxy list not normalized")
		}
	}
	if len(d.CACerts) > MaxCAPEM {
		return errors.New("CA certificates: too large")
	}
	if d.CACerts != "" {
		if _, err := parseCanonicalPEM(d.CACerts); err != nil {
			return err
		}
	}
	return nil
}

// Certs returns the DER of the CA certificates (Validate has checked them).
func (d *Doc) Certs() [][]byte {
	c, _ := parseCanonicalPEM(d.CACerts)
	return c
}

// Encode validates d and returns its wire form: JSON, or nil for the zero Doc.
func (d *Doc) Encode() ([]byte, error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}
	if d.IsZero() {
		return nil, nil
	}
	b, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	if len(b) > MaxDoc {
		return nil, fmt.Errorf("dockerd environment of %d bytes is over the %d byte limit", len(b), MaxDoc)
	}
	return b, nil
}

// Decode is the inverse of Encode, and strict: one JSON object with known fields,
// valid values, and exactly the bytes Encode would give.
func Decode(b []byte) (*Doc, error) {
	var d Doc
	if len(b) == 0 {
		return &d, nil
	}
	if len(b) > MaxDoc || !utf8.Valid(b) {
		return nil, errors.New("dockerd environment: too large or not UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("dockerd environment: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("dockerd environment: trailing data")
	}
	re, err := d.Encode()
	if err != nil {
		return nil, fmt.Errorf("dockerd environment: %w", err)
	}
	if !bytes.Equal(re, b) {
		return nil, errors.New("dockerd environment: not in canonical form")
	}
	return &d, nil
}

// Environ is the environment the guest's init gives dockerd: the proxy variables that
// are set, and SSL_CERT_FILE=caBundle when caBundle is not empty. Go's crypto/x509
// (dockerd, containerd, BuildKit) reads SSL_CERT_FILE instead of the stock bundle, so
// caBundle must contain the stock certificates too (Bundle).
func (d *Doc) Environ(caBundle string) []string {
	var env []string
	for _, kv := range [][2]string{{"HTTP_PROXY", d.HTTPProxy}, {"HTTPS_PROXY", d.HTTPSProxy}, {"NO_PROXY", d.NoProxy}, {"SSL_CERT_FILE", caBundle}} {
		if kv[1] != "" {
			env = append(env, kv[0]+"="+kv[1])
		}
	}
	return env
}

// EnvNames are the variables Environ can set; the init exports no others.
var EnvNames = []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "SSL_CERT_FILE"}
