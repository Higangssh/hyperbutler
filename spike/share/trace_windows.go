package main

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

var traceOn = flagBool("trace", false, "print per-opcode counts and host time for the FUSE target")

type opStat struct {
	n   int
	sum time.Duration
	max time.Duration
}

var (
	traceMu sync.Mutex
	traces  = map[uint32]*opStat{}
)

func traceOp(op uint32, d time.Duration) {
	traceMu.Lock()
	s := traces[op]
	if s == nil {
		s = &opStat{}
		traces[op] = s
	}
	s.n++
	s.sum += d
	if d > s.max {
		s.max = d
	}
	traceMu.Unlock()
}

var opNames = map[uint32]string{1: "LOOKUP", 2: "FORGET", 3: "GETATTR", 4: "SETATTR", 5: "READLINK", 6: "SYMLINK", 8: "MKNOD", 9: "MKDIR",
	10: "UNLINK", 11: "RMDIR", 12: "RENAME", 13: "LINK", 14: "OPEN", 15: "READ", 16: "WRITE", 17: "STATFS", 18: "RELEASE", 20: "FSYNC",
	22: "GETXATTR", 25: "FLUSH", 26: "INIT", 27: "OPENDIR", 28: "READDIR", 29: "RELEASEDIR", 34: "ACCESS", 35: "CREATE", 42: "BATCH_FORGET",
	44: "READDIRPLUS", 45: "RENAME2"}

// dumpTrace prints and resets the per-opcode statistics.
func dumpTrace(label string) {
	traceMu.Lock()
	defer traceMu.Unlock()
	if len(traces) == 0 {
		return
	}
	var ops []uint32
	for op := range traces {
		ops = append(ops, op)
	}
	sort.Slice(ops, func(i, j int) bool { return traces[ops[i]].sum > traces[ops[j]].sum })
	fmt.Printf("  host time by opcode (%s):\n", label)
	for _, op := range ops {
		s := traces[op]
		name := opNames[op]
		if name == "" {
			name = fmt.Sprint(op)
		}
		fmt.Printf("    %-12s n=%-7d mean=%-9v max=%-9v total=%v\n", name, s.n, (s.sum / time.Duration(s.n)).Round(time.Microsecond), s.max.Round(time.Microsecond), s.sum.Round(time.Millisecond))
	}
	traces = map[uint32]*opStat{}
}
