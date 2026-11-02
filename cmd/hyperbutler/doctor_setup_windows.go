//go:build windows

package main

import (
	"strings"

	"golang.org/x/sys/windows/registry"
)

// installerSetupCheck reports the MSI's last machine-wide step (`hyperbutler setup`, run
// as SYSTEM) when it failed: the installer goes on after a failed step, so this is where
// the user learns that, for example, the account was not added to Hyper-V Administrators.
// ok is false without an installer record or when the last step succeeded.
func installerSetupCheck() (c DoctorCheck, ok bool) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, setupKey, registry.QUERY_VALUE)
	if err != nil {
		return c, false
	}
	defer func() { _ = k.Close() }()
	v, _, err := k.GetStringValue(setupResultValue)
	if err != nil {
		return c, false
	}
	return setupResultCheck(v)
}

// setupResultCheck turns a LastResult value ("<time> <step>: ok" or "<time> <step>:
// <error>") into a warning, or ok=false when it says ok.
func setupResultCheck(v string) (c DoctorCheck, ok bool) {
	_, rest, found := strings.Cut(v, " ")
	if !found {
		rest = v
	}
	step, detail, _ := strings.Cut(rest, ": ")
	if detail == "ok" || strings.HasPrefix(detail, "ok, ") || strings.TrimSpace(rest) == "" {
		return c, false
	}
	if len(detail) > 300 {
		detail = strings.ToValidUTF8(detail[:300], "") + "…"
	}
	return DoctorCheck{ID: "installer", Status: doctorWarn,
		Summary: "the installer's last setup step failed (" + step + "): " + detail,
		Fix:     "Uninstall and install hyperbutler again (a repair redoes only the broker service step), or do the step by hand (docs/install.md).",
		Msg:     "doctor.installer.failed", FixMsg: "doctor.fix.installer",
		Args: msgArgs("step", step, "detail", detail)}, true
}
