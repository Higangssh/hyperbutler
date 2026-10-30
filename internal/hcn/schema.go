package hcn

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"unicode/utf8"
)

// NamePrefix is the prefix every network and endpoint hyperbutler creates must carry.
const NamePrefix = "hyperbutler-"

// maxNameLen bounds object names. HNS itself accepts longer names, but hyperbutler has no
// reason to, and a bound keeps names safe to log.
const maxNameLen = 64

// maxDocLen bounds any JSON document read back from HNS. Real documents are a few KiB.
const maxDocLen = 1 << 20

// Network flags (hcsshim hcn.NetworkFlags).
const (
	FlagNone                uint32 = 0
	FlagEnableNonPersistent uint32 = 8
)

// SchemaVersion is the HCN schema version. hyperbutler writes 2.0.
type SchemaVersion struct {
	Major int `json:"Major"`
	Minor int `json:"Minor"`
}

// V2 is schema version 2.0.
var V2 = SchemaVersion{Major: 2, Minor: 0}

// Route is a route in a subnet or on an endpoint.
type Route struct {
	NextHop           string `json:"NextHop,omitempty"`
	DestinationPrefix string `json:"DestinationPrefix,omitempty"`
	Metric            uint16 `json:"Metric,omitempty"`
}

// Subnet is one IPAM subnet.
type Subnet struct {
	IPAddressPrefix string  `json:"IpAddressPrefix,omitempty"`
	Routes          []Route `json:"Routes,omitempty"`
}

// Ipam is an IP address management block.
type Ipam struct {
	Type    string   `json:"Type,omitempty"`
	Subnets []Subnet `json:"Subnets,omitempty"`
}

// DNS is DNS configuration on a network or endpoint.
type DNS struct {
	Domain     string   `json:"Domain,omitempty"`
	Search     []string `json:"Search,omitempty"`
	ServerList []string `json:"ServerList,omitempty"`
}

// Network is the subset of the HCN network document hyperbutler reads and writes.
type Network struct {
	ID            string        `json:"ID,omitempty"`
	Name          string        `json:"Name,omitempty"`
	Type          string        `json:"Type,omitempty"`
	Ipams         []Ipam        `json:"Ipams,omitempty"`
	DNS           *DNS          `json:"Dns,omitempty"`
	Flags         uint32        `json:"Flags"`
	SchemaVersion SchemaVersion `json:"SchemaVersion"`
}

// IPConfig is one address on an endpoint.
type IPConfig struct {
	IPAddress    string `json:"IpAddress,omitempty"`
	PrefixLength uint8  `json:"PrefixLength,omitempty"`
}

// Endpoint is the subset of the HCN endpoint document hyperbutler reads and writes.
type Endpoint struct {
	ID                 string        `json:"ID,omitempty"`
	Name               string        `json:"Name,omitempty"`
	HostComputeNetwork string        `json:"HostComputeNetwork,omitempty"`
	IPConfigurations   []IPConfig    `json:"IpConfigurations,omitempty"`
	Routes             []Route       `json:"Routes,omitempty"`
	DNS                *DNS          `json:"Dns,omitempty"`
	MacAddress         string        `json:"MacAddress,omitempty"`
	SchemaVersion      SchemaVersion `json:"SchemaVersion"`
}

// ValidateName reports whether name is acceptable for an object hyperbutler creates or
// deletes: NamePrefix followed by 1..maxNameLen-len(NamePrefix) of [a-z0-9-].
func ValidateName(name string) error {
	if !strings.HasPrefix(name, NamePrefix) {
		return fmt.Errorf("hcn: name %q does not start with %q", name, NamePrefix)
	}
	if len(name) == len(NamePrefix) || len(name) > maxNameLen {
		return fmt.Errorf("hcn: name length %d out of range", len(name))
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return fmt.Errorf("hcn: name %q has character %q outside [a-z0-9-]", name, r)
		}
	}
	return nil
}

// NATNetwork builds a non-persistent NAT network document for prefix, with gateway as
// the default route next hop. gateway must lie inside prefix and must not be its
// network or broadcast address.
func NATNetwork(name string, prefix netip.Prefix, gateway netip.Addr) (Network, error) {
	if err := ValidateName(name); err != nil {
		return Network{}, err
	}
	if err := checkGateway(prefix, gateway); err != nil {
		return Network{}, err
	}
	return Network{
		Name: name,
		Type: "NAT",
		Ipams: []Ipam{{
			Type: "Static",
			Subnets: []Subnet{{
				IPAddressPrefix: prefix.Masked().String(),
				Routes:          []Route{{NextHop: gateway.String(), DestinationPrefix: "0.0.0.0/0"}},
			}},
		}},
		Flags:         FlagEnableNonPersistent,
		SchemaVersion: V2,
	}, nil
}

func checkGateway(prefix netip.Prefix, gateway netip.Addr) error {
	if !prefix.IsValid() || !prefix.Addr().Is4() {
		return fmt.Errorf("hcn: prefix %v is not a valid IPv4 prefix", prefix)
	}
	if prefix.Bits() < 8 || prefix.Bits() > 30 {
		return fmt.Errorf("hcn: prefix length /%d outside /8../30", prefix.Bits())
	}
	if !gateway.Is4() || !prefix.Contains(gateway) {
		return fmt.Errorf("hcn: gateway %v is not in %v", gateway, prefix)
	}
	base := prefix.Masked().Addr()
	if gateway == base || gateway == lastAddr(prefix) {
		return fmt.Errorf("hcn: gateway %v is the network or broadcast address of %v", gateway, prefix)
	}
	return nil
}

func lastAddr(p netip.Prefix) netip.Addr {
	b := p.Masked().Addr().As4()
	host := 32 - p.Bits()
	v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	v |= (1 << host) - 1
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

// StaticEndpoint builds an endpoint document on network with one static address.
func StaticEndpoint(name, networkID string, addr netip.Prefix, gateway netip.Addr, dns []netip.Addr) (Endpoint, error) {
	if err := ValidateName(name); err != nil {
		return Endpoint{}, err
	}
	if err := checkGateway(addr, gateway); err != nil {
		return Endpoint{}, err
	}
	if addr.Addr() == gateway {
		return Endpoint{}, errors.New("hcn: endpoint address equals the gateway")
	}
	ep := Endpoint{
		Name:               name,
		HostComputeNetwork: networkID,
		IPConfigurations:   []IPConfig{{IPAddress: addr.Addr().String(), PrefixLength: uint8(addr.Bits())}},
		Routes:             []Route{{NextHop: gateway.String(), DestinationPrefix: "0.0.0.0/0"}},
		SchemaVersion:      V2,
	}
	if len(dns) > 0 {
		ep.DNS = &DNS{}
		for _, d := range dns {
			ep.DNS.ServerList = append(ep.DNS.ServerList, d.String())
		}
	}
	return ep, nil
}

// ResultError is a failed HCN call, with the error record HNS returned if any.
type ResultError struct {
	Op      string
	HResult uint32
	Message string // from the error record; may be empty
}

func (e *ResultError) Error() string {
	s := fmt.Sprintf("%s: HRESULT 0x%08X", e.Op, e.HResult)
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

// errorRecord is the JSON document HCN calls return through their ErrorRecord argument.
type errorRecord struct {
	Success   *bool  `json:"Success"`
	Error     string `json:"Error"`
	ErrorCode uint32 `json:"ErrorCode"`
}

// ParseErrorRecord extracts the message and code from an HCN error record. An empty
// record yields ("", 0, nil). The message is sanitised for logging: control characters
// are replaced and its length is bounded.
func ParseErrorRecord(doc string) (msg string, code uint32, err error) {
	doc = strings.TrimSpace(doc)
	if doc == "" {
		return "", 0, nil
	}
	if len(doc) > maxDocLen {
		return "", 0, fmt.Errorf("hcn: error record of %d bytes exceeds %d", len(doc), maxDocLen)
	}
	var r errorRecord
	if err := json.Unmarshal([]byte(doc), &r); err != nil {
		return "", 0, fmt.Errorf("hcn: error record: %w", err)
	}
	return sanitize(r.Error, 512), r.ErrorCode, nil
}

// ParseIDList parses the JSON array of GUID strings that HcnEnumerateNetworks and
// HcnEnumerateEndpoints return. Each element must be a GUID in canonical form, with or
// without braces; IDs are returned upper-case without braces.
func ParseIDList(doc string) ([]string, error) {
	doc = strings.TrimSpace(doc)
	if doc == "" {
		return nil, nil
	}
	if len(doc) > maxDocLen {
		return nil, fmt.Errorf("hcn: id list of %d bytes exceeds %d", len(doc), maxDocLen)
	}
	var raw []string
	if err := json.Unmarshal([]byte(doc), &raw); err != nil {
		return nil, fmt.Errorf("hcn: id list: %w", err)
	}
	out := make([]string, 0, len(raw))
	for _, s := range raw {
		id, err := NormalizeGUID(s)
		if err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, nil
}

// NormalizeGUID checks s is a GUID (8-4-4-4-12 hex, optional braces) and returns it
// upper-case without braces.
func NormalizeGUID(s string) (string, error) {
	t := s
	if len(t) == 38 && t[0] == '{' && t[37] == '}' {
		t = t[1:37]
	}
	if len(t) != 36 {
		return "", fmt.Errorf("hcn: %q is not a GUID", truncate(s, 64))
	}
	for i := 0; i < 36; i++ {
		c := t[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return "", fmt.Errorf("hcn: %q is not a GUID", truncate(s, 64))
			}
		default:
			if !isHex(c) {
				return "", fmt.Errorf("hcn: %q is not a GUID", truncate(s, 64))
			}
		}
	}
	return strings.ToUpper(t), nil
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// ParseNetwork decodes a network properties document from HNS.
func ParseNetwork(doc string) (Network, error) {
	var n Network
	if err := decodeDoc(doc, &n); err != nil {
		return Network{}, fmt.Errorf("hcn: network: %w", err)
	}
	return n, nil
}

// ParseEndpoint decodes an endpoint properties document from HNS.
func ParseEndpoint(doc string) (Endpoint, error) {
	var e Endpoint
	if err := decodeDoc(doc, &e); err != nil {
		return Endpoint{}, fmt.Errorf("hcn: endpoint: %w", err)
	}
	return e, nil
}

func decodeDoc(doc string, v any) error {
	if len(doc) > maxDocLen {
		return fmt.Errorf("document of %d bytes exceeds %d", len(doc), maxDocLen)
	}
	d := json.NewDecoder(bytes.NewReader([]byte(doc)))
	if err := d.Decode(v); err != nil {
		return err
	}
	if d.More() {
		return errors.New("trailing data after document")
	}
	return nil
}

func sanitize(s string, max int) string {
	var b strings.Builder
	for _, r := range s {
		if b.Len() >= max {
			b.WriteString("...")
			break
		}
		switch {
		case r == utf8.RuneError:
			b.WriteRune('?')
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0):
			b.WriteRune(' ')
		default:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
