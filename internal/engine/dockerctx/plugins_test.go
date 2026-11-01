package dockerctx

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSetExtraPluginDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "hb", "cli-plugins")
	other := filepath.Join(t.TempDir(), "other")
	in := []byte(`{"auths":{"r":{"auth":"x"}},"cliPluginsExtraDirs":["` + strings.ReplaceAll(other, `\`, `\\`) + `"]}`)

	out, err := SetExtraPluginDir(in, dir, true)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ExtraPluginDirs(out)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{other, dir}) {
		t.Fatalf("added: %q", got)
	}
	if !strings.Contains(string(out), `"auth": "x"`) {
		t.Fatalf("other keys lost: %s", out)
	}
	// Idempotent, also for a differently cased or uncleaned spelling.
	again, err := SetExtraPluginDir(out, strings.ToUpper(dir)+string(filepath.Separator)+".", true)
	if err != nil || string(again) != string(out) {
		t.Fatalf("second add changed the file: %v\n%s", err, again)
	}
	back, err := SetExtraPluginDir(out, dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := ExtraPluginDirs(back); !reflect.DeepEqual(got, []string{other}) {
		t.Fatalf("removed: %q", got)
	}
	// Removing the only entry drops the key.
	only, _ := SetExtraPluginDir(nil, dir, true)
	none, err := SetExtraPluginDir(only, dir, false)
	if err != nil || strings.Contains(string(none), pluginDirsKey) {
		t.Fatalf("empty list kept: %v %s", err, none)
	}
	for _, bad := range []string{"", "relative\\dir"} {
		if _, err := SetExtraPluginDir(nil, bad, true); err == nil {
			t.Errorf("dir %q accepted", bad)
		}
	}
	if _, err := SetExtraPluginDir([]byte(`{"cliPluginsExtraDirs":"C:\\x"}`), dir, true); err == nil {
		t.Error("a non-list cliPluginsExtraDirs was overwritten")
	}
}

func touch(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The bundled compose is a fallback only: any other copy in the CLI's search wins, and
// a later one takes the bundled directory out of config.json again.
func TestSyncPluginDir(t *testing.T) {
	const plugin = "docker-compose.exe"
	s := Store{Dir: filepath.Join(t.TempDir(), ".docker")}
	bundled := filepath.Join(t.TempDir(), "cli-plugins")
	system := filepath.Join(t.TempDir(), "Docker", "cli-plugins")
	listed := func() bool {
		b, err := os.ReadFile(s.configPath())
		if err != nil {
			return false
		}
		d, err := ExtraPluginDirs(b)
		if err != nil {
			t.Fatal(err)
		}
		return len(d) == 1 && samePath(d[0], bundled)
	}

	// Nothing bundled: nothing written.
	if r, err := s.SyncPluginDir(bundled, plugin, []string{system}); err != nil || r.Changed || r.Active != "" {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err := os.Stat(s.configPath()); !os.IsNotExist(err) {
		t.Fatal("config.json written without a plugin")
	}

	touch(t, filepath.Join(bundled, plugin))
	r, err := s.SyncPluginDir(bundled, plugin, []string{system})
	if err != nil || !r.Changed || !r.Listed || r.Active != filepath.Join(bundled, plugin) || !listed() {
		t.Fatalf("fallback: %+v %v", r, err)
	}
	if r, err := s.SyncPluginDir(bundled, plugin, []string{system}); err != nil || r.Changed || !r.Listed {
		t.Fatalf("second sync: %+v %v", r, err)
	}

	// Docker Desktop's copy appears: it wins, and the bundled directory is unlisted.
	touch(t, filepath.Join(system, plugin))
	r, err = s.SyncPluginDir(bundled, plugin, []string{system})
	if err != nil || !r.Changed || r.Listed || r.Active != filepath.Join(system, plugin) || listed() {
		t.Fatalf("system copy: %+v %v", r, err)
	}

	// A user copy in ~/.docker/cli-plugins wins too.
	if err := os.Remove(filepath.Join(system, plugin)); err != nil {
		t.Fatal(err)
	}
	user := filepath.Join(s.Dir, "cli-plugins", plugin)
	touch(t, user)
	if r, err := s.SyncPluginDir(bundled, plugin, []string{system}); err != nil || r.Listed || r.Active != user || listed() {
		t.Fatalf("user copy: %+v %v", r, err)
	}

	if _, err := s.SyncPluginDir(bundled, `..\x.exe`, nil); err == nil {
		t.Error("a plugin path was accepted as a name")
	}
}
