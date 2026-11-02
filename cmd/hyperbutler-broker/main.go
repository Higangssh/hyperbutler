//go:build windows

// Command hyperbutler-broker runs the HCS access broker (ADR 0004) in the foreground.
//
// It does not install, register or start a Windows service. Installing it as a service
// is a separate, explicit step that is not part of this command.
//
// One backend must be chosen:
//
//   - -fake-backend keeps VM state in memory and starts nothing.
//   - -hcs-backend runs VMs through HCS, so it needs what HCS needs: elevation, or
//     membership in Hyper-V Administrators. It boots only the guest image in the image\
//     folder next to this executable, checked against the SHA-256 pin compiled in; a
//     development build carries no pin and needs -allow-unpinned-image. At start it
//     terminates the VMs that carry the broker's owner tag and that it does not run (left
//     by an earlier broker), and on exit it terminates its own.
//
// Run as a normal user, it serves that user only.
package main

import (
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/broker"
	"github.com/Higangssh/hyperbutler/internal/diag"
)

const defaultPipe = `\\.\pipe\hyperbutler-broker-dev`

// version is set at link time (-ldflags "-X main.version=...").
var version = "dev"

// defaultMemLimit is the broker's Go soft memory limit (ADR 0003, guardrail 4). The
// broker has no data plane, so its heap stays small; GOMEMLIMIT in the environment
// overrides this.
const defaultMemLimit = "64MiB"

type options struct {
	pipe          string
	fake, hcs     bool
	allowUnpinned bool
	dcfg          diag.Config
}

func main() {
	var o options
	flag.StringVar(&o.pipe, "pipe", defaultPipe, "named pipe to serve on")
	flag.BoolVar(&o.fake, "fake-backend", false, "use the in-memory fake backend, which starts no VM")
	flag.BoolVar(&o.hcs, "hcs-backend", false, `run VMs through HCS (needs elevation or Hyper-V Administrators), from the image\ folder next to this executable`)
	flag.BoolVar(&o.allowUnpinned, "allow-unpinned-image", false, "with -hcs-backend in a development build (no image pin compiled in): boot the image checked against its own image.json only")
	flag.StringVar(&o.dcfg.MemoryLimit, "memlimit", defaultMemLimit, "Go soft memory limit, GOMEMLIMIT syntax (the GOMEMLIMIT variable wins)")
	flag.StringVar(&o.dcfg.DebugPipe, "debug-pipe", "", `serve pprof and runtime stats on this pipe, current user only (off when empty), e.g. \\.\pipe\hyperbutler-broker-debug`)
	flag.Parse()
	if err := run(o); err != nil {
		fmt.Fprintln(os.Stderr, "hyperbutler-broker:", err)
		os.Exit(1)
	}
}

func run(o options) error {
	if o.fake == o.hcs {
		return errors.New("choose one backend: -fake-backend or -hcs-backend")
	}
	if o.allowUnpinned && !o.hcs {
		return errors.New("-allow-unpinned-image needs -hcs-backend")
	}
	// The debug pipe's DACL names the user's SID, which an elevated token shares with
	// the same user's medium-integrity processes. Exposing heap dumps and profiles of an
	// elevated broker to those would cross the ADR 0004 boundary, so refuse it.
	if o.dcfg.DebugPipe != "" && windows.GetCurrentProcessToken().IsElevated() {
		return fmt.Errorf("-debug-pipe is refused in an elevated broker")
	}
	dbg, err := diag.Start(o.dcfg)
	if err != nil {
		return err
	}
	defer func() { _ = dbg.Close() }()
	if o.dcfg.DebugPipe != "" {
		log.Printf("debug endpoint on %s", o.dcfg.DebugPipe)
	}
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	user := tu.User.Sid.String()

	var backend broker.Backend = broker.NewFakeBackend()
	what := "fake backend"
	if o.hcs {
		state, err := privateDir(user)
		if err != nil {
			return err
		}
		defer func() { _ = os.RemoveAll(state) }()
		hb, err := broker.NewHCSBackend(broker.HCSConfig{StateDir: state, AllowUnpinned: o.allowUnpinned, Logf: log.Printf})
		if err != nil {
			return err
		}
		// Runs after the server and the listener are closed, whose leases end first.
		defer func() { _ = hb.Close() }()
		if n := hb.Sweep(); n > 0 {
			log.Printf("terminated %d VM(s) an earlier broker left", n)
		}
		backend, what = hb, "HCS backend"
	}
	srv, err := broker.New(broker.Config{
		Backend:     backend,
		Version:     version,
		AllowedSIDs: []string{user},
		Logf:        log.Printf,
	})
	if err != nil {
		return err
	}
	defer func() { _ = srv.Close() }()
	l, err := broker.ListenPipe(broker.PipeConfig{Name: o.pipe, ClientSID: user})
	if err != nil {
		return err
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() {
		<-sig
		_ = l.Close()
	}()
	log.Printf("serving %s for %s (%s)", o.pipe, user, what)
	return srv.Serve(l)
}

// privateDir creates a directory in the temporary directory with a protected DACL for
// sid only, for the backend's own files.
func privateDir(sid string) (string, error) {
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;" + sid + ")")
	if err != nil {
		return "", err
	}
	sa := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	for i := 0; i < 10; i++ {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", err
		}
		dir := filepath.Join(os.TempDir(), fmt.Sprintf("hyperbutler-broker-%x", b))
		p, err := windows.UTF16PtrFromString(dir)
		if err != nil {
			return "", err
		}
		err = windows.CreateDirectory(p, sa)
		if err == nil {
			return dir, nil
		}
		if !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			return "", err
		}
	}
	return "", errors.New("cannot create a private state directory")
}
