//go:build windows

package engine

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLockExclusive(t *testing.T) {
	p := filepath.Join(testDir(t), "engine.lock")
	if held, err := Held(p); held || err != nil {
		t.Fatalf("Held before anything: %v, %v", held, err)
	}
	l, err := TryLock(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := TryLock(p); !errors.Is(err, ErrLocked) {
		t.Fatalf("second TryLock: %v, want ErrLocked", err)
	}
	if held, err := Held(p); !held || err != nil {
		t.Fatalf("Held while locked: %v, %v", held, err)
	}
	if err := l.Unlock(); err != nil {
		t.Fatal(err)
	}
	if err := l.Unlock(); err != nil {
		t.Fatalf("second Unlock: %v", err)
	}
	if held, err := Held(p); held || err != nil {
		t.Fatalf("Held after unlock: %v, %v", held, err)
	}
}

// A Held probe holds the lock for an instant; Lock must ride over it.
func TestLockRetriesOverProbe(t *testing.T) {
	p := filepath.Join(testDir(t), "engine.lock")
	probe, err := TryLock(p)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = probe.Unlock()
	}()
	l, err := Lock(p, 2*time.Second)
	if err != nil {
		t.Fatalf("Lock did not wait out the probe: %v", err)
	}
	_ = l.Unlock()
}

func TestLockRefusesLink(t *testing.T) {
	dir := testDir(t)
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "engine.lock")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Skipf("mklink /J: %v %s", err, out)
	}
	if _, err := TryLock(link); err == nil {
		t.Fatal("locked through a junction")
	}
}

// The engine crashing must not leave a stale lock: the OS drops it with the process.
func TestLockReleasedWhenHolderDies(t *testing.T) {
	p := filepath.Join(testDir(t), "engine.lock")
	cmd := exec.Command(os.Args[0], "-test.run=^TestLockHelperProcess$")
	cmd.Env = append(os.Environ(), "HYPERBUTLER_LOCK_HELPER="+p)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || !strings.Contains(line, "locked") {
		t.Fatalf("helper: %q %v", line, err)
	}
	if held, _ := Held(p); !held {
		t.Fatal("the helper's lock is not seen")
	}
	if _, err := TryLock(p); !errors.Is(err, ErrLocked) {
		t.Fatalf("TryLock against the helper: %v", err)
	}
	if err := cmd.Process.Kill(); err != nil { // TerminateProcess: no cleanup runs
		t.Fatal(err)
	}
	_ = cmd.Wait()
	l, err := Lock(p, 2*time.Second)
	if err != nil {
		t.Fatalf("lock still held after the holder was killed: %v", err)
	}
	_ = l.Unlock()
}

func TestLockHelperProcess(t *testing.T) {
	p := os.Getenv("HYPERBUTLER_LOCK_HELPER")
	if p == "" {
		t.Skip("helper for TestLockReleasedWhenHolderDies")
	}
	if _, err := TryLock(p); err != nil {
		t.Fatal(err)
	}
	os.Stdout.WriteString("locked\n")
	time.Sleep(time.Minute)
}

func TestOpenProcessChecksCreationTime(t *testing.T) {
	p, err := OpenProcess(os.Getpid(), SelfCreated())
	if err != nil {
		t.Fatal(err)
	}
	if exited, err := p.Wait(0); exited || err != nil {
		t.Fatalf("Wait(0) on self: %v, %v", exited, err)
	}
	p.Close()
	// The same PID with another creation time is a different (reused) process.
	if _, err := OpenProcess(os.Getpid(), SelfCreated()+1); !errors.Is(err, ErrProcessGone) {
		t.Fatalf("wrong creation time: %v, want ErrProcessGone", err)
	}
	for _, pid := range []int{0, -1, maxPID} {
		if _, err := OpenProcess(pid, 0); !errors.Is(err, ErrProcessGone) {
			t.Errorf("pid %d: %v", pid, err)
		}
	}
}

func TestOpenProcessExited(t *testing.T) {
	cmd := exec.Command("cmd", "/c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	p, err := OpenProcess(pid, 0)
	if err == nil {
		p.Close()
	}
	_ = cmd.Wait()
	if _, err := OpenProcess(pid, 1); !errors.Is(err, ErrProcessGone) {
		t.Fatalf("exited process: %v", err)
	}
}

func TestStopEvent(t *testing.T) {
	ev, err := CreateStopEvent()
	if err != nil {
		t.Fatal(err)
	}
	defer ev.Close()
	// The name is taken now; a second engine (or a squatter's event) is refused.
	if ev2, err := CreateStopEvent(); err == nil {
		ev2.Close()
		t.Fatal("created the stop event twice")
	}
	done := make(chan struct{})
	got := make(chan bool, 1)
	go func() { got <- ev.Wait(done) }()
	if err := RequestStop(os.Getpid(), SelfCreated()); err != nil {
		t.Fatal(err)
	}
	select {
	case ok := <-got:
		if !ok {
			t.Fatal("Wait returned without the stop")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stop not seen")
	}
	if err := RequestStop(os.Getpid(), SelfCreated()+1); err == nil {
		t.Fatal("stopped an engine with another creation time")
	}
}
