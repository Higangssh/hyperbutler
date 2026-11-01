package guestctl

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/Higangssh/hyperbutler/internal/spaceguard"
)

// ClientConfig bounds what the host accepts from the guest. Zero values pick the
// defaults in parentheses.
type ClientConfig struct {
	// HelloTimeout bounds the handshake (5 s).
	HelloTimeout time.Duration
	// WriteTimeout bounds each frame write (5 s). A guest that stops reading loses the
	// connection instead of blocking the host.
	WriteTimeout time.Duration
	// ReadTimeout closes the connection when the guest sends nothing for this long
	// (10 s). The guest reports status every second, so silence means it is dead or
	// stuck.
	ReadTimeout time.Duration
	// FrameRate and FrameBurst are a token bucket on guest frames (20/s, burst 40). A
	// guest that sends faster is cut off: it cannot make the host spend unbounded CPU.
	FrameRate  float64
	FrameBurst float64
	// MaxInflight bounds outstanding requests (8).
	MaxInflight int
	// OnStatus, if set, is called from the reader goroutine with every status report.
	// It must not block and must not call Close (Close waits for the reader). The values
	// are guest-reported and untrusted.
	OnStatus func(Status)
}

func (c ClientConfig) withDefaults() ClientConfig {
	if c.HelloTimeout <= 0 {
		c.HelloTimeout = 5 * time.Second
	}
	if c.WriteTimeout <= 0 {
		c.WriteTimeout = 5 * time.Second
	}
	if c.ReadTimeout <= 0 {
		c.ReadTimeout = 10 * time.Second
	}
	if c.FrameRate <= 0 {
		c.FrameRate = 20
	}
	if c.FrameBurst < 1 {
		c.FrameBurst = 40
	}
	if c.MaxInflight <= 0 {
		c.MaxInflight = 8
	}
	return c
}

// maxAbandoned bounds the remembered abandoned requests.
const maxAbandoned = 64

// ErrViolation is the error a client closes with when the guest breaks the protocol.
var ErrViolation = errors.New("guestctl: guest protocol violation")

// Client is the host end of one control connection.
type Client struct {
	conn  net.Conn
	cfg   ClientConfig
	hello HelloReply

	mu      sync.Mutex
	seq     uint32
	pending map[uint32]*call
	// abandoned are requests whose caller gave up. A late reply to one is dropped, not
	// treated as a violation. Bounded: the oldest is forgotten first.
	abandoned []uint32
	budget    *spaceguard.Budget // newest unsent budget
	status    Status
	statusT   time.Time
	err       error

	// dl guards suspended and the read deadline, so Suspend and the reader never
	// leave a stale deadline behind.
	dl        sync.Mutex
	suspended bool

	daemonMu chan struct{} // one daemon.json transfer at a time
	envMu    chan struct{} // one dockerd environment transfer at a time

	wake   chan struct{} // the writer has work
	reqs   chan []byte
	closed chan struct{}
	done   chan struct{} // reader and writer exited
	once   sync.Once
}

type call struct {
	want Type
	// nonce is checked for pings.
	nonce uint64
	reply chan Msg
}

// NewClient runs the handshake on conn and starts the client. On error conn is closed.
func NewClient(ctx context.Context, conn net.Conn, cfg ClientConfig) (*Client, error) {
	cfg = cfg.withDefaults()
	c := &Client{
		conn: conn, cfg: cfg, pending: make(map[uint32]*call),
		wake: make(chan struct{}, 1), reqs: make(chan []byte, cfg.MaxInflight),
		closed: make(chan struct{}), done: make(chan struct{}),
		daemonMu: make(chan struct{}, 1), envMu: make(chan struct{}, 1),
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()
	f, _ := Encode(0, Hello{Min: Version, Max: Version})
	_ = conn.SetDeadline(time.Now().Add(cfg.HelloTimeout))
	if _, err := conn.Write(f); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("guestctl: send hello: %w", err)
	}
	var buf [MaxBody]byte
	body, err := ReadFrame(conn, &buf)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("guestctl: read hello reply: %w", err)
	}
	seq, m, err := Decode(body, FromGuest)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: %w", ErrViolation, err)
	}
	h, ok := m.(HelloReply)
	if !ok || seq != 0 {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: expected hello reply, got %v seq %d", ErrViolation, m.Type(), seq)
	}
	if !stop() || ctx.Err() != nil {
		_ = conn.Close()
		return nil, context.Cause(ctx)
	}
	_ = conn.SetDeadline(time.Time{})
	c.hello = h
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); c.reader() }()
	go func() { defer wg.Done(); c.writer() }()
	go func() { wg.Wait(); close(c.done) }()
	return c, nil
}

// Hello returns the guest's handshake reply.
func (c *Client) Hello() HelloReply { return c.hello }

// Done is closed when the client has shut down.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err is why the client shut down, once Done is closed.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Close shuts the client down.
func (c *Client) Close() error {
	c.fail(ErrClosed)
	<-c.done
	return nil
}

func (c *Client) fail(err error) {
	c.once.Do(func() {
		c.mu.Lock()
		c.err = err
		pend := c.pending
		c.pending = nil
		c.mu.Unlock()
		close(c.closed)
		_ = c.conn.Close()
		for _, p := range pend {
			close(p.reply)
		}
	})
}

// Suspend turns the read timeout off (true) or back on (false). The host calls it
// around HcsPauseComputeSystem: a paused guest sends nothing, and that silence must not
// tear the connection down, or the guest's guard fails closed on resume.
func (c *Client) Suspend(on bool) {
	c.dl.Lock()
	defer c.dl.Unlock()
	c.suspended = on
	if on {
		_ = c.conn.SetReadDeadline(time.Time{})
	} else {
		_ = c.conn.SetReadDeadline(time.Now().Add(c.cfg.ReadTimeout))
	}
}

// LastStatus returns the newest status report and when it arrived (zero if none yet).
// The values are guest-reported and untrusted.
func (c *Client) LastStatus() (Status, time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status, c.statusT
}

// SetBudget queues b for the guest. It never blocks: if the previous budget has not been
// written yet, b replaces it.
func (c *Client) SetBudget(b spaceguard.Budget) {
	c.mu.Lock()
	if c.pending == nil { // closed
		c.mu.Unlock()
		return
	}
	c.budget = &b
	c.mu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// Trim asks the guest to FITRIM its data file system and waits for the result.
func (c *Client) Trim(ctx context.Context) (TrimResult, error) {
	m, err := c.request(ctx, TrimRequest{}, RTrim, 0)
	if err != nil {
		return TrimResult{}, err
	}
	return m.(TrimResult), nil
}

// CacheTrim asks the guest to reclaim page cache now and waits for the result.
func (c *Client) CacheTrim(ctx context.Context) (CacheResult, error) {
	m, err := c.request(ctx, CacheTrimRequest{}, RCache, 0)
	if err != nil {
		return CacheResult{}, err
	}
	return m.(CacheResult), nil
}

// Shutdown asks the guest to power off in order and waits until it accepts. The VM
// stops later, when the guest is done.
func (c *Client) Shutdown(ctx context.Context) error {
	_, err := c.request(ctx, ShutdownRequest{}, RShutdown, 0)
	return err
}

// DaemonConfig sends data as the guest's daemon.json (empty: remove it) and waits for
// the guest's verdict. With restart the guest restarts dockerd, which can take as long
// as dockerd needs to stop its containers and start again; ctx bounds the wait. Calls
// are serialized: one transfer is on the wire at a time.
func (c *Client) DaemonConfig(ctx context.Context, data []byte, restart bool) (DaemonResult, error) {
	if len(data) > MaxDaemonConfig {
		return DaemonResult{}, fmt.Errorf("guestctl: daemon.json of %d bytes is over the %d byte limit", len(data), MaxDaemonConfig)
	}
	if c.hello.Features&FeatureDaemonConfig == 0 {
		return DaemonResult{}, &RefusedError{Code: ErrUnsupported}
	}
	select {
	case c.daemonMu <- struct{}{}:
	case <-ctx.Done():
		return DaemonResult{}, ctx.Err()
	}
	defer func() { <-c.daemonMu }()
	for off := 0; off < len(data); off += DaemonChunkData {
		end := min(off+DaemonChunkData, len(data))
		if err := c.post(ctx, DaemonChunk{Offset: uint32(off), Data: data[off:end]}); err != nil {
			return DaemonResult{}, err
		}
	}
	commit := DaemonCommit{Size: uint32(len(data)), SHA256: sha256.Sum256(data)} //nolint:gosec // G115: len(data) <= MaxDaemonConfig, checked on entry
	if restart {
		commit.Flags |= DaemonRestart
	}
	m, err := c.request(ctx, commit, RDaemon, 0)
	if err != nil {
		return DaemonResult{}, err
	}
	return m.(DaemonResult), nil
}

// DockerdEnv sends data as dockerd's environment (an encoded dockerenv.Doc; empty: none)
// and waits for the guest's verdict, like DaemonConfig. The guest restarts dockerd when
// the environment changed while dockerd runs, and with restart in any case.
func (c *Client) DockerdEnv(ctx context.Context, data []byte, restart bool) (EnvResult, error) {
	if len(data) > MaxDockerdEnv {
		return EnvResult{}, fmt.Errorf("guestctl: dockerd environment of %d bytes is over the %d byte limit", len(data), MaxDockerdEnv)
	}
	if c.hello.Features&FeatureDockerdEnv == 0 {
		return EnvResult{}, &RefusedError{Code: ErrUnsupported}
	}
	select {
	case c.envMu <- struct{}{}:
	case <-ctx.Done():
		return EnvResult{}, ctx.Err()
	}
	defer func() { <-c.envMu }()
	for off := 0; off < len(data); off += DaemonChunkData {
		end := min(off+DaemonChunkData, len(data))
		if err := c.post(ctx, EnvChunk{Offset: uint32(off), Data: data[off:end]}); err != nil {
			return EnvResult{}, err
		}
	}
	commit := EnvCommit{Size: uint32(len(data)), SHA256: sha256.Sum256(data)} //nolint:gosec // G115: len(data) <= MaxDockerdEnv, checked on entry
	if restart {
		commit.Flags |= DaemonRestart
	}
	m, err := c.request(ctx, commit, REnv, 0)
	if err != nil {
		return EnvResult{}, err
	}
	return m.(EnvResult), nil
}

// post queues a message that gets no reply, in order with requests.
func (c *Client) post(ctx context.Context, m Msg) error {
	c.mu.Lock()
	if c.pending == nil {
		err := c.err
		c.mu.Unlock()
		return err
	}
	c.seq++
	seq := c.seq
	c.mu.Unlock()
	f, err := Encode(seq, m)
	if err != nil {
		return err
	}
	select {
	case c.reqs <- f:
		return nil
	case <-c.closed:
		return c.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Ping measures the round trip to the guest daemon.
func (c *Client) Ping(ctx context.Context) (time.Duration, error) {
	t0 := time.Now()
	c.mu.Lock()
	nonce := uint64(t0.UnixNano()) ^ uint64(c.seq)<<32
	c.mu.Unlock()
	if _, err := c.request(ctx, Ping{Nonce: nonce}, RPong, nonce); err != nil {
		return 0, err
	}
	return time.Since(t0), nil
}

// RefusedError is returned when the guest answers a request with an error reply.
type RefusedError struct{ Code ErrorCode }

func (e *RefusedError) Error() string {
	switch e.Code {
	case ErrBusy:
		return "guestctl: guest is busy with the same request"
	case ErrUnsupported:
		return "guestctl: guest does not support this request"
	}
	return fmt.Sprintf("guestctl: guest refused the request (code %d)", e.Code)
}

func (c *Client) request(ctx context.Context, m Msg, want Type, nonce uint64) (Msg, error) {
	c.mu.Lock()
	if c.pending == nil {
		err := c.err
		c.mu.Unlock()
		return nil, err
	}
	if len(c.pending) >= c.cfg.MaxInflight {
		c.mu.Unlock()
		return nil, errors.New("guestctl: too many requests in flight")
	}
	c.seq++
	seq := c.seq
	p := &call{want: want, nonce: nonce, reply: make(chan Msg, 1)}
	c.pending[seq] = p
	c.mu.Unlock()
	forget := func() {
		c.mu.Lock()
		if c.pending != nil {
			delete(c.pending, seq)
		}
		c.mu.Unlock()
	}
	f, err := Encode(seq, m)
	if err != nil {
		forget()
		return nil, err
	}
	select {
	case c.reqs <- f: // cannot block for long: cap(reqs) == MaxInflight
	case <-c.closed:
		return nil, c.Err()
	case <-ctx.Done():
		forget()
		return nil, ctx.Err()
	}
	select {
	case r, ok := <-p.reply:
		if !ok {
			return nil, c.Err()
		}
		if e, isErr := r.(ErrorReply); isErr {
			return nil, &RefusedError{Code: e.Code}
		}
		return r, nil
	case <-ctx.Done():
		// The reply may still come. That is not a violation for a request we sent, so
		// remember the sequence number for a while.
		c.mu.Lock()
		if c.pending != nil {
			if _, still := c.pending[seq]; still {
				delete(c.pending, seq)
				if len(c.abandoned) >= maxAbandoned {
					c.abandoned = c.abandoned[1:]
				}
				c.abandoned = append(c.abandoned, seq)
			}
		}
		c.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (c *Client) writer() {
	write := func(f []byte) bool {
		_ = c.conn.SetWriteDeadline(time.Now().Add(c.cfg.WriteTimeout))
		if _, err := c.conn.Write(f); err != nil {
			c.fail(fmt.Errorf("guestctl: write: %w", err))
			return false
		}
		return true
	}
	for {
		select {
		case <-c.closed:
			return
		case f := <-c.reqs:
			if !write(f) {
				return
			}
		case <-c.wake:
			c.mu.Lock()
			b := c.budget
			c.budget = nil
			c.seq++
			seq := c.seq
			c.mu.Unlock()
			if b == nil {
				continue
			}
			f, err := Encode(seq, Budget{*b})
			if err != nil {
				c.fail(err) // a host bug: refuse to send a non-canonical budget
				return
			}
			if !write(f) {
				return
			}
		}
	}
}

func (c *Client) reader() {
	var buf [MaxBody]byte
	tokens, last := c.cfg.FrameBurst, time.Now()
	for {
		c.dl.Lock()
		if c.suspended {
			_ = c.conn.SetReadDeadline(time.Time{})
		} else {
			_ = c.conn.SetReadDeadline(time.Now().Add(c.cfg.ReadTimeout))
		}
		c.dl.Unlock()
		body, err := ReadFrame(c.conn, &buf)
		if err != nil {
			if errors.Is(err, ErrFrameTooLarge) {
				err = fmt.Errorf("%w: %w", ErrViolation, err)
			}
			c.fail(err)
			return
		}
		now := time.Now()
		tokens = min(c.cfg.FrameBurst, tokens+now.Sub(last).Seconds()*c.cfg.FrameRate)
		last = now
		if tokens < 1 {
			c.fail(fmt.Errorf("%w: more than %.0f frames/s", ErrViolation, c.cfg.FrameRate))
			return
		}
		tokens--
		if err := c.dispatch(body); err != nil {
			c.fail(err)
			return
		}
	}
}

// dispatch handles one guest frame. Any error closes the connection.
func (c *Client) dispatch(body []byte) error {
	seq, m, err := Decode(body, FromGuest)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrViolation, err)
	}
	if st, ok := m.(Status); ok {
		c.mu.Lock()
		c.status, c.statusT = st, time.Now()
		c.mu.Unlock()
		if c.cfg.OnStatus != nil {
			c.cfg.OnStatus(st)
		}
		return nil
	}
	if m.Type() == RHello {
		return fmt.Errorf("%w: second hello reply", ErrViolation)
	}
	c.mu.Lock()
	p, ok := c.pending[seq]
	if !ok {
		for i, a := range c.abandoned {
			if a == seq {
				c.abandoned = append(c.abandoned[:i], c.abandoned[i+1:]...)
				c.mu.Unlock()
				return nil
			}
		}
		c.mu.Unlock()
		return fmt.Errorf("%w: %v for seq %d, which is not pending", ErrViolation, m.Type(), seq)
	}
	match := m.Type() == p.want
	if e, isErr := m.(ErrorReply); isErr {
		match = e.Request == requestFor(p.want)
	}
	if pong, isPong := m.(Pong); isPong && pong.Nonce != p.nonce {
		match = false
	}
	if !match {
		c.mu.Unlock()
		return fmt.Errorf("%w: %v does not answer seq %d", ErrViolation, m.Type(), seq)
	}
	delete(c.pending, seq)
	c.mu.Unlock()
	p.reply <- m // buffered, and each call gets at most one reply
	return nil
}

func requestFor(reply Type) Type {
	switch reply {
	case RTrim:
		return TTrim
	case RCache:
		return TCacheTrim
	case RPong:
		return TPing
	case RShutdown:
		return TShutdown
	case RDaemon:
		return TDaemonCommit
	case REnv:
		return TEnvCommit
	}
	return 0
}
