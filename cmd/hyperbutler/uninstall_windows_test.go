//go:build windows

package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Higangssh/hyperbutler/internal/engine"
	"github.com/Higangssh/hyperbutler/internal/engine/dockerctx"
	"github.com/Higangssh/hyperbutler/internal/transport"
)

const testSecret = "Hunter2SecretValue"

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

// fakeInstall lays out an installation as a few starts leave it, without a VM: settings
// with the data disk in dataDir, state, logs, a daemon.json, the docker context.
func fakeInstall(t *testing.T, home, dataDir, dockerCfg string) engine.Layout {
	t.Helper()
	l := engine.Layout{Home: home, Data: dataDir}
	if err := engine.MkdirPrivate(home); err != nil {
		t.Fatal(err)
	}
	if err := engine.SaveSettings(l, &engine.Settings{DataDir: dataDir, DiskSize: engine.DefaultDiskSize}); err != nil {
		t.Fatal(err)
	}
	if err := engine.SaveState(l, &engine.State{Phase: engine.PhaseFailed, PID: 1, PIDCreated: 1, Pipe: transport.DefaultPipeName, DataDir: dataDir,
		Error: "boom token=" + testSecret, ErrorCode: engine.CodeHCSAccessDenied}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, l.DaemonConfigPath(), `{"proxies":{"http-proxy":"http://bob:`+testSecret+`@proxy:3128"},"registry-mirrors":["https://m.example"]}`)
	writeFile(t, l.EngineLogPath(), "level=INFO msg=start\nlevel=WARN msg=pull Authorization: Bearer "+testSecret+"\n")
	writeFile(t, l.ConsoleLogPath(), "[    0.000000] Linux version\n")
	writeFile(t, l.LockPath(), "")
	writeFile(t, l.OpsLockPath(), "")
	writeFile(t, l.DiskPath(), "not a real disk")
	writeFile(t, l.DiskMetaPath(), "{}")
	store := dockerctx.Store{Dir: dockerCfg}
	if err := store.Ensure(contextName, npipeHost(transport.DefaultPipeName), "test"); err != nil {
		t.Fatal(err)
	}
	if err := store.Use(contextName); err != nil {
		t.Fatal(err)
	}
	return l
}

func uninstallJSON(t *testing.T, args ...string) UninstallJSON {
	t.Helper()
	_, out, e := runCLI(t, append([]string{"uninstall", "--json"}, args...)...)
	return parseUninstall(t, out, e)
}

func parseUninstall(t *testing.T, out, stderr string) UninstallJSON {
	t.Helper()
	var r UninstallJSON
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("uninstall: %v\n%s\n%s", err, out, stderr)
	}
	return r
}

func kinds(items []UninstallItem) map[string]int {
	m := map[string]int{}
	for _, it := range items {
		m[it.Kind]++
	}
	return m
}

func TestUninstall(t *testing.T) {
	home, dockerCfg := testHome(t)
	// The data disk elsewhere, in a folder that also holds a file of the user's.
	dataDir := filepath.Join(filepath.Dir(home), "data")
	l := fakeInstall(t, home, dataDir, dockerCfg)
	writeFile(t, filepath.Join(dataDir, "mine.txt"), "the user's")
	// Another docker context and its config.json keys stay.
	writeFile(t, filepath.Join(dockerCfg, "config.json"), `{"auths":{"r":{"auth":"eA=="}},"currentContext":"hyperbutler"}`)

	r := uninstallJSON(t, "--dry-run")
	if !r.DryRun || r.Schema != SchemaVersion {
		t.Fatalf("dry run: %+v", r)
	}
	k := kinds(r.Items)
	if k[uninstallContext] != 1 || k[uninstallFile] < 9 || k[uninstallDir] != 2 {
		t.Fatalf("dry run lists %v:\n%+v", k, r.Items)
	}
	for _, it := range r.Items {
		if engine.SamePath(it.Path, dataDir) {
			t.Fatalf("the data directory with the user's file is listed: %+v", it)
		}
	}
	if !exists(l.DiskPath()) || !exists(l.SettingsPath()) {
		t.Fatal("the dry run removed files")
	}

	// Without --yes and without a console it refuses.
	if code, _, e := runCLI(t, "uninstall"); code != 1 || !strings.Contains(e, "--yes") {
		t.Fatalf("uninstall without --yes: %d %q", code, e)
	}
	if !exists(l.SettingsPath()) {
		t.Fatal("a refused uninstall removed files")
	}

	r = uninstallJSON(t, "--yes")
	if len(r.Failed) != 0 {
		t.Fatalf("failed: %+v", r.Failed)
	}
	for _, p := range []string{l.DiskPath(), l.DiskMetaPath(), l.SettingsPath(), l.EngineLogPath(), home} {
		if exists(p) {
			t.Errorf("%s is still there", p)
		}
	}
	if b, err := os.ReadFile(filepath.Join(dataDir, "mine.txt")); err != nil || string(b) != "the user's" {
		t.Fatalf("the user's file: %q %v", b, err)
	}
	store := dockerctx.Store{Dir: dockerCfg}
	if _, err := store.Get(contextName); err == nil {
		t.Fatal("the docker context is still there")
	}
	if cur, _ := store.Current(); cur != "" {
		t.Fatalf("current context %q", cur)
	}
	if b, _ := os.ReadFile(filepath.Join(dockerCfg, "config.json")); !strings.Contains(string(b), `"auth": "eA=="`) {
		t.Fatalf("config.json lost its other keys: %s", b)
	}

	// Afterwards a dry run lists nothing, and running it again removes nothing.
	if r := uninstallJSON(t, "--dry-run"); len(r.Items) != 0 {
		t.Fatalf("dry run after uninstall: %+v", r.Items)
	}
	if code, out, e := runCLI(t, "uninstall", "--yes"); code != 0 || !strings.Contains(out, "Nothing to remove") {
		t.Fatalf("second uninstall: %d %q %q", code, out, e)
	}
}

func TestUninstallKeepData(t *testing.T) {
	home, dockerCfg := testHome(t)
	l := fakeInstall(t, home, home, dockerCfg)
	r := uninstallJSON(t, "--yes", "--keep-data")
	if len(r.Failed) != 0 {
		t.Fatalf("failed: %+v", r.Failed)
	}
	if !exists(l.DiskPath()) || !exists(l.DiskMetaPath()) {
		t.Fatal("--keep-data removed the disk")
	}
	if exists(l.SettingsPath()) || exists(l.LogDir()) {
		t.Fatal("settings or logs are still there")
	}
	if r := uninstallJSON(t, "--dry-run", "--keep-data"); len(r.Items) != 0 {
		t.Fatalf("dry run after uninstall --keep-data: %+v", r.Items)
	}
	// Without --keep-data the disk and daemon.json are what is left.
	r = uninstallJSON(t, "--dry-run")
	if k := kinds(r.Items); k[uninstallFile] != 3 || k[uninstallDir] != 1 { // disk, disk.json, daemon.json
		t.Fatalf("dry run: %+v", r.Items)
	}
}

// A data directory that names a folder hyperbutler does not own: only its files go, and
// the folder stays even when empty.
func TestUninstallProtectedDataDir(t *testing.T) {
	home, dockerCfg := testHome(t)
	user := filepath.Join(filepath.Dir(home), "profile")
	t.Setenv("USERPROFILE", user)
	l := fakeInstall(t, home, user, dockerCfg)
	r := uninstallJSON(t, "--yes")
	if len(r.Failed) != 0 {
		t.Fatalf("failed: %+v", r.Failed)
	}
	if exists(l.DiskPath()) {
		t.Fatal("the disk is still there")
	}
	if fi, err := os.Stat(user); err != nil || !fi.IsDir() {
		t.Fatalf("the protected folder was removed: %v", err)
	}
	if why := protectedDir(`C:\`); why == "" {
		t.Fatal(`C:\ is not protected`)
	}
}

// A context of the same name that hyperbutler did not create, or that points at another
// installation's pipe, stays.
func TestUninstallForeignContext(t *testing.T) {
	_, dockerCfg := testHome(t)
	store := dockerctx.Store{Dir: dockerCfg}
	if err := store.Ensure(contextName, `npipe:////./pipe/other`, "x"); err != nil {
		t.Fatal(err)
	}
	r := uninstallJSON(t, "--dry-run")
	if len(r.Items) != 0 || kinds(r.Kept)[uninstallContext] != 1 {
		t.Fatalf("dry run: %+v kept %+v", r.Items, r.Kept)
	}
}

func TestDiagnose(t *testing.T) {
	home, dockerCfg := testHome(t)
	fakeInstall(t, home, home, dockerCfg)
	// The docker CLI's config.json, with credentials and a context's TLS material.
	writeFile(t, filepath.Join(dockerCfg, "config.json"), `{"auths":{"r.example":{"auth":"`+testSecret+`"}},"currentContext":"hyperbutler"}`)
	writeFile(t, filepath.Join(dockerCfg, "contexts", "tls", "x", "docker", "key.pem"), testSecret)
	// A daemon.json with CRLF line ends: files.json keeps what redaction re-indents away.
	// It holds a proxy password: no hash of the original may leave (H-1), or a guessed
	// password could be confirmed offline.
	daemonJSON := "{\r\n  \"debug\": true,\r\n  \"proxies\": {\"http-proxy\": \"http://bob:" + testSecret + "@p:3128\"}\r\n}\r\n"
	writeFile(t, filepath.Join(home, "daemon.json"), daemonJSON)
	origSum := sha256.Sum256([]byte(daemonJSON))
	out := filepath.Join(t.TempDir(), "d.zip")
	code, so, e := runCLI(t, "diagnose", "--out", out, "--json")
	if code != 0 {
		t.Fatalf("diagnose: %d %s %s", code, so, e)
	}
	var r DiagnoseJSON
	if err := json.Unmarshal([]byte(so), &r); err != nil || r.Schema != SchemaVersion || !engine.SamePath(r.File, out) {
		t.Fatalf("result %v: %s", err, so)
	}
	zr, err := zip.OpenReader(out)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = zr.Close() }()
	got := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(rc)
		_ = rc.Close()
		got[f.Name] = string(b)
		if strings.Contains(string(b), testSecret) {
			t.Errorf("%s holds the secret:\n%s", f.Name, b)
		}
		if strings.Contains(strings.ToLower(string(b)), hex.EncodeToString(origSum[:])) {
			t.Errorf("%s holds the SHA-256 of the unredacted daemon.json:\n%s", f.Name, b)
		}
	}
	for _, want := range []string{"summary.txt", "version.json", "status.json", "settings.json", "daemon.json", "state.json", "disk.json",
		"logs/engine.log", "logs/console.log", "last-start-failure.json", "host.json", "docker-contexts.json", "docker-config.json", "files.json"} {
		if _, ok := got[want]; !ok {
			t.Errorf("no %s in %v", want, r.Files)
		}
	}
	if len(r.Files) != len(zr.File) {
		t.Errorf("files %v, zip has %d", r.Files, len(zr.File))
	}
	if !strings.Contains(got["daemon.json"], `"debug": true`) || !strings.Contains(got["logs/engine.log"], "msg=start") {
		t.Errorf("redaction removed too much:\n%s\n%s", got["daemon.json"], got["logs/engine.log"])
	}
	var facts map[string]fileFacts
	redSum := sha256.Sum256([]byte(got["daemon.json"]))
	if err := json.Unmarshal([]byte(got["files.json"]), &facts); err != nil || !facts["daemon.json"].CRLF || facts["daemon.json"].Bytes != len(daemonJSON) ||
		facts["daemon.json"].RedactedSHA256 != hex.EncodeToString(redSum[:]) {
		t.Errorf("files.json: %v\n%s", err, got["files.json"])
	}
	if !strings.Contains(got["docker-config.json"], `"currentContext": "hyperbutler"`) {
		t.Errorf("docker-config.json:\n%s", got["docker-config.json"])
	}
	if !strings.Contains(got["host.json"], "computecore") || strings.Contains(got["host.json"], "powershell") {
		t.Errorf("host.json:\n%s", got["host.json"])
	}
	if !strings.Contains(got["docker-contexts.json"], `"hyperbutler"`) || !strings.Contains(got["host.json"], "windows") {
		t.Errorf("contexts or host:\n%s\n%s", got["docker-contexts.json"], got["host.json"])
	}
	// It never overwrites a file.
	if code, _, e := runCLI(t, "diagnose", "--out", out); code != 1 || !strings.Contains(e, "exists") {
		t.Fatalf("second diagnose to the same file: %d %q", code, e)
	}

	// Nothing set up: it still writes what exists.
	testHome(t)
	out2 := filepath.Join(t.TempDir(), "e.zip")
	if code, so, e := runCLI(t, "diagnose", "--out", out2); code != 0 || !strings.Contains(so, "Nothing was uploaded") {
		t.Fatalf("diagnose without an installation: %d %q %q", code, so, e)
	}
}
