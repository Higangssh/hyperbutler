//go:build windows

package main

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/Higangssh/hyperbutler/internal/desktopimport"
)

func TestParseNumbers(t *testing.T) {
	for in, want := range map[string][]int{
		"":         nil,
		"  ":       nil,
		"all":      {0, 1, 2, 3},
		"1":        {0},
		"1,3-4":    {0, 2, 3},
		"2, 2,1-2": {1, 0},
	} {
		got, err := parseNumbers(in, 4)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	for _, in := range []string{"0", "5", "x", "3-1", "1-", "-2", "1,,2"} {
		if _, err := parseNumbers(in, 4); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}

func TestImportUsage(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"import"}, &out, &errb); code != 2 || !strings.Contains(errb.String(), "--from-desktop") {
		t.Errorf("no source: %d %q", code, errb.String())
	}
	errb.Reset()
	// Not a console and nothing picked: say what to pick (before touching any engine).
	if code := run([]string{"import", "--from-desktop"}, &out, &errb); code != 2 || !strings.Contains(errb.String(), "--all-images") {
		t.Errorf("nothing picked: %d %q", code, errb.String())
	}
	errb.Reset()
	if code := run([]string{"import", "-h"}, &out, &errb); code != 0 || strings.Contains(errb.String(), "from-pipe") {
		t.Errorf("help: %d %q", code, errb.String())
	}
}

func TestWriteResult(t *testing.T) {
	var b bytes.Buffer
	res := &desktopimport.Result{
		Items: []desktopimport.ItemResult{
			{Kind: "image", Name: "busybox:latest", Outcome: desktopimport.OutcomeAlready},
			{Kind: "image", Name: "alpine:3.22", Outcome: desktopimport.OutcomeAlready},
			{Kind: "volume", Name: "pg", Outcome: desktopimport.OutcomeAlready},
		},
		Source: desktopimport.SourceCheck{Unchanged: true, Images: 2, Volumes: 1, Containers: 3, HashedVolumes: 1},
	}
	writeResult(&b, res, nil)
	for _, want := range []string{
		"Nothing to import: 2 images and 1 volume already here.",
		"Docker Desktop unchanged: 2 images, 1 volume, 3 containers",
		"Containers are not moved",
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("missing %q in\n%s", want, b.String())
		}
	}
	b.Reset()
	res.Items[0].Outcome = desktopimport.OutcomeImported
	res.Items[0].Bytes, res.Bytes = 4<<20, 4<<20
	res.Source = desktopimport.SourceCheck{Differences: []string{"container \"web running x\" is now \"web exited x\""}}
	writeResult(&b, res, []string{"a note"})
	for _, want := range []string{"Imported 1, tagged 0, already here 2", "4.0 MiB copied", "Docker Desktop CHANGED", "is now", "Note: a note"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("missing %q in\n%s", want, b.String())
		}
	}
}
