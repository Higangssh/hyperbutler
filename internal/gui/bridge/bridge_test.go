package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Higangssh/hyperbutler/internal/config"
	"github.com/Higangssh/hyperbutler/internal/dockerapi"
	"github.com/Higangssh/hyperbutler/internal/engine"
	"github.com/Higangssh/hyperbutler/internal/leaktest"
	"github.com/Higangssh/hyperbutler/internal/schema"
)

// TestMain fails the package's tests if any goroutine is still running after
// they finish (ADR 0003, guardrail 1).
func TestMain(m *testing.M) {
	leaktest.VerifyTestMain(m)
}

const cid = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestDecode(t *testing.T) {
	good := []string{
		`{"id":1,"op":"status","args":{"auto":true}}`,
		`{"id":2,"op":"init"}`,
		`{"id":3,"op":"init","args":null}`,
		`{"id":4,"op":"container.action","args":{"id":"` + cid + `","action":"stop"}}`,
		`{"id":5,"op":"container.logs","args":{"id":"` + cid + `"}}`,
		`{"id":6,"op":"config.set","args":{"key":"cpus","value":"2"}}`,
		`{"id":7,"op":"import.run","args":{"ids":["image:busybox:latest"]}}`,
	}
	for _, m := range good {
		if _, _, err := Decode(m); err != nil {
			t.Errorf("%s: %v", m, err)
		}
	}
	_, a, _ := Decode(`{"id":5,"op":"container.logs","args":{"id":"` + cid + `"}}`)
	if a.(*LogsArgs).Tail != DefaultTail {
		t.Errorf("default tail %d", a.(*LogsArgs).Tail)
	}
	bad := map[string]string{
		"not json":         `{"id":1,"op":`,
		"no id":            `{"op":"status"}`,
		"negative id":      `{"id":-1,"op":"status"}`,
		"huge id":          `{"id":9007199254740993,"op":"status"}`,
		"unknown op":       `{"id":1,"op":"exec"}`,
		"unknown field":    `{"id":1,"op":"status","cmd":"x"}`,
		"unknown arg":      `{"id":1,"op":"status","args":{"auto":true,"path":"C:\\"}}`,
		"trailing":         `{"id":1,"op":"status"}{"id":2,"op":"status"}`,
		"name not id":      `{"id":1,"op":"container.action","args":{"id":"web","action":"stop"}}`,
		"path in id":       `{"id":1,"op":"container.action","args":{"id":"../../images/x","action":"stop"}}`,
		"remove":           `{"id":1,"op":"container.action","args":{"id":"` + cid + `","action":"remove"}}`,
		"kill":             `{"id":1,"op":"container.action","args":{"id":"` + cid + `","action":"kill"}}`,
		"tail too big":     `{"id":1,"op":"container.logs","args":{"id":"` + cid + `","tail":100000}}`,
		"newline in value": `{"id":1,"op":"config.set","args":{"key":"cpus","value":"2\n--apply"}}`,
		"bad key":          `{"id":1,"op":"config.set","args":{"key":"../x","value":"1"}}`,
		"big daemon.json":  `{"id":1,"op":"daemon.save","args":{"text":"` + strings.Repeat("a", MaxDaemonJSON+1) + `"}}`,
		"no import ids":    `{"id":1,"op":"import.run","args":{"ids":[]}}`,
		"bad import id":    `{"id":1,"op":"import.run","args":{"ids":["a b"]}}`,
		"bad ref":          `{"id":1,"op":"error.copy","args":{"ref":0}}`,
		"too big":          `{"id":1,"op":"log","args":{"text":"` + strings.Repeat("a", MaxMessage) + `"}}`,
		"not utf-8":        "{\"id\":1,\"op\":\"status\",\"x\":\"\xff\"}",
	}
	for name, m := range bad {
		if _, _, err := Decode(m); !errors.Is(err, ErrBadRequest) {
			t.Errorf("%s: accepted (%v)", name, err)
		}
	}
	// A refused message still carries its id when it has one, so it can be answered.
	if r, _, err := Decode(`{"id":9,"op":"exec"}`); err == nil || r.ID != 9 {
		t.Errorf("refused message: id %d, %v", r.ID, err)
	}
}

// FuzzDecode: no message panics the decoder, and whatever it accepts is a known
// operation with checked arguments.
func FuzzDecode(f *testing.F) {
	f.Add(`{"id":1,"op":"status","args":{"auto":true}}`)
	f.Add(`{"id":4,"op":"container.action","args":{"id":"` + cid + `","action":"stop"}}`)
	f.Add(`{"id":6,"op":"config.set","args":{"key":"cpus","value":"2"}}`)
	f.Add(`{"id":7,"op":"import.run","args":{"ids":["x"]}}`)
	f.Add(`{"id":1,"op":"daemon.save","args":{"text":"{}"}}`)
	f.Fuzz(func(t *testing.T, msg string) {
		r, args, err := Decode(msg)
		if err != nil {
			return
		}
		if _, ok := argTypes[r.Op]; !ok || args == nil || r.ID <= 0 {
			t.Fatalf("accepted %q as %+v", msg, r)
		}
		switch a := args.(type) {
		case *ContainerArgs:
			if !reContainerID.MatchString(a.ID) || !containerActions[a.Action] {
				t.Fatalf("accepted container args %+v", a)
			}
		case *LogsArgs:
			if a.Tail < 1 || a.Tail > MaxTail {
				t.Fatalf("accepted tail %d", a.Tail)
			}
		case *ConfigSetArgs:
			if strings.ContainsAny(a.Value, "\x00\n\r") || len(a.Value) > maxValue {
				t.Fatalf("accepted value %q", a.Value)
			}
		}
	})
}

// fakeHost is a Host for the dispatcher's tests.
type fakeHost struct {
	status    schema.Status
	sets      []string
	engineOps []EngineOp
	daemon    []byte
	written   []byte
	revealed  string
	copied    string
}

func (h *fakeHost) Status(context.Context) (schema.Status, error) { return h.status, nil }
func (h *fakeHost) Docker(context.Context) (*dockerapi.Client, error) {
	return nil, ErrEngineStopped
}

func (h *fakeHost) Engine(_ context.Context, op EngineOp) (string, error) {
	h.engineOps = append(h.engineOps, op)
	return "done\x1b[2J", nil
}

func (h *fakeHost) ConfigList(context.Context) (schema.ConfigList, error) {
	return schema.ConfigList{Keys: []config.Item{{Key: "cpus", Value: "2", Apply: config.ApplyVM},
		{Key: "proxy.http", Value: "http://user:pass@proxy:3128", Apply: config.ApplyDockerd}}}, nil
}

func (h *fakeHost) ConfigSet(_ context.Context, k, v string) (string, error) {
	h.sets = append(h.sets, k+"="+v)
	return "ok", nil
}

func (h *fakeHost) DaemonRead(context.Context) (string, []byte, bool, error) {
	return `C:\h\daemon.json`, h.daemon, h.daemon != nil, nil
}
func (h *fakeHost) DaemonCheck(b []byte) error {
	if !json.Valid(b) {
		return errors.New("not JSON")
	}
	return nil
}
func (h *fakeHost) DaemonWrite(_ context.Context, b []byte) error { h.written = b; return nil }
func (h *fakeHost) Doctor(context.Context) (schema.Doctor, error) {
	return schema.Doctor{OK: true, Checks: []schema.DoctorCheck{{ID: "pipe", Status: "weird", Summary: "a\u202eb"}}}, nil
}
func (h *fakeHost) Diagnose(context.Context) (schema.Diagnose, error) {
	return schema.Diagnose{File: `C:\t\d.zip`, Files: []string{"a"}}, nil
}
func (h *fakeHost) Reveal(p string) error   { h.revealed = p; return nil }
func (h *fakeHost) CopyText(s string) error { h.copied = s; return nil }

func call(t *testing.T, d *Dispatcher, msg string) Response {
	t.Helper()
	return d.Handle(context.Background(), msg)
}

func TestDispatcher(t *testing.T) {
	h := &fakeHost{status: schema.Status{SetUp: true}}
	visible := true
	d := New(Options{Host: h, Lang: "en", Messages: map[string]string{"a": "b"}, Visible: func() bool { return visible }})

	if r := call(t, d, `{"id":1,"op":"status","args":{"auto":true}}`); !r.OK || r.Data.(StatusView).Phase != "stopped" {
		t.Fatalf("status: %+v", r)
	}
	visible = false
	if r := call(t, d, `{"id":2,"op":"status","args":{"auto":true}}`); r.OK || r.Error.Code != CodeHidden {
		t.Fatalf("hidden auto refresh: %+v", r)
	}
	if r := call(t, d, `{"id":3,"op":"status","args":{"auto":false}}`); !r.OK {
		t.Fatalf("hidden user refresh: %+v", r)
	}
	visible = true

	// Settings: an editable key goes through; a secret or path does not.
	if r := call(t, d, `{"id":4,"op":"config.set","args":{"key":"cpus","value":"3"}}`); !r.OK {
		t.Fatalf("set cpus: %+v", r)
	}
	for _, k := range []string{"proxy.http", "pipe", "data.dir", "ca.extraFile"} {
		if r := call(t, d, `{"id":5,"op":"config.set","args":{"key":"`+k+`","value":"x"}}`); r.OK || r.Error.Code != CodeNotEditable {
			t.Errorf("set %s: %+v", k, r)
		}
	}
	if len(h.sets) != 1 || h.sets[0] != "cpus=3" {
		t.Fatalf("sets %v", h.sets)
	}
	r := call(t, d, `{"id":6,"op":"config.list"}`)
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), "pass@") {
		t.Fatalf("config.list leaked a password: %s", b)
	}

	// Engine operations, output cleaned.
	r = call(t, d, `{"id":7,"op":"engine.stop"}`)
	if !r.OK || strings.Contains(r.Data.(OutputView).Output, "\x1b") || len(h.engineOps) != 1 {
		t.Fatalf("engine.stop: %+v %v", r, h.engineOps)
	}
	// Preview with the engine stopped: not running, nothing listed.
	if r := call(t, d, `{"id":8,"op":"engine.preview"}`); !r.OK || r.Data.(PreviewView).Running {
		t.Fatalf("preview: %+v", r)
	}
	// A Docker operation with the engine stopped says so, with its code.
	if r := call(t, d, `{"id":9,"op":"containers","args":{}}`); r.OK || r.Error.Code != CodeEngineStopped {
		t.Fatalf("containers: %+v", r)
	}

	// Errors keep their English original for error.copy; the page never sends text.
	bad := call(t, d, `{"id":10,"op":"nope"}`)
	if bad.OK || bad.Error.Code != CodeBadRequest || bad.Error.Ref == 0 {
		t.Fatalf("bad op: %+v", bad)
	}
	if r := call(t, d, `{"id":11,"op":"error.copy","args":{"ref":`+jsonInt(bad.Error.Ref)+`}}`); !r.OK || !strings.HasPrefix(h.copied, CodeBadRequest) {
		t.Fatalf("copy: %+v %q", r, h.copied)
	}
	if r := call(t, d, `{"id":12,"op":"error.copy","args":{"ref":999}}`); r.OK {
		t.Fatal("copied an unknown ref")
	}

	// Reveal: only after a diagnose, and only its zip.
	if r := call(t, d, `{"id":13,"op":"diagnose.reveal"}`); r.OK {
		t.Fatal("revealed without a diagnose")
	}
	call(t, d, `{"id":14,"op":"diagnose"}`)
	if r := call(t, d, `{"id":15,"op":"diagnose.reveal"}`); !r.OK || h.revealed != `C:\t\d.zip` {
		t.Fatalf("reveal: %+v %q", r, h.revealed)
	}

	// Doctor: unknown statuses become info, text is cleaned.
	r = call(t, d, `{"id":16,"op":"doctor"}`)
	if c := r.Data.(DoctorView).Checks[0]; c.Status != "info" || strings.ContainsRune(c.Summary, 0x202e) {
		t.Fatalf("doctor: %+v", c)
	}

	// The import is not in this build: it says so.
	if r := call(t, d, `{"id":17,"op":"import.plan"}`); r.OK || r.Error.Code != CodeImportUnavailable {
		t.Fatalf("import.plan: %+v", r)
	}
}

func jsonInt(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestDaemonJSON(t *testing.T) {
	h := &fakeHost{daemon: []byte(`{"proxies":{"http-proxy":"http://u:secretpw@p:3128"}}`)}
	d := New(Options{Host: h})
	r := call(t, d, `{"id":1,"op":"daemon.get"}`)
	v := r.Data.(DaemonView)
	if !v.ReadOnly || strings.Contains(v.Text, "secretpw") {
		t.Fatalf("daemon.get with credentials: %+v", v)
	}
	if r := call(t, d, `{"id":2,"op":"daemon.save","args":{"text":"{}"}}`); r.OK || r.Error.Code != CodeDaemonReadOnly {
		t.Fatalf("saved over a file with credentials: %+v", r)
	}
	h.daemon = []byte(`{"log-level":"info"}`)
	if r := call(t, d, `{"id":3,"op":"daemon.save","args":{"text":"{\"debug\":true}"}}`); !r.OK || string(h.written) != `{"debug":true}` {
		t.Fatalf("save: %+v %q", r, h.written)
	}
	if r := call(t, d, `{"id":4,"op":"daemon.save","args":{"text":"{"}}`); r.OK || r.Error.Code != CodeDaemonInvalid {
		t.Fatalf("saved invalid JSON: %+v", r)
	}
	if r := call(t, d, `{"id":5,"op":"daemon.validate","args":{"text":"{"}}`); !r.OK || r.Data.(CheckView).OK {
		t.Fatalf("validate: %+v", r)
	}
}

// TestEditableCoversEveryKey fails until a new setting is classified as changeable in
// the window or not.
func TestEditableCoversEveryKey(t *testing.T) {
	for _, k := range config.Keys {
		if _, ok := Editable[k.Name]; !ok {
			t.Errorf("setting %s is not in Editable: decide whether the window may change it (docs/gui.md)", k.Name)
		}
	}
	for k := range Editable {
		if _, err := config.Lookup(k); err != nil {
			t.Errorf("Editable lists %s, which is not a setting", k)
		}
	}
}

func TestMakeContainers(t *testing.T) {
	id2 := strings.Repeat("b", 64)
	list := []dockerapi.Container{
		{ID: cid, Names: []string{"/web\u202e<img src=x onerror=alert(1)>"}, Image: "nginx", State: "running",
			Labels: map[string]string{dockerapi.LabelComposeProject: "shop", dockerapi.LabelComposeService: "web"}},
		{ID: id2, Names: []string{"/solo"}, Image: "busybox", State: "exited", Labels: map[string]string{dockerapi.LabelComposeProject: "bad project!"}},
		{ID: "short", Names: []string{"/x"}},
	}
	v := MakeContainers(list)
	if len(v.Groups) != 2 || v.Groups[0].Project != "shop" || v.Groups[1].Project != "" || v.Running != 1 {
		t.Fatalf("groups %+v", v)
	}
	name := v.Groups[0].Containers[0].Name
	if strings.ContainsRune(name, 0x202e) {
		t.Fatalf("bidi override kept: %q", name)
	}
	// Markup stays as text: the page shows it with textContent (and the CSP's Trusted
	// Types would refuse it as HTML).
	if !strings.Contains(name, "<img") {
		t.Fatalf("name changed: %q", name)
	}
}

func TestMakeStatus(t *testing.T) {
	st := &engine.State{Phase: engine.PhaseFailed, Error: "boom token=abcdef0123456789", ErrorCode: engine.CodePipeInUse}
	v := MakeStatus(schema.Status{SetUp: true, State: st})
	if v.Phase != "failed" || v.Error == nil || v.Error.Code != CodePipeInUse || strings.Contains(v.Error.Detail, "abcdef0123456789") {
		t.Fatalf("failed status: %+v %+v", v, v.Error)
	}
	run := &engine.State{Phase: engine.PhaseRunning, Started: time.Now().Add(-time.Minute)}
	if v := MakeStatus(schema.Status{SetUp: true, Running: true, State: run}); v.Phase != "running" || v.UpSeconds < 59 {
		t.Fatalf("running status: %+v", v)
	}
	if v := MakeStatus(schema.Status{}); v.Phase != "notsetup" {
		t.Fatalf("not set up: %+v", v)
	}
}

// fakeImporter checks that import.run passes only planned, not imported items.
type fakeImporter struct{ got chan []string }

func (f *fakeImporter) Plan(context.Context) ([]ImportItem, error) {
	return []ImportItem{{ID: "image:a", Kind: "image", Name: "a", Status: ImportNew}, {ID: "image:b", Kind: "image", Name: "b", Status: ImportDone}}, nil
}

func (f *fakeImporter) Run(_ context.Context, ids []string, progress func(ImportProgress)) error {
	progress(ImportProgress{Running: true, Total: len(ids)})
	f.got <- ids
	return nil
}

func TestImport(t *testing.T) {
	imp := &fakeImporter{got: make(chan []string, 1)}
	events := make(chan ImportProgress, 4)
	d := New(Options{Host: &fakeHost{}, Importer: imp, Notify: func(_ string, p any) { events <- p.(ImportProgress) }})
	if r := call(t, d, `{"id":1,"op":"import.run","args":{"ids":["image:a"]}}`); r.OK {
		t.Fatal("ran without a plan")
	}
	if r := call(t, d, `{"id":2,"op":"import.plan"}`); !r.OK {
		t.Fatalf("plan: %+v", r)
	}
	if r := call(t, d, `{"id":3,"op":"import.run","args":{"ids":["image:b"]}}`); r.OK {
		t.Fatal("imported an item that is already imported")
	}
	if r := call(t, d, `{"id":4,"op":"import.run","args":{"ids":["image:a","image:b","image:zzz"]}}`); !r.OK {
		t.Fatalf("run: %+v", r)
	}
	if got := <-imp.got; len(got) != 1 || got[0] != "image:a" {
		t.Fatalf("ran %v", got)
	}
	for p := range events {
		if !p.Running {
			if p.Done != 1 || p.Error != nil {
				t.Fatalf("final %+v", p)
			}
			break
		}
	}
}
