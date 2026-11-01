//go:build windows

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
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
// still found. Without the record that start created it, the directory itself stays.
func TestUninstallFindsDataFromState(t *testing.T) {
	home, dockerCfg := testHome(t)
	dataDir := filepath.Join(filepath.Dir(home), "elsewhere")
	l := fakeInstall(t, home, dataDir, dockerCfg)
	if err := os.Remove(l.SettingsPath()); err != nil {
		t.Fatal(err)
	}
	r := uninstallJSON(t, "--dry-run")
	if p := itemPaths(r.Items); !contains(p, l.DiskPath()) || contains(p, dataDir) || !contains(itemPaths(r.Kept), dataDir) {
		t.Fatalf("dry run without settings.json: %q, kept %+v", p, r.Kept)
	}
	if r := uninstallJSON(t, "--yes"); len(r.Failed) != 0 || exists(l.DiskPath()) || !exists(dataDir) || exists(home) {
		t.Fatalf("uninstall: %+v", r)
	}
}

// M-3: an invalid settings.json does not block uninstall: it is listed for removal, the
// data disk is found through state.json, and --json still prints one document.
func TestUninstallInvalidSettings(t *testing.T) {
	home, dockerCfg := testHome(t)
	dataDir := filepath.Join(filepath.Dir(home), "elsewhere")
	l := fakeInstall(t, home, dataDir, dockerCfg)
	writeFile(t, l.SettingsPath(), "{")
	code, out, e := runCLI(t, "uninstall", "--dry-run", "--json")
	r := parseUninstall(t, out, e)
	if code != 0 || !contains(itemPaths(r.Items), l.SettingsPath()) || !contains(itemPaths(r.Items), l.DiskPath()) {
		t.Fatalf("dry run with an invalid settings.json: exit %d %+v\n%s", code, r.Items, e)
	}
	if !strings.Contains(strings.Join(r.Notes, "\n"), "settings.json is invalid") {
		t.Errorf("no note on the invalid settings.json: %q", r.Notes)
	}
	if r := uninstallJSON(t, "--yes"); len(r.Failed) != 0 || exists(l.SettingsPath()) || exists(l.DiskPath()) {
		t.Fatalf("uninstall: %+v", r)
	}
	// Other commands still refuse it.
	writeFile(t, l.SettingsPath(), "{")
	if code, _, e := runCLI(t, "status"); code == 0 || !strings.Contains(e, "settings") {
		t.Fatalf("status with an invalid settings.json: %d %q", code, e)
	}
}

// M-4: settings.json and the directories without a lock file go while both locks are
// held; only the lock files and the directory holding them go after.
func TestUninstallRemovesSettingsUnderLock(t *testing.T) {
	home, dockerCfg := testHome(t)
	l := fakeInstall(t, home, filepath.Join(filepath.Dir(home), "data"), dockerCfg)
	held := map[string][2]bool{}
	old := uninstallRemove
	uninstallRemove = func(p string, timeout time.Duration, w io.Writer) error {
		ops, _ := engine.Held(l.OpsLockPath())
		eng, _ := engine.Held(l.LockPath())
		held[strings.ToLower(p)] = [2]bool{ops, eng}
		return old(p, timeout, w)
	}
	t.Cleanup(func() { uninstallRemove = old })
	if r := uninstallJSON(t, "--yes"); len(r.Failed) != 0 {
		t.Fatalf("uninstall: %+v", r.Failed)
	}
	for _, c := range []struct {
		path   string
		locked bool
	}{{l.SettingsPath(), true}, {l.LogDir(), true}, {l.Data, true}, {l.DiskPath(), true}, {l.LockPath(), false}, {l.OpsLockPath(), false}, {home, false}} {
		h, ok := held[strings.ToLower(c.path)]
		if !ok {
			t.Errorf("%s was not removed", c.path)
			continue
		}
		if h != [2]bool{c.locked, c.locked} {
			t.Errorf("%s removed with the ops and engine locks held = %v, want %v", c.path, h, c.locked)
		}
	}
}

// L-2: a data directory that existed before the first start stays, even when empty; one
// start created is removed.
func TestUninstallCreatedDataDir(t *testing.T) {
	home, dockerCfg := testHome(t)
	mine := filepath.Join(filepath.Dir(home), "docker-data")
	l := fakeInstall(t, home, mine, dockerCfg)
	s, err := engine.LoadSettings(l)
	if err != nil {
		t.Fatal(err)
	}
	s.CreatedDataDir = "" // the user made it, or an older hyperbutler did
	if err := engine.SaveSettings(l, s); err != nil {
		t.Fatal(err)
	}
	r := uninstallJSON(t, "--yes")
	if len(r.Failed) != 0 || exists(l.DiskPath()) || !exists(mine) {
		t.Fatalf("uninstall: %+v", r)
	}
	if !contains(itemPaths(r.Kept), mine) {
		t.Fatalf("%s not listed as kept: %+v", mine, r.Kept)
	}

	// start creates a missing one and records it.
	testHome(t)
	in, err := openInstall()
	if err != nil || engine.MkdirPrivate(in.l.Home) != nil {
		t.Fatal(err)
	}
	created := filepath.Join(filepath.Dir(in.l.Home), "new-data")
	if err := in.applySettings(created, "", 0, sizeFlag{}, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := in.createDataDir(); err != nil || !exists(created) {
		t.Fatalf("createDataDir: %v", err)
	}
	if s, err := engine.LoadSettings(in.l); err != nil || !engine.SamePath(s.CreatedDataDir, created) {
		t.Fatalf("not recorded: %+v %v", s, err)
	}
	// An existing directory is not recorded.
	testHome(t)
	in, _ = openInstall()
	if err := engine.MkdirPrivate(in.l.Home); err != nil {
		t.Fatal(err)
	}
	existing := t.TempDir()
	if err := in.applySettings(existing, "", 0, sizeFlag{}, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := in.createDataDir(); err != nil {
		t.Fatal(err)
	}
	if s, _ := engine.LoadSettings(in.l); s.CreatedDataDir != "" {
		t.Fatalf("recorded an existing directory: %q", s.CreatedDataDir)
	}
}

// L-3: a read-only data disk fails at once with a clear message, and keeps its
// attribute.
func TestUninstallReadOnlyDisk(t *testing.T) {
	home, dockerCfg := testHome(t)
	l := fakeInstall(t, home, home, dockerCfg)
	p, _ := windows.UTF16PtrFromString(l.DiskPath())
	if err := windows.SetFileAttributes(p, windows.FILE_ATTRIBUTE_READONLY); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = windows.SetFileAttributes(p, windows.FILE_ATTRIBUTE_NORMAL) })
	t0 := time.Now()
	code, out, e := runCLI(t, "uninstall", "--yes", "--json")
	r := parseUninstall(t, out, e)
	if d := time.Since(t0); d > 30*time.Second {
		t.Errorf("took %v", d)
	}
	if code != 1 || len(r.Failed) != 1 || !strings.Contains(r.Failed[0].Detail, "read-only") {
		t.Fatalf("exit %d, failed %+v", code, r.Failed)
	}
	if a, err := windows.GetFileAttributes(p); err != nil || a&windows.FILE_ATTRIBUTE_READONLY == 0 {
		t.Fatalf("the attribute was cleared: %x %v", a, err)
	}
}

// L-1: a dry run while the engine runs lists the access entries too, as checked again
// after the engine stops.
func TestUninstallDryRunWhileUp(t *testing.T) {
	home, dockerCfg := testHome(t)
	l := fakeInstall(t, home, home, dockerCfg)
	stubLive(t, nil)
	ridA := "1b4e28ba-2fa1-11d2-883f-0016d3cca427"
	st, _ := engine.LoadState(l)
	st.PastRuntimeIDs = []string{ridA}
	if err := engine.SaveState(l, st); err != nil {
		t.Fatal(err)
	}
	sidA, _ := runtimeSID(ridA)
	grantSID(t, l.DiskPath(), sidA)
	lk, err := engine.TryLock(l.LockPath()) // the engine runs
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lk.Unlock() }()
	r := uninstallJSON(t, "--dry-run", "--keep-data")
	k := kinds(r.Items)
	if k[uninstallEngine] != 1 || k[uninstallACL] != 1 {
		t.Fatalf("dry run while up: %+v", r.Items)
	}
	for _, it := range r.Items {
		if it.Kind == uninstallACL && !strings.Contains(it.Detail, "after the engine stops") {
			t.Errorf("acl item: %+v", it)
		}
	}
}

// L-13: every start that replaces another context records it; one that finds
// hyperbutler's selected keeps the record. Uninstall's dry run says what it restores.
func TestSelectContextRecordsLatest(t *testing.T) {
	home, dockerCfg := testHome(t)
	store := dockerctx.Store{Dir: dockerCfg}
	l := fakeInstall(t, home, home, dockerCfg)
	addContext(t, dockerCfg, "desktop-linux")
	addContext(t, dockerCfg, "colima")
	in, err := openInstall()
	if err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	for _, c := range []struct{ selected, want string }{
		{"desktop-linux", "desktop-linux"},
		{contextName, "desktop-linux"}, // already hyperbutler's: kept
		{"colima", "colima"},
		{"", "default"},
	} {
		if err := store.Use(c.selected); err != nil {
			t.Fatal(err)
		}
		in.selectContext(transport.DefaultPipeName, &out, &errb)
		if s, err := engine.LoadSettings(l); err != nil || s.PreviousContext != c.want {
			t.Fatalf("after replacing %q: recorded %+v %v", c.selected, s, err)
		}
	}
	if err := store.Use("colima"); err != nil {
		t.Fatal(err)
	}
	in.selectContext(transport.DefaultPipeName, &out, &errb)
	for _, it := range uninstallJSON(t, "--dry-run").Items {
		if it.Kind == uninstallContext && !strings.Contains(it.Detail, `goes back to "colima"`) {
			t.Fatalf("context item: %+v", it)
		}
	}
}

func addContext(t *testing.T, dockerCfg, name string) {
	t.Helper()
	sum := sha256.Sum256([]byte(name))
	writeFile(t, filepath.Join(dockerCfg, "contexts", "meta", hex.EncodeToString(sum[:]), "meta.json"),
		`{"Name":"`+name+`","Metadata":{},"Endpoints":{"docker":{"Host":"npipe:////./pipe/`+name+`"}}}`)
}

// stubLive makes live the VM accounts of the compute systems that exist, and HCS
// revocation fail (so the DACL path is taken), for the test.
func stubLive(t *testing.T, live map[string]bool) {
	t.Helper()
	oldLive, oldRevoke := liveVMSIDs, revokeVMAccess
	liveVMSIDs = func() (map[string]bool, error) { return live, nil }
	revokeVMAccess = func(string, string) error { return errors.New("no HCS in this test") }
	t.Cleanup(func() { liveVMSIDs, revokeVMAccess = oldLive, oldRevoke })
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

// The access entries on a file that stays (a kept data disk; a shared system disk
// works the same): this installation's VMs' entries go, the last one through HCS and
// earlier ones (state.json's pastRuntimeIds) from the DACL. A live VM's entry stays, and
// so does one of a VM nobody recorded: another installation that shares the image has
// granted it access and not yet created it (M-2). No HCS is needed (L-10).
func TestUninstallStaleVMAccess(t *testing.T) {
	home, dockerCfg := testHome(t)
	l := fakeInstall(t, home, home, dockerCfg)
	const (
		ridLast = "6cf7ba4c-5e89-5915-bea5-3642d57a897b" // the last engine's VM, crashed
		ridOld  = "1b4e28ba-2fa1-11d2-883f-0016d3cca427" // an earlier crash's
		ridLive = "e9059f6c-20aa-5c55-9333-25140a12719b" // a VM that runs
		ridNew  = "0f8fad5b-d9cb-469f-a165-70867728950e" // granted, not yet created: another installation's
	)
	st, err := engine.LoadState(l)
	if err != nil {
		t.Fatal(err)
	}
	st.VMID, st.RuntimeID = "hyperbutler-1-2", ridLast
	st.PastRuntimeIDs = []string{ridOld, ridLast}
	if err := engine.SaveState(l, st); err != nil {
		t.Fatal(err)
	}
	sid := map[string]string{}
	for _, rid := range []string{ridLast, ridOld, ridLive, ridNew} {
		sid[rid], _ = runtimeSID(rid)
		grantSID(t, l.DiskPath(), sid[rid])
	}
	stubLive(t, map[string]bool{sid[ridLive]: true})

	r := uninstallJSON(t, "--dry-run", "--keep-data")
	var removed, kept []string
	for _, it := range r.Items {
		if it.Kind == uninstallACL {
			removed = append(removed, it.Detail)
		}
	}
	for _, it := range r.Kept {
		if it.Kind == uninstallACL {
			kept = append(kept, it.Detail)
		}
	}
	if len(removed) != 2 || !strings.Contains(strings.Join(removed, "|"), sid[ridLast]) || !strings.Contains(strings.Join(removed, "|"), sid[ridOld]) {
		t.Fatalf("dry run removes %q", removed)
	}
	if len(kept) != 1 || !strings.Contains(kept[0], sid[ridNew]) {
		t.Fatalf("dry run keeps %q", kept)
	}
	if !strings.Contains(strings.Join(r.Notes, "\n"), "did not record") {
		t.Errorf("no note on the foreign entry: %q", r.Notes)
	}

	if r := uninstallJSON(t, "--yes", "--keep-data"); len(r.Failed) != 0 {
		t.Fatalf("uninstall: %+v", r.Failed)
	}
	left, _ := vmACEs(l.DiskPath())
	if len(left) != 2 || !contains(left, sid[ridLive]) || !contains(left, sid[ridNew]) {
		t.Fatalf("entries left: %q", left)
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
