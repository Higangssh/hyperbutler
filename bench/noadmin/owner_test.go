package main

import (
	"strings"
	"testing"
)

const runningStatus = `{
  "schema": 1, "running": true, "setUp": true, "home": "C:\\Users\\a\\AppData\\Local\\hyperbutler",
  "state": {"version": 1, "phase": "running", "pid": 4242, "pidCreated": 134064000000000000,
    "vmId": "hyperbutler-0123456789abcdef", "runtimeId": "6c8c3c79-7c1f-4f6a-9d3e-2b8f8f6c1a01",
    "pipe": "\\\\.\\pipe\\docker_engine_hyperbutler", "dataDir": "C:\\Users\\a\\AppData\\Local\\hyperbutler\\data",
    "executableVersion": "0.1.0-preview.3"}
}`

func TestOwnerFromStatus(t *testing.T) {
	o, err := ownerFromStatus([]byte(runningStatus))
	if err != nil {
		t.Fatal(err)
	}
	if o.DockerPipe != `\\.\pipe\docker_engine_hyperbutler` || o.EnginePID != 4242 || o.VMRuntimeID != "6c8c3c79-7c1f-4f6a-9d3e-2b8f8f6c1a01" || o.VMName != "hyperbutler-0123456789abcdef" {
		t.Fatalf("owner = %+v", o)
	}
	if len(o.EngineEvents) != 3 || o.EngineEvents[0] != `Local\hyperbutler-engine-stop-4242-1dc4a7f692b0000` {
		t.Fatalf("events = %v", o.EngineEvents)
	}
	if o.Schema != OwnerSchema {
		t.Fatalf("schema = %d", o.Schema)
	}
}

func TestOwnerFromStatusRefuses(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"not running", `{"running": false}`, "not running"},
		{"starting", strings.Replace(runningStatus, `"phase": "running"`, `"phase": "starting"`, 1), "starting"},
		{"no runtime id", strings.Replace(runningStatus, `"runtimeId": "6c8c3c79-7c1f-4f6a-9d3e-2b8f8f6c1a01",`, "", 1), "runtime id"},
		{"no pipe", strings.Replace(runningStatus, `"pipe": "\\\\.\\pipe\\docker_engine_hyperbutler",`, "", 1), "pipe"},
		{"not JSON", `hyperbutler: stopped`, "status --json"},
	} {
		_, err := ownerFromStatus([]byte(tc.in))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", tc.name, err, tc.want)
		}
	}
}

func TestEngineEventNames(t *testing.T) {
	got := strings.Join(engineEventNames(7, 0xabc), " ")
	want := `Local\hyperbutler-engine-stop-7-abc Local\hyperbutler-engine-daemon-7-abc Local\hyperbutler-engine-settings-7-abc`
	if got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}
