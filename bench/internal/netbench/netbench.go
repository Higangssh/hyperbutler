// Package netbench holds the TCP measurements of the network benchmarks (bench/net-perf,
// ADR 0006): bulk download and upload, small request/response latency and connections
// per second, and the servers they talk to. The same code runs on the Windows host (the
// raw baseline) and in a container (bench/net-guest), so the two sides differ only in
// the path between them.
package netbench

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// BufSize is the read and write size of the bulk measurements. It is large so that the
// endpoints' own syscalls are not what is measured.
const BufSize = 256 << 10

// Server kinds, at consecutive ports from a base port.
const (
	// Source writes an 8-byte seed, then the pattern of that seed (see Pattern) until
	// the peer closes. Download checks what it reads against it.
	Source = 0
	// Sink reads until EOF, then writes how many bytes it read (8 bytes) and their
	// CRC-32C (4 bytes), so that Upload can check that every byte arrived intact.
	Sink = 1
	Echo = 2 // returns every byte (latency)
	// Ping returns one byte, then waits for the client to go away (connections per
	// second). The client resets the connection (SO_LINGER 0): an orderly close leaves
	// TIME_WAIT behind, and at thousands of connections per second that exhausts the
	// ephemeral ports of a loopback pair (both ends are this host) within a minute.
	Ping  = 3
	Kinds = 4
)

// Serve accepts on ln until it fails and serves each connection as kind.
func Serve(ln net.Listener, kind int) error {
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		if tc, ok := c.(*net.TCPConn); ok {
			_ = tc.SetNoDelay(true)
		}
		go serveConn(c, kind)
	}
}

func serveConn(c net.Conn, kind int) {
	defer func() { _ = c.Close() }()
	switch kind {
	case Source:
		seed := newSeed()
		var hdr [8]byte
		binary.BigEndian.PutUint64(hdr[:], seed)
		if _, err := c.Write(hdr[:]); err != nil {
			return
		}
		pw := newPatternWriter(NewPattern(seed))
		for {
			if _, err := c.Write(pw.next()); err != nil {
				return
			}
		}
	case Sink:
		buf := make([]byte, BufSize)
		var n int64
		var sum uint32
		for {
			k, err := c.Read(buf)
			n += int64(k)
			sum = crc32.Update(sum, castagnoli, buf[:k])
			if err != nil {
				break
			}
		}
		_, _ = c.Write(sinkReport(n, sum))
	case Ping:
		var b [1]byte
		_ = c.SetDeadline(time.Now().Add(10 * time.Second))
		if _, err := io.ReadFull(c, b[:]); err == nil {
			if _, err := c.Write(b[:]); err == nil {
				_, _ = c.Read(b[:]) // until the client's reset (or close)
			}
		}
	default:
		buf := make([]byte, 64<<10)
		for {
			n, err := c.Read(buf)
			if n > 0 {
				if _, werr := c.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}
}

// Listen opens the servers on host at base, base+1, ... (one per kind). Close the
// listeners to stop them.
func Listen(host string, base int) ([]net.Listener, error) {
	var lns []net.Listener
	for k := 0; k < Kinds; k++ {
		ln, err := net.Listen("tcp", net.JoinHostPort(host, fmt.Sprint(base+k)))
		if err != nil {
			for _, l := range lns {
				_ = l.Close()
			}
			return nil, err
		}
		lns = append(lns, ln)
		go func(k int) { _ = Serve(ln, k) }(k)
	}
	return lns, nil
}

// Result is one measurement. Fields that do not apply are zero and omitted.
type Result struct {
	Name    string  `json:"name"`
	Bytes   int64   `json:"bytes,omitempty"`
	Seconds float64 `json:"s,omitempty"`
	Gbit    float64 `json:"gbit,omitempty"`
	N       int     `json:"n,omitempty"`
	P50us   float64 `json:"p50_us,omitempty"`
	P99us   float64 `json:"p99_us,omitempty"`
	P999us  float64 `json:"p999_us,omitempty"`
	MaxUs   float64 `json:"max_us,omitempty"`
	ConnsPS float64 `json:"conns_per_s,omitempty"`
	Errors  int64   `json:"errors,omitempty"`
	// Corrupt counts data that arrived different from what was sent: BlockSize blocks
	// for a download, and 1 for an upload whose CRC-32C or length the sink reported
	// different. Anything but 0 fails the benchmark.
	Corrupt int64 `json:"corrupt,omitempty"`
	// FirstError is the first error seen, if any.
	FirstError string `json:"first_error,omitempty"`
}

// JSON is r as one line.
func (r Result) JSON() []byte {
	b, _ := json.Marshal(r)
	return append(b, '\n')
}

// ParseResult reads one line written by JSON.
func ParseResult(line []byte) (Result, error) {
	var r Result
	if err := json.Unmarshal(line, &r); err != nil {
		return r, err
	}
	if r.Name == "" {
		return r, errors.New("netbench: result without a name")
	}
	return r, nil
}

func gbit(n int64, d time.Duration) float64 { return float64(n) * 8 / d.Seconds() / 1e9 }

// Download reads from a Source for d, checks one block in VerifyEvery against the
// pattern that was sent, and reports the rate.
func Download(ctx context.Context, addr string, d time.Duration) (Result, error) {
	c, err := dial(ctx, addr)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = c.Close() }()
	return readFor(c, d, "download")
}

func readFor(r net.Conn, d time.Duration, name string) (Result, error) {
	buf := make([]byte, BufSize)
	t0 := time.Now()
	end := t0.Add(d)
	_ = r.SetReadDeadline(end.Add(30 * time.Second))
	var hdr [8]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return Result{}, fmt.Errorf("%s: reading the source's seed: %w", name, err)
	}
	v := newVerifier(NewPattern(binary.BigEndian.Uint64(hdr[:])), VerifyEvery)
	got := int64(len(hdr))
	for time.Now().Before(end) {
		n, err := r.Read(buf)
		got += int64(n)
		v.check(buf[:n])
		if err != nil {
			return Result{}, fmt.Errorf("%s: %w after %d bytes", name, err, got)
		}
	}
	el := time.Since(t0)
	return Result{Name: name, Bytes: got, Seconds: el.Seconds(), Gbit: gbit(got, el), Corrupt: v.corrupt}, nil
}

type closeWriter interface{ CloseWrite() error }

// Upload writes the pattern of a fresh seed to a Sink for d, half-closes and waits for
// the sink's report, so every byte counted has arrived; Corrupt is 1 if the sink read
// other bytes than were sent.
func Upload(ctx context.Context, addr string, d time.Duration) (Result, error) {
	c, err := dial(ctx, addr)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = c.Close() }()
	var sent int64
	var sum uint32
	pw := newPatternWriter(NewPattern(newSeed()))
	t0 := time.Now()
	end := t0.Add(d)
	_ = c.SetDeadline(end.Add(30 * time.Second))
	for time.Now().Before(end) {
		b := pw.next()
		n, err := c.Write(b)
		sent += int64(n)
		sum = crc32.Update(sum, castagnoli, b[:n])
		if err != nil {
			return Result{}, fmt.Errorf("upload: %w after %d bytes", err, sent)
		}
	}
	cw, ok := c.(closeWriter)
	if !ok {
		return Result{}, errors.New("upload: connection cannot half-close")
	}
	if err := cw.CloseWrite(); err != nil {
		return Result{}, err
	}
	var rep [sinkReportLen]byte
	if _, err := io.ReadFull(c, rep[:]); err != nil {
		return Result{}, fmt.Errorf("upload: waiting for the sink: %w", err)
	}
	el := time.Since(t0)
	r := Result{Name: "upload", Bytes: sent, Seconds: el.Seconds(), Gbit: gbit(sent, el)}
	if n, s := binary.BigEndian.Uint64(rep[:8]), binary.BigEndian.Uint32(rep[8:]); n != uint64(sent) || s != sum {
		r.Corrupt = 1
		r.FirstError = fmt.Sprintf("the sink read %d bytes with CRC-32C %08x; sent %d with %08x", n, s, sent, sum)
	}
	return r, nil
}

// Clock returns a monotonic time in nanoseconds. Windows overrides it with the
// performance counter.
var Clock = func() int64 { return time.Now().UnixNano() }

// RoundTrips sends size bytes to an Echo and waits for them, n times one after the
// other, after a warm-up. With load set, a Download from load runs for the whole time.
func RoundTrips(ctx context.Context, addr string, n, size int, load string) (Result, error) {
	c, err := dial(ctx, addr)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = c.Close() }()
	name := "rr"
	var stopLoad func() error
	if load != "" {
		name = "rr-loaded"
		lc, err := dial(ctx, load)
		if err != nil {
			return Result{}, err
		}
		var wg sync.WaitGroup
		var lerr error
		var stop atomic.Bool
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, BufSize)
			for !stop.Load() {
				if _, err := lc.Read(buf); err != nil {
					if !stop.Load() {
						lerr = err
					}
					return
				}
			}
		}()
		stopLoad = func() error { stop.Store(true); _ = lc.Close(); wg.Wait(); return lerr }
		time.Sleep(300 * time.Millisecond)
	}
	req := make([]byte, size)
	resp := make([]byte, size)
	_ = c.SetDeadline(time.Now().Add(5 * time.Minute))
	lat := make([]int64, 0, n)
	for i := 0; i < n+n/10; i++ {
		t0 := Clock()
		if _, err := c.Write(req); err != nil {
			return Result{}, err
		}
		if _, err := io.ReadFull(c, resp); err != nil {
			return Result{}, err
		}
		if i >= n/10 { // the first tenth warms up both stacks
			lat = append(lat, Clock()-t0)
		}
	}
	if stopLoad != nil {
		if err := stopLoad(); err != nil {
			return Result{}, fmt.Errorf("rr: load: %w", err)
		}
	}
	r := Latency(lat)
	r.Name = name
	return r, nil
}

// Latency summarizes round trips in nanoseconds.
func Latency(ns []int64) Result {
	if len(ns) == 0 {
		return Result{}
	}
	s := append([]int64(nil), ns...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	p := func(q float64) float64 { return float64(s[int(float64(len(s)-1)*q)]) / 1e3 }
	return Result{N: len(s), P50us: p(0.50), P99us: p(0.99), P999us: p(0.999), MaxUs: float64(s[len(s)-1]) / 1e3}
}

// ConnsPerSecond opens connections to a Ping from par goroutines for d: each one
// connects, sends one byte, reads it back and resets the connection.
func ConnsPerSecond(ctx context.Context, addr string, d time.Duration, par int) (Result, error) {
	var ok, bad atomic.Int64
	var first atomic.Pointer[string]
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	// The dialer times out on ctx's deadline by the clock, which can be before ctx's own
	// timer marks it done (on a loaded machine, by tens of milliseconds). Check the clock
	// too, or the loop spins on dials that fail at once and counts each as an error.
	deadline, _ := ctx.Deadline()
	over := func() bool { return ctx.Err() != nil || !time.Now().Before(deadline) }
	var wg sync.WaitGroup
	t0 := time.Now()
	for i := 0; i < par; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b := []byte{1}
			for !over() {
				c, err := dial(ctx, addr)
				if err != nil {
					if !over() {
						bad.Add(1)
						msg := err.Error()
						first.CompareAndSwap(nil, &msg)
					}
					continue
				}
				_ = c.SetDeadline(time.Now().Add(10 * time.Second))
				_, err = c.Write(b)
				if err == nil {
					_, err = io.ReadFull(c, b)
				}
				if tc, ok := c.(*net.TCPConn); ok {
					_ = tc.SetLinger(0) // reset: no TIME_WAIT (see Ping)
				}
				_ = c.Close()
				if err != nil {
					bad.Add(1)
					msg := err.Error()
					first.CompareAndSwap(nil, &msg)
					continue
				}
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	el := time.Since(t0)
	return Result{Name: "cps", N: int(ok.Load()), Seconds: el.Seconds(), ConnsPS: float64(ok.Load()) / el.Seconds(), Errors: bad.Load(), FirstError: deref(first.Load())}, nil
}

// FetchURL downloads url (HTTP or HTTPS) once and discards the body.
func FetchURL(ctx context.Context, url string) (Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Result{}, err
	}
	tr := &http.Transport{Proxy: nil, DisableCompression: true, ForceAttemptHTTP2: false}
	defer tr.CloseIdleConnections()
	t0 := time.Now()
	resp, err := (&http.Client{Transport: tr, Timeout: 10 * time.Minute}).Do(req)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	buf := make([]byte, BufSize)
	var got int64
	for {
		n, err := resp.Body.Read(buf)
		got += int64(n)
		if err == io.EOF {
			break
		}
		if err != nil {
			return Result{}, fmt.Errorf("GET %s: %w after %d bytes", url, err, got)
		}
	}
	el := time.Since(t0)
	return Result{Name: "url", Bytes: got, Seconds: el.Seconds(), Gbit: gbit(got, el)}, nil
}

func dial(ctx context.Context, addr string) (net.Conn, error) {
	d := net.Dialer{Timeout: 10 * time.Second}
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
	return c, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
