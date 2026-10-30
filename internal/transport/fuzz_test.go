package transport

import (
	"bytes"
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

// FuzzParseHvsockTarget: never panics, accepted input round-trips through String, and
// String's output is always accepted.
func FuzzParseHvsockTarget(f *testing.F) {
	f.Add(testVM + ":vsock:2375")
	f.Add("{" + testVM + "}:0000094f-facb-11e6-bd58-64006a7986d3")
	f.Add(testVM + ":vsock:4294967296")
	f.Add(":")
	f.Fuzz(func(t *testing.T, s string) {
		got, err := ParseHvsockTarget(s)
		if err != nil {
			return
		}
		if len(s) > maxHvsockTargetLen {
			t.Fatalf("accepted %d-byte input", len(s))
		}
		again, err := ParseHvsockTarget(got.String())
		if err != nil {
			t.Fatalf("String() output %q rejected: %v", got.String(), err)
		}
		if again != got {
			t.Fatalf("round trip %q: %v != %v", s, again, got)
		}
	})
}

// FuzzValidatePipeName: never panics, and accepted names satisfy every invariant the
// listener relies on.
func FuzzValidatePipeName(f *testing.F) {
	f.Add(DefaultPipeName)
	f.Add(`\\.\pipe\a\b`)
	f.Add(`\\server\pipe\x`)
	f.Add(`\\.\PiPe\` + "\U0001F600")
	f.Fuzz(func(t *testing.T, s string) {
		if ValidatePipeName(s) != nil {
			return
		}
		if !strings.EqualFold(s[:len(localPipePrefix)], localPipePrefix) {
			t.Fatalf("accepted %q without the local prefix", s)
		}
		rest := s[len(localPipePrefix):]
		if rest == "" || strings.ContainsAny(rest, "\\/\x00") {
			t.Fatalf("accepted bad name part %q", rest)
		}
		if !utf8.ValidString(s) {
			t.Fatalf("accepted invalid UTF-8 %q", s)
		}
		if n := len(utf16.Encode([]rune(s))); n > maxPipeNameLen {
			t.Fatalf("accepted %d UTF-16 units", n)
		}
	})
}

// FuzzForward pushes arbitrary bytes through forward in both directions, written in
// arbitrary chunk sizes, with the client half-closing first or last. Every byte must
// arrive intact and in order, and each half-close must reach the other side as EOF.
func FuzzForward(f *testing.F) {
	f.Add([]byte("POST /attach HTTP/1.1\r\nUpgrade: tcp\r\n\r\nhello"), []byte("HTTP/1.1 101 UPGRADED\r\n\r\nHELLO"), uint8(7), true)
	f.Add([]byte{}, []byte{}, uint8(0), false)
	f.Add(bytes.Repeat([]byte{0}, 70000), []byte("x"), uint8(255), false)

	// In-memory pairs: real sockets would exhaust ephemeral ports at fuzzing rates.
	f.Fuzz(func(t *testing.T, up, down []byte, chunk uint8, clientFirst bool) {
		cApp, cProxy := memPair()
		uProxy, uApp := memPair()
		defer cApp.Close()
		defer uApp.Close()

		done := make(chan struct{})
		go func() {
			forward(context.Background(), cProxy, uProxy, time.Second, discardLogger, nil)
			close(done)
		}()

		send := func(c *memConn, b []byte) error {
			step := int(chunk) + 1
			for len(b) > 0 {
				n := min(step, len(b))
				if _, err := c.Write(b[:n]); err != nil {
					return err
				}
				b = b[n:]
			}
			return c.CloseWrite()
		}
		recv := func(c *memConn, out chan<- []byte) {
			b, _ := io.ReadAll(c)
			out <- b
		}
		gotUp := make(chan []byte, 1)
		gotDown := make(chan []byte, 1)
		go recv(uApp, gotUp)
		go recv(cApp, gotDown)

		first, second := cApp, uApp
		firstData, secondData := up, down
		if !clientFirst {
			first, second = uApp, cApp
			firstData, secondData = down, up
		}
		if err := send(first, firstData); err != nil {
			t.Fatal(err)
		}
		if err := send(second, secondData); err != nil {
			t.Fatal(err)
		}
		if b := <-gotUp; !bytes.Equal(b, up) {
			t.Fatalf("upstream got %d bytes, want %d", len(b), len(up))
		}
		if b := <-gotDown; !bytes.Equal(b, down) {
			t.Fatalf("client got %d bytes, want %d", len(b), len(down))
		}
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("forward did not finish after both sides half-closed")
		}
	})
}

// memConn is one end of an in-memory, unbuffered connection that supports CloseWrite.
type memConn struct {
	r *io.PipeReader
	w *io.PipeWriter
}

// memPair returns two connected memConns.
func memPair() (*memConn, *memConn) {
	ar, bw := io.Pipe()
	br, aw := io.Pipe()
	return &memConn{r: ar, w: aw}, &memConn{r: br, w: bw}
}

func (c *memConn) Read(b []byte) (int, error)  { return c.r.Read(b) }
func (c *memConn) Write(b []byte) (int, error) { return c.w.Write(b) }
func (c *memConn) CloseWrite() error           { return c.w.Close() }
func (c *memConn) Close() error {
	_ = c.r.CloseWithError(net.ErrClosed)
	_ = c.w.CloseWithError(net.ErrClosed)
	return nil
}
func (c *memConn) LocalAddr() net.Addr              { return memAddr{} }
func (c *memConn) RemoteAddr() net.Addr             { return memAddr{} }
func (c *memConn) SetDeadline(time.Time) error      { return nil }
func (c *memConn) SetReadDeadline(time.Time) error  { return nil }
func (c *memConn) SetWriteDeadline(time.Time) error { return nil }

type memAddr struct{}

func (memAddr) Network() string { return "mem" }
func (memAddr) String() string  { return "mem" }
