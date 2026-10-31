// Command sockfwd relays TCP connections to a Unix socket. It is a stand-in for the
// guest agent in the ADR 0005 prototype, so the host can reach dockerd's Unix socket
// through QEMU user networking without dockerd listening on TCP itself. (dockerd sleeps
// for one second at start-up for every unauthenticated TCP listener.)
//
// It is cross-compiled on Windows with GOOS=linux CGO_ENABLED=0. Throwaway: hyperbutler will
// relay hvsock/vsock to the socket in the real guest agent.
//
//	sockfwd -listen 10.0.2.15:2375 -unix /run/docker.sock
package main

import (
	"flag"
	"io"
	"log"
	"net"
	"os"
)

func main() {
	listen := flag.String("listen", "", "TCP address to listen on")
	unix := flag.String("unix", "", "Unix socket to connect to")
	flag.Parse()
	if *listen == "" || *unix == "" {
		flag.Usage()
		os.Exit(2)
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	log.Fatal(serve(ln, *unix))
}

func serve(ln net.Listener, unixPath string) error {
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		go relay(c, unixPath)
	}
}

func relay(c net.Conn, unixPath string) {
	defer func() { _ = c.Close() }()
	u, err := net.Dial("unix", unixPath)
	if err != nil {
		log.Printf("dial %s: %v", unixPath, err)
		return
	}
	defer func() { _ = u.Close() }()
	done := make(chan struct{}, 2)
	pipe := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src) // either side closing ends the relay; nothing to report
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		done <- struct{}{}
	}
	go pipe(u, c)
	go pipe(c, u)
	<-done
	<-done
}
