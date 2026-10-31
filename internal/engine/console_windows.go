//go:build windows

package engine

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/transport"
)

// openConsole connects to the COM1 pipe that the VM worker serves.
func openConsole(ctx context.Context, name string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		// SQOS identification: the pipe server must not be able to impersonate us.
		h, err := windows.CreateFile(p, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING,
			windows.SECURITY_SQOS_PRESENT|windows.SECURITY_IDENTIFICATION, 0)
		if err == nil {
			return os.NewFile(uintptr(h), name), nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("open console %s: %w", name, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// maxConsoleLine bounds one console line kept for step reporting. Longer lines still go
// to w in full; only their first maxConsoleLine bytes are considered.
const maxConsoleLine = 4 << 10

// streamConsole copies the guest console to w and reports the init's "hyperbutler-t" timing
// markers and agent lines as steps. The guest is untrusted: reported lines have control
// characters escaped so they cannot drive the host terminal.
func streamConsole(r io.Reader, w io.Writer, step func(string, ...any)) {
	br := bufio.NewReaderSize(r, maxConsoleLine)
	first, cont := true, false
	for {
		chunk, err := br.ReadSlice('\n')
		if len(chunk) > 0 {
			if first {
				step("first console output")
				first = false
			}
			_, _ = w.Write(chunk)
			line := strings.TrimRight(string(chunk), "\r\n")
			if !cont && reportLine(line) {
				step("guest: %s", sanitize(line))
			}
			cont = !bytes.HasSuffix(chunk, []byte("\n"))
		}
		if err != nil && !errors.Is(err, bufio.ErrBufferFull) {
			return
		}
	}
}

func reportLine(line string) bool {
	return strings.HasPrefix(line, "hyperbutler-t ") && !strings.Contains(line, " mem ") ||
		strings.HasPrefix(line, "hyperbutler-agent: ") || strings.HasPrefix(line, "hyperbutler: ")
}

// sanitize escapes control characters and invalid UTF-8.
func sanitize(s string) string {
	q := strconv.QuoteToGraphic(s)
	return q[1 : len(q)-1]
}

// waitPing polls GET /_ping over hvsock until dockerd answers 200 OK.
func waitPing(ctx context.Context, target transport.HvsockTarget, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	d := &transport.HvsockDialer{Target: target}
	client := &http.Client{Transport: &http.Transport{
		DialContext:       func(ctx context.Context, _, _ string) (net.Conn, error) { return d.DialContext(ctx) },
		DisableKeepAlives: true,
	}}
	var last error
	for {
		// A connect issued before the agent listens hangs instead of failing, so keep each
		// attempt short.
		actx, acancel := context.WithTimeout(ctx, 500*time.Millisecond)
		req, _ := http.NewRequestWithContext(actx, http.MethodGet, "http://docker/_ping", nil)
		resp, err := client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				acancel()
				return nil
			}
			err = fmt.Errorf("status %s", resp.Status)
		}
		acancel()
		last = err
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.Canceled) {
				return ctx.Err()
			}
			return fmt.Errorf("dockerd not ready after %v: %w", timeout, last)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// cappedFile is the console log: when it passes max bytes it is renamed to path.1
// (replacing an older one) and a new file is started, so a chatty or hostile guest
// cannot fill the host disk through its console.
type cappedFile struct {
	path string
	max  int64
	f    *os.File
	n    int64
}

func openCapped(path string, max int64) (*cappedFile, error) {
	// Keep the previous boot's console.
	_ = os.Rename(path, path+".1")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // G304: path is the console log in the data directory
	if err != nil {
		return nil, err
	}
	return &cappedFile{path: path, max: max, f: f}, nil
}

func (c *cappedFile) Write(p []byte) (int, error) {
	if c.f == nil {
		return len(p), nil
	}
	if c.n+int64(len(p)) > c.max {
		_ = c.f.Close()
		_ = os.Rename(c.path, c.path+".1")
		f, err := os.OpenFile(c.path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			c.f = nil
			return len(p), nil
		}
		c.f, c.n = f, 0
	}
	n, err := c.f.Write(p)
	c.n += int64(n)
	return n, err
}

func (c *cappedFile) Close() error {
	if c.f == nil {
		return nil
	}
	err := c.f.Close()
	c.f = nil
	return err
}
