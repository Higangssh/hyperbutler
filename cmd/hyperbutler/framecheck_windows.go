//go:build windows

package main

import (
	"github.com/Higangssh/hyperbutler/internal/engine"
	"github.com/Higangssh/hyperbutler/internal/netstack"
)

// frameCheckMode is the guest link's per-frame CRC for these settings
// (network.frameCheck, on unless turned off; ADR 0006, "Integrity").
func frameCheckMode(s *engine.Settings) netstack.CRCMode {
	if s.FrameChecked() {
		return netstack.CRCOn
	}
	return netstack.CRCOff
}
