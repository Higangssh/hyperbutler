// Command share-spike measures ways to expose Windows directories to the hyperbutler guest
// (docs/research/bind-mounts.md, ADR 0007). It boots a throwaway HCS VM whose PID 1 is
// spike/share/guest (sharebench), attaches an ext4 data disk as the baseline, optionally
// an HCS Plan9 share and the hyperbutler file server (internal/share), and drives benchmarks
// from the host.
//
// Run non-elevated; the account must be in Hyper-V Administrators (ADR 0004). Every file
// lives under -work (default %TEMP%\hb-dev\share).
//
// Throwaway code.
package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/Microsoft/go-winio/pkg/guid"

	"github.com/Higangssh/hyperbutler/guest/cpio"
	"github.com/Higangssh/hyperbutler/internal/engine"
	"github.com/Higangssh/hyperbutler/internal/transport"
	"github.com/Higangssh/hyperbutler/internal/vm"
)

const (
	ctlPort   = 7001
	fusePort  = 7002
	syncPort  = 7003
	plan9Port = 564
	kver      = "6.18.58-0-virt"
)

var (
	work     = flag.String("work", filepath.Join(os.TempDir(), "hb-dev", "share"), "work directory (test data, VHDX, logs)")
	imageDir = flag.String("image", filepath.Join(os.TempDir(), "hb-dev", "wbcli", "out"), "dir with vmlinuz-virt and initramfs-hyperbutler.gz")
	modDir   = flag.String("mods", filepath.Join(os.TempDir(), "hb-dev", "hyperbutler-spike", "linux-virt", "lib", "modules", kver), "Alpine linux-virt module tree (for 9p and fuse)")
	memMB    = flag.Int("mem", 4096, "guest memory in MiB")
	cpus     = flag.Int("cpus", 4, "guest vCPUs")
	targets  = flag.String("targets", "ext4,plan9,fuse", "comma-separated: ext4, plan9, fuse, fuse-nocache, sync")
	suite    = flag.String("suite", "seqwrite,seqread,untar,walk,stat,open,statmiss,smallfiles,rm", "comma-separated benchmarks")
	notifyN  = flag.Int("notify", 0, "run the change-notification test this many times per target")
	compatF  = flag.Bool("compat", false, "run the REQ-FS-004 behavior checks per target")
	files    = flag.Int("files", 20000, "files in the untar tree")
	seqMB    = flag.Int("mb", 1024, "MiB for the sequential test")
	runs     = flag.Int("runs", 1, "repeat the suite this many times")
	shell    = flag.String("sh", "", "run this shell command in the guest after mounting and print the output")
	jsonOut  = flag.String("json", "", "append results as JSON lines to this file")
	plan9Ext = flag.String("plan9-opts", "", "extra 9p mount options for the plan9 target (e.g. cache=loose)")
	p9Flags  = flag.Uint("plan9-flags", 0, "HCS Plan9 share flags")
	deadline = flag.Duration("deadline", 15*time.Minute, "exit (and so stop the VM) after this long")
	probe    = flag.String("probe", "", "create a VM with these extra Devices (JSON object) and report what HCS says")
	p9msize  = flag.String("plan9-msize", "65536", "9p msize for the plan9 target")
)

func main() {
	flag.Parse()
	go func() {
		time.Sleep(*deadline)
		fmt.Fprintln(os.Stderr, "error: -deadline reached")
		os.Exit(3)
	}()
	if *demo {
		if err := runDemo(); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}
	if *probe != "" {
		if err := probeDevices(*probe); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func has(list, item string) bool {
	for _, s := range strings.Split(list, ",") {
		if strings.TrimSpace(s) == item {
			return true
		}
	}
	return false
}

func run() error {
	if err := os.MkdirAll(*work, 0o750); err != nil {
		return err
	}
	guestBin, err := buildGuest()
	if err != nil {
		return err
	}
	initrd, err := buildInitrd(guestBin)
	if err != nil {
		return err
	}
	sddl, err := engine.CurrentUserSDDL()
	if err != nil {
		return err
	}
	id := fmt.Sprintf("hyperbutler-share-spike-%d", time.Now().UnixNano()%1e9)

	vhdx := filepath.Join(*work, "ext4.vhdx")
	_ = os.Remove(vhdx)
	if err := vm.CreateSparseVHDX(vhdx, 32<<30); err != nil {
		return fmt.Errorf("create vhdx: %w", err)
	}
	if err := vm.GrantVMAccess(id, vhdx); err != nil {
		return err
	}
	defer func() { _ = vm.RevokeVMAccess(id, vhdx) }()

	p9dir := filepath.Join(*work, "p9")
	fusedir := filepath.Join(*work, "fuse")
	for _, d := range []string{p9dir, fusedir} {
		_ = os.RemoveAll(d)
		if err := os.MkdirAll(d, 0o750); err != nil {
			return err
		}
	}
	cfg := &vm.Config{
		Owner:          "hyperbutler-share-spike",
		KernelPath:     filepath.Join(*imageDir, "vmlinuz-virt"),
		InitrdPath:     initrd,
		KernelCmdLine:  fmt.Sprintf("console=ttyS0 quiet rdinit=/sharebench sb.port=%d", ctlPort),
		MemoryMB:       *memMB,
		ProcessorCount: *cpus,
		MemoryHints:    true,
		Disks:          []vm.Disk{{Path: vhdx, Sparse: true}},
		ConsolePipe:    `\\.\pipe\` + id + "-com1",
		Services: []vm.Service{
			{ID: transport.VsockServiceID(ctlPort), BindSDDL: sddl},
			{ID: transport.VsockServiceID(fusePort), BindSDDL: sddl},
			{ID: transport.VsockServiceID(syncPort), BindSDDL: sddl},
		},
	}
	if has(*targets, "plan9") {
		if err := vm.GrantVMAccess(id, p9dir); err != nil {
			return fmt.Errorf("grant plan9 dir: %w", err)
		}
		defer func() { _ = vm.RevokeVMAccess(id, p9dir) }()
		cfg.Plan9 = []vm.Plan9Share{{Name: "p9", AccessName: "p9", Path: p9dir, Port: plan9Port, Flags: uint32(*p9Flags)}}
	}
	t0 := time.Now()
	sys, err := vm.Create(id, cfg)
	if err != nil {
		return err
	}
	defer func() {
		_ = sys.Terminate()
		sys.Close()
	}()
	if err := sys.Start(); err != nil {
		return err
	}
	go streamConsole(cfg.ConsolePipe, filepath.Join(*work, "console.log"))
	vmid, err := sys.RuntimeID()
	if err != nil {
		return err
	}
	g, err := acceptGuest(vmid, 60*time.Second)
	if err != nil {
		return err
	}
	defer g.close()
	fmt.Printf("guest connected after %v\n", time.Since(t0).Round(time.Millisecond))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Baseline: ext4 on the SCSI disk.
	if out, err := g.sh("/sbin/mke2fs -q -t ext4 -m 0 -E nodiscard,lazy_itable_init=1,lazy_journal_init=1 /dev/sda 2>&1"); err != nil {
		return fmt.Errorf("mke2fs: %w: %s", err, out)
	}
	if _, err := g.call("mount", map[string]string{"source": "/dev/sda", "target": "/mnt/ext4", "fstype": "ext4"}); err != nil {
		return fmt.Errorf("mount ext4: %w", err)
	}

	type target struct {
		name, guestDir, hostDir string
	}
	var tgts []target
	for _, t := range strings.Split(*targets, ",") {
		switch t = strings.TrimSpace(t); t {
		case "ext4":
			tgts = append(tgts, target{t, "/mnt/ext4", ""})
		case "plan9":
			_, err := g.call("mount9p", map[string]string{"port": fmt.Sprint(plan9Port), "aname": "p9", "target": "/mnt/p9", "msize": *p9msize, "opts": *plan9Ext})
			if err != nil {
				fmt.Println("plan9 mount:", err)
				continue
			}
			tgts = append(tgts, target{t, "/mnt/p9", p9dir})
		case "fuse", "fuse-nocache":
			dir, err := startFuse(ctx, g, vmid, fusedir, t == "fuse-nocache")
			if err != nil {
				fmt.Println("fuse:", err)
				continue
			}
			tgts = append(tgts, target{t, dir, fusedir})
		case "sync":
			s, err := startSync(ctx, g, vmid)
			if err != nil {
				fmt.Println("sync:", err)
				continue
			}
			tgts = append(tgts, target{t, s.guestDir, s.hostDir})
		}
	}
	if *vsockBW {
		runVsockBW(g, vmid)
		runVsockPar(g, vmid)
	}
	if *shell != "" {
		out, err := g.sh(*shell)
		fmt.Printf("$ %s\n%s(err=%v)\n", *shell, out, err)
	}

	results := map[string]map[string]float64{}
	for r := 0; r < *runs; r++ {
		for _, t := range tgts {
			res := map[string]float64{}
			dir := t.guestDir + "/bench"
			_, _ = g.sh("mkdir -p " + dir)
			for _, b := range strings.Split(*suite, ",") {
				b = strings.TrimSpace(b)
				if b == "" {
					continue
				}
				args := map[string]string{"name": b, "dir": dir, "files": fmt.Sprint(*files), "mb": fmt.Sprint(*seqMB)}
				switch b {
				case "walk":
					args["dir"] = dir + "/tree"
					for _, cold := range []bool{true, false} {
						args["drop"] = map[bool]string{true: "1", false: "0"}[cold]
						out, err := g.bench(args)
						if err != nil {
							fmt.Printf("%s %s: %v\n", t.name, b, err)
							continue
						}
						k := map[bool]string{true: "walk_cold_s", false: "walk_warm_s"}[cold]
						res[k] = out["walk_s"]
						if *traceOn && strings.HasPrefix(t.name, "fuse") {
							dumpTrace(t.name + " " + k)
						}
					}
					continue
				case "untar", "statmiss", "stat", "open":
					args["dir"] = dir + "/tree"
				case "rm":
					args["dir"] = dir + "/tree"
				}
				out, err := g.bench(args)
				if err != nil {
					fmt.Printf("%s %s: %v\n", t.name, b, err)
					continue
				}
				for k, v := range out {
					res[k] = v
				}
				if *traceOn && strings.HasPrefix(t.name, "fuse") {
					dumpTrace(t.name + " " + b)
				}
			}
			if *compatF {
				out, err := g.call("bench", map[string]string{"name": "compat", "dir": t.guestDir})
				fmt.Printf("== compat %s (err=%v)\n", t.name, err)
				if m, ok := out.(map[string]any); ok {
					var ks []string
					for k := range m {
						ks = append(ks, k)
					}
					sort.Strings(ks)
					for _, k := range ks {
						fmt.Printf("  %-34s %v\n", k, m[k])
					}
				}
				if t.hostDir != "" {
					printHostView(filepath.Join(t.hostDir, "compat"))
				}
			}
			if *notifyN > 0 && t.hostDir != "" {
				for k, v := range notifyTest(g, t.guestDir, t.hostDir, *notifyN) {
					res[k] = v
				}
			}
			results[t.name] = res
			printResult(t.name, res)
			if *jsonOut != "" {
				appendJSON(*jsonOut, t.name, res)
			}
		}
	}
	_, _ = g.call("poweroff", nil)
	time.Sleep(300 * time.Millisecond)
	return nil
}

func printResult(name string, res map[string]float64) {
	var ks []string
	for k := range res {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	fmt.Printf("== %s\n", name)
	for _, k := range ks {
		fmt.Printf("  %-22s %10.3f\n", k, res[k])
	}
}

func appendJSON(path, name string, res map[string]float64) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	b, _ := json.Marshal(map[string]any{"target": name, "time": time.Now().Format(time.RFC3339), "res": res})
	_, werr := f.Write(append(b, '\n'))
	if err := errors.Join(werr, f.Close()); err != nil {
		fmt.Println("append", path+":", err)
	}
}

func printHostView(dir string) {
	es, err := os.ReadDir(dir)
	if err != nil {
		fmt.Println("  host view:", err)
		return
	}
	var names []string
	for _, e := range es {
		names = append(names, fmt.Sprintf("%q", e.Name()))
	}
	fmt.Println("  host view:", strings.Join(names, " "))
}

// buildGuest cross-compiles spike/share/guest for linux/amd64.
func buildGuest() (string, error) {
	out := filepath.Join(*work, "sharebench")
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags=-s -w", "-o", out, "./spike/share/guest") //nolint:gosec // G204: command and arguments come from the operator's own flags or this tool's work directory; exec passes argv, no shell
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("build guest: %w", err)
	}
	return out, nil
}

// buildInitrd appends a cpio with /sharebench and the extra modules to the hyperbutler
// initramfs, which brings busybox, mke2fs and the Hyper-V modules.
func buildInitrd(guestBin string) (string, error) {
	base, err := os.ReadFile(filepath.Join(*imageDir, "initramfs-hyperbutler.gz"))
	if err != nil {
		return "", err
	}
	var extra bytes.Buffer
	zw := gzip.NewWriter(&extra)
	w := cpio.NewWriter(zw)
	add := func(name string, mode uint32, data []byte) error {
		if err := w.WriteHeader(cpio.Header{Name: name, Mode: mode, Size: uint32(len(data)), Nlink: 1}); err != nil {
			return err
		}
		_, err := w.Write(data)
		return err
	}
	bin, err := os.ReadFile(guestBin) //nolint:gosec // G304: the path comes from the operator's own flags or this tool's work directory
	if err != nil {
		return "", err
	}
	if err := add("sharebench", cpio.TypeRegular|0o755, bin); err != nil {
		return "", err
	}
	if err := add("sbmods", cpio.TypeDir|0o755, nil); err != nil {
		return "", err
	}
	lib := "/lib/modules/" + kver + "/kernel/"
	order := []string{
		lib + "net/vmw_vsock/vsock.ko", lib + "net/vmw_vsock/hv_sock.ko",
		lib + "drivers/scsi/scsi_transport_fc.ko", lib + "drivers/scsi/hv_storvsc.ko", lib + "drivers/scsi/sd_mod.ko",
		lib + "lib/crc/crc16.ko", lib + "fs/mbcache.ko", lib + "fs/jbd2/jbd2.ko", lib + "fs/ext4/ext4.ko",
		lib + "drivers/hv/hv_balloon.ko",
	}
	for _, m := range []string{"fs/netfs/netfs.ko", "net/9p/9pnet.ko", "net/9p/9pnet_fd.ko", "fs/9p/9p.ko", "fs/fuse/fuse.ko"} {
		data, err := gunzipFile(filepath.Join(*modDir, "kernel", filepath.FromSlash(m)+".gz"))
		if err != nil {
			return "", err
		}
		name := filepath.Base(m)
		if err := add("sbmods/"+name, cpio.TypeRegular|0o644, data); err != nil {
			return "", err
		}
		order = append(order, name)
	}
	if err := add("sbmods/order", cpio.TypeRegular|0o644, []byte(strings.Join(order, "\n")+"\n")); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	if err := zw.Close(); err != nil {
		return "", err
	}
	out := filepath.Join(*work, "initramfs-share")
	return out, os.WriteFile(out, append(base, extra.Bytes()...), 0o600)
}

func gunzipFile(p string) ([]byte, error) {
	f, err := os.Open(p) //nolint:gosec // G304: the path comes from the operator's own flags or this tool's work directory
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(zr)
}

func streamConsole(pipe, logPath string) {
	var c net.Conn
	var err error
	for i := 0; i < 100; i++ {
		d := 2 * time.Second
		c, err = winio.DialPipe(pipe, &d)
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		fmt.Println("console:", err)
		return
	}
	defer c.Close()
	f, err := os.Create(logPath) //nolint:gosec // G304: the path comes from the operator's own flags or this tool's work directory
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(c)
	for sc.Scan() {
		line := sc.Text()
		if _, err := f.WriteString(line + "\n"); err != nil {
			fmt.Println("  [guest log]", err)
			return
		}
		if strings.Contains(line, "sharebench") || strings.Contains(line, "panic") || strings.Contains(line, "9p") || strings.Contains(line, "fuse") || strings.Contains(line, "BUG") {
			fmt.Println("  [guest]", line)
		}
	}
}

// guest is the control connection to sharebench.
type guest struct {
	c      net.Conn
	mu     sync.Mutex
	nextID int
	wait   map[int]chan reply
	events chan event
	wmu    sync.Mutex
}

type reply struct {
	ID  int             `json:"id"`
	OK  bool            `json:"ok"`
	Err string          `json:"err"`
	Out json.RawMessage `json:"out"`
	Ev  string          `json:"ev"`
}

type event struct {
	Path string
	Mask uint32
	Seq  int64
	At   time.Time
}

func acceptGuest(vmid guid.GUID, timeout time.Duration) (*guest, error) {
	ln, err := winio.ListenHvsock(&winio.HvsockAddr{VMID: vmid, ServiceID: transport.VsockServiceID(ctlPort)})
	if err != nil {
		return nil, err
	}
	defer ln.Close()
	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		c, err := ln.Accept()
		ch <- res{c, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, r.err
		}
		g := &guest{c: r.c, wait: map[int]chan reply{}, events: make(chan event, 1<<16)}
		go g.read()
		return g, nil
	case <-time.After(timeout):
		return nil, errors.New("guest did not connect (see console.log)")
	}
}

func (g *guest) close() { g.c.Close() }

func (g *guest) read() {
	sc := bufio.NewScanner(g.c)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		now := time.Now()
		var r reply
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			continue
		}
		if r.Ev != "" {
			var e struct {
				Path string `json:"path"`
				Mask uint32 `json:"mask"`
				Seq  int64  `json:"seq"`
			}
			if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
				continue
			}
			select {
			case g.events <- event{e.Path, e.Mask, e.Seq, now}:
			default:
			}
			continue
		}
		g.mu.Lock()
		ch := g.wait[r.ID]
		delete(g.wait, r.ID)
		g.mu.Unlock()
		if ch != nil {
			ch <- r
		}
	}
}

func (g *guest) call(op string, args map[string]string) (any, error) {
	g.mu.Lock()
	g.nextID++
	id := g.nextID
	ch := make(chan reply, 1)
	g.wait[id] = ch
	g.mu.Unlock()
	b, _ := json.Marshal(map[string]any{"id": id, "op": op, "args": args})
	g.wmu.Lock()
	_, err := g.c.Write(append(b, '\n'))
	g.wmu.Unlock()
	if err != nil {
		return nil, err
	}
	select {
	case r := <-ch:
		var out any
		if len(r.Out) > 0 {
			if err := json.Unmarshal(r.Out, &out); err != nil {
				return nil, fmt.Errorf("%s: reply: %w", op, err)
			}
		}
		if !r.OK {
			return out, errors.New(r.Err)
		}
		return out, nil
	case <-time.After(30 * time.Minute):
		return nil, errors.New("timeout")
	}
}

func (g *guest) sh(cmd string) (string, error) {
	out, err := g.call("sh", map[string]string{"cmd": cmd})
	s, _ := out.(string)
	return s, err
}

func (g *guest) bench(args map[string]string) (map[string]float64, error) {
	out, err := g.call("bench", args)
	if err != nil {
		return nil, err
	}
	m := map[string]float64{}
	if mm, ok := out.(map[string]any); ok {
		for k, v := range mm {
			if f, ok := v.(float64); ok {
				m[k] = f
			}
		}
	}
	return m, nil
}
