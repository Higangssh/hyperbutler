package main

import (
	"bytes"
	"io"
	"sync"
	"testing"
	"time"
)

// FuzzRelay pushes arbitrary bytes through relay in both directions, written in
// arbitrary chunk sizes, with either side half-closing first. Every byte must arrive
// intact and in order, each half-close must arrive as EOF, and relay must return nil.
// The relay parses nothing, so this is its protocol-handler fuzz test: it checks that no
// byte pattern (HTTP upgrade, zero-length writes, large bursts) changes the framing.
func FuzzRelay(f *testing.F) {
	f.Add([]byte("POST /containers/x/attach?stream=1&stdin=1 HTTP/1.1\r\nUpgrade: tcp\r\nConnection: Upgrade\r\n\r\nhello"),
		[]byte("HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\n\r\nHELLO"), uint8(7), true)
	f.Add([]byte{}, []byte{}, uint8(0), false)
	f.Add(bytes.Repeat([]byte{0}, 3*relayBufferSize+1), []byte("x"), uint8(255), false)

	f.Fuzz(func(t *testing.T, up, down []byte, chunk uint8, hostFirst bool) {
		hostApp, hostRelay := memPair()
		dockerRelay, dockerApp := memPair()
		defer hostApp.Close()
		defer dockerApp.Close()
		done := runRelay(hostRelay, dockerRelay)

		send := func(c *memConn, b []byte) {
			n := int(chunk) + 1
			for len(b) > 0 {
				k := min(n, len(b))
				if _, err := c.Write(b[:k]); err != nil {
					t.Errorf("write: %v", err)
					return
				}
				b = b[k:]
			}
			if err := c.CloseWrite(); err != nil {
				t.Errorf("close write: %v", err)
			}
		}
		var gotUp, gotDown []byte
		var errUp, errDown error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); gotUp, errUp = io.ReadAll(dockerApp) }()
		go func() { defer wg.Done(); gotDown, errDown = io.ReadAll(hostApp) }()
		if hostFirst {
			send(hostApp, up)
			send(dockerApp, down)
		} else {
			send(dockerApp, down)
			send(hostApp, up)
		}
		wg.Wait()
		if errUp != nil || errDown != nil {
			t.Fatalf("read errors: up %v, down %v", errUp, errDown)
		}
		if !bytes.Equal(gotUp, up) {
			t.Fatalf("host->docker: got %d bytes, want %d", len(gotUp), len(up))
		}
		if !bytes.Equal(gotDown, down) {
			t.Fatalf("docker->host: got %d bytes, want %d", len(gotDown), len(down))
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("relay: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("relay did not return after both half-closes")
		}
	})
}
