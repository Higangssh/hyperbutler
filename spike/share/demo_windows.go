package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"time"

	"github.com/Higangssh/hyperbutler/guest/cpio"
	"github.com/Higangssh/hyperbutler/internal/engine"
	"github.com/Higangssh/hyperbutler/internal/share"
	"github.com/Higangssh/hyperbutler/internal/transport"
	"github.com/Higangssh/hyperbutler/internal/vm"
	"github.com/Microsoft/go-winio"
	"github.com/Microsoft/go-winio/pkg/guid"
)

var (
	demo       = flag.Bool("demo", false, "boot the full hyperbutler image through internal/engine with bind mounts and serve a translating Docker API on -demo-pipe until Ctrl+C")
	demoPipe   = flag.String("demo-pipe", `\\.\pipe\hyperbutler-share-demo`, "Docker API pipe of the demo")
	demoEngine = flag.String("demo-engine-pipe", `\\.\pipe\hyperbutler-share-demo-raw`, "the engine's own (untranslated) Docker API pipe in the demo")
)

const guestMount = "/mnt/hyperbutler"

// createPath matches POST /containers/create with or without an API version prefix.
var createPath = regexp.MustCompile(`^(/v[0-9]+\.[0-9]+)?/containers/create$`)

// runDemo is the integration path of ADR 0007 in miniature:
//   - the guest gets fuse.ko and the relay (here as a wrapper that starts the relay
//     and then execs the normal /init);
//   - the engine gets an extra hvsock service the relay connects to;
//   - a Docker API proxy rewrites Windows bind sources to /mnt/hyperbutler/<share>.
func runDemo() error {
	guestBin, err := buildGuest()
	if err != nil {
		return err
	}
	initrd, err := buildDemoInitrd(guestBin)
	if err != nil {
		return err
	}
	sddl, err := engine.CurrentUserSDDL()
	if err != nil {
		return err
	}
	srv := share.NewServer(share.Options{})
	defer srv.Close()
	reg := &share.Registry{Server: srv, MountPoint: guestMount}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg := engine.Config{
		KernelPath:    filepath.Join(*imageDir, "vmlinuz-virt"),
		InitrdPath:    initrd,
		MemoryMB:      *memMB,
		CPUs:          *cpus,
		Pipe:          *demoEngine,
		VMID:          fmt.Sprintf("hyperbutler-share-demo-%d", time.Now().UnixNano()%1e6),
		Logger:        log,
		Share:         engine.ShareConfig{Off: true}, // the demo wires its own share server
		ExtraServices: []vm.Service{{ID: transport.VsockServiceID(fusePort), BindSDDL: sddl}},
		ExtraCmdLine:  fmt.Sprintf(" rdinit=/sharebench sbwrap=1 sbfuseport=%d sbqueues=%d", fusePort, *fuseQueues),
		OnCreated: func(_ string, rid guid.GUID) {
			go serveShares(ctx, srv, rid)
		},
		OnReady: func(ctx context.Context, r *engine.Running) error {
			return serveProxy(ctx, r, reg)
		},
	}
	if f, err := os.Create(filepath.Join(*work, "demo-console.log")); err == nil {
		defer f.Close()
		cfg.ConsoleLog = f
	}
	return engine.Run(ctx, cfg)
}

func serveShares(ctx context.Context, srv *share.Server, rid guid.GUID) {
	ln, err := winio.ListenHvsock(&winio.HvsockAddr{VMID: rid, ServiceID: transport.VsockServiceID(fusePort)})
	if err != nil {
		fmt.Println("share listen:", err)
		return
	}
	context.AfterFunc(ctx, func() { ln.Close() })
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			srv.Serve(c)
		}()
	}
}

// serveProxy serves the translating Docker API until ctx ends.
func serveProxy(ctx context.Context, r *engine.Running, reg *share.Registry) error {
	ln, err := transport.ListenPipe(*demoPipe)
	if err != nil {
		return err
	}
	d := &transport.HvsockDialer{Target: r.Docker, Retries: 2, RetryWait: 100 * time.Millisecond}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = "http", "docker"
		},
		Transport:     &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return d.DialContext(ctx) }},
		FlushInterval: -1,
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost && createPath.MatchString(req.URL.Path) {
			body, err := io.ReadAll(io.LimitReader(req.Body, 4<<20+1))
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			nb, err := share.RewriteCreate(body, reg.Resolve)
			if err != nil {
				http.Error(w, `{"message":"hyperbutler: `+err.Error()+`"}`, http.StatusBadRequest)
				return
			}
			if !bytes.Equal(nb, body) {
				fmt.Printf("rewrote container create: %s\n", nb)
			}
			req.Body = io.NopCloser(bytes.NewReader(nb))
			req.ContentLength = int64(len(nb))
			req.Header.Del("Content-Length")
		}
		rp.ServeHTTP(w, req)
	})
	hs := &http.Server{Handler: h}
	context.AfterFunc(ctx, func() { hs.Close() })
	fmt.Printf("demo ready: docker -H npipe://%s run -v C:\\some\\dir:/src ...\n", filepathToNpipe(*demoPipe))
	if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return engine.ErrDone
}

func filepathToNpipe(p string) string {
	b := []byte(p)
	for i := range b {
		if b[i] == '\\' {
			b[i] = '/'
		}
	}
	return string(b)
}

// buildDemoInitrd appends sharebench (as the wrapper) and fuse.ko to the hyperbutler image.
func buildDemoInitrd(guestBin string) (string, error) {
	base, err := os.ReadFile(filepath.Join(*imageDir, "initramfs-hyperbutler.gz"))
	if err != nil {
		return "", err
	}
	var extra bytes.Buffer
	zw := gzip.NewWriter(&extra)
	w := cpio.NewWriter(zw)
	add := func(name string, mode uint32, data []byte) error {
		if err := w.WriteHeader(cpio.Header{Name: name, Mode: mode, Size: uint32(len(data)), Nlink: 1}); err != nil {
			return err
		}
		_, err := w.Write(data)
		return err
	}
	bin, err := os.ReadFile(guestBin)
	if err != nil {
		return "", err
	}
	fuse, err := gunzipFile(filepath.Join(*modDir, "kernel", "fs", "fuse", "fuse.ko.gz"))
	if err != nil {
		return "", err
	}
	for _, e := range []struct {
		name string
		mode uint32
		data []byte
	}{{"sharebench", cpio.TypeRegular | 0o755, bin}, {"sbmods", cpio.TypeDir | 0o755, nil}, {"sbmods/fuse.ko", cpio.TypeRegular | 0o644, fuse}} {
		if err := add(e.name, e.mode, e.data); err != nil {
			return "", err
		}
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	if err := zw.Close(); err != nil {
		return "", err
	}
	out := filepath.Join(*work, "initramfs-demo")
	return out, os.WriteFile(out, append(base, extra.Bytes()...), 0o644)
}
