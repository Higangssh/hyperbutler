// Command net-guest is the in-container half of bench/net-perf (ADR 0006). The Windows
// driver runs it in containers and reads one JSON result per line from its standard
// output; it runs the same internal/netbench code as the driver's host baseline.
//
//	net-guest download -addr HOST:PORT [-secs 5]
//	net-guest upload   -addr HOST:PORT [-secs 5]
//	net-guest rr       -addr HOST:PORT [-n 20000] [-size 64] [-load HOST:PORT]
//	net-guest cps      -addr HOST:PORT [-secs 5] [-par 8]
//	net-guest url      -url URL
//	net-guest serve    -port P          (source P, sink P+1, echo P+2, ping P+3)
//
// The output is an interface between two programs built from the same commit, not a
// stable format.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/Higangssh/hyperbutler/bench/internal/netbench"
)

func main() {
	if len(os.Args) < 2 {
		fail(errors.New("usage: net-guest download|upload|rr|cps|url|serve [flags]"))
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	addr := fs.String("addr", "", "server address")
	secs := fs.Float64("secs", 5, "download, upload, cps: seconds")
	n := fs.Int("n", 20000, "rr: round trips")
	size := fs.Int("size", 64, "rr: bytes each way")
	load := fs.String("load", "", "rr: download from this source meanwhile")
	par := fs.Int("par", 8, "cps: parallel connectors")
	url := fs.String("url", "", "url: what to fetch")
	port := fs.Int("port", 5301, "serve: base port")
	_ = fs.Parse(os.Args[2:])
	ctx := context.Background()
	d := time.Duration(*secs * float64(time.Second))
	var r netbench.Result
	var err error
	switch os.Args[1] {
	case "download":
		r, err = netbench.Download(ctx, *addr, d)
	case "upload":
		r, err = netbench.Upload(ctx, *addr, d)
	case "rr":
		r, err = netbench.RoundTrips(ctx, *addr, *n, *size, *load)
	case "cps":
		r, err = netbench.ConnsPerSecond(ctx, *addr, d, *par)
	case "url":
		r, err = netbench.FetchURL(ctx, *url)
	case "serve":
		if _, lerr := netbench.Listen("0.0.0.0", *port); lerr != nil {
			fail(lerr)
		}
		fmt.Printf("serving on %d..%d\n", *port, *port+netbench.Kinds-1)
		select {} // until the container is stopped
	default:
		err = fmt.Errorf("unknown mode %q", os.Args[1])
	}
	if err != nil {
		fail(err)
	}
	os.Stdout.Write(r.JSON())
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "net-guest:", err)
	os.Exit(1)
}
