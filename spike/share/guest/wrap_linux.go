package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"syscall"
	"time"

	"github.com/Higangssh/hyperbutler/internal/share/relay"
	"golang.org/x/sys/unix"
)

// wrapMain runs as PID 1 when the kernel command line has sb.wrap=1: it starts the
// share relay as a child and then execs the normal hyperbutler /init. This stands in for
// the integration ADR 0007 proposes (the relay inside hyperbutlerd).
func wrapMain() {
	if _, err := syscall.ForkExec("/sharebench", []string{"/sharebench", "relay-daemon"}, &syscall.ProcAttr{
		Env:   []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin"},
		Files: []uintptr{0, 1, 2},
	}); err != nil {
		fmt.Fprintln(os.Stderr, "sharebench: start relay:", err)
	}
	err := syscall.Exec("/init", []string{"/init"}, os.Environ())
	fmt.Fprintln(os.Stderr, "sharebench: exec /init:", err)
	os.Exit(1)
}

// wrapRequested checks for sbwrap=1 without /proc: the kernel hands PID 1 the
// parameters it does not know, as arguments (with a dot) or environment variables.
func wrapRequested() bool {
	for _, a := range os.Args[1:] {
		if a == "sbwrap=1" {
			return true
		}
	}
	return os.Getenv("sbwrap") == "1"
}

// relayDaemon waits for the system to come up, then mounts the host's shares at
// /mnt/hyperbutler and keeps them mounted.
func relayDaemon() {
	if f, err := os.Open("/sbmods/fuse.ko"); err == nil {
		unix.FinitModule(int(f.Fd()), "", 0)
		f.Close()
	}
	var port uint32
	var queues int
	for i := 0; i < 600; i++ {
		if p := cmdlineValue("sbfuseport"); p != "" {
			fmt.Sscan(p, &port)
			queues, _ = strconv.Atoi(cmdlineValue("sbqueues"))
			break
		}
		time.Sleep(50 * time.Millisecond) // /proc is not mounted yet
	}
	if queues <= 0 {
		queues = 1
	}
	for {
		if _, err := os.Stat("/dev/fuse"); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	for {
		var conns []io.ReadWriteCloser
		for len(conns) < queues {
			c, err := dialVsock(port, time.Minute)
			if err != nil {
				time.Sleep(time.Second)
				continue
			}
			conns = append(conns, c)
		}
		r, err := relay.Start(relay.Config{Mountpoint: "/mnt/hyperbutler", Conns: conns, Logf: logf})
		if err != nil {
			logf("relay: %v", err)
			for _, c := range conns {
				c.Close()
			}
			time.Sleep(time.Second)
			continue
		}
		logf("shares mounted at /mnt/hyperbutler (%d queues)", queues)
		logf("relay ended: %v", r.Wait())
		r.Close()
	}
}
