// Command mksysdisk turns a raw erofs system image into the fixed VHD that winbox
// attaches read-only (it pads the image to whole MiB and appends a VHD footer), and
// writes the image manifest (image.json) that ties a kernel, an initramfs and a system
// disk of one build together. See docs/adr/0005-guest-image.md.
//
//	mksysdisk -erofs system.erofs -o out/system.vhd [-mtime N]
//	mksysdisk -manifest out/image.json -kernel out/vmlinuz-virt -initrd out/initramfs-winbox.gz -system out/system.vhd
//
// Output is deterministic: the footer's unique id is derived from the content and its
// timestamp from -mtime ($SOURCE_DATE_EPOCH, else 0).
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/Higangssh/winbox/guest/image"
)

// erofsMagic is the erofs superblock magic, at byte 1024 (EROFS_SUPER_OFFSET).
const erofsMagic = 0xE0F5E1E2

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "mksysdisk:", err)
		os.Exit(1)
	}
}

func run() error {
	erofs := flag.String("erofs", "", "raw erofs image to wrap")
	out := flag.String("o", "", "output .vhd")
	mtimeFlag := flag.String("mtime", "", "footer timestamp, Unix seconds (default: $SOURCE_DATE_EPOCH, else 0)")
	manifest := flag.String("manifest", "", "write this image.json instead")
	kernel := flag.String("kernel", "", "kernel, for -manifest")
	initrd := flag.String("initrd", "", "initramfs, for -manifest")
	system := flag.String("system", "", "system .vhd, for -manifest")
	flag.Parse()
	switch {
	case *manifest != "":
		if *kernel == "" || *initrd == "" || *system == "" {
			return errors.New("-manifest needs -kernel, -initrd and -system")
		}
		return writeManifest(*manifest, *kernel, *initrd, *system)
	case *erofs != "" && *out != "":
		mt := *mtimeFlag
		if mt == "" {
			mt = os.Getenv("SOURCE_DATE_EPOCH")
		}
		var sec int64
		if mt != "" {
			var err error
			if sec, err = strconv.ParseInt(mt, 10, 64); err != nil || sec < 0 {
				return fmt.Errorf("bad mtime %q", mt)
			}
		}
		return wrap(*erofs, *out, time.Unix(sec, 0).UTC())
	}
	flag.Usage()
	return errors.New("want -erofs and -o, or -manifest")
}

func wrap(in, out string, ts time.Time) error {
	f, err := os.Open(in)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	var sb [4]byte
	if _, err := f.ReadAt(sb[:], 1024); err != nil || binary.LittleEndian.Uint32(sb[:]) != erofsMagic {
		return fmt.Errorf("%s is not an erofs image", in)
	}
	n := uint64(fi.Size())
	size := (n + image.DiskAlign - 1) / image.DiskAlign * image.DiskAlign
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	var id [16]byte
	copy(id[:], h.Sum(nil))
	id[6] = id[6]&0x0f | 0x40 // a version 4 (random-format) UUID, as VHD tools expect
	id[8] = id[8]&0x3f | 0x80
	footer, err := image.EncodeFooter(size, ts, id)
	if err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	tmp := out + ".tmp"
	o, err := os.Create(tmp)
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(o, 1<<20)
	_, err = io.Copy(w, f)
	if err == nil {
		_, err = w.Write(make([]byte, size-n))
	}
	if err == nil {
		_, err = w.Write(footer[:])
	}
	if err == nil {
		err = w.Flush()
	}
	if cerr := o.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, out)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}

func writeManifest(path, kernel, initrd, system string) error {
	describe := func(p string) (image.File, error) {
		if filepath.Dir(p) != filepath.Dir(path) {
			return image.File{}, fmt.Errorf("%s must be next to %s", p, path)
		}
		f, err := os.Open(p)
		if err != nil {
			return image.File{}, err
		}
		defer f.Close()
		return image.Describe(filepath.Base(p), f)
	}
	var m image.Manifest
	m.Version = image.ManifestVersion
	var err error
	if m.Kernel, err = describe(kernel); err != nil {
		return err
	}
	if m.Initrd, err = describe(initrd); err != nil {
		return err
	}
	if m.System, err = describe(system); err != nil {
		return err
	}
	b, err := m.Encode()
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
