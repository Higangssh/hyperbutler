// Command fs-seq measures REQ-FS-001: sequential I/O on a bind mount of a Windows
// directory, against the guest's own ext4 (a named volume) and, with -wsl, WSL 2's 9P
// mount of the same directory.
//
//	fs-seq -docker C:\...\docker.exe [-host npipe:////./pipe/docker_engine_hyperbutler] [-mb 256] [-rounds 3]
//
// Pass: a cached sequential read of the bind-mounted file is within 2x of ext4, and a
// sequential write (with fsync) is at least 3x as fast as on 9P.
package main

import (
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"

	"github.com/Higangssh/hyperbutler/bench/internal/fsbench"
)

func main() {
	fsbench.Exit("fs-seq", run())
}

func run() error {
	var c fsbench.Config
	fs := flag.NewFlagSet("fs-seq", flag.ExitOnError)
	c.Flags(fs)
	mb := fs.Int("mb", 256, "MiB to write and read")
	rounds := fs.Int("rounds", 3, "runs per target; the median is reported")
	_ = fs.Parse(os.Args[1:])
	if err := c.Prepare(); err != nil {
		return err
	}
	dir := filepath.Join(c.Work, "seq")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	if err := c.ResetVolume(); err != nil {
		return err
	}
	cmd := func(d string) string { return "/bench/fs-guest seq -dir " + d + " -mb " + strconv.Itoa(*mb) }
	targets := map[string][]fsbench.Results{}
	for i := 0; i < *rounds; i++ {
		rs, out, err := c.Run(c.Image, []fsbench.Mount{fsbench.Bind(dir, "/w")}, cmd("/w"))
		if err != nil {
			return fmt.Errorf("bind: %w: %s", err, out)
		}
		targets["bind"] = append(targets["bind"], rs)
		rs, out, err = c.Run(c.Image, []fsbench.Mount{{"-v", c.Volume + ":/v"}}, cmd("/v"))
		if err != nil {
			return fmt.Errorf("ext4: %w: %s", err, out)
		}
		targets["ext4"] = append(targets["ext4"], rs)
		if c.WSL != "" {
			rs, err := c.WSLRun(dir, "seq", "-dir", "{dir}", "-mb", strconv.Itoa(*mb))
			if err != nil {
				return err
			}
			targets["9p"] = append(targets["9p"], rs)
		}
	}
	med := func(t, name string) float64 {
		var xs []float64
		for _, rs := range targets[t] {
			if r, ok := rs[name]; ok {
				xs = append(xs, r.Num("s"))
			}
		}
		return fsbench.Quantile(xs, 0.5)
	}
	rate := func(s float64) string {
		if math.IsNaN(s) {
			return "—"
		}
		return fmt.Sprintf("%.0f MiB/s", float64(*mb)/s)
	}
	var rows []fsbench.Row
	add := func(req, what, name, base string, limit float64, faster bool) {
		b, x := med("bind", name), med(base, name)
		row := fsbench.Row{Req: req, What: what, Bind: rate(b), Base: base + " " + rate(x)}
		if faster {
			row.Ratio, row.Verdict = fsbench.Faster(b, x, limit)
			row.Limit = fmt.Sprintf("≥ %.0f× faster than 9P", limit)
		} else {
			row.Ratio, row.Verdict = fsbench.Slower(b, x, limit)
			if limit > 0 {
				row.Limit = fmt.Sprintf("≤ %.0f× ext4 time", limit)
			}
		}
		rows = append(rows, row)
	}
	add("REQ-FS-001", "read, guest cache warm", "seq-read-cached", "ext4", 2, false)
	add("REQ-FS-001", "write + fsync", "seq-write-fsync", "9p", 3, true)
	add("", "write + fsync (context)", "seq-write-fsync", "ext4", 0, false)
	add("", "write, no fsync (context)", "seq-write", "ext4", 0, false)
	add("", "read, guest cache dropped (context)", "seq-read-cold", "ext4", 0, false)
	fsbench.Report(os.Stdout, fmt.Sprintf("bench/fs-seq: %d MiB, 1 MiB writes, median of %d", *mb, *rounds), rows)
	if err := c.Save(map[string]any{"machine": c.Machine(), "rows": rows, "raw": targets}); err != nil {
		return err
	}
	return fsbench.Verdict(rows)
}
