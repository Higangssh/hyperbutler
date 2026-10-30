//go:build linux

package relay

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

// Config describes one relay.
type Config struct {
	// Mountpoint is where the FUSE file system is mounted (for example /mnt/hyperbutler).
	Mountpoint string
	// Conns are connections to the host's share server, one per queue. Conns[0] opens
	// the session; hvsock bandwidth grows with each extra connection (ADR 0007). They
	// must be blocking descriptors whose Close interrupts a blocked Read (see Relay).
	Conns []io.ReadWriteCloser
	// ReadAheadKB sets the mount's readahead so several 1 MiB reads are in flight
	// [4096]; the kernel default (128 KiB) keeps one.
	ReadAheadKB int
	// Logf receives errors [discarded].
	Logf func(format string, args ...any)
}

// Relay is a mounted share file system.
//
// Every wait in the relay is a blocking system call on its own goroutine: read(2) of
// /dev/fuse (the kernel wakes exactly one reader per request) and read(2)/write(2) of
// the host connections, which must be blocking descriptors too (Config.Conns). Mixing
// them with poller-driven connections is what made sequential writes slow: the thread
// that waited in epoll ran the goroutine it woke, that goroutine then blocked in a read
// of /dev/fuse, nothing waited in epoll any more, and the reply to the request just
// forwarded was noticed only by the Go runtime's 10 ms background poll. Every request
// whose frame did not fit the hvsock ring in one write (a WRITE of 64 KiB or more) paid
// about 15 ms, so sequential writes ran at 4-56 MB/s (ADR 0007, 2026-10-31).
type Relay struct {
	cfg  Config
	fd   int           // the mounted /dev/fuse descriptor
	conn atomic.Uint64 // the FUSE connection number (the mount's device), for abort
	done chan struct{}
	once sync.Once
	err  error

	// wg counts the goroutines that use fd; it is closed only after they end.
	wg        sync.WaitGroup
	closeOnce sync.Once

	qmu   sync.Mutex
	qcond *sync.Cond
	queue [][]byte
}

// maxQueue bounds queued notifications; past it the oldest invalidations are dropped
// (the guest may then serve stale data until its cache times out).
const maxQueue = 1 << 20

// fuseBuf is the /dev/fuse read buffer: a maximal WRITE request plus headers.
const fuseBuf = 1<<20 + 8192

// fusectl is where the FUSE control file system lists connections.
const fusectl = "/sys/fs/fuse/connections"

// Start opens /dev/fuse, introduces the injector thread to the host and mounts.
func Start(cfg Config) (*Relay, error) {
	if len(cfg.Conns) == 0 || len(cfg.Conns) > 8 {
		return nil, errors.New("relay: need 1 to 8 connections")
	}
	if cfg.ReadAheadKB == 0 {
		cfg.ReadAheadKB = 4096
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if err := os.MkdirAll(cfg.Mountpoint, 0o755); err != nil {
		return nil, err
	}
	fd, err := unix.Open("/dev/fuse", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("relay: open /dev/fuse: %w", err)
	}
	r := &Relay{cfg: cfg, fd: fd, done: make(chan struct{})}
	r.qcond = sync.NewCond(&r.qmu)
	tidc := make(chan int, 1)
	r.wg.Add(1)
	go r.injector(tidc)
	tid := uint32(<-tidc)
	// Queue 0 first: it opens the session the others join.
	for i, c := range cfg.Conns {
		if _, err := c.Write(helloFrame(tid, uint32(i))); err != nil {
			r.fail(err)
			r.closeFD()
			return nil, err
		}
	}
	// nosuid and nodev: a host file must not become a setuid binary or a device in the
	// guest. default_permissions makes the guest kernel check modes; allow_other lets
	// every container user in.
	data := fmt.Sprintf("fd=%d,rootmode=40000,user_id=0,group_id=0,default_permissions,allow_other,max_read=%d", fd, 1<<20)
	if err := unix.Mount("hyperbutler", cfg.Mountpoint, "fuse.hyperbutler", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOATIME, data); err != nil {
		r.fail(err)
		r.closeFD()
		return nil, fmt.Errorf("relay: mount %s: %w", cfg.Mountpoint, err)
	}
	for _, c := range cfg.Conns {
		r.wg.Add(2)
		go r.fromKernel(c)
		go r.fromHost(c)
	}
	// Only now: a stat of the mount needs the readers to answer it.
	var st unix.Stat_t
	if err := unix.Stat(cfg.Mountpoint, &st); err == nil {
		// fusectl names a connection by the kernel's dev_t (major<<20 | minor), not
		// by the encoding stat(2) returns.
		r.conn.Store(uint64(unix.Major(st.Dev))<<20 | uint64(unix.Minor(st.Dev)))
	}
	r.setReadAhead(st)
	return r, nil
}

// setReadAhead raises the readahead of the mount's backing device info.
func (r *Relay) setReadAhead(st unix.Stat_t) {
	if st.Dev == 0 {
		return
	}
	p := fmt.Sprintf("/sys/class/bdi/%d:%d/read_ahead_kb", unix.Major(st.Dev), unix.Minor(st.Dev))
	if err := os.WriteFile(p, []byte(fmt.Sprint(r.cfg.ReadAheadKB)), 0); err != nil {
		r.cfg.Logf("relay: readahead: %v", err)
	}
}

func (r *Relay) fail(err error) {
	r.once.Do(func() {
		r.err = err
		close(r.done)
		r.stopQueue()
	})
}

// Wait blocks until the relay stops and returns why.
func (r *Relay) Wait() error {
	<-r.done
	return r.err
}

// closeWait bounds how long Close waits for the relay's goroutines before it gives up
// and leaves the descriptor open (closing it under a blocked reader could let a later
// open reuse the number).
const closeWait = 5 * time.Second

// Close unmounts, aborts the FUSE connection (so readers blocked in /dev/fuse, and guest
// processes waiting for a reply, return at once instead of when the last container
// lets go of the mount) and stops the relay.
func (r *Relay) Close() error {
	unix.Unmount(r.cfg.Mountpoint, unix.MNT_DETACH)
	if err := r.abortConn(); err != nil {
		r.cfg.Logf("relay: abort: %v", err)
	}
	for _, c := range r.cfg.Conns {
		c.Close()
	}
	r.fail(errors.New("relay: closed"))
	ended := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(ended)
	}()
	select {
	case <-ended:
		r.closeFD()
	case <-time.After(closeWait):
		r.cfg.Logf("relay: readers did not stop; leaving /dev/fuse open")
	}
	return nil
}

func (r *Relay) closeFD() {
	r.closeOnce.Do(func() { unix.Close(r.fd) })
}

// abortConn aborts the connection through the FUSE control file system, mounting it
// if nothing has.
func (r *Relay) abortConn() error {
	conn := r.conn.Load()
	if conn == 0 {
		return errors.New("connection unknown")
	}
	p := fmt.Sprintf("%s/%d/abort", fusectl, conn)
	err := os.WriteFile(p, []byte("1"), 0)
	if errors.Is(err, os.ErrNotExist) {
		if merr := unix.Mount("fusectl", fusectl, "fusectl", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); merr == nil || errors.Is(merr, unix.EBUSY) {
			err = os.WriteFile(p, []byte("1"), 0)
		}
	}
	if errors.Is(err, os.ErrNotExist) {
		// The connection is already gone: nothing waits on it.
		return nil
	}
	return err
}

// fromKernel copies requests to the host on one connection. Every queue reads the same
// /dev/fuse descriptor; each request goes to whichever reader takes it.
func (r *Relay) fromKernel(conn io.Writer) {
	defer r.wg.Done()
	buf := make([]byte, fuseBuf)
	for {
		n, err := unix.Read(r.fd, buf)
		switch {
		case err == unix.EINTR || err == unix.EAGAIN || err == unix.ENOENT:
			continue
		case err == unix.ENODEV, err == unix.ECONNABORTED:
			r.fail(nil) // unmounted or aborted
			return
		case err != nil:
			r.fail(fmt.Errorf("relay: read /dev/fuse: %w", err))
			return
		}
		if _, err = conn.Write(buf[:n]); err != nil {
			r.fail(fmt.Errorf("relay: write to host: %w", err))
			return
		}
	}
}

// fromHost copies replies to the kernel and queues notifications.
func (r *Relay) fromHost(conn io.Reader) {
	defer r.wg.Done()
	buf := make([]byte, maxHostFrame)
	for {
		f, err := readFrame(conn, buf)
		if err != nil {
			r.fail(fmt.Errorf("relay: read from host: %w", err))
			return
		}
		if binary.LittleEndian.Uint64(f[8:]) != 0 {
			// A reply. ENOENT means the request was interrupted and is gone.
			if _, err := unix.Write(r.fd, f); err != nil && err != unix.ENOENT {
				r.cfg.Logf("relay: reply: %v", err)
			}
			continue
		}
		c := make([]byte, len(f))
		copy(c, f)
		r.qmu.Lock()
		if len(r.queue) >= maxQueue {
			r.queue = r.queue[1:]
		}
		r.queue = append(r.queue, c)
		r.qcond.Signal()
		r.qmu.Unlock()
	}
}

func (r *Relay) stopQueue() {
	r.qmu.Lock()
	r.queue = append(r.queue, nil)
	r.qcond.Broadcast()
	r.qmu.Unlock()
}

// injector owns one OS thread for its whole life: the host recognizes replayed
// operations by this thread's id.
func (r *Relay) injector(tid chan<- int) {
	defer r.wg.Done()
	runtime.LockOSThread()
	tid <- unix.Gettid()
	for {
		r.qmu.Lock()
		for len(r.queue) == 0 {
			r.qcond.Wait()
		}
		f := r.queue[0]
		r.queue = r.queue[1:]
		r.qmu.Unlock()
		if f == nil {
			return
		}
		code := int32(binary.LittleEndian.Uint32(f[4:]))
		if code == notifyInject {
			// ENOENT is routine: the change is to a name the guest no longer has (or
			// a share whose root was deleted), and a log line per file of a removed
			// tree would flood the console.
			if err := r.replay(f[outHeaderLen:]); err != nil && !errors.Is(err, unix.ENOENT) {
				r.cfg.Logf("relay: replay: %v", err)
			}
			continue
		}
		// Invalidations. ENOENT: the guest has nothing cached for it.
		if _, err := unix.Write(r.fd, f); err != nil && err != unix.ENOENT {
			r.cfg.Logf("relay: notify %d: %v", code, err)
		}
	}
}
