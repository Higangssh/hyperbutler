package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// guestScript runs as rdinit inside Alpine's stock initramfs (appended as a second cpio
// archive). It only uses busybox applets and modules already in initramfs-virt, plus any
// extra modules the host packs under /wbmods (insmod'ed in WB_MODS order). Each
// "@@M <label>" line asks the host to measure the image; the guest then waits for a line
// on the console before going on, so host and guest stay in lock step.
//
// Kernel command line variables: WB_MB (MiB to write), WB_DEV (vda for virtio-blk, sda for
// Hyper-V SCSI), WB_MODS (comma-separated .ko names under /wbmods), WB_FSYNC (non-empty:
// dd uses conv=fsync). Note that sync(2) on FAT does not send a device flush; only fsync does.
//
// The guest filesystem is FAT32 because initramfs-virt has mkfs.vfat but neither mke2fs nor
// an ext4 module. FAT32 with one big file is a best case: fstrim sends a few huge,
// aligned discards, where ext4 after churn sends many small ones. Every
// layer this spike measures (block device discard, VMM block layer, host file) sits below
// the filesystem, so ext4 would issue the same kind of discards; see the doc for that caveat.
const guestScript = `#!/bin/sh
export PATH=/usr/bin:/usr/sbin:/bin:/sbin
/usr/bin/busybox mkdir -p /proc /sys /dev /mnt /bin /sbin /tmp
/usr/bin/busybox mount -t proc proc /proc
/usr/bin/busybox mount -t sysfs sys /sys
/usr/bin/busybox mount -t devtmpfs dev /dev
/usr/bin/busybox --install -s
m() { echo "@@M $1"; read -r _ack; }
D=/dev/${WB_DEV:-vda}
Q=/sys/block/${WB_DEV:-vda}/queue
# kmod has no "insmod" subcommand; it acts as insmod only when argv[0] is insmod.
[ -n "$WB_MODS" ] && /usr/bin/busybox ln -sf "$(command -v kmod)" /tmp/insmod
for k in $(echo "$WB_MODS" | tr ',' ' '); do /tmp/insmod /wbmods/$k 2>&1 | sed 's/^/@@E insmod: /'; done
modprobe virtio_blk 2>&1
modprobe vfat 2>&1
modprobe nls_cp437 2>&1
modprobe nls_ascii 2>&1
i=0; while [ ! -b $D ] && [ $i -lt 100 ]; do sleep 0.1; i=$((i+1)); done
[ -b $D ] || { echo "@@E no $D"; ls /sys/block | sed 's/^/@@E have: /'; echo "@@DONE"; poweroff -f; }
echo "@@I discard_granularity=$(cat $Q/discard_granularity) discard_max_bytes=$(cat $Q/discard_max_bytes) write_zeroes_max_bytes=$(cat $Q/write_zeroes_max_bytes)"
for f in /sys/block/${WB_DEV:-vda}/device/scsi_disk/*/provisioning_mode; do [ -f $f ] && echo "@@I provisioning_mode=$(cat $f)"; done
# WB_MODE=align: no filesystem. Fill the device, then discard 256 MiB starting off a 32 MiB
# boundary (4 MiB) and 256 MiB starting on one (512 MiB), measuring after each.
if [ "$WB_MODE" = align ]; then
	echo "@@I write: $(dd if=/dev/urandom of=$D bs=1M count=$WB_MB conv=fsync 2>&1 | tail -n 1)"
	m written
	echo "@@I unaligned: $(blkdiscard -o 4194304 -l 268435456 $D 2>&1 && echo ok || echo failed)"
	m discard-unaligned-256M
	echo "@@I aligned: $(blkdiscard -o 536870912 -l 268435456 $D 2>&1 && echo ok || echo failed)"
	m discard-aligned-256M
	echo "@@DONE"
	poweroff -f
fi
# WB_MODE=mem: no disk I/O. Fill WB_MB MiB of tmpfs with random data, free it, then wait and
# drop caches; the host prints vmmem before each measurement.
if [ "$WB_MODE" = mem ]; then
	mi() { echo "@@I meminfo: $(grep -E '^(MemTotal|MemFree|Cached):' /proc/meminfo | tr -s ' ' | tr '\n' ' ')"; }
	echo "@@I modules: $(cut -d' ' -f1 /proc/modules | tr '\n' ' ')"
	dmesg | grep -iE 'balloon|page.?report|hot.?add' | sed 's/^/@@I dmesg: /'
	mi; m booted
	mkdir -p /m; mount -t tmpfs -o size=$((WB_MB + 64))M tmpfs /m
	dd if=/dev/urandom of=/m/x bs=1M count=$WB_MB 2>/dev/null
	mi; m allocated
	rm /m/x; umount /m
	mi; m freed
	sleep 15; mi; m freed-15s
	sleep 15; mi; m freed-30s
	echo 3 >/proc/sys/vm/drop_caches; sleep 15; mi; m dropped-15s
	# Page cache: read the 600 MiB from the attached disk, then drop caches.
	dd if=$D of=/dev/null bs=1M count=$WB_MB 2>/dev/null
	mi; m cache-filled
	echo 3 >/proc/sys/vm/drop_caches; sleep 30; mi; m cache-dropped-30s
	dmesg | grep -iE 'balloon|page.?report' | tail -n 5 | sed 's/^/@@I dmesg: /'
	echo "@@DONE"
	poweroff -f
fi
# WB_MODE=full: host-disk-full test. ext4, then write 256 KiB files until a write fails or
# WB_MB*4 files exist. The host frees space and resumes the VM if it stalls. Then delete half,
# fstrim, and unmount. WB_MODE=check (same disk, no mkfs) runs e2fsck -fn and re-reads every
# file, to find corruption after the fill.
if [ "$WB_MODE" = full ] || [ "$WB_MODE" = check ]; then
	export LD_LIBRARY_PATH=/wbextra
	if [ "$WB_MODE" = full ]; then
		/wbextra/mke2fs -q -F -t ext4 -E nodiscard,lazy_itable_init=0,lazy_journal_init=0 $D 2>&1 | sed 's/^/@@E mke2fs: /'
		mount -t ext4 $D /mnt || echo "@@E mount"
		mkdir /mnt/d; sync
		m formatted
		i=0; fail=0
		while [ $i -lt $((WB_MB * 4)) ]; do
			if ! head -c 262144 /dev/urandom >/mnt/d/f$i 2>/dev/null; then fail=1; break; fi
			[ $((i % 400)) -eq 0 ] && echo "@@I progress: $i files"
			i=$((i + 1))
		done
		echo "@@I wrote $i files, write_failed=$fail"
		sync; echo "@@I sync exit=$?"
		dmesg | grep -iE 'error|I/O|EXT4-fs|sd ' | tail -n 8 | sed 's/^/@@I dmesg: /'
		echo "@@I df: $(df -k /mnt | tail -n 1)"
		m filled
		j=0; while [ $j -lt $i ]; do [ $(((j / 8) % 2)) -eq 0 ] && rm -f /mnt/d/f$j; j=$((j + 1)); done
		sync
		echo "@@I fstrim: $(fstrim -v /mnt 2>&1)"
		m half-deleted-trimmed
		umount /mnt; echo "@@I umount exit=$?"
	else
		echo "@@I e2fsck: $(/wbextra/e2fsck -fn $D >/tmp/fsck.log 2>&1; echo exit=$?)"
		tail -n 6 /tmp/fsck.log | sed 's/^/@@I fsck: /'
		mount -t ext4 -o ro $D /mnt || echo "@@E mount"
		n=0; bad=0
		for f in /mnt/d/*; do n=$((n + 1)); cat "$f" >/dev/null 2>&1 || bad=$((bad + 1)); done
		echo "@@I reread files=$n read_errors=$bad"
		dmesg | grep -iE 'error|EXT4-fs' | tail -n 5 | sed 's/^/@@I dmesg: /'
		umount /mnt
		# The first e2fsck -n runs before journal replay; mounting replays it. Check again.
		echo "@@I e2fsck after replay: $(/wbextra/e2fsck -fn $D >/tmp/fsck2.log 2>&1; echo exit=$?)"
		grep -vE '^$' /tmp/fsck2.log | tail -n 8 | sed 's/^/@@I fsck2: /'
		m checked
	fi
	echo "@@DONE"
	poweroff -f
fi
# WB_MODE=ext4: ext4 (mke2fs from /wbextra), WB_MB*4 files of 256 KiB, delete, fstrim twice,
# then a second write/delete churn and one more fstrim. Closer to image layers than FAT32.
if [ "$WB_MODE" = ext4 ]; then
	export LD_LIBRARY_PATH=/wbextra
	/wbextra/mke2fs -q -F -t ext4 -E nodiscard,lazy_itable_init=0,lazy_journal_init=0 $D 2>&1 | sed 's/^/@@E mke2fs: /'
	mount -t ext4 $D /mnt || echo "@@E mount"
	sync
	m formatted
	fill() { mkdir /mnt/d; i=0; while [ $i -lt $((WB_MB * 4)) ]; do head -c 262144 /dev/urandom >/mnt/d/f$i; i=$((i + 1)); done; sync; }
	fill
	echo "@@I df: $(df -k /mnt | tail -n 1)"
	m written
	if [ -n "$WB_GROUP" ]; then
		# Delete alternating groups of WB_GROUP files, leaving holes of WB_GROUP*256 KiB
		# between kept files, closer to deleting some image layers but not others.
		i=0; while [ $i -lt $((WB_MB * 4)) ]; do [ $(((i / WB_GROUP) % 2)) -eq 0 ] && rm /mnt/d/f$i; i=$((i + 1)); done
		sync
		echo "@@I df after partial delete: $(df -k /mnt | tail -n 1)"
		m deleted-half
		echo "@@I fstrim: $(fstrim -v /mnt 2>&1)"
		m fstrimmed-half
		umount /mnt
		echo "@@DONE"
		poweroff -f
	fi
	rm -rf /mnt/d; sync
	m deleted
	echo "@@I fstrim: $(fstrim -v /mnt 2>&1)"
	m fstrimmed
	echo "@@I fstrim2: $(fstrim -v /mnt 2>&1)"
	m fstrimmed-again
	fill; rm -rf /mnt/d; sync
	m churned
	echo "@@I fstrim3: $(fstrim -v /mnt 2>&1)"
	m churn-fstrimmed
	umount /mnt
	echo "@@DONE"
	poweroff -f
fi
mkfs.vfat -F 32 $D >/dev/null || echo "@@E mkfs"
mount -t vfat -o iocharset=ascii $D /mnt || echo "@@E mount"
m formatted
echo "@@I write: $(dd if=/dev/urandom of=/mnt/big bs=1M count=$WB_MB ${WB_FSYNC:+conv=fsync} 2>&1 | tail -n 1)"; sync
echo "@@I df: $(df -k /mnt | tail -n 1)"
m written
rm /mnt/big; sync
m deleted
t0=$(date +%s)
echo "@@I fstrim: $(fstrim -v /mnt 2>&1)"
echo "@@I fstrim_seconds=$(( $(date +%s) - t0 ))"
m fstrimmed
echo "@@I rewrite: $(dd if=/dev/urandom of=/mnt/big2 bs=1M count=$WB_MB ${WB_FSYNC:+conv=fsync} 2>&1 | tail -n 1)"; sync
m rewritten
sha1=$(sha1sum /mnt/big2 | cut -d' ' -f1)
umount /mnt
mount -t vfat -o iocharset=ascii $D /mnt
sha2=$(sha1sum /mnt/big2 | cut -d' ' -f1)
[ "$sha1" = "$sha2" ] && echo "@@I reread_ok" || echo "@@E reread_mismatch"
rm /mnt/big2
if [ "$WB_MODE" = trimonly ]; then
	echo "@@I fstrim2: $(fstrim -v /mnt 2>&1)"
	umount /mnt
	m fstrimmed-again
else
	umount /mnt
	echo "@@I blkdiscard: $(blkdiscard $D 2>&1 && echo ok || echo failed)"
	m blkdiscarded
fi
echo "@@DONE"
poweroff -f
`

func runQEMU(args []string) error {
	fs := flag.NewFlagSet("qemu", flag.ExitOnError)
	backend := fs.String("backend", "raw", "raw | qcow2 | nbd")
	guestDir := fs.String("guest", filepath.Join(os.TempDir(), "hb-dev", "winbox-spike", "guest"), "dir with vmlinuz-virt and initramfs-virt")
	diskSize := fs.Int64("size", 2*gib, "virtual disk size in bytes")
	writeMB := fs.Int("mb", 1024, "MiB the guest writes and deletes")
	flush := fs.Bool("hostflush", true, "flush the image from the host before each measurement")
	fsync := fs.Bool("fsync", false, "guest dd uses conv=fsync, so the guest sends a device flush")
	fs.Parse(args)

	initrd, err := buildInitrd(filepath.Join(*guestDir, "initramfs-virt"), nil)
	if err != nil {
		return err
	}

	var image string
	var drive []string
	switch *backend {
	case "raw":
		image = fresh("guest.raw")
		if err := newSparseFile(image, *diskSize); err != nil {
			return err
		}
		drive = []string{"-drive", "if=none,id=d0,format=raw,discard=unmap,detect-zeroes=unmap,file=" + qemuOpt(image)}
	case "qcow2":
		image = fresh("guest.qcow2")
		out, err := exec.Command(filepath.Join(*qemuDir, "qemu-img.exe"), "create", "-f", "qcow2", image, fmt.Sprint(*diskSize)).CombinedOutput()
		if err != nil {
			return fmt.Errorf("qemu-img: %w\n%s", err, out)
		}
		drive = []string{"-drive", "if=none,id=d0,format=qcow2,discard=unmap,detect-zeroes=unmap,file=" + qemuOpt(image)}
	case "nbd":
		image = fresh("guest-nbd.raw")
		if err := newSparseFile(image, *diskSize); err != nil {
			return err
		}
		srv, err := serveNBD(image)
		if err != nil {
			return err
		}
		defer srv.Close()
		addr := srv.ln.Addr().(*net.TCPAddr)
		drive = []string{"-blockdev", fmt.Sprintf(
			"driver=nbd,node-name=d0,server.type=inet,server.host=127.0.0.1,server.port=%d,export=disk,discard=unmap,detect-zeroes=unmap",
			addr.Port)}
	default:
		return fmt.Errorf("unknown backend %q", *backend)
	}

	// The serial console goes to a TCP socket so the host can answer the guest's prompts.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer ln.Close()

	qargs := []string{
		"-accel", "whpx", "-m", "1024", "-smp", "2",
		"-display", "none", "-monitor", "none", "-nic", "none", "-no-reboot",
		"-serial", "tcp:" + ln.Addr().String(),
		"-kernel", filepath.Join(*guestDir, "vmlinuz-virt"),
		"-initrd", initrd,
		"-append", fmt.Sprintf("console=ttyS0 rdinit=/wbinit loglevel=3 WB_DEV=vda WB_MB=%d%s", *writeMB, map[bool]string{true: " WB_FSYNC=1"}[*fsync]),
	}
	qargs = append(qargs, drive...)
	qargs = append(qargs, "-device", "virtio-blk-pci,drive=d0,discard=on,write-zeroes=on")
	cmd := exec.Command(filepath.Join(*qemuDir, "qemu-system-x86_64.exe"), qargs...)
	var qemuOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &qemuOut, &qemuOut
	fmt.Printf("backend=%s image=%s guest writes %d MiB hostflush=%v fsync=%v\n", *backend, image, *writeMB, *flush, *fsync)
	start := time.Now()
	if err := cmd.Start(); err != nil {
		return err
	}
	// Runs before the NBD server's Close (defers are LIFO), so an error path never leaves a
	// QEMU process holding the image. After a normal exit Kill just returns an error.
	defer cmd.Process.Kill()
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	conn, err := acceptOrExit(ln, exited)
	if err != nil {
		return fmt.Errorf("%w\n%s", err, qemuOut.String())
	}
	defer conn.Close()

	report("before boot", image)
	var before func()
	if *flush {
		before = func() { hostFlush(image) }
	}
	done, err := driveGuest(conn, image, start, 0, before)
	if err != nil {
		return err
	}
	select {
	case <-exited:
	case <-time.After(30 * time.Second):
		cmd.Process.Kill()
	}
	if !done {
		return fmt.Errorf("guest did not finish\n%s", qemuOut.String())
	}
	report("after QEMU exit", image)
	return nil
}

// driveGuest follows the guest console, measures the image at every "@@M" marker, and
// acknowledges it. With settle > 0 it measures a second time after that delay, to catch
// host-side work that finishes after the guest's request completed.
func driveGuest(conn io.ReadWriter, image string, start time.Time, settle time.Duration, beforeMeasure func()) (bool, error) {
	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		switch {
		case strings.HasPrefix(line, "@@M "):
			if beforeMeasure != nil {
				beforeMeasure()
			}
			report(fmt.Sprintf("[%6.1fs] %s", time.Since(start).Seconds(), line[4:]), image)
			if settle > 0 {
				time.Sleep(settle)
				report(fmt.Sprintf("[%6.1fs] %s +%v", time.Since(start).Seconds(), line[4:], settle), image)
			}
			if _, err := conn.Write([]byte("ok\n")); err != nil {
				return false, err
			}
		case strings.HasPrefix(line, "@@I "), strings.HasPrefix(line, "@@E "):
			fmt.Println("  guest:", line[4:])
		case line == "@@DONE":
			return true, nil
		case strings.Contains(line, "Kernel panic"), strings.Contains(line, "not found"), strings.Contains(line, "rror"):
			fmt.Println("  console:", line)
		}
	}
	return false, sc.Err()
}

// hostFlush pushes the host cache for the image to disk before measuring. Without it,
// GetCompressedFileSizeW lagged far behind (111 MiB reported right after the guest had
// written and synced 1 GiB): QEMU writes through the cache manager, and the allocation
// only showed up once the cached data was written out.
func hostFlush(path string) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		fmt.Println("  host flush: open:", err)
		return
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		fmt.Println("  host flush:", err)
	}
}

// qemuOpt escapes a value for a QEMU option string, where "," separates options.
func qemuOpt(s string) string { return strings.ReplaceAll(s, ",", ",,") }

func acceptOrExit(ln net.Listener, exited <-chan error) (net.Conn, error) {
	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() { c, err := ln.Accept(); ch <- res{c, err} }()
	select {
	case r := <-ch:
		return r.c, r.err
	case err := <-exited:
		return nil, fmt.Errorf("qemu exited before connecting the serial port: %v", err)
	case <-time.After(30 * time.Second):
		return nil, fmt.Errorf("timeout waiting for qemu serial connection")
	}
}

func newSparseFile(path string, size int64) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := setSparse(f); err != nil {
		return err
	}
	return f.Truncate(size)
}

// buildInitrd writes Alpine's initramfs followed by a second gzip'd newc cpio that adds
// /wbinit. The kernel unpacks concatenated archives in order.
func buildInitrd(alpine string, modules []string) (string, error) {
	return buildInitrdExtra(alpine, modules, "")
}

// buildInitrdExtra also packs every file in extraDir (flat) under /wbextra, for guest
// tools that initramfs-virt lacks, such as mke2fs and its shared libraries.
func buildInitrdExtra(alpine string, modules []string, extraDir string) (string, error) {
	base, err := os.ReadFile(alpine)
	if err != nil {
		return "", err
	}
	var extra bytes.Buffer
	zw := gzip.NewWriter(&extra)
	cw := cpioWriter{w: zw}
	cw.file("wbinit", 0o100755, []byte(guestScript))
	if len(modules) > 0 {
		cw.entry("wbmods", 0o040755, nil)
	}
	for _, m := range modules {
		data, err := os.ReadFile(m)
		if err != nil {
			return "", err
		}
		cw.file("wbmods/"+filepath.Base(m), 0o100644, data)
	}
	if extraDir != "" {
		files, err := os.ReadDir(extraDir)
		if err != nil {
			return "", err
		}
		cw.entry("wbextra", 0o040755, nil)
		for _, f := range files {
			data, err := os.ReadFile(filepath.Join(extraDir, f.Name()))
			if err != nil {
				return "", err
			}
			cw.file("wbextra/"+f.Name(), 0o100755, data)
		}
	}
	cw.trailer()
	if cw.err != nil {
		return "", cw.err
	}
	if err := zw.Close(); err != nil {
		return "", err
	}
	out := filepath.Join(*dir, "initramfs-wb")
	return out, os.WriteFile(out, append(base, extra.Bytes()...), 0o644)
}

// cpioWriter writes the "newc" (070701) format the kernel's initramfs unpacker reads.
type cpioWriter struct {
	w   interface{ Write([]byte) (int, error) }
	n   int64
	ino int
	err error
}

func (c *cpioWriter) write(b []byte) {
	if c.err != nil {
		return
	}
	n, err := c.w.Write(b)
	c.n += int64(n)
	c.err = err
}

func (c *cpioWriter) pad() {
	if r := c.n % 4; r != 0 {
		c.write(make([]byte, 4-r))
	}
}

func (c *cpioWriter) entry(name string, mode uint32, data []byte) {
	c.ino++
	hdr := fmt.Sprintf("070701%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X",
		c.ino, mode, 0, 0, 1, 0, len(data), 0, 0, 0, 0, len(name)+1, 0)
	c.write([]byte(hdr))
	c.write(append([]byte(name), 0))
	c.pad()
	c.write(data)
	c.pad()
}

func (c *cpioWriter) file(name string, mode uint32, data []byte) { c.entry(name, mode, data) }
func (c *cpioWriter) trailer()                                   { c.entry("TRAILER!!!", 0, nil) }
