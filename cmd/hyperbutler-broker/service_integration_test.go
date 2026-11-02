//go:build integration && windows

package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/Higangssh/hyperbutler/internal/broker"
	"github.com/Higangssh/hyperbutler/internal/broker/proto"
)

// TestServiceLifecycle installs the broker as the Windows service hyperbutler-broker,
// with the FAKE backend (it starts no VM and makes no HCS call), and checks:
//
//   - install refuses a copy in a user-writable folder;
//   - the service's configuration (command line, start type, account, SID type,
//     privileges, recovery actions) and its event log source;
//   - a client reaches it on \\.\pipe\hyperbutler-broker, whose owner is SYSTEM, and
//     the pipe name cannot be taken while it runs;
//   - killing its process makes the SCM restart it;
//   - uninstall stops and deletes it and removes the event log source.
//
// It needs an elevated prompt, HYPERBUTLER_TEST_BROKER_SERVICE=1 and -tags integration,
// and refuses to run if the service is already installed. It builds the broker into a
// new folder under %ProgramData% that only SYSTEM and Administrators may change, and
// removes the folder at the end.
func TestServiceLifecycle(t *testing.T) {
	if os.Getenv("HYPERBUTLER_TEST_BROKER_SERVICE") != "1" {
		t.Skip("set HYPERBUTLER_TEST_BROKER_SERVICE=1 (in an elevated prompt) to install the broker service")
	}
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Fatal("needs an elevated prompt")
	}
	if st, err := broker.QueryService(broker.ServiceName); err != nil || st.Installed {
		t.Fatalf("%s is already installed (%+v, %v); this test installs and deletes it, so uninstall it first", broker.ServiceName, st, err)
	}

	dir := adminOnlyDir(t)
	exe := filepath.Join(dir, "hyperbutler-broker.exe")
	if out, err := exec.Command("go", "build", "-o", exe, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	// A copy in the user's temporary folder is refused before anything is installed.
	userCopy := filepath.Join(t.TempDir(), "hyperbutler-broker.exe")
	copyFile(t, exe, userCopy)
	if out, err := exec.Command(userCopy, "install", "-fake-backend").CombinedOutput(); err == nil || !strings.Contains(string(out), "refusing to install") {
		t.Fatalf("install from %s: %v\n%s", userCopy, err, out)
	}
	if st, _ := broker.QueryService(broker.ServiceName); st.Installed {
		t.Fatal("a refused install left the service")
	}

	t.Cleanup(func() {
		if out, err := exec.Command(exe, "uninstall").CombinedOutput(); err != nil {
			t.Errorf("uninstall: %v\n%s", err, out)
		}
	})
	if out, err := exec.Command(exe, "install", "-fake-backend").CombinedOutput(); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	checkServiceConfig(t, exe)
	if _, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Services\EventLog\Application\`+broker.EventSource, registry.QUERY_VALUE); err != nil {
		t.Errorf("event log source: %v", err)
	}

	st := waitRunning(t, 0)
	t.Logf("running as pid %d", st.PID)
	checkClient(t)
	// The name is the service's: no one else, not even an administrator, can serve it.
	if l, err := broker.ListenPipe(broker.PipeConfig{Name: broker.ServicePipe}); !errors.Is(err, broker.ErrPipeExists) {
		if l != nil {
			_ = l.Close()
		}
		t.Errorf("a second listener on %s: %v", broker.ServicePipe, err)
	}

	// Killing the process is a crash to the SCM: the first recovery action restarts the
	// service after 5 s.
	if out, err := exec.Command("taskkill", "/F", "/PID", fmt.Sprint(st.PID)).CombinedOutput(); err != nil {
		t.Fatalf("taskkill: %v\n%s", err, out)
	}
	st2 := waitRunning(t, st.PID)
	t.Logf("restarted as pid %d", st2.PID)
	checkClient(t)

	// Users may start the service; starts racing the uninstall must not leave it running
	// under a deleted service (UninstallService deletes before it stops).
	stopStarts := make(chan struct{})
	startsDone := make(chan struct{})
	go func() {
		defer close(startsDone)
		for {
			select {
			case <-stopStarts:
				return
			default:
			}
			_ = broker.RequestServiceStart(broker.ServiceName)
			time.Sleep(20 * time.Millisecond)
		}
	}()
	out, err := exec.Command(exe, "uninstall").CombinedOutput()
	close(stopStarts)
	<-startsDone
	if err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	if st, err := broker.QueryService(broker.ServiceName); err != nil || st.Installed {
		t.Errorf("after uninstall: %+v, %v", st, err)
	}
	if _, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Services\EventLog\Application\`+broker.EventSource, registry.QUERY_VALUE); err == nil {
		t.Error("the event log source is still registered")
	}
	// The pipe went with the service.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if c, err := broker.Dial(ctx, broker.ServicePipe); err == nil {
		_ = c.Close()
		t.Error("the pipe outlived the service")
	}
}

// adminOnlyDir creates a folder under %ProgramData% with a protected DACL: SYSTEM and
// Administrators full control, Users read and execute, as Program Files has.
func adminOnlyDir(t *testing.T) string {
	t.Helper()
	var b [6]byte
	_, _ = rand.Read(b[:])
	dir := filepath.Join(os.Getenv("ProgramData"), fmt.Sprintf("hyperbutler-broker-test-%x", b))
	sd, err := windows.SecurityDescriptorFromString("O:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;0x1200a9;;;BU)")
	if err != nil {
		t.Fatal(err)
	}
	sa := &windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(*sa))
	p, _ := windows.UTF16PtrFromString(dir)
	if err := windows.CreateDirectory(p, sa); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// The SCM may hold the executable for a moment after the service is deleted.
		for i := 0; i < 20; i++ {
			if err := os.RemoveAll(dir); err == nil {
				return
			}
			time.Sleep(250 * time.Millisecond)
		}
		t.Errorf("could not remove %s", dir)
	})
	return dir
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, b, 0o700); err != nil {
		t.Fatal(err)
	}
}

func checkServiceConfig(t *testing.T, exe string) {
	t.Helper()
	m, err := mgr.Connect()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(broker.ServiceName)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, err := s.Config()
	if err != nil {
		t.Fatal(err)
	}
	if want := `"` + exe + `" run -fake-backend`; c.BinaryPathName != want {
		t.Errorf("command line %q, want %q", c.BinaryPathName, want)
	}
	if c.StartType != mgr.StartAutomatic || !c.DelayedAutoStart || c.ServiceStartName != "LocalSystem" ||
		c.SidType != windows.SERVICE_SID_TYPE_UNRESTRICTED || c.DisplayName != broker.ServiceDisplayName ||
		c.Description != broker.ServiceDescription {
		t.Errorf("config %+v", c)
	}
	rec, err := s.RecoveryActions()
	if err != nil {
		t.Fatal(err)
	}
	want := []mgr.RecoveryAction{{Type: mgr.ServiceRestart, Delay: 5 * time.Second}, {Type: mgr.ServiceRestart, Delay: 30 * time.Second}, {Type: mgr.NoAction}}
	if !slices.Equal(rec, want) {
		t.Errorf("recovery actions %+v", rec)
	}
	if p, err := s.ResetPeriod(); err != nil || p != 86400 {
		t.Errorf("reset period %d, %v", p, err)
	}
	if on, err := s.RecoveryActionsOnNonCrashFailures(); err != nil || !on {
		t.Errorf("recovery on non-crash failures %v, %v", on, err)
	}
	if privs := requiredPrivileges(t, s); !slices.Equal(privs, []string{"SeChangeNotifyPrivilege", "SeImpersonatePrivilege"}) {
		t.Errorf("required privileges %q", privs)
	}
	// Interactive users may start the service (hyperbutler start does when it is
	// stopped), and do nothing else to it.
	sd, err := windows.GetSecurityInfo(s.Handle, windows.SE_SERVICE, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	if d := sd.String(); !strings.Contains(d, "(A;;CCLCSWRPLOCRRC;;;IU)") {
		t.Errorf("service DACL %s does not let interactive users start it", d)
	}
}

// requiredPrivileges reads SERVICE_CONFIG_REQUIRED_PRIVILEGES_INFO.
func requiredPrivileges(t *testing.T, s *mgr.Service) []string {
	t.Helper()
	buf := make([]byte, 4096)
	var n uint32
	if err := windows.QueryServiceConfig2(s.Handle, windows.SERVICE_CONFIG_REQUIRED_PRIVILEGES_INFO, &buf[0], uint32(len(buf)), &n); err != nil {
		t.Fatal(err)
	}
	p := *(**uint16)(unsafe.Pointer(&buf[0]))
	var out []string
	for p != nil && *p != 0 {
		s := windows.UTF16PtrToString(p)
		out = append(out, s)
		p = (*uint16)(unsafe.Add(unsafe.Pointer(p), 2*(len(windows.StringToUTF16(s)))))
	}
	return out
}

// waitRunning waits up to a minute for the service to run in a process other than
// notPID.
func waitRunning(t *testing.T, notPID uint32) broker.ServiceState {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		st, err := broker.QueryService(broker.ServiceName)
		if err != nil {
			t.Fatal(err)
		}
		if st.State == svc.Running && st.PID != 0 && st.PID != notPID {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("the service is %s (pid %d)", broker.StateName(st.State), st.PID)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// checkClient connects as the engine does and asks for the version and the status.
func checkClient(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := broker.Dial(ctx, broker.ServicePipe)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	pv, v, err := c.Version()
	if err != nil || pv != proto.Version || v != "dev" {
		t.Fatalf("version %d %q, %v", pv, v, err)
	}
	r, err := c.Status()
	if err != nil || r.State != proto.StateStopped {
		t.Fatalf("status %+v, %v", r, err)
	}
}
