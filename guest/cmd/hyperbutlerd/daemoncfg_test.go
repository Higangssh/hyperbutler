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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Higangssh/hyperbutler/internal/guestctl"
)

// fakeInit plays the guest init: a restart starts a new "dockerd", which answers only
// if the installed file does not contain "bad".
type fakeInit struct {
	t        *testing.T
	d        *dockerdConfig
	mu       sync.Mutex
	gen      uint64
	restarts int
	running  bool
}

func newFakeInit(t *testing.T, started bool) *fakeInit {
	dir := t.TempDir()
	f := &fakeInit{t: t}
	f.d = &dockerdConfig{
		path:  filepath.Join(dir, "daemon.json"),
		ready: filepath.Join(dir, "daemon.ready"),
		gen:   filepath.Join(dir, "dockerd.gen"),
		hold:  filepath.Join(dir, "dockerd.hold"),
		validate: func(_ context.Context, path string) (string, error) {
			b, _ := os.ReadFile(path)
			if bytes.Contains(b, []byte("invalid")) {
				return `the following directives don't match any configuration option: invalid in ` + path, errors.New("exit status 1")
			}
			return "", nil
		},
		restart:   f.restart,
		alive:     func(int) bool { f.mu.Lock(); defer f.mu.Unlock(); return f.running },
		upTimeout: 2 * time.Second,
		pollEvery: time.Millisecond,
		ping: func(context.Context) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			if !f.running {
				return errors.New("connection refused")
			}
			return nil
		},
	}
	if started {
		f.start()
	}
	return f
}

func (f *fakeInit) start() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gen++
	b, _ := os.ReadFile(f.d.path)
	f.running = !bytes.Contains(b, []byte("bad"))
	if err := os.WriteFile(f.d.gen, fmt.Appendf(nil, "%d %d\n", f.gen, 100+f.gen), 0o600); err != nil {
		f.t.Error(err)
	}
}

func (f *fakeInit) restart() error {
	if _, err := os.Stat(f.d.hold); err != nil {
		f.t.Error("restart without the hold file")
	}
	f.mu.Lock()
	f.restarts++
	f.mu.Unlock()
	f.start()
	return nil
}

func installed(t *testing.T, d *dockerdConfig) string {
	b, err := os.ReadFile(d.path)
	if errors.Is(err, os.ErrNotExist) {
		return "<none>"
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// bootInit starts dockerd once the ready file appears, as the init does at boot.
func (f *fakeInit) bootInit(t *testing.T) chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 2000; i++ {
			if _, err := os.Stat(f.d.ready); err == nil {
				f.start()
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Error("the ready file never appeared")
	}()
	return done
}

func TestDaemonConfigInstall(t *testing.T) {
	f := newFakeInit(t, false)
	d, ctx := f.d, context.Background()
	// Boot: dockerd has not started; the file is installed, and the reply waits until
	// dockerd, started by the init, answers.
	booted := f.bootInit(t)
	r := d.apply(ctx, []byte(`{"debug":true}`), false)
	<-booted
	if r.Code != guestctl.DaemonOK || r.Flags != guestctl.DaemonChanged || installed(t, d) != `{"debug":true}` || f.restarts != 0 {
		t.Fatalf("boot install: %#v, file %q", r, installed(t, d))
	}
	if _, err := os.Stat(d.ready); err != nil {
		t.Fatal("no ready file after the first install")
	}
	// The same file again without restart: nothing to do.
	if r := d.apply(ctx, []byte(`{"debug":true}`), false); r.Code != guestctl.DaemonOK || r.Flags != 0 {
		t.Fatalf("same file: %#v", r)
	}
	// dockerd refuses the file: the installed one stays, and the path is not leaked.
	r = d.apply(ctx, []byte(`{"invalid":1}`), true)
	if r.Code != guestctl.DaemonRejected || installed(t, d) != `{"debug":true}` || f.restarts != 0 {
		t.Fatalf("rejected: %#v, file %q, %d restarts", r, installed(t, d), f.restarts)
	}
	if strings.Contains(r.Message, d.path) || !strings.Contains(r.Message, "invalid") {
		t.Errorf("message %q", r.Message)
	}
	if _, err := os.Stat(d.path + ".new"); !errors.Is(err, os.ErrNotExist) {
		t.Error("candidate file left behind")
	}
	// A new file with a restart.
	r = d.apply(ctx, []byte(`{"insecure-registries":["10.0.0.1:5000"]}`), true)
	if r.Code != guestctl.DaemonOK || r.Flags != guestctl.DaemonChanged|guestctl.DaemonRestarted || f.restarts != 1 {
		t.Fatalf("restart: %#v, %d restarts", r, f.restarts)
	}
	if _, err := os.Stat(d.hold); !errors.Is(err, os.ErrNotExist) {
		t.Error("hold file left behind")
	}
	// Removal.
	r = d.apply(ctx, nil, true)
	if r.Code != guestctl.DaemonOK || installed(t, d) != "<none>" || f.restarts != 2 {
		t.Fatalf("removal: %#v, %q", r, installed(t, d))
	}
	// Removing nothing changes nothing.
	if r := d.apply(ctx, nil, false); r.Flags != 0 {
		t.Fatalf("second removal: %#v", r)
	}
}

func TestDaemonConfigBootRollback(t *testing.T) {
	// At boot, a file dockerd does not start with is removed and dockerd restarted
	// without it, instead of the VM powering off.
	f := newFakeInit(t, false)
	booted := f.bootInit(t)
	r := f.d.apply(context.Background(), []byte(`{"bad":1}`), false)
	<-booted
	if r.Code != guestctl.DaemonRolledBack || installed(t, f.d) != "<none>" || f.restarts != 1 || !f.running {
		t.Fatalf("boot rollback: %#v, file %q, %d restarts", r, installed(t, f.d), f.restarts)
	}
	// No file at boot: nothing to watch, the reply is immediate.
	f2 := newFakeInit(t, false)
	if r := f2.d.apply(context.Background(), nil, false); r.Code != guestctl.DaemonOK || f2.restarts != 0 {
		t.Fatalf("boot without a file: %#v", r)
	}
}

func TestDaemonConfigRollback(t *testing.T) {
	f := newFakeInit(t, false)
	d, ctx := f.d, context.Background()
	booted := f.bootInit(t)
	if r := d.apply(ctx, []byte(`{"debug":true}`), false); r.Code != guestctl.DaemonOK {
		t.Fatal(r)
	}
	<-booted
	// Valid to dockerd --validate, but dockerd does not come up with it.
	r := d.apply(ctx, []byte(`{"bad":true}`), true)
	if r.Code != guestctl.DaemonRolledBack || installed(t, d) != `{"debug":true}` || f.restarts != 2 {
		t.Fatalf("rollback: %#v, file %q, %d restarts", r, installed(t, d), f.restarts)
	}
	if !f.running {
		t.Fatal("dockerd is not running after the rollback")
	}
	// From no file at all: the rollback removes the new one.
	f2 := newFakeInit(t, true)
	if r := f2.d.apply(ctx, []byte(`{"bad":1}`), true); r.Code != guestctl.DaemonRolledBack || installed(t, f2.d) != "<none>" {
		t.Fatalf("rollback to none: %#v, %q", r, installed(t, f2.d))
	}
	// A restart with an unchanged file that does not come back is a failure.
	f3 := newFakeInit(t, true)
	f3.d.restart = func() error { return nil } // the init never restarts dockerd
	f3.d.upTimeout = 20 * time.Millisecond
	_ = os.WriteFile(f3.d.hold, nil, 0o600)
	if r := f3.d.apply(ctx, nil, true); r.Code != guestctl.DaemonFailed {
		t.Fatalf("stuck restart: %#v", r)
	}
}

// Review finding 2: a changed file on a running dockerd is applied with a restart even
// when the host did not ask for one.
func TestDaemonConfigChangedWhileRunning(t *testing.T) {
	f := newFakeInit(t, true)
	r := f.d.apply(context.Background(), []byte(`{"debug":true}`), false)
	if r.Code != guestctl.DaemonOK || r.Flags != guestctl.DaemonChanged|guestctl.DaemonRestarted || f.restarts != 1 {
		t.Fatalf("%#v, %d restarts", r, f.restarts)
	}
	// Unchanged: nothing restarts.
	if r := f.d.apply(context.Background(), []byte(`{"debug":true}`), false); r.Flags != 0 || f.restarts != 1 {
		t.Fatalf("unchanged: %#v, %d restarts", r, f.restarts)
	}
}

// Review finding 1: the connection ending during a restart does not roll back a dockerd
// that comes up.
func TestDaemonConfigConnectionLostDuringRestart(t *testing.T) {
	f := newFakeInit(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	slow := f.d.restart
	f.d.restart = func() error {
		f.mu.Lock()
		f.restarts++
		f.running = false
		f.mu.Unlock()
		cancel() // the host reconnects while dockerd restarts
		go func() {
			time.Sleep(20 * time.Millisecond)
			f.start()
		}()
		return nil
	}
	_ = slow
	r := f.d.apply(ctx, []byte(`{"debug":true}`), true)
	if r.Code != guestctl.DaemonOK || f.restarts != 1 || installed(t, f.d) != `{"debug":true}` {
		t.Fatalf("%#v, %d restarts, file %q", r, f.restarts, installed(t, f.d))
	}
}

func TestReadGen(t *testing.T) {
	p := filepath.Join(t.TempDir(), "gen")
	for _, c := range []struct {
		in  string
		gen uint64
		pid int
		ok  bool
	}{
		{"1 42\n", 1, 42, true},
		{"7 9", 7, 9, true},
		{"", 0, 0, false},
		{"1", 0, 0, false},
		{"1 0", 0, 0, false},
		{"-1 3", 0, 0, false},
		{"1 2 3", 0, 0, false},
		{"x 2", 0, 0, false},
	} {
		_ = os.WriteFile(p, []byte(c.in), 0o600)
		g, pid, err := readGen(p)
		if (err == nil) != c.ok || g != c.gen || pid != c.pid {
			t.Errorf("readGen(%q) = %d %d %v", c.in, g, pid, err)
		}
	}
}

func TestLastLineAndClip(t *testing.T) {
	out := []byte("time=x level=info\nunable to configure the Docker daemon with file /run/hyperbutler/daemon.json.new: the following directives don't match any configuration option: foo\n\n")
	if got := lastLine(out); got != "the following directives don't match any configuration option: foo" {
		t.Errorf("lastLine = %q", got)
	}
	long := strings.Repeat("é", guestctl.MaxDaemonMessage)
	if c := clip(long); len(c) > guestctl.MaxDaemonMessage || !strings.HasPrefix(long, c) {
		t.Errorf("clip: %d bytes", len(c))
	}
	if c := clip("a\xffb"); c != "a?b" {
		t.Errorf("clip = %q", c)
	}
}

func TestPingDocker(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skip("no AF_UNIX:", err)
	}
	defer ln.Close()
	answers := []string{"HTTP/1.1 200 OK\r\n\r\nOK", "HTTP/1.1 500 Internal Server Error\r\n\r\n"}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for _, a := range answers {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			buf := make([]byte, 128)
			_, _ = c.Read(buf)
			_, _ = io.WriteString(c, a)
			c.Close()
		}
	}()
	if err := pingDocker(context.Background(), sock); err != nil {
		t.Errorf("ping 200: %v", err)
	}
	if err := pingDocker(context.Background(), sock); err == nil {
		t.Error("ping 500 succeeded")
	}
	wg.Wait()
}
