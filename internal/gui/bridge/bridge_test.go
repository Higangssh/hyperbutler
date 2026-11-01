package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
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

const (
	cid   = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	tok32 = "0123456789abcdef0123456789abcdef"
)

func TestDecode(t *testing.T) {
	good := []string{
		`{"id":1,"op":"status","args":{"auto":true}}`,
		`{"id":2,"op":"init"}`,
		`{"id":3,"op":"init","args":null}`,
		`{"id":4,"op":"container.action","args":{"id":"` + cid + `","action":"stop","token":"` + tok32 + `"}}`,
		`{"id":8,"op":"container.action","args":{"id":"` + cid + `","action":"start"}}`,
		`{"id":9,"op":"confirm","args":{"action":"engine.stop"}}`,
		`{"id":10,"op":"confirm","args":{"action":"config.set","key":"hostGateway.ports","value":"8080,9000"}}`,
		`{"id":11,"op":"config.set","args":{"key":"autostart","value":"true","token":"` + tok32 + `"}}`,
		`{"id":5,"op":"container.logs","args":{"id":"` + cid + `"}}`,
		`{"id":6,"op":"config.set","args":{"key":"cpus","value":"2"}}`,
		`{"id":7,"op":"import.run","args":{"ids":["image:busybox:latest"],"token":"` + tok32 + `"}}`,
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
		"no import ids":    `{"id":1,"op":"import.run","args":{"ids":[],"token":"` + tok32 + `"}}`,
		"stop no token":    `{"id":1,"op":"container.action","args":{"id":"` + cid + `","action":"stop"}}`,
		"engine no token":  `{"id":1,"op":"engine.stop"}`,
		"save no token":    `{"id":1,"op":"daemon.save","args":{"text":"{}"}}`,
		"import no token":  `{"id":1,"op":"import.run","args":{"ids":["x"]}}`,
		"bad token":        `{"id":1,"op":"engine.stop","args":{"token":"xyz"}}`,
		"confirm start":    `{"id":1,"op":"confirm","args":{"action":"engine.start"}}`,
		"confirm no id":    `{"id":1,"op":"confirm","args":{"action":"container.stop"}}`,
		"confirm no key":   `{"id":1,"op":"confirm","args":{"action":"config.set"}}`,
		"key on engine":    `{"id":1,"op":"confirm","args":{"action":"engine.stop","key":"cpus"}}`,
		"set bad token":    `{"id":1,"op":"config.set","args":{"key":"autostart","value":"true","token":"x"}}`,
		"bad import id":    `{"id":1,"op":"import.run","args":{"ids":["a b"],"token":"` + tok32 + `"}}`,
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
	f.Add(`{"id":4,"op":"container.action","args":{"id":"` + cid + `","action":"stop","token":"` + tok32 + `"}}`)
	f.Add(`{"id":6,"op":"config.set","args":{"key":"cpus","value":"2"}}`)
	f.Add(`{"id":7,"op":"import.run","args":{"ids":["x"],"token":"` + tok32 + `"}}`)
	f.Add(`{"id":1,"op":"daemon.save","args":{"text":"{}","token":"` + tok32 + `"}}`)
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
	locked    bool // another operation holds the operations lock
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
	if h.locked {
		return "", Codedf(CodeEngineBusy, "another hyperbutler start, stop or delete is still running")
	}
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
func (h *fakeHost) DaemonWrite(_ context.Context, b []byte, check func([]byte, bool) error) error {
	if err := check(h.daemon, h.daemon != nil); err != nil {
		return err
	}
	h.written = b
	return nil
}
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

	// Engine operations need a token from confirm, which carries the REQ-UX-017 list
	// (here the engine is stopped: not running, nothing listed). Output is cleaned.
	c := call(t, d, `{"id":7,"op":"confirm","args":{"action":"engine.stop"}}`)
	cv := c.Data.(ConfirmView)
	if !c.OK || cv.Preview == nil || cv.Preview.Running || cv.Token == "" {
		t.Fatalf("confirm: %+v", c)
	}
	r = call(t, d, `{"id":8,"op":"engine.stop","args":{"token":"`+cv.Token+`"}}`)
	if !r.OK || strings.Contains(r.Data.(OutputView).Output, "\x1b") || len(h.engineOps) != 1 {
		t.Fatalf("engine.stop: %+v %v", r, h.engineOps)
	}
	if r := call(t, d, `{"id":8,"op":"engine.stop","args":{"token":"`+cv.Token+`"}}`); r.OK || r.Error.Code != CodeConfirm || len(h.engineOps) != 1 {
		t.Fatalf("token reused: %+v", r)
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
	if r := call(t, d, `{"id":2,"op":"daemon.save","args":{"text":"{}","token":"`+tokenFor(t, d, "daemon.save", "")+`"}}`); r.OK || r.Error.Code != CodeDaemonReadOnly {
		t.Fatalf("saved over a file with credentials: %+v", r)
	}
	h.daemon = []byte(`{"log-level":"info"}`)
	if r := call(t, d, `{"id":3,"op":"daemon.save","args":{"text":"{\"debug\":true}","token":"`+tokenFor(t, d, "daemon.save", "")+`"}}`); !r.OK || string(h.written) != `{"debug":true}` {
		t.Fatalf("save: %+v %q", r, h.written)
	}
	if r := call(t, d, `{"id":4,"op":"daemon.save","args":{"text":"{","token":"`+tokenFor(t, d, "daemon.save", "")+`"}}`); r.OK || r.Error.Code != CodeDaemonInvalid {
		t.Fatalf("saved invalid JSON: %+v", r)
	}
	if r := call(t, d, `{"id":5,"op":"daemon.validate","args":{"text":"{"}}`); !r.OK || r.Data.(CheckView).OK {
		t.Fatalf("validate: %+v", r)
	}
	// L3: the file is judged as it is when written (under the host's lock), not as the
	// page loaded it: credentials added meanwhile are not overwritten.
	h.daemon, h.written = []byte(`{}`), nil
	tk := tokenFor(t, d, "daemon.save", "")
	h.daemon = []byte(`{"proxies":{"https-proxy":"http://u:secretpw@p:3128"}}`)
	if r := call(t, d, `{"id":6,"op":"daemon.save","args":{"text":"{}","token":"`+tk+`"}}`); r.OK || r.Error.Code != CodeDaemonReadOnly || h.written != nil {
		t.Fatalf("saved over credentials added after the page read the file: %+v", r)
	}
}

// TestBusyLock (L2): a setting while another operation holds the lock comes back at
// once with the busy code, which the page explains.
func TestBusyLock(t *testing.T) {
	h := &fakeHost{locked: true}
	d := New(Options{Host: h})
	if r := call(t, d, `{"id":1,"op":"config.set","args":{"key":"cpus","value":"3"}}`); r.OK || r.Error.Code != CodeEngineBusy {
		t.Fatalf("set while locked: %+v", r)
	}
}

// TestEditableCoversEveryKey fails until a new setting is classified: changed in the
// window freely, changed there after a confirmation, or CLI-only.
func TestEditableCoversEveryKey(t *testing.T) {
	for _, k := range config.Keys {
		a, ok := Editable[k.Name]
		if !ok {
			t.Errorf("setting %s is not in Editable: decide whether the window may change it freely, after a confirmation, or not at all (docs/gui.md)", k.Name)
			continue
		}
		if a != Free && a != Confirmed && a != CLIOnly {
			t.Errorf("setting %s: access %d is none of Free, Confirmed, CLIOnly", k.Name, a)
		}
	}
	for k := range Editable {
		if _, err := config.Lookup(k); err != nil {
			t.Errorf("Editable lists %s, which is not a setting", k)
		}
	}
	// The ones the review found a page could flip silently need a confirmation.
	for _, k := range []string{"hostGateway.enabled", "hostGateway.ports", "autostart", "updateCheck", "context.manage"} {
		if Editable[k] != Confirmed {
			t.Errorf("%s: want Confirmed", k)
		}
	}
}

// TestConfirmedSettings: a Confirmed setting changes only with a token for that key and
// that value; a Free one needs none; a CLI-only one gets no token.
func TestConfirmedSettings(t *testing.T) {
	h := &fakeHost{}
	d := New(Options{Host: h})
	set := func(key, value, token string) Response {
		msg := `{"id":1,"op":"config.set","args":{"key":"` + key + `","value":"` + value + `"`
		if token != "" {
			msg += `,"token":"` + token + `"`
		}
		return call(t, d, msg+`}}`)
	}
	confirm := func(key, value string) Response {
		return call(t, d, `{"id":2,"op":"confirm","args":{"action":"config.set","key":"`+key+`","value":"`+value+`"}}`)
	}
	if r := set("hostGateway.enabled", "true", ""); r.OK || r.Error.Code != CodeConfirm {
		t.Fatalf("hostGateway.enabled without a token: %+v", r)
	}
	c := confirm("hostGateway.enabled", "true")
	if !c.OK {
		t.Fatalf("confirm: %+v", c)
	}
	tk := c.Data.(ConfirmView).Token
	// The token is for that value: another one is refused (and the token spent).
	if r := set("hostGateway.enabled", "false", tk); r.OK || r.Error.Code != CodeConfirm {
		t.Fatalf("token for another value: %+v", r)
	}
	if r := set("hostGateway.enabled", "true", tk); r.OK {
		t.Fatal("a spent token was still good")
	}
	// The key is matched as config.Lookup names it.
	tk = confirm("HOSTGATEWAY.PORTS", "8080").Data.(ConfirmView).Token
	if r := set("hostGateway.ports", "8080", tk); !r.OK {
		t.Fatalf("hostGateway.ports with its token: %+v", r)
	}
	if r := set("cpus", "3", ""); !r.OK {
		t.Fatalf("cpus: %+v", r)
	}
	if r := confirm("cpus", "3"); r.OK || r.Error.Code != CodeNotEditable {
		t.Fatalf("a token for a free setting: %+v", r)
	}
	if r := confirm("pipe", `x`); r.OK || r.Error.Code != CodeNotEditable {
		t.Fatalf("a token for a CLI-only setting: %+v", r)
	}
	if want := []string{"hostGateway.ports=8080", "cpus=3"}; strings.Join(h.sets, " ") != strings.Join(want, " ") {
		t.Fatalf("sets %v, want %v", h.sets, want)
	}
	// The settings view says which ones ask first.
	for _, it := range MakeSettings(schema.ConfigList{Keys: []config.Item{{Key: "autostart"}, {Key: "cpus"}, {Key: "pipe"}}}).Keys {
		if want := it.Key == "autostart"; it.Confirm != want || it.Editable != (it.Key != "pipe") {
			t.Errorf("%s: editable %v confirm %v", it.Key, it.Editable, it.Confirm)
		}
	}
}

// TestBusyKeepsToken: a request refused as busy has not spent its token.
func TestBusyKeepsToken(t *testing.T) {
	h := &fakeHost{}
	d := New(Options{Host: h})
	for _, c := range []struct{ action, id, msg string }{
		{"engine.stop", "", `{"id":1,"op":"engine.stop","args":{"token":"%s"}}`},
		{"container.stop", cid, `{"id":1,"op":"container.action","args":{"id":"` + cid + `","action":"stop","token":"%s"}}`},
		{"daemon.save", "", `{"id":1,"op":"daemon.save","args":{"text":"{}","token":"%s"}}`},
	} {
		tk := tokenFor(t, d, c.action, c.id)
		msg := strings.Replace(c.msg, "%s", tk, 1)
		d.mutating.Lock() // another change runs
		r := call(t, d, msg)
		d.mutating.Unlock()
		if r.OK || r.Error.Code != CodeBusy {
			t.Fatalf("%s while busy: %+v", c.action, r)
		}
		r = call(t, d, msg)
		if r.Error != nil && r.Error.Code == CodeConfirm {
			t.Fatalf("%s: the busy refusal spent the token: %+v", c.action, r.Error)
		}
	}
	tk := confirmSetting(t, d, "autostart", "true")
	d.mutating.Lock()
	r := call(t, d, `{"id":1,"op":"config.set","args":{"key":"autostart","value":"true","token":"`+tk+`"}}`)
	d.mutating.Unlock()
	if r.OK || r.Error.Code != CodeBusy {
		t.Fatalf("config.set while busy: %+v", r)
	}
	if r := call(t, d, `{"id":1,"op":"config.set","args":{"key":"autostart","value":"true","token":"`+tk+`"}}`); !r.OK {
		t.Fatalf("config.set after busy: %+v", r)
	}
	// Tokens live TokenTTL, ten minutes.
	now := time.Now()
	d.tok.now = func() time.Time { return now }
	tk = tokenFor(t, d, "engine.restart", "")
	now = now.Add(9 * time.Minute)
	if r := call(t, d, `{"id":1,"op":"engine.restart","args":{"token":"`+tk+`"}}`); !r.OK {
		t.Fatalf("a nine-minute-old token: %+v", r)
	}
}

func confirmSetting(t *testing.T, d *Dispatcher, key, value string) string {
	t.Helper()
	r := call(t, d, `{"id":2,"op":"confirm","args":{"action":"config.set","key":"`+key+`","value":"`+value+`"}}`)
	if !r.OK {
		t.Fatalf("confirm %s: %+v", key, r)
	}
	return r.Data.(ConfirmView).Token
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
type fakeImporter struct {
	got     chan []string
	noSpace bool
}

func (f *fakeImporter) Check(context.Context, []string) error {
	if f.noSpace {
		return Codedf(CodeImportSpace, "not enough space")
	}
	return nil
}

func (f *fakeImporter) Plan(context.Context) (ImportPlan, error) {
	return ImportPlan{Items: []ImportItem{{ID: "image:a", Kind: "image", Name: "a", Status: ImportNew},
		{ID: "image:b", Kind: "image", Name: "b", Status: ImportDone, Detail: DetailImageAlready, Note: "in hyperbutler with the same id and tags"},
		{ID: "image:c", Kind: "image", Name: "c", Status: ImportConflict, Detail: DetailImageConflict, Args: map[string][]string{"tags": {"c:1‮", "c:2"}, "Bad Arg": {"x"}}, Note: "hyperbutler has c:1, c:2 as another image"},
		{ID: "volume:v", Kind: "volume", Name: "v", Status: "some-new-state", Detail: "import.detail.made.up", Note: "a reason"}},
		Notes: []string{"an interrupted import left a helper"}}, nil
}

func (f *fakeImporter) Run(_ context.Context, ids []string, progress func(ImportProgress)) (ImportResult, error) {
	progress(ImportProgress{Running: true, Total: len(ids)})
	f.got <- ids
	return ImportResult{Imported: len(ids), SourceUnchanged: true}, nil
}

func TestImport(t *testing.T) {
	imp := &fakeImporter{got: make(chan []string, 1)}
	events := make(chan ImportProgress, 4)
	d := New(Options{Host: &fakeHost{}, Importer: imp, Notify: func(_ string, p any) { events <- p.(ImportProgress) }})
	if r := call(t, d, `{"id":1,"op":"import.run","args":{"ids":["image:a"],"token":"`+tokenFor(t, d, "import.run", "")+`"}}`); r.OK {
		t.Fatal("ran without a plan")
	}
	pr := call(t, d, `{"id":2,"op":"import.plan"}`)
	if !pr.OK {
		t.Fatalf("plan: %+v", pr)
	}
	// The import's English reasons reach the page only as refs; details are catalog keys
	// with cleaned arguments; an unknown status or detail becomes one the page knows.
	pv := pr.Data.(ImportPlanView)
	if b, _ := json.Marshal(pv); strings.Contains(string(b), "same id and tags") || strings.Contains(string(b), "interrupted") {
		t.Fatalf("English reasons sent to the page: %s", b)
	}
	byID := map[string]ImportItem{}
	for _, it := range pv.Items {
		byID[it.ID] = it
	}
	if it := byID["image:c"]; it.Detail != DetailImageConflict || len(it.Args) != 1 || strings.ContainsRune(strings.Join(it.Args["tags"], ""), 0x202e) || it.NoteRef == 0 {
		t.Fatalf("conflict item %+v", it)
	}
	if it := byID["volume:v"]; it.Status != ImportAttention || it.Detail != DetailOther {
		t.Fatalf("unknown item %+v", it)
	}
	if it := byID["image:a"]; it.Detail != "" || it.NoteRef != 0 {
		t.Fatalf("new item %+v", it)
	}
	h := d.o.Host.(*fakeHost)
	if r := call(t, d, `{"id":9,"op":"error.copy","args":{"ref":`+jsonInt(byID["image:b"].NoteRef)+`}}`); !r.OK || h.copied != "in hyperbutler with the same id and tags" {
		t.Fatalf("copy the English original: %+v %q", r, h.copied)
	}
	if call(t, d, `{"id":9,"op":"error.copy","args":{"ref":`+jsonInt(pv.NotesRef)+`}}`); pv.Notes != 1 || !strings.Contains(h.copied, "interrupted") {
		t.Fatalf("plan notes: %d %q", pv.Notes, h.copied)
	}
	if r := call(t, d, `{"id":3,"op":"import.run","args":{"ids":["image:b"],"token":"`+tokenFor(t, d, "import.run", "")+`"}}`); r.OK {
		t.Fatal("imported an item that is already imported")
	}
	imp.noSpace = true
	// Busy (an import runs) keeps the token; the space refusal comes after it.
	tk := tokenFor(t, d, "import.run", "")
	d.imp.cancel = func() {}
	if r := call(t, d, `{"id":5,"op":"import.run","args":{"ids":["image:a"],"token":"`+tk+`"}}`); r.OK || r.Error.Code != CodeBusy {
		t.Fatalf("import while one runs: %+v", r)
	}
	d.imp.cancel = nil
	if r := call(t, d, `{"id":5,"op":"import.run","args":{"ids":["image:a"],"token":"`+tk+`"}}`); r.OK || r.Error.Code != CodeImportSpace {
		t.Fatalf("ran without space: %+v", r)
	}
	// force is import --force-space: the check is skipped.
	if r := call(t, d, `{"id":4,"op":"import.run","args":{"ids":["image:a","image:b","image:zzz"],"force":true,"token":"`+tokenFor(t, d, "import.run", "")+`"}}`); !r.OK {
		t.Fatalf("run: %+v", r)
	}
	if got := <-imp.got; len(got) != 1 || got[0] != "image:a" {
		t.Fatalf("ran %v", got)
	}
	for p := range events {
		if !p.Running {
			if p.Done != 1 || p.Error != nil || p.Result == nil || !p.Result.SourceUnchanged {
				t.Fatalf("final %+v", p)
			}
			break
		}
	}
}

// slowImporter's Run blocks until it is canceled, then takes a while to clean up (as
// the import undoes its item in progress and checks Docker Desktop), then sets cleaned.
type slowImporter struct {
	fakeImporter
	started chan struct{}
	cleaned atomic.Bool
}

func (f *slowImporter) Run(ctx context.Context, _ []string, progress func(ImportProgress)) (ImportResult, error) {
	progress(ImportProgress{Running: true, Current: "evil‮exe.gpj"})
	close(f.started)
	<-ctx.Done()
	time.Sleep(300 * time.Millisecond)
	f.cleaned.Store(true)
	return ImportResult{}, ctx.Err()
}

// TestImportOutlivesWindow (H1): closing the window does not cancel an import under
// its cleanup; CancelImport cancels it and Wait returns only after Run did.
func TestImportOutlivesWindow(t *testing.T) {
	imp := &slowImporter{started: make(chan struct{})}
	window, closeWindow := context.WithCancel(context.Background())
	events := make(chan ImportProgress, 8)
	d := New(Options{Host: &fakeHost{}, Importer: imp, Context: window, Notify: func(_ string, p any) { events <- p.(ImportProgress) }})
	if r := call(t, d, `{"id":1,"op":"import.plan"}`); !r.OK {
		t.Fatalf("plan: %+v", r)
	}
	if r := call(t, d, `{"id":2,"op":"import.run","args":{"ids":["image:a"],"token":"`+tokenFor(t, d, "import.run", "")+`"}}`); !r.OK {
		t.Fatalf("run: %+v", r)
	}
	<-imp.started
	// M4: the item name is cleaned before it reaches the page.
	if p := <-events; strings.ContainsRune(p.Current, 0x202e) || !strings.Contains(p.Current, "evil") {
		t.Fatalf("progress name %q", p.Current)
	}
	if !d.ImportRunning() {
		t.Fatal("no import running")
	}
	// A plan while it runs would replace its importer.
	if r := call(t, d, `{"id":3,"op":"import.plan"}`); r.OK || r.Error.Code != CodeBusy {
		t.Fatalf("plan during an import: %+v", r)
	}
	closeWindow()
	if d.WaitImport(200*time.Millisecond) || d.Wait(10*time.Millisecond) {
		t.Fatal("Wait returned while the import still runs")
	}
	if !d.ImportRunning() {
		t.Fatal("closing the window canceled the import")
	}
	d.CancelImport()
	if !d.WaitImport(5 * time.Second) {
		t.Fatal("Wait timed out")
	}
	if !imp.cleaned.Load() {
		t.Fatal("Wait returned before the import's cleanup ended")
	}
	if p := <-events; p.Running || !p.Canceled {
		t.Fatalf("final event %+v", p)
	}
}

// tokenFor asks the dispatcher for a confirmation token.
func tokenFor(t *testing.T, d *Dispatcher, action, id string) string {
	t.Helper()
	msg := `{"id":99,"op":"confirm","args":{"action":"` + action + `"`
	if id != "" {
		msg += `,"id":"` + id + `"`
	}
	r := call(t, d, msg+`}}`)
	if !r.OK {
		t.Fatalf("confirm %s: %+v", action, r)
	}
	return r.Data.(ConfirmView).Token
}

// TestTokens: the host enforces the confirmation of destructive operations. A token is
// for one action (and container), once, for TokenTTL.
func TestTokens(t *testing.T) {
	h := &fakeHost{}
	d := New(Options{Host: h})
	now := time.Now()
	d.tok.now = func() time.Time { return now }
	other := strings.Repeat("b", 64)

	// Unknown token.
	if r := call(t, d, `{"id":1,"op":"engine.restart","args":{"token":"`+tok32+`"}}`); r.OK || r.Error.Code != CodeConfirm {
		t.Fatalf("unknown token: %+v", r)
	}
	// A token for another action.
	tk := tokenFor(t, d, "engine.stop", "")
	if r := call(t, d, `{"id":2,"op":"engine.restartDockerd","args":{"token":"`+tk+`"}}`); r.OK || r.Error.Code != CodeConfirm {
		t.Fatalf("token for another action: %+v", r)
	}
	// The mismatch spent it.
	if r := call(t, d, `{"id":3,"op":"engine.stop","args":{"token":"`+tk+`"}}`); r.OK {
		t.Fatal("a refused token was still good")
	}
	// A container token is for that container.
	tk = tokenFor(t, d, "container.stop", cid)
	if r := call(t, d, `{"id":4,"op":"container.action","args":{"id":"`+other+`","action":"stop","token":"`+tk+`"}}`); r.OK || r.Error.Code != CodeConfirm {
		t.Fatalf("container token on another container: %+v", r)
	}
	// Expired.
	tk = tokenFor(t, d, "engine.restart", "")
	now = now.Add(TokenTTL + time.Second)
	if r := call(t, d, `{"id":5,"op":"engine.restart","args":{"token":"`+tk+`"}}`); r.OK || r.Error.Code != CodeConfirm {
		t.Fatalf("expired token: %+v", r)
	}
	if len(h.engineOps) != 0 {
		t.Fatalf("an engine operation ran without a valid token: %v", h.engineOps)
	}
	// A good one runs, once.
	tk = tokenFor(t, d, "engine.restart", "")
	if r := call(t, d, `{"id":6,"op":"engine.restart","args":{"token":"`+tk+`"}}`); !r.OK || len(h.engineOps) != 1 {
		t.Fatalf("good token: %+v %v", r, h.engineOps)
	}
	// Starting needs none.
	if r := call(t, d, `{"id":7,"op":"engine.start"}`); !r.OK {
		t.Fatalf("start: %+v", r)
	}
	// Outstanding tokens are bounded.
	for range maxTokens + 5 {
		tokenFor(t, d, "daemon.save", "")
	}
	d.tok.mu.Lock()
	n := len(d.tok.m)
	d.tok.mu.Unlock()
	if n > maxTokens {
		t.Fatalf("%d tokens outstanding", n)
	}
}
