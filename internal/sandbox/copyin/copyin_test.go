package copyin

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Fixture tokens are assembled at run time so that this file does not itself look like
// a secret to scanners.
var (
	fakeGitHub = "ghp" + "_" + strings.Repeat("A1b2", 9)
	fakeAWS    = "AKIA" + "IOSFODNN7EXAMPLE"
	fakeKey    = "-----BEGIN " + "OPENSSH PRIVATE KEY-----\nabc\n"
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

func paths(p Plan) (files, skipped []string) {
	for _, f := range p.Files {
		files = append(files, f.Path)
	}
	for _, s := range p.Skipped {
		skipped = append(skipped, s.Path)
	}
	sort.Strings(files)
	sort.Strings(skipped)
	return files, skipped
}

func fixture(t *testing.T) string {
	root := t.TempDir()
	put(t, root, "main.go", "package main // password := os.Getenv(\"PASSWORD\")\n")
	put(t, root, "README.md", "hello")
	put(t, root, ".env", "TOKEN=x")
	put(t, root, ".env.example", "TOKEN=")
	put(t, root, "deploy/id_rsa", "k")
	put(t, root, "deploy/id_rsa.pub", "ssh-rsa AAAA")
	put(t, root, "config/settings.yaml", "token: "+fakeGitHub+"\n")
	put(t, root, "infra/keys.txt", "aws "+fakeAWS)
	put(t, root, "certs/server.txt", fakeKey)
	put(t, root, "build/out.bin", "ignored")
	return root
}

func TestSelectWalk(t *testing.T) {
	root := fixture(t)
	p, err := Select(root, Options{}) // no .git: a plain walk
	if err != nil {
		t.Fatal(err)
	}
	files, skipped := paths(p)
	if want := ".env.example README.md build/out.bin deploy/id_rsa.pub main.go"; strings.Join(files, " ") != want {
		t.Errorf("files %q, want %q", files, want)
	}
	if want := ".env certs/server.txt config/settings.yaml deploy/id_rsa infra/keys.txt"; strings.Join(skipped, " ") != want {
		t.Errorf("skipped %q, want %q", skipped, want)
	}
	if p.FromGit {
		t.Error("FromGit without a repository")
	}
}

func TestSelectGit(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	root := fixture(t)
	put(t, root, ".gitignore", "build/\n")
	for _, args := range [][]string{{"init", "-q"}, {"add", "main.go"}} {
		cmd := exec.Command(git, append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=nul", "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	p, err := Select(root, Options{Include: []string{"deploy/id_rsa"}})
	if err != nil {
		t.Fatal(err)
	}
	files, skipped := paths(p)
	if want := ".env.example .gitignore README.md deploy/id_rsa deploy/id_rsa.pub main.go"; strings.Join(files, " ") != want {
		t.Errorf("files %q, want %q", files, want)
	}
	if want := ".env certs/server.txt config/settings.yaml infra/keys.txt"; strings.Join(skipped, " ") != want {
		t.Errorf("skipped %q, want %q", skipped, want)
	}
	if !p.FromGit {
		t.Error("not FromGit in a repository")
	}
}

func TestScanLongLine(t *testing.T) {
	// A token across the 1 MiB line buffer's edge is still found.
	s := strings.Repeat("x", (1<<20)-10) + " " + fakeGitHub + "\n"
	if got, err := Scan(strings.NewReader(s)); err != nil || got == "" {
		t.Errorf("Scan = %q, %v", got, err)
	}
	if got, _ := Scan(strings.NewReader("const apiKey = process.env.API_KEY\nsk-short\n")); got != "" {
		t.Errorf("ordinary code flagged as %s", got)
	}
}

func FuzzScan(f *testing.F) {
	f.Add([]byte(fakeKey))
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = Scan(strings.NewReader(string(b)))
	})
}
