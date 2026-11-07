package review

import (
	"bytes"
	"fmt"
	"strings"
)

// Diff limits. Past them a file is summed up in a line: a review still lists it, and the
// bytes it would write are the same.
const (
	maxDiffLines   = 20000
	maxDiffLine    = 4096
	maxDiffText    = 1 << 20
	diffContext    = 3
	binaryScanSize = 8192
)

// Diff is one file's change as text. Everything from the guest in Text went through
// Escape, a line at a time, so a line cannot pretend to be another or move the cursor
// (REQ-SBX-009).
type Diff struct {
	Added, Removed int
	Binary         bool
	Text           string
}

// LooksBinary reports whether b holds a NUL byte near its start, git's own test.
func LooksBinary(b []byte) bool {
	return bytes.IndexByte(b[:min(len(b), binaryScanSize)], 0) >= 0
}

// TextDiff compares old and new. It trims the common start and end, then takes the
// longest common subsequence of what is left when that is small enough; otherwise it
// shows the middle as replaced, which is correct and only longer.
func TextDiff(old, new []byte) Diff {
	if LooksBinary(old) || LooksBinary(new) {
		return Diff{Binary: true}
	}
	a, b := splitLines(old), splitLines(new)
	if len(a) > maxDiffLines || len(b) > maxDiffLines {
		return Diff{Added: len(b), Removed: len(a), Text: fmt.Sprintf("(%d lines to %d lines: too long to show)\n", len(a), len(b))}
	}
	start := 0
	for start < len(a) && start < len(b) && a[start] == b[start] {
		start++
	}
	endA, endB := len(a), len(b)
	for endA > start && endB > start && a[endA-1] == b[endB-1] {
		endA--
		endB--
	}
	ops := lcsOps(a[start:endA], b[start:endB])

	d := Diff{}
	var sb strings.Builder
	line := func(prefix byte, s string) {
		if sb.Len() < maxDiffText {
			sb.WriteByte(prefix)
			if len(s) > maxDiffLine {
				sb.WriteString(Escape(s[:maxDiffLine]))
				fmt.Fprintf(&sb, " (cut, %d bytes)", len(s))
			} else {
				sb.WriteString(Escape(s))
			}
			sb.WriteByte('\n')
		}
	}
	if start == endA && start == endB {
		return d
	}
	first := max(start-diffContext, 0)
	if first > 0 {
		fmt.Fprintf(&sb, "@@ line %d @@\n", first+1)
	}
	for _, s := range a[first:start] {
		line(' ', s)
	}
	i, j := start, start
	for _, op := range ops {
		switch op {
		case opKeep:
			line(' ', a[i])
			i++
			j++
		case opDel:
			line('-', a[i])
			d.Removed++
			i++
		case opAdd:
			line('+', b[j])
			d.Added++
			j++
		}
	}
	for _, s := range a[endA:min(endA+diffContext, len(a))] {
		line(' ', s)
	}
	if sb.Len() >= maxDiffText {
		sb.WriteString("(the rest of this diff is not shown)\n")
	}
	d.Text = sb.String()
	return d
}

// splitLines cuts at \n, keeping a final line without one; a \r stays in the line, where
// Escape shows it.
func splitLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	s := string(b)
	lines := strings.Split(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

const (
	opKeep = iota
	opDel
	opAdd
)

// maxLCSCells bounds the table of lcsOps, here about 32 MiB of int32.
const maxLCSCells = 8 << 20

// lcsOps returns the edit from a to b.
func lcsOps(a, b []string) []int {
	ops := make([]int, 0, len(a)+len(b))
	if len(a) == 0 || len(b) == 0 || (len(a)+1)*(len(b)+1) > maxLCSCells {
		for range a {
			ops = append(ops, opDel)
		}
		for range b {
			ops = append(ops, opAdd)
		}
		return ops
	}
	w := len(b) + 1
	t := make([]int32, (len(a)+1)*w)
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				t[i*w+j] = t[(i+1)*w+j+1] + 1
			} else {
				t[i*w+j] = max(t[(i+1)*w+j], t[i*w+j+1])
			}
		}
	}
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			ops = append(ops, opKeep)
			i++
			j++
		case t[(i+1)*w+j] >= t[i*w+j+1]:
			ops = append(ops, opDel)
			i++
		default:
			ops = append(ops, opAdd)
			j++
		}
	}
	for ; i < len(a); i++ {
		ops = append(ops, opDel)
	}
	for ; j < len(b); j++ {
		ops = append(ops, opAdd)
	}
	return ops
}
