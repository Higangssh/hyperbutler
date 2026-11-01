//go:build windows

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/Higangssh/hyperbutler/internal/engine"
	"github.com/Higangssh/hyperbutler/internal/updatecheck"
)

// No test reaches the real Task Scheduler or GitHub: the scheduler is a fake, and the
// update check's client fails every request unless a test gives it a server.
func init() {
	scheduler = &fakeScheduler{}
	newUpdateChecker = func(version string, _ *engine.Settings) *updatecheck.Checker {
		return &updatecheck.Checker{URL: updatecheck.ReleaseURL, Version: version, Client: &http.Client{Transport: failTransport{}}}
	}
}

type failTransport struct{}

func (failTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("no network in tests")
}

// fakeScheduler is a Task Scheduler of one user, in memory.
type fakeScheduler struct {
	mu        sync.Mutex
	tasks     map[string]scheduledTask
	xml       map[string][]byte
	createErr error
}

func (f *fakeScheduler) Query(name string) (scheduledTask, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tasks[name]
	return t, ok, nil
}

func (f *fakeScheduler) Create(name string, x []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return f.createErr
	}
	t, err := parseTaskAction(decodeTaskXML(x))
	if err != nil {
		return err
	}
	if f.tasks == nil {
		f.tasks, f.xml = map[string]scheduledTask{}, map[string][]byte{}
	}
	f.tasks[name], f.xml[name] = t, x
	return nil
}

func (f *fakeScheduler) Delete(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.tasks[name]; !ok {
		return errors.New("no such task")
	}
	delete(f.tasks, name)
	return nil
}

// useFakeScheduler gives the test a fresh fake and makes home the installation a task
// starts.
func useFakeScheduler(t *testing.T, home string) *fakeScheduler {
	t.Helper()
	f := &fakeScheduler{}
	oldS, oldH := scheduler, autostartHome
	scheduler, autostartHome = f, func() (string, error) { return home, nil }
	t.Cleanup(func() { scheduler, autostartHome = oldS, oldH })
	return f
}

// TestSchtasksCommandLines pins what schtasks.exe would be run with, without running it.
func TestSchtasksCommandLines(t *testing.T) {
	exe := `C:\Program Files\hb & co\hyperbutler.exe`
	x := taskXML("S-1-5-21-1-2-3-1001", exe)
	var calls [][]string
	var created []byte
	s := schtasks{run: func(args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		if args[0] == "/Create" {
			b, err := os.ReadFile(args[4])
			if err != nil {
				t.Fatalf("the task XML file: %v", err)
			}
			created = b
		}
		if args[0] == "/Query" {
			return x, nil
		}
		return nil, nil
	}}
	const name = `\hyperbutler autostart (me)`
	if err := s.Create(name, x); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || len(calls[0]) != 6 {
		t.Fatalf("create: %q", calls)
	}
	tmp := calls[0][4]
	if want := []string{"/Create", "/TN", name, "/XML", tmp, "/F"}; !reflect.DeepEqual(calls[0], want) {
		t.Fatalf("create: %q", calls[0])
	}
	if !bytes.Equal(created, x) {
		t.Fatal("the XML file differs from the task XML")
	}
	if _, err := os.Stat(tmp); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the XML file was left behind: %v", err)
	}
	task, found, err := s.Query(name)
	if err != nil || !found || taskExe(task) != exe {
		t.Fatalf("query: %+v %v %v", task, found, err)
	}
	if err := s.Delete(name); err != nil {
		t.Fatal(err)
	}
	if want := [][]string{{"/Query", "/TN", name, "/XML"}, {"/Delete", "/TN", name, "/F"}}; !reflect.DeepEqual(calls[1:], want) {
		t.Fatalf("query, delete: %q", calls[1:])
	}

	// A missing task is exit status 1 with nothing on stdout.
	exit1 := exec.Command("cmd.exe", "/c", "exit 1").Run()
	s = schtasks{run: func(...string) ([]byte, error) { return nil, exit1 }}
	if _, found, err := s.Query(name); found || err != nil {
		t.Fatalf("missing task: %v %v", found, err)
	}
	s = schtasks{run: func(...string) ([]byte, error) { return nil, errors.New("access denied") }}
	if _, _, err := s.Query(name); err == nil {
		t.Fatal("a failing query was taken as no task")
	}
}

// TestTaskXML: the task is this user's logon, 30 s later, least privilege, with no
// window (conhost --headless), and its action points back at exe.
func TestTaskXML(t *testing.T) {
	exe := `C:\Users\me\hb <1>\hyperbutler.exe`
	b := taskXML("S-1-5-21-1-2-3-1001", exe)
	if !bytes.HasPrefix(b, []byte{0xff, 0xfe}) {
		t.Fatal("not UTF-16LE with a byte order mark")
	}
	s := decodeTaskXML(b)
	for _, want := range []string{"<LogonTrigger>", "<UserId>S-1-5-21-1-2-3-1001</UserId>", "<Delay>PT30S</Delay>", "<LogonType>InteractiveToken</LogonType>",
		"<RunLevel>LeastPrivilege</RunLevel>", "<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>", "<DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>",
		`<Arguments>--headless &#34;C:\Users\me\hb &lt;1&gt;\hyperbutler.exe&#34; start</Arguments>`, `conhost.exe</Command>`, "\r\n"} {
		if !strings.Contains(s, want) {
			t.Errorf("task XML lacks %q", want)
		}
	}
	a, err := parseTaskAction(s)
	if err != nil || a != autostartAction(exe) || taskExe(a) != exe {
		t.Fatalf("%+v %v", a, err)
	}
	if !strings.EqualFold(filepath.Base(a.Command), "conhost.exe") || !filepath.IsAbs(a.Command) {
		t.Fatalf("command %q", a.Command)
	}
	for _, other := range []scheduledTask{{Command: `C:\x\hyperbutler.exe`, Arguments: "start"}, {Command: `C:\Windows\System32\conhost.exe`, Arguments: `--headless "x" stop`}} {
		if taskExe(other) != "" {
			t.Errorf("%+v taken as ours", other)
		}
	}
	// 8-bit output decodes too: UTF-8 as it is, other code pages through Windows.
	if got := decodeTaskXML([]byte("\xef\xbb\xbf<Task>정기</Task>")); got != "<Task>정기</Task>" {
		t.Fatalf("%q", got)
	}
	// "C:\Users\홍길동" in CP949, as schtasks writes it to a pipe on a Korean console.
	cp949 := []byte{'C', ':', '\\', 'U', 's', 'e', 'r', 's', '\\', 0xc8, 0xab, 0xb1, 0xe6, 0xb5, 0xbf}
	if got, ok := fromCodePage(949, cp949); !ok || got != `C:\Users\홍길동` {
		t.Fatalf("CP949: %q %v", got, ok)
	}
}

// TestConfigSetAutostart: true creates one task that starts this hyperbutler.exe, list
// shows it, true again changes nothing, false removes it; a task that cannot be created
// leaves settings.json as it was (REQ-UX-015).
func TestConfigSetAutostart(t *testing.T) {
	home, _ := testHome(t)
	f := useFakeScheduler(t, home)
	l := engine.Layout{Home: home}
	code, out, e := runCLI(t, "config", "set", "autostart", "true")
	if code != 0 || !strings.Contains(out, "Created the Task Scheduler task \\hyperbutler autostart (") || !strings.Contains(out, "--headless") {
		t.Fatalf("set true: %d %q %q", code, out, e)
	}
	if len(f.tasks) != 1 {
		t.Fatalf("%d tasks", len(f.tasks))
	}
	if s, err := engine.LoadSettings(l); err != nil || !s.Autostart {
		t.Fatalf("settings %+v %v", s, err)
	}
	code, out, _ = runCLI(t, "config", "list")
	if code != 0 || !strings.Contains(out, "autostart task: \\hyperbutler autostart (") || !strings.Contains(out, "at sign-in runs") {
		t.Fatalf("list: %d %q", code, out)
	}
	code, out, _ = runCLI(t, "config", "list", "--json")
	var lj ConfigListJSON
	if code != 0 || json.Unmarshal([]byte(out), &lj) != nil || lj.Autostart == nil || !lj.Autostart.Exists || !lj.Autostart.ThisExe {
		t.Fatalf("list --json: %d %q", code, out)
	}
	if code, out, _ := runCLI(t, "config", "set", "autostart", "true"); code != 0 || !strings.Contains(out, "already starts") || !strings.Contains(out, "nothing changed") {
		t.Fatalf("set true again: %d %q", code, out)
	}
	// The uninstall plan of this installation lists the task.
	code, out, _ = runCLI(t, "uninstall", "--dry-run", "--json")
	if code != 0 || !strings.Contains(out, `"kind": "scheduled-task"`) {
		t.Fatalf("uninstall --dry-run: %d %q", code, out)
	}
	if code, out, _ := runCLI(t, "config", "set", "autostart", "false"); code != 0 || !strings.Contains(out, "Removed the Task Scheduler task") {
		t.Fatalf("set false: %d %q", code, out)
	}
	if len(f.tasks) != 0 {
		t.Fatal("the task is still there")
	}
	if s, _ := engine.LoadSettings(l); s.Autostart {
		t.Fatal("settings still say autostart")
	}
	f.createErr = errors.New("access denied")
	if code, _, e := runCLI(t, "config", "set", "autostart", "true"); code != 1 || !strings.Contains(e, "access denied") {
		t.Fatalf("failed create: %d %q", code, e)
	}
	if s, _ := engine.LoadSettings(l); s.Autostart {
		t.Fatal("a failed create was recorded")
	}
}

// TestAutostartOtherHome: the task starts the default installation, so another state
// directory can neither create it nor uninstall it.
func TestAutostartOtherHome(t *testing.T) {
	testHome(t)
	f := useFakeScheduler(t, `C:\elsewhere\hyperbutler`)
	if code, _, e := runCLI(t, "config", "set", "autostart", "true"); code != 1 || !strings.Contains(e, "not supported for another state directory") {
		t.Fatalf("%d %q", code, e)
	}
	f.tasks = map[string]scheduledTask{}
	name, _ := autostartTaskName()
	f.tasks[name] = autostartAction(`C:\x\hyperbutler.exe`)
	if code, out, _ := runCLI(t, "uninstall", "--dry-run", "--json"); code != 0 || strings.Contains(out, "scheduled-task") {
		t.Fatalf("uninstall of another installation lists the task: %q", out)
	}
}
