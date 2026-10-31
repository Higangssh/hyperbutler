//go:build windows

// Command hyperbutler-broker runs the HCS access broker (ADR 0004) in the foreground.
//
// It does not install, register or start a Windows service, and it needs no elevation:
// run as a normal user, it serves that user only. Installing it as a service is a
// separate, explicit step that is not part of this command.
//
// No HCS backend exists yet, so -fake-backend is required; it keeps VM state in memory
// and starts nothing.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"

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

func main() {
	pipe := flag.String("pipe", defaultPipe, "named pipe to serve on")
	fake := flag.Bool("fake-backend", false, "use the in-memory fake backend (required for now)")
	var dcfg diag.Config
	flag.StringVar(&dcfg.MemoryLimit, "memlimit", defaultMemLimit, "Go soft memory limit, GOMEMLIMIT syntax (the GOMEMLIMIT variable wins)")
	flag.StringVar(&dcfg.DebugPipe, "debug-pipe", "", `serve pprof and runtime stats on this pipe, current user only (off when empty), e.g. \\.\pipe\hyperbutler-broker-debug`)
	flag.Parse()
	if err := run(*pipe, *fake, dcfg); err != nil {
		fmt.Fprintln(os.Stderr, "hyperbutler-broker:", err)
		os.Exit(1)
	}
}

func run(pipe string, fake bool, dcfg diag.Config) error {
	if !fake {
		return fmt.Errorf("no HCS backend yet; run with -fake-backend")
	}
	// The debug pipe's DACL names the user's SID, which an elevated token shares with
	// the same user's medium-integrity processes. Exposing heap dumps and profiles of an
	// elevated broker to those would cross the ADR 0004 boundary, so refuse it.
	if dcfg.DebugPipe != "" && windows.GetCurrentProcessToken().IsElevated() {
		return fmt.Errorf("-debug-pipe is refused in an elevated broker")
	}
	dbg, err := diag.Start(dcfg)
	if err != nil {
		return err
	}
	defer func() { _ = dbg.Close() }()
	if dcfg.DebugPipe != "" {
		log.Printf("debug endpoint on %s", dcfg.DebugPipe)
	}
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	user := tu.User.Sid.String()
	srv, err := broker.New(broker.Config{
		Backend:     broker.NewFakeBackend(),
		Version:     version,
		AllowedSIDs: []string{user},
		Logf:        log.Printf,
	})
	if err != nil {
		return err
	}
	defer func() { _ = srv.Close() }()
	l, err := broker.ListenPipe(broker.PipeConfig{Name: pipe, ClientSID: user})
	if err != nil {
		return err
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() {
		<-sig
		_ = l.Close()
	}()
	log.Printf("serving %s for %s (fake backend)", pipe, user)
	return srv.Serve(l)
}
