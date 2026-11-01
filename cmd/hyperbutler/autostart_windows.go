//go:build windows

package main

import (
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/engine"
)

// Autostart (REQ-UX-015): `config set autostart true` creates one Task Scheduler task
// of the current user that runs `hyperbutler.exe start` 30 s after this user signs in;
// `false` and `uninstall` remove it. A per-user task needs no admin rights.
//
// The task is made with schtasks.exe and a task XML, not the Task Scheduler COM API:
// schtasks is part of every Windows, needs no COM binding in Go, and the XML sets
// everything (the user's own logon trigger, least privilege, no time limit, on battery
// too). hyperbutler.exe is a console program, so the task runs it through conhost.exe
// --headless, which gives it a console that is never shown: no window flashes at
// sign-in. The engine it starts already runs detached, without a window.

// scheduledTask is what the task runs.
type scheduledTask struct {
	Command   string // the program
	Arguments string
}

// taskScheduler is the Task Scheduler as autostart uses it (a fake in the tests).
type taskScheduler interface {
	// Query returns the task's action; found is false when there is no such task.
	Query(name string) (t scheduledTask, found bool, err error)
	// Create creates (or replaces) the task from its XML.
	Create(name string, taskXML []byte) error
	// Delete removes the task.
	Delete(name string) error
}

// scheduler is the real Task Scheduler, through schtasks.exe.
var scheduler taskScheduler = schtasks{run: runSchtasks}

// autostartHome is the state directory a task-started `hyperbutler start` uses (no
// HYPERBUTLER_HOME there); a variable for the tests.
var autostartHome = func() (string, error) {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		return "", errors.New("LOCALAPPDATA is not set")
	}
	return filepath.Join(base, "hyperbutler"), nil
}

// currentAccount is the user's SID and account name; a variable for the tests.
var currentAccount = func() (sid, name string, err error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", "", err
	}
	sid = u.User.Sid.String()
	acct, _, _, err := u.User.Sid.LookupAccount("")
	if err != nil {
		return sid, "", err
	}
	return sid, acct, nil
}

// autostartTaskName is the task's name in the Task Scheduler library's root folder,
// which a standard user may write: one per user, so the user's account name is in it.
func autostartTaskName() (string, error) {
	_, acct, err := currentAccount()
	if err != nil {
		return "", fmt.Errorf("autostart: who is signed in: %w", err)
	}
	clean := strings.Map(func(r rune) rune {
		if r < 0x20 || strings.ContainsRune(`\/:*?"<>|`, r) {
			return '_'
		}
		return r
	}, acct)
	return `\hyperbutler autostart (` + clean + `)`, nil
}

// autostartAction is what the task runs to start exe without a window.
func autostartAction(exe string) scheduledTask {
	sys, err := windows.GetSystemDirectory()
	if err != nil || sys == "" {
		sys = `C:\Windows\System32`
	}
	return scheduledTask{Command: filepath.Join(sys, "conhost.exe"), Arguments: `--headless "` + exe + `" start`}
}

// taskExe is the hyperbutler.exe an action of autostartAction runs, or "" for another.
func taskExe(t scheduledTask) string {
	if !strings.EqualFold(filepath.Base(t.Command), "conhost.exe") {
		return ""
	}
	rest, ok := strings.CutPrefix(t.Arguments, `--headless "`)
	if !ok {
		return ""
	}
	exe, tail, ok := strings.Cut(rest, `"`)
	if !ok || strings.TrimSpace(tail) != "start" {
		return ""
	}
	return exe
}

// taskXML is the task: this user's logon, 30 s later, at least privilege, on battery
// too, no run time limit (start returns once the engine is up; the engine runs detached).
func taskXML(sid, exe string) []byte {
	a := autostartAction(exe)
	esc := func(s string) string {
		var b bytes.Buffer
		_ = xml.EscapeText(&b, []byte(s))
		return b.String()
	}
	s := `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>Starts hyperbutler (the Docker engine) when you sign in. Created by: hyperbutler config set autostart true. Removed by: hyperbutler config set autostart false, or hyperbutler uninstall.</Description>
  </RegistrationInfo>
  <Triggers>
    <LogonTrigger>
      <Enabled>true</Enabled>
      <UserId>` + esc(sid) + `</UserId>
      <Delay>PT30S</Delay>
    </LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <UserId>` + esc(sid) + `</UserId>
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>false</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>
    <IdleSettings>
      <StopOnIdleEnd>false</StopOnIdleEnd>
      <RestartOnIdle>false</RestartOnIdle>
    </IdleSettings>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>false</Hidden>
    <RunOnlyIfIdle>false</RunOnlyIfIdle>
    <WakeToRun>false</WakeToRun>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <Priority>7</Priority>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>` + esc(a.Command) + `</Command>
      <Arguments>` + esc(a.Arguments) + `</Arguments>
      <WorkingDirectory>` + esc(filepath.Dir(exe)) + `</WorkingDirectory>
    </Exec>
  </Actions>
</Task>
`
	return encodeUTF16(strings.ReplaceAll(s, "\n", "\r\n"))
}

// encodeUTF16 is s as UTF-16LE with a byte order mark, the encoding schtasks /XML reads
// reliably.
func encodeUTF16(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, 2, 2+2*len(u))
	b[0], b[1] = 0xff, 0xfe
	for _, c := range u {
		b = binary.LittleEndian.AppendUint16(b, c)
	}
	return b
}

// decodeTaskXML reads the XML schtasks /Query /XML prints: UTF-16 (with or without a
// byte order mark), or 8-bit text in the console's output code page, which is what
// schtasks writes to a pipe (UTF-8 under code page 65001, else Korean CP949, Western
// 437 and so on: a hyperbutler.exe under C:\Users\<a Korean name> must still match).
func decodeTaskXML(b []byte) string {
	if len(b) >= 2 && (b[0] == 0xff && b[1] == 0xfe || len(b) >= 4 && b[1] == 0 && b[3] == 0) {
		if b[0] == 0xff && b[1] == 0xfe {
			b = b[2:]
		}
		u := make([]uint16, 0, len(b)/2)
		for i := 0; i+1 < len(b); i += 2 {
			u = append(u, uint16(b[i])|uint16(b[i+1])<<8)
		}
		return string(utf16.Decode(u))
	}
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	if utf8.Valid(b) {
		return string(b)
	}
	cp, err := windows.GetConsoleOutputCP()
	if err != nil || cp == 0 {
		cp = windows.GetACP()
	}
	if s, ok := fromCodePage(cp, b); ok {
		return s
	}
	return strings.ToValidUTF8(string(b), "?")
}

// fromCodePage decodes b from Windows code page cp.
func fromCodePage(cp uint32, b []byte) (string, bool) {
	if len(b) == 0 || len(b) > 1<<20 {
		return "", false
	}
	n, err := windows.MultiByteToWideChar(cp, 0, &b[0], int32(len(b)), nil, 0) //nolint:gosec // G115: bounded above
	if err != nil || n <= 0 {
		return "", false
	}
	u := make([]uint16, n)
	if _, err := windows.MultiByteToWideChar(cp, 0, &b[0], int32(len(b)), &u[0], n); err != nil { //nolint:gosec // G115: bounded above
		return "", false
	}
	return string(utf16.Decode(u)), true
}

// parseTaskAction reads the first Exec action of a task XML.
func parseTaskAction(s string) (scheduledTask, error) {
	// The declaration names UTF-16, but s is already decoded.
	if i := strings.Index(s, "?>"); strings.HasPrefix(strings.TrimSpace(s), "<?xml") && i >= 0 {
		s = s[i+2:]
	}
	var doc struct {
		Exec []struct {
			Command   string `xml:"Command"`
			Arguments string `xml:"Arguments"`
		} `xml:"Actions>Exec"`
	}
	if err := xml.Unmarshal([]byte(s), &doc); err != nil {
		return scheduledTask{}, fmt.Errorf("autostart: the task's XML: %w", err)
	}
	if len(doc.Exec) == 0 {
		return scheduledTask{}, errors.New("autostart: the task runs no program")
	}
	return scheduledTask{Command: strings.TrimSpace(doc.Exec[0].Command), Arguments: strings.TrimSpace(doc.Exec[0].Arguments)}, nil
}

// schtasks drives schtasks.exe through run (a fake in the tests, which check the
// command lines).
type schtasks struct {
	run func(args ...string) (stdout []byte, err error)
}

func (s schtasks) Query(name string) (scheduledTask, bool, error) {
	out, err := s.run("/Query", "/TN", name, "/XML")
	if err != nil {
		// A missing task is exit status 1 with nothing on stdout (the message is on
		// stderr, in the user's language).
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 && len(bytes.TrimSpace(out)) == 0 {
			return scheduledTask{}, false, nil
		}
		return scheduledTask{}, false, fmt.Errorf("schtasks /Query: %w", err)
	}
	t, err := parseTaskAction(decodeTaskXML(out))
	return t, true, err
}

func (s schtasks) Create(name string, taskXML []byte) error {
	f, err := os.CreateTemp("", "hyperbutler-task-*.xml")
	if err != nil {
		return err
	}
	path := f.Name()
	defer func() { _ = os.Remove(path) }()
	if _, err := f.Write(taskXML); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if _, err := s.run("/Create", "/TN", name, "/XML", path, "/F"); err != nil {
		return fmt.Errorf("schtasks /Create: %w", err)
	}
	return nil
}

func (s schtasks) Delete(name string) error {
	if _, err := s.run("/Delete", "/TN", name, "/F"); err != nil {
		return fmt.Errorf("schtasks /Delete: %w", err)
	}
	return nil
}

// runSchtasks runs %SystemRoot%\System32\schtasks.exe without a window; its stderr (the
// error message, localized) becomes part of the error.
func runSchtasks(args ...string) ([]byte, error) {
	sys, err := windows.GetSystemDirectory()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(filepath.Join(sys, "schtasks.exe"), args...) //nolint:gosec // G204: the system's schtasks.exe with arguments built here
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	if err != nil {
		if msg := strings.TrimSpace(decodeTaskXML(stderr.Bytes())); msg != "" {
			return stdout.Bytes(), fmt.Errorf("%w: %s", err, printable([]byte(msg)))
		}
	}
	return stdout.Bytes(), err
}

// autostartOurs reports whether in is the installation the task starts (the one in
// %LOCALAPPDATA%\hyperbutler, home): only that one shows, changes or removes it.
func autostartOurs(in *install) (home string, ok bool) {
	home, err := autostartHome()
	return home, err == nil && engine.SamePath(home, in.l.Home)
}

// autostartFor is the task's state for in: for another installation (HYPERBUTLER_HOME)
// it is not looked up, and Error says why.
func autostartFor(in *install) AutostartJSON {
	if home, ok := autostartOurs(in); !ok {
		return AutostartJSON{Error: "it starts the installation in " + home + ", not this one"}
	}
	return autostartState()
}

// autostartState looks the task up.
func autostartState() AutostartJSON {
	name, err := autostartTaskName()
	if err != nil {
		return AutostartJSON{Error: err.Error()}
	}
	a := AutostartJSON{Task: name}
	t, found, err := scheduler.Query(name)
	if err != nil {
		a.Error = err.Error()
	}
	if !found {
		return a
	}
	a.Exists, a.Command = true, strings.TrimSpace(t.Command+" "+t.Arguments)
	if exe, err := os.Executable(); err == nil {
		if te := taskExe(t); te != "" {
			a.ThisExe = sameExecutable(te, exe)
		}
	}
	return a
}

// describeAutostart is config list's line about the task.
func describeAutostart(a AutostartJSON, wanted bool) string {
	switch {
	case a.Error != "" && !a.Exists:
		if a.Task == "" {
			return "autostart task: " + a.Error
		}
		return "autostart task: cannot tell (" + a.Error + ")"
	case !a.Exists && wanted:
		return fmt.Sprintf("autostart task: %s is missing; `hyperbutler config set autostart true` creates it again", a.Task)
	case !a.Exists:
		return "autostart task: none"
	case !a.ThisExe:
		return fmt.Sprintf("autostart task: %s runs %s, not this hyperbutler.exe; `hyperbutler config set autostart true` points it here", a.Task, a.Command)
	case !wanted:
		return fmt.Sprintf("autostart task: %s exists although autostart is false; `hyperbutler config set autostart false` removes it", a.Task)
	}
	return fmt.Sprintf("autostart task: %s, at sign-in runs %s", a.Task, a.Command)
}

// applyAutostart creates (on) or removes (off) the task and says what it did. It is
// called before settings.json records the change, so a task that could not be made
// leaves the setting as it was.
func applyAutostart(in *install, on bool, stdout io.Writer) error {
	if home, ok := autostartOurs(in); !ok {
		if on {
			return fmt.Errorf("autostart starts the installation in %s, but this one is %s (HYPERBUTLER_HOME); it is not supported for another state directory", home, in.l.Home)
		}
		// The task is the other installation's: leave it.
		return nil
	}
	name, err := autostartTaskName()
	if err != nil {
		return err
	}
	cur, found, err := scheduler.Query(name)
	if err != nil {
		return fmt.Errorf("autostart: %w", err)
	}
	if !on {
		if !found {
			_, _ = fmt.Fprintf(stdout, "There is no autostart task (%s).\n", name)
			return nil
		}
		if err := scheduler.Delete(name); err != nil {
			return fmt.Errorf("autostart: remove the task: %w", err)
		}
		_, _ = fmt.Fprintf(stdout, "Removed the Task Scheduler task %s.\n", name)
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.Abs(exe); err != nil {
		return err
	}
	want := autostartAction(exe)
	if found && cur == want {
		_, _ = fmt.Fprintf(stdout, "The Task Scheduler task %s already starts %s at sign-in.\n", name, exe)
		return nil
	}
	sid, _, err := currentAccount()
	if err != nil {
		return fmt.Errorf("autostart: %w", err)
	}
	if err := scheduler.Create(name, taskXML(sid, exe)); err != nil {
		return fmt.Errorf("autostart: create the task: %w", err)
	}
	verb := "Created"
	if found {
		verb = "Replaced"
	}
	_, _ = fmt.Fprintf(stdout, "%s the Task Scheduler task %s: 30 s after you sign in it runs\n  %s %s\n"+
		"(no window; Task Scheduler, Task Scheduler Library, lists it). `hyperbutler config set autostart false` removes it.\n", verb, name, want.Command, want.Arguments)
	return nil
}

// planAutostart adds the autostart task to the uninstall plan when it exists. The
// task's name is this user's, so any task of that name is hyperbutler's. It starts the
// installation in %LOCALAPPDATA%\hyperbutler, so the uninstall of another one (with
// HYPERBUTLER_HOME) leaves it.
func planAutostart(in *install, p *uninstallPlan) {
	if _, ok := autostartOurs(in); !ok {
		return
	}
	a := autostartState()
	switch {
	case a.Exists:
		p.add(planned{UninstallItem: UninstallItem{Kind: uninstallTask, Path: a.Task, Detail: "starts hyperbutler at sign-in (" + a.Command + "); removed"}})
	case a.Error != "":
		p.notes = append(p.notes, "Could not look for the autostart task: "+a.Error)
	}
}

// autostartCheck is doctor's line about the task.
func autostartCheck(in *install) DoctorCheck {
	a := autostartFor(in)
	wanted := in.settings != nil && in.settings.Autostart
	c := DoctorCheck{ID: "autostart", Status: doctorInfo, Summary: describeAutostart(a, wanted)}
	switch {
	case a.Exists && wanted && a.ThisExe, !a.Exists && !wanted && a.Error == "":
		c.Status = doctorOK
	case a.Exists && !a.ThisExe, wanted && !a.Exists:
		c.Status, c.Fix = doctorWarn, "hyperbutler config set autostart true (or false)"
		c.FixMsg = "doctor.fix.autostart"
	}
	// The same cases as describeAutostart; an error stays English only.
	switch {
	case a.Error != "" && !a.Exists:
	case !a.Exists && wanted:
		c.Msg, c.Args = "doctor.autostart.missing", msgArgs("task", a.Task)
	case !a.Exists:
		c.Msg = "doctor.autostart.none"
	case !a.ThisExe:
		c.Msg, c.Args = "doctor.autostart.otherExe", msgArgs("task", a.Task, "command", a.Command)
	case !wanted:
		c.Msg, c.Args = "doctor.autostart.unwanted", msgArgs("task", a.Task)
	default:
		c.Msg, c.Args = "doctor.autostart.on", msgArgs("task", a.Task, "command", a.Command)
	}
	return c
}
