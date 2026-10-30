//go:build windows

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/Higangssh/winbox/internal/diag"
	"github.com/Higangssh/winbox/internal/engine"
	"github.com/Higangssh/winbox/internal/transport"
)

// runCmd is a thin wrapper over engine.Run: flags in, one VM run in the foreground.
func runCmd(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	kernel := fs.String("kernel", "", "guest kernel (vmlinuz-virt), required")
	initrd := fs.String("initrd", "", "guest initramfs with the winbox agent, required")
	cmdline := fs.String("cmdline", engine.DefaultCmdLine, "kernel command line")
	memMB := fs.Int("mem", 1024, "guest memory in MB (512 is too small for dockerd)")
	cpus := fs.Int("cpus", 2, "vCPUs")
	pipe := fs.String("pipe", transport.DefaultPipeName, "named pipe to serve the Docker API on")
	consoleLog := fs.String("console-log", "", "write the guest serial console to this file (default: discard)")
	dataDir := fs.String("data", "", "directory for a persistent data disk (data.vhdx); default: /var/lib/docker in guest RAM")
	diskSize := fs.String("disk-size", "64GiB", "data disk size when -data creates it")
	readyTimeout := fs.Duration("ready-timeout", 30*time.Second, "how long to wait for dockerd")
	exitWhenReady := fs.Bool("exit-when-ready", false, "stop after dockerd answers /_ping (boot timing only)")
	var dcfg diag.Config
	fs.StringVar(&dcfg.MemoryLimit, "memlimit", "", "Go soft memory limit, GOMEMLIMIT syntax (off by default)")
	fs.StringVar(&dcfg.DebugPipe, "debug-pipe", "", "serve pprof and runtime stats on this pipe, current user only (off by default)")
	var nf netFlags
	nf.register(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *kernel == "" || *initrd == "" || fs.NArg() != 0 {
		fs.Usage()
		return errors.New("run: -kernel and -initrd are required")
	}
	dbg, err := diag.Start(dcfg)
	if err != nil {
		return err
	}
	defer dbg.Close()
	sddl, err := engine.CurrentUserSDDL()
	if err != nil {
		return err
	}
	nc, extraSvcs, extraArgs, err := nf.config(sddl)
	if err != nil {
		return err
	}
	cfg := engine.Config{
		KernelPath: *kernel, InitrdPath: *initrd, CmdLine: *cmdline,
		MemoryMB: *memMB, CPUs: *cpus, Pipe: *pipe, Net: nc,
		ReadyTimeout: *readyTimeout, ExtraServices: extraSvcs, ExtraCmdLine: extraArgs,
		Logger: slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}
	if *dataDir != "" {
		abs, err := filepath.Abs(*dataDir)
		if err != nil {
			return err
		}
		if cfg.DiskSize, err = engine.ParseSize(*diskSize); err != nil {
			return fmt.Errorf("-disk-size: %w", err)
		}
		cfg.Disk = engine.Layout{Home: abs, Data: abs}
	}
	if *consoleLog != "" {
		f, err := os.Create(*consoleLog)
		if err != nil {
			return err
		}
		defer f.Close()
		cfg.ConsoleLog = f
	}
	cfg.OnReady = func(ctx context.Context, r *engine.Running) error {
		if nf.benchBytes > 0 && r.Stack != nil {
			step := func(f string, a ...any) { cfg.Logger.Info(fmt.Sprintf(f, a...)) }
			if err := (&benchEnv{st: r.Stack}).bench(ctx, r.RuntimeID, nf.benchBytes, step); err != nil {
				return err
			}
			return engine.ErrDone
		}
		if *exitWhenReady {
			return engine.ErrDone
		}
		return nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	fmt.Println("Ctrl+C to stop")
	return engine.Run(ctx, cfg)
}
