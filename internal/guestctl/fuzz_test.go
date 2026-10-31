package guestctl

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"math"
	"net"
	"reflect"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/Higangssh/hyperbutler/internal/spaceguard"
)

// fuzzSeedFrames returns one valid frame for every message type, plus edge values.
func fuzzSeedFrames() [][]byte {
	msgs := append(sampleMsgs(),
		Hello{Min: 1, Max: math.MaxUint16},
		Hello{Min: 2, Max: 3},
		Budget{},
		Status{Guard: GuardStale, GuardErrno: MaxErrno},
		Status{Guard: GuardNoBudget, Device: math.MaxUint64},
		TrimResult{Errno: MaxErrno, Duration: time.Duration(maxDurationMS) * time.Millisecond},
		CacheResult{Errno: 12, Method: CacheDrop, Duration: time.Duration(maxDurationMS) * time.Millisecond},
		ErrorReply{Code: ErrUnsupported, Request: TCacheTrim},
		ErrorReply{Code: ErrBusy, Request: TPing},
		ErrorReply{Code: ErrBusy, Request: TBudget},
		HelloReply{Version: 1},
		Ping{Nonce: math.MaxUint64},
		DaemonChunk{Offset: 0, Data: []byte(`{"debug":true}`)},
		DaemonChunk{Offset: MaxDaemonConfig - DaemonChunkData, Data: bytes.Repeat([]byte{'x'}, DaemonChunkData)},
		DaemonCommit{Size: 14, Flags: DaemonRestart, SHA256: sha256.Sum256([]byte(`{"debug":true}`))},
		DaemonCommit{},
		DaemonResult{Code: DaemonRejected, Flags: DaemonChanged, Duration: time.Second, Message: "unknown key"},
		DaemonResult{Code: DaemonOK, Flags: DaemonChanged | DaemonRestarted, Message: string(bytes.Repeat([]byte{'m'}, MaxDaemonMessage))},
		ErrorReply{Code: ErrBusy, Request: TDaemonCommit},
	)
	var out [][]byte
	for i, m := range msgs {
		f, err := Encode(uint32(i)*0x01010101, m)
		if err != nil {
			panic(err)
		}
		out = append(out, f)
	}
	return out
}

func lenPrefix(n int) []byte {
	var b [4]byte
	be.PutUint32(b[:], uint32(n))
	return b[:]
}

// FuzzDecode checks that Decode is strict and canonical in both directions.
func FuzzDecode(f *testing.F) {
	for _, fr := range fuzzSeedFrames() {
		f.Add(fr[4:])
		// Edge cases around each valid body.
		f.Add(fr[4 : len(fr)-1])
		f.Add(append(bytes.Clone(fr[4:]), 0))
		b := bytes.Clone(fr[4:])
		b[3] = 1
		f.Add(b)
	}
	f.Add([]byte{})
	f.Add([]byte{1})
	f.Add(make([]byte, HeaderLen))
	f.Add(make([]byte, MaxBody+1))
	f.Fuzz(func(t *testing.T, body []byte) {
		decoded := 0
		for _, dir := range []Direction{FromHost, FromGuest} {
			seq, m, err := Decode(body, dir)
			if err != nil {
				if m != nil {
					t.Fatalf("error %v with message %#v", err, m)
				}
				continue
			}
			decoded++
			if len(body) > MaxBody || len(body) < HeaderLen {
				t.Fatalf("decoded a %d-byte body", len(body))
			}
			if m == nil {
				t.Fatal("nil message without error")
			}
			if (m.Type() >= 0x80) != (dir == FromGuest) {
				t.Fatalf("%v decoded from direction %d", m.Type(), dir)
			}
			enc, err := Encode(seq, m)
			if err != nil {
				t.Fatalf("decoded %#v does not re-encode: %v", m, err)
			}
			want := append(lenPrefix(len(body)), body...)
			if !bytes.Equal(enc, want) {
				t.Fatalf("not canonical:\n got %x\nwant %x\nmsg %#v", enc, want, m)
			}
			seq2, m2, err := Decode(enc[4:], dir)
			if err != nil || seq2 != seq || !reflect.DeepEqual(m2, m) {
				t.Fatalf("round trip: %d %#v %v, want %d %#v", seq2, m2, err, seq, m)
			}
		}
		if decoded > 1 {
			t.Fatalf("body %x decodes in both directions", body)
		}
	})
}

// FuzzReadFrame checks framing on arbitrary streams.
func FuzzReadFrame(f *testing.F) {
	var all []byte
	for _, fr := range fuzzSeedFrames() {
		f.Add(fr)
		f.Add(fr[:len(fr)-1])
		all = append(all, fr...)
	}
	f.Add(all)
	f.Add([]byte{})
	f.Add([]byte{0, 0, 0})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff})
	f.Add(append(lenPrefix(MaxBody), make([]byte, MaxBody)...))
	f.Add(append(lenPrefix(MaxBody+1), make([]byte, MaxBody+1)...))
	f.Add(append(lenPrefix(HeaderLen-1), make([]byte, HeaderLen)...))
	f.Fuzz(func(t *testing.T, data []byte) {
		var buf, buf2 [MaxBody]byte
		r := bytes.NewReader(data)
		r2 := iotest.OneByteReader(bytes.NewReader(data))
		off := 0
		// Every successful frame consumes at least 4+HeaderLen bytes, so this loop is
		// bounded; the cap catches a regression that consumes nothing.
		for i := 0; i <= len(data)/(4+HeaderLen)+1; i++ {
			before := r.Len()
			body, err := ReadFrame(r, &buf)
			body2, err2 := ReadFrame(r2, &buf2)
			if (err == nil) != (err2 == nil) || !bytes.Equal(body, body2) {
				t.Fatalf("one-byte reader differs: %v %v", err, err2)
			}
			if err != nil {
				if body != nil {
					t.Fatalf("body with error %v", err)
				}
				if errors.Is(err, io.EOF) && before != 0 {
					t.Fatalf("io.EOF with %d bytes left", before)
				}
				if errors.Is(err, ErrFrameTooLarge) {
					n := be.Uint32(data[off:])
					if n >= HeaderLen && n <= MaxBody {
						t.Fatalf("rejected valid length %d", n)
					}
				}
				return
			}
			n := len(body)
			if n > MaxBody || n < HeaderLen {
				t.Fatalf("body length %d", n)
			}
			if got := before - r.Len(); got != 4+n {
				t.Fatalf("consumed %d bytes for a %d-byte body", got, n)
			}
			if int(be.Uint32(data[off:])) != n || !bytes.Equal(body, data[off+4:off+4+n]) {
				t.Fatal("body does not match the stream")
			}
			off += 4 + n
		}
		t.Fatal("ReadFrame loop did not terminate")
	})
}

// memConn is a net.Conn whose reads come from a fixed input and whose writes are
// collected. When the input is exhausted it either returns io.EOF or, if hang is set,
// blocks until the read deadline passes or the conn is closed.
type memConn struct {
	r    *bytes.Reader
	hang bool

	mu       sync.Mutex
	out      bytes.Buffer
	deadline time.Time
	closed   chan struct{}
	once     sync.Once
}

func newMemConn(in []byte, hang bool) *memConn {
	return &memConn{r: bytes.NewReader(in), hang: hang, closed: make(chan struct{})}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func (c *memConn) Read(p []byte) (int, error) {
	for {
		select {
		case <-c.closed:
			return 0, net.ErrClosed
		default:
		}
		c.mu.Lock()
		dl := c.deadline
		if !dl.IsZero() && !time.Now().Before(dl) {
			c.mu.Unlock()
			return 0, timeoutErr{}
		}
		if c.r.Len() > 0 || !c.hang {
			n, err := c.r.Read(p)
			c.mu.Unlock()
			return n, err
		}
		c.mu.Unlock()
		select {
		case <-c.closed:
		case <-time.After(time.Millisecond):
		}
	}
}

func (c *memConn) Write(p []byte) (int, error) {
	select {
	case <-c.closed:
		return 0, net.ErrClosed
	default:
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.out.Write(p)
	return len(p), nil
}

func (c *memConn) Close() error { c.once.Do(func() { close(c.closed) }); return nil }
func (c *memConn) output() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return bytes.Clone(c.out.Bytes())
}
func (c *memConn) LocalAddr() net.Addr  { return fuzzAddr{} }
func (c *memConn) RemoteAddr() net.Addr { return fuzzAddr{} }
func (c *memConn) SetDeadline(t time.Time) error {
	return c.SetReadDeadline(t)
}
func (c *memConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadline = t
	c.mu.Unlock()
	return nil
}
func (c *memConn) SetWriteDeadline(time.Time) error { return nil }

type fuzzAddr struct{}

func (fuzzAddr) Network() string { return "fuzz" }
func (fuzzAddr) String() string  { return "fuzz" }

// recHandler records Handler calls. Its results come from fuzz input and may be invalid,
// which the server must refuse to send.
type recHandler struct {
	mu                          sync.Mutex
	budgets, lost, trims, cache int
	daemons                     int
	daemonData                  [][]byte
	status                      Status
	trim                        TrimResult
	cacheRes                    CacheResult
	daemonRes                   DaemonResult
}

func (h *recHandler) ApplyDaemonConfig(_ context.Context, data []byte, _ bool) DaemonResult {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.daemons++
	h.daemonData = append(h.daemonData, data)
	return h.daemonRes
}

func (h *recHandler) SetBudget(spaceguard.Budget) { h.mu.Lock(); h.budgets++; h.mu.Unlock() }
func (h *recHandler) ConnLost()                   { h.mu.Lock(); h.lost++; h.mu.Unlock() }
func (h *recHandler) Trim(context.Context) TrimResult {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.trims++
	return h.trim
}
func (h *recHandler) CacheTrim(context.Context) CacheResult {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cache++
	return h.cacheRes
}
func (h *recHandler) Status() Status { return h.status }

// FuzzServeConn feeds an arbitrary host stream to the guest side.
func FuzzServeConn(f *testing.F) {
	frames := fuzzSeedFrames()
	hello, _ := Encode(1, Hello{Min: 1, Max: 1})
	var hostOnly []byte
	hostOnly = append(hostOnly, hello...)
	for _, fr := range frames {
		if fr[5] < 0x80 && Type(fr[5]) != THello {
			hostOnly = append(hostOnly, fr...)
		}
	}
	trim, _ := Encode(9, TrimRequest{})
	cache, _ := Encode(10, CacheTrimRequest{})
	ping, _ := Encode(11, Ping{Nonce: 5})
	budget, _ := Encode(12, Budget{spaceguard.Budget{Bytes: 1}})
	badHello, _ := Encode(1, Hello{Min: 2, Max: 3})
	cfg := []byte(`{"insecure-registries":["192.168.127.2:5000"]}`)
	chunk, _ := Encode(13, DaemonChunk{Data: cfg})
	commit, _ := Encode(14, DaemonCommit{Size: uint32(len(cfg)), Flags: DaemonRestart, SHA256: sha256.Sum256(cfg)})
	badCommit, _ := Encode(15, DaemonCommit{Size: uint32(len(cfg))})
	gap, _ := Encode(16, DaemonChunk{Offset: 9, Data: cfg})
	remove, _ := Encode(17, DaemonCommit{SHA256: sha256.Sum256(nil)})
	transfer := append(append(bytes.Clone(hello), chunk...), commit...)
	f.Add(transfer, byte(FeatureDaemonConfig), byte(0))
	f.Add(append(bytes.Clone(transfer), commit...), byte(FeatureDaemonConfig), byte(0))
	f.Add(append(append(append(bytes.Clone(hello), chunk...), badCommit...), remove...), byte(FeatureDaemonConfig), byte(0))
	f.Add(append(append(bytes.Clone(hello), chunk...), gap...), byte(FeatureDaemonConfig), byte(0))
	f.Add(append(append(bytes.Clone(hello), chunk...), remove...), byte(0), byte(0))
	f.Add(hostOnly, byte(0), byte(0))
	f.Add(hostOnly, byte(7), byte(1))
	f.Add(append(append(bytes.Clone(hello), trim...), trim...), byte(7), byte(0))
	f.Add(append(append(append(bytes.Clone(hello), cache...), ping...), budget...), byte(6), byte(2))
	f.Add(append(bytes.Clone(hello), hello...), byte(7), byte(0))
	f.Add(badHello, byte(7), byte(0))
	f.Add(budget, byte(7), byte(0))
	f.Add(hello, byte(7), byte(0xff))
	f.Add([]byte{}, byte(0), byte(1))
	f.Fuzz(func(t *testing.T, in []byte, features, knobs byte) {
		hang := knobs&1 != 0
		h := &recHandler{
			status:    Status{Guard: GuardState(knobs >> 1 & 7), GuardErrno: uint32(knobs>>4) * 300},
			trim:      TrimResult{Errno: uint32(knobs) * 20, Duration: time.Millisecond},
			cacheRes:  CacheResult{Method: CacheMethod(knobs >> 6), Duration: time.Millisecond},
			daemonRes: DaemonResult{Code: DaemonCode(knobs >> 5), Flags: DaemonResultFlags(knobs & 3)},
		}
		srv := &Server{
			Handler:      h,
			Features:     Features(features),
			Instance:     7,
			StatusEvery:  2 * time.Millisecond,
			HelloTimeout: 5 * time.Millisecond,
			WriteTimeout: 100 * time.Millisecond,
		}
		conn := newMemConn(in, hang)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		done := make(chan error, 1)
		start := time.Now()
		go func() { done <- srv.ServeConn(ctx, conn) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("ServeConn did not return")
		}
		elapsed := time.Since(start)

		h.mu.Lock()
		lost, budgets, trims, caches, daemons := h.lost, h.budgets, h.trims, h.cache, h.daemons
		daemonData := h.daemonData
		h.mu.Unlock()
		if lost != 1 {
			t.Fatalf("ConnLost called %d times", lost)
		}

		// Parse what the host sent, the way the server reads it.
		var buf [MaxBody]byte
		r := bytes.NewReader(in)
		reqSeqs := map[uint32]map[Type]bool{}
		// commits are the hashes of the daemon.json commits; every file the handler got
		// must match one of them.
		commits := map[[32]byte]bool{}
		helloOK := false
		var helloSeq uint32
		nReq := 0
		for i := 0; ; i++ {
			body, err := ReadFrame(r, &buf)
			if err != nil {
				break
			}
			seq, m, err := Decode(body, FromHost)
			if err != nil {
				break
			}
			if i == 0 {
				hl, ok := m.(Hello)
				if !ok || hl.Min > Version || hl.Max < Version {
					break
				}
				helloOK, helloSeq = true, seq
				continue
			}
			if m.Type() == THello {
				break
			}
			nReq++
			if c, ok := m.(DaemonCommit); ok {
				commits[c.SHA256] = true
			}
			if m.Type() != TBudget {
				if reqSeqs[seq] == nil {
					reqSeqs[seq] = map[Type]bool{}
				}
				reqSeqs[seq][m.Type()] = true
			}
		}
		if !helloOK && (budgets != 0 || trims != 0 || caches != 0) {
			t.Fatalf("handler called without a valid hello: budgets %d trims %d caches %d", budgets, trims, caches)
		}
		if budgets+trims+caches+daemons > nReq {
			t.Fatalf("%d handler calls for %d requests", budgets+trims+caches+daemons, nReq)
		}
		if daemons > 0 && features&byte(FeatureDaemonConfig) == 0 {
			t.Fatal("daemon.json applied without FeatureDaemonConfig")
		}
		for _, d := range daemonData {
			if len(d) > MaxDaemonConfig || !commits[sha256.Sum256(d)] {
				t.Fatalf("handler got %d bytes that match no commit", len(d))
			}
		}

		out := conn.output()
		if !helloOK && len(out) != 0 {
			t.Fatalf("server wrote %d bytes without a valid hello", len(out))
		}
		or := bytes.NewReader(out)
		replies, statuses := 0, 0
		for first := true; or.Len() > 0; first = false {
			body, err := ReadFrame(or, &buf)
			if err != nil {
				t.Fatalf("server wrote a bad frame: %v", err)
			}
			seq, m, err := Decode(body, FromGuest)
			if err != nil {
				t.Fatalf("server frame does not decode: %v (%x)", err, body)
			}
			if first {
				hr, ok := m.(HelloReply)
				if !ok || seq != helloSeq || hr.Features&^knownFeatures != 0 {
					t.Fatalf("first frame %#v seq %d, want hello reply seq %d", m, seq, helloSeq)
				}
				continue
			}
			switch v := m.(type) {
			case Status:
				statuses++
				if v != h.status {
					t.Fatalf("status %#v, want %#v", v, h.status)
				}
			case HelloReply:
				t.Fatal("second hello reply")
			default:
				replies++
				types, ok := reqSeqs[seq]
				if !ok {
					t.Fatalf("%v with seq %d answers no request", m.Type(), seq)
				}
				req := map[Type]Type{RTrim: TTrim, RCache: TCacheTrim, RPong: TPing, RDaemon: TDaemonCommit, RShutdown: TShutdown}[m.Type()]
				if e, isErr := m.(ErrorReply); isErr {
					req = e.Request
				}
				if !types[req] {
					t.Fatalf("%v answers seq %d, which carried %v", m, seq, types)
				}
			}
		}
		if replies > nReq {
			t.Fatalf("%d replies for %d requests", replies, nReq)
		}
		// One status per period at most, plus the one at start and some slack.
		if maxStatus := int(elapsed/srv.StatusEvery) + 3; statuses > maxStatus {
			t.Fatalf("%d statuses in %v", statuses, elapsed)
		}
	})
}

// fakeGuest scripts a guest from fuzz input. The script is a list of ops:
//
//	0: raw    n=next byte, write the next n bytes as they are
//	1: reply  wait briefly for the next host frame and answer it. The next byte k picks
//	          the reply: bit 0x08 clear = the type that answers the host frame, else
//	          type k&7; bit 0x10 = do not copy the seq; bit 0x20 = do not sanitize the
//	          payload (or copy a ping's nonce); bit 0x40 = take the payload from the
//	          following script bytes (else zero)
//	2: close  close the connection
//	3: pause  sleep next byte %32 ms (requests time out after 15 ms)
type fakeGuest struct {
	conn   net.Conn
	frames chan []byte
}

func (g *fakeGuest) drain() {
	defer close(g.frames)
	for {
		var buf [MaxBody]byte
		body, err := ReadFrame(g.conn, &buf)
		if err != nil {
			return
		}
		select {
		case g.frames <- bytes.Clone(body):
		default: // not interesting enough to keep; never block the host
		}
	}
}

var replyTypes = [...]Type{RTrim, RCache, RPong, RError, RStatus, RHello, RDaemon}

func (g *fakeGuest) run(script []byte) (closed bool) {
	next := func() (byte, bool) {
		if len(script) == 0 {
			return 0, false
		}
		b := script[0]
		script = script[1:]
		return b, true
	}
	for {
		op, ok := next()
		if !ok {
			return false
		}
		switch op % 4 {
		case 0:
			n, _ := next()
			k := min(int(n), len(script))
			if _, err := g.conn.Write(script[:k]); err != nil {
				return false
			}
			script = script[k:]
		case 1:
			k, _ := next()
			var host []byte
			for {
				select {
				case host = <-g.frames:
				case <-time.After(5 * time.Millisecond):
				}
				// In auto mode budgets need no answer: answer the next request instead.
				if k&0x08 != 0 || len(host) < 2 || Type(host[1]) != TBudget {
					break
				}
			}
			typ := replyTypes[int(k&0x7)%len(replyTypes)]
			if k&0x08 == 0 && len(host) >= HeaderLen {
				// Answer with the type the host frame asks for.
				if want, ok := map[Type]Type{TTrim: RTrim, TCacheTrim: RCache, TPing: RPong, TDaemonCommit: RDaemon}[Type(host[1])]; ok {
					typ = want
				}
			}
			size := map[Type]int{RTrim: 24, RCache: 32, RPong: 8, RError: 8, RStatus: statusPayloadSize, RHello: 16, RDaemon: 16 + MaxDaemonMessage}[typ]
			body := make([]byte, HeaderLen+size)
			body[0] = Version
			body[1] = byte(typ)
			if len(host) >= HeaderLen && k&0x10 == 0 {
				copy(body[4:8], host[4:8])
			}
			p := body[HeaderLen:]
			if k&0x40 != 0 {
				m := min(size, len(script))
				copy(p, script[:m])
				script = script[m:]
			}
			if k&0x20 == 0 {
				// Make the payload valid-ish so that matched replies are reached.
				switch typ {
				case RTrim, RCache, RError:
					p[0], p[1], p[2] = 0, 0, 0
					clear(p[5:8])
					if typ == RError {
						p[3] = 1 + p[3]%2
						p[4] = byte(TTrim) + p[4]%3
					} else {
						p[3] = 0
						if typ == RCache {
							p[4] %= 3
						} else {
							p[4] = 0
						}
						p[len(p)-8] = 0
					}
				case RPong:
					if len(host) == HeaderLen+8 && Type(host[1]) == TPing {
						copy(p, host[HeaderLen:])
					}
				case RStatus:
					p[0] %= 4
					clear(p[1:6])
				case RDaemon:
					clear(p[0:3])
					p[3] %= 5
					p[4] %= 4
					p[5], p[6], p[7] = 0, 0, p[7]%8
					p[8] = 0
					clear(p[16+int(p[7]):])
				}
			}
			if _, err := g.conn.Write(append(lenPrefix(len(body)), body...)); err != nil {
				return false
			}
		case 2:
			g.conn.Close()
			return true
		case 3:
			n, _ := next()
			time.Sleep(time.Duration(n%32) * time.Millisecond)
		}
	}
}

// FuzzClient runs the host side against a scripted, hostile guest.
func FuzzClient(f *testing.F) {
	hr, _ := Encode(0, HelloReply{Version: 1, Features: knownFeatures, Instance: 3})
	hrSeq, _ := Encode(1, HelloReply{Version: 1})
	st, _ := Encode(1, Status{Guard: GuardOK, FSSize: 5})
	pong, _ := Encode(5, Pong{Nonce: 1})
	raw := func(fr []byte) []byte { return append([]byte{0, byte(len(fr))}, fr...) }
	f.Add(hr, []byte{}, byte(0))
	f.Add(hr, []byte{1, 0, 1, 1, 1, 2, 1, 3}, byte(0))
	f.Add(hr, []byte{1, 0, 1, 1, 1, 2, 2}, byte(1))
	f.Add(hr, append(raw(st), 1, 3, 1, 0x13, 3, 3, 2), byte(2))
	f.Add(hr, append(raw(st), raw(st)...), byte(3))
	f.Add(hr, raw(pong), byte(0))
	f.Add(hr, raw(hr), byte(0))
	f.Add(hr, []byte{1, 0x20, 1, 0x30, 1, 0x05, 1, 0x04}, byte(0))
	f.Add(hr, []byte{1, 0, 1, 0, 1, 0, 1, 0}, byte(0x0c))
	f.Add(hr, []byte{1, 0, 1, 0, 1, 0, 1, 0, 2}, byte(0x2e))
	f.Add(hr, []byte{3, 20, 1, 0, 1, 0, 1, 0, 1, 0}, byte(0x0c))
	f.Add(hr, []byte{1, 0x0b, 1, 0x0b, 1, 0x0b, 1, 0x0b}, byte(0x0c))
	f.Add(hr, []byte{1, 0x30, 1, 0x00, 1, 0x00, 1, 0x00}, byte(0x0c))
	f.Add(hr, append([]byte{1, 0x40}, bytes.Repeat([]byte{0xff}, 40)...), byte(0x0c))
	f.Add(hrSeq, []byte{}, byte(0))
	f.Add(st, []byte{}, byte(0))
	f.Add([]byte{0xff, 0xff, 0xff, 0xff}, []byte{}, byte(0))
	f.Add(hr[:5], []byte{}, byte(0))
	for _, fr := range fuzzSeedFrames() {
		if fr[5] >= 0x80 {
			f.Add(hr, append(raw(fr), 1, fr[5]-byte(RTrim)), byte(0))
		}
	}
	f.Fuzz(func(t *testing.T, handshake, script []byte, knobs byte) {
		a, b := net.Pipe()
		g := &fakeGuest{conn: b, frames: make(chan []byte, 16)}
		guestDone := make(chan struct{})
		go func() {
			defer close(guestDone)
			var buf [MaxBody]byte
			if _, err := ReadFrame(b, &buf); err != nil {
				b.Close()
				return
			}
			go g.drain()
			if _, err := b.Write(handshake); err != nil {
				return
			}
			if !g.run(script) && knobs&1 != 0 {
				b.Close()
			}
		}()
		defer func() {
			b.Close()
			<-guestDone
		}()

		var statuses []Status
		var stMu sync.Mutex
		cfg := ClientConfig{
			HelloTimeout: 20 * time.Millisecond,
			WriteTimeout: 20 * time.Millisecond,
			ReadTimeout:  20 * time.Millisecond,
			FrameRate:    1000,
			FrameBurst:   float64(knobs>>4) + 1,
			MaxInflight:  int(knobs>>2&3) + 1,
			OnStatus: func(s Status) {
				stMu.Lock()
				statuses = append(statuses, s)
				stMu.Unlock()
			},
		}
		c, err := NewClient(context.Background(), a, cfg)
		if err != nil {
			if c != nil {
				t.Fatal("client with error")
			}
			// The conn must be closed.
			_ = a.SetWriteDeadline(time.Now().Add(10 * time.Millisecond))
			if _, werr := a.Write([]byte{0}); werr == nil {
				t.Fatalf("conn left open after %v", err)
			}
			return
		}
		h := c.Hello()
		if _, err := Encode(0, h); err != nil {
			t.Fatalf("accepted bad hello %#v: %v", h, err)
		}

		var wg sync.WaitGroup
		var reqErrs [4]error
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
		defer cancel()
		wg.Add(4)
		go func() {
			defer wg.Done()
			var r DaemonResult
			r, reqErrs[3] = c.DaemonConfig(ctx, bytes.Repeat([]byte{'{'}, int(knobs)), knobs&4 != 0)
			if reqErrs[3] == nil {
				if _, err := Encode(1, r); err != nil {
					t.Errorf("DaemonConfig returned invalid %#v", r)
				}
			}
		}()
		go func() { defer wg.Done(); _, reqErrs[0] = c.Trim(ctx) }()
		go func() { defer wg.Done(); _, reqErrs[1] = c.CacheTrim(ctx) }()
		go func() { defer wg.Done(); _, reqErrs[2] = c.Ping(ctx) }()
		c.SetBudget(spaceguard.Budget{Bytes: uint64(knobs)})
		c.SetBudget(spaceguard.Budget{Unlimited: true})

		reqDone := make(chan struct{})
		go func() { wg.Wait(); close(reqDone) }()
		select {
		case <-reqDone:
		case <-time.After(5 * time.Second):
			t.Fatal("requests did not return")
		}
		for i, err := range reqErrs {
			var re *RefusedError
			if errors.As(err, &re) && (re.Code == 0 || re.Code > maxErrorCode) {
				t.Fatalf("request %d refused with code %d", i, re.Code)
			}
		}

		if knobs&2 != 0 {
			c.Close()
		}
		select {
		case <-c.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("client did not shut down after the guest went quiet")
		}
		if c.Err() == nil {
			t.Fatal("Done closed with nil Err")
		}
		c.Close()
		c.Close()
		if _, err := c.Trim(context.Background()); err == nil {
			t.Fatal("trim on a closed client succeeded")
		}
		c.SetBudget(spaceguard.Budget{})

		stMu.Lock()
		defer stMu.Unlock()
		for _, s := range statuses {
			enc, err := Encode(1, s)
			if err != nil {
				t.Fatalf("OnStatus got invalid %#v: %v", s, err)
			}
			_, m, err := Decode(enc[4:], FromGuest)
			if err != nil || m != s {
				t.Fatalf("status does not round-trip: %#v %v", m, err)
			}
		}
		if last, _ := c.LastStatus(); len(statuses) > 0 && last != statuses[len(statuses)-1] {
			t.Fatalf("LastStatus %#v, last OnStatus %#v", last, statuses[len(statuses)-1])
		}
	})
}
