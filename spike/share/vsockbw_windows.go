package main

import (
	"fmt"
	"io"

	"github.com/Higangssh/hyperbutler/internal/guid"
	"github.com/Higangssh/hyperbutler/internal/winipc"

	"github.com/Higangssh/hyperbutler/internal/transport"
)

var vsockBW = flagBool("vsockbw", false, "measure raw hvsock bandwidth and latency (guest buffer sizes 0, 256 KiB, 1 MiB)")

func runVsockBW(g *guest, vmid guid.GUID) {
	for _, sb := range []int{0, 256 << 10, 1 << 20} {
		ln, err := winipc.ListenHvsock(winipc.HvsockAddr{VMID: vmid, ServiceID: transport.VsockServiceID(syncPort)})
		if err != nil {
			fmt.Println("vsockbw:", err)
			return
		}
		const mb = 256
		go func() {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
			rb := make([]byte, 1<<20)
			for i := 0; i < mb; i++ {
				_, _ = io.ReadFull(c, rb)
			}
			_, _ = c.Write([]byte{1})
			buf := make([]byte, 1<<20)
			for i := 0; i < mb; i++ {
				_, _ = c.Write(buf)
			}
			one := make([]byte, 1)
			for i := 0; i < 2000; i++ {
				if _, err := io.ReadFull(c, one); err != nil {
					return
				}
				_, _ = c.Write(one)
			}
		}()
		_, _ = g.call("sockbuf", map[string]string{"bytes": fmt.Sprint(sb)})
		out, err := g.bench2("vsockbw", map[string]string{"port": fmt.Sprint(syncPort), "mb": fmt.Sprint(mb)})
		ln.Close()
		fmt.Printf("== vsock (guest SO_SNDBUF/SO_RCVBUF %d): %v %v\n", sb, out, err)
	}
	_, _ = g.call("sockbuf", map[string]string{"bytes": "0"})
}

func runVsockPar(g *guest, vmid guid.GUID) {
	for _, n := range []int{1, 2, 4, 8} {
		ln, err := winipc.ListenHvsock(winipc.HvsockAddr{VMID: vmid, ServiceID: transport.VsockServiceID(syncPort)})
		if err != nil {
			fmt.Println("vsockpar:", err)
			return
		}
		const mb = 512
		per := mb / n
		go func() {
			for i := 0; i < n; i++ {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go func() {
					defer c.Close()
					buf := make([]byte, 1<<20)
					for i := 0; i < per; i++ {
						if _, err := io.ReadFull(c, buf); err != nil {
							return
						}
					}
					_, _ = c.Write(buf[:1])
					for i := 0; i < per; i++ {
						if _, err := c.Write(buf); err != nil {
							return
						}
					}
					_, _ = io.ReadFull(c, buf[:1])
				}()
			}
		}()
		out, err := g.bench2("vsockpar", map[string]string{"port": fmt.Sprint(syncPort), "mb": fmt.Sprint(mb), "n": fmt.Sprint(n)})
		ln.Close()
		fmt.Printf("== vsock x%d: %v %v\n", n, out, err)
	}
}

func (g *guest) bench2(op string, args map[string]string) (map[string]float64, error) {
	out, err := g.call(op, args)
	if err != nil {
		return nil, err
	}
	m := map[string]float64{}
	if mm, ok := out.(map[string]any); ok {
		for k, v := range mm {
			if f, ok := v.(float64); ok {
				m[k] = f
			}
		}
	}
	return m, nil
}
