//go:build windows

// Command winbox-broker runs the HCS access broker (ADR 0004) in the foreground.
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

	"github.com/Higangssh/winbox/internal/broker"
	"golang.org/x/sys/windows"
)

const defaultPipe = `\\.\pipe\winbox-broker-dev`

// version is set at link time (-ldflags "-X main.version=...").
var version = "dev"

func main() {
	pipe := flag.String("pipe", defaultPipe, "named pipe to serve on")
	fake := flag.Bool("fake-backend", false, "use the in-memory fake backend (required for now)")
	flag.Parse()
	if err := run(*pipe, *fake); err != nil {
		fmt.Fprintln(os.Stderr, "winbox-broker:", err)
		os.Exit(1)
	}
}

func run(pipe string, fake bool) error {
	if !fake {
		return fmt.Errorf("no HCS backend yet; run with -fake-backend")
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
	defer srv.Close()
	l, err := broker.ListenPipe(broker.PipeConfig{Name: pipe, ClientSID: user})
	if err != nil {
		return err
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() {
		<-sig
		l.Close()
	}()
	log.Printf("serving %s for %s (fake backend)", pipe, user)
	return srv.Serve(l)
}
