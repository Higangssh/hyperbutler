// Package sandboxmsi gets the agent sandbox image's installer onto a machine (ADR 0009,
// "Shipping the image"): `hyperbutler sandbox install` downloads the MSI of this release,
// refuses it unless its SHA-256 equals the pin compiled into hyperbutler.exe, and runs it
// elevated with one UAC prompt. The broker's privileges do not change: it goes on booting
// only what is in the admin-only folder next to it, against its own pin.
//
// A download into a folder the user can write, handed to an elevated installer, is the
// classic race. What this package does about it:
//
//   - The file is opened with FILE_SHARE_READ only (Lock) and hashed through that handle,
//     after the writer closed it. Bytes written before the lock are hashed; none can be
//     written after it, by anyone, until msiexec ends. An open writer, or a writable
//     mapping, makes the lock itself fail.
//   - msiexec gets the final path of the locked handle: no junction or link is left in it,
//     and a directory with an open file below it cannot be renamed, so the path keeps
//     naming the locked file. The file must be on a local fixed volume.
//   - Nothing else reaches the elevated process: the fixed System32 msiexec.exe, a fixed
//     command line, no log file (an elevated write to a user-writable path is the same
//     mistake again), and an MSI with its cabinet inside it.
//
// What it assumes and does not stop: a program running as this same user can end this
// process, which drops the lock, or inject code into it. Such a program can also show the
// same UAC prompt for an MSI of its own, so the lock takes away quiet substitution, not
// that. The compiled-in pin is what hyperbutler.exe's own file in Program Files vouches for,
// and TLS and redirects add nothing to the check: only the hash decides.
//
// A download is only ever started by the user's request: this is not the update check
// (REQ-UX-022), and it sends nothing but the request for the file.
package sandboxmsi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// ReleaseBase is where a release's assets are; <tag>/<file name> follows.
const ReleaseBase = "https://github.com/Higangssh/hyperbutler/releases/download/"

// MaxPinBytes bounds the size a pin may state: a pin above it is a broken build. The MSI is
// about 260 MB (docs/release.md); dist/msi/build-sandbox-msi.ps1 has the same limit.
const MaxPinBytes = 1 << 30

// Pin is the installer of this release, as compiled into hyperbutler.exe.
type Pin struct {
	SHA256 string // lowercase hex
	Name   string // the release asset's file name
	Bytes  int64
	// ProductCode is the MSI's, uppercase, without braces: what tells the same file from
	// another build of the same version.
	ProductCode string
}

var (
	pinName        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}\.msi$`)
	pinProductCode = regexp.MustCompile(`^[0-9A-F]{8}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{12}$`)
	// A release tag as dist/package.ps1 accepts it. Pseudo-versions match too, which a
	// caller with updatecheck.IsDevBuild keeps out.
	// Pre-release identifiers as semver has them: dot-separated and none empty, so no "..".
	releaseTag = regexp.MustCompile(`^v\d+\.\d+\.\d+(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)
)

// ErrNoPin is a build that carries no installer pin: a development build.
var ErrNoPin = errors.New("this build carries no pin for the sandbox installer")

// ParsePin checks the pin as linked in. All empty is ErrNoPin; anything else that is not a
// whole, well-formed pin is a broken build and an error of its own.
func ParsePin(sum, name, size, productCode string) (Pin, error) {
	if sum == "" && name == "" && size == "" && productCode == "" {
		return Pin{}, ErrNoPin
	}
	if len(sum) != 64 || strings.Trim(sum, "0123456789abcdef") != "" {
		return Pin{}, fmt.Errorf("this hyperbutler.exe carries a malformed sandbox installer hash %q; download the release again", sum)
	}
	if !pinName.MatchString(name) || strings.Contains(name, "..") {
		return Pin{}, fmt.Errorf("this hyperbutler.exe carries a malformed sandbox installer name %q; download the release again", name)
	}
	n, err := strconv.ParseInt(size, 10, 64)
	if err != nil || n <= 0 || n > MaxPinBytes || size != strconv.FormatInt(n, 10) {
		return Pin{}, fmt.Errorf("this hyperbutler.exe carries a malformed sandbox installer size %q; download the release again", size)
	}
	if !pinProductCode.MatchString(productCode) {
		return Pin{}, fmt.Errorf("this hyperbutler.exe carries a malformed sandbox installer product code %q; download the release again", productCode)
	}
	return Pin{SHA256: sum, Name: name, Bytes: n, ProductCode: productCode}, nil
}

// ReleaseURL is the address of the asset name in the release tag. Both go into a URL, so a
// tag that is not one, or a name that is not a pin's, is refused rather than escaped.
func ReleaseURL(tag, name string) (string, error) {
	if !releaseTag.MatchString(tag) {
		return "", fmt.Errorf("%q is not a release version", tag)
	}
	if !pinName.MatchString(name) || strings.Contains(name, "..") {
		return "", fmt.Errorf("%q is not a release asset name", name)
	}
	return ReleaseBase + tag + "/" + name, nil
}

// Errors of Installer.Run. The caller's message says what to do about each.
var (
	// ErrHash is a file that is not the pinned one.
	ErrHash = errors.New("the file is not the installer this hyperbutler was released with")
	// ErrDeclined is the UAC prompt answered no (or nobody there to answer it).
	ErrDeclined = errors.New("the permission prompt was declined, so nothing was installed")
	// ErrCanceled is msiexec's 1602: the user canceled the install.
	ErrCanceled = errors.New("the install was canceled")
	// ErrBusy is msiexec's 1618: another install is running.
	ErrBusy = errors.New("another Windows Installer install is running; try again when it has finished")
)

// ExitError is any other msiexec exit code.
type ExitError struct{ Code uint32 }

func (e *ExitError) Error() string {
	switch e.Code {
	case 1603:
		return "msiexec exit code 1603: the install failed, or refused (it needs hyperbutler installed, and no newer sandbox installed)"
	case 1619:
		return "msiexec exit code 1619: Windows Installer could not open the file"
	case 1638:
		return "msiexec exit code 1638: another version of the sandbox is installed; remove it in Apps first"
	}
	return fmt.Sprintf("msiexec exit code %d", e.Code)
}

// Result says how an install ended.
type Result struct {
	// RestartNeeded: msiexec asked for a restart (3010, or 1641), which it was told not to
	// make; a file that was in use is replaced at the next one.
	RestartNeeded bool
}

// outcome maps msiexec's exit code.
func outcome(code uint32) (Result, error) {
	switch code {
	case 0:
		return Result{}, nil
	case 3010, 1641:
		return Result{RestartNeeded: true}, nil
	case 1602:
		return Result{}, ErrCanceled
	case 1618:
		return Result{}, ErrBusy
	}
	return Result{}, &ExitError{Code: code}
}

// Locked is the file between the hash and the end of the install: nobody can write, delete
// or rename it while it is open.
type Locked interface {
	// Path names the locked file, with no link in it.
	Path() string
	// SHA256 hashes the file through this handle, as lowercase hex.
	SHA256() (string, error)
	Close() error
}

// Installer runs one install. Every step that touches the machine is a field, so the tests
// run the whole sequence without an elevation.
type Installer struct {
	Pin Pin
	// Dir is a private folder of this user's, for the temporary file.
	Dir string
	// Fetch writes the installer to path, which it must create, and at most max bytes.
	Fetch func(ctx context.Context, path string, max int64) error
	// Lock opens path as Locked. Windows: Lock in this package.
	Lock func(path string) (Locked, error)
	// Exec runs msiexec on the locked path and waits for it. Windows: Exec.
	Exec func(ctx context.Context, path string, reinstall bool) (uint32, error)
	// Verify checks what is installed against the manifest pin.
	Verify func() error
	// Reinstall makes msiexec rewrite the files of a product that is already installed,
	// for the same MSI only: it otherwise does nothing there, and on any other MSI of this
	// upgrade code, REINSTALL would remove the old product and install nothing.
	Reinstall bool
	// Note says what is going on.
	Note func(format string, args ...any)
}

// RemoveLeftovers deletes what an earlier run that was killed left in Dir. A file a run
// still holds does not go, and is none of this one's business.
func RemoveLeftovers(dir string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".msi") {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// tempName is a name nobody could have prepared: Fetch creates it and fails if it exists.
func tempName(name string) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]) + "-" + name, nil
}

// Run fetches, locks, hashes, installs and verifies, and removes the temporary file
// whatever happens.
func (i *Installer) Run(ctx context.Context) (Result, error) {
	note := i.Note
	if note == nil {
		note = func(string, ...any) {}
	}
	RemoveLeftovers(i.Dir)
	name, err := tempName(i.Pin.Name)
	if err != nil {
		return Result{}, err
	}
	path := filepath.Join(i.Dir, name)
	defer func() { _ = os.Remove(path) }()
	if err := i.Fetch(ctx, path, i.Pin.Bytes); err != nil {
		return Result{}, err
	}
	l, err := i.Lock(path)
	if err != nil {
		return Result{}, fmt.Errorf("lock the downloaded file: %w", err)
	}
	// Closed before the deferred remove above, which a held file would refuse.
	defer func() { _ = l.Close() }()
	sum, err := l.SHA256()
	if err != nil {
		return Result{}, fmt.Errorf("hash the downloaded file: %w", err)
	}
	if sum != i.Pin.SHA256 {
		return Result{}, fmt.Errorf("%w (sha256 %s, expected %s); nothing was installed", ErrHash, sum, i.Pin.SHA256)
	}
	note("The file matches this release's hash. Windows asks for permission to install it.")
	code, err := i.Exec(ctx, l.Path(), i.Reinstall)
	if err != nil {
		return Result{}, err
	}
	res, err := outcome(code)
	if err != nil {
		return res, err
	}
	if err := i.Verify(); err != nil {
		if res.RestartNeeded {
			return res, fmt.Errorf("the installer needs a restart of Windows before the image can be checked (%w); restart, then run this again", err)
		}
		return res, fmt.Errorf("the installer finished, but the image it left is not the one this hyperbutler was built with: %w", err)
	}
	return res, nil
}

// Copy is a Fetch from a local file (`sandbox install --msi`): the same bound, and the same
// hash check afterward, so where the file came from does not matter.
func Copy(src string) func(ctx context.Context, path string, max int64) error {
	return func(ctx context.Context, path string, max int64) (err error) {
		in, err := os.Open(src) //nolint:gosec // G304: the user's own --msi argument, hashed before use
		if err != nil {
			return err
		}
		defer func() { _ = in.Close() }()
		out, err := createNew(path)
		if err != nil {
			return err
		}
		defer func() {
			if cerr := out.Close(); err == nil {
				err = cerr
			}
		}()
		return copyBounded(ctx, out, in, max, nil)
	}
}

// createNew creates path, which must not exist.
func createNew(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: a random name in the caller's private folder
}

// ErrTooLarge is more bytes than the pin allows.
var ErrTooLarge = errors.New("the file is larger than the installer this hyperbutler was released with")

// copyBounded copies at most max bytes, and fails on more without writing them.
func copyBounded(ctx context.Context, dst io.Writer, src io.Reader, max int64, progress func(int64)) error {
	buf := make([]byte, 256<<10)
	var done int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, rerr := src.Read(buf)
		if n > 0 {
			if done+int64(n) > max {
				return ErrTooLarge
			}
			if _, err := dst.Write(buf[:n]); err != nil {
				return err
			}
			done += int64(n)
			if progress != nil {
				progress(done)
			}
		}
		if errors.Is(rerr, io.EOF) {
			return nil
		}
		if rerr != nil {
			return rerr
		}
	}
}
