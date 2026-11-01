//go:build windows

package main

import (
	"testing"

	"github.com/Higangssh/hyperbutler/internal/engine"
	"github.com/Higangssh/hyperbutler/internal/netstack"
)

// network.frameCheck is on unless set to false.
func TestFrameCheckMode(t *testing.T) {
	off, on := false, true
	for _, c := range []struct {
		v    *bool
		want netstack.CRCMode
	}{{nil, netstack.CRCOn}, {&on, netstack.CRCOn}, {&off, netstack.CRCOff}} {
		if got := frameCheckMode(&engine.Settings{FrameCheck: c.v}); got != c.want {
			t.Errorf("FrameCheck %v: %v, want %v", c.v, got, c.want)
		}
	}
}
