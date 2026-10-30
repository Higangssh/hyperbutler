//go:build windows

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/winbox/internal/transport"
	"github.com/Higangssh/winbox/internal/vm"
)

func runCmd(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	kernel := fs.String("kernel", "", "guest kernel (vmlinuz-virt), required")
	initrd := fs.String("initrd", "", "guest initramfs with the winbox agent, required")
	cmdline := fs.String("cmdline", "console=ttyS0 quiet", "kernel command line")
	memMB := fs.Int("mem", 1024, "guest memory in MB (512 is too small for dockerd)")
	cpus := fs.Int("cpus", 2, "vCPUs")
	port := fs.Uint("port", 2375, "AF_VSOCK port the guest agent listens on")
	pipe := fs.String("pipe", transport.DefaultPipeName, "named pipe to serve the Docker API on")
	consoleLog := fs.String("console-log", "", "write the guest serial console to this file (default: discard)")
	readyTimeout := fs.Duration("ready-timeout", 30*time.Second, "how long to wait for dockerd")
	exitWhenReady := fs.Bool("exit-when-ready", false, "stop after dockerd answers /_ping (boot timing only)")
	var nf netFlags
	nf.register(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *kernel == "" || *initrd == "" || fs.NArg() != 0 || *port > 0xFFFFFFFE {
		fs.Usage()
		return errors.New("run: -kernel and -initrd are required")
	}
	kpath, err := filepath.Abs(*kernel)
	if err != nil {
		return err
	}
	ipath, err := filepath.Abs(*initrd)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	t0 := time.Now()
	step := func(format string, a ...any) {
		fmt.Printf("[%7.1f ms] %s\n", float64(time.Since(t0).Microseconds())/1000, fmt.Sprintf(format, a...))
	}

	sddl, err := currentUserSDDL()
	if err != nil {
		return err
	}
	// Listen first: if the pipe name is taken (another winbox, or a squatter), fail
	// before booting anything.
	ln, err := transport.ListenPipe(*pipe)
	if err != nil {
		return err
	}
	defer ln.Close()

	gnet, netSvcs, netArgs, err := setupNet(&nf, sddl)
	if err != nil {
		return err
	}
	if gnet != nil {
		defer gnet.close()
	}

	id := "winbox-dev-" + randomHex(8)
	consolePipe := `\\.\pipe\` + id + "-com1"
	svc := transport.VsockServiceID(uint32(*port))
	sys, err := vm.Create(id, &vm.Config{
		Owner:          "winbox-dev",
		KernelPath:     kpath,
		InitrdPath:     ipath,
		KernelCmdLine:  *cmdline + netArgs,
		MemoryMB:       *memMB,
		ProcessorCount: *cpus,
		ConsolePipe:    consolePipe,
		Services:       append([]vm.Service{{ID: svc, ConnectSDDL: sddl}}, netSvcs...),
	})
	if err != nil {
		return err
	}
	defer func() {
		if err := sys.Terminate(); err != nil {
			fmt.Fprintln(os.Stderr, "terminate:", err)
		}
		_ = sys.Close()
		step("VM terminated")
	}()
	step("created %s", id)
	if err := sys.Start(); err != nil {
		return err
	}
	step("started")

	console, err := openConsole(ctx, consolePipe)
	if err != nil {
		return err
	}
	defer console.Close()
	var logw io.Writer = io.Discard
	if *consoleLog != "" {
		f, err := os.Create(*consoleLog)
		if err != nil {
			return err
		}
		defer f.Close()
		logw = f
	}
	go streamConsole(console, logw, step)

	vmid, err := sys.RuntimeID()
	if err != nil {
		return err
	}
	target := transport.HvsockTarget{VMID: vmid, ServiceID: svc}
	step("runtime id %s, hvsock target %s", vmid, target)
	if gnet != nil {
		if err := gnet.start(ctx, vmid, step); err != nil {
			return err
		}
	}

	if err := waitPing(ctx, target, *readyTimeout); err != nil {
		return err
	}
	step("dockerd answered /_ping over hvsock")
	if gnet != nil && nf.benchBytes > 0 {
		return gnet.bench(ctx, vmid, nf.benchBytes, step)
	}
	if *exitWhenReady {
		return nil
	}
	if gnet != nil && nf.autoPublish {
		gnet.startAutoPublish(ctx, target, step)
	}

	p := &transport.Proxy{
		Upstream: &transport.HvsockDialer{Target: target, Retries: 2, RetryWait: 100 * time.Millisecond},
		// Match the guest agent's -max-conns, so excess clients wait for a pipe instance
		// instead of timing out on an hvsock dial the agent will not accept.
		MaxConns: agentMaxConns,
		Logger:   slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}
	step("serving %s (Ctrl+C to stop)", *pipe)
	return p.Serve(ctx, ln)
}

// agentMaxConns is the guest agent's default -max-conns.
const agentMaxConns = 256

// currentUserSDDL grants hvsock connect access to the user running this process only.
func currentUserSDDL() (string, error) {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("query token user: %w", err)
	}
	return vm.UserOnlySDDL(tu.User.Sid.String())
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// openConsole connects to the COM1 pipe that the VM worker serves.
func openConsole(ctx context.Context, name string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		// SQOS identification: the pipe server must not be able to impersonate us.
		h, err := windows.CreateFile(p, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING,
			windows.SECURITY_SQOS_PRESENT|windows.SECURITY_IDENTIFICATION, 0)
		if err == nil {
			return os.NewFile(uintptr(h), name), nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("open console %s: %w", name, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// maxConsoleLine bounds one console line kept for step reporting. Longer lines still go
// to w in full; only their first maxConsoleLine bytes are considered.
const maxConsoleLine = 4 << 10

// streamConsole copies the guest console to w and reports the init's "winbox-t" timing
// markers and agent lines as steps. The guest is untrusted: reported lines have control
// characters escaped so they cannot drive the host terminal.
func streamConsole(r io.Reader, w io.Writer, step func(string, ...any)) {
	br := bufio.NewReaderSize(r, maxConsoleLine)
	first, cont := true, false
	for {
		chunk, err := br.ReadSlice('\n')
		if len(chunk) > 0 {
			if first {
				step("first console output")
				first = false
			}
			_, _ = w.Write(chunk)
			line := strings.TrimRight(string(chunk), "\r\n")
			if !cont && reportLine(line) {
				step("guest: %s", sanitize(line))
			}
			cont = !bytes.HasSuffix(chunk, []byte("\n"))
		}
		if err != nil && !errors.Is(err, bufio.ErrBufferFull) {
			return
		}
	}
}

func reportLine(line string) bool {
	return strings.HasPrefix(line, "winbox-t ") && !strings.Contains(line, " mem ") ||
		strings.HasPrefix(line, "winbox-agent: ")
}

// sanitize escapes control characters and invalid UTF-8.
func sanitize(s string) string {
	q := strconv.QuoteToGraphic(s)
	return q[1 : len(q)-1]
}

// waitPing polls GET /_ping over hvsock until dockerd answers 200 OK.
func waitPing(ctx context.Context, target transport.HvsockTarget, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	d := &transport.HvsockDialer{Target: target}
	client := &http.Client{Transport: &http.Transport{
		DialContext:       func(ctx context.Context, _, _ string) (net.Conn, error) { return d.DialContext(ctx) },
		DisableKeepAlives: true,
	}}
	var last error
	for {
		// A connect issued before the agent listens hangs instead of failing, so keep each
		// attempt short.
		actx, acancel := context.WithTimeout(ctx, 500*time.Millisecond)
		req, _ := http.NewRequestWithContext(actx, http.MethodGet, "http://docker/_ping", nil)
		resp, err := client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				acancel()
				return nil
			}
			err = fmt.Errorf("status %s", resp.Status)
		}
		acancel()
		last = err
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.Canceled) {
				return ctx.Err()
			}
			return fmt.Errorf("dockerd not ready after %v: %v", timeout, last)
		case <-time.After(20 * time.Millisecond):
		}
	}
}
