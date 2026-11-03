package netstack

import (
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
)

var qpcFreq = func() int64 {
	var f int64
	_, _, _ = windows.NewLazySystemDLL("kernel32.dll").NewProc("QueryPerformanceFrequency").Call(uintptr(unsafe.Pointer(&f)))
	return f
}()

var procQPCExp = windows.NewLazySystemDLL("kernel32.dll").NewProc("QueryPerformanceCounter")

func qpcNow() time.Duration {
	var v int64
	_, _, _ = procQPCExp.Call(uintptr(unsafe.Pointer(&v)))
	return time.Duration(float64(v) * 1e9 / float64(qpcFreq))
}

// TestTailExperiment measures 64-byte round trips through a published port while a
// second published connection carries bulk data host to guest, reconnected every
// HB_TAIL_RECONNECT bytes (0: one connection). Off unless HB_TAIL_EXP is set.
func TestTailExperiment(t *testing.T) {
	if os.Getenv("HB_TAIL_EXP") == "" {
		t.Skip("HB_TAIL_EXP not set")
	}
	reconnect, _ := strconv.ParseInt(os.Getenv("HB_TAIL_RECONNECT"), 10, 64)
	mbps, _ := strconv.ParseFloat(os.Getenv("HB_TAIL_MBPS"), 64) // load rate cap, 0: none
	samples := 20000
	if s, err := strconv.Atoi(os.Getenv("HB_TAIL_N")); err == nil {
		samples = s
	}
	n := newTestStack(t, func(c *Config) { c.MTU = 1500 })
	g := newFakeGuestMode(t, n, true)

	listen := func(port uint16, serve func(net.Conn)) net.Listener {
		gl, err := gonet.ListenTCP(g.s, tcpip.FullAddress{NIC: 1, Port: port}, ipv4.ProtocolNumber)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { gl.Close() })
		go func() {
			for {
				c, err := gl.Accept()
				if err != nil {
					return
				}
				go serve(c)
			}
		}()
		cl, err := n.Publish(Publish{Host: netip.MustParseAddrPort("127.0.0.1:0"), GuestPort: port})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cl.Close() })
		return cl.(net.Listener)
	}
	echoL := listen(8080, func(c net.Conn) { _, _ = io.Copy(c, c); c.Close() })
	sinkL := listen(9090, func(c net.Conn) { _, _ = io.Copy(io.Discard, c); c.Close() })

	stop := make(chan struct{})
	var conns, moved atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 256<<10)
		t0 := qpcNow()
		for {
			c, err := net.Dial("tcp", sinkL.Addr().String())
			if err != nil {
				return
			}
			conns.Add(1)
			var sent int64
			for reconnect == 0 || sent < reconnect {
				select {
				case <-stop:
					c.Close()
					return
				default:
				}
				m, err := c.Write(buf)
				sent += int64(m)
				tot := moved.Add(int64(m))
				if mbps > 0 {
					due := time.Duration(float64(tot) * 8 / (mbps * 1e6) * 1e9)
					if ahead := due - (qpcNow() - t0); ahead > 0 {
						time.Sleep(ahead)
					}
				}
				if err != nil {
					break
				}
			}
			c.Close()
		}
	}()

	time.Sleep(500 * time.Millisecond)
	c, err := net.Dial("tcp", echoL.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.(*net.TCPConn).SetNoDelay(true)
	msg, back := make([]byte, 64), make([]byte, 64)
	lat := make([]time.Duration, 0, samples)
	var slowAt []string
	start := time.Now()
	for range samples {
		t0 := qpcNow()
		if _, err := c.Write(msg); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(c, back); err != nil {
			t.Fatal(err)
		}
		d := qpcNow() - t0
		lat = append(lat, d)
		if d > 3*time.Millisecond {
			slowAt = append(slowAt, fmt.Sprintf("%.0fms@%.2fs(conn %d)", float64(d)/1e6, time.Since(start).Seconds(), conns.Load()))
		}
	}
	elapsed := time.Since(start)
	close(stop)
	<-done
	slices.Sort(lat)
	q := func(p float64) time.Duration { return lat[min(len(lat)-1, int(p*float64(len(lat))))] }
	t.Logf("reconnect=%d n=%d in %.1fs: p50 %v p99 %v p99.9 %v max %v; load %d MiB over %d conns (%.0f Mbit/s)",
		reconnect, samples, elapsed.Seconds(), q(0.5), q(0.99), q(0.999), lat[len(lat)-1],
		moved.Load()>>20, conns.Load(), float64(moved.Load())*8/elapsed.Seconds()/1e6)
	t.Logf("samples over 3 ms: %d %v", len(slowAt), slowAt[:min(len(slowAt), 30)])
}
