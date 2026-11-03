// Command xuser is the cross-user probe of ADR 0004's two-account test (M3): run as a
// second, non-admin user B while user A's VM runs through the broker service, it tries
// what B must not be able to do to A and prints one line per attempt, "PASS" when the
// attempt was refused and "FAIL" when it was not.
//
//	xuser -vmid <A's runtime id> -data <A's data directory> [-shared \\.\pipe\hyperbutler-broker]
//
// With -catch it instead listens on the wildcard VM id for the guest-initiated services
// (the plain network link 1024, the share 2385) for that long, as B squatting the window
// in which A's engine (re)starts while A's VM runs, and reports every connection that
// arrives: it answers alternately with nothing and with 32 random bytes (a wrong
// secret), and the guest must close each one without having written a byte.
//
//	xuser -catch 3m [-vmid <A's runtime id>]
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Higangssh/hyperbutler/internal/guid"
	"github.com/Microsoft/go-winio"
	wguid "github.com/Microsoft/go-winio/pkg/guid"
	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/broker"
	"github.com/Higangssh/hyperbutler/internal/transport"
)

var failed int

func report(ok bool, what string, detail any) {
	verdict := "PASS"
	if !ok {
		verdict = "FAIL"
		failed++
	}
	fmt.Printf("%s  %-58s %v\n", verdict, what, detail)
}

func main() {
	vmidFlag := flag.String("vmid", "", "A's VM runtime id")
	data := flag.String("data", "", "A's data directory")
	shared := flag.String("shared", broker.ServicePipe, "the broker's shared endpoint")
	catchFor := flag.Duration("catch", 0, "listen on the wildcard VM id for the guest-initiated services this long instead of probing")
	flag.Parse()
	if *catchFor > 0 {
		catch(*catchFor, *vmidFlag)
		fmt.Printf("%d attempt(s) not refused\n", failed)
		if failed > 0 {
			os.Exit(1)
		}
		return
	}
	vmid, err := guid.FromString(*vmidFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "xuser: -vmid:", err)
		os.Exit(2)
	}
	me := "?"
	if tu, err := windows.GetCurrentProcessToken().GetTokenUser(); err == nil {
		me = tu.User.Sid.String()
	}
	fmt.Printf("xuser as %s, against VM %s\n", me, vmid)
	ctx := context.Background()

	// 1. The broker: B gets its own endpoint, which knows nothing of A's VM.
	c, err := dialBroker(ctx, *shared)
	if err != nil {
		report(false, "broker: B reaches its own endpoint (sanity)", err)
	} else {
		st, err := c.Status()
		report(err == nil && st.VMID != vmid.ToArray() && st.Secret.IsZero(), "broker: B's status does not show A's VM", fmt.Sprintf("state=%v err=%v", st.State, err))
		err = c.Stop(true)
		report(true, "broker: B's stop touches only B (checked by A afterwards)", err)
		_ = c.Close()
	}

	// 2. A's per-user endpoint: open it, and create an instance of it.
	for _, name := range brokerPipes() {
		if strings.Contains(name, userHash(me)) {
			continue
		}
		p := `\\.\pipe\` + name
		ctx2, cancel := context.WithTimeout(ctx, 2*time.Second)
		conn, err := winio.DialPipeContext(ctx2, p)
		cancel()
		if conn != nil {
			_ = conn.Close()
		}
		report(err != nil, "pipe: open another user's endpoint "+short(name), err)
		l, err := winio.ListenPipe(p, nil)
		if l != nil {
			_ = l.Close()
		}
		report(err != nil, "pipe: add an instance to "+short(name), err)
	}

	// 3. A's guest services over hvsock: no secret, a zero secret, a short one.
	for _, port := range []uint32{2375, 2380, 1024, 2385} {
		probeService(ctx, vmid, port)
	}

	// 4. Listen in A's VM's place (guest-to-host services).
	for _, port := range []uint32{1024, 2385} {
		l, err := winio.ListenHvsock(&winio.HvsockAddr{VMID: wguid.GUID(vmid), ServiceID: wguid.GUID(transport.VsockServiceID(port))})
		if l != nil {
			_ = l.Close()
		}
		report(err != nil, fmt.Sprintf("hvsock: listen for A's VM on port %d", port), err)
		l, err = winio.ListenHvsock(&winio.HvsockAddr{VMID: wguid.GUID{}, ServiceID: wguid.GUID(transport.VsockServiceID(port))})
		if l != nil {
			_ = l.Close()
		}
		report(err != nil, fmt.Sprintf("hvsock: listen for every VM on port %d", port), err)
	}

	// 5. A's data directory.
	if *data != "" {
		_, err := os.ReadDir(*data)
		report(err != nil, "files: list A's data directory", err)
		f, err := os.Open(*data + `\data.vhdx`)
		if f != nil {
			_ = f.Close()
		}
		report(err != nil, "files: open A's data disk", err)
	}

	fmt.Printf("%d attempt(s) not refused\n", failed)
	if failed > 0 {
		os.Exit(1)
	}
}

// catch listens on the wildcard VM id for ports 1024 and 2385 for d and reports each
// connection a guest makes to it. A refused listen passes; a connection passes when the
// guest wrote nothing to it (it checks the listener's secret before it writes, ADR 0004
// rule 6). vmid, if set, names A's VM in the report.
func catch(d time.Duration, vmid string) {
	fmt.Printf("xuser -catch: listening on the wildcard VM id for %v; restart A's engine now (A's VM %s keeps running)\n", d, vmid)
	var wg sync.WaitGroup
	var mu sync.Mutex
	conns := 0
	for _, port := range []uint32{1024, 2385} {
		l, err := winio.ListenHvsock(&winio.HvsockAddr{VMID: wguid.GUID{}, ServiceID: wguid.GUID(transport.VsockServiceID(port))})
		if err != nil {
			report(true, fmt.Sprintf("hvsock: listen for every VM on port %d", port), err)
			continue
		}
		time.AfterFunc(d, func() { _ = l.Close() })
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				mu.Lock()
				conns++
				n := conns
				mu.Unlock()
				wg.Add(1)
				go func() {
					defer wg.Done()
					catchOne(c, port, n%2 == 0)
				}()
			}
		}()
	}
	wg.Wait()
	if conns == 0 {
		fmt.Println("no connection arrived: meaningful only if A's engine restarted during the window")
	}
}

// catchOne reads what the guest sends on c, after sending a wrong secret if wrong.
func catchOne(c net.Conn, port uint32, wrong bool) {
	defer func() { _ = c.Close() }()
	how := "nothing sent"
	if wrong {
		how = "a wrong secret sent"
		b := make([]byte, 32)
		_, _ = rand.Read(b)
		_, _ = c.Write(b)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := io.Copy(io.Discard, c)
	report(n == 0, fmt.Sprintf("hvsock: guest %v on port %d (%s)", c.RemoteAddr(), port, how),
		fmt.Sprintf("the guest wrote %d byte(s), then %s", n, errText(err)))
}

func dialBroker(ctx context.Context, shared string) (*broker.Client, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return broker.Dial(ctx, shared)
}

// probeService connects to A's guest on port and sends what B can: nothing, a zero
// secret, 31 bytes, then a Docker request. Refused means no connection, or a connection
// closed without one byte of answer.
func probeService(ctx context.Context, vmid guid.GUID, port uint32) {
	tries := []struct {
		name string
		send []byte
	}{
		{"nothing", nil},
		{"a zero secret", make([]byte, 32)},
		{"31 bytes", make([]byte, 31)},
		{"a Docker request", []byte("GET /_ping HTTP/1.0\r\n\r\n")},
	}
	for _, try := range tries {
		what := fmt.Sprintf("hvsock: port %d with %s", port, try.name)
		d := &transport.HvsockDialer{Target: transport.HvsockTarget{VMID: vmid, ServiceID: transport.VsockServiceID(port)}}
		ctx2, cancel := context.WithTimeout(ctx, 3*time.Second)
		conn, err := d.DialContext(ctx2)
		cancel()
		if err != nil {
			report(true, what, "connect refused: "+errText(err))
			continue
		}
		if try.send != nil {
			_, _ = conn.Write(try.send)
		}
		_ = conn.SetReadDeadline(time.Now().Add(4 * time.Second))
		b := make([]byte, 64)
		n, err := conn.Read(b)
		_ = conn.Close()
		report(n == 0, what, fmt.Sprintf("connected; read %d byte(s), %s", n, errText(err)))
	}
}

func errText(err error) string {
	switch {
	case err == nil:
		return "no error"
	case errors.Is(err, io.EOF):
		return "closed (EOF)"
	case errors.Is(err, os.ErrDeadlineExceeded), errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	return err.Error()
}

// brokerPipes lists the pipes whose names start with hyperbutler-broker-.
func brokerPipes() []string {
	var out []string
	var fd windows.Win32finddata
	p, _ := windows.UTF16PtrFromString(`\\.\pipe\*`)
	h, err := windows.FindFirstFile(p, &fd)
	if err != nil {
		return nil
	}
	defer windows.FindClose(h)
	for {
		n := windows.UTF16ToString(fd.FileName[:])
		if strings.HasPrefix(n, "hyperbutler-broker-") {
			out = append(out, n)
		}
		if windows.FindNextFile(h, &fd) != nil {
			return out
		}
	}
}

// userHash is the SID part of sid's endpoint names (internal/broker userPipePrefix).
func userHash(sid string) string {
	h := sha256.Sum256([]byte(sid))
	return hex.EncodeToString(h[:16])
}

func short(name string) string {
	if len(name) > 40 {
		return name[:40] + "…"
	}
	return name
}
