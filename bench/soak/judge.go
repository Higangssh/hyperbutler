package main

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"time"
)

// readCSV reads a soak CSV (-out) back into the series report judges. It finds columns
// by name, so CSVs written before a column was added still read; the adjusted metrics
// are always recomputed from ws_private, private_bytes and heap_released, so CSVs
// written before they existed are judged the same way as new ones. It returns the
// metrics whose column is missing or has no value at all.
func readCSV(r io.Reader) (map[string][]Point, []string, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	header, err := cr.Read()
	if err != nil {
		return nil, nil, fmt.Errorf("CSV header: %w", err)
	}
	col := map[string]int{}
	for i, h := range header {
		col[h] = i
	}
	if _, ok := col["t_seconds"]; !ok {
		return nil, nil, errors.New("CSV has no t_seconds column")
	}
	num := func(rec []string, name string) (float64, bool, error) {
		i, ok := col[name]
		if !ok || i >= len(rec) || rec[i] == "" {
			return 0, false, nil
		}
		v, err := strconv.ParseFloat(rec[i], 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, false, fmt.Errorf("bad %s value %q", name, rec[i])
		}
		return v, true, nil
	}
	series := map[string][]Point{}
	for line := 2; ; line++ {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		t, ok, err := num(rec, "t_seconds")
		if err != nil {
			return nil, nil, fmt.Errorf("line %d: %w", line, err)
		}
		if !ok {
			return nil, nil, fmt.Errorf("line %d: no t_seconds", line)
		}
		row := map[string]float64{}
		for _, m := range metrics {
			if isAdjusted(m) {
				continue
			}
			v, ok, err := num(rec, m)
			if err != nil {
				return nil, nil, fmt.Errorf("line %d: %w", line, err)
			}
			if ok {
				row[m] = v
			}
		}
		rel, ok, err := num(rec, "heap_released")
		if err != nil {
			return nil, nil, fmt.Errorf("line %d: %w", line, err)
		}
		if ok {
			addAdjusted(row, rel)
		}
		for _, m := range metrics {
			if v, ok := row[m]; ok {
				series[m] = append(series[m], Point{T: t, V: v})
			}
		}
		trims, ok, err := num(rec, trimsSeries)
		if err != nil {
			return nil, nil, fmt.Errorf("line %d: %w", line, err)
		}
		if ok {
			series[trimsSeries] = append(series[trimsSeries], Point{T: t, V: trims})
		}
	}
	var empty []string
	for _, m := range metrics {
		if len(series[m]) == 0 {
			empty = append(empty, m)
		}
	}
	return series, empty, nil
}

// judgeCSV re-judges a recorded run from its CSV with the trend analysis and limits of
// c, without running it again. Metrics with no samples in the CSV are skipped, since the
// CSV does not record whether they were opted out. A window shorter than c.minWindow
// fails; when c.minWindow is 0 it is 80 % of the recorded time after the warm-up, as a
// live run's default is of -duration minus -warmup.
func judgeCSV(out io.Writer, r io.Reader, c verdictConfig) error {
	series, empty, err := readCSV(r)
	if err != nil {
		return err
	}
	if len(empty) > 0 {
		_, _ = fmt.Fprintf(out, "judge: no samples in the CSV for %v; they are skipped\n", empty)
	}
	c.optedOut = map[string]bool{}
	for _, m := range empty {
		c.optedOut[m] = true
	}
	if c.minWindow == 0 {
		var last float64
		for _, pts := range series {
			for _, p := range pts {
				last = max(last, p.T)
			}
		}
		if rec := time.Duration(last*float64(time.Second)) - c.warmup; rec > 0 {
			c.minWindow = rec * 8 / 10
		}
	}
	if !judge(out, c, series) {
		return errors.New("FAIL: a metric trends upward beyond noise, or has too little data")
	}
	_, _ = fmt.Fprintln(out, "PASS")
	return nil
}
