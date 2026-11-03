//go:build windows

package winipc

import (
	"io"
	"net"
	"testing"
)

// The benchmarks measure one direction's throughput (Stream) and request/response
// latency (PingPong) over a loopback pipe and a loopback Hyper-V socket:
//
//	go test ./internal/winipc -run - -bench . -benchtime 2s

func benchPipe(b *testing.B) (net.Conn, net.Conn) {
	c, s := pipePair(b, true)
	return c, s
}

func benchHvsock(b *testing.B) (net.Conn, net.Conn) {
	c, s := hvPair(b)
	return c, s
}

func benchStream(b *testing.B, pair func(*testing.B) (net.Conn, net.Conn), size int) {
	c, s := pair(b)
	buf := make([]byte, size)
	b.SetBytes(int64(size))
	b.ReportAllocs()
	errc := make(chan error, 1)
	total := int64(b.N) * int64(size)
	go func() {
		rb := make([]byte, 64<<10)
		for left := total; left > 0; {
			n, err := s.Read(rb)
			if err != nil {
				errc <- err
				return
			}
			left -= int64(n)
		}
		errc <- nil
	}()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.Write(buf); err != nil {
			b.Fatal(err)
		}
	}
	if err := <-errc; err != nil {
		b.Fatal(err)
	}
}

func benchPingPong(b *testing.B, pair func(*testing.B) (net.Conn, net.Conn), size int) {
	c, s := pair(b)
	go func() {
		rb := make([]byte, size)
		for {
			if _, err := io.ReadFull(s, rb); err != nil {
				return
			}
			if _, err := s.Write(rb); err != nil {
				return
			}
		}
	}()
	buf := make([]byte, size)
	b.SetBytes(int64(size))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.Write(buf); err != nil {
			b.Fatal(err)
		}
		if _, err := io.ReadFull(c, buf); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPipeStream64K(b *testing.B)    { benchStream(b, benchPipe, 64<<10) }
func BenchmarkPipeStream1K(b *testing.B)     { benchStream(b, benchPipe, 1<<10) }
func BenchmarkPipePingPong1K(b *testing.B)   { benchPingPong(b, benchPipe, 1<<10) }
func BenchmarkHvsockStream64K(b *testing.B)  { benchStream(b, benchHvsock, 64<<10) }
func BenchmarkHvsockStream1K(b *testing.B)   { benchStream(b, benchHvsock, 1<<10) }
func BenchmarkHvsockPingPong1K(b *testing.B) { benchPingPong(b, benchHvsock, 1<<10) }
