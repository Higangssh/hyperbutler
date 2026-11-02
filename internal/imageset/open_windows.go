//go:build windows

package imageset

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/guest/image"
)

// Set is a verified image set (ADR 0005): the kernel, the stage-1 initramfs and the
// read-only system disk named by image.json, each checked against the manifest's size
// and SHA-256.
//
// The files and image.json stay open, readable by others but not writable or deletable
// (FILE_SHARE_READ only), from the check until the VM is gone. So what HCS attaches is
// what was hashed: nothing can rewrite a file between the check and the boot, nor while
// the guest reads its system disk.
type Set struct {
	Manifest *image.Manifest
	// Kernel, Initrd and System are the final paths of the open files (finalPath): what
	// HCS is to open.
	Kernel, Initrd, System string
	files                  []*os.File
	system                 *os.File
}

// SystemFile is the open system disk, readable by others but not writable. It stays
// valid until Close.
func (g *Set) SystemFile() *os.File { return g.system }

// maxManifestSize bounds image.json.
const maxManifestSize = 64 << 10

// Close closes every file of the set. It is safe to call more than once.
func (g *Set) Close() error {
	var err error
	for _, f := range g.files {
		err = errors.Join(err, f.Close())
	}
	g.files = nil
	return err
}

// Open reads the manifest at path and verifies the set it names in the same directory. Any mismatch (a half-copied update, a file from another build) is an
// error: booting a stage 1 against the wrong system disk is never attempted.
//
// A non-empty pin is the SHA-256 that image.json must have (Pin, ADR 0005 Decision 4).
// image.json is opened and held like the other files, and the bytes hashed are the
// bytes parsed, so a replaced manifest is refused even when the files next to it match
// it.
func Open(path, pin string) (_ *Set, err error) {
	path, err = filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	g := &Set{}
	defer func() {
		if err != nil {
			_ = g.Close()
		}
	}()
	fix := "rebuild or reinstall the guest image"
	if pin != "" {
		fix = "extract the release zip again, replacing every file"
	}
	mf, err := openDenyWrite(path)
	if err != nil {
		return nil, fmt.Errorf("engine: guest image %s: %w (%s)", path, err, fix)
	}
	g.files = append(g.files, mf)
	b, err := io.ReadAll(io.LimitReader(mf, maxManifestSize+1))
	if err != nil {
		return nil, fmt.Errorf("engine: guest image %s: %w", path, err)
	}
	if len(b) > maxManifestSize {
		return nil, fmt.Errorf("engine: guest image %s is larger than %d bytes (%s)", path, maxManifestSize, fix)
	}
	if err := CheckPin(pin, path, b); err != nil {
		return nil, err
	}
	m, err := image.ParseManifest(b)
	if err != nil {
		return nil, fmt.Errorf("engine: %s: %w", path, err)
	}
	g.Manifest = m
	dir := filepath.Dir(path)
	for i, want := range m.Files() {
		p := filepath.Join(dir, want.Name)
		f, err := openDenyWrite(p)
		if err != nil {
			return nil, fmt.Errorf("engine: guest image file %s: %w (%s)", p, err, fix)
		}
		g.files = append(g.files, f)
		if err := want.Check(f); err != nil {
			return nil, fmt.Errorf("engine: %w (%s)", err, fix)
		}
		// HCS opens the files by path. Hand it the path of the file this handle holds,
		// with any junction or directory link on the way resolved: a link can be
		// retargeted while the file stays open, the real directories cannot be renamed.
		if p, err = FinalPath(f); err != nil {
			return nil, fmt.Errorf("engine: guest image file %s: %w", f.Name(), err)
		}
		switch i {
		case 0:
			g.Kernel = p
		case 1:
			g.Initrd = p
		case 2:
			g.System, g.system = p, f
			if err := checkFooter(f, want.Size); err != nil {
				return nil, fmt.Errorf("engine: %s: %w", p, err)
			}
		}
	}
	return g, nil
}

// GetFinalPathNameByHandle flags (fileapi.h).
const fileNameNormalized, volumeNameDOS = 0x0, 0x0

// FinalPath returns the normalized path of the open file f, as a plain drive-letter or
// UNC path.
func FinalPath(f *os.File) (string, error) { return FinalPathOf(windows.Handle(f.Fd())) }

// FinalPathOf is FinalPath for a handle.
func FinalPathOf(h windows.Handle) (string, error) {
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetFinalPathNameByHandle(h, &buf[0], windows.MAX_LONG_PATH,
		fileNameNormalized|volumeNameDOS)
	if err != nil {
		return "", err
	}
	if int(n) >= len(buf) {
		return "", errors.New("path too long")
	}
	p := windows.UTF16ToString(buf[:n])
	switch {
	case strings.HasPrefix(p, `\\?\UNC\`):
		return `\\` + p[len(`\\?\UNC\`):], nil
	case strings.HasPrefix(p, `\\?\`) && len(p) > 6 && p[5] == ':' && p[6] == '\\':
		return p[len(`\\?\`):], nil
	}
	return "", fmt.Errorf("unexpected final path %q", p)
}

// checkFooter checks that the system disk ends in a fixed-VHD footer for exactly the
// bytes before it.
func checkFooter(f *os.File, size uint64) error {
	var b [image.FooterSize]byte
	off := int64(size) - image.FooterSize //nolint:gosec // G115: a size above MaxInt64 wraps negative, refused below
	if off < 0 {
		return fmt.Errorf("vhd: system disk size %d is out of range", size)
	}
	if _, err := f.ReadAt(b[:], off); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	ft, err := image.ParseFooter(b[:])
	if err != nil {
		return err
	}
	if ft.Size != size-image.FooterSize {
		return fmt.Errorf("vhd: footer says %d bytes, the file holds %d", ft.Size, size-image.FooterSize)
	}
	return nil
}

// openDenyWrite opens path for reading and lets others only read it. It refuses
// anything but a regular file: no reparse points (links, junctions), no devices.
func openDenyWrite(path string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil,
		windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_SEQUENTIAL_SCAN, 0)
	if err != nil {
		return nil, err
	}
	var fi windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &fi); err != nil {
		_ = windows.CloseHandle(h)
		return nil, err
	}
	if fi.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_DEVICE) != 0 {
		_ = windows.CloseHandle(h)
		return nil, errors.New("not a regular file")
	}
	if ft, err := windows.GetFileType(h); err != nil || ft != windows.FILE_TYPE_DISK {
		_ = windows.CloseHandle(h)
		return nil, errors.New("not a regular file")
	}
	return os.NewFile(uintptr(h), path), nil
}
