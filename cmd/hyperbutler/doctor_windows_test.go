//go:build windows

package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Higangssh/hyperbutler/internal/engine"
)

// TestDoctorJSON: doctor runs every check read-only and reports them with schema 1.
func TestDoctorJSON(t *testing.T) {
	testHome(t)
	useFakeScheduler(t, `C:\elsewhere`)
	_, out, _ := runCLI(t, "doctor", "--json")
	var r DoctorJSON
	if err := json.Unmarshal([]byte(out), &r); err != nil || r.Schema != 1 {
		t.Fatalf("%v %q", err, out)
	}
	ids := map[string]string{}
	for _, c := range r.Checks {
		ids[c.ID] = c.Status
		switch c.Status {
		case doctorOK, doctorInfo, doctorWarn, doctorFail:
		default:
			t.Errorf("%s: status %q", c.ID, c.Status)
		}
		if c.Status == doctorFail && r.OK {
			t.Error("ok with a failed check")
		}
	}
	for _, id := range []string{"vm-platform", "hcs", "pipe", "docker-cli", "docker-context", "credential-helper", "other-engines", "host-disk", "guest-disk", "autostart"} {
		if _, ok := ids[id]; !ok {
			t.Errorf("no %s check in %q", id, out)
		}
	}
	if ids["guest-disk"] != doctorInfo {
		t.Errorf("guest-disk while stopped: %s", ids["guest-disk"])
	}
	// Text output has one line per check.
	_, txt, _ := runCLI(t, "doctor")
	if !strings.Contains(txt, "pipe") || !strings.Contains(txt, "credential-helper") {
		t.Fatalf("text: %q", txt)
	}
}

func TestVirtualizationChecks(t *testing.T) {
	good := map[string]any{"computecore": "loadable", "services": map[string]any{"vmcompute": map[string]any{"start": uint64(3)}},
		"hypervisorLaunchType": uint64(3), "computeSystems": []string{}}
	for _, c := range virtualizationChecks(good) {
		if c.Status != doctorOK {
			t.Errorf("good: %+v", c)
		}
	}
	bad := map[string]any{"computecore": "not loadable: x", "services": map[string]any{"vmcompute": map[string]any{"start": uint64(4)}},
		"hypervisorLaunchType": uint64(0), "computeSystems": "HcsEnumerateComputeSystems failed: denied"}
	cs := virtualizationChecks(bad)
	if len(cs) != 4 {
		t.Fatalf("%+v", cs)
	}
	for _, c := range cs {
		if c.Status != doctorFail || c.Fix == "" {
			t.Errorf("bad: %+v", c)
		}
	}
}

func TestPipeCheck(t *testing.T) {
	const pipe = `\\.\pipe\docker_engine_hyperbutler`
	fakePipes(t)
	if c := pipeCheck(pipe, false, nil); c.Status != doctorOK || !strings.Contains(c.Summary, "free") {
		t.Errorf("free: %+v", c)
	}
	fakePipes(t, pipe)
	if c := pipeCheck(pipe, false, nil); c.Status != doctorFail || c.Fix == "" {
		t.Errorf("taken: %+v", c)
	}
	st := &engine.State{Phase: engine.PhaseRunning, PID: 7, Pipe: pipe}
	if c := pipeCheck(pipe, true, st); c.Status != doctorOK || !strings.Contains(c.Summary, "this hyperbutler (engine pid 7)") {
		t.Errorf("ours: %+v", c)
	}
}

func TestGuestDiskCheck(t *testing.T) {
	home, _ := testHome(t)
	in := &install{l: engine.Layout{Home: home, Data: home}}
	st := &engine.State{Phase: engine.PhaseRunning, PID: 1, GuestFS: &engine.GuestFSState{SizeBytes: 100 << 30, FreeBytes: 5 << 30}}
	var guest DoctorCheck
	for _, c := range diskChecks(in, st, true) {
		if c.ID == "guest-disk" {
			guest = c
		}
	}
	if guest.Status != doctorWarn || !strings.Contains(guest.Summary, "95.0 GiB of 100.0 GiB used in the VM (95%)") || !strings.Contains(guest.Fix, "docker system prune") {
		t.Fatalf("%+v", guest)
	}
}
