package main

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// table prints aligned columns.
type table struct{ w *tabwriter.Writer }

func newTable(out io.Writer, header ...string) *table {
	t := &table{w: tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)}
	t.row(header...)
	return t
}

func (t *table) row(cells ...string) {
	for i, c := range cells {
		// Values can come from settings.json; keep them on one line and printable.
		cells[i] = strings.NewReplacer("\t", " ", "\n", " ").Replace(string(printable([]byte(c))))
	}
	fmt.Fprintln(t.w, strings.Join(cells, "\t"))
}

func (t *table) flush() { _ = t.w.Flush() }
