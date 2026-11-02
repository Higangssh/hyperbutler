//go:build integration && windows

package broker

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

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
	if err := vm.CreateSparseVHDX(disk, 8<<30); err != nil {
		t.Fatal(err)
	}
	be, err := NewHCSBackend(HCSConfig{StateDir: t.TempDir(), AllowUnpinned: true, Logf: t.Logf, imageDir: imageDir})
	if err != nil {
		t.Fatal(err)
	}
	defer be.Close()
	ctx := context.Background()
	st, err := be.Start(ctx, VMSpec{Owner: sid, CPUs: 2, MemoryMB: 2048, DataDir: &DataDir{h: dataDirHandle(t, dir)},
		BlankDisk: true, Share: true,
		Net: &vmtemplate.Net{Subnet: netip.MustParsePrefix("192.168.127.0/24"), MTU: 9000, Offload: true, HostGateway: true}})
	if err != nil {
		t.Fatal(err)
	}
	if !st.Running || st.ID == ([16]byte{}) {
		t.Fatalf("status %+v", st)
	}
	if got, _ := vm.AccountsOnFile(disk); len(got) != 1 {
		t.Fatalf("VM accounts on the data disk: %v", got)
	}
	time.Sleep(2 * time.Second)
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
