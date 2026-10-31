//go:build windows

package engine

import (
	"log/slog"
	"strings"
	"sync"

	"github.com/Higangssh/hyperbutler/internal/netstack"
)

// liveSettings applies the settings that change without a restart (ApplyLive keys the
// engine itself uses: hostGateway.*) to the running engine, when the CLI sets the
// settings event and once the guest network is up, and publishes what is in effect in
// state.json.
type liveSettings struct {
	l       Layout
	log     *slog.Logger
	publish func(*HostGatewayState)

	mu      sync.Mutex
	st      *netstack.Stack
	reloads uint64
}

// attach is called once the guest network runs. It applies settings.json as it is now,
// which also covers a change made while the VM was booting.
func (ls *liveSettings) attach(st *netstack.Stack) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.st = st
	ls.applyLocked(false)
}

// reload is called for each settings event.
func (ls *liveSettings) reload() {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.applyLocked(true)
}

func (ls *liveSettings) applyLocked(requested bool) {
	if requested {
		ls.reloads++
	}
	if ls.st == nil {
		return // attach applies the file once the network is up
	}
	addr, cur := ls.st.HostGateway()
	if !addr.IsValid() {
		return
	}
	hs := &HostGatewayState{Address: addr.String(), Reloads: ls.reloads}
	s, err := LoadSettings(ls.l)
	if err == nil {
		next := s.HostGateway()
		if err = ls.st.SetHostGateway(next); err == nil {
			if next.String() != cur.String() {
				ls.log.Info("host gateway setting changed", "from", cur.String(), "to", next.String())
			}
			cur = next
		}
	}
	if err != nil {
		// Keep what is in effect; an invalid settings.json changes nothing.
		ls.log.Warn("host gateway: settings not applied", "err", err)
		hs.Error = err.Error()
		if len(hs.Error) > maxErrorLen {
			hs.Error = strings.ToValidUTF8(hs.Error[:maxErrorLen], "")
		}
	}
	hs.Enabled, hs.Ports = cur.Enabled, cur.Ports.String()
	ls.publish(hs)
}
