//go:build windows

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/engine"
	"github.com/Higangssh/hyperbutler/internal/engine/dockerctx"
	"github.com/Higangssh/hyperbutler/internal/transport"
)

// A data disk that cannot be removed keeps settings.json, fails the command, and a dry
// run still lists both; once the disk is released, uninstall finishes.
func TestUninstallPartialFailure(t *testing.T) {
	home, dockerCfg := testHome(t)
	l := fakeInstall(t, home, home, dockerCfg)
	old := diskRemoveTimeout
	diskRemoveTimeout = 500 * time.Millisecond
	t.Cleanup(func() { diskRemoveTimeout = old })
	f, err := os.Open(l.DiskPath()) // no FILE_SHARE_DELETE: the delete fails
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	code, out, e := runCLI(t, "uninstall", "--yes", "--json")
	r := parseUninstall(t, out, e)
	if code != 1 || len(r.Failed) != 1 || r.Failed[0].Path != l.DiskPath() || !strings.Contains(r.Failed[0].Detail, "in use by") {
		t.Fatalf("exit %d, failed %+v", code, r.Failed)
	}
	if !exists(l.SettingsPath()) || !exists(l.DiskPath()) {
		t.Fatal("settings.json went although the disk could not")
	}
	if exists(l.StatePath()) || exists(l.EngineLogPath()) {
		t.Fatal("the rest was not removed")
	}
	p := itemPaths(uninstallJSON(t, "--dry-run").Items)
	if !contains(p, l.DiskPath()) || !contains(p, l.SettingsPath()) {
		t.Fatalf("dry run after a partial uninstall: %q", p)
	}
	_ = f.Close()
	if r := uninstallJSON(t, "--yes"); len(r.Failed) != 0 || exists(home) {
		t.Fatalf("second uninstall: %+v", r)
	}
}

// With settings.json gone, the data directory the last engine used (state.json) is
// still found.
func TestUninstallFindsDataFromState(t *testing.T) {
	home, dockerCfg := testHome(t)
	dataDir := filepath.Join(filepath.Dir(home), "elsewhere")
	l := fakeInstall(t, home, dataDir, dockerCfg)
	if err := os.Remove(l.SettingsPath()); err != nil {
		t.Fatal(err)
	}
	p := itemPaths(uninstallJSON(t, "--dry-run").Items)
	if !contains(p, l.DiskPath()) || !contains(p, dataDir) {
		t.Fatalf("dry run without settings.json: %q", p)
	}
	if r := uninstallJSON(t, "--yes"); len(r.Failed) != 0 || exists(l.DiskPath()) || exists(dataDir) {
		t.Fatalf("uninstall: %+v", r)
	}
}

// The context selected before hyperbutler's is recorded and selected again.
func TestUninstallRestoresPreviousContext(t *testing.T) {
	home, dockerCfg := testHome(t)
	store := dockerctx.Store{Dir: dockerCfg}
	l := fakeInstall(t, home, home, dockerCfg)
	// desktop-linux, another tool's context, was selected before.
	sum := sha256.Sum256([]byte("desktop-linux"))
	writeFile(t, filepath.Join(dockerCfg, "contexts", "meta", hex.EncodeToString(sum[:]), "meta.json"),
		`{"Name":"desktop-linux","Metadata":{},"Endpoints":{"docker":{"Host":"npipe:////./pipe/dockerDesktopLinuxEngine"}}}`)
	if err := store.Use("desktop-linux"); err != nil {
		t.Fatal(err)
	}
	in, err := openInstall()
	if err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	in.selectContext(transport.DefaultPipeName, &out, &errb)
	if cur, _ := store.Current(); cur != contextName {
		t.Fatalf("not selected: %q %s", cur, errb.String())
	}
	s, err := engine.LoadSettings(l)
	if err != nil || s.PreviousContext != "desktop-linux" {
		t.Fatalf("previous context not recorded: %+v %v", s, err)
	}
	if r := uninstallJSON(t, "--yes"); len(r.Failed) != 0 {
		t.Fatalf("uninstall: %+v", r.Failed)
	}
	if cur, _ := store.Current(); cur != "desktop-linux" {
		t.Fatalf("current context after uninstall: %q", cur)
	}
	if _, err := store.Get("desktop-linux"); err != nil {
		t.Fatalf("desktop-linux: %v", err)
	}
}

// A stale VM access entry on a kept data disk is listed and removed.
func TestUninstallStaleVMAccess(t *testing.T) {
	if _, err := liveVMSIDs(); err != nil {
		t.Skip("HCS not available:", err)
	}
	home, dockerCfg := testHome(t)
	l := fakeInstall(t, home, home, dockerCfg)
	const sid = "S-1-5-83-1-1-2-3-4" // a VM that does not exist
	grantSID(t, l.DiskPath(), sid)
	if s, err := vmACEs(l.DiskPath()); err != nil || !contains(s, sid) {
		t.Fatalf("vmACEs: %q %v", s, err)
	}
	if r := uninstallJSON(t, "--dry-run", "--keep-data"); kinds(r.Items)[uninstallACL] != 1 {
		t.Fatalf("dry run: %+v", r.Items)
	}
	if r := uninstallJSON(t, "--yes", "--keep-data"); len(r.Failed) != 0 {
		t.Fatalf("uninstall: %+v", r.Failed)
	}
	if s, _ := vmACEs(l.DiskPath()); len(s) != 0 {
		t.Fatalf("entries left: %q", s)
	}
	if r := uninstallJSON(t, "--dry-run", "--keep-data"); len(r.Items) != 0 {
		t.Fatalf("dry run after: %+v", r.Items)
	}
}

func grantSID(t *testing.T, path, sid string) {
	t.Helper()
	s, err := windows.StringToSid(sid)
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, _ := sd.DACL()
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{AccessPermissions: windows.GENERIC_READ, AccessMode: windows.GRANT_ACCESS,
		Trustee: windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeValue: windows.TrusteeValueFromSID(s)}}}, dacl)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
}

func TestVMSID(t *testing.T) {
	// Measured: a VM with this runtime id ran as this account (the DACL of its disks).
	if s, err := vmSID("e9059f6c-20aa-5c55-9333-25140a12719b"); err != nil || s != "S-1-5-83-1-3909459820-1549082794-337982355-2607878666" {
		t.Fatalf("%q %v", s, err)
	}
}

// A relative HYPERBUTLER_HOME is refused: it would name another installation per
// working directory.
func TestRelativeHome(t *testing.T) {
	testHome(t)
	t.Setenv(engine.HomeEnv, "relative")
	if code, _, e := runCLI(t, "uninstall", "--dry-run"); code != 1 || !strings.Contains(e, "absolute") {
		t.Fatalf("relative home: %d %q", code, e)
	}
}

func itemPaths(items []UninstallItem) []string {
	var out []string
	for _, it := range items {
		out = append(out, it.Path)
	}
	return out
}

func contains(list []string, s string) bool {
	for _, e := range list {
		if strings.EqualFold(e, s) {
			return true
		}
	}
	return false
}
