//go:build integration && windows

package main

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/broker"
	"github.com/Higangssh/hyperbutler/internal/broker/proto"
	"github.com/Higangssh/hyperbutler/internal/engine"
	"github.com/Higangssh/hyperbutler/internal/vm"
)

// TestServiceHCS installs the broker service with the real HCS backend and starts and
// stops one VM through its pipe, as a user would: the HCS backend under the service's
// token (LocalSystem with SeChangeNotifyPrivilege and SeImpersonatePrivilege only). It
// boots the image set in HYPERBUTLER_TEST_IMAGE_DIR (with image.json), copied into the
// image folder next to the broker, which a development build boots with
// -allow-unpinned-image; the VM has no network and no console, and a new 64 GiB sparse,
// blank data disk in a private directory passed by handle. It needs an elevated prompt,
// HYPERBUTLER_TEST_BROKER_SERVICE_HCS=1, HYPERBUTLER_TEST_IMAGE_DIR and -tags
// integration, refuses to run if the service is installed, and uninstalls it at the end.
// Its service sweeps every broker-tagged VM on the machine at start, as the service
// does: run it when no other broker runs VMs.
func TestServiceHCS(t *testing.T) {
	imageDir := os.Getenv("HYPERBUTLER_TEST_IMAGE_DIR")
	if os.Getenv("HYPERBUTLER_TEST_BROKER_SERVICE_HCS") != "1" || imageDir == "" {
		t.Skip("set HYPERBUTLER_TEST_BROKER_SERVICE_HCS=1 and HYPERBUTLER_TEST_IMAGE_DIR (in an elevated prompt) to boot a VM through the service")
	}
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Fatal("needs an elevated prompt")
	}
	if st, err := broker.QueryService(broker.ServiceName); err != nil || st.Installed {
		t.Fatalf("%s is already installed (%+v, %v); this test installs and deletes it, so uninstall it first", broker.ServiceName, st, err)
	}

	dir := adminOnlyDir(t)
	exe := filepath.Join(dir, "hyperbutler-broker.exe")
	if out, err := exec.Command("go", "build", "-o", exe, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	copyTree(t, imageDir, filepath.Join(dir, broker.ImageDirName))

	t.Cleanup(func() {
		if out, err := exec.Command(exe, "uninstall").CombinedOutput(); err != nil {
			t.Errorf("uninstall: %v\n%s", err, out)
		}
	})
	if out, err := exec.Command(exe, "install", "-allow-unpinned-image").CombinedOutput(); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	waitRunning(t, 0)

	// The data location: a directory the user owns with a protected DACL (user, SYSTEM,
	// Administrators), as the engine makes it, and a blank data disk the user owns.
	data := filepath.Join(t.TempDir(), "data")
	if err := engine.MkdirPrivate(data); err != nil {
		t.Fatal(err)
	}
	disk := filepath.Join(data, broker.DataDiskName)
	if err := vm.CreateSparseVHDX(disk, 64<<30); err != nil {
		t.Fatal(err)
	}
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(disk, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION, tu.User.Sid, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	h := openDataDir(t, data)
	defer windows.CloseHandle(h)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := broker.Dial(ctx, broker.ServicePipe)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.SetDataLocation(h); err != nil {
		t.Fatalf("set the data location: %v", err)
	}
	r, err := c.Start(proto.Request{CPUs: 2, MemoryMB: 2048, Features: proto.FeatureBlankDisk})
	if err != nil {
		t.Fatalf("start (the Application event log, source %s, has the detail): %v", broker.EventSource, err)
	}
	if r.State != proto.StateRunning || r.VMID == ([16]byte{}) {
		t.Fatalf("start reply %+v", r)
	}
	time.Sleep(2 * time.Second)
	if r, err := c.Status(); err != nil || r.State != proto.StateRunning {
		t.Fatalf("the VM did not keep running: %+v, %v", r, err)
	}
	if err := c.Stop(true); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if r, err := c.Status(); err != nil || r.State != proto.StateStopped {
		t.Fatalf("after stop: %+v, %v", r, err)
	}
	if got, _ := vm.AccountsOnFile(disk); len(got) != 0 {
		t.Errorf("VM accounts left on the data disk: %v", got)
	}
}

// openDataDir opens dir as the engine does to pass it to the broker.
func openDataDir(t *testing.T, dir string) windows.Handle {
	t.Helper()
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		t.Fatal(err)
	}
	const fileListDirectory, fileTraverse, fileReadAttributes = 0x0001, 0x0020, 0x0080
	h, err := windows.CreateFile(p, fileListDirectory|fileTraverse|fileReadAttributes|windows.READ_CONTROL|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// copyTree copies the files below from into to.
func copyTree(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, p)
		if err != nil {
			return err
		}
		dst := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		copyFile(t, p, dst)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
