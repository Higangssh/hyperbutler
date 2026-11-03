package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Higangssh/hyperbutler/internal/guid"
	"github.com/Microsoft/go-winio"
	wguid "github.com/Microsoft/go-winio/pkg/guid"

	"github.com/Higangssh/hyperbutler/internal/share"
	"github.com/Higangssh/hyperbutler/internal/transport"
)

var (
	fuseTTL       = flagDuration("fuse-ttl", 0, "FUSE entry/attr TTL (0 = server default, 1h)")
	fuseWriteback = flagBool("fuse-writeback", false, "FUSE writeback cache")
	fuseQueues    = flagInt("fuse-queues", 4, "hvsock connections per FUSE mount")
	fuseRA        = flagInt("fuse-readahead", 4096, "FUSE mount readahead in KiB")
)

// startFuse serves hostDir over hvsock with internal/share and has the guest mount it
// at /mnt/hyperbutler. It returns the guest path of the share.
func startFuse(ctx context.Context, g *guest, vmid guid.GUID, hostDir string, nocache bool) (string, error) {
	fs, err := share.OpenNTFS(hostDir)
	if err != nil {
		return "", err
	}
	opt := share.Options{NoCache: nocache, Writeback: *fuseWriteback, EntryTTL: *fuseTTL, AttrTTL: *fuseTTL}
	if *traceOn {
		opt.Trace = traceOp
	}
	srv := share.NewServer(opt)
	if err := srv.AddShare(&share.Share{Name: "src", FS: fs}); err != nil {
		return "", err
	}
	ln, err := winio.ListenHvsock(&winio.HvsockAddr{VMID: wguid.GUID(vmid), ServiceID: wguid.GUID(transport.VsockServiceID(fusePort))})
	if err != nil {
		return "", err
	}
	context.AfterFunc(ctx, func() { ln.Close(); srv.Close() })
	accepted := make(chan error, 1)
	go func() {
		for i := 0; ; i++ {
			c, err := ln.Accept()
			if i == 0 {
				accepted <- err
			}
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				if err := srv.Serve(c); err != nil && ctx.Err() == nil {
					fmt.Println("share server:", err)
				}
			}()
		}
	}()
	mp := "/mnt/hyperbutler"
	if nocache {
		mp = "/mnt/hyperbutler-nocache"
	}
	if _, err := g.call("fuse", map[string]string{"port": fmt.Sprint(fusePort), "target": mp, "queues": fmt.Sprint(*fuseQueues), "readahead": fmt.Sprint(*fuseRA)}); err != nil {
		return "", err
	}
	select {
	case err := <-accepted:
		if err != nil {
			return "", err
		}
	case <-time.After(10 * time.Second):
		return "", errors.New("guest did not connect to the share server")
	}
	return mp + "/src", nil
}
