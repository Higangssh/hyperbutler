//go:build windows

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/config"
	"github.com/Higangssh/hyperbutler/internal/daemoncfg"
	"github.com/Higangssh/hyperbutler/internal/engine"
	"github.com/Higangssh/hyperbutler/internal/netstack"
)

const configUsage = `Usage:
  hyperbutler config list   [--json]
  hyperbutler config get    KEY [--json]
  hyperbutler config set    KEY VALUE [--apply] [--image-dir DIR]
  hyperbutler config unset  KEY [--apply] [--image-dir DIR]
  hyperbutler config path   [--json]
  hyperbutler config daemon validate [--file FILE]
  hyperbutler config daemon apply    [--timeout DURATION]

Keys: %s
set and unset write settings.json and say what restart, if any, the change needs.
They never restart anything unless --apply is given.

daemon.json (%s) is passed to the guest's dockerd byte for byte;
"config daemon apply" restarts dockerd (not the VM) with it.
`

func configUsageText() string {
	return fmt.Sprintf(configUsage, strings.Join(config.Names(), ", "), `%LOCALAPPDATA%\hyperbutler\daemon.json`)
}

func (in *install) env() config.Env {
	return config.Env{
		Home: in.l.Home, HostCPUs: runtime.NumCPU(), Getenv: os.Getenv,
		DiskExists: func(dir string) bool { return diskExists(engine.Layout{Home: in.l.Home, Data: dir}) },
	}
}

// diskExists reports whether the data disk (or its metadata) is in l.Data.
func diskExists(l engine.Layout) bool {
	for _, p := range []string{l.DiskPath(), l.DiskMetaPath(), l.DiskPath() + ".new"} {
		if _, err := os.Lstat(p); err == nil {
			return true
		}
	}
	return false
}

func cmdConfig(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		_, _ = fmt.Fprint(stdout, configUsageText())
		if len(args) == 0 {
			return &shownError{err: errors.New("config: no subcommand")}
		}
		return nil
	}
	switch args[0] {
	case "list":
		return configList(args[1:], stdout, stderr)
	case "get":
		return configGet(args[1:], stdout, stderr)
	case "set", "unset":
		return configSet(args[0], args[1:], stdout, stderr)
	case "path":
		return configPath(args[1:], stdout, stderr)
	case "daemon":
		return configDaemon(args[1:], stdout, stderr)
	}
	return fmt.Errorf("config: unknown subcommand %q\n\n%s", args[0], configUsageText())
}

func configList(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("config list", stderr)
	asJSON := fs.Bool("json", false, "print JSON")
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	in, err := openInstall()
	if err != nil {
		return err
	}
	items := config.List(in.settings, in.env())
	if *asJSON {
		return writeJSON(stdout, ConfigListJSON{Schema: SchemaVersion, SettingsFile: in.l.SettingsPath(), Keys: items})
	}
	w := newTable(stdout, "KEY", "VALUE", "DEFAULT", "SOURCE", "APPLIES")
	for _, it := range items {
		w.row(it.Key, it.Value, it.Default, string(it.Source), applyText(it.Apply))
	}
	w.flush()
	if in.settings == nil {
		_, _ = fmt.Fprintf(stdout, "\nNo settings file yet (%s): every value is a default.\n", in.l.SettingsPath())
	}
	return nil
}

func applyText(a config.ApplyTime) string {
	switch a {
	case config.ApplyLive:
		return "at once"
	case config.ApplyDockerd:
		return "dockerd restart"
	case config.ApplyVM:
		return "VM restart"
	case config.ApplyFirstStart:
		return "first start"
	}
	return string(a)
}

func configGet(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("config get", stderr)
	asJSON := fs.Bool("json", false, "print JSON")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("config get: want one KEY")
	}
	k, err := config.Lookup(pos[0])
	if err != nil {
		return err
	}
	in, err := openInstall()
	if err != nil {
		return err
	}
	it := k.Get(in.settings, in.env())
	if *asJSON {
		return writeJSON(stdout, ConfigGetJSON{Schema: SchemaVersion, Item: it})
	}
	_, _ = fmt.Fprintln(stdout, it.Value)
	return nil
}

func configPath(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("config path", stderr)
	asJSON := fs.Bool("json", false, "print JSON")
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	home, err := engine.DefaultHome()
	if err != nil {
		return err
	}
	l := engine.Layout{Home: home, Data: home}
	// The paths are shown even when settings.json is invalid: that is when they matter.
	if s, err := engine.LoadSettings(l); err == nil {
		l.Data = s.DataDir
	}
	p := ConfigPathJSON{Schema: SchemaVersion, Home: l.Home, SettingsFile: l.SettingsPath(), DaemonJSON: l.DaemonConfigPath(), DataDir: l.Data}
	if *asJSON {
		return writeJSON(stdout, p)
	}
	_, _ = fmt.Fprintf(stdout, "settings:    %s\ndaemon.json: %s\nstate:       %s\ndata:        %s\n", p.SettingsFile, p.DaemonJSON, p.Home, p.DataDir)
	return nil
}

func configSet(verb string, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("config "+verb, stderr)
	apply := fs.Bool("apply", false, "do the restart the change needs now (VM restart: stop and start)")
	imageDir := fs.String("image-dir", "", "with --apply: the guest image for the new start (default: the running engine's)")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	want := 2
	if verb == "unset" {
		want = 1
	}
	if len(pos) != want {
		if verb == "set" {
			return errors.New("config set: want KEY VALUE")
		}
		return errors.New("config unset: want KEY")
	}
	k, err := config.Lookup(pos[0])
	if err != nil {
		return err
	}
	in, err := openInstall()
	if err != nil {
		return err
	}
	ops, err := in.opsLock()
	if err != nil {
		return err
	}
	locked := true
	unlock := func() {
		if locked {
			_ = ops.Unlock()
			locked = false
		}
	}
	defer unlock()
	// Read again under the lock; an invalid or unreadable file stops here, unchanged.
	if err := in.reload(); err != nil {
		return err
	}
	env := in.env()
	before := k.Get(in.settings, env)
	var next *engine.Settings
	if verb == "set" {
		next, err = k.Set(in.settings, pos[1], env)
	} else {
		next, err = k.Unset(in.settings, env)
	}
	if err != nil {
		return err
	}
	after := k.Get(next, env)
	if in.settings != nil && config.Equal(next, in.settings) {
		_, _ = fmt.Fprintf(stdout, "%s is already %s; nothing changed.\n", k.Name, after.Value)
		return nil
	}
	if err := engine.SaveSettings(in.l, next); err != nil {
		return fmt.Errorf("write %s: %w", in.l.SettingsPath(), err)
	}
	_, _ = fmt.Fprintf(stdout, "%s: %s -> %s (%s)\n", k.Name, before.Value, after.Value, after.Source)
	if after.Source == config.SourceEnv {
		_, _ = fmt.Fprintf(stdout, "Note: the environment overrides this setting in this shell.\n")
	}
	up, err := in.running()
	if err != nil {
		return err
	}
	switch k.Apply {
	case config.ApplyLive:
		if k.Engine && up {
			return reloadLive(in, stdout)
		}
		_, _ = fmt.Fprintln(stdout, "Applies at once; nothing restarts.")
		return nil
	case config.ApplyFirstStart:
		_, _ = fmt.Fprintln(stdout, "Used when the data disk is created, at the first start.")
		return nil
	case config.ApplyDockerd:
		_, _ = fmt.Fprintln(stdout, "Applies when dockerd restarts: hyperbutler config daemon apply.")
		return nil
	}
	if !up {
		_, _ = fmt.Fprintln(stdout, "Applies at the next hyperbutler start.")
		return nil
	}
	if !*apply {
		_, _ = fmt.Fprintln(stdout, "The VM must restart to apply it: run `hyperbutler restart`, or repeat this with --apply. Nothing was restarted.")
		return nil
	}
	unlock()
	_, _ = fmt.Fprintln(stdout, "Restarting the VM (--apply).")
	return restart(*imageDir, 2*time.Minute, stdout, stderr)
}

// reloadLive asks the running engine to apply the live settings it uses (hostGateway.*)
// and waits until its state.json shows the result.
func reloadLive(in *install, stdout io.Writer) error {
	st, p, err := in.liveState()
	if err != nil {
		return err
	}
	p.Close()
	if st.Phase != engine.PhaseRunning || st.HostGateway == nil {
		// The engine applies settings.json as it is once its guest network is up.
		_, _ = fmt.Fprintf(stdout, "hyperbutler is %s; it applies this once the guest is up. Nothing restarts.\n", st.Phase)
		return nil
	}
	prev := st.HostGateway.Reloads
	if err := engine.RequestSettingsReload(st.PID, st.PIDCreated); err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			// An engine from a release without live settings: settings.json is saved.
			_, _ = fmt.Fprintln(stdout, "The running engine cannot apply settings live; this applies at the next hyperbutler start.")
			return nil
		}
		return err
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		cur, err := engine.LoadState(in.l)
		if err == nil && (cur.PID != st.PID || cur.PIDCreated != st.PIDCreated || !cur.Phase.Live()) {
			return errors.New("the engine stopped while applying the setting; see `hyperbutler logs`")
		}
		if err == nil && cur.HostGateway != nil && cur.HostGateway.Reloads > prev {
			h := cur.HostGateway
			if h.Error != "" {
				return fmt.Errorf("the running engine did not apply it: %s", printable([]byte(h.Error)))
			}
			state := "off"
			if h.Enabled {
				state = "on, ports " + h.Ports
			}
			_, _ = fmt.Fprintf(stdout, "Applied to the running engine; nothing restarted. host.docker.internal (%s): %s.\n", h.Address, state)
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("the running engine did not confirm the change within 10 s; see `hyperbutler logs`")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// --- daemon.json ------------------------------------------------------------------------

func configDaemon(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("config daemon: want validate or apply")
	}
	switch args[0] {
	case "validate":
		return daemonValidate(args[1:], stdout, stderr)
	case "apply":
		return daemonApply(args[1:], stdout, stderr)
	}
	return fmt.Errorf("config daemon: unknown subcommand %q (want validate or apply)", args[0])
}

func guestSubnet() daemoncfg.Options { return daemoncfg.Options{GuestSubnet: netstack.DefaultSubnet} }

func daemonValidate(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("config daemon validate", stderr)
	file := fs.String("file", "", "the file to check (default: the daemon.json hyperbutler uses)")
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	path := *file
	if path == "" {
		home, err := engine.DefaultHome()
		if err != nil {
			return err
		}
		path = engine.Layout{Home: home}.DaemonConfigPath()
	}
	b, err := engine.ReadFileBounded(path)
	if errors.Is(err, os.ErrNotExist) {
		_, _ = fmt.Fprintf(stdout, "There is no %s: dockerd runs with its defaults.\n", path)
		return nil
	}
	if err != nil {
		return err
	}
	if err := daemoncfg.Check(b, guestSubnet()); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	_, _ = fmt.Fprintf(stdout, "%s is valid for hyperbutler (%d bytes). dockerd checks it again (dockerd --validate) when it is applied.\n", path, len(b))
	return nil
}

func daemonApply(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("config daemon apply", stderr)
	timeout := fs.Duration("timeout", 3*time.Minute, "how long to wait for dockerd to restart")
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	in, err := openInstall()
	if err != nil {
		return err
	}
	path := in.l.DaemonConfigPath()
	// Check here first, for a clear message; the engine checks again.
	data, err := engine.ReadDaemonJSON(in.l, netstack.DefaultSubnet)
	if err != nil {
		return fmt.Errorf("%w\nNothing was applied; the previous daemon.json stays in effect", err)
	}
	ops, err := in.opsLock()
	if err != nil {
		return err
	}
	defer func() { _ = ops.Unlock() }()
	up, err := in.running()
	if err != nil {
		return err
	}
	if !up {
		if data == nil {
			_, _ = fmt.Fprintf(stdout, "There is no %s. hyperbutler is not running; dockerd starts with its defaults.\n", path)
		} else {
			_, _ = fmt.Fprintf(stdout, "%s is valid. hyperbutler is not running; dockerd starts with it at the next start.\n", path)
		}
		return nil
	}
	st, p, err := in.liveState()
	if err != nil {
		return err
	}
	p.Close()
	if st.Phase != engine.PhaseRunning {
		return fmt.Errorf("hyperbutler is %s; try again once it is running", st.Phase)
	}
	var gen uint64
	if st.Daemon != nil {
		gen = st.Daemon.Applies
	}
	if err := engine.RequestDaemonApply(st.PID, st.PIDCreated); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(stdout, "Restarting dockerd with", describeDaemonFile(path, data), "(the VM keeps running)...")
	t0 := time.Now()
	deadline := t0.Add(*timeout)
	for {
		cur, err := engine.LoadState(in.l)
		if err == nil && (cur.PID != st.PID || cur.PIDCreated != st.PIDCreated || !cur.Phase.Live()) {
			return errors.New("the engine stopped while applying daemon.json; see `hyperbutler logs`")
		}
		if err == nil && cur.Daemon != nil && cur.Daemon.Applies > gen {
			d := cur.Daemon
			if d.Error != "" {
				return fmt.Errorf("daemon.json not applied: %s", printable([]byte(d.Error)))
			}
			_, _ = fmt.Fprintf(stdout, "dockerd restarted with %s in %.1f s.\n", describeState(d), time.Since(t0).Seconds())
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("no result after %v; see `hyperbutler logs`", *timeout)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func describeDaemonFile(path string, data []byte) string {
	if data == nil {
		return "no daemon.json (dockerd defaults)"
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%s (%d bytes, sha256 %s)", path, len(data), hex.EncodeToString(sum[:6]))
}

func describeState(d *engine.DaemonState) string {
	switch d.Source {
	case engine.DaemonNone:
		return "no daemon.json (dockerd defaults)"
	case engine.DaemonPrevious:
		return fmt.Sprintf("the previous daemon.json (%d bytes)", d.Bytes)
	}
	return fmt.Sprintf("daemon.json (%d bytes, sha256 %s)", d.Bytes, d.SHA256[:12])
}

// daemonStatusLine summarizes the daemon.json delivery for `hyperbutler status`.
func daemonStatusLine(l engine.Layout, st *engine.State) string {
	b, err := engine.ReadFileBounded(l.DaemonConfigPath())
	file := "none"
	if err == nil {
		file = fmt.Sprintf("%d bytes", len(b))
	}
	if st == nil || st.Daemon == nil || !st.Phase.Live() {
		return fmt.Sprintf("%s (%s)", l.DaemonConfigPath(), file)
	}
	d := st.Daemon
	s := "in effect: " + describeState(d)
	if err == nil && d.Source == engine.DaemonFile {
		sum := sha256.Sum256(b)
		if hex.EncodeToString(sum[:]) != d.SHA256 {
			s += "; the file changed since (hyperbutler config daemon apply)"
		}
	} else if err == nil && d.Source == engine.DaemonNone && len(bytes.TrimSpace(b)) > 0 {
		s += "; the file is not applied yet (hyperbutler config daemon apply)"
	}
	if d.Error != "" {
		s += "; last problem: " + string(printable([]byte(d.Error)))
	}
	return s
}

// --- restart ------------------------------------------------------------------------------

func cmdRestart(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("restart", stderr)
	imageDir := fs.String("image-dir", "", "the guest image for the new start (default: the running engine's)")
	timeout := fs.Duration("timeout", 2*time.Minute, "how long to wait for the Docker API")
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	return restart(*imageDir, *timeout, stdout, stderr)
}

// restart stops hyperbutler if it runs and starts it again, with the same guest image
// unless imageDir says otherwise.
func restart(imageDir string, timeout time.Duration, stdout, stderr io.Writer) error {
	in, err := openInstall()
	if err != nil {
		return err
	}
	if imageDir == "" {
		if st, err := engine.LoadState(in.l); err == nil && st.ImageDir != "" {
			imageDir = st.ImageDir
		}
	}
	if err := cmdStop(nil, stdout, stderr); err != nil {
		return err
	}
	startArgs := []string{"--timeout", timeout.String()}
	if imageDir != "" {
		startArgs = append(startArgs, "--image-dir", imageDir)
	}
	return cmdStart(startArgs, stdout, stderr)
}
