package main

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func flatSeries() map[string][]Point {
	s := map[string][]Point{}
	for _, m := range metrics {
		s[m] = series(3600, 10, func(float64) float64 { return 100 })
	}
	return s
}

func testVerdictConfig() verdictConfig {
	lim := map[string]Limit{}
	for _, m := range metrics {
		lim[m] = Limit{Abs: 1}
	}
	return verdictConfig{warmup: 10 * time.Minute, bucket: 5 * time.Minute, limits: lim, optedOut: map[string]bool{}}
}

func TestReport(t *testing.T) {
	c := testVerdictConfig()
	if err := report(io.Discard, c, flatSeries(), 100, 0, "", nil, nil, nil, false); err != nil {
		t.Fatalf("flat run failed: %v", err)
	}

	missing := flatSeries()
	delete(missing, "goroutines")
	if err := report(io.Discard, c, missing, 100, 0, "", nil, nil, nil, false); err == nil {
		t.Fatal("run without goroutine samples passed without opting out")
	}
	c.optedOut["goroutines"] = true
	var out strings.Builder
	if err := report(&out, c, missing, 100, 0, "", nil, nil, nil, false); err != nil {
		t.Fatalf("opted-out metric failed the run: %v", err)
	}
	if !strings.Contains(out.String(), "goroutines        skipped (opted out)") {
		t.Fatalf("opt-out not reported:\n%s", out.String())
	}

	noLimit := testVerdictConfig()
	delete(noLimit.limits, "handles")
	for name, tc := range map[string]struct {
		c           verdictConfig
		ops, errs   int
		fatal       error
		interrupted bool
	}{
		"fatal":         {c: testVerdictConfig(), ops: 100, fatal: errors.New("target exited")},
		"interrupted":   {c: testVerdictConfig(), ops: 100, interrupted: true},
		"no workload":   {c: testVerdictConfig()},
		"docker errors": {c: testVerdictConfig(), ops: 100, errs: 1},
		"no limit":      {c: noLimit, ops: 100},
	} {
		if err := report(io.Discard, tc.c, flatSeries(), tc.ops, tc.errs, "", nil, nil, tc.fatal, tc.interrupted); err == nil {
			t.Errorf("%s: passed", name)
		}
	}

	leaky := flatSeries()
	leaky["handles"] = series(3600, 10, func(t float64) float64 { return 100 + t/60 })
	if err := report(io.Discard, testVerdictConfig(), leaky, 100, 0, "", nil, nil, nil, false); err == nil {
		t.Fatal("handle leak passed")
	}
}
