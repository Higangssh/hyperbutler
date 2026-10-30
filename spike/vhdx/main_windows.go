// Command vhdx-spike answers one question for REQ-DISK-001/002: can disk space freed inside
// the guest go back to the Windows host automatically, without admin rights?
//
// Subcommands (all run non-elevated; every file lives under -dir):
//
//	vhdx   create dynamic/sparse VHDX files with virtdisk.dll, then try sparse, compact, attach
//	raw    sparse raw file: FSCTL_SET_SPARSE / FSCTL_SET_ZERO_DATA granularity and cost
//	qemu   boot Alpine under QEMU-WHPX, write+delete+fstrim in the guest, measure the host file
//	       (-backend raw | qcow2 | nbd; nbd serves the image from this process)
//	hcs    the same guest run on HCS with the VHDX on Hyper-V SCSI (needs elevation; not run)
//	measure <file>...   print size and allocated bytes
//
// Throwaway code: see docs/research/disk-reclaim.md.
package main

import (
	"crypto/rand"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

const (
	gib = 1 << 30
	mb  = 1 << 20
)

var (
	dir     = flag.String("dir", filepath.Join(os.TempDir(), "hb-dev", "winbox-spike", "disk"), "scratch directory for images")
	qemuDir = flag.String("qemu", `C:\Program Files\qemu`, "QEMU install directory")
)

func main() {
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: vhdx-spike [flags] vhdx|raw|qemu|hcs|measure ...")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(2)
	}
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		fatal(err)
	}
	var err error
	switch flag.Arg(0) {
	case "vhdx":
		err = runVHDX()
	case "raw":
		err = runRaw()
	case "qemu":
		err = runQEMU(flag.Args()[1:])
	case "hcs":
		err = runHCS(flag.Args()[1:])
	case "measure":
		for _, p := range flag.Args()[1:] {
			u, merr := measure(p)
			if merr != nil {
				err = merr
				break
			}
			fmt.Printf("%s: %s\n", p, u)
		}
	default:
		flag.Usage()
		os.Exit(2)
	}
	if err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

func report(label, path string) usage {
	u, err := measure(path)
	if err != nil {
		fmt.Printf("  %-34s %v\n", label, err)
		return u
	}
	fmt.Printf("  %-34s %s\n", label, u)
	return u
}

func fresh(name string) string {
	p := filepath.Join(*dir, name)
	_ = os.Remove(p)
	return p
}

// qemuIO runs qemu-io against an image. It is only used to put guest-like data into a VHDX
// without attaching it (attach needs admin).
func qemuIO(format, path string, cmds ...string) error {
	args := []string{"-f", format}
	for _, c := range cmds {
		args = append(args, "-c", c)
	}
	args = append(args, path)
	out, err := exec.Command(filepath.Join(*qemuDir, "qemu-io.exe"), args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("qemu-io %v: %w\n%s", cmds, err, out)
	}
	return nil
}

func runVHDX() error {
	const size = 10 * gib

	fmt.Println("1. CreateVirtualDisk: dynamic VHDX, 10 GiB, 32 MiB blocks (default)")
	for _, tc := range []struct {
		name  string
		flags uint32
	}{
		{"flags=NONE", createFlagNone},
		{"flags=SPARSE_FILE", createFlagSparseFile},
		{"flags=SPARSE_FILE|SUPPORT_SPARSE_FILES_ANY_FS (what WSL uses)", createFlagSparseFile | createFlagSparseAnyFS},
	} {
		p := fresh(fmt.Sprintf("create-%x.vhdx", tc.flags))
		fmt.Println("  ", tc.name)
		if err := createVHDX(p, size, tc.flags); err != nil {
			fmt.Println("    ", err)
			continue
		}
		report("  after create", p)
	}

	fmt.Println("2. FSCTL_SET_SPARSE on a plain VHDX after creation, then OpenVirtualDisk")
	a := fresh("set-sparse.vhdx")
	if err := createVHDX(a, size, createFlagNone); err != nil {
		return err
	}
	if f, err := os.OpenFile(a, os.O_RDWR, 0); err != nil {
		fmt.Println("   open:", err)
	} else {
		if err := setSparse(f); err != nil {
			fmt.Println("  ", err)
		}
		f.Close()
		report("after FSCTL_SET_SPARSE", a)
	}
	for _, of := range []uint32{0, openFlagSparseAnyFS} {
		if h, err := openVHDX(a, accessMetaOps, of); err != nil {
			fmt.Println("  ", err)
		} else {
			fmt.Printf("   OpenVirtualDisk(METAOPS, flags=0x%x): ok\n", of)
			syscall.CloseHandle(h)
		}
	}

	// A guest cannot trim a VHDX that is not attached through vhdmp, and attaching needs
	// admin. The best a guest can do without vhdmp is overwrite free space with zeros, which
	// is what file-system-agnostic compaction looks for. qemu-io stands in for that guest.
	for _, tc := range []struct {
		name      string
		sparse    bool
		openFlags uint32
	}{
		{"plain VHDX", false, 0},
		{"sparse VHDX (FSCTL_SET_SPARSE), opened with SUPPORT_SPARSE_FILES_ANY_FS", true, openFlagSparseAnyFS},
	} {
		fmt.Printf("3. CompactVirtualDisk (file-system-agnostic, VHDX detached) on a %s\n", tc.name)
		p := fresh(fmt.Sprintf("compact-%v.vhdx", tc.sparse))
		if err := createVHDX(p, size, createFlagNone); err != nil {
			return err
		}
		if tc.sparse {
			f, err := os.OpenFile(p, os.O_RDWR, 0)
			if err != nil {
				return err
			}
			err = setSparse(f)
			f.Close()
			if err != nil {
				return err
			}
		}
		if err := qemuIO("vhdx", p, "write -P 0xab 0 512M"); err != nil {
			return err
		}
		report("after writing 512 MiB", p)
		if err := qemuIO("vhdx", p, "write -P 0 0 512M"); err != nil {
			return err
		}
		report("after overwriting with zeros", p)
		h, err := openVHDX(p, accessMetaOps, tc.openFlags)
		if err != nil {
			fmt.Println("  ", err)
			continue
		}
		t0 := time.Now()
		if err := compactVHDX(h); err != nil {
			fmt.Println("  ", err)
		}
		fmt.Printf("   CompactVirtualDisk returned after %v\n", time.Since(t0).Round(time.Millisecond))
		syscall.CloseHandle(h)
		report("after CompactVirtualDisk", p)
	}

	fmt.Println("4. AttachVirtualDisk read-only (needed for file-system-aware compaction)")
	c := fresh("attach.vhdx")
	if err := createVHDX(c, size, createFlagNone); err != nil {
		return err
	}
	h, err := openVHDX(c, accessMetaOps|accessAttachRO, 0)
	if err != nil {
		fmt.Println("  ", err)
		return nil
	}
	defer syscall.CloseHandle(h)
	if err := attachVHDX(h, attachFlagReadOnly|attachFlagNoDriveLetter); err != nil {
		fmt.Println("  ", err)
		return nil
	}
	fmt.Println("   attached (unexpected for a standard user); detaching")
	return detachVHDX(h)
}

func runRaw() error {
	p := fresh("sparse.raw")
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	fmt.Println("1. FSCTL_SET_SPARSE on a new file, then extend to 10 GiB")
	if err := setSparse(f); err != nil {
		return err
	}
	if err := f.Truncate(10 * gib); err != nil {
		return err
	}
	report("after set sparse + truncate", p)

	fmt.Println("2. Write 512 MiB of random data at offset 0")
	buf := make([]byte, 4*mb)
	for off := int64(0); off < 512*mb; off += int64(len(buf)) {
		if _, err := io.ReadFull(rand.Reader, buf); err != nil {
			return err
		}
		if _, err := f.WriteAt(buf, off); err != nil {
			return err
		}
	}
	if err := f.Sync(); err != nil {
		return err
	}
	report("after write", p)

	fmt.Println("3. FSCTL_SET_ZERO_DATA granularity (each range starts on a fresh 1 MiB boundary)")
	base := int64(0)
	for _, n := range []int64{4 << 10, 32 << 10, 64 << 10, 128 << 10, 1 << 20} {
		before, err := measure(p)
		if err != nil {
			return err
		}
		if err := zeroData(f, base, n); err != nil {
			return err
		}
		after, err := measure(p)
		if err != nil {
			return err
		}
		fmt.Printf("  zero %7d bytes at %4d MiB: allocation drops by %d bytes\n", n, base/mb, before.Allocated-after.Allocated)
		base += 1 << 20
	}
	before, err := measure(p)
	if err != nil {
		return err
	}
	if err := zeroData(f, base+512, 128<<10); err != nil {
		return err
	}
	after, err := measure(p)
	if err != nil {
		return err
	}
	fmt.Printf("  zero  131072 bytes at %d MiB + 512 (unaligned): allocation drops by %d bytes\n", base/mb, before.Allocated-after.Allocated)

	fmt.Println("4. FSCTL_SET_ZERO_DATA over the remaining written region")
	if err := zeroData(f, 0, 512*mb); err != nil {
		return err
	}
	report("after zero-data 512 MiB", p)
	rs, err := allocatedRanges(f, 10*gib)
	if err != nil {
		return err
	}
	fmt.Printf("  allocated ranges left: %d\n", len(rs))

	fmt.Println("5. Read back a deallocated range: must be zeros")
	if _, err := f.ReadAt(buf[:64<<10], 100*mb); err != nil {
		return err
	}
	for _, c := range buf[:64<<10] {
		if c != 0 {
			return fmt.Errorf("non-zero byte after FSCTL_SET_ZERO_DATA")
		}
	}
	fmt.Println("  ok")
	return nil
}
