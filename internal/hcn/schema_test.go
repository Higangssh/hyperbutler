package hcn

import (
	"encoding/json"
	"net/netip"
	"strings"
	"testing"
)

func TestValidateName(t *testing.T) {
	for _, tc := range []struct {
		name string
		ok   bool
	}{
		{"winbox-nat", true},
		{"winbox-spike-ep-1", true},
		{"winbox-", false},
		{"WSL", false},
		{"Default Switch", false},
		{"winbox-NAT", false},
		{"winbox-a b", false},
		{"xwinbox-a", false},
		{"winbox-" + strings.Repeat("a", 57), true},
		{"winbox-" + strings.Repeat("a", 58), false},
		{"winbox-é", false},
	} {
		if err := ValidateName(tc.name); (err == nil) != tc.ok {
			t.Errorf("ValidateName(%q) = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}

func TestNATNetwork(t *testing.T) {
	p := netip.MustParsePrefix("10.239.77.0/24")
	n, err := NATNetwork("winbox-nat", p, netip.MustParseAddr("10.239.77.1"))
	if err != nil {
		t.Fatal(err)
	}
	if n.Flags&FlagEnableNonPersistent == 0 {
		t.Error("network is persistent")
	}
	b, _ := json.Marshal(n)
	for _, want := range []string{`"Type":"NAT"`, `"IpAddressPrefix":"10.239.77.0/24"`, `"NextHop":"10.239.77.1"`, `"Flags":8`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("document %s lacks %s", b, want)
		}
	}
	for _, bad := range []struct{ prefix, gw string }{
		{"10.239.77.0/24", "10.239.78.1"},   // outside
		{"10.239.77.0/24", "10.239.77.0"},   // network address
		{"10.239.77.0/24", "10.239.77.255"}, // broadcast
		{"10.239.77.0/31", "10.239.77.1"},   // too small
		{"fd00::/64", "fd00::1"},            // IPv6
	} {
		if _, err := NATNetwork("winbox-nat", netip.MustParsePrefix(bad.prefix), netip.MustParseAddr(bad.gw)); err == nil {
			t.Errorf("NATNetwork(%s, %s) accepted", bad.prefix, bad.gw)
		}
	}
	if _, err := NATNetwork("nat", p, netip.MustParseAddr("10.239.77.1")); err == nil {
		t.Error("accepted a name without the prefix")
	}
}

func TestStaticEndpoint(t *testing.T) {
	gw := netip.MustParseAddr("10.239.77.1")
	e, err := StaticEndpoint("winbox-ep", "net", netip.MustParsePrefix("10.239.77.2/24"), gw, []netip.Addr{netip.MustParseAddr("1.1.1.1")})
	if err != nil {
		t.Fatal(err)
	}
	if e.IPConfigurations[0].IPAddress != "10.239.77.2" || e.IPConfigurations[0].PrefixLength != 24 || e.DNS.ServerList[0] != "1.1.1.1" {
		t.Errorf("endpoint %+v", e)
	}
	if _, err := StaticEndpoint("winbox-ep", "net", netip.MustParsePrefix("10.239.77.1/24"), gw, nil); err == nil {
		t.Error("accepted the gateway as the endpoint address")
	}
}

func TestParseErrorRecord(t *testing.T) {
	msg, code, err := ParseErrorRecord(`{"Success":false,"Error":"Access is denied.\r\n","ErrorCode":2147942405}`)
	if err != nil || msg != "Access is denied." || code != 0x80070005 {
		t.Errorf("got %q %#x %v", msg, code, err)
	}
	if msg, code, err := ParseErrorRecord(""); msg != "" || code != 0 || err != nil {
		t.Errorf("empty record: %q %v %v", msg, code, err)
	}
	if _, _, err := ParseErrorRecord("not json"); err == nil {
		t.Error("accepted garbage")
	}
}

func TestParseIDList(t *testing.T) {
	ids, err := ParseIDList(`["3afee72c-1e7c-4cb2-9fbc-f9663a27c59f","{7E6081D5-03C0-46AC-A30A-72A47B0C7DEA}"]`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"3AFEE72C-1E7C-4CB2-9FBC-F9663A27C59F", "7E6081D5-03C0-46AC-A30A-72A47B0C7DEA"}
	if len(ids) != 2 || ids[0] != want[0] || ids[1] != want[1] {
		t.Errorf("got %v", ids)
	}
	for _, bad := range []string{`["x"]`, `[1]`, `{"a":1}`, `["3afee72c-1e7c-4cb2-9fbc-f9663a27c59"]`, `["{3afee72c-1e7c-4cb2-9fbc-f9663a27c59f"]`} {
		if _, err := ParseIDList(bad); err == nil {
			t.Errorf("ParseIDList(%s) accepted", bad)
		}
	}
	if ids, err := ParseIDList(""); err != nil || ids != nil {
		t.Errorf("empty: %v %v", ids, err)
	}
}

// TestParseNetworkRecorded parses a trimmed copy of a document HNS returned on Windows 11
// Home 26200 (see docs/research/guest-networking.md).
func TestParseNetworkRecorded(t *testing.T) {
	doc := `{"ID":"3afee72c-1e7c-4cb2-9fbc-f9663a27c59f","Name":"winbox-spike-nat","Version":68719476736,"SchemaVersion":{"Major":2,"Minor":0},"Policies":[{"Type":"VSwitchExtension","Settings":{"ExtensionID":"f74f241b-440f-4433-bb28-00f89ead20d8"}}],"MacPool":{"Ranges":[{"StartMacAddress":"00-15-5D-E1-70-00","EndMacAddress":"00-15-5D-E1-7F-FF"}]},"Ipams":[{"Subnets":[{"ID":"6a8b1cbf-bb0a-44f8-a8a1-e264d301610a","IpAddressPrefix":"10.239.77.0/24","Routes":[{"NextHop":"10.239.77.1","DestinationPrefix":"0.0.0.0/0"}]}]}],"Flags":8}`
	n, err := ParseNetwork(doc)
	if err != nil {
		t.Fatal(err)
	}
	if n.Name != "winbox-spike-nat" || n.Flags != 8 || n.Ipams[0].Subnets[0].IPAddressPrefix != "10.239.77.0/24" {
		t.Errorf("got %+v", n)
	}
	if _, err := ParseNetwork(doc + "{}"); err == nil {
		t.Error("accepted trailing data")
	}
}
