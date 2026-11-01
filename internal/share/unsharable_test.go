package share

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// TestUnsharableBindSources: UNC, WSL and device sources are refused with a reason and a
// fix before anything is resolved (REQ-FS-006), in -v and in --mount.
func TestUnsharableBindSources(t *testing.T) {
	resolve := func(p string) (string, error) {
		t.Errorf("resolved %q", p)
		return "/mnt/x", nil
	}
	for _, tc := range []struct{ src, why string }{
		{`\\server\share\src`, "is a network path"},
		{`\\server\share`, "is a network path"},
		{`\\?\UNC\server\share\x`, "is a network path"},
		{`\\wsl$\Ubuntu\home\me\app`, "is inside a WSL distribution"},
		{`\\WSL.localhost\Ubuntu\home`, "is inside a WSL distribution"},
		{`\\?\UNC\wsl$\Ubuntu\home`, "is inside a WSL distribution"},
		{`\\.\PhysicalDrive0`, "is a device path"},
		{`\\.\C:\src`, "is a device path"},
		{`\\?\Volume{0c1f2d3e-0000-0000-0000-100000000000}\src`, "is a device path"},
	} {
		for _, opts := range []string{"", ":ro"} {
			body, _ := json.Marshal(map[string]any{"HostConfig": map[string]any{"Binds": []string{tc.src + ":/data" + opts}}})
			_, err := RewriteCreate(body, resolve)
			var ue *UnsharableError
			if !errors.As(err, &ue) || ue.Source != tc.src || ue.Why != tc.why {
				t.Errorf("-v %s%s: %v", tc.src, opts, err)
				continue
			}
			msg := err.Error()
			if !strings.HasPrefix(msg, "bind source "+tc.src+" "+tc.why+"; hyperbutler shares folders on local drives only. ") || !strings.Contains(msg, `C:\src`) {
				t.Errorf("message: %s", msg)
			}
		}
		body, _ := json.Marshal(map[string]any{"HostConfig": map[string]any{"Mounts": []map[string]string{{"Type": "bind", "Source": tc.src, "Target": "/data"}}}})
		var ue *UnsharableError
		if _, err := RewriteCreate(body, resolve); !errors.As(err, &ue) || ue.Source != tc.src {
			t.Errorf("--mount %s: %v", tc.src, err)
		}
	}
}

// TestSharableSourcesPass: drive paths (also behind \\?\), guest paths, named volumes
// and Git Bash's //var/run/docker.sock are not refused.
func TestSharableSourcesPass(t *testing.T) {
	for _, spec := range []string{`C:\src:/data`, `\\?\C:\src:/data:ro`, `/var/run/docker.sock:/var/run/docker.sock`,
		`//var/run/docker.sock:/var/run/docker.sock`, `pgdata:/var/lib/postgresql/data`} {
		body, _ := json.Marshal(map[string]any{"HostConfig": map[string]any{"Binds": []string{spec}}})
		if _, err := RewriteCreate(body, func(string) (string, error) { return "/mnt/x", nil }); err != nil {
			t.Errorf("%s: %v", spec, err)
		}
	}
	// A volume mount is never a bind source, whatever its name looks like.
	body := []byte(`{"HostConfig":{"Mounts":[{"Type":"volume","Source":"\\\\server\\x","Target":"/d"}]}}`)
	if _, err := RewriteCreate(body, nil); err != nil {
		t.Errorf("volume mount: %v", err)
	}
}

func TestNetworkDriveError(t *testing.T) {
	err := NetworkDriveError(`Z:\src`, "Z:")
	if want := `bind source Z:\src is on Z:, a network drive; hyperbutler shares folders on local drives only. Copy the files`; !strings.HasPrefix(err.Error(), want) {
		t.Fatal(err)
	}
}
