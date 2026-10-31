package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	inModify    = 0x2
	inAttrib    = 0x4
	inCloseW    = 0x8
	inMovedFrom = 0x40
	inMovedTo   = 0x80
	inCreate    = 0x100
	inDelete    = 0x200
)

// notifyTest edits files from Windows and times how long the guest's inotify watcher
// takes to report them. Latency is measured on the host clock: from just before the
// Windows call to the arrival of the event line on the control connection, so it
// includes one guest-to-host hop (see "ping" in the report).
func notifyTest(g *guest, guestDir, hostDir string, n int) map[string]float64 {
	res := map[string]float64{}
	gdir := guestDir + "/notify"
	hdir := filepath.Join(hostDir, "notify")
	_ = os.RemoveAll(hdir)
	if err := os.MkdirAll(hdir, 0o750); err != nil {
		fmt.Println("notify:", err)
		return res
	}
	if err := os.WriteFile(filepath.Join(hdir, "edit.txt"), []byte("v0"), 0o600); err != nil {
		fmt.Println("notify:", err)
		return res
	}
	// Let the guest see the directory before watching it.
	time.Sleep(300 * time.Millisecond)
	if out, err := g.sh("ls -la " + gdir); err != nil {
		fmt.Println("notify: guest ls:", err, out)
		return res
	}
	// One-way control-channel latency estimate: half a ping round trip.
	var pings []time.Duration
	for i := 0; i < 20; i++ {
		t := time.Now()
		_, _ = g.call("ping", nil)
		pings = append(pings, time.Since(t))
	}
	res["ctl_rtt_ms"] = ms(median(pings))

	if _, err := g.call("watch", map[string]string{"dir": gdir}); err != nil {
		fmt.Println("notify: watch:", err)
		return res
	}
	defer func() { _, _ = g.call("unwatch", nil) }()
	drain(g)

	type kind struct {
		name string
		do   func(i int) error
		want uint32
		file func(i int) string
	}
	kinds := []kind{
		{"modify", func(i int) error {
			return os.WriteFile(filepath.Join(hdir, "edit.txt"), []byte(fmt.Sprintf("v%d", i+1)), 0o600)
		}, inModify | inCloseW, func(int) string { return "edit.txt" }},
		{"create", func(i int) error {
			return os.WriteFile(filepath.Join(hdir, fmt.Sprintf("new%d.txt", i)), []byte("x"), 0o600)
		}, inCreate | inMovedTo, func(i int) string { return fmt.Sprintf("new%d.txt", i) }},
		{"delete", func(i int) error {
			return os.Remove(filepath.Join(hdir, fmt.Sprintf("new%d.txt", i)))
		}, inDelete | inMovedFrom, func(i int) string { return fmt.Sprintf("new%d.txt", i) }},
		{"rename", func(i int) error {
			return os.Rename(filepath.Join(hdir, "edit.txt"), filepath.Join(hdir, "edit.txt.tmp")) // then back
		}, inMovedFrom | inMovedTo | inCreate | inDelete, func(int) string { return "edit.txt" }},
	}
	for _, k := range kinds {
		var lat []time.Duration
		missed := 0
		for i := 0; i < n; i++ {
			drain(g)
			t0 := time.Now()
			if err := k.do(i); err != nil {
				fmt.Printf("notify %s: %v\n", k.name, err)
				missed++
				continue
			}
			name := k.file(i)
			got := false
			deadline := time.After(2 * time.Second)
		wait:
			for {
				select {
				case e := <-g.events:
					if e.Mask&k.want != 0 && strings.HasSuffix(e.Path, "/"+name) {
						lat = append(lat, e.At.Sub(t0))
						got = true
						break wait
					}
				case <-deadline:
					break wait
				}
			}
			if !got {
				missed++
			}
			if k.name == "rename" {
				if err := os.Rename(filepath.Join(hdir, "edit.txt.tmp"), filepath.Join(hdir, "edit.txt")); err != nil {
					fmt.Println("  rename:", err)
				}
				time.Sleep(50 * time.Millisecond)
			}
			time.Sleep(20 * time.Millisecond)
		}
		res[k.name+"_missed"] = float64(missed)
		if len(lat) > 0 {
			sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
			res[k.name+"_p50_ms"] = ms(lat[len(lat)/2])
			res[k.name+"_p95_ms"] = ms(lat[len(lat)*95/100])
			res[k.name+"_max_ms"] = ms(lat[len(lat)-1])
		}
	}
	// Coherence: does a guest read right after the event see the new content?
	if err := os.WriteFile(filepath.Join(hdir, "edit.txt"), []byte("coherent-check"), 0o600); err != nil {
		fmt.Println("  coherence write:", err)
	}
	time.Sleep(200 * time.Millisecond)
	out, _ := g.sh("cat " + gdir + "/edit.txt")
	res["read_after_edit_ok"] = b2f(strings.TrimSpace(out) == "coherent-check")
	return res
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func drain(g *guest) {
	for {
		select {
		case <-g.events:
		default:
			return
		}
	}
}

func median(d []time.Duration) time.Duration {
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[len(s)/2]
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
