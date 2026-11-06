//go:build windows

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"time"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/engine"
	"github.com/Higangssh/hyperbutler/internal/imageset"
	"github.com/Higangssh/hyperbutler/internal/sandbox/copyin"
	"github.com/Higangssh/hyperbutler/internal/sandbox/session"
	"github.com/Higangssh/hyperbutler/internal/sandbox/wire"
	"github.com/Higangssh/hyperbutler/internal/transport"
	"github.com/Higangssh/hyperbutler/internal/vm"
	"github.com/Higangssh/hyperbutler/internal/vmsecret"
	"github.com/Higangssh/hyperbutler/internal/vmtemplate"
)

// sandboxCmd boots the agent sandbox image (ADR 0009) in this process, copies a project
// in through sbxd and lists the session's changes: a check of the image and the guest
// agent until the engine's session manager exists. The VM keeps the session in guest RAM
// (no data disk) and ends with this process.
func sandboxCmd(args []string) error {
	fs := flag.NewFlagSet("sandbox", flag.ContinueOnError)
	manifest := fs.String("image", "", "sandbox image manifest (image.json from guest/sandbox/build.ps1)")
	project := fs.String("project", "", "project directory to copy in")
	memMB := fs.Int("mem", 2048, "guest memory in MB")
	cpus := fs.Int("cpus", 2, "vCPUs")
	consoleLog := fs.String("console-log", "", "write the guest serial console to this file (default: discard)")
	wait := fs.Bool("wait", false, "after the review, keep the VM until Ctrl+C and review again on each Enter")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *manifest == "" || *project == "" || fs.NArg() != 0 {
		fs.Usage()
		return errors.New("sandbox: -image and -project are required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	plan, err := copyin.Select(*project, copyin.Options{})
	if err != nil {
		return err
	}
	fmt.Printf("project: %d files, %d bytes, %d skipped\n", len(plan.Files), plan.Total, len(plan.Skipped))
	for _, s := range plan.Skipped {
		fmt.Printf("  skipped %s (%s)\n", s.Path, s.Reason)
	}

	set, err := imageset.Open(*manifest, "")
	if err != nil {
		return err
	}
	defer func() { _ = set.Close() }()
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	secret, err := vmsecret.New()
	if err != nil {
		return err
	}
	id := "hyperbutler-sbx-" + randomSuffix()
	consolePipe := `\\.\pipe\` + id + "-com1"
	cfg, err := vmtemplate.Build(&vmtemplate.Spec{
		Owner: engine.VMOwner, UserSID: tu.User.Sid.String(), Secret: secret,
		MemoryMB: *memMB, CPUs: *cpus,
		Kernel: set.Kernel, Initrd: set.Initrd, System: set.System,
		Sandbox: true, ConsolePipe: consolePipe,
	})
	if err != nil {
		return err
	}
	for _, p := range []string{set.Kernel, set.Initrd, set.System} {
		if err := vm.GrantVMAccess(id, p); err != nil {
			return fmt.Errorf("grant %s: %w", p, err)
		}
		defer func() { _ = vm.RevokeVMAccess(id, p) }()
	}
	start := time.Now()
	m, err := vm.Create(id, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = m.Terminate(); _ = m.Close() }()
	if err := m.Start(); err != nil {
		return err
	}
	if err := followConsole(ctx, consolePipe, *consoleLog); err != nil {
		return err
	}
	rid, err := m.RuntimeID()
	if err != nil {
		return err
	}
	// A connect to a port nobody listens on yet stays pending until hvsock's own timeout
	// (about 30 s), so each attempt gets one second and the next starts at once.
	dialer := func(port uint32) transport.Dialer {
		d := &transport.HvsockDialer{Target: transport.HvsockTarget{VMID: rid, ServiceID: transport.VsockServiceID(port), Secret: secret}}
		return transport.DialerFunc(func(ctx context.Context) (net.Conn, error) {
			ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()
			for {
				actx, acancel := context.WithTimeout(ctx, time.Second)
				c, err := d.DialContext(actx)
				acancel()
				if err == nil || ctx.Err() != nil {
					return c, err
				}
				time.Sleep(50 * time.Millisecond)
			}
		})
	}
	s := &session.Session{Copy: dialer(vmtemplate.SandboxCopyPort), Changes: dialer(vmtemplate.SandboxPort)}
	sent, err := s.CopyIn(ctx, *project, plan)
	if err != nil {
		return fmt.Errorf("copy in: %w", err)
	}
	fmt.Printf("copied %d entries in %s after the start\n", len(sent), time.Since(start).Round(time.Millisecond))
	for {
		items, err := s.Review(ctx, sent)
		if err != nil {
			return fmt.Errorf("review: %w", err)
		}
		fmt.Printf("%d changes\n", len(items))
		for _, it := range items {
			note := ""
			if it.Risk != "" {
				note += " risk=" + string(it.Risk)
			}
			if it.Refused != "" {
				note += " refused: " + it.Refused
			}
			fmt.Printf("  %-8s %s%s\n", kindName[it.Kind], it.Shown, note)
		}
		if !*wait {
			return nil
		}
		fmt.Println("Enter to review again, Ctrl+C to end the session")
		line := make(chan error, 1)
		go func() { _, err := fmt.Scanln(); line <- err }()
		select {
		case <-ctx.Done():
			return nil
		case err := <-line:
			if errors.Is(err, io.EOF) {
				return nil
			}
		}
	}
}

var kindName = map[wire.Kind]string{wire.Added: "added", wire.Modified: "modified", wire.Deleted: "deleted"}

func randomSuffix() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// followConsole copies the guest's COM1 to path (or discards it) until the VM closes it.
// The console is the guest's, so it never goes to the terminal raw.
func followConsole(ctx context.Context, pipe, path string) error {
	p, err := windows.UTF16PtrFromString(pipe)
	if err != nil {
		return err
	}
	var h windows.Handle
	for deadline := time.Now().Add(10 * time.Second); ; {
		// SQOS identification: the pipe server must not be able to impersonate us.
		h, err = windows.CreateFile(p, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING,
			windows.SECURITY_SQOS_PRESENT|windows.SECURITY_IDENTIFICATION, 0)
		if err == nil {
			break
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			return fmt.Errorf("open console %s: %w", pipe, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	console := os.NewFile(uintptr(h), pipe)
	w := io.Discard
	if path != "" {
		f, err := os.Create(path)
		if err != nil {
			_ = console.Close()
			return err
		}
		w = f
	}
	go func() {
		_, _ = io.Copy(w, console)
		_ = console.Close()
		if f, ok := w.(*os.File); ok {
			_ = f.Close()
		}
	}()
	return nil
}
