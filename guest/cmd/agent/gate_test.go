package main

import (
	"errors"
	"io"
	"testing"
	"time"

	"github.com/Higangssh/hyperbutler/internal/vmsecret"
)

func newSecret(t *testing.T) vmsecret.Secret {
	t.Helper()
	s, err := vmsecret.New()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// With the VM's secret first, the connection reaches dockerd and the bytes after the
// secret arrive there unchanged.
func TestServeHostAdmits(t *testing.T) {
	secret := newSecret(t)
	host, agent := tcpPair(t)
	docker, dockerd := tcpPair(t)
	done := make(chan error, 1)
	go func() {
		done <- serveHost(agent, secret, time.Second, func() (stream, error) { return docker, nil })
	}()
	if _, err := host.Write(append(secret[:], "GET /_ping"...)); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len("GET /_ping"))
	_ = dockerd.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(dockerd, got); err != nil || string(got) != "GET /_ping" {
		t.Fatalf("dockerd read %q, %v", got, err)
	}
	if _, err := dockerd.Write([]byte("OK")); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 2)
	_ = host.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(host, reply); err != nil || string(reply) != "OK" {
		t.Fatalf("host read %q, %v", reply, err)
	}
	_ = host.CloseWrite()
	_ = dockerd.CloseWrite()
	if err := waitRelay(t, done); err != nil {
		t.Fatal(err)
	}
}

// A wrong, short, late or missing secret closes the connection: dockerd is never
// dialed and the peer reads nothing but the close.
func TestServeHostRefuses(t *testing.T) {
	secret := newSecret(t)
	wrong := secret
	wrong[0] ^= 0x80
	for name, tc := range map[string]struct {
		send []byte
		want vmsecret.Secret
	}{
		"wrong secret":    {append(wrong[:], "GET /_ping"...), secret},
		"short":           {secret[:vmsecret.Size-1], secret},
		"nothing":         {nil, secret},
		"no secret in VM": {make([]byte, vmsecret.Size), vmsecret.Secret{}},
		"plain HTTP":      {[]byte("GET /_ping HTTP/1.1\r\nHost: docker\r\n\r\n"), secret},
		"zeros sent":      {make([]byte, vmsecret.Size), secret},
	} {
		t.Run(name, func(t *testing.T) {
			host, agent := tcpPair(t)
			dialed := false
			done := make(chan error, 1)
			go func() {
				done <- serveHost(agent, tc.want, 200*time.Millisecond, func() (stream, error) {
					dialed = true
					return nil, errors.New("dialed")
				})
			}()
			if len(tc.send) > 0 {
				if _, err := host.Write(tc.send); err != nil {
					t.Fatal(err)
				}
			}
			err := waitRelay(t, done)
			if !errors.Is(err, vmsecret.ErrMismatch) || dialed {
				t.Fatalf("err %v, dialed %v", err, dialed)
			}
			_ = host.SetReadDeadline(time.Now().Add(5 * time.Second))
			if n, err := host.Read(make([]byte, 64)); n != 0 || err == nil {
				t.Fatalf("the peer read %d bytes, %v", n, err)
			}
		})
	}
}
