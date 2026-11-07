//go:build windows

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/broker"
	"github.com/Higangssh/hyperbutler/internal/engine"
	"github.com/Higangssh/hyperbutler/internal/imageset"
	"github.com/Higangssh/hyperbutler/internal/sandbox/ctl"
	"github.com/Higangssh/hyperbutler/internal/sandbox/egress"
	"github.com/Higangssh/hyperbutler/internal/sandbox/manager"
)

const sandboxUsage = `hyperbutler sandbox: run a coding agent in a VM of its own (docs/sandbox.md)

  hyperbutler sandbox run [flags] DIR [COMMAND ARG...]
      Copies DIR into a new session and opens a terminal in it. DIR does not change until
      you apply. Without COMMAND, a shell.
        --name NAME        keep the session under this name (unnamed: gone when stopped)
        --egress MODE      dev (default), locked or open
        --allow ENTRY      also allow a host: host, host:port, *.host, lan:host (repeat)
        --env NAME         pass this variable from here into the terminal (repeat)
        --include PATH     copy a file kept out as a secret (repeat)
        --memory MB, --cpus N, --large
  hyperbutler sandbox install [--msi FILE]
      Downloads and installs the agent sandbox image, a separate installer of about 260 MB
      that this release pins by hash; Windows asks for permission once. run and start offer
      it when it is missing. --msi takes the file from here instead of the network.
  hyperbutler sandbox ls
  hyperbutler sandbox attach NAME [TERMINAL]   back to a terminal: the first, or the one
      Ctrl+] named when you left it (sh-2, ...)
  hyperbutler sandbox shell NAME [COMMAND ARG...]   another terminal
  hyperbutler sandbox diff NAME [--stat]   what changed; apply writes exactly this
  hyperbutler sandbox apply NAME (--all | --path PATH...)
  hyperbutler sandbox commits NAME [--branch BRANCH]   fetch the agent's commits into a branch
  hyperbutler sandbox allow NAME ENTRY...   widen a running session's network
  hyperbutler sandbox blocked NAME      what it asked for and did not get
  hyperbutler sandbox start NAME        run a stopped session again
  hyperbutler sandbox stop NAME [--discard]
  hyperbutler sandbox rm NAME [--discard]
`

// detachKey leaves a terminal; the session goes on.
const detachKey = 0x1d // Ctrl+]

func cmdSandbox(args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		_, _ = fmt.Fprint(stdout, sandboxUsage)
		return 0, nil
	}
	in, err := openInstall()
	if err != nil {
		return 1, err
	}
	// Before anything makes a folder in it: the broker takes only a data folder that is this
	// user's alone, and a session can come before the first `hyperbutler start`.
	if err := engine.MkdirPrivate(in.l.Home); err != nil {
		return 1, err
	}
	if err := engine.MkdirPrivate(in.l.Data); err != nil {
		return 1, err
	}
	sub, rest := args[0], args[1:]
	if sub == "manager" {
		return 0, runSandboxManager(in, rest)
	}
	switch sub {
	case "install":
		return sandboxInstall(in, rest, stdout, stderr)
	case "run":
		return sandboxRun(in, rest, stdin, stdout, stderr)
	case "shell", "attach":
		return sandboxTerm(in, sub, rest, stderr)
	case "ls":
		return 0, sandboxList(in, stdout)
	case "diff":
		return sandboxDiff(in, rest, stdout, stderr)
	case "apply":
		return sandboxApply(in, rest, stdout, stderr)
	case "commits":
		return sandboxCommits(in, rest, stdout, stderr)
	case "allow":
		return sandboxAllow(in, rest, stderr)
	case "blocked":
		return sandboxBlocked(in, rest, stdout)
	case "start", "stop", "rm":
		return sandboxLifecycle(in, sub, rest, stdin, stdout, stderr)
	}
	_, _ = fmt.Fprint(stderr, sandboxUsage)
	return 2, fmt.Errorf("unknown sandbox command %q", sub)
}

type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(s string) error { *l = append(*l, s); return nil }

// parseCommand parses flags up to the first other argument, as docker run does: that one
// is the folder or the session, and the rest is the command in the session. A "--" after
// it is optional, which matters because PowerShell drops it on the way into a function.
func parseCommand(fs *flag.FlagSet, args []string) (target string, argv []string, err error) {
	if err := fs.Parse(args); err != nil {
		return "", nil, err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return "", nil, nil
	}
	argv = rest[1:]
	if len(argv) > 0 && argv[0] == "--" {
		argv = argv[1:]
	}
	return rest[0], argv, nil
}

// parseInterleaved takes flags before and after the positional arguments.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func (in *install) sandboxDir() string { return filepath.Join(in.l.Home, "sandbox") }

// sandboxClient connects to the manager, starting it when none runs.
func sandboxClient(in *install) (*manager.Client, error) {
	pipe := manager.PipeName(in.l.Tag())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := manager.Dial(ctx, pipe)
	if err == nil {
		return c, nil
	}
	if !errors.Is(err, manager.ErrNotRunning) {
		return nil, err
	}
	if err := spawnSandboxManager(in); err != nil {
		return nil, err
	}
	for {
		c, err := manager.Dial(ctx, pipe)
		if err == nil {
			return c, nil
		}
		if !errors.Is(err, manager.ErrNotRunning) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("the sandbox manager did not start; see %s", filepath.Join(in.l.LogDir(), "sandbox.log"))
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// spawnSandboxManager starts the manager in the background, as spawnEngine does the
// engine: it outlives this terminal, which is what a kept session is for.
func spawnSandboxManager(in *install) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(in.l.LogDir(), 0o700); err != nil {
		return err
	}
	logf, err := os.OpenFile(filepath.Join(in.l.LogDir(), "sandbox.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = logf.Close() }()
	const (
		detachedProcess        = 0x00000008
		createNewProcessGroup  = 0x00000200
		createBreakawayFromJob = 0x01000000
	)
	for _, breakaway := range []bool{true, false} {
		cmd := exec.Command(exe, "sandbox", "manager") //nolint:gosec // G204: this program with fixed arguments
		cmd.Dir = in.l.Home
		cmd.Stdout, cmd.Stderr = logf, logf
		flags := uint32(detachedProcess | createNewProcessGroup)
		if breakaway {
			flags |= createBreakawayFromJob
		}
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags, HideWindow: true}
		err = cmd.Start()
		if err == nil {
			return cmd.Process.Release()
		}
		if !breakaway || !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			break
		}
	}
	return err
}

// sessionDisks lists the sandbox sessions' disks in l.Data, with what a crash left.
func sessionDisks(l engine.Layout) []string {
	var out []string
	for _, pat := range []string{"session-*.vhdx", "session-*.vhdx.new"} {
		m, err := filepath.Glob(filepath.Join(l.Data, pat))
		if err == nil {
			out = append(out, m...)
		}
	}
	return out
}

// sandboxStateFiles lists the files of the sandbox folder: records and what a crash left.
func sandboxStateFiles(in *install) []string {
	m, err := filepath.Glob(filepath.Join(in.sandboxDir(), "*"))
	if err != nil {
		return nil
	}
	out := m[:0]
	for _, p := range m {
		if fi, err := os.Lstat(p); err == nil && !fi.IsDir() {
			out = append(out, p)
		}
	}
	return out
}

func sandboxLogs(l engine.Layout) []string {
	var out []string
	if _, err := os.Lstat(filepath.Join(l.LogDir(), "sandbox.log")); err == nil {
		out = append(out, filepath.Join(l.LogDir(), "sandbox.log"))
	}
	if m, err := filepath.Glob(filepath.Join(l.LogDir(), "sandbox-console-*.log")); err == nil {
		out = append(out, m...)
	}
	return out
}

// stopSandboxManager ends the manager and every session it runs, if it runs. Their disks
// and records stay; the caller decides about those.
func stopSandboxManager(in *install) error {
	pipe := manager.PipeName(in.l.Tag())
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c, err := manager.Dial(ctx, pipe)
	if errors.Is(err, manager.ErrNotRunning) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = c.Do(&manager.Request{Op: manager.OpQuit}, nil)
	_ = c.Close()
	if err != nil {
		return err
	}
	for {
		c, err := manager.Dial(ctx, pipe)
		if errors.Is(err, manager.ErrNotRunning) {
			return nil
		}
		if err == nil {
			_ = c.Close()
		}
		select {
		case <-ctx.Done():
			return errors.New("the sandbox manager did not end")
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// sandboxHost boots sessions for the manager, through the broker when this machine's
// hyperbutler uses it.
type sandboxHost struct {
	l        engine.Layout
	broker   *engine.SessionBroker // nil in developer mode
	manifest string
	custom   bool // manifest is a developer's own image, or the broker a foreground one
	log      *slog.Logger
}

func (h *sandboxHost) Boot(ctx context.Context, r manager.BootRequest) (manager.VM, error) {
	blank, err := engine.SessionDiskBlank(h.l, r.Slot, r.DiskMeta)
	if err != nil {
		return nil, err
	}
	var console io.Writer
	var logf *os.File
	if os.Getenv("HYPERBUTLER_DEBUG") != "" && h.broker == nil {
		logf, err = os.Create(filepath.Join(h.l.LogDir(), "sandbox-console-"+r.Name+".log"))
		if err == nil {
			console = logf
		}
	}
	v, err := engine.BootSession(ctx, engine.SessionConfig{
		Layout: h.l, Slot: r.Slot, Broker: h.broker, Manifest: h.manifest,
		CPUs: r.CPUs, MemoryMB: r.MemoryMB, DiskBlank: blank, Egress: r.Egress,
		ConsoleLog: console, Logger: h.log,
	})
	if err != nil {
		if logf != nil {
			_ = logf.Close()
		}
		pin, _ := imageset.SandboxPin()
		return nil, sandboxBootError(err, h.custom, h.manifest, pin)
	}
	return &sessionVM{SessionVM: v, log: logf}, nil
}

// sessionVM closes the VM's console log with it.
type sessionVM struct {
	*engine.SessionVM
	log *os.File
}

func (v *sessionVM) Close() error {
	err := v.SessionVM.Close()
	if v.log != nil {
		_ = v.log.Close()
	}
	return err
}

func (h *sandboxHost) CreateDisk(slot uint8, size uint64) (uint64, error) {
	return engine.CreateSessionDisk(h.l, slot, size)
}

// RemoveDisk removes the session's disk. A VM's worker process can hold it for a moment
// after the VM is gone.
func (h *sandboxHost) RemoveDisk(slot uint8) error {
	path := engine.SessionDiskPath(h.l, slot)
	for deadline := time.Now().Add(10 * time.Second); ; {
		err := os.Remove(path)
		switch {
		case err == nil, errors.Is(err, os.ErrNotExist):
			return nil
		case !errors.Is(err, windows.ERROR_SHARING_VIOLATION) || time.Now().After(deadline):
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// sandboxImageDirName is the folder next to hyperbutler.exe with the sandbox image, as
// next to the broker. For a developer's own build, HYPERBUTLER_SANDBOX_IMAGE names another
// manifest, which boots in the manager's process, and HYPERBUTLER_SANDBOX_BROKER a foreground
// broker's pipe.
const sandboxImageDirName = broker.SandboxImageDirName

func runSandboxManager(in *install, args []string) error {
	fs := flag.NewFlagSet("sandbox manager", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	manifest := os.Getenv("HYPERBUTLER_SANDBOX_IMAGE")
	custom := manifest != ""
	if !custom {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		manifest = filepath.Join(filepath.Dir(exe), sandboxImageDirName, "image.json")
	}
	choice, err := chooseBackend(backendAuto, os.Getenv("HYPERBUTLER_SANDBOX_BROKER"), custom, machineService, os.Stderr)
	if err != nil {
		return err
	}
	if choice.note != "" {
		log.Warn("not using the broker service; sessions run from this process (needs Hyper-V Administrators or elevation)", "reason", choice.note)
	}
	if err := engine.MkdirPrivate(in.sandboxDir()); err != nil {
		return err
	}
	host := &sandboxHost{l: in.l, manifest: manifest, custom: custom || os.Getenv("HYPERBUTLER_SANDBOX_BROKER") != "", log: log}
	if choice.pipe != "" {
		host.broker = engine.NewSessionBroker(choice.pipe, in.l.Data, log)
	}
	m, err := manager.New(host, in.sandboxDir(), log)
	if err != nil {
		return err
	}
	defer m.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return manager.ListenAndServe(ctx, m, manager.PipeName(in.l.Tag()))
}

func sandboxRun(in *install, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	fs := flag.NewFlagSet("sandbox run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	name := fs.String("name", "", "keep the session under this name")
	mode := fs.String("egress", "", "dev, locked or open")
	memory := fs.Int("memory", 0, "guest memory in MiB")
	cpus := fs.Int("cpus", 0, "vCPUs")
	large := fs.Bool("large", false, "copy a project over the size limits")
	var allow, env, include listFlag
	fs.Var(&allow, "allow", "also allow: host, host:port, *.host, lan:host")
	fs.Var(&env, "env", "pass this variable into the terminal")
	fs.Var(&include, "include", "copy a file kept out as a secret")
	target, argv, err := parseCommand(fs, args)
	if err != nil {
		return 2, err
	}
	if target == "" {
		return 2, errors.New("sandbox run takes a folder")
	}
	dir, err := filepath.Abs(target)
	if err != nil {
		return 1, err
	}
	envs, err := envValues(env)
	if err != nil {
		return 2, err
	}
	if err := ensureSandboxImage(in, stdin, stdout, stderr); err != nil {
		return 1, err
	}
	c, err := sandboxClient(in)
	if err != nil {
		return 1, err
	}
	defer func() { _ = c.Close() }()
	r, err := c.Do(&manager.Request{
		Op: manager.OpRun, Name: *name, Kept: *name != "", Dir: dir, Mode: *mode, Allow: allow, Include: include,
		MemoryMB: *memory, CPUs: *cpus, LargeOK: *large,
	}, func(p string) { _, _ = fmt.Fprintln(stderr, p) })
	if err != nil {
		return 1, err
	}
	for _, s := range r.Skipped {
		_, _ = fmt.Fprintf(stderr, "left out: %s (%s)\n", s.Path, s.Reason)
	}
	s := r.Session
	_, _ = fmt.Fprintf(stdout, "session %s: %d files, %d MiB, network %s. Ctrl+] leaves the terminal; the session goes on.\n", s.Name, r.Files, r.Bytes>>20, s.Mode)
	code, detached, err := openTerminal(in, s.Name, "", argv, envs)
	if err != nil {
		return 1, err
	}
	if !detached {
		_, _ = fmt.Fprintf(stdout, "next: `hyperbutler sandbox diff %s`, `apply %s --all`, `stop %s`\n", s.Name, s.Name, s.Name)
	}
	return code, nil
}

// envValues reads the named variables here: a value on the command line would be in every
// process listing.
func envValues(names []string) ([]string, error) {
	out := make([]string, 0, len(names))
	for _, n := range names {
		v, ok := os.LookupEnv(n)
		if !ok {
			return nil, fmt.Errorf("--env %s: that variable is not set here", n)
		}
		out = append(out, n+"="+v)
	}
	return out, nil
}

func sandboxTerm(in *install, sub string, args []string, stderr io.Writer) (int, error) {
	fs := flag.NewFlagSet("sandbox "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	var env listFlag
	if sub == "shell" {
		fs.Var(&env, "env", "pass this variable into the terminal")
	}
	name, argv, err := parseCommand(fs, args)
	if err != nil {
		return 2, err
	}
	if name == "" {
		return 2, fmt.Errorf("sandbox %s takes a session name", sub)
	}
	if sub == "attach" && len(argv) > 1 {
		return 2, errors.New("sandbox attach takes a session name and maybe a terminal (main, sh-2, ...)")
	}
	envs, err := envValues(env)
	if err != nil {
		return 2, err
	}
	attach := ""
	if sub == "attach" {
		attach = "main"
		if len(argv) == 1 {
			attach, argv = argv[0], nil
		}
	}
	code, _, err := openTerminal(in, name, attach, argv, envs)
	if err != nil {
		return 1, err
	}
	return code, nil
}

// openTerminal connects this console to a terminal of the session until its process ends
// or the user leaves with detachKey.
func openTerminal(in *install, name, attach string, argv, env []string) (code int, detached bool, err error) {
	c, err := sandboxClient(in)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = c.Close() }()
	con, err := newConsole()
	if err != nil {
		return 0, false, err
	}
	defer con.restore()
	cols, rows := con.size()
	r, err := c.Do(&manager.Request{Op: manager.OpTerm, Name: name, Attach: attach, Argv: argv, Env: env, Cols: cols, Rows: rows}, nil)
	if err != nil {
		return 0, false, err
	}
	back := name
	if r.Terminal != "main" && r.Terminal != "" {
		back += " " + shownGuestText([]byte(r.Terminal))
	}
	s := c.Stream()
	done := make(chan struct{})
	defer close(done)
	detach := make(chan struct{})
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil {
				return
			}
			b := buf[:n]
			if i := indexByte(b, detachKey); i >= 0 {
				if i > 0 {
					_ = ctl.WriteFrame(s, ctl.TypeInput, b[:i])
				}
				close(detach)
				return
			}
			if ctl.WriteFrame(s, ctl.TypeInput, b) != nil {
				return
			}
		}
	}()
	go func() {
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if nc, nr := con.size(); nc != cols || nr != rows {
					cols, rows = nc, nr
					_ = ctl.WriteFrame(s, ctl.TypeResize, ctl.Resize(cols, rows))
				}
			}
		}
	}()
	type end struct {
		code int32
		err  error
	}
	out := make(chan end, 1)
	go func() {
		for {
			t, data, err := ctl.ReadFrame(s)
			if err != nil {
				out <- end{err: err}
				return
			}
			switch t {
			case ctl.TypeOutput:
				// Raw on purpose: this is the session's screen, as over ssh. It reaches only
				// this terminal; the review never goes through here.
				if _, err := os.Stdout.Write(data); err != nil {
					out <- end{err: err}
					return
				}
			case ctl.TypeExit:
				c, err := ctl.ParseExit(data)
				out <- end{code: c, err: err}
				return
			case ctl.TypeError:
				out <- end{err: fmt.Errorf("the session: %s", shownGuestText(data))}
				return
			default:
				out <- end{err: fmt.Errorf("frame type %d from the session", t)}
				return
			}
		}
	}()
	select {
	case <-detach:
		con.restore()
		_, _ = fmt.Fprintf(os.Stdout, "\r\nleft the terminal; it goes on. `hyperbutler sandbox attach %s` goes back.\n", back)
		return 0, true, nil
	case e := <-out:
		if e.err != nil {
			if errors.Is(e.err, io.EOF) {
				return 0, false, errors.New("the session's terminal closed; the session may have stopped (`hyperbutler sandbox ls`)")
			}
			return 0, false, e.err
		}
		return int(e.code), false, nil
	}
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

func sandboxOne(in *install, req *manager.Request, stderr io.Writer) (manager.Reply, error) {
	c, err := sandboxClient(in)
	if err != nil {
		return manager.Reply{}, err
	}
	defer func() { _ = c.Close() }()
	return c.Do(req, func(p string) { _, _ = fmt.Fprintln(stderr, p) })
}

func sandboxList(in *install, stdout io.Writer) error {
	r, err := sandboxOne(in, &manager.Request{Op: manager.OpList}, io.Discard)
	if err != nil {
		return err
	}
	if len(r.Sessions) == 0 {
		_, _ = fmt.Fprintln(stdout, "no sessions")
		return nil
	}
	t := newTable(stdout, "NAME", "STATE", "KEPT", "NETWORK", "MEMORY", "PROJECT")
	for _, s := range r.Sessions {
		state := "stopped"
		if s.Running {
			state = "running"
		}
		t.row(s.Name, state, strconv.FormatBool(s.Kept), s.Mode, strconv.Itoa(s.MemoryMB)+" MiB", s.Dir)
	}
	t.flush()
	return nil
}

func sandboxDiff(in *install, args []string, stdout, stderr io.Writer) (int, error) {
	fs := flag.NewFlagSet("sandbox diff", flag.ContinueOnError)
	fs.SetOutput(stderr)
	stat := fs.Bool("stat", false, "only the list of files")
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		return 2, err
	}
	if len(pos) != 1 {
		return 2, errors.New("sandbox diff takes one session name")
	}
	r, err := sandboxOne(in, &manager.Request{Op: manager.OpDiff, Name: pos[0], StatOnly: *stat}, stderr)
	if err != nil {
		return 1, err
	}
	if len(r.Items) == 0 {
		_, _ = fmt.Fprintln(stdout, "no changes")
		return 0, nil
	}
	for _, it := range r.Items {
		note := ""
		switch {
		case it.Refused != "":
			note = " NOT APPLICABLE: " + it.Refused
		case it.Binary:
			note = " binary"
		default:
			note = fmt.Sprintf(" +%d -%d", it.Added, it.Removed)
		}
		if it.Risk != "" {
			note += " RISK: " + it.Risk
		}
		if it.Conflict {
			note += " CONFLICT: changed on this computer since the copy"
		}
		_, _ = fmt.Fprintf(stdout, "%-8s %s%s\n", it.Kind, it.Shown, note)
		if it.Text != "" {
			_, _ = fmt.Fprint(stdout, it.Text)
		}
		if it.Unshown {
			_, _ = fmt.Fprintf(stdout, "(not all of this change is shown; `apply --all` leaves it out, `--path %s` takes it)\n", it.Path)
		}
	}
	return 0, nil
}

func sandboxApply(in *install, args []string, stdout, stderr io.Writer) (int, error) {
	fs := flag.NewFlagSet("sandbox apply", flag.ContinueOnError)
	fs.SetOutput(stderr)
	all := fs.Bool("all", false, "every change of the last review, except the risky ones")
	var paths listFlag
	fs.Var(&paths, "path", "this change of the last review (repeat)")
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		return 2, err
	}
	if len(pos) != 1 || (!*all && len(paths) == 0) {
		return 2, errors.New("sandbox apply takes a session name and --all or --path")
	}
	r, err := sandboxOne(in, &manager.Request{Op: manager.OpApply, Name: pos[0], All: *all, Paths: paths}, stderr)
	if err != nil {
		return 1, err
	}
	code := 0
	for _, o := range r.Outcomes {
		if o.Error != "" {
			code = 1
			_, _ = fmt.Fprintf(stdout, "not applied: %s: %s\n", o.Path, o.Error)
			continue
		}
		_, _ = fmt.Fprintf(stdout, "applied: %s\n", o.Path)
	}
	return code, nil
}

func sandboxCommits(in *install, args []string, stdout, stderr io.Writer) (int, error) {
	fs := flag.NewFlagSet("sandbox commits", flag.ContinueOnError)
	fs.SetOutput(stderr)
	branch := fs.String("branch", "", "the branch to fetch into (default sandbox/NAME)")
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		return 2, err
	}
	if len(pos) != 1 {
		return 2, errors.New("sandbox commits takes one session name")
	}
	r, err := sandboxOne(in, &manager.Request{Op: manager.OpCommits, Name: pos[0], Branch: *branch}, stderr)
	if err != nil {
		return 1, err
	}
	if r.Branch != "" {
		_, _ = fmt.Fprintf(stdout, "fetched into %s:\n", r.Branch)
	}
	_, _ = fmt.Fprintln(stdout, strings.TrimRight(r.Commits, "\n"))
	return 0, nil
}

func sandboxAllow(in *install, args []string, stderr io.Writer) (int, error) {
	if len(args) < 2 {
		return 2, errors.New("sandbox allow takes a session name and what to allow")
	}
	if _, err := sandboxOne(in, &manager.Request{Op: manager.OpAllow, Name: args[0], Allow: args[1:]}, stderr); err != nil {
		return 1, err
	}
	return 0, nil
}

func sandboxBlocked(in *install, args []string, stdout io.Writer) (int, error) {
	if len(args) != 1 {
		return 2, errors.New("sandbox blocked takes one session name")
	}
	r, err := sandboxOne(in, &manager.Request{Op: manager.OpBlocked, Name: args[0]}, io.Discard)
	if err != nil {
		return 1, err
	}
	if len(r.Blocked) == 0 {
		_, _ = fmt.Fprintln(stdout, "nothing blocked")
		return 0, nil
	}
	t := newTable(stdout, "NAME", "KIND", "COUNT")
	ech := false
	for _, b := range r.Blocked {
		t.row(b.Name, b.Kind, strconv.FormatUint(b.Count, 10))
		ech = ech || b.Kind == egress.KindECH
	}
	t.flush()
	if r.BlockedLost > 0 {
		_, _ = fmt.Fprintf(stdout, "and %d more\n", r.BlockedLost)
	}
	if ech {
		_, _ = fmt.Fprintln(stdout, echHint)
	}
	return 0, nil
}

// echHint follows an ech line, which a browser is the usual cause of (docs/sandbox.md,
// Browsers).
const echHint = `
ech: a connection that hid which server it wanted, so it was refused. Browsers do this on
every connection. In a session Chrome and Playwright's channel "chromium" have it turned
off; Playwright's default headless browser does not: launch it with channel: "chromium".`

func sandboxLifecycle(in *install, sub string, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	fs := flag.NewFlagSet("sandbox "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	discard := fs.Bool("discard", false, "even though it has changes nobody applied")
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		return 2, err
	}
	if len(pos) != 1 {
		return 2, fmt.Errorf("sandbox %s takes one session name", sub)
	}
	if sub == "start" {
		if err := ensureSandboxImage(in, stdin, stdout, stderr); err != nil {
			return 1, err
		}
	}
	op := map[string]manager.Op{"start": manager.OpStart, "stop": manager.OpStop, "rm": manager.OpRemove}[sub]
	r, err := sandboxOne(in, &manager.Request{Op: op, Name: pos[0], Discard: *discard}, stderr)
	if err != nil {
		return 1, err
	}
	if r.Session != nil {
		_, _ = fmt.Fprintf(stdout, "%s is running. `hyperbutler sandbox shell %s` opens a terminal.\n", r.Session.Name, r.Session.Name)
	}
	return 0, nil
}
