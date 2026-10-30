//go:build windows

package broker

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Higangssh/hyperbutler/internal/broker/proto"
	"golang.org/x/sys/windows"
)

func currentSID(t *testing.T) string {
	t.Helper()
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	return tu.User.Sid.String()
}

// setDACL replaces path's DACL with the one in sddl (a "D:..." string), protected from
// inheritance.
func setDACL(t *testing.T, path, sddl string) {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
}

// goodDir returns a new directory owned by the current user whose DACL grants only the
// user, SYSTEM and Administrators.
func goodDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	setDACL(t, dir, "D:P(A;OICI;FA;;;"+currentSID(t)+")(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICIIO;FA;;;CO)")
	return dir
}

// openDir opens path as a directory in this process, optionally without following a
// reparse point, and returns the handle.
func openDir(t *testing.T, path string, reparse bool, flags uint32) windows.Handle {
	t.Helper()
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	attrs := uint32(windows.FILE_FLAG_BACKUP_SEMANTICS) | flags
	if reparse {
		attrs |= windows.FILE_FLAG_OPEN_REPARSE_POINT
	}
	h, err := windows.CreateFile(p, fileListDirectory|fileReadAttributes|windows.READ_CONTROL|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, attrs, 0)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { windows.CloseHandle(h) })
	return h
}

// take runs the full set-data-location path for handle h of this process.
func take(t *testing.T, h windows.Handle) (*DataDir, error) {
	t.Helper()
	sid := currentSID(t)
	d, err := openDataDir(&localPeer{sid: sid}, uint64(h), sid, ValidateDataDir)
	if d != nil {
		t.Cleanup(func() { d.Close() })
	}
	return d, err
}

func wantReject(t *testing.T, err error, code proto.Code, reason ...string) {
	t.Helper()
	var re *rejectError
	if !errors.As(err, &re) || re.code != code {
		t.Fatalf("err = %v, want rejection with code %d", err, code)
	}
	if len(reason) > 0 && re.reason != reason[0] {
		t.Fatalf("rejected for %q (%v), want %q", re.reason, err, reason[0])
	}
}

func mklinkJ(t *testing.T, link, target string) {
	t.Helper()
	out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
	if err != nil {
		t.Fatalf("mklink /J: %v: %s", err, out)
	}
}

func TestDataDirAccepted(t *testing.T) {
	d, err := take(t, openDir(t, goodDir(t), false, 0))
	if err != nil {
		t.Fatal(err)
	}
	// The broker holds its own handle, not the caller's.
	if d.Handle() == 0 {
		t.Fatal("no handle")
	}
	if err := d.revalidate(currentSID(t), ValidateDataDir); err != nil {
		t.Fatalf("revalidate: %v", err)
	}
}

func TestDataDirWideACL(t *testing.T) {
	me := currentSID(t)
	const wide = "DACL grants access to another account"
	for _, tt := range []struct{ name, dacl, reason string }{
		{"everyone read", "D:P(A;;FA;;;" + me + ")(A;;FR;;;WD)", wide},
		{"users modify", "D:P(A;;FA;;;" + me + ")(A;OICI;0x1301bf;;;BU)", wide},
		{"authenticated users inherit-only", "D:P(A;;FA;;;" + me + ")(A;OICIIO;FA;;;AU)", wide},
		{"another user", "D:P(A;;FA;;;" + me + ")(A;;FA;;;" + sidB + ")", wide},
		{"NULL DACL", "D:NO_ACCESS_CONTROL", "has a NULL DACL"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "data")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			setDACL(t, dir, tt.dacl)
			_, err := take(t, openDir(t, dir, false, 0))
			wantReject(t, err, proto.CodeDataLocationRejected, tt.reason)
		})
	}
}

func TestDataDirDenyACEAllowed(t *testing.T) {
	dir := goodDir(t)
	setDACL(t, dir, "D:P(D;;FA;;;"+sidB+")(A;;FA;;;"+currentSID(t)+")")
	if _, err := take(t, openDir(t, dir, false, 0)); err != nil {
		t.Fatalf("deny ACE for Everyone was rejected: %v", err)
	}
}

func TestDataDirOwnerMustBeCaller(t *testing.T) {
	h := openDir(t, goodDir(t), false, 0)
	_, err := openDataDir(&localPeer{sid: sidB}, uint64(h), sidB, ValidateDataDir)
	wantReject(t, err, proto.CodeDataLocationRejected, "owner is not the caller")
}

// A junction opened as itself is a reparse point and is rejected.
func TestDataDirJunction(t *testing.T) {
	link := filepath.Join(t.TempDir(), "link")
	mklinkJ(t, link, goodDir(t))
	_, err := take(t, openDir(t, link, true, 0))
	wantReject(t, err, proto.CodeDataLocationRejected, "is a reparse point")
}

// A junction the caller followed when opening yields a handle to the target, which is
// judged on its own owner and ACL: here a system directory, which is rejected (the
// caller cannot even open it with the access the broker asks for).
func TestDataDirJunctionToSystemDir(t *testing.T) {
	link := filepath.Join(t.TempDir(), "link")
	mklinkJ(t, link, os.Getenv("SystemRoot"))
	_, err := take(t, openDir(t, link, false, 0))
	wantReject(t, err, proto.CodeDataLocationRejected)
}

func TestDataDirSymlink(t *testing.T) {
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(goodDir(t), link); err != nil {
		t.Skipf("cannot create a directory symlink as this user (needs Developer Mode): %v", err)
	}
	_, err := take(t, openDir(t, link, true, 0))
	wantReject(t, err, proto.CodeDataLocationRejected, "is a reparse point")
}

func TestDataDirSystemDirectory(t *testing.T) {
	_, err := take(t, openDir(t, os.Getenv("SystemRoot"), false, 0))
	wantReject(t, err, proto.CodeDataLocationRejected)
}

// Directories cannot be hard-linked, so a hard link can only be to a file, which is not
// a directory.
func TestDataDirFileAndHardLink(t *testing.T) {
	dir := goodDir(t)
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	hard := filepath.Join(dir, "hard")
	if err := os.Link(file, hard); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{file, hard} {
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		_, err = take(t, windows.Handle(f.Fd()))
		f.Close()
		wantReject(t, err, proto.CodeDataLocationRejected)
	}
}

func TestDataDirNonFileHandle(t *testing.T) {
	ev, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(ev)
	_, err = take(t, ev)
	wantReject(t, err, proto.CodeDataLocationRejected)
}

func TestDataDirBadHandleValue(t *testing.T) {
	_, err := take(t, windows.Handle(0xFFFFF0))
	wantReject(t, err, proto.CodeInvalidHandle)
}

// A handle to a pipe the caller controls must be rejected without the broker ever
// opening (connecting to) the pipe: that is how a privileged process gets impersonated.
func TestDataDirPipeHandleNotConnected(t *testing.T) {
	name := `\\.\pipe\hyperbutler_test_datadir_` + time.Now().Format("150405.000000000")
	n16, _ := windows.UTF16PtrFromString(name)
	mk := func(first bool) windows.Handle {
		flags := uint32(windows.PIPE_ACCESS_DUPLEX | windows.FILE_FLAG_OVERLAPPED)
		if first {
			flags |= windows.FILE_FLAG_FIRST_PIPE_INSTANCE
		}
		h, err := windows.CreateNamedPipe(n16, flags, windows.PIPE_TYPE_BYTE, 2, 512, 512, 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { windows.CloseHandle(h) })
		return h
	}
	mk(true)
	client, err := windows.CreateFile(n16, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(client)

	second := mk(false)
	ev, _ := windows.CreateEvent(nil, 1, 0, nil)
	defer windows.CloseHandle(ev)
	ov := &windows.Overlapped{HEvent: ev}
	if err := windows.ConnectNamedPipe(second, ov); !errors.Is(err, windows.ERROR_IO_PENDING) {
		t.Fatalf("ConnectNamedPipe: %v", err)
	}

	_, err = take(t, client)
	wantReject(t, err, proto.CodeDataLocationRejected, "cannot open the handle as a directory")

	if s, _ := windows.WaitForSingleObject(ev, 200); s == windows.WAIT_OBJECT_0 {
		t.Fatal("validating a pipe handle connected to the caller's pipe")
	}
	windows.CancelIoEx(second, ov)
	var n uint32
	windows.GetOverlappedResult(second, ov, &n, true)
}

// A caller can hold a synchronous handle with a blocking operation pending on it. The
// broker must not stall behind that operation's file object lock.
func TestDataDirSynchronousHandleWithPendingIO(t *testing.T) {
	dir := goodDir(t)
	h := openDir(t, dir, false, 0) // synchronous: no FILE_FLAG_OVERLAPPED
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		var n uint32
		close(started)
		_ = windows.ReadDirectoryChanges(h, &buf[0], uint32(len(buf)), false,
			windows.FILE_NOTIFY_CHANGE_FILE_NAME, &n, nil, 0)
	}()
	<-started
	time.Sleep(100 * time.Millisecond) // let the read block

	res := make(chan error, 1)
	go func() {
		_, err := take(t, h)
		res <- err
	}()
	select {
	case err := <-res:
		if err != nil {
			t.Fatalf("validation failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("validation blocked behind the caller's pending synchronous I/O")
	}
	// Unblock the watcher.
	_ = os.WriteFile(filepath.Join(dir, "wake"), nil, 0o600)
	<-done
}
