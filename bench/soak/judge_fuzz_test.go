package main

import (
	"io"
	"math"
	"strings"
	"testing"
	"time"
)

// FuzzReadCSV feeds readCSV arbitrary input: it must not panic, every value it returns
// must be finite, and judging what it returns must not panic either.
func FuzzReadCSV(f *testing.F) {
	f.Add("t_seconds,ws_private,private_bytes,heap_released,ws_trims\n0,1,2,0,0\n10,1,2,0,1\n")
	f.Add("t_seconds,ws_private\n0,x\n")
	f.Add("t_seconds,ws_trims\n0,1e309\n")
	f.Add("ws_private\n1\n")
	f.Add("t_seconds,t_seconds,ws_trims\n1,2\n\"3,4\n")
	f.Add(oldCSV(func(float64) float64 { return 40 * mib }, scavenged))
	f.Fuzz(func(t *testing.T, in string) {
		series, _, err := readCSV(strings.NewReader(in))
		if err != nil {
			return
		}
		for m, pts := range series {
			for _, p := range pts {
				if math.IsNaN(p.T) || math.IsInf(p.T, 0) || math.IsNaN(p.V) || math.IsInf(p.V, 0) {
					t.Fatalf("%s: non-finite point %+v", m, p)
				}
			}
		}
		c := testVerdictConfig()
		c.minWindow = time.Minute
		judge(io.Discard, c, series)
	})
}
