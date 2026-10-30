package main

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"time"
)

func init() {
	// vsockpar uploads then downloads mb MiB split over n parallel connections.
	extraOps["vsockpar"] = func(a map[string]string) (any, error) {
		var port uint32
		fmt.Sscan(a["port"], &port)
		mb, n := atoi(a["mb"], 256), atoi(a["n"], 4)
		conns := make([]io.ReadWriteCloser, n)
		for i := range conns {
			c, err := dialVsock(port, 5*time.Second)
			if err != nil {
				return nil, err
			}
			defer c.Close()
			conns[i] = c
		}
		run := func(f func(c io.ReadWriteCloser) error) (time.Duration, error) {
			t := time.Now()
			errs := make(chan error, n)
			for _, c := range conns {
				go func() { errs <- f(c) }()
			}
			for range conns {
				if err := <-errs; err != nil {
					return 0, err
				}
			}
			return time.Since(t), nil
		}
		per := mb / n
		up, err := run(func(c io.ReadWriteCloser) error {
			buf := make([]byte, 1<<20)
			for i := 0; i < per; i++ {
				if _, err := c.Write(buf); err != nil {
					return err
				}
			}
			_, err := io.ReadFull(c, buf[:1])
			return err
		})
		if err != nil {
			return nil, err
		}
		down, err := run(func(c io.ReadWriteCloser) error {
			buf := make([]byte, 1<<20)
			for i := 0; i < per; i++ {
				if _, err := io.ReadFull(c, buf); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		return map[string]float64{"up_MBps": float64(per*n) / up.Seconds(), "down_MBps": float64(per*n) / down.Seconds()}, nil
	}
}

func init() {
	extraOps["sockbuf"] = func(a map[string]string) (any, error) {
		n, err := strconv.Atoi(a["bytes"])
		sockBuf = n
		return nil, err
	}
	// vsockbw measures the raw hvsock path the share server uses: upload mb MiB, then
	// download mb MiB, then time 2000 one-byte round trips.
	extraOps["vsockbw"] = func(a map[string]string) (any, error) {
		var port uint32
		fmt.Sscan(a["port"], &port)
		mb := atoi(a["mb"], 256)
		c, err := dialVsock(port, 5*time.Second)
		if err != nil {
			return nil, err
		}
		defer c.Close()
		buf := make([]byte, 1<<20)
		res := map[string]float64{}
		t := time.Now()
		for i := 0; i < mb; i++ {
			if _, err := c.Write(buf); err != nil {
				return nil, err
			}
		}
		one := make([]byte, 1)
		if _, err := io.ReadFull(c, one); err != nil {
			return nil, err
		}
		res["up_MBps"] = float64(mb) / time.Since(t).Seconds()
		t = time.Now()
		if _, err := io.ReadFull(c, make([]byte, mb<<20)); err != nil {
			return nil, err
		}
		res["down_MBps"] = float64(mb) / time.Since(t).Seconds()
		lat := make([]time.Duration, 2000)
		for i := range lat {
			t0 := time.Now()
			if _, err := c.Write(one); err != nil {
				return nil, err
			}
			if _, err := io.ReadFull(c, one); err != nil {
				return nil, err
			}
			lat[i] = time.Since(t0)
		}
		sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
		res["rtt_p50_us"] = float64(lat[1000].Nanoseconds()) / 1000
		res["rtt_p99_us"] = float64(lat[1980].Nanoseconds()) / 1000
		return res, nil
	}
}
