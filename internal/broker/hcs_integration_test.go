//go:build integration && windows

package broker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"

	"github.com/Higangssh/hyperbutler/internal/guid"
	"github.com/Higangssh/hyperbutler/internal/transport"
	"github.com/Higangssh/hyperbutler/internal/vm"
	"github.com/Higangssh/hyperbutler/internal/vmtemplate"
)

// TestHCSBackendBoots boots a real VM through the HCS backend: the image from
// HYPERBUTLER_TEST_IMAGE_DIR (an image set with image.json), a fresh sparse data disk in
// a private directory, taken by handle. It needs HCS access (elevation or Hyper-V
// Administrators) and HYPERBUTLER_TEST_BROKER_HCS=1, and runs only with -tags
// integration. It creates and terminates one VM named hyperbutler-broker-*; it does not
// run the sweep, which would touch other broker VMs on the machine.
func TestHCSBackendBoots(t *testing.T) {
	imageDir := os.Getenv("HYPERBUTLER_TEST_IMAGE_DIR")
	if os.Getenv("HYPERBUTLER_TEST_BROKER_HCS") != "1" || imageDir == "" {
		t.Skip("set HYPERBUTLER_TEST_BROKER_HCS=1 and HYPERBUTLER_TEST_IMAGE_DIR to boot a VM")
	}
	sid := currentSID(t)
	dir := goodDir(t)
	disk := filepath.Join(dir, DataDiskName)
	// The product's size: hyperbutlerd's free-space reserve leaves an 8 GiB disk full.
	if err := vm.CreateSparseVHDX(disk, 64<<30); err != nil {
		t.Fatal(err)
	}
	// No COM port by default, as the broker runs (ADR 0004 decision 7): the guest must
	// keep running without a console. HYPERBUTLER_TEST_BROKER_CONSOLE=1 adds one to read
	// the guest's boot when this fails.
	var console string
	if os.Getenv("HYPERBUTLER_TEST_BROKER_CONSOLE") == "1" {
		console = fmt.Sprintf(`\\.\pipe\hyperbutler-broker-test-%d-com1`, os.Getpid())
	}
	be, err := NewHCSBackend(HCSConfig{StateDir: t.TempDir(), AllowUnpinned: true, Logf: t.Logf, imageDir: imageDir, consolePipe: console})
	if err != nil {
		t.Fatal(err)
	}
	defer be.Close()
	ctx := context.Background()
	// No network or share: nothing here serves their host ends, and a guest that cannot
	// reach them powers off. The engine-level run covers those.
	st, err := be.Start(ctx, VMSpec{Owner: sid, CPUs: 2, MemoryMB: 2048, DataDir: &DataDir{h: dataDirHandle(t, dir)},
		BlankDisk: true})
	if err != nil {
		t.Fatal(err)
	}
	if !st.Running || st.ID == ([16]byte{}) {
		t.Fatalf("status %+v", st)
	}
	// The guest's console, logged when the test fails.
	var con bytes.Buffer
	if console == "" {
		// nothing to read
	} else if c, err := winio.DialPipe(console, nil); err == nil {
		go func() { _, _ = io.Copy(&con, c) }()
		defer c.Close()
		defer func() {
			if t.Failed() {
				t.Logf("guest console:\n%s", con.Bytes())
			}
		}()
	} else {
		t.Logf("no console: %v", err)
	}
	if got, _ := vm.AccountsOnFile(disk); len(got) != 1 {
		t.Fatalf("VM accounts on the data disk: %v", got)
	}
	// Booted means the guest's agent accepts on the Docker API port: its services started
	// without a console. dockerd itself does not come up here: with no host on the control
	// channel, hyperbutlerd's free-space guard gets no budget and reserves the whole disk.
	// The engine-level run covers dockerd.
	d := &transport.HvsockDialer{Target: transport.HvsockTarget{VMID: guid.FromArray(st.ID),
		ServiceID: transport.VsockServiceID(vmtemplate.DockerPort)}}
	start := time.Now()
	accepted := false
	for !accepted && time.Since(start) < 60*time.Second {
		dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		if c, err := d.DialContext(dctx); err == nil {
			accepted = true
			c.Close()
		} else {
			time.Sleep(200 * time.Millisecond)
		}
		cancel()
	}
	if !accepted {
		st, err := be.Status(ctx, sid)
		t.Fatalf("the guest agent did not accept within 60 s (status %+v, %v)", st, err)
	}
	t.Logf("the guest agent accepted after %v", time.Since(start))
	if st, err := be.Status(ctx, sid); err != nil || !st.Running {
		t.Fatalf("the VM did not keep running: %+v, %v", st, err)
	}
	if err := be.Pause(ctx, sid, false); err != nil {
		t.Fatal(err)
	}
	if st, _ := be.Status(ctx, sid); !st.Paused {
		t.Fatalf("after pause: %+v", st)
	}
	if err := be.Pause(ctx, sid, true); err != nil {
		t.Fatal(err)
	}
	if err := be.Stop(ctx, sid, true); err != nil {
		t.Fatal(err)
	}
	if got, _ := vm.AccountsOnFile(disk); len(got) != 0 {
		t.Fatalf("VM accounts left on the data disk: %v", got)
	}
}
