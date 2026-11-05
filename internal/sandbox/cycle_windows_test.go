// Package sandbox_test runs a session's project cycle without a VM: copy in, an agent's
// edits, the host listing and reading the changes, and apply.
package sandbox_test

import (
	"bytes"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/Higangssh/hyperbutler/internal/sandbox/apply"
	"github.com/Higangssh/hyperbutler/internal/sandbox/copyin"
	"github.com/Higangssh/hyperbutler/internal/sandbox/review"
	"github.com/Higangssh/hyperbutler/internal/sandbox/wire"
	"github.com/Higangssh/hyperbutler/internal/sandbox/workspace"
)

func put(t *testing.T, root, rel, s string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCycle(t *testing.T) {
	project := t.TempDir()
	put(t, project, "main.go", "package main\n")
	put(t, project, "util/strings.go", "package util\n")
	put(t, project, "old.txt", "remove me")
	put(t, project, "edited-on-host.txt", "v1")
	put(t, project, ".env", "SECRET=1")

	// Copy in.
	plan, err := copyin.Select(project, copyin.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var stream bytes.Buffer
	sent, err := copyin.WriteTar(&stream, project, plan)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := sent[".env"]; ok {
		t.Fatal(".env was sent into the session")
	}
	ws, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	base, err := workspace.Extract(ws, &stream)
	if err != nil {
		t.Fatal(err)
	}

	// The agent works.
	wsDir := ws.Name()
	put(t, wsDir, "main.go", "package main\n\nfunc main() {}\n")
	put(t, wsDir, "new/feature.go", "package feature\n")
	put(t, wsDir, ".github/workflows/ci.yml", "on: push\n")
	put(t, wsDir, "edited-on-host.txt", "agent")
	put(t, wsDir, ".git/hooks/pre-commit", "evil")
	if err := os.Remove(filepath.Join(wsDir, "old.txt")); err != nil {
		t.Fatal(err)
	}
	// Meanwhile the user edits a file on the host.
	put(t, project, "edited-on-host.txt", "v2 by the user")

	// The host reads the changes.
	a, b := net.Pipe()
	defer a.Close()
	go func() { _ = workspace.Serve(b, ws, base); b.Close() }()
	c := wire.NewClient(a)
	list, err := c.List()
	if err != nil {
		t.Fatal(err)
	}
	var changes []apply.Change
	var listed []string
	for _, e := range list {
		listed = append(listed, e.Path)
		ch := apply.Change{Path: e.Path, Delete: e.Kind == wire.Deleted}
		if s, ok := sent[e.Path]; ok {
			ch.Base = &s.Sum
		}
		if !ch.Delete {
			data, err := c.ReadFile(e.Path, 1<<20)
			if err != nil {
				t.Fatalf("read %s: %v", e.Path, err)
			}
			ch.Data = data
		}
		if review.Classify(e.Path, e.Flags&wire.FlagExec != 0) == review.RiskCI && ch.Delete {
			t.Errorf("%s: a CI deletion", e.Path)
		}
		changes = append(changes, ch)
	}
	want := []string{".github/workflows/ci.yml", "edited-on-host.txt", "main.go", "new/feature.go", "old.txt"}
	if len(listed) != len(want) {
		t.Fatalf("listed %q, want %q", listed, want)
	}
	for i := range want {
		if listed[i] != want[i] {
			t.Fatalf("listed %q, want %q", listed, want)
		}
	}

	// Apply all of it.
	out, err := apply.Apply(project, changes)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range out {
		wantErr := error(nil)
		if o.Path == "edited-on-host.txt" {
			wantErr = apply.ErrConflict
		}
		if !errors.Is(o.Err, wantErr) {
			t.Errorf("%s: %v, want %v", o.Path, o.Err, wantErr)
		}
	}
	for p, w := range map[string]string{
		"main.go": "package main\n\nfunc main() {}\n", "new/feature.go": "package feature\n",
		".github/workflows/ci.yml": "on: push\n", "edited-on-host.txt": "v2 by the user", ".env": "SECRET=1",
	} {
		got, err := os.ReadFile(filepath.Join(project, filepath.FromSlash(p)))
		if err != nil || string(got) != w {
			t.Errorf("%s = %q (%v), want %q", p, got, err, w)
		}
	}
	if _, err := os.Stat(filepath.Join(project, "old.txt")); !os.IsNotExist(err) {
		t.Errorf("old.txt: %v", err)
	}
	if _, err := os.Stat(filepath.Join(project, ".git")); !os.IsNotExist(err) {
		t.Errorf("a .git change reached the project: %v", err)
	}
}
