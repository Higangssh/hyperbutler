//go:build windows

package main

import (
	"slices"
	"strings"
	"testing"
)

func TestServiceFlags(t *testing.T) {
	if !devBuild() {
		t.Skip("a test binary is a development build")
	}
	for _, c := range []struct {
		f    serviceFlags
		ok   bool
		args []string
	}{
		{serviceFlags{}, true, nil},
		{serviceFlags{fake: true}, true, []string{"-fake-backend"}},
		{serviceFlags{allowUnpinned: true}, true, []string{"-allow-unpinned-image"}},
		{serviceFlags{fake: true, allowUnpinned: true}, false, nil},
	} {
		if err := c.f.check(); (err == nil) != c.ok {
			t.Errorf("%+v: %v", c.f, err)
		}
		if c.ok && !slices.Equal(c.f.args(), c.args) {
			t.Errorf("%+v: args %q", c.f, c.f.args())
		}
	}
	// A release build takes neither.
	old := version
	version = "0.2.0"
	defer func() { version = old }()
	for _, f := range []serviceFlags{{fake: true}, {allowUnpinned: true}} {
		if err := f.check(); err == nil || !strings.Contains(err.Error(), "development builds only") {
			t.Errorf("release build, %+v: %v", f, err)
		}
	}
	if err := (serviceFlags{}).check(); err != nil {
		t.Errorf("release build, no flags: %v", err)
	}
}

func TestServiceCommands(t *testing.T) {
	// run refuses outside the service control manager, before it opens the event log.
	if err := cmdRun(nil); err == nil || !strings.Contains(err.Error(), "service control manager") {
		t.Errorf("run: %v", err)
	}
	for name, cmd := range subcommands {
		if err := cmd([]string{"extra"}); err == nil {
			t.Errorf("%s accepted an argument", name)
		}
		if err := cmd([]string{"-pipe", `\\.\pipe\x`}); err == nil {
			t.Errorf("%s accepted -pipe", name)
		}
	}
}

type discardLog struct{}

func (discardLog) Infof(string, ...any) {}
func (discardLog) Warnf(string, ...any) {}

// TestServiceStarterFlags: run's flag errors come back from the start function, which
// the service handler logs to the event log (open by then) and ends the service with.
func TestServiceStarterFlags(t *testing.T) {
	for _, args := range [][]string{{"-bogus"}, {"extra"}, {"-fake-backend", "-allow-unpinned-image"}} {
		inst, err := serviceStarter(args, discardLog{})()
		if err == nil || inst != nil || !strings.HasPrefix(err.Error(), "run: ") {
			t.Errorf("%q: %v", args, err)
		}
	}
	old := version
	version = "0.2.0"
	defer func() { version = old }()
	if _, err := serviceStarter([]string{"-fake-backend"}, discardLog{})(); err == nil || !strings.Contains(err.Error(), "development builds only") {
		t.Errorf("release build, -fake-backend: %v", err)
	}
}
