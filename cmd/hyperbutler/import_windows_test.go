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
	// PowerShell splits an unquoted comma list into two arguments: say so (L10).
	for _, args := range [][]string{
		{"import", "--from-desktop", "--volume", "pgdata", "cache"},
		{"import", "--from-desktop", "--image=busybox", "alpine"},
	} {
		errb.Reset()
		if code := run(args, &out, &errb); code != 2 || !strings.Contains(errb.String(), `In PowerShell, quote it`) {
			t.Errorf("%v: %d %q", args, code, errb.String())
		}
	}
	errb.Reset()
	if code := run([]string{"import", "--from-desktop", "--all-images", "x"}, &out, &errb); code != 2 || strings.Contains(errb.String(), "PowerShell") {
		t.Errorf("no list flag: %d %q", code, errb.String())
	}
	errb.Reset()
	if code := run([]string{"import", "--from-desktop", "--container", "web", "db"}, &out, &errb); code != 2 || !strings.Contains(errb.String(), `"--container=a,b"`) {
		t.Errorf("container list: %d %q", code, errb.String())
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
		"No container was created",
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

// The plan shows a container's reason, its capabilities and what stays behind, names a
// Compose project instead of offering its containers, and asks to confirm what needs it.
func TestContainerPlanText(t *testing.T) {
	p := &desktopimport.Plan{Containers: []desktopimport.ContainerPlan{
		{Name: "web", Image: "myapp:1", Status: desktopimport.StatusNew, CapAdd: []string{"NET_ADMIN"}, WritableBytes: 5,
			Notes: []string{"its writable layer holds 5 B and is not copied"}, Ports: []string{"8080->80/tcp"}},
		{Name: "priv", Image: "myapp:1", Status: desktopimport.StatusConfirm, Requires: []string{"it runs privileged"},
			Reason: "skipped unless confirmed (--confirm-container priv): it runs privileged"},
		{Name: "no", Image: "myapp:1", Status: desktopimport.StatusConfirm, Requires: []string{"it uses host networking"}},
		{Name: "shop-web-1", Status: desktopimport.StatusCompose, Project: "shop", ProjectDir: `C:\work\shop`},
		{Name: "shop-db-1", Status: desktopimport.StatusCompose, Project: "shop", ProjectDir: `C:\work\shop`},
	}}
	var b bytes.Buffer
	writePlan(&b, p, true)
	for _, want := range []string{
		"Containers (3; created in hyperbutler, not started)", "adds the capabilities NET_ADMIN", "not copied",
		"publishes 8080->80/tcp", "needs your confirmation", "--confirm-container priv",
		`Compose project shop (shop-web-1, shop-db-1) is not recreated: run docker compose up in C:\work\shop.`,
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("missing %q in\n%s", want, b.String())
		}
	}
	b.Reset()
	// No images or volumes to ask about: containers 1-3, yes to the first confirmation.
	sel, err := askSelection(strings.NewReader("1-3\ny\nn\n"), &b, p, desktopimport.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sel.Containers, []string{"web"}) || !slices.Equal(sel.ConfirmContainers, []string{"priv"}) {
		t.Errorf("selection %+v", sel)
	}
	if !strings.Contains(b.String(), "it runs privileged") {
		t.Errorf("the question does not say why: %s", b.String())
	}
}

func TestWriteResultContainers(t *testing.T) {
	var b bytes.Buffer
	writeResult(&b, &desktopimport.Result{
		Items:  []desktopimport.ItemResult{{Kind: "container", Name: "web", Outcome: desktopimport.OutcomeImported, Reason: "created, not started"}},
		Source: desktopimport.SourceCheck{Unchanged: true},
	}, nil)
	if !strings.Contains(b.String(), "created, not started") || !strings.Contains(b.String(), "docker start them") {
		t.Errorf("%s", b.String())
	}
	b.Reset()
	writeResult(&b, &desktopimport.Result{
		Items:  []desktopimport.ItemResult{{Kind: "container", Name: "web", Outcome: desktopimport.OutcomeAlready}},
		Source: desktopimport.SourceCheck{Unchanged: true},
	}, nil)
	if !strings.Contains(b.String(), "Nothing to import: 0 images and 0 volumes, 1 container already here.") {
		t.Errorf("%s", b.String())
	}
}

func TestContainerPlanJSON(t *testing.T) {
	j := containerPlanJSON(desktopimport.ContainerPlan{Name: "web", ID: "abc", Status: desktopimport.StatusConfirm, Image: "myapp:1",
		WritableBytes: -1, Requires: []string{"r"}, CapAdd: []string{"c"}})
	if j.Kind != "container" || j.Status != "needs-confirm" || j.Image != "myapp:1" || *j.WritableBytes != -1 || j.Requires[0] != "r" || j.CapAdd[0] != "c" {
		t.Errorf("%+v", j)
	}
}
