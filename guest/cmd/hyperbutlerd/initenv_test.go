package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/Higangssh/hyperbutler/internal/dockerenv"
)

// initPath is the guest init, which reads the dockerd.env this package writes.
const initPath = "../../prototype/init"

// trickyDoc has values a shell would expand or split if the init evaluated the file.
var trickyDoc = dockerenv.Doc{
	HTTPProxy:  "http://user:pa$(touch%20x)';*@proxy.corp:3128",
	HTTPSProxy: "http://us$HOME:p;w=1&x@127.0.0.1:3128",
	NoProxy:    "localhost,*.corp,10.0.0.0/8",
}

// TestDockerdEnvContract checks what files writes against what the init's reader
// (`while IFS= read -r kv; ... export "$kv"`) needs (review L-F): one KEY=VALUE per
// line, the last one ended by a newline (read drops an unterminated line), only the
// names the init exports, and no byte in a value that would end or split a line.
func TestDockerdEnvContract(t *testing.T) {
	e := &dockerdEnv{bundle: "/run/hyperbutler/ca-certificates.crt", stock: filepath.Join(t.TempDir(), "none")}
	doc := trickyDoc
	if err := doc.Validate(); err != nil {
		t.Fatal(err)
	}
	env, _, _, err := e.files(&doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(env) == 0 || env[len(env)-1] != '\n' {
		t.Fatalf("env %q does not end with a newline", env)
	}
	for _, l := range strings.Split(strings.TrimSuffix(string(env), "\n"), "\n") {
		k, v, ok := strings.Cut(l, "=")
		if !ok || !slices.Contains(dockerenv.EnvNames, k) || v == "" || strings.ContainsAny(v, "\r\x00") {
			t.Errorf("line %q", l)
		}
	}
	// The init's case pattern names exactly dockerenv.EnvNames.
	b, err := os.ReadFile(initPath)
	if err != nil {
		t.Fatal(err)
	}
	var pats []string
	for _, n := range dockerenv.EnvNames {
		pats = append(pats, n+"=*")
	}
	if !bytes.Contains(b, []byte(strings.Join(pats, " | ")+") export \"$kv\"")) {
		t.Errorf("the init does not export exactly %v", dockerenv.EnvNames)
	}
}

// TestInitReadsDockerdEnv runs the init's own reader (the lines between its
// `if [ -f "$DOCKERD_ENV" ]` and `fi`) in sh on what files writes, where there is a
// POSIX sh (the guest's is BusyBox), and checks dockerd would get the values byte for
// byte, nothing evaluated. It also checks that the whole init parses (sh -n).
func TestInitReadsDockerdEnv(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs a POSIX sh; TestDockerdEnvContract covers the format")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	if out, err := exec.Command(sh, "-n", initPath).CombinedOutput(); err != nil {
		t.Fatalf("sh -n init: %v\n%s", err, out)
	}
	b, err := os.ReadFile(initPath)
	if err != nil {
		t.Fatal(err)
	}
	var block []string
	in := false
	for _, l := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(l) == `if [ -f "$DOCKERD_ENV" ]; then` {
			in = true
		}
		if in {
			block = append(block, l)
			if strings.TrimSpace(l) == "fi" {
				break
			}
		}
	}
	if len(block) < 3 {
		t.Fatal("the init's dockerd.env reader was not found")
	}
	dir := t.TempDir()
	e := &dockerdEnv{bundle: "/run/hyperbutler/ca-certificates.crt", stock: filepath.Join(dir, "none")}
	doc := trickyDoc
	env, _, _, err := e.files(&doc)
	if err != nil {
		t.Fatal(err)
	}
	// An unknown name in the file is not exported.
	env = append(env, "LD_PRELOAD=/tmp/x.so\n"...)
	p := filepath.Join(dir, "dockerd.env")
	if err := os.WriteFile(p, env, 0o600); err != nil {
		t.Fatal(err)
	}
	script := "DOCKERD_ENV=$1\n" + strings.Join(block, "\n") + "\n" +
		`printf '%s\n' "HTTP_PROXY=$HTTP_PROXY" "HTTPS_PROXY=$HTTPS_PROXY" "NO_PROXY=$NO_PROXY" "LD_PRELOAD=${LD_PRELOAD-unset}"` + "\n"
	cmd := exec.Command(sh, "-c", script, "sh", p)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	want := "HTTP_PROXY=" + doc.HTTPProxy + "\nHTTPS_PROXY=" + doc.HTTPSProxy + "\nNO_PROXY=" + doc.NoProxy + "\nLD_PRELOAD=unset\n"
	if string(out) != want {
		t.Fatalf("the init's reader gave\n%s\nwant\n%s", out, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "x")); err == nil {
		t.Fatal("the file was evaluated as shell code")
	}
}
