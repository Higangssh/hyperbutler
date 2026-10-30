//go:build windows

package hostmon

import (
	"fmt"
	"os"
	"testing"

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
