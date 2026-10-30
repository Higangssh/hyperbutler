package engine

import (
	"net/netip"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

var testGuestIP = netip.MustParseAddr("192.168.127.2")

const testID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestPublishedPorts(t *testing.T) {
	doc := `{"Id":"` + testID + `","NetworkSettings":{"Ports":{
		"80/tcp":[{"HostIp":"0.0.0.0","HostPort":"8080"},{"HostIp":"::","HostPort":"8080"}],
		"443/tcp":[{"HostIp":"127.0.0.1","HostPort":"8443"}],
		"53/udp":[{"HostIp":"0.0.0.0","HostPort":"5353"}],
		"22/tcp":[{"HostIp":"192.168.127.2","HostPort":"2222"}],
		"9/tcp":null,
		"1/tcp":[{"HostIp":"","HostPort":"70000"},{"HostIp":"","HostPort":"0"},{"HostIp":"","HostPort":"+5"}]}}}`
	id, ports, skipped, err := publishedPorts([]byte(doc), testGuestIP)
	if err != nil {
		t.Fatal(err)
	}
	if id != testID || !reflect.DeepEqual(ports, []uint16{2222, 8080}) || skipped != 1 {
		t.Fatalf("got %s %v", id, ports)
	}
}

func TestPublishedPortsRejects(t *testing.T) {
	for _, doc := range []string{
		``, `[]`, `{"Id":"abc"}`, `{"Id":"` + strings.ToUpper(testID) + `"}`,
		`{"Id":"` + testID + `","NetworkSettings":{"Ports":{"1/tcp":"x"}}}`,
	} {
		if _, _, _, err := publishedPorts([]byte(doc), testGuestIP); err == nil {
			t.Errorf("accepted %q", doc)
		}
	}
	var b strings.Builder
	b.WriteString(`{"Id":"` + testID + `","NetworkSettings":{"Ports":{"1/tcp":[`)
	for i := 1; i <= maxPortsPerContainer+1; i++ {
		if i > 1 {
			b.WriteString(",")
		}
		b.WriteString(`{"HostPort":"` + strconv.Itoa(10000+i) + `"}`)
	}
	b.WriteString(`]}}}`)
	if _, _, _, err := publishedPorts([]byte(b.String()), testGuestIP); err == nil {
		t.Error("too many ports accepted")
	}
}

// FuzzPublishedPorts: the inspect document comes from the guest. Whatever it holds, the
// result is a sorted, duplicate-free list of non-zero ports of bounded length.
func FuzzPublishedPorts(f *testing.F) {
	f.Add([]byte(`{"Id":"` + testID + `","NetworkSettings":{"Ports":{"80/tcp":[{"HostIp":"0.0.0.0","HostPort":"8080"}]}}}`))
	f.Add([]byte(`{"Id":"` + testID + `","NetworkSettings":{"Ports":{"80/tcp":[{"HostIp":"::ffff:192.168.127.2","HostPort":"08080"}]}}}`))
	f.Add([]byte(`{"Id":"` + testID + `","NetworkSettings":{"Ports":{"80":[{"HostPort":"1"}],"/tcp":[{"HostPort":"2"}]}}}`))
	f.Fuzz(func(t *testing.T, doc []byte) {
		id, ports, _, err := publishedPorts(doc, testGuestIP)
		if err != nil {
			return
		}
		if !validContainerID(id) {
			t.Fatalf("bad id %q accepted", id)
		}
		if len(ports) > maxPortsPerContainer {
			t.Fatalf("%d ports", len(ports))
		}
		for i, p := range ports {
			if p == 0 || (i > 0 && ports[i-1] >= p) {
				t.Fatalf("ports not sorted, unique and non-zero: %v", ports)
			}
		}
	})
}
