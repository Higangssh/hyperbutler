// Package diag holds the runtime limits and the local debug endpoint that every
// long-running hyperbutler process uses (ADR 0003, guardrail 4).
//
//   - ApplyMemoryLimit sets the Go soft memory limit from configuration unless the
//     GOMEMLIMIT environment variable already sets one. The environment wins so an
//     operator can always override a shipped default.
//   - Serve exposes pprof, runtime.MemStats, the goroutine count and the process handle
//     count on a named pipe that only the current user can open. It is off unless the
//     process passes a pipe name.
package diag

import (
	"errors"
	"fmt"
	"math"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
)

// Config is the diag part of a long-running process's configuration. The zero value
// changes nothing and serves nothing.
type Config struct {
	// MemoryLimit is the Go soft memory limit, in GOMEMLIMIT syntax ("512MiB",
	// "268435456", "off"). Empty leaves the runtime default. Ignored when the
	// GOMEMLIMIT environment variable is set.
	MemoryLimit string
	// DebugPipe, when non-empty, is the named pipe the debug endpoint listens on, for
	// example `\\.\pipe\hyperbutler-dev-debug`. Empty means no endpoint (the default).
	DebugPipe string
}

// MinMemoryLimit is the smallest limit ApplyMemoryLimit accepts. Below it the GC runs
// almost continuously, which is a misconfiguration rather than a limit.
const MinMemoryLimit = 4 << 20

// maxLimitLen bounds the input ParseMemoryLimit looks at.
const maxLimitLen = 64

// ParseMemoryLimit parses a limit in the syntax the Go runtime accepts for GOMEMLIMIT:
// a non-negative decimal integer with an optional unit suffix B, KiB, MiB, GiB or TiB,
// or the word "off" (no limit, returned as math.MaxInt64). No sign, spaces, fractions
// or SI units (KB, MB) are accepted, and a value that overflows int64 is an error.
func ParseMemoryLimit(s string) (int64, error) {
	if len(s) > maxLimitLen {
		return 0, errors.New("diag: memory limit is too long")
	}
	if s == "off" {
		return math.MaxInt64, nil
	}
	num := s
	mult := int64(1)
	for _, u := range []struct {
		suffix string
		mult   int64
	}{
		{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}, {"B", 1},
	} {
		if strings.HasSuffix(s, u.suffix) {
			num, mult = s[:len(s)-len(u.suffix)], u.mult
			break
		}
	}
	if num == "" {
		return 0, fmt.Errorf("diag: memory limit %q has no number", s)
	}
	for i := 0; i < len(num); i++ {
		if num[i] < '0' || num[i] > '9' {
			return 0, fmt.Errorf("diag: memory limit %q is not a byte count", s)
		}
	}
	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("diag: memory limit %q is out of range", s)
	}
	if n > math.MaxInt64/mult {
		return 0, fmt.Errorf("diag: memory limit %q is out of range", s)
	}
	return n * mult, nil
}

// Start applies cfg: the memory limit, then the debug endpoint if cfg.DebugPipe is set.
// The returned Server is nil when the endpoint is off; (*Server)(nil).Close is a no-op.
func Start(cfg Config) (*Server, error) {
	if _, _, err := ApplyMemoryLimit(cfg.MemoryLimit); err != nil {
		return nil, err
	}
	if cfg.DebugPipe == "" {
		return nil, nil
	}
	return Serve(cfg.DebugPipe)
}

// LimitSource says where the effective memory limit came from.
type LimitSource string

const (
	// SourceEnv means GOMEMLIMIT was set in the environment; the runtime already applied
	// it at start-up and the configured value was ignored.
	SourceEnv LimitSource = "env"
	// SourceConfig means the configured value was applied.
	SourceConfig LimitSource = "config"
	// SourceDefault means neither was set; the runtime default (no limit) stays.
	SourceDefault LimitSource = "default"
)

// ApplyMemoryLimit applies limit (GOMEMLIMIT syntax) with debug.SetMemoryLimit unless
// the GOMEMLIMIT environment variable is set to a non-empty value. It returns the limit
// now in effect and where it came from. A limit below MinMemoryLimit is an error.
func ApplyMemoryLimit(limit string) (int64, LimitSource, error) {
	return applyMemoryLimit(limit, os.LookupEnv, debug.SetMemoryLimit)
}

func applyMemoryLimit(limit string, lookupEnv func(string) (string, bool), set func(int64) int64) (int64, LimitSource, error) {
	if v, ok := lookupEnv("GOMEMLIMIT"); ok && v != "" {
		// The runtime parsed it at start-up (and refused to start if it was invalid).
		// A negative argument only reads the current limit.
		return set(-1), SourceEnv, nil
	}
	if limit == "" {
		return set(-1), SourceDefault, nil
	}
	n, err := ParseMemoryLimit(limit)
	if err != nil {
		return 0, "", err
	}
	if n < MinMemoryLimit {
		return 0, "", fmt.Errorf("diag: memory limit %q is below the %d MiB minimum", limit, MinMemoryLimit>>20)
	}
	set(n)
	return n, SourceConfig, nil
}

// Stats is the document served at /debug/stats.
type Stats struct {
	// Goroutines is runtime.NumGoroutine().
	Goroutines int `json:"goroutines"`
	// Handles is the number of open kernel handles (Windows) or file descriptors
	// (Linux), or -1 if it could not be read.
	Handles int64 `json:"handles"`
	// MemoryLimit is the current Go soft memory limit in bytes.
	MemoryLimit int64 `json:"memory_limit"`
	// MemStats is runtime.ReadMemStats, without the per-size-class table.
	MemStats MemStats `json:"memstats"`
	// OS is the operating system's view of the process memory (Windows only), so that
	// memory outside the Go runtime shows: private bytes minus what Go mapped
	// (MemStats.Sys - HeapReleased) is DLL heaps, thread stacks and other native
	// allocations.
	OS *OSMemory `json:"os,omitempty"`
	// WorkingSetTrims is how often the WorkingSetGuard emptied the working set.
	WorkingSetTrims int64 `json:"working_set_trims"`
}

// OSMemory is the operating system's accounting of the process's memory.
type OSMemory struct {
	PrivateBytes uint64 `json:"private_bytes"` // committed private memory
	WorkingSet   uint64 `json:"working_set"`   // resident, private and shared
	// Heaps is the number of Windows heaps (-1: too many to read), and HeapCommitted
	// and HeapAllocated sum their HeapSummary: memory native code took with HeapAlloc
	// or malloc, invisible to the Go runtime.
	Heaps         int    `json:"heaps"`
	HeapCommitted uint64 `json:"heap_committed"`
	HeapAllocated uint64 `json:"heap_allocated"`
}

// MemStats is the subset of runtime.MemStats that tells a heap leak from GC slack.
type MemStats struct {
	HeapAlloc    uint64 `json:"heap_alloc"`
	HeapInuse    uint64 `json:"heap_inuse"`
	HeapIdle     uint64 `json:"heap_idle"`
	HeapReleased uint64 `json:"heap_released"`
	HeapObjects  uint64 `json:"heap_objects"`
	StackInuse   uint64 `json:"stack_inuse"`
	Sys          uint64 `json:"sys"`
	TotalAlloc   uint64 `json:"total_alloc"`
	Mallocs      uint64 `json:"mallocs"`
	Frees        uint64 `json:"frees"`
	NumGC        uint32 `json:"num_gc"`
	PauseTotalNs uint64 `json:"pause_total_ns"`
	NextGC       uint64 `json:"next_gc"`
}

// Snapshot reads the current Stats. It stops the world briefly (runtime.ReadMemStats).
func Snapshot() Stats {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return Stats{
		Goroutines:      runtime.NumGoroutine(),
		Handles:         handleCount(),
		MemoryLimit:     debug.SetMemoryLimit(-1),
		OS:              osMemory(),
		WorkingSetTrims: workingSetTrims.Load(),
		MemStats: MemStats{
			HeapAlloc:    m.HeapAlloc,
			HeapInuse:    m.HeapInuse,
			HeapIdle:     m.HeapIdle,
			HeapReleased: m.HeapReleased,
			HeapObjects:  m.HeapObjects,
			StackInuse:   m.StackInuse,
			Sys:          m.Sys,
			TotalAlloc:   m.TotalAlloc,
			Mallocs:      m.Mallocs,
			Frees:        m.Frees,
			NumGC:        m.NumGC,
			PauseTotalNs: m.PauseTotalNs,
			NextGC:       m.NextGC,
		},
	}
}
