package main

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// testCA is a throwaway root CA, made for one run and never stored anywhere but the
// bench's own temporary directory.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte

	mu     sync.Mutex
	leaves map[string]*tls.Certificate
}

func newTestCA() (*testCA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(), Subject: pkix.Name{CommonName: "hyperbutler bench corp-proxy test CA", Organization: []string{"hyperbutler bench"}},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		BasicConstraintsValid: true, IsCA: true, MaxPathLenZero: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &testCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), leaves: map[string]*tls.Certificate{}}, nil
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	return n
}

// leaf returns a certificate for host signed by the CA, as a TLS-inspecting proxy
// makes one for every site it intercepts.
func (ca *testCA) leaf(host string) (*tls.Certificate, error) {
	ca.mu.Lock()
	defer ca.mu.Unlock()
	if c := ca.leaves[host]; c != nil {
		return c, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(), Subject: pkix.Name{CommonName: host},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return nil, err
	}
	c := &tls.Certificate{Certificate: [][]byte{der, ca.cert.Raw}, PrivateKey: key}
	ca.leaves[host] = c
	return c, nil
}

// mitmProxy is a TLS-inspecting HTTP proxy, like a corporate one: it answers CONNECT,
// terminates the client's TLS with a certificate from its own CA, and forwards each
// request to the real site over a TLS connection it verifies itself. It requires Basic
// proxy credentials when user is set. It listens on loopback only.
type mitmProxy struct {
	ca       *testCA
	user, pw string
	// upstream sends the intercepted requests (its TLS config verifies the sites).
	upstream http.RoundTripper

	mu         sync.Mutex
	connects   []string // CONNECT targets, in order
	authFailed int
	tlsOK      int // client handshakes that accepted our certificate
	tlsFailed  int // client handshakes that refused it (unknown authority)
	requests   int
	ln         net.Listener
	wg         sync.WaitGroup
	conns      map[net.Conn]bool
	closed     bool
}

func (p *mitmProxy) start(addr string) error {
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		return fmt.Errorf("the proxy listens on 127.0.0.1 only, not %s", addr)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	p.ln, p.conns = ln, map[net.Conn]bool{}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			if !p.track(c, true) {
				_ = c.Close()
				return
			}
			p.wg.Add(1)
			go func() {
				defer p.wg.Done()
				defer p.track(c, false)
				defer func() { _ = c.Close() }()
				p.serve(c)
			}()
		}
	}()
	return nil
}

func (p *mitmProxy) track(c net.Conn, add bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if add {
		if p.closed {
			return false
		}
		p.conns[c] = true
	} else {
		delete(p.conns, c)
	}
	return true
}

func (p *mitmProxy) addr() string { return p.ln.Addr().String() }

func (p *mitmProxy) close() {
	_ = p.ln.Close()
	p.mu.Lock()
	p.closed = true
	for c := range p.conns {
		_ = c.Close()
	}
	p.mu.Unlock()
	p.wg.Wait()
}

type stats struct {
	Connects              []string
	AuthFailed            int
	TLSOK, TLSFailed, Req int
}

func (p *mitmProxy) stats() stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return stats{Connects: append([]string(nil), p.connects...), AuthFailed: p.authFailed, TLSOK: p.tlsOK, TLSFailed: p.tlsFailed, Req: p.requests}
}

func (p *mitmProxy) count(f func()) { p.mu.Lock(); f(); p.mu.Unlock() }

func (p *mitmProxy) authorized(r *http.Request) bool {
	if p.user == "" {
		return true
	}
	h := r.Header.Get("Proxy-Authorization")
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(p.user+":"+p.pw))
	return subtle.ConstantTimeCompare([]byte(h), []byte(want)) == 1
}

func (p *mitmProxy) serve(c net.Conn) {
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	if !p.authorized(req) {
		p.count(func() { p.authFailed++ })
		_, _ = io.WriteString(c, "HTTP/1.1 407 Proxy Authentication Required\r\nProxy-Authenticate: Basic realm=\"bench\"\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		return
	}
	if req.Method != http.MethodConnect {
		// Plain http:// through the proxy: forward it as it is.
		p.count(func() { p.requests++ })
		req.RequestURI = ""
		req.Header.Del("Proxy-Authorization")
		resp, err := p.upstream.RoundTrip(req)
		if err != nil {
			_, _ = io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
			return
		}
		defer func() { _ = resp.Body.Close() }()
		_ = resp.Write(c)
		return
	}
	host := req.Host
	p.count(func() { p.connects = append(p.connects, host) })
	if _, err := io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	name, _, err := net.SplitHostPort(host)
	if err != nil {
		return
	}
	cert, err := p.ca.leaf(name)
	if err != nil {
		return
	}
	tc := tls.Server(&bufConn{Conn: c, r: br}, &tls.Config{Certificates: []tls.Certificate{*cert}, NextProtos: []string{"http/1.1"}, MinVersion: tls.VersionTLS12})
	if err := tc.Handshake(); err != nil {
		p.count(func() { p.tlsFailed++ })
		return
	}
	p.count(func() { p.tlsOK++ })
	_ = c.SetDeadline(time.Time{})
	tr := bufio.NewReader(tc)
	for {
		_ = tc.SetReadDeadline(time.Now().Add(60 * time.Second))
		r, err := http.ReadRequest(tr)
		if err != nil {
			return
		}
		_ = tc.SetReadDeadline(time.Time{})
		p.count(func() { p.requests++ })
		r.URL.Scheme, r.URL.Host, r.RequestURI = "https", host, ""
		resp, err := p.upstream.RoundTrip(r)
		if err != nil {
			_, _ = io.WriteString(tc, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
			return
		}
		err = resp.Write(tc)
		_ = resp.Body.Close()
		if err != nil || r.Close || resp.Close {
			return
		}
	}
}

// bufConn reads what the CONNECT reader already buffered first.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufConn) Read(p []byte) (int, error) { return b.r.Read(p) }
