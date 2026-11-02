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
