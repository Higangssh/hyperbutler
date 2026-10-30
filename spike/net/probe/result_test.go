package probe

import (
	"strings"
	"testing"
)

func TestParseResult(t *testing.T) {
	r, ok, err := ParseResult("WBNET step=blob ok=true bytes=29767612 ms=808.9 mbit_s=294.4\r")
	if err != nil || !ok || r.Step != "blob" || !r.OK || r.Fields["bytes"] != "29767612" {
		t.Fatalf("got %+v %v %v", r, ok, err)
	}
	// Kernel messages can share a line with ours on the serial console.
	r, ok, err = ParseResult("[    1.2] hv_netvsc: fooWBNET step=nic ok=false")
	if err != nil || !ok || r.Step != "nic" || r.OK {
		t.Fatalf("got %+v %v %v", r, ok, err)
	}
	if _, ok, _ := ParseResult("Alpine Init 3.22"); ok {
		t.Error("ordinary output taken as a result")
	}
	for _, bad := range []string{
		"WBNET ok=true",
		"WBNET step=a step=b",
		"WBNET step=a ok=maybe",
		"WBNET step=a Bad=1",
		"WBNET step=a =1",
		"WBNET step=a noequals",
		"WBNET step=a " + strings.Repeat("k=v ", 1000),
	} {
		if _, ok, err := ParseResult(bad); !ok || err == nil {
			t.Errorf("ParseResult(%.40q) = ok=%v err=%v, want an error", bad, ok, err)
		}
	}
}

func TestQuote(t *testing.T) {
	q := quote("dial tcp: lookup x=y\non 1.1.1.1")
	if strings.ContainsAny(q, " =\n") {
		t.Errorf("quote left separators: %q", q)
	}
	line := "WBNET step=x ok=false err=" + q
	if _, _, err := ParseResult(line); err != nil {
		t.Errorf("quoted value does not parse: %v", err)
	}
}
