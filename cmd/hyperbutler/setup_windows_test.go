//go:build windows

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

func TestSetupUserSID(t *testing.T) {
	for _, ok := range []string{"S-1-5-21-1-2-3-1001", "S-1-12-1-1-2-3-4"} {
		if err := checkSetupUserSID(ok, windows.SidTypeUser); err != nil {
			t.Error(err)
		}
	}
	for _, c := range []struct {
		sid string
		use uint32
	}{
		{"S-1-5-18", windows.SidTypeWellKnownGroup}, // SYSTEM, when a deployment tool installs
		{"S-1-5-18", windows.SidTypeUser},
		{"S-1-5-32-544", windows.SidTypeAlias},
		{"S-1-5-21-1-2-3-513", windows.SidTypeGroup},
		{"S-1-5-80-1-2-3-4-5", windows.SidTypeWellKnownGroup},
	} {
		if err := checkSetupUserSID(c.sid, c.use); err == nil {
			t.Errorf("%s (type %d) accepted", c.sid, c.use)
		}
	}
	for _, bad := range []string{"", "S-1-5-18", "S-1-5-32-578", "not a sid", "s-1-5-18"} {
		if _, _, err := setupUserSID(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	// The account running the tests is a user account.
	tok, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	if _, name, err := setupUserSID(tok.User.Sid.String()); err != nil || !strings.Contains(name, `\`) {
		t.Errorf("own SID: %q %v", name, err)
	}
}

func TestHyperVAdminsName(t *testing.T) {
	name, err := hypervAdminsName()
	if err != nil {
		t.Fatal(err)
	}
	if name == "" {
		t.Fatal("no name")
	}
}

func TestDismFeatureState(t *testing.T) {
	out := "\r\nDeployment Image Servicing and Management tool\r\nVersion: 10.0.26100.1\r\n\r\nFeature Information:\r\n\r\n" +
		"Feature Name : VirtualMachinePlatform\r\nDisplay Name : Virtual Machine Platform\r\nRestart Required : Possible\r\nState : Enable Pending\r\n"
	if s := dismFeatureState(out); s != "Enable Pending" {
		t.Errorf("state %q", s)
	}
	if s := dismFeatureState(strings.Replace(out, "Enable Pending", "Disabled", 1)); s != "Disabled" {
		t.Errorf("state %q", s)
	}
	if s := dismFeatureState("Error: 740\r\nElevated permissions are required"); s != "" {
		t.Errorf("state %q", s)
	}
}

// TestSetupNeedsElevation: the tests run unelevated, so every verb refuses before it
// touches the group, the registry or DISM.
func TestSetupNeedsElevation(t *testing.T) {
	if windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("elevated")
	}
	for _, args := range [][]string{
		{"hyperv-admins", "add", "--sid", "S-1-5-21-1-2-3-1001"},
		{"hyperv-admins", "remove", "--all"},
		{"hyperv-admins", "remove", "--all", "--if-broker"},
		{"vm-platform"},
		{"broker", "install"},
		{"broker", "uninstall"},
	} {
		var out, errb bytes.Buffer
		err := cmdSetup(args, &out, &errb)
		if err == nil || !strings.Contains(err.Error(), "elevated") {
			t.Errorf("%v: %v", args, err)
		}
	}
}

// fakeGroup stands in for Hyper-V Administrators and the account lookup.
type fakeGroup map[string]bool

func (g fakeGroup) install(t *testing.T) {
	saveNet, saveLookup := netLocalGroup, lookupSetupSID
	t.Cleanup(func() { netLocalGroup, lookupSetupSID = saveNet, saveLookup })
	netLocalGroup = func(proc *windows.LazyProc, member *windows.SID) error {
		s := member.String()
		if proc == procNetLocalGroupAddMembers {
			if g[s] {
				return errMemberInAlias
			}
			g[s] = true
			return nil
		}
		if !g[s] {
			return errMemberNotInAlias
		}
		delete(g, s)
		return nil
	}
	lookupSetupSID = func(s string) (*windows.SID, string, error) {
		if err := checkSetupUserSID(s, windows.SidTypeUser); err != nil {
			return nil, "", err
		}
		sid, err := windows.StringToSid(s)
		return sid, `PC\u` + s[len(s)-4:], err
	}
}

// TestRemovesRecord: only the uninstall's remove --all drops the record; the upgrade's
// remove --all --if-broker records its outcome, as the product stays.
func TestRemovesRecord(t *testing.T) {
	for _, c := range []struct{ all, ifBroker, want bool }{
		{true, false, true}, {true, true, false}, {false, false, false}, {false, true, false},
	} {
		if got := removesRecord(c.all, c.ifBroker); got != c.want {
			t.Errorf("removesRecord(all=%v, ifBroker=%v) = %v", c.all, c.ifBroker, got)
		}
	}
	rec := testRecord(t)
	if err := setupRemoveHyperVAdmins(rec, "", io.Discard); err != nil {
		t.Fatal(err)
	}
	finishSetup(rec, "hyperv-admins remove", nil, removesRecord(true, true))
	k, err := registry.OpenKey(rec.root, rec.path, registry.QUERY_VALUE)
	if err != nil {
		t.Fatal("the upgrade's remove left no record:", err)
	}
	defer k.Close()
	if s, _, _ := k.GetStringValue(setupResultValue); !strings.Contains(s, "hyperv-admins remove: ok") {
		t.Errorf("LastResult %q", s)
	}
}

// TestFinishSetupBrokerUninstall: a broker uninstall that succeeded (the uninstall's, or
// a failed first install's rollback) leaves no record behind when it names no account;
// with an account, or after a failure, it records the outcome.
func TestFinishSetupBrokerUninstall(t *testing.T) {
	exists := func(r setupRecord) bool {
		k, err := registry.OpenKey(r.root, r.path, registry.QUERY_VALUE)
		if err == nil {
			_ = k.Close()
		}
		return err == nil
	}
	rec := testRecord(t)
	finishSetup(rec, "broker install", errors.New("installed, but did not start"), false)
	if !exists(rec) {
		t.Fatal("a failed install recorded nothing")
	}
	finishSetup(rec, "broker uninstall", nil, false)
	if exists(rec) {
		t.Error("the rollback's uninstall left the record of a product that is gone")
	}
	rec = testRecord(t)
	if err := rec.setAdded([]string{"S-1-5-21-1-2-3-1001"}); err != nil {
		t.Fatal(err)
	}
	finishSetup(rec, "broker uninstall", nil, false)
	if v, _ := rec.added(); len(v) != 1 || !exists(rec) {
		t.Errorf("a record with an account went: %v", v)
	}
	finishSetup(rec, "broker uninstall", errors.New("stop the service: access denied"), false)
	k, err := registry.OpenKey(rec.root, rec.path, registry.QUERY_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	if s, _, _ := k.GetStringValue(setupResultValue); !strings.Contains(s, "broker uninstall: stop the service") {
		t.Errorf("LastResult %q", s)
	}
}

func testRecord(t *testing.T) setupRecord {
	path := fmt.Sprintf(`Software\hyperbutler-test\setup-%d`, time.Now().UnixNano())
	t.Cleanup(func() {
		_ = registry.DeleteKey(registry.CURRENT_USER, path)
		_ = registry.DeleteKey(registry.CURRENT_USER, `Software\hyperbutler-test`)
	})
	return setupRecord{registry.CURRENT_USER, path}
}

// TestSetupRecord: every account an install or upgrade added is removed on uninstall, one
// that was a member before is not, and a rollback takes out only its own.
func TestSetupRecord(t *testing.T) {
	const a, b, c = "S-1-5-21-1-2-3-1001", "S-1-5-21-1-2-3-1002", "S-1-5-21-1-2-3-1003"
	g := fakeGroup{c: true} // c was a member before
	g.install(t)
	rec := testRecord(t)
	for _, s := range []string{a, b, c, a} { // install by a, upgrade by b, by c, by a again
		if err := setupAddHyperVAdmin(rec, s, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	if have, _ := rec.added(); !slices.Equal(have, []string{a, b}) {
		t.Fatalf("recorded %v", have)
	}
	// A failed upgrade by b rolls back b only.
	if err := setupRemoveHyperVAdmins(rec, b, io.Discard); err != nil {
		t.Fatal(err)
	}
	if g[b] || !g[a] {
		t.Fatalf("after rollback: %v", g)
	}
	// A rollback for c, which the record does not name, leaves c in.
	if err := setupRemoveHyperVAdmins(rec, c, io.Discard); err != nil || !g[c] {
		t.Fatalf("rollback of an unrecorded member: %v %v", g, err)
	}
	if err := setupRemoveHyperVAdmins(rec, "", io.Discard); err != nil {
		t.Fatal(err)
	}
	if g[a] || !g[c] {
		t.Fatalf("after uninstall: %v", g)
	}
	if k, err := registry.OpenKey(rec.root, rec.path, registry.QUERY_VALUE); err == nil {
		k.Close()
		t.Error("the record's key survived the uninstall")
	}
	if k, err := registry.OpenKey(rec.root, `Software\hyperbutler-test`, registry.QUERY_VALUE); err == nil {
		k.Close()
		t.Error("the empty parent key survived the uninstall")
	}
}

// TestSetupRecordBadEntry: a recorded value that is not a user SID is dropped, never acted on.
func TestSetupRecordBadEntry(t *testing.T) {
	g := fakeGroup{"S-1-5-32-544": true}
	g.install(t)
	rec := testRecord(t)
	if err := rec.setAdded([]string{"S-1-5-32-544"}); err != nil {
		t.Fatal(err)
	}
	if err := setupRemoveHyperVAdmins(rec, "", io.Discard); err == nil {
		t.Error("no error for a bad entry")
	}
	if !g["S-1-5-32-544"] {
		t.Error("removed a non-user SID")
	}
	if have, _ := rec.added(); len(have) != 0 {
		t.Errorf("bad entry kept: %v", have)
	}
}

func TestSetupAddRefuses(t *testing.T) {
	g := fakeGroup{}
	g.install(t)
	rec := testRecord(t)
	for _, s := range []string{"", "S-1-5-18", "S-1-5-32-544"} {
		if err := setupAddHyperVAdmin(rec, s, io.Discard); err == nil {
			t.Errorf("%q added", s)
		}
	}
	if len(g) != 0 {
		t.Errorf("group: %v", g)
	}
	rec.result("hyperv-admins add", errors.New("x"))
	k, err := registry.OpenKey(rec.root, rec.path, registry.QUERY_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	if v, _, err := k.GetStringValue(setupResultValue); err != nil || !strings.HasSuffix(v, "hyperv-admins add: x") {
		t.Errorf("LastResult %q %v", v, err)
	}
}
