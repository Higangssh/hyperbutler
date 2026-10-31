package daemoncfg

import (
	"errors"
	"net/netip"
	"strings"
	"testing"

	"github.com/Higangssh/hyperbutler/internal/leaktest"
)

func TestMain(m *testing.M) { leaktest.VerifyTestMain(m) }

var guest = Options{GuestSubnet: netip.MustParsePrefix("192.168.127.0/24")}

func TestCheckAccepts(t *testing.T) {
	for _, s := range []string{
		`{}`,
		"{}\n",
		` { "insecure-registries": ["192.168.1.10:5000"] } `,
		`{"registry-mirrors":["https://mirror.example"],"log-driver":"json-file","log-opts":{"max-size":"10m","hosts":"nested keys are not top-level"}}`,
		`{"bip":"10.200.0.1/24","default-address-pools":[{"base":"10.201.0.0/16","size":24}]}`,
		`{"bip": 5}`,              // a wrong type is dockerd's to report
		`{"bip": "not a prefix"}`, // so is a bad prefix
		`{"debug": true, "features": {"buildkit": true}}`,
		`{"hostsfile": 1}`, // not an owned key, only a prefix of one
		`{"x": 1e400}`,     // numbers are not interpreted
	} {
		if err := Check([]byte(s), guest); err != nil {
			t.Errorf("Check(%q) = %v, want nil", s, err)
		}
	}
}

func TestCheckRefuses(t *testing.T) {
	for name, c := range map[string]struct {
		in, key, msg string
	}{
		"empty":         {"", "", "empty"},
		"blank":         {" \n\t", "", "empty"},
		"bom":           {"\xef\xbb\xbf{}", "", "byte order mark"},
		"not utf8":      {"{\"a\":\"\xff\"}", "", "UTF-8"},
		"array":         {`[]`, "", "one JSON object"},
		"string":        {`"x"`, "", "one JSON object"},
		"null":          {`null`, "", "one JSON object"},
		"trailing":      {`{} {}`, "", "after its JSON object"},
		"trailing junk": {`{}x`, "", "after its JSON object"},
		"truncated":     {`{"a": [1, 2`, "", "middle"},
		"syntax":        {"{\n  \"a\": tru\n}", "", "not valid JSON at line"},
		"comma":         {`{"a":1,}`, "", "not valid JSON"},
		"hosts":         {`{"hosts":["tcp://0.0.0.0:2375"]}`, "hosts", "owns it"},
		"Hosts case":    {`{"Hosts":["tcp://0.0.0.0:2375"]}`, "Hosts", "owns it"},
		"kelvin":        {"{\"tlsKey\":true}", "tlsKey", "hyperbutler owns"},
		"long s":        {"{\"hoſts\":[]}", "hoſts", "hyperbutler owns"},
		"iptables":      {`{"iptables":true}`, "iptables", "--iptables=false"},
		"data-root":     {`{"data-root":"/mnt"}`, "data-root", "data disk"},
		"log-level":     {`{"log-level":"debug"}`, "log-level", `"debug": true`},
		"live-restore":  {`{"live-restore":true}`, "live-restore", "dirty"},
		"duplicate":     {`{"debug":true,"debug":false}`, "debug", "twice"},
		"dup folded":    {`{"debug":true,"DEBUG":false}`, "DEBUG", "same key"},
		"bip overlap":   {`{"bip":"192.168.127.1/24"}`, "bip", "overlaps"},
		"pool overlap":  {`{"default-address-pools":[{"base":"192.168.0.0/16","size":24}]}`, "default-address-pools", "overlaps"},
		"too large":     {`{"a":"` + strings.Repeat("x", MaxSize) + `"}`, "", "limit"},
	} {
		t.Run(name, func(t *testing.T) {
			err := Check([]byte(c.in), guest)
			if err == nil {
				t.Fatalf("Check(%q) = nil", c.in)
			}
			if !strings.Contains(err.Error(), c.msg) {
				t.Errorf("error %q does not mention %q", err, c.msg)
			}
			var e *Error
			if errors.As(err, &e) && e.Key != c.key {
				t.Errorf("key %q, want %q", e.Key, c.key)
			}
		})
	}
}

func TestOwnedKeysAllRefused(t *testing.T) {
	for _, o := range Owned {
		for _, k := range []string{o.Key, strings.ToUpper(o.Key)} {
			if err := Check([]byte(`{"`+k+`": null}`), Options{}); err == nil {
				t.Errorf("owned key %q accepted", k)
			}
		}
	}
}

func TestNoSubnetCheckWithoutSubnet(t *testing.T) {
	if err := Check([]byte(`{"bip":"192.168.127.1/24"}`), Options{}); err != nil {
		t.Fatal(err)
	}
}
