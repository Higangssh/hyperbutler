// Package probe measures what `docker pull` needs from the network: DNS for the
// registry, a TLS connection to registry-1.docker.io, an anonymous token, and the
// download of one image layer. It runs in the guest and, for a baseline, on the host.
//
// Results are printed as "WBNET key=value ..." lines that the host parses from the
// serial console (see ParseResult).
package probe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"time"
)

// Dialer opens TCP connections. A nil Dialer uses net.Dialer. In relay mode it is a
// CONNECT tunnel over hvsock, and name resolution happens on the host.
type Dialer func(ctx context.Context, network, addr string) (net.Conn, error)

// Config controls a probe run.
type Config struct {
	Registry string // default registry-1.docker.io
	Auth     string // default auth.docker.io
	Image    string // repository, default library/ubuntu
	Tag      string // default 24.04
	Dial     Dialer
	Roots    *x509.CertPool // nil = system roots
	Printf   func(format string, args ...any)
	// ResolveLocally makes the probe time a DNS lookup itself. Off in relay mode,
	// where the guest has no resolver.
	ResolveLocally bool
	// OnTLS, if set, sees every verified TLS connection (the host baseline uses it to
	// collect the root certificates the guest will need).
	OnTLS func(tls.ConnectionState)
}

func (c *Config) tlsConfig(serverName string) *tls.Config {
	return &tls.Config{
		ServerName: serverName,
		RootCAs:    c.Roots,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if c.OnTLS != nil {
				c.OnTLS(cs)
			}
			return nil
		},
	}
}

func (c *Config) defaults() {
	if c.Registry == "" {
		c.Registry = "registry-1.docker.io"
	}
	if c.Auth == "" {
		c.Auth = "auth.docker.io"
	}
	if c.Image == "" {
		c.Image = "library/ubuntu"
	}
	if c.Tag == "" {
		c.Tag = "24.04"
	}
	if c.Printf == nil {
		c.Printf = func(f string, a ...any) { fmt.Printf(f, a...) }
	}
}

func (c *Config) emit(kv ...any) {
	s := "WBNET"
	for i := 0; i+1 < len(kv); i += 2 {
		s += fmt.Sprintf(" %v=%v", kv[i], kv[i+1])
	}
	c.Printf("%s\n", s)
}

func msf(d time.Duration) string { return fmt.Sprintf("%.1f", float64(d.Microseconds())/1000) }

// Reach dials each "host:port" in targets once and prints a step=reach line per target.
// It is for VPN checks: an address only routed through the VPN, and a name only the
// VPN's split DNS can resolve. It never fails the run.
func Reach(ctx context.Context, c Config, targets []string) {
	c.defaults()
	dial := c.Dial
	if dial == nil {
		d := &net.Dialer{Timeout: 5 * time.Second}
		dial = d.DialContext
	}
	for i, t := range targets {
		cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
		start := time.Now()
		conn, err := dial(cctx, "tcp", t)
		cancel()
		if err != nil {
			c.emit("step", "reach", "ok", false, "target", i, "err", quote(err.Error()))
			continue
		}
		conn.Close()
		c.emit("step", "reach", "ok", true, "target", i, "ms", msf(time.Since(start)))
	}
}

// Run runs the probe and prints WBNET lines. It returns the first error.
func Run(ctx context.Context, c Config) error {
	c.defaults()
	if c.ResolveLocally {
		t := time.Now()
		addrs, err := net.DefaultResolver.LookupHost(ctx, c.Registry)
		if err != nil {
			c.emit("step", "dns", "ok", false, "err", quote(err.Error()))
			return err
		}
		c.emit("step", "dns", "ok", true, "ms", msf(time.Since(t)), "addrs", len(addrs))
	}

	dial := c.Dial
	if dial == nil {
		d := &net.Dialer{Timeout: 10 * time.Second}
		dial = d.DialContext
	}

	// TCP connect + TLS handshake, timed separately.
	t := time.Now()
	raw, err := dial(ctx, "tcp", net.JoinHostPort(c.Registry, "443"))
	if err != nil {
		c.emit("step", "tcp", "ok", false, "err", quote(err.Error()))
		return err
	}
	c.emit("step", "tcp", "ok", true, "ms", msf(time.Since(t)))
	t = time.Now()
	tc := tls.Client(raw, c.tlsConfig(c.Registry))
	if err := tc.HandshakeContext(ctx); err != nil {
		raw.Close()
		c.emit("step", "tls", "ok", false, "err", quote(err.Error()))
		return err
	}
	c.emit("step", "tls", "ok", true, "ms", msf(time.Since(t)), "version", fmt.Sprintf("0x%04x", tc.ConnectionState().Version))
	tc.Close()

	client := &http.Client{
		Timeout: 120 * time.Second,
		Transport: &http.Transport{
			DialContext:     dial,
			TLSClientConfig: c.tlsConfig(""),
			Proxy:           nil,
		},
	}
	defer client.CloseIdleConnections()

	// GET /v2/ must answer 401 with a WWW-Authenticate challenge.
	t = time.Now()
	resp, err := client.Get("https://" + c.Registry + "/v2/")
	if err != nil {
		c.emit("step", "v2", "ok", false, "err", quote(err.Error()))
		return err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	c.emit("step", "v2", "ok", resp.StatusCode == http.StatusUnauthorized, "status", resp.StatusCode, "ms", msf(time.Since(t)))

	// Anonymous pull token.
	t = time.Now()
	tokURL := fmt.Sprintf("https://%s/token?service=registry.docker.io&scope=%s", c.Auth,
		url.QueryEscape("repository:"+c.Image+":pull"))
	var tok struct {
		Token string `json:"token"`
	}
	if err := getJSON(client, tokURL, "", "", &tok); err != nil || tok.Token == "" {
		c.emit("step", "token", "ok", false, "err", quote(fmt.Sprint(err)))
		return fmt.Errorf("token: %v", err)
	}
	c.emit("step", "token", "ok", true, "ms", msf(time.Since(t)))

	// Manifest list -> linux/amd64 manifest -> largest layer.
	t = time.Now()
	base := fmt.Sprintf("https://%s/v2/%s", c.Registry, c.Image)
	var index struct {
		Manifests []struct {
			Digest   string `json:"digest"`
			Platform struct {
				OS           string `json:"os"`
				Architecture string `json:"architecture"`
			} `json:"platform"`
		} `json:"manifests"`
	}
	accept := "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json"
	if err := getJSON(client, base+"/manifests/"+c.Tag, tok.Token, accept, &index); err != nil {
		c.emit("step", "manifest", "ok", false, "err", quote(err.Error()))
		return err
	}
	digest := ""
	for _, m := range index.Manifests {
		if m.Platform.OS == "linux" && m.Platform.Architecture == "amd64" {
			digest = m.Digest
		}
	}
	if digest == "" {
		c.emit("step", "manifest", "ok", false, "err", quote("no linux/amd64 manifest"))
		return fmt.Errorf("no linux/amd64 manifest")
	}
	var man struct {
		Layers []struct {
			Digest string `json:"digest"`
			Size   int64  `json:"size"`
		} `json:"layers"`
	}
	accept = "application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json"
	if err := getJSON(client, base+"/manifests/"+digest, tok.Token, accept, &man); err != nil || len(man.Layers) == 0 {
		c.emit("step", "manifest", "ok", false, "err", quote(fmt.Sprint(err)))
		return fmt.Errorf("manifest: %v", err)
	}
	sort.Slice(man.Layers, func(i, j int) bool { return man.Layers[i].Size > man.Layers[j].Size })
	layer := man.Layers[0]
	c.emit("step", "manifest", "ok", true, "ms", msf(time.Since(t)), "layer_bytes", layer.Size)

	// Layer download (follows the redirect to the CDN, so a second TLS host).
	t = time.Now()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/blobs/"+layer.Digest, nil)
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	resp, err = client.Do(req)
	if err != nil {
		c.emit("step", "blob", "ok", false, "err", quote(err.Error()))
		return err
	}
	n, err := io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	el := time.Since(t)
	if err != nil || resp.StatusCode != http.StatusOK {
		c.emit("step", "blob", "ok", false, "status", resp.StatusCode, "err", quote(fmt.Sprint(err)))
		return fmt.Errorf("blob: status %d: %v", resp.StatusCode, err)
	}
	c.emit("step", "blob", "ok", n == layer.Size, "bytes", n, "ms", msf(el),
		"mbit_s", fmt.Sprintf("%.1f", float64(n)*8/el.Seconds()/1e6))
	return nil
}

func getJSON(c *http.Client, u, token, accept string, v any) error {
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d", u, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(v)
}

// quote makes a value safe for a single WBNET token: no space, tab, '=' or control byte.
func quote(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c == ' ' || c == '=' || c < 0x20 || c == 0x7f {
			b[i] = '_'
		}
	}
	if len(b) > 200 {
		b = b[:200]
	}
	return string(b)
}
