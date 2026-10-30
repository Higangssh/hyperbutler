package policy

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

func at(s float64) time.Time { return t0.Add(time.Duration(s * float64(time.Second))) }

func TestTrimPeriodic(t *testing.T) {
	p := DefaultTrim
	if r := p.Observe(at(0), 1<<30); r != NoTrim {
		t.Fatal(r)
	}
	for s := 1.0; s < 120; s++ {
		if r := p.Observe(at(s), 1<<30); r != NoTrim {
			t.Fatalf("at %vs: %v", s, r)
		}
	}
	if r := p.Observe(at(120), 1<<30); r != TrimPeriodic {
		t.Fatalf("got %v", r)
	}
	p.Trimmed(at(121), 1<<30)
	if r := p.Observe(at(122), 1<<30); r != NoTrim {
		t.Fatal(r)
	}
}

func TestTrimAfterDelete(t *testing.T) {
	p := DefaultTrim
	p.Observe(at(0), 500<<20)
	p.Observe(at(1), 100<<20) // writes
	// A prune frees 400 MiB over 3 s; free stops rising at 13 s.
	p.Observe(at(10), 200<<20)
	p.Observe(at(11), 300<<20)
	p.Observe(at(13), 500<<20)
	for s := 14.0; s < 19; s++ {
		if r := p.Observe(at(s), 500<<20); r != NoTrim {
			t.Fatalf("at %vs (still settling): %v", s, r)
		}
	}
	if r := p.Observe(at(19), 500<<20); r != TrimDelete {
		t.Fatalf("got %v", r)
	}
}

func TestTrimDeleteMaxDelay(t *testing.T) {
	p := DefaultTrim
	p.Observe(at(0), 0)
	free := uint64(0)
	var got TrimReason
	var when float64
	for s := 1.0; s <= 60; s++ { // free space keeps rising for a minute
		free += 10 << 20
		if r := p.Observe(at(s), free); r != NoTrim {
			got, when = r, s
			break
		}
	}
	// The jump passes 64 MiB at 7 s; MaxDelay is 30 s after that.
	if got != TrimDelete || when != 37 {
		t.Fatalf("got %v at %vs", got, when)
	}
}

func TestTrimSmallChangesIgnored(t *testing.T) {
	p := DefaultTrim
	p.Observe(at(0), 1<<30)
	for s := 1.0; s < 100; s++ {
		free := uint64(1<<30) + uint64(int(s)%2)*(32<<20)
		if r := p.Observe(at(s), free); r != NoTrim {
			t.Fatalf("at %vs: %v", s, r)
		}
	}
}

func TestTrimMinInterval(t *testing.T) {
	p := DefaultTrim
	p.Observe(at(0), 0)
	p.Trimmed(at(0), 0)
	p.Observe(at(1), 1<<30)
	for s := 2.0; s < 5; s++ {
		if r := p.Observe(at(s), 1<<30); r != NoTrim {
			t.Fatalf("at %vs: %v", s, r)
		}
	}
}

type cacheSim struct {
	t         float64
	busy, tot uint64
	io        uint64
}

func (c *cacheSim) step(dt, busyFrac float64, ioBytes uint64, cache uint64) Sample {
	c.t += dt
	j := uint64(dt * 100 * 2) // 2 CPUs, 100 Hz
	c.tot += j
	c.busy += uint64(float64(j) * busyFrac)
	c.io += ioBytes
	return Sample{T: at(c.t), CPUBusy: c.busy, CPUTotal: c.tot, PageIO: c.io, FileCache: cache}
}

func TestCacheReclaimWhenIdle(t *testing.T) {
	p := DefaultCache
	var c cacheSim
	// A workload reads 1.5 GiB: busy, lots of I/O.
	for i := 0; i < 10; i++ {
		if n := p.Observe(c.step(2, 0.5, 300<<20, 1536<<20)); n != 0 {
			t.Fatalf("reclaim while busy: %d", n)
		}
	}
	// Then idle: reclaim after 10 s.
	var got uint64
	var when float64
	for i := 0; i < 20 && got == 0; i++ {
		got = p.Observe(c.step(2, 0.01, 0, 1536<<20))
		when = c.t
	}
	if got != 1536<<20-p.Keep {
		t.Fatalf("reclaim %d", got)
	}
	if when-20 > 12 {
		t.Fatalf("reclaimed %vs after the workload stopped", when-20)
	}
	p.Reclaimed(at(c.t))
	if n := p.Observe(c.step(2, 0.01, 0, 1536<<20)); n != 0 {
		t.Fatalf("reclaim again within MinInterval: %d", n)
	}
}

func TestCacheKeepsSmallCache(t *testing.T) {
	p := DefaultCache
	var c cacheSim
	for i := 0; i < 50; i++ {
		if n := p.Observe(c.step(2, 0, 0, p.Keep+p.Threshold-1)); n != 0 {
			t.Fatalf("reclaimed %d of a small cache", n)
		}
	}
}

func TestCacheCountersBackwards(t *testing.T) {
	p := DefaultCache
	p.Observe(Sample{T: at(0), CPUTotal: 100, FileCache: 1 << 30})
	for s := 2.0; s < 30; s += 2 {
		if n := p.Observe(Sample{T: at(s), CPUTotal: 50, FileCache: 1 << 30}); n != 0 {
			t.Fatal(n)
		}
	}
}

const meminfo = `MemTotal:        3013488 kB
MemFree:         1311616 kB
MemAvailable:    2700000 kB
Cached:          1600000 kB
Shmem:            180000 kB
Active(file):     400000 kB
Inactive(file):  1020000 kB
HugePages_Total:       0
`

func TestParseMeminfo(t *testing.T) {
	m, err := ParseMeminfo([]byte(meminfo))
	if err != nil {
		t.Fatal(err)
	}
	if m.MemTotal != 3013488*1024 || m.FileCache() != 1420000*1024 || m.Shmem != 180000*1024 {
		t.Fatalf("%+v", m)
	}
	for _, bad := range []string{"", "MemFree: 1 kB\n", "MemTotal: x kB\n", "MemTotal: 1\n", "MemTotal: 18446744073709551615 kB\n"} {
		if _, err := ParseMeminfo([]byte(bad)); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestParseStat(t *testing.T) {
	busy, total, err := ParseStatCPU([]byte("cpu  10 0 5 80 5 0 0 0 0 0\ncpu0 1 2 3 4 5\n"))
	if err != nil || busy != 15 || total != 100 {
		t.Fatalf("%d %d %v", busy, total, err)
	}
	if _, _, err := ParseStatCPU([]byte("cpu0 1 2 3 4 5\n")); err == nil {
		t.Fatal("no aggregate line accepted")
	}
}

func TestParseVmstat(t *testing.T) {
	n, err := ParseVmstatPageIO([]byte("nr_free_pages 5\npgpgin 10\npgpgout 6\n"))
	if err != nil || n != 16*1024 {
		t.Fatalf("%d %v", n, err)
	}
	if _, err := ParseVmstatPageIO([]byte("pgpgin 10\n")); err == nil {
		t.Fatal("missing pgpgout accepted")
	}
}

func FuzzParseProc(f *testing.F) {
	f.Add([]byte(meminfo))
	f.Add([]byte("cpu  10 0 5 80 5 0 0 0 0 0\n"))
	f.Add([]byte("pgpgin 10\npgpgout 6\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		if m, err := ParseMeminfo(b); err == nil && m.FileCache() < m.ActiveFile {
			t.Fatal("file cache overflowed")
		}
		if busy, total, err := ParseStatCPU(b); err == nil && busy > total {
			t.Fatalf("busy %d > total %d", busy, total)
		}
		_, _ = ParseVmstatPageIO(b)
	})
}

// FuzzPolicies drives both policies with arbitrary sample streams. They must never
// panic, the trim policy never asks twice within MinInterval when every request is
// honoured, and the cache policy never asks for more than FileCache - Keep.
func FuzzPolicies(f *testing.F) {
	f.Add([]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16})
	f.Fuzz(func(t *testing.T, b []byte) {
		tp, cp := DefaultTrim, DefaultCache
		now := t0
		var lastTrim time.Time
		var s Sample
		for len(b) >= 4 {
			now = now.Add(time.Duration(b[0]) * 100 * time.Millisecond)
			free := uint64(b[1]) << 22
			if r := tp.Observe(now, free); r != NoTrim {
				if !lastTrim.IsZero() && now.Sub(lastTrim) < tp.MinInterval {
					t.Fatalf("trim %v after %v", r, now.Sub(lastTrim))
				}
				lastTrim = now
				tp.Trimmed(now, free)
			}
			s.T = now
			s.CPUTotal += uint64(b[2])
			s.CPUBusy += uint64(b[2] & b[3])
			s.PageIO += uint64(b[3]) << 16
			s.FileCache = uint64(b[1]^b[3]) << 22
			if n := cp.Observe(s); n != 0 {
				if s.FileCache < cp.Keep || n > s.FileCache-cp.Keep {
					t.Fatalf("reclaim %d of %d", n, s.FileCache)
				}
				cp.Reclaimed(now)
			}
			b = b[4:]
		}
	})
}
