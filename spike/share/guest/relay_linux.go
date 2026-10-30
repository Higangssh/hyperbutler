package main

import (
	"fmt"
	"io"
	"strconv"

	"github.com/Higangssh/winbox/internal/share/relay"
)

var relays []*relay.Relay

// startRelay dials the host's share server (queues connections) and mounts it.
func startRelay(a map[string]string) (any, error) {
	var port uint32
	fmt.Sscan(a["port"], &port)
	n, _ := strconv.Atoi(a["queues"])
	if n <= 0 {
		n = 1
	}
	var conns []io.ReadWriteCloser
	for i := 0; i < n; i++ {
		c, err := dialVsock(port, 10e9)
		if err != nil {
			for _, c := range conns {
				c.Close()
			}
			return nil, err
		}
		conns = append(conns, c)
	}
	ra, _ := strconv.Atoi(a["readahead"])
	r, err := relay.Start(relay.Config{Mountpoint: a["target"], Conns: conns, ReadAheadKB: ra, Logf: logf})
	if err != nil {
		for _, c := range conns {
			c.Close()
		}
		return nil, err
	}
	relays = append(relays, r)
	go func() {
		if err := r.Wait(); err != nil {
			logf("relay %s: %v", a["target"], err)
		}
	}()
	return nil, nil
}

func relayMain(args []string) {}
