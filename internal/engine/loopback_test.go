package engine

import (
	"encoding/json"
	"net/netip"
	"strings"
	"testing"
)

func bindingsOf(t *testing.T, body []byte) map[string][]portBinding {
	t.Helper()
	var doc struct {
		HostConfig struct {
			PortBindings map[string][]portBinding `json:"PortBindings"`
		} `json:"HostConfig"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("%s: %v", body, err)
	}
	return doc.HostConfig.PortBindings
}

// `-p 127.0.0.1:8080:80` (and ::1, and the rest of 127/8) is bound to the guest's
// address, which the engine mirrors on Windows' loopback; every other binding and every
// other field is left as it was.
func TestRewriteLoopbackBindings(t *testing.T) {
	in := `{"Image":"nginx","HostConfig":{"Memory":1073741824,"NanoCpus":1500000000,"PortBindings":{` +
		`"80/tcp":[{"HostIp":"127.0.0.1","HostPort":"8080"},{"HostIp":"::1","HostPort":"8080"}],` +
		`"81/tcp":[{"HostIp":"127.0.0.2","HostPort":"8081"}],` +
		`"82/tcp":[{"HostIp":"","HostPort":"8082"}],` +
		`"83/tcp":[{"HostIp":"0.0.0.0","HostPort":"8083"}],` +
		`"84/tcp":[{"HostIp":"192.168.1.5","HostPort":"8084"}],` +
		`"85/udp":[{"HostIp":"127.0.0.1","HostPort":"5353"}]}}}`
	out, err := RewriteLoopbackBindings([]byte(in), testGuestIP)
	if err != nil {
		t.Fatal(err)
	}
	got := bindingsOf(t, out)
	want := map[string]string{"80/tcp": "192.168.127.2", "81/tcp": "192.168.127.2", "82/tcp": "", "83/tcp": "0.0.0.0",
		"84/tcp": "192.168.1.5", "85/udp": "192.168.127.2"}
	for port, ip := range want {
		for _, b := range got[port] {
			if b.HostIP != ip {
				t.Errorf("%s: HostIp %q, want %q", port, b.HostIP, ip)
			}
		}
	}
	// 127.0.0.1 and ::1 on one host port become one binding: dockerd refuses the same
	// address and port twice.
	if len(got["80/tcp"]) != 1 || got["80/tcp"][0].HostPort != "8080" {
		t.Errorf("80/tcp: %+v", got["80/tcp"])
	}
	// Large numbers keep their exact digits (no float64 round trip).
	if !strings.Contains(string(out), `"Memory":1073741824`) || !strings.Contains(string(out), `"NanoCpus":1500000000`) {
		t.Errorf("numbers changed: %s", out)
	}
	if !strings.Contains(string(out), `"Image":"nginx"`) {
		t.Errorf("other fields changed: %s", out)
	}
	// What the rewrite produces, the mirror rule publishes.
	if !reachableBinding(got["80/tcp"][0].HostIP, testGuestIP) {
		t.Error("the rewritten binding is not mirrored")
	}
}

// Bodies without a loopback binding come back byte for byte; a body that cannot be
// parsed is an error only when it may carry bindings.
func TestRewriteLoopbackBindingsUnchanged(t *testing.T) {
	for _, in := range []string{
		`{"Image":"alpine"}`,
		`{"HostConfig":{"PortBindings":{"80/tcp":[{"HostIp":"","HostPort":"8080"}]}}}`,
		`{"HostConfig":{"PortBindings":null}}`,
		`{"HostConfig":{"PortBindings":{"80/tcp":null}}}`,
		`{"HostConfig":{"PortBindings":{"80/tcp":[{"HostIp":"not an ip"}]}}}`,
		`not json at all`,
	} {
		out, err := RewriteLoopbackBindings([]byte(in), testGuestIP)
		if err != nil || string(out) != in {
			t.Errorf("%s: got %s, %v", in, out, err)
		}
	}
	if _, err := RewriteLoopbackBindings([]byte(`{"HostConfig":{"PortBindings":`), testGuestIP); err == nil {
		t.Error("a truncated body with PortBindings was accepted")
	}
	if out, err := RewriteLoopbackBindings([]byte(`{"HostConfig":{"PortBindings":{"80/tcp":[{"HostIp":"127.0.0.1"}]}}}`), netip.Addr{}); err != nil || strings.Contains(string(out), "192.168") {
		t.Errorf("without a guest address: %s, %v", out, err)
	}
	big := `{"HostConfig":{"PortBindings":{}},"x":"` + strings.Repeat("a", maxCreateBody) + `"}`
	if _, err := RewriteLoopbackBindings([]byte(big), testGuestIP); err == nil {
		t.Error("an oversized body was accepted")
	}
}

func FuzzRewriteLoopbackBindings(f *testing.F) {
	f.Add([]byte(`{"HostConfig":{"PortBindings":{"80/tcp":[{"HostIp":"127.0.0.1","HostPort":"8080"}]}}}`))
	f.Add([]byte(`{"HostConfig":{"PortBindings":{"80/tcp":[{"HostIp":"::ffff:127.0.0.1"}],"x":[1,2]}}}`))
	f.Add([]byte(`{"HostConfig":{"PortBindings":[]}}`))
	f.Fuzz(func(t *testing.T, in []byte) {
		out, err := RewriteLoopbackBindings(in, testGuestIP)
		if err != nil {
			return
		}
		if string(out) == string(in) {
			return
		}
		// A rewritten body is valid JSON with no loopback binding left.
		var doc map[string]any
		if err := json.Unmarshal(out, &doc); err != nil {
			t.Fatalf("rewrote %q into invalid JSON %q: %v", in, out, err)
		}
		hc, _ := doc["HostConfig"].(map[string]any)
		pb, _ := hc["PortBindings"].(map[string]any)
		for _, v := range pb {
			list, _ := v.([]any)
			for _, b := range list {
				m, _ := b.(map[string]any)
				ip, _ := m["HostIp"].(string)
				if a, err := netip.ParseAddr(ip); err == nil && a.Unmap().IsLoopback() {
					t.Fatalf("loopback binding %q left in %q", ip, out)
				}
			}
		}
	})
}

// Dedupe keeps distinct ports and random ports, and a binding already on the guest's
// address absorbs a loopback one with the same host port.
func TestRewriteLoopbackBindingsDedupe(t *testing.T) {
	in := `{"HostConfig":{"PortBindings":{"80/tcp":[` +
		`{"HostIp":"127.0.0.1","HostPort":"8080"},{"HostIp":"::1","HostPort":"8080"},` +
		`{"HostIp":"127.0.0.1","HostPort":"8081"},{"HostIp":"127.0.0.1","HostPort":""},{"HostIp":"::1","HostPort":""}],` +
		`"81/tcp":[{"HostIp":"192.168.127.2","HostPort":"9090"},{"HostIp":"127.0.0.1","HostPort":"9090"}],` +
		`"82/tcp":null}}}`
	out, err := RewriteLoopbackBindings([]byte(in), testGuestIP)
	if err != nil {
		t.Fatal(err)
	}
	got := bindingsOf(t, out)
	if n := len(got["80/tcp"]); n != 4 {
		t.Errorf("80/tcp: %d bindings, want 4 (8080, 8081 and two random): %+v", n, got["80/tcp"])
	}
	if n := len(got["81/tcp"]); n != 1 {
		t.Errorf("81/tcp: %+v", got["81/tcp"])
	}
	if !strings.Contains(string(out), `"82/tcp":null`) {
		t.Errorf("a null list changed: %s", out)
	}
}
