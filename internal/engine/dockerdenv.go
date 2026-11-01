package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/netip"
	"net/url"
	"os"
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
		if len(s) > dockerenv.MaxProxyURL+dockerenv.MaxDocNoProxy || !utf8.ValidString(s) {
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

// mergeNoProxy joins lists, dropping repeats (case-insensitively), in order. An entry
// that would make the list longer than limit is left out and counted in dropped.
func mergeNoProxy(limit int, lists ...[]string) (list string, dropped int) {
	seen := map[string]bool{}
	var out []string
	n := 0
	for _, l := range lists {
		for _, e := range l {
			e = strings.TrimSpace(e)
			k := strings.ToLower(e)
			if e == "" || seen[k] {
				continue
			}
			add := len(e)
			if len(out) > 0 {
				add++ // the comma
			}
			if n+add > limit {
				dropped++
				continue
			}
			seen[k] = true
			out = append(out, e)
			n += add
		}
	}
	return strings.Join(out, ","), dropped
}

// CheckProxyReach reports a proxy on this host's loopback (such as a local NTLM relay)
// that the guest cannot reach: such a proxy is reached through the host gateway, so the
// gateway must be on and forward its port, and the proxy must be at localhost or
// 127.0.0.1, which is where the gateway connects (dockerenv.LoopbackHost).
func (s *Settings) CheckProxyReach() error {
	hg := s.HostGateway()
	for _, p := range []struct{ name, v string }{{"proxy.http", s.HTTPProxy}, {"proxy.https", s.HTTPSProxy}} {
		u, err := dockerenv.ParseProxyURL(p.v)
		if err != nil {
			continue
		}
		loopback, via := dockerenv.LoopbackHost(u.Hostname())
		if !loopback {
			continue
		}
		if !via {
			return fmt.Errorf("%s is on this host's loopback at %s, which the guest cannot reach: the host gateway (host.docker.internal) connects to 127.0.0.1; bind the relay to 127.0.0.1 and use http://127.0.0.1:PORT (or localhost)", p.name, u.Hostname())
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
// CA certificates of the Windows root stores (with ca.import) and of ca.extraFile, read
// from caCopy, the engine's copy of it (Layout.CAExtraPath), never from the user's
// path. Certificates in the Windows Untrusted Certificates stores are left out
// whatever their source. A store or file that cannot be read is reported in the
// state's Error and left out; the rest is still used, so a broken file never keeps the
// engine from starting.
func BuildDockerdEnv(s *Settings, n EnvNet, daemonJSON []byte, caCopy string) (*dockerenv.Doc, EnvState) {
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
		// The defaults and the user's list always fit (MaxDocNoProxy leaves room for
		// the defaults over MaxNoProxy); registries that do not are left out.
		var dropped int
		doc.NoProxy, dropped = mergeNoProxy(dockerenv.MaxDocNoProxy, DefaultNoProxy(n), user, registryHosts(daemonJSON))
		if dropped > 0 {
			errs = append(errs, fmt.Sprintf("%d NO_PROXY entries (daemon.json registries) left out: the list would be over %d bytes", dropped, dockerenv.MaxDocNoProxy))
		}
		if err := s.CheckProxyReach(); err != nil {
			errs = append(errs, err.Error())
		}
	}
	st.HTTPProxy, st.HTTPSProxy, st.NoProxy = dockerenv.Redact(doc.HTTPProxy), dockerenv.Redact(doc.HTTPSProxy), doc.NoProxy

	var set dockerenv.CertSet
	// The Untrusted Certificates stores first, so they apply to ca.extraFile too.
	if err := loadDisallowed(&set); err != nil {
		errs = append(errs, "Windows Untrusted Certificates store: "+err.Error())
	}
	// The user's file next: within the size limit it wins over the store.
	if s.CAExtraFile != "" {
		certs, err := readCAExtra(s, caCopy)
		if err != nil {
			errs = append(errs, err.Error())
		}
		for _, der := range certs {
			set.Add(der, false)
		}
	}
	if s.CAImported() {
		if err := importRoots(&set); err != nil {
			errs = append(errs, "Windows certificate store: "+err.Error())
		}
	}
	if set.Stats.OverLimit > 0 {
		errs = append(errs, fmt.Sprintf("%d CA certificates left out: over the limit of %d certificates or %d KiB", set.Stats.OverLimit, dockerenv.MaxCerts, dockerenv.MaxCAPEM>>10))
	}
	doc.CACerts = set.PEM()
	st.CACerts, st.CASkipped, st.CAStats = set.Len(), set.Stats.Skipped(), set.Stats
	doc, err := settleDoc(doc, &st)
	if err != nil {
		errs = append(errs, err.Error())
	}
	st.Error = strings.Join(errs, "; ")
	return doc, st
}

// The certificate stores BuildDockerdEnv reads; tests replace them.
var (
	loadDisallowed = wincert.LoadDisallowed
	importRoots    = wincert.ImportRoots
)

// settleDoc returns doc if it is valid. It cannot be invalid, since every part was
// checked; if it is anyway, the proxy settings are dropped and the certificates kept
// (they were checked one by one, and are what a TLS-inspecting proxy elsewhere needs
// without a proxy setting), or nothing at all is sent when they are what is invalid.
// st is changed to match, and the error says what was dropped.
func settleDoc(doc *dockerenv.Doc, st *EnvState) (*dockerenv.Doc, error) {
	err := doc.Validate()
	if err == nil {
		return doc, nil
	}
	st.HTTPProxy, st.HTTPSProxy, st.NoProxy = "", "", ""
	certs := &dockerenv.Doc{CACerts: doc.CACerts}
	if certs.Validate() == nil {
		return certs, fmt.Errorf("the proxy settings are not used: %w", err)
	}
	st.CACerts, st.CASkipped, st.CAStats = 0, 0, dockerenv.Stats{}
	return &dockerenv.Doc{}, fmt.Errorf("neither the proxy settings nor the CA certificates are used: %w", err)
}

// ReadCAFile reads a ca.extraFile PEM file: CERTIFICATE blocks only, at most
// dockerenv.MaxExtraFile bytes. It returns the file's bytes and the certificates' DER.
func ReadCAFile(path string) (data []byte, certs [][]byte, err error) {
	b, err := readFileLimit(path, dockerenv.MaxExtraFile)
	if err != nil {
		return nil, nil, fmt.Errorf("ca.extraFile: %w", err)
	}
	certs, err = dockerenv.ParsePEMFile(b)
	if err != nil {
		return nil, nil, fmt.Errorf("ca.extraFile %s: %w", path, err)
	}
	return b, certs, nil
}

// readCAExtra reads the engine's copy of ca.extraFile, at copyPath. A copy whose hash
// is not the one settings.json records (settings.json edited by hand to name another
// file) is still used, with an error that says so: the copy is what `config set`
// checked, and the file the settings name now is never read here.
func readCAExtra(s *Settings, copyPath string) ([][]byte, error) {
	b, certs, err := ReadCAFile(copyPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("ca.extraFile %s: hyperbutler has no copy of it (%s); run `hyperbutler config set ca.extraFile %s` again", s.CAExtraFile, copyPath, s.CAExtraFile)
	}
	if err != nil {
		return nil, err
	}
	if digest(b) != s.CAExtraSHA256 {
		return certs, fmt.Errorf("ca.extraFile %s: the copy in %s is not the one `config set` made of it (settings.json changed by hand?); the copy is used; run `hyperbutler config set ca.extraFile %s` to copy the file again", s.CAExtraFile, copyPath, s.CAExtraFile)
	}
	return certs, nil
}

// SaveCAExtra makes data the engine's copy of ca.extraFile (Layout.CAExtraPath),
// atomically, with a DACL for the current user only; nil data removes the copy.
func SaveCAExtra(l Layout, data []byte) error {
	p := l.CAExtraPath()
	if data == nil {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	return writeFileAtomic(p, data, true)
}
