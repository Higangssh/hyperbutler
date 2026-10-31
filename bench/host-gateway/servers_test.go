package main

import (
	"io"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/Higangssh/hyperbutler/internal/leaktest"
)

func TestMain(m *testing.M) { leaktest.VerifyTestMain(m) }

func TestServers(t *testing.T) {
	h, err := startHTTP("tcp4", "127.0.0.1:0", "body")
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 5 * time.Second}
	resp, err := c.Get("http://127.0.0.1:" + strconv.Itoa(h.port()) + "/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	h.close()
	if string(b) != "body" {
		t.Fatalf("body %q", b)
	}

	hold, err := startHold("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var cs []net.Conn
	for i := 0; i < 3; i++ {
		c, err := net.Dial("tcp4", "127.0.0.1:"+strconv.Itoa(hold.port()))
		if err != nil {
			t.Fatal(err)
		}
		cs = append(cs, c)
	}
	for deadline := time.Now().Add(5 * time.Second); hold.maxOpen.Load() < 3 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if hold.maxOpen.Load() != 3 {
		t.Fatalf("peak %d", hold.maxOpen.Load())
	}
	for _, c := range cs {
		c.Close()
	}
	hold.close()

	u, err := startUDPEcho("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	uc, err := net.Dial("udp4", "127.0.0.1:"+strconv.Itoa(u.port()))
	if err != nil {
		t.Fatal(err)
	}
	uc.Write([]byte("x"))
	uc.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 16)
	k, err := uc.Read(buf)
	uc.Close()
	u.close()
	if err != nil || string(buf[:k]) != "echo x" {
		t.Fatalf("udp %q %v", buf[:k], err)
	}
}
