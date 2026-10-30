// Package relay is the guest half of hyperbutler bind mounts (ADR 0007). It mounts a FUSE
// file system whose server is the host (package share), and copies frames between
// /dev/fuse and one hvsock connection without decoding them. Notifications from the
// host are written to /dev/fuse from their own queue, never from the reply path, so an
// invalidation that waits for a directory lock cannot block the reply that releases it.
// Replay requests (share.IsInject) are performed on the mount by one dedicated thread
// whose id the host learns from the HELLO frame.
package relay

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// maxHostFrame bounds a frame from the host: a 1 MiB READ reply plus its header, or a
// directory listing of the same size.
const maxHostFrame = 1<<20 + 4096

const outHeaderLen = 16

// readFrame reads one host frame (fuse_out_header-prefixed) into buf and returns it.
func readFrame(r io.Reader, buf []byte) ([]byte, error) {
	if _, err := io.ReadFull(r, buf[:outHeaderLen]); err != nil {
		return nil, err
	}
	n := binary.LittleEndian.Uint32(buf)
	if n < outHeaderLen || n > maxHostFrame || int(n) > len(buf) {
		return nil, fmt.Errorf("relay: host frame length %d", n)
	}
	if _, err := io.ReadFull(r, buf[outHeaderLen:n]); err != nil {
		return nil, err
	}
	return buf[:n], nil
}

// helloFrame is the first frame the relay sends: an opcode no kernel uses, carrying
// the injector thread id and the connection's queue number.
func helloFrame(tid, queue uint32) []byte {
	b := make([]byte, 48)
	le := binary.LittleEndian
	le.PutUint32(b[0:], 48)
	le.PutUint32(b[4:], 0x77620001)
	le.PutUint32(b[40:], tid)
	le.PutUint32(b[44:], queue)
	return b
}

// notifyInject is the out-header error code of a replay request (share.notifyInject).
const notifyInject = 0x7762

// Replay kinds (share.injModify and friends).
const (
	injModify = 1
	injCreate = 2
	injDelete = 3
	injRename = 4
)

type injectFrame struct {
	kind          int
	mode          uint32
	path, newPath string
}

// parseInject decodes a replay request body (after the out header).
func parseInject(b []byte) (injectFrame, bool) {
	le := binary.LittleEndian
	var f injectFrame
	if len(b) < 12 {
		return f, false
	}
	f.kind, f.mode = int(le.Uint32(b)), le.Uint32(b[4:])
	n := int(le.Uint32(b[8:]))
	b = b[12:]
	if n < 0 || n > len(b) || n > 4096 {
		return f, false
	}
	f.path = string(b[:n])
	b = b[n:]
	if len(b) < 4 {
		return f, false
	}
	n = int(le.Uint32(b))
	b = b[4:]
	if n < 0 || n > len(b) || n > 4096 {
		return f, false
	}
	f.newPath = string(b[:n])
	return f, f.kind >= injModify && f.kind <= injRename
}

// safeJoin joins a host-supplied path (share/a/b) under the mount point, refusing
// anything that could leave it.
func safeJoin(mount, p string) (string, error) {
	if p == "" || strings.HasPrefix(p, "/") {
		return "", errors.New("relay: bad replay path")
	}
	for _, c := range strings.Split(p, "/") {
		if c == "" || c == "." || c == ".." || strings.IndexByte(c, 0) >= 0 {
			return "", errors.New("relay: bad replay path")
		}
	}
	return mount + "/" + p, nil
}

// mountDev returns the device (major, minor) of the last mount at mountpoint in a
// /proc/self/mountinfo listing. It needs no request to the mounted file system, so it
// works while the FUSE connection has nobody serving it.
func mountDev(mountinfo []byte, mountpoint string) (major, minor uint32, ok bool) {
	for _, line := range strings.Split(string(mountinfo), "\n") {
		f := strings.Fields(line)
		// id parent major:minor root mountpoint options ...
		if len(f) < 5 || unescapeMount(f[4]) != mountpoint {
			continue
		}
		maj, min, found := strings.Cut(f[2], ":")
		a, err1 := strconv.ParseUint(maj, 10, 32)
		b, err2 := strconv.ParseUint(min, 10, 32)
		if !found || err1 != nil || err2 != nil {
			continue
		}
		major, minor, ok = uint32(a), uint32(b), true
	}
	return major, minor, ok
}

// unescapeMount undoes mountinfo's octal escapes (\040 for a space).
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
