// Command sharebench is the guest half of the bind-mount spike (docs/research/bind-mounts.md).
//
// It runs as PID 1 (rdinit=/sharebench) in a throwaway HCS VM. It mounts the API file
// systems, loads the kernel modules listed in /sbmods/order, connects back to the host on
// the vsock port given as sb.port=N, and then executes JSON commands from the host: mount
// file systems, run benchmarks, watch directories with inotify, run shell commands.
//
// Throwaway code. The protocol is newline-delimited JSON between two programs built from
// the same commit; it is not an interface.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const hostCID = 2

type request struct {
	ID   int               `json:"id"`
	Op   string            `json:"op"`
	Args map[string]string `json:"args"`
}

type reply struct {
	ID  int    `json:"id"`
	OK  bool   `json:"ok"`
	Err string `json:"err,omitempty"`
	Out any    `json:"out,omitempty"`
}

// event is an asynchronous message (inotify events).
type event struct {
	Ev   string `json:"ev"`
	Path string `json:"path"`
	Mask uint32 `json:"mask"`
	Seq  int64  `json:"seq"`
}

var (
	outMu sync.Mutex
	out   *bufio.Writer
	conn  *os.File
)

func send(v any) {
	b, _ := json.Marshal(v)
	outMu.Lock()
	out.Write(b)
	out.WriteByte('\n')
	out.Flush()
	outMu.Unlock()
}

func logf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "sharebench: "+format+"\n", a...)
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "relay-daemon" {
		relayDaemon()
		return
	}
	if os.Getpid() == 1 && wrapRequested() {
		wrapMain()
	}
	if len(os.Args) > 1 && os.Args[1] == "relay" {
		relayMain(os.Args[2:])
		return
	}
	if os.Getpid() == 1 {
		initSystem()
	}
	port := cmdlineValue("sb.port")
	if port == "" {
		logf("no sb.port on the kernel command line")
		halt()
	}
	var p uint32
	fmt.Sscan(port, &p)
	c, err := dialVsock(p, 30*time.Second)
	if err != nil {
		logf("dial host: %v", err)
		halt()
	}
	conn = c
	out = bufio.NewWriterSize(c, 1<<16)
	logf("connected to host port %d", p)
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var r request
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			send(reply{ID: -1, Err: err.Error()})
			continue
		}
		go handle(r)
	}
	logf("host connection closed: %v", sc.Err())
	halt()
}

func handle(r request) {
	res, err := dispatch(r)
	rep := reply{ID: r.ID, OK: err == nil, Out: res}
	if err != nil {
		rep.Err = err.Error()
	}
	send(rep)
}

func dispatch(r request) (any, error) {
	a := r.Args
	switch r.Op {
	case "ping":
		return time.Now().UnixNano(), nil
	case "sh":
		cmd := exec.Command("/bin/busybox", "sh", "-c", a["cmd"])
		b, err := cmd.CombinedOutput()
		return string(b), err
	case "insmod":
		return nil, insmod(a["path"], a["params"])
	case "mount":
		var flags uintptr
		if a["ro"] == "1" {
			flags |= unix.MS_RDONLY
		}
		os.MkdirAll(a["target"], 0o755)
		return nil, unix.Mount(a["source"], a["target"], a["fstype"], flags, a["data"])
	case "umount":
		return nil, unix.Unmount(a["target"], 0)
	case "mount9p":
		return nil, mount9p(a)
	case "fuse":
		return startRelay(a)
	case "dropcaches":
		unix.Sync()
		return nil, os.WriteFile("/proc/sys/vm/drop_caches", []byte("3"), 0)
	case "bench":
		return runBench(a)
	case "watch":
		return nil, startWatch(a["dir"], a["recursive"] == "1")
	case "unwatch":
		stopWatch()
		return nil, nil
	case "poweroff":
		go func() {
			time.Sleep(100 * time.Millisecond)
			halt()
		}()
		return nil, nil
	}
	if f := extraOps[r.Op]; f != nil {
		return f(a)
	}
	return nil, fmt.Errorf("unknown op %q", r.Op)
}

func initSystem() {
	for _, m := range []struct{ src, dst, fs, data string }{
		{"proc", "/proc", "proc", ""},
		{"sysfs", "/sys", "sysfs", ""},
		{"devtmpfs", "/dev", "devtmpfs", "mode=0755"},
		{"tmpfs", "/tmp", "tmpfs", "mode=1777"},
		{"tmpfs", "/run", "tmpfs", "mode=0755"},
	} {
		os.MkdirAll(m.dst, 0o755)
		if err := unix.Mount(m.src, m.dst, m.fs, 0, m.data); err != nil {
			logf("mount %s: %v", m.dst, err)
		}
	}
	os.MkdirAll("/sys/fs/cgroup", 0o755)
	unix.Mount("cgroup2", "/sys/fs/cgroup", "cgroup2", 0, "")
	os.Setenv("PATH", "/usr/sbin:/usr/bin:/sbin:/bin")
	exec.Command("/bin/busybox", "--install", "-s").Run()
	if b, err := os.ReadFile("/sbmods/order"); err == nil {
		for _, l := range strings.Fields(string(b)) {
			p := l
			if !strings.HasPrefix(p, "/") {
				p = filepath.Join("/sbmods", l)
			}
			if err := insmod(p, ""); err != nil && err != unix.EEXIST {
				logf("insmod %s: %v", l, err)
			}
		}
	}
}

func insmod(path, params string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return unix.FinitModule(int(f.Fd()), params, 0)
}

func cmdlineValue(key string) string {
	b, _ := os.ReadFile("/proc/cmdline")
	for _, f := range strings.Fields(string(b)) {
		if v, ok := strings.CutPrefix(f, key+"="); ok {
			return v
		}
	}
	return ""
}

func dialVsock(port uint32, timeout time.Duration) (*os.File, error) {
	deadline := time.Now().Add(timeout)
	for {
		fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
		if err != nil {
			if time.Now().After(deadline) {
				return nil, err
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if sockBuf > 0 {
			unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_SNDBUF, sockBuf)
			unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, sockBuf)
		}
		err = unix.Connect(fd, &unix.SockaddrVM{CID: hostCID, Port: port})
		if err == nil {
			return os.NewFile(uintptr(fd), fmt.Sprintf("vsock:%d", port)), nil
		}
		unix.Close(fd)
		if time.Now().After(deadline) {
			return nil, err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// mount9p connects to the host's Plan9 server on a vsock port and mounts it with the
// fd transport, as LCOW's GCS and WSL do.
func mount9p(a map[string]string) error {
	var port uint32
	fmt.Sscan(a["port"], &port)
	f, err := dialVsock(port, 5*time.Second)
	if err != nil {
		return fmt.Errorf("dial 9p port %d: %w", port, err)
	}
	fd := int(f.Fd())
	// The mount keeps its own reference to the file; ours must stay open too, or the
	// fd numbers in the options would be closed before the kernel takes them.
	keep = append(keep, f)
	msize := a["msize"]
	if msize == "" {
		msize = "65536"
	}
	data := fmt.Sprintf("trans=fd,rfdno=%d,wfdno=%d,msize=%s,version=9p2000.L,aname=%s", fd, fd, msize, a["aname"])
	if extra := a["opts"]; extra != "" {
		data += "," + extra
	}
	os.MkdirAll(a["target"], 0o755)
	if err := unix.Mount(a["aname"], a["target"], "9p", 0, data); err != nil {
		return fmt.Errorf("mount 9p %s: %w", data, err)
	}
	return nil
}

var keep []*os.File

// extraOps are ops defined in other files.
var extraOps = map[string]func(map[string]string) (any, error){}

// sockBuf, if set, is the SO_SNDBUF/SO_RCVBUF for new vsock sockets (hv_sock sizes its
// VMBus ring from them).
var sockBuf int

func halt() {
	unix.Sync()
	unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF)
	os.Exit(1)
}
