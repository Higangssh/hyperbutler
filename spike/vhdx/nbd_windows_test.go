package main

import (
	"math"
	"testing"
)

func TestNBDInRange(t *testing.T) {
	const size = 2 << 30
	for _, tc := range []struct {
		name   string
		off, n int64
		want   bool
	}{
		{"whole export", 0, size, true},
		{"empty at end", size, 0, true},
		{"last byte", size - 1, 1, true},
		{"one past end", size - 1, 2, false},
		{"offset past end", size + 1, 0, false},
		{"negative offset (uint64 >= 2^63 on the wire)", -1, 1, false},
		{"offset near 2^63 must not wrap", math.MaxInt64 - 0xff, 0x1000, false},
		{"max length", 0, math.MaxUint32, false},
	} {
		if got := nbdInRange(tc.off, tc.n, size); got != tc.want {
			t.Errorf("%s: nbdInRange(%d, %d) = %v, want %v", tc.name, tc.off, tc.n, got, tc.want)
		}
	}
}

func FuzzNBDInRange(f *testing.F) {
	f.Add(int64(0), uint32(512), int64(1<<30))
	f.Add(int64(math.MaxInt64-0xff), uint32(0x1000), int64(1<<30))
	f.Fuzz(func(t *testing.T, off int64, n uint32, size int64) {
		if size < 0 {
			return
		}
		if nbdInRange(off, int64(n), size) {
			if off < 0 || off > size || int64(n) > size-off {
				t.Fatalf("accepted out-of-range request off=%d n=%d size=%d", off, n, size)
			}
		}
	})
}
