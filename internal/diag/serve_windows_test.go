//go:build windows

package diag

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

var pipeSeq atomic.Int64

func testPipeName() string {
	return fmt.Sprintf(`\\.\pipe\hyperbutler-diag-test-%d-%d`, os.Getpid(), pipeSeq.Add(1))
}

func pipeClient(name string) *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return winio.DialPipeContext(ctx, name)
			},
		},
	}
}

func TestServeStatsOverPipe(t *testing.T) {
	name := testPipeName()
	s, err := Serve(name)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	c := pipeClient(name)
	defer c.CloseIdleConnections()
	resp, err := c.Get("http://diag" + StatsPath)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st Stats
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if st.Goroutines < 1 || st.Handles < 1 || st.MemStats.Sys == 0 {
		t.Fatalf("implausible stats %+v", st)
	}
}

// The pipe's DACL must grant the current user and nobody else.
func TestServePipeACL(t *testing.T) {
	name := testPipeName()
	s, err := Serve(name)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Open a client handle with only READ_CONTROL and read the descriptor through it.
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		t.Fatal(err)
	}
	// The accept loop starts asynchronously; until it calls Accept there is no free
	// pipe instance.
	var h windows.Handle
	for deadline := time.Now().Add(5 * time.Second); ; {
		h, err = windows.CreateFile(p, windows.READ_CONTROL, 0, nil, windows.OPEN_EXISTING, 0, 0)
		if !errors.Is(err, windows.ERROR_PIPE_BUSY) || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	sd, err := windows.GetSecurityInfo(h, windows.SE_KERNEL_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	me := tu.User.Sid
	owner, _, err := sd.Owner()
	if err != nil || !owner.Equals(me) {
		t.Fatalf("owner = %v, %v; want %s", owner, err, me)
	}
	ctl, _, err := sd.Control()
	if err != nil || ctl&windows.SE_DACL_PROTECTED == 0 {
		t.Fatalf("DACL not protected (control %#x, %v)", ctl, err)
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		t.Fatalf("no DACL: %v", err)
	}
	if dacl.AceCount != 1 {
		t.Fatalf("DACL has %d ACEs, want 1 (SDDL %s)", dacl.AceCount, sd)
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, 0, &ace); err != nil {
		t.Fatal(err)
	}
	sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || !sid.Equals(me) {
		t.Fatalf("ACE type %d for %s, want allow for %s (SDDL %s)", ace.Header.AceType, sid, me, sd)
	}
}

func TestServeRejectsTakenName(t *testing.T) {
	name := testPipeName()
	s, err := Serve(name)
	if err != nil {
		t.Fatal(err)
	}
	if s2, err := Serve(name); err == nil {
		s2.Close()
		t.Fatal("second Serve on the same pipe name succeeded")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// After Close the name is free again.
	s3, err := Serve(name)
	if err != nil {
		t.Fatalf("Serve after Close: %v", err)
	}
	s3.Close()
}

func TestServeInvalidName(t *testing.T) {
	for _, name := range []string{"", `\\server\pipe\x`, `\\.\pipe\`, `\\.\pipe\a\b`} {
		if s, err := Serve(name); err == nil {
			s.Close()
			t.Errorf("Serve(%q) succeeded", name)
		}
	}
}

// Close must end a running CPU profile instead of waiting out its duration, and must
// leave no goroutine behind (TestMain checks the latter).
func TestCloseInterruptsProfile(t *testing.T) {
	name := testPipeName()
	s, err := Serve(name)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := winio.DialPipe(name, nil)
	if err != nil {
		s.Close()
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := fmt.Fprintf(conn, "GET /debug/pprof/profile?seconds=60 HTTP/1.1\r\nHost: diag\r\n\r\n"); err != nil {
		s.Close()
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // let the handler start
	start := time.Now()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("Close took %v with a profile running", d)
	}
}

// At most MaxConns connections are served at once; the next one waits until a slot
// frees.
func TestConnLimit(t *testing.T) {
	name := testPipeName()
	s, err := Serve(name)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var held []net.Conn
	defer func() {
		for _, c := range held {
			c.Close()
		}
	}()
	get := func(c net.Conn) error {
		if err := c.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: diag\r\n\r\n", StatsPath); err != nil {
			return err
		}
		resp, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err != nil {
			return err
		}
		return resp.Body.Close()
	}
	for i := 0; i < MaxConns; i++ {
		c, err := winio.DialPipe(name, nil)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, c)
		if err := get(c); err != nil {
			t.Fatalf("conn %d: %v", i, err)
		}
	}
	// go-winio creates the next pipe instance only when Accept is called, and Accept is
	// not called while every slot is held, so a further client cannot even connect. If
	// a future go-winio queued instances, the request must still go unanswered.
	short := 500 * time.Millisecond
	if extra, err := winio.DialPipe(name, &short); err == nil {
		held = append(held, extra)
		if err := extra.SetDeadline(time.Now().Add(short)); err != nil {
			t.Fatal(err)
		}
		if _, err := fmt.Fprintf(extra, "GET %s HTTP/1.1\r\nHost: diag\r\n\r\n", StatsPath); err != nil {
			t.Fatal(err)
		}
		if resp, err := http.ReadResponse(bufio.NewReader(extra), nil); err == nil {
			_ = resp.Body.Close()
			t.Fatal("connection beyond MaxConns was served")
		}
		extra.Close()
		held = held[:len(held)-1]
	}

	held[0].Close()
	held = held[1:]
	c, err := winio.DialPipe(name, nil)
	if err != nil {
		t.Fatal(err)
	}
	held = append(held, c)
	if err := get(c); err != nil {
		t.Fatalf("after a slot freed: %v", err)
	}
}
