//go:build windows

package hostmon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/Higangssh/hyperbutler/internal/engine"
	"github.com/Higangssh/hyperbutler/internal/transport"
)

// Hyperbutler drives one hyperbutler installation through its CLI, the way a user
// does: `hyperbutler start` and `hyperbutler stop` with HYPERBUTLER_HOME set. It never
// touches the user's docker context (--no-context).
type Hyperbutler struct {
	Exe      string // hyperbutler.exe
	Home     string // HYPERBUTLER_HOME; required, so a bench never uses the user's install
	ImageDir string // --image-dir; empty: next to Exe
}

// Layout is the installation's file layout.
func (h *Hyperbutler) Layout() engine.Layout { return engine.Layout{Home: h.Home, Data: h.Home} }

// Start runs `hyperbutler start --no-context` with extra flags and returns its output.
func (h *Hyperbutler) Start(ctx context.Context, extra ...string) (string, error) {
	args := []string{"start", "--no-context"}
	if h.ImageDir != "" {
		args = append(args, "--image-dir", h.ImageDir)
	}
	return h.run(ctx, append(args, extra...)...)
}

// Stop runs `hyperbutler stop`.
func (h *Hyperbutler) Stop(ctx context.Context) (string, error) { return h.run(ctx, "stop") }

func (h *Hyperbutler) run(ctx context.Context, args ...string) (string, error) {
	if h.Home == "" {
		return "", errors.New("hostmon: no HYPERBUTLER_HOME for the bench's installation")
	}
	cmd := exec.CommandContext(ctx, h.Exe, args...) //nolint:gosec // G204: the operator's hyperbutler.exe
	cmd.Env = append(os.Environ(), engine.HomeEnv+"="+h.Home)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	s := strings.TrimSpace(out.String())
	if err != nil {
		return s, fmt.Errorf("hyperbutler %s: %w: %s", args[0], err, s)
	}
	return s, nil
}

// State reads the installation's state.json.
func (h *Hyperbutler) State() (*engine.State, error) { return engine.LoadState(h.Layout()) }

// Running reports whether an engine holds this installation's lock.
func (h *Hyperbutler) Running() (bool, error) { return engine.Held(h.Layout().LockPath()) }

// DockerPipe is the Docker API pipe the installation serves (from settings.json, or
// the default before the first start).
func (h *Hyperbutler) DockerPipe() string {
	if s, err := engine.LoadSettings(h.Layout()); err == nil {
		return s.PipeName()
	}
	return transport.DefaultPipeName
}

// Docker runs the stock docker CLI against one endpoint, with a private DOCKER_CONFIG
// so the user's contexts and config never apply.
type Docker struct {
	Exe       string
	Host      string // -H, e.g. npipe:////./pipe/docker_engine_hyperbutler
	ConfigDir string
}

// Run runs one docker command and returns its stdout.
func (d *Docker) Run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, d.Exe, append([]string{"-H", d.Host}, args...)...) //nolint:gosec // G204: the operator's docker.exe
	cmd.Env = append(os.Environ(), "DOCKER_CONFIG="+d.ConfigDir, "DOCKER_CLI_HINTS=false")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("docker %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// NpipeHost is the docker -H URL of a local pipe name.
func NpipeHost(pipe string) string { return "npipe://" + strings.ReplaceAll(pipe, `\`, "/") }

// WaitQuiet waits until the Docker API pipe is free and no other VM or hyperbutler
// engine runs, polling every poll, for at most wait. Another agent or user may be
// running VMs on the same machine; a measurement that overlaps them is disturbed. It
// returns what was still running when it gave up (Any() false means quiet), or the
// context's error.
func WaitQuiet(ctx context.Context, pipe string, poll, wait time.Duration, logf func(string, ...any)) (Foreign, bool, error) {
	deadline := time.Now().Add(wait)
	for {
		busy, err := PipeExists(pipe)
		if err != nil {
			return Foreign{}, false, err
		}
		ps, err := Snapshot()
		if err != nil {
			return Foreign{}, false, err
		}
		f := FindForeign(ps, nil)
		if !busy && !f.Any() {
			return f, false, nil
		}
		if time.Now().After(deadline) {
			return f, busy, nil
		}
		if logf != nil {
			logf("waiting for a quiet host: pipe busy=%v, foreign: %s", busy, f)
		}
		select {
		case <-ctx.Done():
			return f, busy, ctx.Err()
		case <-time.After(poll):
		}
	}
}
