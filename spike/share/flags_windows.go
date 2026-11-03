package main

import (
	"archive/tar"
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Higangssh/hyperbutler/internal/guid"
	"github.com/Microsoft/go-winio"
	wguid "github.com/Microsoft/go-winio/pkg/guid"

	"github.com/Higangssh/hyperbutler/internal/share"
	"github.com/Higangssh/hyperbutler/internal/transport"
)

func flagDuration(name string, def time.Duration, usage string) *time.Duration {
	return flag.Duration(name, def, usage)
}

func flagInt(name string, def int, usage string) *int { return flag.Int(name, def, usage) }

func flagBool(name string, def bool, usage string) *bool { return flag.Bool(name, def, usage) }

type syncTarget struct{ guestDir, hostDir string }

// genHostTree writes the same node_modules-like tree the guest untar uses, on Windows.
func genHostTree(dir string, files int) (time.Duration, error) {
	r := rand.New(rand.NewSource(42)) //nolint:gosec // G404: a fixed seed makes the generated tree reproducible; not security-relevant
	data := make([]byte, 256<<10)
	r.Read(data)
	t := time.Now()
	n := 0
	for pkg := 0; n < files; pkg++ {
		base := filepath.Join(dir, "node_modules", fmt.Sprintf("pkg-%04d", pkg))
		for sub := 0; sub < 3 && n < files; sub++ {
			d := base
			if sub > 0 {
				d = filepath.Join(base, fmt.Sprintf("lib%d", sub))
			}
			if err := os.MkdirAll(d, 0o750); err != nil {
				return 0, err
			}
			for f := 0; f < 7 && n < files; f++ {
				sz := 512 + r.Intn(8<<10)
				if r.Intn(20) == 0 {
					sz = r.Intn(200 << 10)
				}
				if err := os.WriteFile(filepath.Join(d, fmt.Sprintf("file%d.js", f)), data[:sz], 0o600); err != nil {
					return 0, err
				}
				n++
			}
		}
	}
	return time.Since(t), nil
}

// startSync is option (d): a host tree mirrored into guest ext4. The initial copy is a
// tar stream over hvsock; afterwards the share watcher pushes each change.
func startSync(ctx context.Context, g *guest, vmid guid.GUID) (*syncTarget, error) {
	hostDir := filepath.Join(*work, "sync")
	_ = os.RemoveAll(hostDir)
	if err := os.MkdirAll(hostDir, 0o750); err != nil {
		return nil, err
	}
	d, err := genHostTree(filepath.Join(hostDir, "tree"), *files)
	if err != nil {
		return nil, err
	}
	fmt.Printf("sync: wrote %d files on Windows in %v\n", *files, d.Round(time.Millisecond))
	guestDir := "/mnt/ext4/sync"
	ln, err := winio.ListenHvsock(&winio.HvsockAddr{VMID: wguid.GUID(vmid), ServiceID: wguid.GUID(transport.VsockServiceID(syncPort))})
	if err != nil {
		return nil, err
	}
	go func() {
		c, err := ln.Accept()
		ln.Close()
		if err != nil {
			return
		}
		defer c.Close()
		tw := tar.NewWriter(c)
		// A failed walk shows up on the guest side as a short tar stream.
		_ = filepath.WalkDir(hostDir, func(p string, de fs.DirEntry, err error) error {
			if err != nil || p == hostDir {
				return err
			}
			rel, _ := filepath.Rel(hostDir, p)
			rel = filepath.ToSlash(rel)
			if de.IsDir() {
				return tw.WriteHeader(&tar.Header{Name: rel + "/", Typeflag: tar.TypeDir, Mode: 0o755})
			}
			b, err := os.ReadFile(p) //nolint:gosec // G304: the path comes from the operator's own flags or this tool's work directory
			if err != nil {
				return err
			}
			if err := tw.WriteHeader(&tar.Header{Name: rel, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(b))}); err != nil {
				return err
			}
			_, err = tw.Write(b)
			return err
		})
		tw.Close()
	}()
	out, err := g.bench2("recvtar", map[string]string{"port": fmt.Sprint(syncPort), "root": guestDir})
	if err != nil {
		return nil, err
	}
	fmt.Printf("sync: initial copy of %v files into guest ext4 in %.2f s\n", out["files"], out["sync_s"])
	fs, err := share.OpenNTFS(hostDir)
	if err != nil {
		return nil, err
	}
	rel := func(p []string) string { return strings.Join(p, "/") }
	w, err := fs.Watch(func(k share.EventKind, p, np []string) {
		switch k {
		case share.Created, share.Modified:
			b, err := os.ReadFile(filepath.Join(append([]string{hostDir}, p...)...))
			if err != nil {
				return // a directory, or gone again
			}
			_, _ = g.call("put", map[string]string{"root": guestDir, "path": rel(p), "data": base64.StdEncoding.EncodeToString(b)})
		case share.Removed:
			_, _ = g.call("del", map[string]string{"root": guestDir, "path": rel(p)})
		case share.Renamed:
			_, _ = g.call("mv", map[string]string{"root": guestDir, "path": rel(p), "to": rel(np)})
		}
	}, func() { fmt.Println("sync: watcher overflow (a full rescan would be needed)") })
	if err != nil {
		return nil, err
	}
	context.AfterFunc(ctx, func() { w.Close(); fs.Close() })
	var _ io.Reader
	return &syncTarget{guestDir: guestDir, hostDir: hostDir}, nil
}
