package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"

	"github.com/Higangssh/winbox/internal/diag"
)

// ProcSample is one process's counters from Win32_PerfFormattedData_PerfProc_Process.
type ProcSample struct {
	PID            uint32
	WorkingSetPriv uint64 // WorkingSetPrivate, bytes: the REQ-MEM-001 metric
	HandleCount    uint64
	ThreadCount    uint64
	PrivateBytes   uint64 // PrivateBytes: committed private memory, bytes
}

// cimFields is the number of comma-separated fields cimScript prints per process.
const cimFields = 5

// maxCIMOutput bounds what ParseCIM reads; a few processes produce well under 1 KiB.
const maxCIMOutput = 64 << 10

// ParseCIM parses cimScript's output: one line per process,
//
//	IDProcess,WorkingSetPrivate,HandleCount,ThreadCount,PrivateBytes
//
// every field an unsigned decimal integer. Blank lines and surrounding spaces or CR are
// ignored. Anything else (a PowerShell error text, a field that is empty, signed,
// fractional or out of range, a PID of 0, or a PID that appears twice) is an error, so a
// broken probe can never produce a plausible-looking sample.
func ParseCIM(out string) (map[uint32]ProcSample, error) {
	if len(out) > maxCIMOutput {
		return nil, errors.New("soak: CIM output too large")
	}
	res := make(map[uint32]ProcSample)
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 0, 4096), maxCIMOutput)
	line := 0
	for sc.Scan() {
		line++
		s := strings.TrimSpace(sc.Text())
		if s == "" {
			continue
		}
		f := strings.Split(s, ",")
		if len(f) != cimFields {
			return nil, fmt.Errorf("soak: CIM line %d: %d fields, want %d", line, len(f), cimFields)
		}
		var n [cimFields]uint64
		for i, field := range f {
			v, err := parseUint(field)
			if err != nil {
				return nil, fmt.Errorf("soak: CIM line %d field %d: %w", line, i+1, err)
			}
			n[i] = v
		}
		if n[0] == 0 || n[0] > math.MaxUint32 {
			return nil, fmt.Errorf("soak: CIM line %d: bad PID %d", line, n[0])
		}
		pid := uint32(n[0])
		if _, dup := res[pid]; dup {
			return nil, fmt.Errorf("soak: CIM line %d: PID %d twice", line, pid)
		}
		res[pid] = ProcSample{
			PID: pid, WorkingSetPriv: n[1], HandleCount: n[2], ThreadCount: n[3],
			PrivateBytes: n[4],
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("soak: CIM output: %w", err)
	}
	return res, nil
}

// maxStatsBody bounds a /debug/stats document.
const maxStatsBody = 64 << 10

// parseStats decodes one /debug/stats document. Unknown fields, trailing data, a
// negative or zero goroutine count, and a handle count below -1 are errors.
func parseStats(r io.Reader) (diag.Stats, error) {
	var s diag.Stats
	dec := json.NewDecoder(io.LimitReader(r, maxStatsBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return diag.Stats{}, fmt.Errorf("soak: diag stats: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return diag.Stats{}, errors.New("soak: diag stats: trailing data")
	}
	if s.Goroutines < 1 || s.Handles < -1 {
		return diag.Stats{}, fmt.Errorf("soak: diag stats: implausible goroutines=%d handles=%d", s.Goroutines, s.Handles)
	}
	return s, nil
}

func parseUint(s string) (uint64, error) {
	if s == "" || len(s) > 20 {
		return 0, fmt.Errorf("bad number %q", s)
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, fmt.Errorf("bad number %q", s)
		}
	}
	return strconv.ParseUint(s, 10, 64)
}
