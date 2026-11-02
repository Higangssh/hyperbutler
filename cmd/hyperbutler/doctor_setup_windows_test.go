//go:build windows

package main

import (
	"strings"
	"testing"

	"github.com/Higangssh/hyperbutler/internal/gui/i18n"
)

// TestSetupResultCheck: a failed installer step becomes a warning with its step and
// error; a successful one, or nothing, adds no check.
func TestSetupResultCheck(t *testing.T) {
	for _, ok := range []string{"", "2026-11-02T01:17:47Z hyperv-admins add: ok", "2026-11-02T01:17:47Z vm-platform: ok"} {
		if _, show := setupResultCheck(ok); show {
			t.Errorf("%q shown", ok)
		}
	}
	c, show := setupResultCheck("2026-11-02T01:17:47Z vm-platform: setup vm-platform: dism /Enable-Feature: exit status 5\n" + strings.Repeat("x", 500))
	if !show || c.Status != doctorWarn || c.Args["step"] != "vm-platform" || !strings.HasPrefix(c.Args["detail"], "setup vm-platform: dism") {
		t.Fatalf("%+v", c)
	}
	if len(c.Args["detail"]) > 310 {
		t.Errorf("detail not bounded: %d", len(c.Args["detail"]))
	}
	en := i18n.Raw(i18n.Source)
	for _, k := range []string{c.Msg, c.FixMsg, "doctor." + c.ID} {
		if _, ok := en[k]; !ok {
			t.Errorf("no catalog sentence %s", k)
		}
	}
	for _, p := range i18n.Placeholders(en[c.Msg]) {
		if _, ok := c.Args[p]; !ok {
			t.Errorf("%s uses {%s}, not sent", c.Msg, p)
		}
	}
}
