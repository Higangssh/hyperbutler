//go:build windows

package engine

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/guest/image"
)

// guestImage is a verified image set (ADR 0005): the kernel, the stage-1 initramfs and
// the read-only system disk named by image.json, each checked against the manifest's
// size and SHA-256.
//
// The files stay open, readable by others but not writable or deletable
// (FILE_SHARE_READ only), from the check until the VM is gone. So what HCS attaches is
// what was hashed: nothing can rewrite a file between the check and the boot, nor while
// the guest reads its system disk.
type guestImage struct {
	man                    *image.Manifest
	kernel, initrd, system string
	files                  []*os.File
}

func (g *guestImage) Close() error {
	var err error
	for _, f := range g.files {
		err = errors.Join(err, f.Close())
	}
	g.files = nil
	return err
}

// openImage reads the manifest at path and verifies the set it names in the same
// directory. Any mismatch (a half-copied update, a file from another build) is an
// error: booting a stage 1 against the wrong system disk is never attempted.
func openImage(path string) (_ *guestImage, err error) {
	path, err = filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	b, err := ReadFileBounded(path)
	if err != nil {
		return nil, fmt.Errorf("engine: guest image: %w", err)
	}
	m, err := image.ParseManifest(b)
	if err != nil {
		return nil, fmt.Errorf("engine: %s: %w", path, err)
	}
	dir := filepath.Dir(path)
	g := &guestImage{man: m}
	defer func() {
		if err != nil {
			_ = g.Close()
		}
	}()
	for i, want := range m.Files() {
		p := filepath.Join(dir, want.Name)
		f, err := openDenyWrite(p)
		if err != nil {
			return nil, fmt.Errorf("engine: guest image file %s: %w", p, err)
		}
		g.files = append(g.files, f)
		if err := want.Check(f); err != nil {
			return nil, fmt.Errorf("engine: %w (rebuild or reinstall the guest image)", err)
		}
		switch i {
		case 0:
			g.kernel = p
		case 1:
			g.initrd = p
		case 2:
			g.system = p
			if err := checkFooter(f, want.Size); err != nil {
				return nil, fmt.Errorf("engine: %s: %w", p, err)
			}
		}
	}
	return g, nil
}

// checkFooter checks that the system disk ends in a fixed-VHD footer for exactly the
// bytes before it.
func checkFooter(f *os.File, size uint64) error {
	var b [image.FooterSize]byte
	if _, err := f.ReadAt(b[:], int64(size)-image.FooterSize); err != nil && !errors.Is(err, io.EOF) {
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
