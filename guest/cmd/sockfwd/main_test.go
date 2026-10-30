package main

import (
	"io"
	"net"
	"path/filepath"
	"testing"
)

func TestRelayHalfClose(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "s.sock")
	ul, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("no AF_UNIX: %v", err)
	}
	defer ul.Close()
	go func() {
		for {
			c, err := ul.Accept()
			if err != nil {
				return
			}
			go func() { // echo until EOF, then close
				io.Copy(c, c)
				c.Close()
			}()
		}
	}()

	tl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tl.Close()
	go serve(tl, sock)

	for i := 0; i < 3; i++ {
		c, err := net.Dial("tcp", tl.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		msg := []byte("GET /_ping HTTP/1.0\r\n\r\n")
		if _, err := c.Write(msg); err != nil {
			t.Fatal(err)
		}
		c.(*net.TCPConn).CloseWrite()
		got, err := io.ReadAll(c)
		c.Close()
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(msg) {
			t.Fatalf("got %q, want %q", got, msg)
		}
	}
}
