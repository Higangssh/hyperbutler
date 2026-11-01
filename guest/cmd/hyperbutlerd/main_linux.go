//go:build linux

// Command hyperbutlerd is the hyperbutler guest services daemon. It serves the guestctl control
// channel (internal/guestctl) on AF_VSOCK port guestctl.ControlPort, accepting the host
// (CID 2) only, and runs three policies:
//
//   - the free-space guard (REQ-DISK-004): an fallocate'd reservation sized from the
//     host's budget (guest/internal/fsguard), failing closed without a budget;
//   - fstrim (REQ-DISK-001): periodically, and soon after large deletes, detected as a
//     jump in statfs free space (guest/internal/policy.Trim);
//   - page-cache reclaim (REQ-MEM-002): when the guest is idle and holds more file cache
//     than it should keep, reclaim it through cgroup v2 memory.reclaim (or drop_caches),
//     so free page reporting can give the memory back to the host
//     (guest/internal/policy.Cache).
//
// With -daemon-config it installs the host's daemon.json there (REQ-UX-007) and restarts
// dockerd on request, through the init (see dockerdConfig). With -dockerd-env it also
// installs dockerd's proxy environment and the host's extra CA certificates
// (REQ-UX-009, see dockerdEnv).
//
// With -share-port it also keeps the host's bind-mount file system (ADR 0007) mounted
// at -share-mount: a FUSE relay over AF_VSOCK to the host's share server, redialled with
// a bounded backoff while the host is not listening. SIGTERM unmounts it and exits.
//
// See docs/research/guest-services.md. Cross-compile with GOOS=linux CGO_ENABLED=0.
//
//	hyperbutlerd -mount /mnt -dev /dev/sda -file /mnt/.hyperbutler-reserve
package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"flag"
	"log"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Higangssh/hyperbutler/guest/internal/fsguard"
	"github.com/Higangssh/hyperbutler/guest/internal/policy"
	"github.com/Higangssh/hyperbutler/internal/guestctl"
	"github.com/Higangssh/hyperbutler/internal/spaceguard"
)

type options struct {
	port                   uint
	shutdownPID            int
	mount, dev, file       string
	interval, stale        time.Duration
	lostGrace              time.Duration
	hyst, quantum          uint64
	guardTrimEvery         time.Duration
	trim                   policy.Trim
	trimOff                bool
	cache                  policy.Cache
	cacheOff               bool
	cacheMethod            string
	cacheEvery             time.Duration
	compact                bool
	ready                  string
	statusEvery            time.Duration
	cgroupRoot, dropCaches string
	sharePort              uint
	shareMount             string
	shareQueues            int
	daemonConfig           string
	dockerdBin             string
	dockerdEnv             string
	caBundle, caStock      string
}

func main() {
	var o options
	o.trim, o.cache = policy.DefaultTrim, policy.DefaultCache
	flag.UintVar(&o.port, "port", guestctl.ControlPort, "AF_VSOCK port for the control channel")
	flag.StringVar(&o.mount, "mount", "", "mount point of the data file system (trim, guard)")
	flag.StringVar(&o.dev, "dev", "", "block device of the data file system; with -file, enables the guard")
	flag.StringVar(&o.file, "file", "", "guard reservation file on the data file system")
	flag.DurationVar(&o.interval, "interval", 100*time.Millisecond, "guard measurement interval")
	flag.DurationVar(&o.stale, "stale", 30*time.Second, "guard: fail closed after this many intervals' worth of ticks without a budget (0 = never)")
	flag.DurationVar(&o.lostGrace, "lost-grace", time.Second, "guard: keep applying the last budget this long after the control connection drops, so a quick reconnect does not swing the reservation (0 = fail closed at once)")
	flag.Uint64Var(&o.hyst, "hysteresis", spaceguard.DefaultConfig.Hysteresis, "guard reservation hysteresis in bytes")
	flag.Uint64Var(&o.quantum, "quantum", spaceguard.DefaultConfig.Quantum, "guard reservation granularity in bytes")
	flag.DurationVar(&o.guardTrimEvery, "guard-trim-every", 10*time.Second, "guard: trim before growing the reservation at most this often (0 = never)")
	flag.BoolVar(&o.trimOff, "no-trim", false, "disable the fstrim policy")
	flag.DurationVar(&o.trim.Period, "trim-period", o.trim.Period, "fstrim at least this often")
	flag.Uint64Var(&o.trim.DeleteBytes, "trim-delete", o.trim.DeleteBytes, "free-space rise in bytes that triggers a trim")
	flag.DurationVar(&o.trim.Settle, "trim-settle", o.trim.Settle, "after a delete, trim once free space is stable this long")
	flag.DurationVar(&o.trim.MaxDelay, "trim-max-delay", o.trim.MaxDelay, "after a delete, trim at the latest after this long")
	flag.BoolVar(&o.cacheOff, "no-cache", false, "disable the page-cache policy")
	flag.Uint64Var(&o.cache.Keep, "cache-keep", o.cache.Keep, "file cache bytes to keep")
	flag.Uint64Var(&o.cache.Threshold, "cache-threshold", o.cache.Threshold, "reclaim when file cache exceeds keep by this many bytes")
	flag.DurationVar(&o.cache.IdleFor, "cache-idle", o.cache.IdleFor, "how long the guest must be idle before a reclaim")
	flag.Float64Var(&o.cache.IdleCPU, "cache-idle-cpu", o.cache.IdleCPU, "busy fraction below which the guest counts as idle")
	flag.DurationVar(&o.cache.MinInterval, "cache-min-interval", o.cache.MinInterval, "shortest time between reclaims")
	flag.StringVar(&o.cacheMethod, "cache-method", "reclaim", `"reclaim" (cgroup v2 memory.reclaim, falls back to drop) or "drop" (vm.drop_caches=1)`)
	flag.DurationVar(&o.cacheEvery, "cache-every", 2*time.Second, "page-cache policy sampling period")
	flag.BoolVar(&o.compact, "compact", true, "write vm.compact_memory after a reclaim, so freed pages form the 2 MiB blocks free page reporting hands back")
	flag.StringVar(&o.ready, "ready", "", "file to create once the first host budget has been applied (or at start without a guard)")
	flag.DurationVar(&o.statusEvery, "status-every", time.Second, "status report period on the control channel")
	flag.StringVar(&o.cgroupRoot, "cgroup", "/sys/fs/cgroup", "cgroup v2 root")
	flag.StringVar(&o.dropCaches, "drop-caches", "/proc/sys/vm/drop_caches", "drop_caches path")
	flag.UintVar(&o.sharePort, "share-port", 0, "AF_VSOCK port of the host's bind-mount share server (0 = no shares)")
	flag.StringVar(&o.shareMount, "share-mount", "/mnt/hyperbutler", "where the host's shares are mounted")
	flag.IntVar(&o.shareQueues, "share-queues", 4, "connections to the share server (1 to 8)")
	flag.StringVar(&o.daemonConfig, "daemon-config", "", "install the host's daemon.json at this path and restart dockerd on request (the init must support it; empty = off)")
	flag.StringVar(&o.dockerdBin, "dockerd", "/usr/bin/dockerd", "dockerd, for --validate")
	flag.StringVar(&o.dockerdEnv, "dockerd-env", "", "with -daemon-config: install the host's dockerd environment (proxies, CA certificates) as KEY=VALUE lines at this path (the init must support it; empty = off)")
	flag.StringVar(&o.caBundle, "ca-bundle", "/run/hyperbutler/ca-certificates.crt", "with -dockerd-env: where the combined CA bundle goes")
	flag.StringVar(&o.caStock, "ca-stock", "/etc/ssl/certs/ca-certificates.crt", "with -dockerd-env: the image's stock CA bundle")
	flag.IntVar(&o.shutdownPID, "shutdown-pid", 1, "on a host shutdown request, send SIGTERM to this process (the init, which stops dockerd and powers off); 0 disables the feature")
	flag.Parse()
	if flag.NArg() != 0 || o.port > math.MaxUint32-1 || (o.file != "") != (o.dev != "") ||
		((o.file != "" || !o.trimOff) && o.mount == "") || o.interval <= 0 ||
		(o.cacheMethod != "reclaim" && o.cacheMethod != "drop") ||
		o.sharePort > math.MaxUint32-1 || o.shareQueues < 1 || o.shareQueues > 8 || o.shareMount == "" || o.shareMount[0] != '/' ||
		(o.daemonConfig != "" && (o.daemonConfig[0] != '/' || o.shutdownPID <= 0)) ||
		(o.dockerdEnv != "" && (o.daemonConfig == "" || !absPath(o.dockerdEnv) || !absPath(o.caBundle) || !absPath(o.caStock))) {
		flag.Usage()
		os.Exit(2)
	}
	log.SetPrefix("hyperbutlerd: ")
	log.SetFlags(0)
	d, err := newDaemon(o)
	if err != nil {
		log.Fatal(err)
	}
	var shares *shareRelay
	if o.sharePort != 0 {
		// hv_sock and fuse are loaded by the init before hyperbutlerd starts.
		shares = newShareRelay(uint32(o.sharePort), o.shareMount, o.shareQueues)
		go shares.run()
	}
	// SIGTERM (the init's shutdown, after dockerd has stopped the containers): unmount
	// the shares cleanly, then exit.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, unix.SIGTERM, unix.SIGINT)
	go func() {
		s := <-sig
		log.Printf("%v: stopping", s)
		if shares != nil {
			shares.shutdown()
		}
		os.Exit(0)
	}()
	log.Fatal(d.run())
}

type daemon struct {
	o        options
	features guestctl.Features
	trimmer  *fsguard.FSTrimmer
	guard    *fsguard.Guard
	box      *mailbox // budget and lost events for the guard loop
	dcfg     *dockerdConfig

	mu         sync.Mutex
	snap       fsguard.Result
	steps      uint64
	trimPol    policy.Trim
	cachePol   policy.Cache
	cacheTrims uint64
	cacheFreed uint64

	cacheMu sync.Mutex // one reclaim at a time
}

func newDaemon(o options) (*daemon, error) {
	d := &daemon{o: o, box: newMailbox(), trimPol: o.trim, cachePol: o.cache}
	if o.shutdownPID > 0 {
		d.features |= guestctl.FeatureShutdown
	}
	if o.mount != "" {
		f, err := os.Open(o.mount)
		if err != nil {
			return nil, err
		}
		d.trimmer = &fsguard.FSTrimmer{F: f, OnTrim: d.recordTrim}
		if !o.trimOff {
			d.features |= guestctl.FeatureTrim
		}
	}
	if o.file != "" {
		// Only Hysteresis and Quantum matter in the guest; Margin and Floor are
		// host-side and only need to pass Validate.
		cfg := spaceguard.Config{Margin: 2, Floor: 1, Hysteresis: o.hyst, Quantum: o.quantum}
		g, err := fsguard.Open(o.file, o.dev, cfg)
		if err != nil {
			return nil, err
		}
		g.TrimEvery, g.Trim = o.guardTrimEvery, d.trimmer
		if o.stale > 0 {
			g.StaleTicks = max(1, int(o.stale/o.interval))
		}
		g.LostGrace = int(o.lostGrace / o.interval)
		d.guard = g
		d.features |= guestctl.FeatureGuard
	}
	if !o.cacheOff {
		d.features |= guestctl.FeatureCacheTrim
	}
	if o.daemonConfig != "" {
		dir := filepath.Dir(o.daemonConfig)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		pid := o.shutdownPID
		bin := o.dockerdBin
		d.dcfg = &dockerdConfig{
			path:  o.daemonConfig,
			ready: filepath.Join(dir, "daemon.ready"),
			gen:   filepath.Join(dir, "dockerd.gen"),
			hold:  filepath.Join(dir, "dockerd.hold"),
			sock:  "/run/docker.sock",
			validate: func(ctx context.Context, path string) (string, error) {
				out, err := exec.CommandContext(ctx, bin, "--validate", "--config-file="+path).CombinedOutput()
				if err != nil {
					return lastLine(out), err
				}
				return "", nil
			},
			restart:   func() error { return unix.Kill(pid, unix.SIGHUP) },
			alive:     func(p int) bool { return unix.Kill(p, 0) == nil },
			upTimeout: 90 * time.Second,
			pollEvery: 200 * time.Millisecond,
		}
		// A hold left by an earlier hyperbutlerd that died mid-apply would keep the init
		// waiting for a restart nobody asks for.
		_ = os.Remove(d.dcfg.hold)
		d.features |= guestctl.FeatureDaemonConfig
		if o.dockerdEnv != "" {
			d.dcfg.denv = &dockerdEnv{env: o.dockerdEnv, bundle: o.caBundle, stock: o.caStock}
			d.features |= guestctl.FeatureDockerdEnv
		}
	}
	return d, nil
}

func (d *daemon) run() error {
	ln, err := listenVsock(uint32(d.o.port))
	if err != nil {
		return err
	}
	log.Printf("listening on vsock port %d, features 0x%x", d.o.port, uint32(d.features))
	if d.guard != nil {
		go d.guardLoop()
	} else if d.o.ready != "" {
		_ = os.WriteFile(d.o.ready, nil, 0o644)
	}
	if d.features&guestctl.FeatureTrim != 0 {
		go d.trimLoop()
	}
	if d.features&guestctl.FeatureCacheTrim != 0 {
		go d.cacheLoop()
	}
	var inst [8]byte
	_, _ = rand.Read(inst[:])
	srv := &guestctl.Server{
		Features: d.features, Instance: binary.BigEndian.Uint64(inst[:]),
		StatusEvery: d.o.statusEvery, Logf: log.Printf,
	}
	var cancelPrev context.CancelFunc
	var backoff time.Duration
	for {
		c, cid, err := ln.accept()
		if err != nil {
			if errors.Is(err, os.ErrClosed) {
				return err
			}
			backoff = min(max(2*backoff, 5*time.Millisecond), time.Second)
			log.Printf("accept: %v; retrying in %v", err, backoff)
			time.Sleep(backoff)
			continue
		}
		backoff = 0
		if cid != unix.VMADDR_CID_HOST {
			log.Printf("refused control connection from CID %d", cid)
			_ = c.Close()
			continue
		}
		// One host connection at a time: a new one means the host reconnected, so the
		// old one is dead or stale.
		if cancelPrev != nil {
			cancelPrev()
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancelPrev = cancel
		gen := d.box.connect()
		s := *srv
		s.Handler = &connHandler{d: d, gen: gen}
		go func() {
			err := s.ServeConn(ctx, c)
			if err != nil && ctx.Err() == nil {
				log.Printf("control connection: %v", err)
			}
		}()
	}
}

func (d *daemon) guardLoop() {
	g := d.guard
	tick := time.NewTicker(d.o.interval)
	defer tick.Stop()
	readyDone := d.o.ready == ""
	for {
		select {
		case <-d.box.notify:
			lost, b := d.box.take()
			if lost {
				log.Printf("control channel lost; guard fails closed in %v unless the host reconnects", d.o.lostGrace)
				g.Lost()
			}
			if b != nil {
				g.SetBudget(*b)
			}
		case <-tick.C:
			g.Tick()
		}
		g.Step()
		d.mu.Lock()
		d.snap, d.steps = g.Last, g.Steps
		d.mu.Unlock()
		if !readyDone && g.Have() {
			if err := os.WriteFile(d.o.ready, nil, 0o644); err != nil {
				log.Printf("ready file: %v", err)
			}
			readyDone = true
		}
	}
}

func (d *daemon) trimLoop() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for range t.C {
		free, err := fsguard.Free(d.trimmer.F)
		if err != nil {
			continue
		}
		d.mu.Lock()
		r := d.trimPol.Observe(time.Now(), free)
		d.mu.Unlock()
		if r == policy.NoTrim {
			continue
		}
		n, dur, err := d.trimmer.Trim()
		log.Printf("fstrim (%v): %d MiB in %v, err=%v", r, n>>20, dur.Round(time.Millisecond), err)
	}
}

// recordTrim resets the trim policy after any trim (FSTrimmer.OnTrim), whoever asked
// for it: the policy, the guard, or the host.
func (d *daemon) recordTrim() {
	free, err := fsguard.Free(d.trimmer.F)
	if err != nil {
		return
	}
	d.mu.Lock()
	d.trimPol.Trimmed(time.Now(), free)
	d.mu.Unlock()
}

func readFile(p string) []byte {
	f, err := os.Open(p)
	if err != nil {
		return nil
	}
	defer f.Close()
	b := make([]byte, policy.MaxProcFile)
	n, _ := f.Read(b) // /proc files are read in one call
	return b[:n]
}

func meminfo() (policy.Meminfo, error) { return policy.ParseMeminfo(readFile("/proc/meminfo")) }

func (d *daemon) cacheLoop() {
	t := time.NewTicker(d.o.cacheEvery)
	defer t.Stop()
	for range t.C {
		mi, err := meminfo()
		if err != nil {
			continue
		}
		busy, total, err := policy.ParseStatCPU(readFile("/proc/stat"))
		if err != nil {
			continue
		}
		io, err := policy.ParseVmstatPageIO(readFile("/proc/vmstat"))
		if err != nil {
			continue
		}
		now := time.Now()
		d.mu.Lock()
		n := d.cachePol.Observe(policy.Sample{T: now, CPUBusy: busy, CPUTotal: total, PageIO: io, FileCache: mi.FileCache()})
		d.mu.Unlock()
		if n == 0 {
			continue
		}
		r := d.reclaim(n)
		log.Printf("cache reclaim (idle): method=%d %d -> %d MiB in %v, errno=%d", r.Method, r.Before>>20, r.After>>20, r.Duration.Round(time.Millisecond), r.Errno)
	}
}

// reclaim frees up to n bytes of file cache.
func (d *daemon) reclaim(n uint64) guestctl.CacheResult {
	d.cacheMu.Lock()
	defer d.cacheMu.Unlock()
	before, _ := meminfo()
	t0 := time.Now()
	method := guestctl.CacheDrop
	var err error
	if d.o.cacheMethod == "reclaim" {
		method = guestctl.CacheReclaim
		err = d.memoryReclaim(n)
		if err != nil && !errors.Is(err, unix.EAGAIN) {
			// No cgroup v2 or no memory.reclaim: fall back.
			log.Printf("memory.reclaim: %v; using drop_caches", err)
			method, err = guestctl.CacheDrop, nil
		}
	}
	if method == guestctl.CacheDrop {
		err = os.WriteFile(d.o.dropCaches, []byte("1"), 0)
	}
	if d.o.compact {
		_ = os.WriteFile("/proc/sys/vm/compact_memory", []byte("1"), 0)
	}
	dur := time.Since(t0)
	after, _ := meminfo()
	res := guestctl.CacheResult{Method: method, Before: before.FileCache(), After: after.FileCache(), Duration: dur}
	// EAGAIN from memory.reclaim means "reclaimed less than asked", which is expected
	// when part of the cache is hot or dirty.
	if err != nil && !errors.Is(err, unix.EAGAIN) {
		res.Errno = errnoOf(err)
	}
	d.mu.Lock()
	d.cachePol.Reclaimed(time.Now())
	d.cacheTrims++
	if res.Before > res.After {
		d.cacheFreed += res.Before - res.After
	}
	d.mu.Unlock()
	return res
}

func (d *daemon) memoryReclaim(n uint64) error {
	p := d.o.cgroupRoot + "/memory.reclaim"
	// swappiness=0 (Linux 6.13+) keeps the reclaim to file pages; older kernels reject
	// the key with EINVAL, so retry without it.
	err := os.WriteFile(p, []byte(strconv.FormatUint(n, 10)+" swappiness=0"), 0)
	if errors.Is(err, unix.EINVAL) {
		err = os.WriteFile(p, []byte(strconv.FormatUint(n, 10)), 0)
	}
	return err
}

func errnoOf(err error) uint32 {
	var e unix.Errno
	if errors.As(err, &e) && uint32(e) <= guestctl.MaxErrno {
		return uint32(e)
	}
	return uint32(unix.EIO)
}

func (d *daemon) status() guestctl.Status {
	d.mu.Lock()
	snap, steps := d.snap, d.steps
	cacheTrims, cacheFreed := d.cacheTrims, d.cacheFreed
	d.mu.Unlock()
	var s guestctl.Status
	if d.guard != nil {
		switch snap.State {
		case fsguard.OK:
			s.Guard = guestctl.GuardOK
		case fsguard.Stale:
			s.Guard = guestctl.GuardStale
		default:
			s.Guard = guestctl.GuardNoBudget
		}
		if snap.Err != nil && !errors.Is(snap.Err, unix.ENOSPC) {
			s.GuardErrno = errnoOf(snap.Err)
		} else if snap.Err != nil {
			s.GuardErrno = uint32(unix.ENOSPC)
		}
		m, r := snap.Measured, snap.Reservation
		s.Device, s.FSSize, s.FSFree, s.Reserved = m.Device, m.Size, m.Free, m.Reserved
		s.Target, s.Need, s.Short = r.Target, r.Need, r.Short
		s.GuardSteps = steps
	} else if d.trimmer != nil {
		var fs unix.Statfs_t
		if unix.Fstatfs(int(d.trimmer.F.Fd()), &fs) == nil {
			bs := uint64(fs.Frsize)
			s.FSSize, s.FSFree = fs.Blocks*bs, fs.Bfree*bs
		}
	}
	s.LastTrimAge = math.MaxUint64
	if d.trimmer != nil {
		var last time.Time
		s.Trims, s.Trimmed, last = d.trimmer.Stats()
		if !last.IsZero() {
			s.LastTrimAge = uint64(time.Since(last).Milliseconds())
		}
	}
	if mi, err := meminfo(); err == nil {
		s.MemTotal, s.MemFree, s.FileCache, s.Shmem = mi.MemTotal, mi.MemFree, mi.FileCache(), mi.Shmem
	}
	s.CacheTrims, s.CacheFreed = cacheTrims, cacheFreed
	return s
}

// connHandler is the guestctl.Handler for one host connection.
type connHandler struct {
	d   *daemon
	gen uint64
}

func (h *connHandler) SetBudget(b spaceguard.Budget) { h.d.box.postBudget(h.gen, b) }
func (h *connHandler) ConnLost()                     { h.d.box.postLost(h.gen) }
func (h *connHandler) Status() guestctl.Status       { return h.d.status() }

// Shutdown starts an orderly power-off: the init traps SIGTERM, stops dockerd (which
// stops the containers), unmounts the data disk and powers the VM off.
func (h *connHandler) Shutdown() error {
	log.Printf("host requested shutdown; signalling pid %d", h.d.o.shutdownPID)
	return unix.Kill(h.d.o.shutdownPID, unix.SIGTERM)
}

// ApplyDaemonConfig installs the host's daemon.json and, if asked, restarts dockerd.
func (h *connHandler) ApplyDaemonConfig(ctx context.Context, data []byte, restart bool) guestctl.DaemonResult {
	return h.d.dcfg.apply(ctx, data, restart)
}

func absPath(p string) bool { return p != "" && p[0] == '/' }

// ApplyDockerdEnv installs the host's dockerd environment and restarts dockerd when it
// changed.
func (h *connHandler) ApplyDockerdEnv(ctx context.Context, data []byte, restart bool) guestctl.EnvResult {
	return h.d.dcfg.applyEnv(ctx, data, restart)
}

func (h *connHandler) Trim(context.Context) guestctl.TrimResult {
	n, dur, err := h.d.trimmer.Trim()
	log.Printf("fstrim (host request): %d MiB in %v, err=%v", n>>20, dur.Round(time.Millisecond), err)
	r := guestctl.TrimResult{Trimmed: n, Duration: dur}
	if err != nil {
		r.Errno = errnoOf(err)
	}
	return r
}

func (h *connHandler) CacheTrim(context.Context) guestctl.CacheResult {
	mi, err := meminfo()
	if err != nil {
		return guestctl.CacheResult{Errno: uint32(unix.EIO)}
	}
	fc := mi.FileCache()
	if fc <= h.d.o.cache.Keep {
		return guestctl.CacheResult{Method: guestctl.CacheNone, Before: fc, After: fc}
	}
	r := h.d.reclaim(fc - h.d.o.cache.Keep)
	log.Printf("cache reclaim (host request): method=%d %d -> %d MiB in %v", r.Method, r.Before>>20, r.After>>20, r.Duration.Round(time.Millisecond))
	return r
}
