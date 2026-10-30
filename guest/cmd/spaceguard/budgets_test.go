package main

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/Higangssh/hyperbutler/internal/spaceguard"
)

func TestReadBudgets(t *testing.T) {
	in := "budget 5 7\n" +
		"garbage\n" +
		"budget " + strings.Repeat("9", 500) + "\n" + // over-long: dropped whole
		"budget 1 2\r\n" +
		"budget unlimited\n" +
		"budget 3 4" // no newline before EOF: not a complete line
	var got []spaceguard.Budget
	err := readBudgets(strings.NewReader(in), func(e event) { got = append(got, e.b) })
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want EOF", err)
	}
	want := []spaceguard.Budget{{Bytes: 5, Backed: 7}, {Bytes: 1, Backed: 2}, {Unlimited: true}}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	}
}
