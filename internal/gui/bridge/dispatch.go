package bridge

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Higangssh/hyperbutler/internal/config"
	"github.com/Higangssh/hyperbutler/internal/dockerapi"
	"github.com/Higangssh/hyperbutler/internal/redact"
	"github.com/Higangssh/hyperbutler/internal/schema"
)

// EngineOp is an engine operation the host runs the CLI for.
type EngineOp string

const (
	EngineStart          EngineOp = "start"
	EngineStop           EngineOp = "stop"
	EngineRestart        EngineOp = "restart"         // VM restart: `hyperbutler restart`
	EngineRestartDockerd EngineOp = "restart-dockerd" // `hyperbutler config daemon apply`
)

// Host is what the bridge needs from hyperbutler. cmd/hyperbutler implements it with
// the CLI's own code, so the GUI has no second implementation of any operation.
type Host interface {
	// Status is the `status --json` document, without the update check.
	Status(ctx context.Context) (schema.Status, error)
	// Docker returns a client of the running engine's Docker API (ErrEngineStopped
	// when none runs). The caller closes it.
	Docker(ctx context.Context) (*dockerapi.Client, error)
	// Engine runs an engine operation and returns what it printed (English).
	Engine(ctx context.Context, op EngineOp) (string, error)
	// ConfigList is `config list --json` (secrets redacted).
	ConfigList(ctx context.Context) (schema.ConfigList, error)
	// ConfigSet is `config set KEY VALUE` without --apply: it never restarts anything.
	ConfigSet(ctx context.Context, key, value string) (string, error)
	// DaemonRead reads the user's daemon.json (exists false when there is none).
	DaemonRead(ctx context.Context) (path string, data []byte, exists bool, err error)
	// DaemonCheck validates a daemon.json as `config daemon validate` does.
	DaemonCheck(data []byte) error
	// DaemonWrite replaces the user's daemon.json (already checked).
	DaemonWrite(ctx context.Context, data []byte) error
	// Doctor runs `doctor`'s checks.
	Doctor(ctx context.Context) (schema.Doctor, error)
	// Diagnose writes a diagnose zip in the user's temporary directory.
	Diagnose(ctx context.Context) (schema.Diagnose, error)
	// Reveal opens Explorer on the folder of path, with the file selected.
	Reveal(path string) error
	// CopyText puts s on the clipboard.
	CopyText(s string) error
}

// Options configure a Dispatcher.
type Options struct {
	Host Host
	// Lang is the page's language and Messages its catalog, already merged over
	// English.
	Lang     string
	Messages map[string]string
	Version  string
	HostCPUs int
	// StartOnOpen starts a stopped engine at the page's first init (gui.startOnOpen).
	StartOnOpen bool
	// Visible reports whether the window is shown (not minimized or hidden).
	Visible func() bool
	// Log receives a line for the host's log (stderr with --verbose); may be nil.
	Log func(string)
	// Painted is called once, when the page reports its first paint.
	Painted func()
	// CheckLayout asks the page to check its layout for overflow (the pseudo-locale
	// check) and report it to Layout; off in normal use.
	CheckLayout bool
	Layout      func(string)
	// Importer is the Docker Desktop import; nil until it is in the build (the page
	// then says it is not available yet).
	Importer Importer
	// Notify sends an event to the page ({"event": name, "data": data}); may be nil.
	Notify func(name string, data any)
	// Context ends with the window: background work (an import) stops with it.
	Context context.Context
}

// Response is the host's answer to one request.
type Response struct {
	ID    int64      `json:"id"`
	OK    bool       `json:"ok"`
	Data  any        `json:"data,omitempty"`
	Error *ErrorView `json:"error,omitempty"`
}

// ErrorView is a failed request as the page shows it: Code is stable and names the
// catalog's explanation, Detail is the English original (bounded, redacted), and Ref
// lets error.copy put that original on the clipboard without the page sending text.
type ErrorView struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
	Ref    int64  `json:"ref"`
}

// Limits of the dispatcher.
const (
	maxInFlight = 8
	maxErrors   = 64
)

// Dispatcher answers the page's requests. Handle may be called from any goroutine.
type Dispatcher struct {
	o Options

	inflight atomic.Int32
	// mutating serializes the operations that change state; a second one while one
	// runs is refused as busy, so a page cannot queue up actions.
	mutating sync.Mutex

	initOnce  sync.Once
	readyOnce sync.Once

	mu       sync.Mutex
	errs     map[int64]string
	errOrder []int64
	nextRef  int64
	lastZip  string

	base context.Context
	imp  importState
	tok  tokens
}

// New returns a dispatcher.
func New(o Options) *Dispatcher {
	if o.Visible == nil {
		o.Visible = func() bool { return true }
	}
	if o.Log == nil {
		o.Log = func(string) {}
	}
	if o.Context == nil {
		o.Context = context.Background()
	}
	return &Dispatcher{o: o, errs: map[int64]string{}, base: o.Context, tok: tokens{now: time.Now}}
}

// Handle answers one raw message. The context ends the operation (the window closed).
func (d *Dispatcher) Handle(ctx context.Context, msg string) Response {
	req, args, err := Decode(msg)
	if err != nil {
		d.o.Log("refused a message: " + dockerapi.CleanText(err.Error(), false))
		return d.fail(req.ID, err, CodeBadRequest)
	}
	if d.inflight.Add(1) > maxInFlight {
		d.inflight.Add(-1)
		return d.fail(req.ID, Codedf(CodeBusy, "too many requests at once"), CodeBusy)
	}
	defer d.inflight.Add(-1)
	data, err := d.serve(ctx, req.Op, args)
	if err != nil {
		return d.fail(req.ID, err, fallbackCode(req.Op))
	}
	return Response{ID: req.ID, OK: true, Data: data}
}

func fallbackCode(op string) string {
	switch op {
	case OpEngineStart:
		return CodeStartFailed
	case OpEngineStop:
		return CodeStopFailed
	case OpEngineRestart:
		return CodeRestartFailed
	case OpDockerdRestart:
		return CodeDaemonApply
	case OpConfigSet, OpConfigList:
		return CodeConfigFailed
	case OpDaemonValidate, OpDaemonSave:
		return CodeDaemonInvalid
	case OpDiagnose:
		return CodeDiagnose
	case OpDoctor:
		return CodeDoctor
	case OpDiagnoseReveal, OpErrorCopy:
		return CodeOS
	}
	return CodeAPIError
}

// fail records err's English original under a new ref and returns the response.
func (d *Dispatcher) fail(id int64, err error, fallback string) Response {
	code := codeOf(err, fallback)
	text := englishText(err)
	return Response{ID: id, Error: &ErrorView{Code: code, Detail: text, Ref: d.remember(code, text)}}
}

// remember keeps an error's English original for error.copy and returns its ref.
func (d *Dispatcher) remember(code, text string) int64 {
	d.mu.Lock()
	d.nextRef++
	ref := d.nextRef
	d.errs[ref] = code + ": " + text
	d.errOrder = append(d.errOrder, ref)
	if len(d.errOrder) > maxErrors {
		delete(d.errs, d.errOrder[0])
		d.errOrder = d.errOrder[1:]
	}
	d.mu.Unlock()
	return ref
}

// exclusive runs f unless another state change runs.
func (d *Dispatcher) exclusive(f func() (any, error)) (any, error) {
	if !d.mutating.TryLock() {
		return nil, Codedf(CodeBusy, "another change is still running")
	}
	defer d.mutating.Unlock()
	return f()
}

// Per-operation time limits.
const (
	readTimeout    = 20 * time.Second
	startTimeout   = 6 * time.Minute
	stopTimeout    = 2 * time.Minute
	restartTimeout = 8 * time.Minute
	slowTimeout    = 2 * time.Minute
)

func (d *Dispatcher) serve(ctx context.Context, op string, args any) (any, error) {
	timed := func(t time.Duration) (context.Context, context.CancelFunc) { return context.WithTimeout(ctx, t) }
	switch op {
	case OpInit:
		return d.init(ctx)
	case OpReady:
		d.readyOnce.Do(func() {
			if d.o.Painted != nil {
				d.o.Painted()
			}
		})
		return struct{}{}, nil
	case OpLayout:
		if d.o.CheckLayout && d.o.Layout != nil {
			d.o.Layout(dockerapi.CleanText(args.(*LogArgs).Text, false))
		}
		return struct{}{}, nil
	case OpImportPlan:
		c, cancel := timed(slowTimeout)
		defer cancel()
		return d.importPlan(c)
	case OpImportRun:
		a := args.(*ImportArgs)
		if err := d.tok.use(a.Token, OpImportRun, ""); err != nil {
			return nil, err
		}
		return d.importRun(a.IDs, a.Force)
	case OpImportCancel:
		return d.importCancel()
	case OpLog:
		d.o.Log("page: " + dockerapi.CleanText(args.(*LogArgs).Text, false))
		return struct{}{}, nil
	case OpStatus:
		if args.(*RefreshArgs).Auto && !d.o.Visible() {
			return nil, Codedf(CodeHidden, "no refresh while the window is hidden")
		}
		c, cancel := timed(readTimeout)
		defer cancel()
		s, err := d.o.Host.Status(c)
		if err != nil {
			return nil, err
		}
		v := MakeStatus(s)
		if v.Error != nil {
			v.Error.Ref = d.remember(v.Error.Code, v.Error.Detail)
		}
		return v, nil
	case OpContainers:
		if args.(*RefreshArgs).Auto && !d.o.Visible() {
			return nil, Codedf(CodeHidden, "no refresh while the window is hidden")
		}
		c, cancel := timed(readTimeout)
		defer cancel()
		return d.containers(c)
	case OpContainerDo:
		a := args.(*ContainerArgs)
		if a.Action != "start" {
			if err := d.tok.use(a.Token, "container."+a.Action, a.ID); err != nil {
				return nil, err
			}
		}
		return d.exclusive(func() (any, error) {
			c, cancel := timed(readTimeout + 10*time.Second)
			defer cancel()
			return d.containerAction(c, a.ID, a.Action)
		})
	case OpContainerLogs:
		a := args.(*LogsArgs)
		c, cancel := timed(readTimeout)
		defer cancel()
		return d.logs(c, a.ID, a.Tail)
	case OpConfirm:
		return d.confirm(ctx, args.(*ConfirmArgs))
	case OpEngineStart, OpEngineStop, OpEngineRestart, OpDockerdRestart:
		eop, t := map[string]EngineOp{OpEngineStart: EngineStart, OpEngineStop: EngineStop, OpEngineRestart: EngineRestart, OpDockerdRestart: EngineRestartDockerd}[op],
			map[string]time.Duration{OpEngineStart: startTimeout, OpEngineStop: stopTimeout, OpEngineRestart: restartTimeout, OpDockerdRestart: stopTimeout + time.Minute}[op]
		if eop != EngineStart {
			if err := d.tok.use(args.(*TokenArgs).Token, op, ""); err != nil {
				return nil, err
			}
		}
		return d.exclusive(func() (any, error) {
			c, cancel := timed(t)
			defer cancel()
			out, err := d.o.Host.Engine(c, eop)
			if err != nil {
				return nil, err
			}
			return OutputView{Output: boundOutput(out)}, nil
		})
	case OpConfigList:
		c, cancel := timed(readTimeout)
		defer cancel()
		l, err := d.o.Host.ConfigList(c)
		if err != nil {
			return nil, err
		}
		return MakeSettings(l), nil
	case OpConfigSet:
		a := args.(*ConfigSetArgs)
		return d.exclusive(func() (any, error) { return d.configSet(ctx, a.Key, a.Value) })
	case OpDaemonGet:
		return d.daemonGet(ctx)
	case OpDaemonValidate:
		return d.daemonCheck(args.(*DaemonArgs).Text)
	case OpDaemonSave:
		a := args.(*DaemonArgs)
		if err := d.tok.use(a.Token, OpDaemonSave, ""); err != nil {
			return nil, err
		}
		text := a.Text
		return d.exclusive(func() (any, error) { return d.daemonSave(ctx, text) })
	case OpStorage:
		c, cancel := timed(readTimeout)
		defer cancel()
		return d.storage(c)
	case OpDoctor:
		c, cancel := timed(slowTimeout)
		defer cancel()
		r, err := d.o.Host.Doctor(c)
		if err != nil {
			return nil, err
		}
		return MakeDoctor(r), nil
	case OpDiagnose:
		return d.exclusive(func() (any, error) {
			c, cancel := timed(slowTimeout)
			defer cancel()
			r, err := d.o.Host.Diagnose(c)
			if err != nil {
				return nil, err
			}
			d.mu.Lock()
			d.lastZip = r.File
			d.mu.Unlock()
			return DiagnoseView{File: field(r.File), Files: len(r.Files), Missing: len(r.Missing)}, nil
		})
	case OpDiagnoseReveal:
		d.mu.Lock()
		p := d.lastZip
		d.mu.Unlock()
		// Only the zip this window made: the page never names a path.
		if p == "" {
			return nil, Codedf(CodeOS, "no diagnose file was made in this window")
		}
		return struct{}{}, Coded(CodeOS, d.o.Host.Reveal(p))
	case OpErrorCopy:
		d.mu.Lock()
		text, ok := d.errs[args.(*RefArgs).Ref]
		d.mu.Unlock()
		if !ok {
			return nil, Codedf(CodeOS, "that error is no longer known")
		}
		return struct{}{}, Coded(CodeOS, d.o.Host.CopyText(text))
	}
	return nil, badf("unknown operation %.40q", op)
}

// InitView is the page's start: its language and catalog, and what the host knows.
type InitView struct {
	Lang     string            `json:"lang"`
	Messages map[string]string `json:"messages"`
	Version  string            `json:"version"`
	HostCPUs int               `json:"hostCpus"`
	MaxTail  int               `json:"maxTail"`
	// Starting says the host started the engine because gui.startOnOpen is set.
	Starting bool `json:"starting"`
	// CheckLayout asks the page to run its layout check (Options.CheckLayout).
	CheckLayout bool `json:"checkLayout,omitempty"`
}

func (d *Dispatcher) init(ctx context.Context) (any, error) {
	v := InitView{Lang: d.o.Lang, Messages: d.o.Messages, Version: field(d.o.Version), HostCPUs: d.o.HostCPUs, MaxTail: MaxTail, CheckLayout: d.o.CheckLayout}
	// gui.startOnOpen: only at the first init of the window, only when set, and only
	// for an installation that is set up and stopped (REQ-UX-016).
	d.initOnce.Do(func() {
		if !d.o.StartOnOpen {
			return
		}
		c, cancel := context.WithTimeout(ctx, readTimeout)
		s, err := d.o.Host.Status(c)
		cancel()
		if err != nil || !s.SetUp || s.Running {
			return
		}
		v.Starting = true
		go func() {
			_, _ = d.exclusive(func() (any, error) {
				c, cancel := context.WithTimeout(ctx, startTimeout)
				defer cancel()
				_, err := d.o.Host.Engine(c, EngineStart)
				if err != nil {
					d.o.Log("start on open: " + englishText(err))
				}
				return nil, err
			})
		}()
	})
	return v, nil
}

// OutputView is an engine operation's output, in English as the CLI printed it.
type OutputView struct {
	Output string `json:"output"`
}

// maxOutput bounds an operation's output.
const maxOutput = 16 << 10

func boundOutput(s string) string {
	s = dockerapi.CleanText(redact.String(s), true)
	if len(s) > maxOutput {
		s = dockerapi.Clip(s, maxOutput) + "\n[truncated]"
	}
	return s
}

// --- containers ---------------------------------------------------------------------

// Bounds of the container views.
const (
	maxContainers = 200
	maxLogBytes   = 256 << 10
	logReadMax    = 4 << 20
)

func (d *Dispatcher) docker(ctx context.Context) (*dockerapi.Client, error) {
	return d.o.Host.Docker(ctx)
}

func (d *Dispatcher) containers(ctx context.Context) (any, error) {
	c, err := d.docker(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	var list []dockerapi.Container
	if err := c.GetJSON(ctx, "/containers/json", url.Values{"all": {"1"}}, &list); err != nil {
		return nil, err
	}
	return MakeContainers(list), nil
}

func (d *Dispatcher) containerAction(ctx context.Context, id, action string) (any, error) {
	c, err := d.docker(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	var q url.Values
	if action != "start" {
		q = url.Values{"t": {"10"}}
	}
	// id is 64 hex digits (checkArgs) and action one of three words: one path segment.
	resp, err := c.Do(ctx, http.MethodPost, "/containers/"+id+"/"+action, q, nil, "")
	if err != nil {
		return nil, err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	return struct{}{}, nil
}

// LogView is a container's last lines, as text: the page shows it with textContent.
type LogView struct {
	Text      string `json:"text"`
	Lines     int    `json:"lines"`
	Truncated bool   `json:"truncated"`
}

func (d *Dispatcher) logs(ctx context.Context, id string, tail int) (any, error) {
	c, err := d.docker(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	var ins dockerapi.Inspect
	if err := c.GetJSON(ctx, "/containers/"+id+"/json", nil, &ins); err != nil {
		return nil, err
	}
	q := url.Values{"stdout": {"1"}, "stderr": {"1"}, "tail": {strconv.Itoa(tail)}, "timestamps": {"1"}}
	resp, err := c.Do(ctx, http.MethodGet, "/containers/"+id+"/logs", q, nil, "")
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, logReadMax+1))
	if err != nil {
		return nil, Coded(CodeAPIUnreachable, err)
	}
	cut := false
	if len(raw) > logReadMax {
		raw, cut = raw[len(raw)-logReadMax:], true
		if !ins.Config.Tty {
			// The tail starts mid-frame: find the first whole frame.
			raw = dockerapi.Resync(raw)
		}
	}
	if !ins.Config.Tty {
		var ok bool
		if raw, ok = dockerapi.Demux(raw); !ok {
			cut = true
		}
	}
	text, n, c2 := dockerapi.LogText(raw, tail, maxLogBytes)
	return LogView{Text: text, Lines: n, Truncated: cut || c2}, nil
}

// PreviewView is REQ-UX-017's list: the running containers a stop or restart stops.
type PreviewView struct {
	Running    bool           `json:"running"`
	Containers []RunningView  `json:"containers"`
	Unknown    bool           `json:"unknown"` // the list could not be read
	Detail     string         `json:"detail,omitempty"`
	Counts     map[string]int `json:"counts"`
}

// RunningView is one container in PreviewView. Then is "back", "maybe" or "stays".
type RunningView struct {
	Name  string `json:"name"`
	Image string `json:"image"`
	Then  string `json:"then"`
}

func (d *Dispatcher) preview(ctx context.Context) (any, error) {
	v := PreviewView{Containers: []RunningView{}, Counts: map[string]int{"back": 0, "maybe": 0, "stays": 0}}
	c, err := d.docker(ctx)
	if errors.Is(err, ErrEngineStopped) {
		return v, nil
	}
	if err != nil {
		v.Running, v.Unknown, v.Detail = true, true, englishText(err)
		return v, nil
	}
	defer c.Close()
	v.Running = true
	list, err := runningContainers(ctx, c)
	if err != nil {
		v.Unknown, v.Detail = true, englishText(err)
		return v, nil
	}
	for i, rc := range list {
		if i == maxContainers {
			break
		}
		then := "stays"
		switch {
		case rc.ComesBack:
			then = "back"
		case rc.MayComeBack():
			then = "maybe"
		}
		v.Counts[then]++
		v.Containers = append(v.Containers, RunningView{Name: field(rc.Name), Image: field(rc.Image), Then: then})
	}
	return v, nil
}

// --- settings -----------------------------------------------------------------------

// Editable are the settings the GUI changes. The others are shown and changed with the
// CLI: they are paths, pipes, secrets or trust decisions that a page showing
// container text should not be able to change (docs/gui.md). TestEditableCoversEveryKey
// fails until a new key is classified here.
var Editable = map[string]bool{
	"memory":              true,
	"cpus":                true,
	"context.manage":      true,
	"autostart":           true,
	"updateCheck":         true,
	"hostGateway.enabled": true,
	"hostGateway.ports":   true,
	"gui.startOnOpen":     true,
	"gui.language":        true,

	"disk.size":          false,
	"data.dir":           false,
	"pipe":               false,
	"engine.memoryLimit": false,
	"network.frameCheck": false,
	"ca.import":          false,
	"ca.extraFile":       false,
	"proxy.http":         false,
	"proxy.https":        false,
	"proxy.noProxy":      false,
}

// SetView is a change of one setting.
type SetView struct {
	Item    SettingView `json:"item"`
	Output  string      `json:"output"`
	Running bool        `json:"running"`
}

func (d *Dispatcher) configSet(ctx context.Context, key, value string) (any, error) {
	k, err := config.Lookup(key)
	if err != nil {
		return nil, Coded(CodeConfigValue, err)
	}
	if !Editable[k.Name] {
		return nil, Codedf(CodeNotEditable, "%s is changed with `hyperbutler config set` in a terminal", k.Name)
	}
	c, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	out, err := d.o.Host.ConfigSet(c, k.Name, value)
	if err != nil {
		return nil, Coded(CodeConfigValue, err)
	}
	l, err := d.o.Host.ConfigList(c)
	if err != nil {
		return nil, err
	}
	v := SetView{Output: boundOutput(out)}
	for _, it := range MakeSettings(l).Keys {
		if it.Key == k.Name {
			v.Item = it
		}
	}
	if s, err := d.o.Host.Status(c); err == nil {
		v.Running = s.Running
	}
	return v, nil
}

// DaemonView is the user's daemon.json for the editor.
type DaemonView struct {
	Path   string `json:"path"`
	Text   string `json:"text"`
	Exists bool   `json:"exists"`
	// ReadOnly is set when the file holds credentials (a proxy password): the page
	// gets the text redacted and cannot save it.
	ReadOnly bool `json:"readOnly"`
}

func (d *Dispatcher) daemonGet(ctx context.Context) (any, error) {
	p, b, exists, err := d.o.Host.DaemonRead(ctx)
	if err != nil {
		return nil, Coded(CodeDaemonInvalid, err)
	}
	v := DaemonView{Path: field(p), Exists: exists, Text: string(b)}
	// String catches secret keys ("password": ...) and credentials in URLs.
	if r := redact.String(string(b)); r != string(b) {
		v.Text, v.ReadOnly = r, true
	}
	return v, nil
}

// CheckView is the result of a daemon.json check.
type CheckView struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

func (d *Dispatcher) daemonCheck(text string) (any, error) {
	if err := d.o.Host.DaemonCheck([]byte(text)); err != nil {
		return CheckView{Detail: englishText(err)}, nil
	}
	return CheckView{OK: true}, nil
}

func (d *Dispatcher) daemonSave(ctx context.Context, text string) (any, error) {
	b := []byte(text)
	if r := redact.String(text); r != text {
		return nil, Codedf(CodeDaemonReadOnly, "the text holds credentials; edit daemon.json in a text editor")
	}
	_, cur, exists, err := d.o.Host.DaemonRead(ctx)
	if err == nil && exists && redact.String(string(cur)) != string(cur) {
		return nil, Codedf(CodeDaemonReadOnly, "daemon.json holds credentials; edit it in a text editor")
	}
	if err := d.o.Host.DaemonCheck(b); err != nil {
		return nil, Coded(CodeDaemonInvalid, err)
	}
	if err := d.o.Host.DaemonWrite(ctx, b); err != nil {
		return nil, Coded(CodeDaemonInvalid, err)
	}
	return CheckView{OK: true}, nil
}

// --- storage ------------------------------------------------------------------------

func (d *Dispatcher) storage(ctx context.Context) (any, error) {
	s, err := d.o.Host.Status(ctx)
	if err != nil {
		return nil, err
	}
	v := StorageView{DiskUsed: s.DiskUsed, DiskMax: s.DiskMax, HostFree: s.HostFree, Images: []ImageView{}, Volumes: []VolumeView{}}
	if s.GuestDisk != nil {
		v.GuestUsed, v.GuestSize = s.GuestDisk.UsedBytes, s.GuestDisk.SizeBytes
	}
	c, err := d.docker(ctx)
	if errors.Is(err, ErrEngineStopped) {
		return v, nil
	}
	if err != nil {
		return nil, err
	}
	defer c.Close()
	v.Engine = true
	var df dockerapi.DF
	if err := c.GetJSON(ctx, "/system/df", nil, &df); err != nil {
		return nil, err
	}
	fillStorage(&v, df)
	return v, nil
}

// runningContainers lists the running containers with their restart policies, the
// inspects several at a time within the context's deadline (dockerapi.RestartPolicies);
// a container whose policy is not known by then is listed as staying stopped and the
// list is still returned.
func runningContainers(ctx context.Context, c *dockerapi.Client) ([]dockerapi.RunningContainer, error) {
	var list []dockerapi.Container
	if err := c.GetJSON(ctx, "/containers/json", nil, &list); err != nil {
		return nil, err
	}
	if len(list) > maxContainers {
		list = list[:maxContainers]
	}
	deadline := time.Now().Add(5 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Add(-time.Second).Before(deadline) {
		deadline = d.Add(-time.Second)
	}
	pol, err := dockerapi.RestartPolicies(ctx, c, list, deadline, maxContainers, 8)
	if err != nil {
		return nil, err
	}
	out := make([]dockerapi.RunningContainer, 0, len(list))
	for i, ct := range list {
		rc := dockerapi.RunningContainer{Name: dockerapi.ContainerName(ct.ID, ct.Names), Image: ct.Image, RestartPolicy: pol[i]}
		rc.ComesBack = dockerapi.ComesBack(rc.RestartPolicy)
		out = append(out, rc)
	}
	return out, nil
}
