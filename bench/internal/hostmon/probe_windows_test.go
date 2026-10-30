//go:build windows

package hostmon

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Higangssh/hyperbutler/internal/transport"
)

func TestPipeExists(t *testing.T) {
	name := fmt.Sprintf(`\\.\pipe\hostmon-test-%d`, os.Getpid())
	if ok, err := PipeExists(name); err != nil || ok {
		t.Fatalf("before listen: %v %v", ok, err)
	}
	ln, err := transport.ListenPipe(name)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := PipeExists(name); err != nil || !ok {
		t.Fatalf("while listening: %v %v", ok, err)
	}
	ln.Close()
	if ok, err := PipeExists(name); err != nil || ok {
		t.Fatalf("after close: %v %v", ok, err)
	}
}

// TestWatcher: it stops (goleak checks its goroutine), Take forgets, and PIDs marked as
// ours are never reported.
func TestWatcher(t *testing.T) {
	w := Watch(10 * time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	w.Stop()
	w.mu.Lock()
	w.seen[4242] = "vmmem"
	w.seen[4343] = "vmwp.exe"
	w.mu.Unlock()
	w.SetOurs(4343)
	if got := w.Take(); got != "vmmem 4242" && !strings.Contains(got, "vmmem 4242") || strings.Contains(got, "4343") {
		t.Fatalf("Take = %q", got)
	}
	if got := w.Take(); got != "" {
		t.Fatalf("second Take = %q", got)
	}
}

func TestSnapshotHasSelf(t *testing.T) {
	ps, err := Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		if p.PID == uint32(os.Getpid()) {
			if p.PPID != uint32(os.Getppid()) {
				t.Fatalf("parent %d, want %d", p.PPID, os.Getppid())
			}
			return
		}
	}
	t.Fatal("own process not in the snapshot")
}
