//go:build windows

package engine

import (
	"log/slog"
	"os"
	"testing"

	"github.com/Higangssh/hyperbutler/internal/netstack"
)

// REQ-NET-004: `hyperbutler config set hostGateway.*` reaches the running stack
// through the settings event, without a restart, and an invalid settings.json changes
// nothing.
func TestLiveSettingsHostGateway(t *testing.T) {
	l := Layout{Home: testDir(t)}
	l.Data = l.Home
	st, err := netstack.New(netstack.Config{HostGateway: &netstack.HostGateway{Enabled: true}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var got []*HostGatewayState
	ls := &liveSettings{l: l, log: slog.New(slog.DiscardHandler), publish: func(h *HostGatewayState) { got = append(got, h) }}

	ls.reload() // before the network is up: counted, nothing published
	if len(got) != 0 {
		t.Fatalf("published before attach: %+v", got)
	}
	off := false
	s := &Settings{DataDir: l.Home, DiskSize: DefaultDiskSize, HostGatewayEnabled: &off}
	if err := SaveSettings(l, s); err != nil {
		t.Fatal(err)
	}
	ls.attach(st) // a change made while booting
	if _, hg := st.HostGateway(); hg.Enabled {
		t.Fatal("attach did not apply settings.json")
	}
	if h := got[len(got)-1]; h.Enabled || h.Address != "192.168.127.254" || h.Reloads != 1 || h.Ports != "all" || h.Validate() != nil {
		t.Fatalf("state %+v", h)
	}

	s.HostGatewayEnabled, s.HostGatewayPorts = nil, "5432,8000-8100"
	if err := SaveSettings(l, s); err != nil {
		t.Fatal(err)
	}
	ls.reload()
	_, hg := st.HostGateway()
	if !hg.Enabled || hg.Ports.String() != "5432,8000-8100" {
		t.Fatalf("in effect %+v", hg)
	}
	if h := got[len(got)-1]; !h.Enabled || h.Ports != "5432,8000-8100" || h.Reloads != 2 || h.Error != "" {
		t.Fatalf("state %+v", h)
	}

	// An invalid file: the setting in effect stays, and the state says why.
	if err := os.WriteFile(l.SettingsPath(), []byte(`{"version":1,"dataDir":"C:\\x","diskSizeBytes":68719476736,"hostGatewayPorts":"0"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ls.reload()
	if _, hg := st.HostGateway(); !hg.Enabled || hg.Ports.String() != "5432,8000-8100" {
		t.Fatalf("invalid settings changed the gateway: %+v", hg)
	}
	if h := got[len(got)-1]; h.Error == "" || h.Reloads != 3 || h.Ports != "5432,8000-8100" {
		t.Fatalf("state %+v", h)
	}

	ls.attach(nil)
	n := len(got)
	ls.reload()
	if len(got) != n {
		t.Fatal("published after the stack closed")
	}
}
