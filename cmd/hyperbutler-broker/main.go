//go:build windows

// Command hyperbutler-broker runs the HCS access broker (ADR 0004), as a Windows service
// or in the foreground.
//
// As a service:
//
//   - install (elevated) registers the service hyperbutler-broker, which runs
//     "hyperbutler-broker.exe" run as LocalSystem at boot (delayed), with its Event Log
//     source, and starts it. It refuses an executable that anyone but SYSTEM,
//     Administrators or TrustedInstaller could replace, such as one in a user's folder:
//     the service runs it as SYSTEM. The MSI installs it in Program Files.
//   - uninstall (elevated) stops and deletes the service and removes its source.
//   - run is what the service control manager starts. It serves \\.\pipe\hyperbutler-broker
//     to interactive users with the HCS backend, and logs to the Event Log only.
//
// A development build (version dev, no image pin) also takes -fake-backend for install
// and run, which the integration test uses; it is not in the usage text.
//
// In the foreground (no subcommand), one backend must be chosen:
//
//   - -fake-backend keeps VM state in memory and starts nothing.
//   - -hcs-backend runs VMs through HCS, so it needs what HCS needs: elevation, or
//     membership in Hyper-V Administrators. It boots only the guest image in the image\
//     folder next to this executable, checked against the SHA-256 pin compiled in; a
//     development build carries no pin and needs -allow-unpinned-image, which a release
//     build refuses. At start it terminates the VMs that carry its user's owner tag and
//     that it does not run (left by an earlier broker), and on exit it terminates its own.
//
// Run in the foreground as a normal user, it serves that user only.
package main

import (
	"crypto/rand"
	"encoding/json"
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
	"github.com/Higangssh/hyperbutler/internal/imageset"
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
	if len(os.Args) > 1 {
		if cmd, ok := subcommands[os.Args[1]]; ok {
			if err := cmd(os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, "hyperbutler-broker:", err)
				os.Exit(1)
			}
			return
		}
	}
	var o options
	flag.StringVar(&o.pipe, "pipe", defaultPipe, "named pipe to serve on")
	flag.BoolVar(&o.fake, "fake-backend", false, "use the in-memory fake backend, which starts no VM")
	flag.BoolVar(&o.hcs, "hcs-backend", false, `run VMs through HCS (needs elevation or Hyper-V Administrators), from the image\ folder next to this executable`)
	flag.BoolVar(&o.allowUnpinned, "allow-unpinned-image", false, "with -hcs-backend in a development build (no image pin compiled in): boot the image checked against its own image.json only")
	flag.StringVar(&o.dcfg.MemoryLimit, "memlimit", defaultMemLimit, "Go soft memory limit, GOMEMLIMIT syntax (the GOMEMLIMIT variable wins)")
	versionJSON := flag.Bool("version-json", false, "print the broker version and its guest image pin as JSON, and exit (dist/package.ps1 checks them)")
	flag.Usage = func() {
		w := flag.CommandLine.Output()
		_, _ = fmt.Fprintln(w, "usage: hyperbutler-broker install | uninstall | run    (the Windows service; install and uninstall as administrator)")
		_, _ = fmt.Fprintln(w, "       hyperbutler-broker -fake-backend | -hcs-backend [flags]    (in the foreground)")
		flag.PrintDefaults()
	}
	flag.Parse()
	if *versionJSON {
		pin, pinned := imageset.Pin()
		b, _ := json.Marshal(struct {
			Version             string `json:"version"`
			ImagePinned         bool   `json:"imagePinned"`
			ImageManifestSha256 string `json:"imageManifestSha256,omitempty"`
		}{version, pinned, pin})
		fmt.Println(string(b))
		return
	}
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
	if _, pinned := imageset.Pin(); o.allowUnpinned && (pinned || version != "dev") {
		// A release build boots only the image it was built with.
		return errors.New("-allow-unpinned-image is for development builds only")
	}
	// No debug endpoint: its pipe listener is go-winio's, which the broker may not link
	// (ADR 0004 rule 10).
	if _, err := diag.Start(o.dcfg); err != nil {
		return err
	}
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	user := tu.User.Sid.String()

	var backend broker.Backend = broker.NewFakeBackend()
	what := "fake backend"
	if o.hcs {
		state, err := privateDir(os.TempDir(), user)
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
		// In the foreground the broker serves its own user only, so it sweeps only that
		// user's VMs; another user's broker may run theirs.
		if n := hb.Sweep(broker.OwnerTag(user)); n > 0 {
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
	// The shared endpoint and this user's own, as the service serves them (ADR 0004
	// rule 5); AllowedSIDs keeps every other user out.
	eps, err := broker.ListenEndpoints(srv, broker.EndpointsConfig{Name: o.pipe})
	if err != nil {
		return err
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() {
		<-sig
		_ = eps.Close()
	}()
	log.Printf("serving %s for %s (%s)", o.pipe, user, what)
	return eps.Serve()
}

// privateDir creates a directory with a random name in base, with a protected DACL for
// sid only, for the backend's own files.
func privateDir(base, sid string) (string, error) {
	// The owner is set too: an elevated token's default owner is Administrators,
	// and removeStaleStateDirs removes only folders the account owns.
	sd, err := windows.SecurityDescriptorFromString("O:" + sid + "D:P(A;OICI;FA;;;" + sid + ")")
	if err != nil {
		return "", err
	}
	sa := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	for i := 0; i < 10; i++ {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", err
		}
		dir := filepath.Join(base, fmt.Sprintf("hyperbutler-broker-%x", b))
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
