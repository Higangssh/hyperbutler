package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Higangssh/hyperbutler/internal/dockerenv"
	"github.com/Higangssh/hyperbutler/internal/wincert"
)

// EnvState is what state.json says about dockerd's environment (REQ-UX-009): the proxy
// settings (redacted) and the CA certificates in effect in the guest.
type EnvState struct {
	// HTTPProxy and HTTPSProxy are redacted (dockerenv.Redact), and as the guest sees
	// them: a proxy on this host's loopback is reached through the host gateway.
	HTTPProxy  string `json:"httpProxy,omitempty"`
	HTTPSProxy string `json:"httpsProxy,omitempty"`
	// NoProxy is the list dockerd gets, with DefaultNoProxy added.
	NoProxy string `json:"noProxy,omitempty"`
	// CAImport says whether the Windows root stores were read; CACerts is how many
	// CA certificates were sent (Windows roots and ca.extraFile, deduplicated; the
	// guest leaves out those its stock bundle already has), CASkipped how many were
	// left out and CAStats why (not a CA, expired, untrusted, over the size limit).
	CAImport  bool            `json:"caImport"`
	CACerts   int             `json:"caCerts"`
	CASkipped int             `json:"caSkipped,omitempty"`
	CAStats   dockerenv.Stats `json:"caStats"`
	// SHA256 identifies the environment in effect; empty for none.
	SHA256 string `json:"sha256,omitempty"`
	// Restarted says the last delivery restarted dockerd.
	Restarted bool `json:"restarted,omitempty"`
	// Error is why the last delivery did not put the settings fully in effect: a
	// certificate file or store that could not be read, an unreachable proxy, or the
	// guest's refusal.
	Error string `json:"error,omitempty"`
}

// Validate reports the first field out of range.
func (e *EnvState) Validate() error {
	for _, s := range []string{e.HTTPProxy, e.HTTPSProxy, e.NoProxy} {
		if len(s) > dockerenv.MaxProxyURL+dockerenv.MaxNoProxy || !utf8.ValidString(s) {
			return errors.New("dockerd environment state: a proxy field is too long or not UTF-8")
		}
	}
	switch {
	case e.CACerts < 0 || e.CACerts > dockerenv.MaxCerts || e.CASkipped < 0:
		return errors.New("dockerd environment state: certificate count out of range")
	case e.SHA256 != "" && !isHexDigest(e.SHA256):
		return fmt.Errorf("dockerd environment state: bad hash %q", e.SHA256)
	case len(e.Error) > maxErrorLen || !utf8.ValidString(e.Error):
		return errors.New("dockerd environment state: error text too long or not UTF-8")
	}
	return nil
}

// EnvNet is what BuildDockerdEnv needs to know of the guest network.
type EnvNet struct {
	// Subnet is the guest subnet; HostGW the guest's address for this host's loopback
	// (invalid: there is no host gateway, so a loopback proxy is not rewritten).
	Subnet netip.Prefix
	HostGW netip.Addr
}

// DefaultNoProxy are the NO_PROXY entries added to the user's whenever a proxy is
// set: this host's and the guest's own addresses, which a proxy elsewhere cannot reach.
func DefaultNoProxy(n EnvNet) []string {
	l := []string{"localhost", "127.0.0.1", "::1"}
	if n.Subnet.IsValid() {
		l = append(l, n.Subnet.Masked().String())
	}
	l = append(l, "host.docker.internal", "gateway.docker.internal")
	if n.HostGW.IsValid() {
		l = append(l, n.HostGW.String())
	}
	return l
}

// registryHosts are the hosts of daemon.json's insecure-registries and
// registry-mirrors: registries on the LAN, which a proxy usually cannot reach. Entries
// that are not a usable NO_PROXY entry are left out; so is everything on a parse error.
func registryHosts(daemonJSON []byte) []string {
	var m map[string]json.RawMessage
	if len(daemonJSON) == 0 || json.Unmarshal(daemonJSON, &m) != nil {
		return nil
	}
	var out []string
	for k, v := range m {
		var list []string
		if (!strings.EqualFold(k, "insecure-registries") && !strings.EqualFold(k, "registry-mirrors")) || json.Unmarshal(v, &list) != nil {
			continue
		}
		for _, e := range list {
			if u, err := url.Parse(e); err == nil && u.Host != "" {
				e = u.Hostname()
			} else if h, _, err := net.SplitHostPort(e); err == nil {
				e = h
			}
			if !strings.Contains(e, ",") && dockerenv.ValidateNoProxy(e) == nil {
				out = append(out, e)
			}
		}
	}
	sort.Strings(out)
	return out
}

// mergeNoProxy joins lists, dropping repeats (case-insensitively), in order.
func mergeNoProxy(lists ...[]string) string {
	seen := map[string]bool{}
	var out []string
	for _, l := range lists {
		for _, e := range l {
			e = strings.TrimSpace(e)
			if k := strings.ToLower(e); e != "" && !seen[k] {
				seen[k] = true
				out = append(out, e)
			}
		}
	}
	return strings.Join(out, ",")
}

// CheckProxyReach reports a proxy on this host's loopback (such as a local NTLM relay)
// that the guest cannot reach: such a proxy is reached through the host gateway, so the
// gateway must be on and forward its port.
func (s *Settings) CheckProxyReach() error {
	hg := s.HostGateway()
	for _, p := range []struct{ name, v string }{{"proxy.http", s.HTTPProxy}, {"proxy.https", s.HTTPSProxy}} {
		u, err := dockerenv.ParseProxyURL(p.v)
		if err != nil {
			continue
		}
		h := u.Hostname()
		a, aerr := netip.ParseAddr(h)
		if !strings.EqualFold(h, "localhost") && (aerr != nil || !a.IsLoopback()) {
			continue
		}
		port := map[string]uint16{"http": 80, "https": 443, "socks5": 1080}[u.Scheme]
		if ps := u.Port(); ps != "" {
			n, _ := strconv.Atoi(ps)
			port = uint16(n) //nolint:gosec // G115: ParseProxyURL checked 1..65535
		}
		if !hg.Allows(port) {
			return fmt.Errorf("%s is on this host's loopback (port %d), which the guest reaches only through the host gateway (host.docker.internal), and it does not forward that port (%s); run `hyperbutler config set hostGateway.enabled true` or add the port to hostGateway.ports", p.name, port, hg)
		}
	}
	return nil
}

// BuildDockerdEnv makes the guest's dockerd environment from s: its proxies, with a
// loopback proxy host replaced by the host gateway; NO_PROXY with DefaultNoProxy and
// the registries of daemonJSON (the user's daemon.json, nil for none) added; and the
// CA certificates of the Windows root stores (with ca.import) and of ca.extraFile. A
// store or file that cannot be read is reported in the state's Error and left out;
// the rest is still used, so a broken file never keeps the engine from starting.
func BuildDockerdEnv(s *Settings, n EnvNet, daemonJSON []byte) (*dockerenv.Doc, EnvState) {
	doc := &dockerenv.Doc{}
	st := EnvState{CAImport: s.CAImported()}
	var errs []string
	doc.HTTPProxy, _ = dockerenv.RewriteLoopback(s.HTTPProxy, n.HostGW)
	doc.HTTPSProxy, _ = dockerenv.RewriteLoopback(s.HTTPSProxy, n.HostGW)
	if doc.HTTPProxy != "" || doc.HTTPSProxy != "" {
		var user []string
		if s.NoProxy != "" {
			user = strings.Split(s.NoProxy, ",")
		}
		doc.NoProxy = mergeNoProxy(DefaultNoProxy(n), user, registryHosts(daemonJSON))
		if len(doc.NoProxy) > dockerenv.MaxNoProxy {
			doc.NoProxy = mergeNoProxy(DefaultNoProxy(n), user)
		}
		if err := s.CheckProxyReach(); err != nil {
			errs = append(errs, err.Error())
		}
	}
	st.HTTPProxy, st.HTTPSProxy, st.NoProxy = dockerenv.Redact(doc.HTTPProxy), dockerenv.Redact(doc.HTTPSProxy), doc.NoProxy

	var set dockerenv.CertSet
	// The user's file first: within the size limit it wins over the store.
	if s.CAExtraFile != "" {
		certs, err := ReadCAFile(s.CAExtraFile)
		if err != nil {
			errs = append(errs, err.Error())
		}
		for _, der := range certs {
			set.Add(der, false)
		}
	}
	if s.CAImported() {
		if err := wincert.ImportRoots(&set); err != nil {
			errs = append(errs, "Windows certificate store: "+err.Error())
		}
	}
	if set.Stats.OverLimit > 0 {
		errs = append(errs, fmt.Sprintf("%d CA certificates left out: over the limit of %d certificates or %d KiB", set.Stats.OverLimit, dockerenv.MaxCerts, dockerenv.MaxCAPEM>>10))
	}
	doc.CACerts = set.PEM()
	st.CACerts, st.CASkipped, st.CAStats = set.Len(), set.Stats.Skipped(), set.Stats
	if err := doc.Validate(); err != nil { // cannot happen: every part was checked
		errs = append(errs, err.Error())
		doc = &dockerenv.Doc{}
		st.CACerts = 0
	}
	st.Error = strings.Join(errs, "; ")
	return doc, st
}

// ReadCAFile reads the ca.extraFile PEM file: CERTIFICATE blocks only, at most
// dockerenv.MaxExtraFile bytes.
func ReadCAFile(path string) ([][]byte, error) {
	b, err := readFileLimit(path, dockerenv.MaxExtraFile)
	if err != nil {
		return nil, fmt.Errorf("ca.extraFile: %w", err)
	}
	certs, err := dockerenv.ParsePEMFile(b)
	if err != nil {
		return nil, fmt.Errorf("ca.extraFile %s: %w", path, err)
	}
	return certs, nil
}
