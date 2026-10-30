//go:build windows

package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Microsoft/go-winio/pkg/guid"

	"github.com/Higangssh/winbox/internal/transport"
)

func TestStreamConsole(t *testing.T) {
	long := strings.Repeat("x", 70<<10)
	in := "kernel line\n" + long + "winbox-t 1\n" + "winbox-agent: ok\x1b[2J\n" + "winbox-t 0.6 init-start\r\n" + "winbox-t 15 mem MemFree: 1\n"
	var w bytes.Buffer
	var steps []string
	streamConsole(strings.NewReader(in), &w, func(f string, a ...any) { steps = append(steps, fmt.Sprintf(f, a...)) })
	if w.String() != in {
		t.Errorf("console log lost bytes: %d of %d", w.Len(), len(in))
	}
	want := []string{
		"first console output",
		// The tail of the long line ("...winbox-t 1") is not a line of its own.
		`guest: winbox-agent: ok\x1b[2J`,
		"guest: winbox-t 0.6 init-start",
	}
	if strings.Join(steps, "|") != strings.Join(want, "|") {
		t.Errorf("steps %q, want %q", steps, want)
	}
}

func TestWaitPingCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// A random VM ID: nothing answers.
	vmid, err := guid.NewV4()
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err = waitPing(ctx, transport.HvsockTarget{VMID: vmid, ServiceID: transport.VsockServiceID(2375)}, time.Minute)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v, want context.Canceled", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("took %v", time.Since(start))
	}
}
