package mcpserver

import (
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strings"
)

// Which registries pull may name (docs/mcp.md, "Which registries pull reaches"). dockerd
// connects to whatever host a reference names, and its error says whether something
// answered there, so a pull of HOST:PORT/x is a port scan of the guest's network, of this
// machine (host.docker.internal) or of the LAN. By default a reference may name no
// registry host: it is a Docker Hub image, which dockerd pulls from Docker Hub or the
// registry mirrors daemon.json configures. Other registries are those --allow-registry
// lists. An IP literal, localhost and the docker.internal names are always refused.

// dockerHubHosts are the spellings of Docker Hub a reference may use.
var dockerHubHosts = []string{"docker.io", "index.docker.io", "registry-1.docker.io"}

// reRegistryHost is a registry host as --allow-registry takes it: DNS labels, and an
// optional port.
var reRegistryHost = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?)*(:[0-9]{1,5})?$`)

// refHost is the registry host a reference names, lower case, or "" for none. As
// Docker's own parser: the part before the first '/' is a host when it holds a '.' or a
// ':', is localhost, or has an upper-case letter (a repository path cannot).
func refHost(ref string) string {
	i := strings.IndexByte(ref, '/')
	if i < 0 {
		return ""
	}
	first := ref[:i]
	if strings.ContainsAny(first, ".:") || first == "localhost" || strings.ToLower(first) != first {
		return strings.ToLower(first)
	}
	return ""
}

// alwaysRefused says why a registry host is never allowed, or is "".
func alwaysRefused(host string) string {
	name := host
	if i := strings.LastIndexByte(host, ':'); i >= 0 {
		name = host[:i]
	}
	if _, err := netip.ParseAddr(strings.Trim(name, "[]")); err == nil {
		return "it is an IP address"
	}
	labels := strings.Split(name, ".")
	last := labels[len(labels)-1]
	if last == "" {
		return "it is not a plain host name"
	}
	// A name that is all digits (or 0x...) is an address in some resolvers' eyes.
	if strings.Trim(last, "0123456789") == "" || strings.HasPrefix(last, "0x") {
		return "it is a number, which some resolvers take as an IP address"
	}
	switch {
	case name == "localhost" || strings.HasSuffix(name, ".localhost"):
		return "it is the guest's own loopback"
	case name == "docker.internal" || strings.HasSuffix(name, ".docker.internal"):
		return "it names this machine or the engine's gateway (host.docker.internal, gateway.docker.internal)"
	}
	return ""
}

// checkRegistryHost checks one --allow-registry entry.
func checkRegistryHost(h string) error {
	if !reRegistryHost.MatchString(h) {
		return fmt.Errorf("--allow-registry %q: want a registry host name, such as ghcr.io or registry.corp.example:5000, in lower case", clip(h, 80))
	}
	if why := alwaysRefused(h); why != "" {
		return fmt.Errorf("--allow-registry %q is refused because %s", h, why)
	}
	return nil
}

// ParseRegistries parses --allow-registry's comma-separated list.
func ParseRegistries(s string) ([]string, error) {
	var out []string
	for h := range strings.SplitSeq(s, ",") {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" {
			continue
		}
		if err := checkRegistryHost(h); err != nil {
			return nil, err
		}
		if !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	return out, nil
}

// pullAllowed checks the registry a reference names against the policy above.
func (t *toolset) pullAllowed(ref string) error {
	h := refHost(ref)
	if h == "" || slices.Contains(dockerHubHosts, h) {
		return nil
	}
	if why := alwaysRefused(h); why != "" {
		return fmt.Errorf("pull %s: the registry %s is refused because %s", ref, h, why)
	}
	if slices.Contains(t.o.AllowRegistries, h) {
		return nil
	}
	return fmt.Errorf("pull %s: the registry %s is not allowed. This server pulls Docker Hub images (and through the registry mirrors daemon.json sets) only; "+
		"the user can allow it with `hyperbutler mcp --allow-write --allow-registry=%s`, or pull it with `docker pull` in a terminal", ref, h, h)
}
