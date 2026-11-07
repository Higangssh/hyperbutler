//go:build windows

package main

import (
	"os"
	"regexp"
	"testing"

	"github.com/Higangssh/hyperbutler/internal/gui/i18n"
)

// TestDoctorCatalog: every catalog sentence a doctor check names is in every catalog,
// with the values the check fills in, so the window never falls back to English for
// a check that has a translation.
func TestDoctorCatalog(t *testing.T) {
	reKey := regexp.MustCompile(`"(doctor\.[a-zA-Z0-9.-]+)"`)
	var keys []string
	for _, f := range []string{"doctor_windows.go", "autostart_windows.go", "doctor_setup_windows.go", "doctor_broker_windows.go", "sandbox_install_windows.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range reKey.FindAllStringSubmatch(string(b), -1) {
			keys = append(keys, m[1])
		}
	}
	// cliCheck builds its keys from a stem and the place.
	for _, stem := range []string{"doctor.docker-cli.ok", "doctor.docker-cli.unknown", "doctor.docker-cli.old"} {
		keys = append(keys, stem+"Bundled", stem+"Path")
	}
	if len(keys) < 40 {
		t.Fatalf("found only %d doctor keys", len(keys))
	}
	for _, lang := range i18n.Languages() {
		cat := i18n.Raw(lang)
		for _, k := range keys {
			if k == "doctor.docker-cli.ok" || k == "doctor.docker-cli.unknown" || k == "doctor.docker-cli.old" {
				continue
			}
			if _, ok := cat[k]; ok {
				continue
			}
			if _, ok := cat[k+"#other"]; !ok {
				t.Errorf("%s: no %s", lang, k)
			}
		}
	}
}

// TestDoctorArgs: the values a check sends are the ones its sentence uses.
func TestDoctorArgs(t *testing.T) {
	en := i18n.Raw(i18n.Source)
	checks := []DoctorCheck{
		pipeCheck(`\\.\pipe\hb-doctor-test-none`, false, nil),
		cliCheck(dockerCLI{}),
		cliCheck(dockerCLI{Bundled: `C:\hb\clients\docker.exe`}),
		otherEnginesCheck(`\\.\pipe\hb-doctor-test-none`),
	}
	checks = append(checks, virtualizationChecks(map[string]any{
		"computecore":    "loadable",
		"services":       map[string]any{"vmcompute": map[string]any{"start": uint64(3)}},
		"computeSystems": []string{"a", "b"},
	})...)
	checks = append(checks, virtualizationChecks(map[string]any{
		"computecore":          "missing",
		"services":             map[string]any{"vmcompute": "absent"},
		"hypervisorLaunchType": uint64(0),
	})...)
	for _, c := range checks {
		for _, k := range []string{c.Msg, c.FixMsg} {
			if k == "" {
				continue
			}
			s, ok := en[k]
			if !ok {
				s, ok = en[k+"#other"]
			}
			if !ok {
				t.Errorf("%s: %s is not in the catalog", c.ID, k)
				continue
			}
			for _, p := range i18n.Placeholders(s) {
				if _, ok := c.Args[p]; !ok {
					t.Errorf("%s: %s uses {%s}, which the check does not send (%v)", c.ID, k, p, c.Args)
				}
			}
		}
		if c.Msg == "" && c.Status == doctorOK {
			t.Errorf("%s: an ok check without a catalog sentence", c.ID)
		}
	}
}
