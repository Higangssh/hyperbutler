package main

import (
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// The bench's host servers. They bind the address they are given, which the bench keeps
// on loopback unless -wildcard.

// httpServer answers every request with body.
type httpServer struct {
	ln  net.Listener
	srv *http.Server
}

func startHTTP(network, addr, body string) (*httpServer, error) {
	ln, err := net.Listen(network, addr)
	if err != nil {
		return nil, err
	}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, body)
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	return &httpServer{ln: ln, srv: srv}, nil
}

func (h *httpServer) port() int { return h.ln.Addr().(*net.TCPAddr).Port }
func (h *httpServer) close()    { _ = h.srv.Close() }

// holdServer accepts connections and keeps them open until close, counting how many
// are open at once.
type holdServer struct {
	ln       net.Listener
	mu       sync.Mutex
	conns    []net.Conn
	open     atomic.Int64
	maxOpen  atomic.Int64
	accepted atomic.Int64
	done     chan struct{}
}

func startHold(addr string) (*holdServer, error) {
	ln, err := net.Listen("tcp4", addr)
	if err != nil {
		return nil, err
	}
	h := &holdServer{ln: ln, done: make(chan struct{})}
	go func() {
		defer close(h.done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			h.accepted.Add(1)
			n := h.open.Add(1)
			for {
				m := h.maxOpen.Load()
				if n <= m || h.maxOpen.CompareAndSwap(m, n) {
					break
				}
			}
			h.mu.Lock()
			h.conns = append(h.conns, c)
			h.mu.Unlock()
			go func() {
				// Count the connection as closed once the peer closes it.
				var b [1]byte
				for {
					if _, err := c.Read(b[:]); err != nil {
						h.open.Add(-1)
						return
					}
				}
			}()
		}
	}()
	return h, nil
}

func (h *holdServer) port() int { return h.ln.Addr().(*net.TCPAddr).Port }

func (h *holdServer) close() {
	_ = h.ln.Close()
	<-h.done
	h.mu.Lock()
	for _, c := range h.conns {
		_ = c.Close()
	}
	h.conns = nil
	h.mu.Unlock()
}

// udpEcho answers each datagram with "echo " and the datagram.
type udpEcho struct {
	pc   net.PacketConn
	done chan struct{}
}

func startUDPEcho(addr string) (*udpEcho, error) {
	pc, err := net.ListenPacket("udp4", addr)
	if err != nil {
		return nil, err
	}
	u := &udpEcho{pc: pc, done: make(chan struct{})}
	go func() {
		defer close(u.done)
		buf := make([]byte, 2048)
		for {
			k, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = pc.WriteTo(append([]byte("echo "), buf[:k]...), from)
		}
	}()
	return u, nil
}

func (u *udpEcho) port() int { return u.pc.LocalAddr().(*net.UDPAddr).Port }
func (u *udpEcho) close()    { _ = u.pc.Close(); <-u.done }
