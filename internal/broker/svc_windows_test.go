//go:build windows

package broker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/Higangssh/hyperbutler/internal/vm"
)

func TestBuildInstallConfig(t *testing.T) {
	exe := `C:\Program Files\hyperbutler\hyperbutler-broker.exe`
	c, err := BuildInstallConfig(exe, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "hyperbutler-broker" || c.DisplayName != "hyperbutler broker" || !strings.Contains(c.Description, "without administrator rights") {
		t.Errorf("names: %q %q %q", c.Name, c.DisplayName, c.Description)
	}
	if want := `"` + exe + `" run`; c.CommandLine != want {
		t.Errorf("command line %q, want %q", c.CommandLine, want)
	}
	if c.StartType != windows.SERVICE_AUTO_START || !c.DelayedAutoStart || c.ErrorControl != windows.SERVICE_ERROR_NORMAL {
		t.Errorf("start: %d delayed %v error control %d", c.StartType, c.DelayedAutoStart, c.ErrorControl)
	}
	if c.Account != "LocalSystem" || c.SidType != windows.SERVICE_SID_TYPE_UNRESTRICTED {
		t.Errorf("account %q, SID type %d", c.Account, c.SidType)
	}
	wantRec := []mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
		{Type: mgr.NoAction},
	}
	if !slices.Equal(c.Recovery, wantRec) || c.ResetPeriod != 86400 || !c.RecoverOnNonCrashFailures {
		t.Errorf("recovery %+v, reset %d, non-crash %v", c.Recovery, c.ResetPeriod, c.RecoverOnNonCrashFailures)
	}
	if !slices.Contains(c.RequiredPrivileges, "SeImpersonatePrivilege") {
		t.Errorf("privileges %v lack SeImpersonatePrivilege", c.RequiredPrivileges)
	}
	for _, p := range c.RequiredPrivileges {
		if p != "SeImpersonatePrivilege" && p != "SeChangeNotifyPrivilege" {
			t.Errorf("privilege %s is more than the broker needs", p)
		}
	}
	// The executable is quoted even without a space, and the flags follow "run".
	c, err = BuildInstallConfig(`C:\hb\hyperbutler-broker.exe`, []string{"-fake-backend"})
	if err != nil {
		t.Fatal(err)
	}
	if want := `"C:\hb\hyperbutler-broker.exe" run -fake-backend`; c.CommandLine != want {
		t.Errorf("command line %q, want %q", c.CommandLine, want)
	}
	checkServiceDACL(t, c.SecurityDescriptor)
	// The config is a copy: changing it does not change the next one.
	c.Recovery[0].Delay = time.Hour
	if c2, _ := BuildInstallConfig(exe, nil); c2.Recovery[0].Delay != 5*time.Second {
		t.Error("BuildInstallConfig shares its recovery slice")
	}
}

// checkServiceDACL: SYSTEM and Administrators control the service, interactive users
// may query and start it and nothing else (no stop, pause, reconfiguration, deletion or
// DACL change), service accounts may query it, and no one else is named.
func checkServiceDACL(t *testing.T, sddl string) {
	t.Helper()
	dacl, err := serviceDACL(sddl)
	if err != nil {
		t.Fatal(err)
	}
	// Windows writes it back the same way, which the service test compares with.
	if sd, err := windows.SecurityDescriptorFromString(sddl); err != nil || sd.String() != sddl {
		t.Errorf("SDDL %q round-trips as %v, %v", sddl, sd, err)
	}
	const (
		queryConfig, changeConfig, queryStatus, enumDeps = 0x1, 0x2, 0x4, 0x8
		start, stop, pauseContinue, interrogate, userCtl = 0x10, 0x20, 0x40, 0x80, 0x100
		readControl                                      = windows.READ_CONTROL
	)
	userMask := uint32(queryConfig | queryStatus | enumDeps | start | interrogate | userCtl | readControl)
	want := map[string]uint32{
		"S-1-5-18": queryConfig | queryStatus | enumDeps | start | stop | pauseContinue | interrogate | userCtl | readControl,
		"S-1-5-32-544": queryConfig | changeConfig | queryStatus | enumDeps | start | stop | pauseContinue | interrogate | userCtl |
			windows.DELETE | readControl | windows.WRITE_DAC | windows.WRITE_OWNER,
		"S-1-5-4": userMask,                  // INTERACTIVE
		"S-1-5-6": userMask &^ uint32(start), // SERVICE
	}
	got := map[string]uint32{}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			t.Fatal(err)
		}
		if ace.Header.AceType != accessAllowedACEType {
			t.Errorf("ACE %d has type %d", i, ace.Header.AceType)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String()
		got[sid] |= uint32(ace.Mask)
	}
	if len(got) != len(want) {
		t.Errorf("DACL names %v", got)
	}
	for sid, m := range want {
		if got[sid] != m {
			t.Errorf("%s has 0x%x, want 0x%x", sid, got[sid], m)
		}
	}
}

func TestRequestServiceStartMissing(t *testing.T) {
	if err := RequestServiceStart("hyperbutler-no-such-service"); !errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		t.Errorf("missing service: %v", err)
	}
}

func TestBuildInstallConfigRejects(t *testing.T) {
	for _, c := range []struct {
		exe  string
		args []string
	}{
		{`hyperbutler-broker.exe`, nil},
		{`C:\hb\..\hb\hyperbutler-broker.exe`, nil},
		{`C:\hb\hyperbutler-broker`, nil},
		{`C:\hb\hyper"butler.exe`, nil},
		{`C:\%ProgramFiles%\hyperbutler-broker.exe`, nil}, // the SCM would expand it
		{`C:\hb%\hyperbutler-broker.exe`, nil},
		{`C:\hb\hyperbutler-broker.exe`, []string{"-pipe x"}},
		{`C:\hb\hyperbutler-broker.exe`, []string{"-pipe=x"}},
		{`C:\hb\hyperbutler-broker.exe`, []string{`"`}},
		{`C:\hb\hyperbutler-broker.exe`, []string{"run"}},
		{`C:\hb\hyperbutler-broker.exe`, []string{""}},
	} {
		if _, err := BuildInstallConfig(c.exe, c.args); err == nil {
			t.Errorf("%q %q: accepted", c.exe, c.args)
		}
	}
}

func TestCheckWritersSD(t *testing.T) {
	const (
		ti = "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464"
		// A Program Files-like folder: TrustedInstaller, SYSTEM and Administrators may
		// write; users and app packages read; CREATOR OWNER only for children.
		programFiles = "O:" + ti + "D:P(A;;FA;;;" + ti + ")(A;;0x1301bf;;;SY)(A;;0x1301bf;;;BA)(A;;0x1200a9;;;BU)" +
			"(A;OICIIO;GA;;;CO)(A;;0x1200a9;;;S-1-15-2-1)"
		// The root of C:: authenticated users may add folders to it.
		driveRoot = "O:" + ti + "D:P(A;OICI;FA;;;BA)(A;OICI;FA;;;SY)(A;OICI;0x1200a9;;;BU)(A;OICIIO;SDGXGWGR;;;AU)(A;;0x4;;;AU)"
	)
	for _, c := range []struct {
		name string
		sddl string
		mask uint32
		ok   bool
	}{
		{"program files", programFiles, leafWriteMask, true},
		{"admin-owned folder", "O:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;0x1200a9;;;BU)", leafWriteMask, true},
		{"drive root as an ancestor", driveRoot, ancestorWriteMask, true},
		{"drive root as the folder", driveRoot, leafWriteMask, false},
		{"users may write", "O:BAD:P(A;;FA;;;SY)(A;;0x1301bf;;;BU)", leafWriteMask, false},
		{"users may add files", "O:BAD:P(A;;FA;;;SY)(A;;0x2;;;BU)", leafWriteMask, false},
		{"everyone generic write", "O:BAD:P(A;;GW;;;WD)", leafWriteMask, false},
		{"users may delete children of an ancestor", "O:BAD:P(A;;FA;;;SY)(A;;0x40;;;BU)", ancestorWriteMask, false},
		{"users may change the DACL of an ancestor", "O:BAD:P(A;;WD;;;BU)", ancestorWriteMask, false},
		{"owned by a user", "O:S-1-5-21-1-2-3-1001D:P(A;;FA;;;SY)", leafWriteMask, false},
		{"owned by OWNER RIGHTS", "O:S-1-3-4D:P(A;;FA;;;SY)", leafWriteMask, false},
		{"null DACL", "O:BAD:NO_ACCESS_CONTROL", leafWriteMask, false},
		{"a deny does not make up for an allow", "O:BAD:P(D;;FA;;;BU)(A;;FA;;;BU)", leafWriteMask, false},
		{"deny ACEs are fine", "O:BAD:P(D;;FA;;;BG)(A;;FA;;;SY)", leafWriteMask, true},
		{"OWNER RIGHTS for a trusted owner", "O:BAD:P(A;;FA;;;S-1-3-4)", leafWriteMask, true},
		{"object ACE", "O:BAD:P(OA;;FA;bf967aba-0de6-11d0-a285-00aa003049e2;;BU)", leafWriteMask, false},
	} {
		sd, err := windows.SecurityDescriptorFromString(c.sddl)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		err = checkWritersSD(sd, c.mask)
		if (err == nil) != c.ok {
			t.Errorf("%s: %v, want ok=%v", c.name, err, c.ok)
		}
	}
}

func TestCheckServiceBinaryPath(t *testing.T) {
	sys, err := windows.GetSystemDirectory()
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckServiceBinaryPath(filepath.Join(sys, "svchost.exe")); err != nil {
		t.Errorf("System32: %v", err)
	}
	// Program Files, where the MSI installs the broker (a folder in it stands in for the
	// executable here).
	if pf := os.Getenv("ProgramFiles"); pf != "" {
		if err := CheckServiceBinaryPath(filepath.Join(pf, "Common Files")); err != nil {
			t.Errorf("Program Files: %v", err)
		}
	}
	// A user's own folder (the test's temporary directory) is refused.
	exe := filepath.Join(t.TempDir(), "hyperbutler-broker.exe")
	if err := os.WriteFile(exe, []byte("MZ"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckServiceBinaryPath(exe); err == nil {
		t.Errorf("%s: accepted", exe)
	}
	for _, p := range []string{`\\server\share\hyperbutler-broker.exe`, `hyperbutler-broker.exe`, `C:\no such dir\hyperbutler-broker.exe`} {
		if err := CheckServiceBinaryPath(p); err == nil {
			t.Errorf("%s: accepted", p)
		}
	}
}

func TestCheckServiceBinaryPathJunction(t *testing.T) {
	// A junction is refused even when it points into System32.
	sys, _ := windows.GetSystemDirectory()
	j := filepath.Join(t.TempDir(), "j")
	mklinkJ(t, j, sys)
	if err := CheckServiceBinaryPath(filepath.Join(j, "svchost.exe")); err == nil {
		t.Error("a path through a junction was accepted")
	}
}

func TestFormatEvent(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"plain", "plain"},
		{"line\nnext\ttab", "line\nnext\ttab"},
		{"bell\a esc\x1b del\x7f c1\u0085", "bell? esc? del? c1?"},
		{"100%", "100%"},
		{"%%1 and %%%", "% %1 and % % %"},
		{"bad \xff utf8", "bad \uFFFD utf8"},
	} {
		if got := FormatEvent("%s", c.in); got != c.want {
			t.Errorf("FormatEvent(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := FormatEvent("caller %s: %v", "S-1-5-21-1", errors.New("denied")); got != "caller S-1-5-21-1: denied" {
		t.Errorf("args: %q", got)
	}
	long := FormatEvent("%s", strings.Repeat("é", 2*maxEventRunes))
	if n := utf8.RuneCountInString(long); n != maxEventRunes+1 || !strings.HasSuffix(long, "…") {
		t.Errorf("long message: %d runes", n)
	}
}

type fakeEventWriter struct {
	mu     sync.Mutex
	events []string
}

func (f *fakeEventWriter) add(kind string, eid uint32, msg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, fmt.Sprintf("%s %d %s", kind, eid, msg))
	return nil
}
func (f *fakeEventWriter) Info(eid uint32, msg string) error    { return f.add("info", eid, msg) }
func (f *fakeEventWriter) Warning(eid uint32, msg string) error { return f.add("warning", eid, msg) }
func (f *fakeEventWriter) Error(eid uint32, msg string) error   { return f.add("error", eid, msg) }
func (f *fakeEventWriter) Close() error                         { return nil }

func TestEventLogLimit(t *testing.T) {
	w := &fakeEventWriter{}
	now := time.Unix(1000, 0)
	l := newEventLog(w, func() time.Time { return now })
	for i := 0; i < maxEventsPerWindow+5; i++ {
		l.Warnf("connection %d: %s", i, "EOF")
	}
	l.Error("start failed") // errors are never dropped
	if len(w.events) != maxEventsPerWindow+1 {
		t.Fatalf("%d events in the first minute", len(w.events))
	}
	if w.events[0] != "warning 2 connection 0: EOF" || w.events[maxEventsPerWindow] != "error 3 start failed" {
		t.Errorf("events %q ... %q", w.events[0], w.events[maxEventsPerWindow])
	}
	now = now.Add(eventWindow)
	l.Infof("serving %s", ServicePipe)
	got := w.events[maxEventsPerWindow+1:]
	if len(got) != 2 || !strings.HasPrefix(got[0], "warning 2 5 messages were not logged") || got[1] != `info 1 serving \\.\pipe\hyperbutler-broker` {
		t.Errorf("next minute: %q", got)
	}
}

type fakeInstance struct {
	done    chan error
	stopped chan struct{}
	once    sync.Once
	// hang, if set, makes Stop block until it is closed.
	hang chan struct{}
}

func newFakeInstance() *fakeInstance {
	return &fakeInstance{done: make(chan error, 1), stopped: make(chan struct{})}
}
func (f *fakeInstance) Done() <-chan error { return f.done }
func (f *fakeInstance) Stop() {
	f.once.Do(func() {
		if f.hang != nil {
			<-f.hang
		}
		time.Sleep(60 * time.Millisecond) // long enough for a few checkpoints
		close(f.stopped)
	})
}

type textLog struct {
	mu   sync.Mutex
	msgs []string
}

func (l *textLog) add(s string)     { l.mu.Lock(); l.msgs = append(l.msgs, s); l.mu.Unlock() }
func (l *textLog) Info(s string)    { l.add("info: " + s) }
func (l *textLog) Warning(s string) { l.add("warning: " + s) }
func (l *textLog) Error(s string)   { l.add("error: " + s) }
func (l *textLog) Status(s string)  { l.add("status: " + s) }
func (l *textLog) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.msgs, "\n")
}

// runHandler runs Execute, with drive sending requests and reading statuses, and returns
// its result, its log and the statuses drive did not read.
func runHandler(t *testing.T, start func() (ServiceInstance, error), drive func(req chan<- svc.ChangeRequest, st <-chan svc.Status)) (bool, uint32, *textLog, []svc.Status) {
	t.Helper()
	return runHandlerLimits(t, 5*time.Second, 5*time.Second, start, drive)
}

// runHandlerLimits is runHandler with the handler's start and stop limits.
func runHandlerLimits(t *testing.T, startLimit, stopLimit time.Duration, start func() (ServiceInstance, error), drive func(req chan<- svc.ChangeRequest, st <-chan svc.Status)) (bool, uint32, *textLog, []svc.Status) {
	t.Helper()
	log := &textLog{}
	h := &serviceHandler{start: start, log: log, tick: 10 * time.Millisecond, startLimit: startLimit, stopLimit: stopLimit}
	req := make(chan svc.ChangeRequest)
	st := make(chan svc.Status, 100)
	type result struct {
		ssec bool
		code uint32
	}
	res := make(chan result, 1)
	go func() {
		s, c := h.Execute(nil, req, st)
		res <- result{s, c}
	}()
	if drive != nil {
		drive(req, st)
	}
	select {
	case r := <-res:
		var left []svc.Status
		for len(st) > 0 {
			left = append(left, <-st)
		}
		return r.ssec, r.code, log, left
	case <-time.After(10 * time.Second):
		t.Fatal("Execute did not return")
	}
	return false, 0, nil, nil
}

// waitStatus reads statuses until one in state.
func waitStatus(t *testing.T, st <-chan svc.Status, state svc.State) svc.Status {
	t.Helper()
	for {
		select {
		case s := <-st:
			if s.State == state {
				return s
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no status %d", state)
		}
	}
}

func TestServiceHandlerStop(t *testing.T) {
	for _, cmd := range []svc.Cmd{svc.Stop, svc.Shutdown} {
		inst := newFakeInstance()
		ssec, code, log, left := runHandler(t, func() (ServiceInstance, error) {
			time.Sleep(30 * time.Millisecond)
			return inst, nil
		}, func(req chan<- svc.ChangeRequest, st <-chan svc.Status) {
			p := waitStatus(t, st, svc.StartPending)
			if p.WaitHint == 0 || p.CheckPoint == 0 {
				t.Errorf("start pending without a hint: %+v", p)
			}
			r := waitStatus(t, st, svc.Running)
			if r.Accepts != svc.AcceptStop|svc.AcceptShutdown {
				t.Errorf("accepts %d", r.Accepts)
			}
			cur := svc.Status{State: svc.Running, Accepts: r.Accepts}
			req <- svc.ChangeRequest{Cmd: svc.Interrogate, CurrentStatus: cur}
			if s := <-st; s != cur {
				t.Errorf("interrogate: %+v", s)
			}
			req <- svc.ChangeRequest{Cmd: cmd}
		})
		if ssec || code != 0 {
			t.Errorf("%d: exit %v %d", cmd, ssec, code)
		}
		select {
		case <-inst.stopped:
		default:
			t.Errorf("%d: the instance was not stopped", cmd)
		}
		// Stopping takes a few ticks: each reports StopPending with a new checkpoint.
		if len(left) < 2 {
			t.Errorf("%d: stop pending reported %d times", cmd, len(left))
		}
		for i, s := range left {
			if s.State != svc.StopPending || s.WaitHint == 0 || i > 0 && s.CheckPoint <= left[i-1].CheckPoint {
				t.Errorf("%d: while stopping: %+v", cmd, left)
				break
			}
		}
		if !strings.Contains(log.text(), "is serving") {
			t.Errorf("log: %s", log.text())
		}
	}
}

func TestServiceHandlerStartFails(t *testing.T) {
	ssec, code, log, _ := runHandler(t, func() (ServiceInstance, error) { return nil, errors.New("no state directory") }, nil)
	if !ssec || code != exitStartFailed || !strings.Contains(log.text(), "error: hyperbutler broker did not start: no state directory") {
		t.Errorf("exit %v %d, log %s", ssec, code, log.text())
	}
	taken := fmt.Errorf("%w: %s: %w", ErrPipeExists, ServicePipe, windows.ERROR_ACCESS_DENIED)
	ssec, code, log, _ = runHandler(t, func() (ServiceInstance, error) { return nil, taken }, nil)
	if !ssec || code != exitPipeTaken || !strings.Contains(log.text(), "another program holds the pipe name "+ServicePipe) ||
		!strings.Contains(log.text(), "does not fall back") {
		t.Errorf("exit %v %d, log %s", ssec, code, log.text())
	}
}

func TestServiceHandlerServeStops(t *testing.T) {
	inst := newFakeInstance()
	inst.done <- errors.New("listener broke")
	ssec, code, log, _ := runHandler(t, func() (ServiceInstance, error) { return inst, nil }, nil)
	if !ssec || code != exitServeStopped || !strings.Contains(log.text(), "stopped serving: listener broke") {
		t.Errorf("exit %v %d, log %s", ssec, code, log.text())
	}
	select {
	case <-inst.stopped:
	default:
		t.Error("the instance was not stopped")
	}
}

// TestServingStop serves a real pipe with the fake backend and stops it: the lease of
// a connection that started a VM ends, so the VM is stopped, and the pipe is gone.
// TestServiceHandlerStopHangs: a Stop that never returns is given up after the stop
// limit; the handler ends with exitServeStopped, so the process exits.
func TestServiceHandlerStopHangs(t *testing.T) {
	for _, viaDone := range []bool{false, true} {
		inst := newFakeInstance()
		inst.hang = make(chan struct{})
		defer close(inst.hang)
		if viaDone {
			inst.done <- errors.New("listener broke")
		}
		began := time.Now()
		ssec, code, log, left := runHandlerLimits(t, 5*time.Second, 200*time.Millisecond, func() (ServiceInstance, error) { return inst, nil },
			func(req chan<- svc.ChangeRequest, st <-chan svc.Status) {
				if !viaDone {
					waitStatus(t, st, svc.Running)
					req <- svc.ChangeRequest{Cmd: svc.Stop}
				}
			})
		if !ssec || code != exitServeStopped {
			t.Errorf("done=%v: exit %v %d", viaDone, ssec, code)
		}
		if d := time.Since(began); d > 3*time.Second {
			t.Errorf("done=%v: took %v", viaDone, d)
		}
		if !strings.Contains(log.text(), "did not stop within 200ms") {
			t.Errorf("done=%v: log %s", viaDone, log.text())
		}
		if strings.Contains(log.text(), "broker stopped\n") || strings.HasSuffix(log.text(), "broker stopped") {
			t.Errorf("done=%v: logged a clean stop: %s", viaDone, log.text())
		}
		if len(left) == 0 || left[len(left)-1].State != svc.StopPending {
			t.Errorf("done=%v: statuses %+v", viaDone, left)
		}
	}
}

// TestServiceHandlerStartHangs: a start that never returns is given up too.
func TestServiceHandlerStartHangs(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	ssec, code, log, _ := runHandlerLimits(t, 200*time.Millisecond, time.Second, func() (ServiceInstance, error) {
		<-block
		return nil, errors.New("too late")
	}, nil)
	if !ssec || code != exitStartFailed || !strings.Contains(log.text(), "did not start within 200ms") {
		t.Errorf("exit %v %d, log %s", ssec, code, log.text())
	}
}

// TestEventLogStatusUnlimited: the service's own lifecycle events are logged even when
// client-caused warnings have used up the minute's budget.
func TestEventLogStatusUnlimited(t *testing.T) {
	w := &fakeEventWriter{}
	now := time.Unix(1000, 0)
	l := newEventLog(w, func() time.Time { return now })
	for i := 0; i < maxEventsPerWindow; i++ {
		l.Warnf("connection %d: EOF", i)
	}
	l.Warnf("one too many")
	l.Status("hyperbutler broker stopped")
	if n := len(w.events); n != maxEventsPerWindow+1 || w.events[n-1] != "info 1 hyperbutler broker stopped" {
		t.Errorf("%d events, last %q", n, w.events[n-1])
	}
}

// TestStartServiceSweepsAfterPipe: the sweep runs only once the pipe is the broker's.
// With the name taken, StartService fails with ErrPipeExists, terminates no VM and runs
// its cleanup; with the name free it sweeps the VM an earlier broker left.
func TestStartServiceSweepsAfterPipe(t *testing.T) {
	api := newFakeHCS()
	api.list = []vm.Summary{{ID: "hyperbutler-broker-left", Owner: OwnerTag("S-1-5-21-1-2-3-1001"), State: "Running"}}
	be, err := NewHCSBackend(HCSConfig{StateDir: t.TempDir(), Logf: t.Logf, imageDir: t.TempDir(), api: api})
	if err != nil {
		t.Fatal(err)
	}
	defer be.Close()
	name := testPipeName(t)
	cfg := func(cleaned *bool) ServiceConfig {
		return ServiceConfig{Pipe: name, Instances: 2, Version: "dev", Backend: be, Sweep: func() int { return be.Sweep("") },
			Cleanup: func() { *cleaned = true }, Infof: t.Logf, Warnf: t.Logf}
	}
	squat, err := ListenPipe(PipeConfig{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	cleaned := false
	if _, err := StartService(cfg(&cleaned)); !errors.Is(err, ErrPipeExists) {
		t.Fatalf("with the name taken: %v", err)
	}
	if len(api.systems) != 0 || !cleaned {
		t.Fatalf("with the name taken: opened %d systems, cleanup %v", len(api.systems), cleaned)
	}
	_ = squat.Close()
	cleaned = false
	s, err := StartService(cfg(&cleaned))
	if err != nil {
		t.Fatal(err)
	}
	if sys := api.systems["hyperbutler-broker-left"]; sys == nil || !sys.terminated {
		t.Errorf("the left VM was not swept: %+v", sys)
	}
	s.Stop()
	if !cleaned {
		t.Error("Stop did not run the cleanup")
	}
}

// The service serves the shared endpoint and the per-user ones (ADR 0004 rule 5): a
// client's VM verbs, which only its own endpoint answers, reach the backend.
func TestStartServiceServesEndpoints(t *testing.T) {
	be := NewFakeBackend()
	s, err := StartService(ServiceConfig{Pipe: testPipeName(t), Version: "dev", Backend: be, Infof: t.Logf, Warnf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, s.l.(*Endpoints).Name())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Status(); err != nil {
		t.Fatalf("status on the own endpoint: %v", err)
	}
}

func TestServingStop(t *testing.T) {
	be := NewFakeBackend()
	srv, err := New(Config{Backend: be, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	name := testPipeName(t)
	eps, err := ListenEndpoints(srv, EndpointsConfig{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	cleaned := false
	s := startServing(srv, eps, eps.Serve, func() { cleaned = true })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Version(); err != nil {
		t.Fatal(err)
	}
	s.Stop()
	s.Stop() // a second Stop does nothing
	if !cleaned {
		t.Error("cleanup did not run")
	}
	if err := <-s.Done(); err != nil {
		t.Errorf("Serve: %v", err)
	}
	if _, _, err := c.Version(); err == nil {
		t.Error("the connection survived Stop")
	}
	_ = c.Close()
	l2, err := ListenPipe(PipeConfig{Name: name})
	if err != nil {
		t.Fatalf("the pipe is still there after Stop: %v", err)
	}
	_ = l2.Close()
}

func TestQueryService(t *testing.T) {
	st, err := QueryService("hyperbutler-no-such-service")
	if err != nil || st.Installed {
		t.Errorf("missing service: %+v, %v", st, err)
	}
	// The event log service runs on every Windows; a standard user may query it.
	st, err = QueryService("EventLog")
	if err != nil || !st.Installed || st.State != svc.Running || st.PID == 0 {
		t.Errorf("EventLog: %+v, %v", st, err)
	}
}

func TestStateName(t *testing.T) {
	if StateName(svc.Running) != "running" || StateName(svc.Stopped) != "stopped" || StateName(svc.State(99)) != "state 99" {
		t.Error(StateName(svc.Running), StateName(svc.Stopped), StateName(svc.State(99)))
	}
}

// FuzzFormatEvent: whatever an error text holds, the event text is valid UTF-8, has no
// control characters but tab and newline, no "%%", and at most maxEventRunes runes (plus
// the ellipsis).
func FuzzFormatEvent(f *testing.F) {
	for _, s := range []string{"", "plain", "%%1", "%%%", "a\x00b\x1bc", "\xff\xfe", "\u0085\u009f", strings.Repeat("%", 5000)} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		got := FormatEvent("%s", in)
		if !utf8.ValidString(got) {
			t.Fatalf("not UTF-8: %q", got)
		}
		if strings.Contains(got, "%%") {
			t.Fatalf("holds %%%%: %q", got)
		}
		n := 0
		for _, r := range got {
			n++
			if r != '\t' && r != '\n' && (r < 0x20 || r == 0x7f || r >= 0x80 && r < 0xa0) {
				t.Fatalf("control character %U in %q", r, got)
			}
		}
		if n > maxEventRunes+1 || n == maxEventRunes+1 && !strings.HasSuffix(got, "…") {
			t.Fatalf("%d runes", n)
		}
	})
}
