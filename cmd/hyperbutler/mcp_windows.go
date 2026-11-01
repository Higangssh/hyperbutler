//go:build windows

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Microsoft/go-winio"

	"github.com/Higangssh/hyperbutler/internal/engine"
	"github.com/Higangssh/hyperbutler/internal/mcpserver"
)

// cmdMCP is `hyperbutler mcp` (REQ-UX-012): a Model Context Protocol server on stdin and
// stdout for an AI client, which starts it and ends it by closing stdin. See docs/mcp.md.
func cmdMCP(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := newFlagSet("mcp", stderr)
	allowWrite := fs.Bool("allow-write", false, "add the tools that change state: start, stop, container start/stop/restart, run, pull, config_set")
	allowDelete := fs.Bool("allow-delete", false, "with --allow-write: add the tools that delete: container_rm, image_rm, volume_rm, prune")
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	if *allowDelete && !*allowWrite {
		return usagef("mcp: %v", mcpserver.ErrDeleteNeedsWrite)
	}
	// stdout carries the protocol: anything else printed there would corrupt it, so
	// stray writes to os.Stdout go to stderr while the server runs.
	if f, ok := stdout.(*os.File); ok && f == os.Stdout {
		os.Stdout = os.Stderr
		defer func() { os.Stdout = f }()
	}
	o := mcpserver.Options{
		AllowWrite: *allowWrite, AllowDelete: *allowDelete,
		Host:    &mcpHost{},
		Dial:    func(ctx context.Context, pipe string) (net.Conn, error) { return winio.DialPipeContext(ctx, pipe) },
		Version: buildVersion(),
	}
	return mcpserver.Run(context.Background(), o, io.NopCloser(stdin), nopWriteCloser{stdout})
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// mcpHost runs the CLI's own commands for the MCP tools, with their output captured.
// The commands that change state run one at a time.
type mcpHost struct {
	mu sync.Mutex
}

// captured runs a command with its stdout and stderr captured; on failure the error
// carries what it printed.
func captured(f func(stdout, stderr io.Writer) error) ([]byte, error) {
	var out, errb bytes.Buffer
	err := f(&out, &errb)
	if err != nil {
		if s := strings.TrimSpace(errb.String()); s != "" {
			err = fmt.Errorf("%w (%s)", err, s)
		}
		return out.Bytes(), err
	}
	return out.Bytes(), nil
}

func (h *mcpHost) Status(context.Context) ([]byte, error) {
	return captured(func(stdout, stderr io.Writer) error {
		_, err := cmdStatus([]string{"--json"}, stdout, stderr)
		return err
	})
}

func (h *mcpHost) Settings(context.Context) ([]byte, error) {
	return captured(func(stdout, stderr io.Writer) error { return configList([]string{"--json"}, stdout, stderr) })
}

// Diagnose writes the zip to the user's temporary directory: the tool takes no path, so
// a client cannot make it write anywhere else.
func (h *mcpHost) Diagnose(context.Context) ([]byte, error) {
	out := filepath.Join(os.TempDir(), "hyperbutler-diagnose-"+time.Now().Format("20060102-150405")+".zip")
	return captured(func(stdout, stderr io.Writer) error {
		return cmdDiagnose([]string{"--json", "--out", out}, stdout, stderr)
	})
}

func (h *mcpHost) EngineLog(_ context.Context, console bool, lines int) ([]byte, error) {
	args := []string{"-n", strconv.Itoa(lines)}
	if console {
		args = append(args, "--console")
	}
	return captured(func(stdout, stderr io.Writer) error { return cmdLogs(args, stdout, stderr) })
}

func (h *mcpHost) Pipe(context.Context) (string, error) {
	in, err := openInstall()
	if err != nil {
		return "", err
	}
	up, err := in.running()
	if err != nil {
		return "", err
	}
	if !up {
		return "", mcpserver.ErrEngineStopped
	}
	st, err := engine.LoadState(in.l)
	if err != nil {
		return "", err
	}
	switch {
	case !st.Phase.Live():
		return "", mcpserver.ErrEngineStopped
	case st.Phase != engine.PhaseRunning || st.Pipe == "":
		return "", fmt.Errorf("the hyperbutler engine is %s, not running yet; try again shortly", st.Phase)
	case st.GuardPause != nil:
		return "", errors.New("the free-space guard paused the VM because the host volume is low on space; free space on it (the status tool says how much), and it resumes")
	}
	return st.Pipe, nil
}

func (h *mcpHost) Start(context.Context) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var args []string
	if in, err := openInstall(); err == nil {
		// The guest image the engine last ran, as restart picks it.
		if st, err := engine.LoadState(in.l); err == nil && st.ImageDir != "" {
			_, pinned := engine.ImagePin()
			if dir, ignored := restartImageDir(st.ImageDir, exeDir(), pinned, engine.CheckImageDirPin); ignored == nil && dir != "" {
				args = append(args, "--image-dir", dir)
			}
		}
	}
	b, err := captured(func(stdout, stderr io.Writer) error { return cmdStart(args, stdout, stdout) })
	return string(b), err
}

func (h *mcpHost) Stop(context.Context) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	b, err := captured(func(stdout, stderr io.Writer) error { return stopCommand(nil, stdout, stdout, "") })
	return string(b), err
}

func (h *mcpHost) ConfigSet(_ context.Context, key, value string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	// "--" ends the flags: a value such as "--apply" is a value, never a flag.
	b, err := captured(func(stdout, stderr io.Writer) error {
		return configSet("set", []string{"--", key, value}, stdout, stdout)
	})
	return string(b), err
}
