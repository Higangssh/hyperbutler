//go:build windows

package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Higangssh/hyperbutler/internal/engine/dockerctx"
	"github.com/Higangssh/hyperbutler/internal/transport"
)

// TestPipeExistsDoesNotConnect: an existing pipe is found by listing, and its server
// sees no connection.
func TestPipeExistsDoesNotConnect(t *testing.T) {
	var rnd [6]byte
	_, _ = rand.Read(rnd[:])
	pipe := `\\.\pipe\hyperbutler-test-exists-` + hex.EncodeToString(rnd[:])
	if pipeExists(pipe) {
		t.Fatal("a pipe that was never made exists")
	}
	ln, err := transport.ListenPipe(pipe)
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan struct{})
	go func() {
		if c, err := ln.Accept(); err == nil {
			_ = c.Close()
			close(accepted)
		}
	}()
	if !pipeExists(pipe) {
		t.Fatal("the pipe was not found")
	}
	if !pipeExists(strings.ToUpper(pipe)) {
		t.Fatal("pipe names are not case-sensitive")
	}
	select {
	case <-accepted:
		t.Fatal("pipeExists connected to the pipe")
	case <-time.After(300 * time.Millisecond):
	}
	_ = ln.Close()
	if pipeExists(pipe) {
		t.Fatal("the pipe still exists after its listener closed")
	}
}

func fakePipes(t *testing.T, present ...string) {
	t.Helper()
	old := pipeExists
	pipeExists = func(p string) bool {
		for _, q := range present {
			if strings.EqualFold(p, q) {
				return true
			}
		}
		return false
	}
	t.Cleanup(func() { pipeExists = old })
}

// TestNoteOtherEngines: start names the other engine and which one plain docker
// reaches, and says nothing when there is none (REQ-UX-018).
func TestNoteOtherEngines(t *testing.T) {
	_, dockerCfg := testHome(t)
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	store := dockerctx.Store{Dir: dockerCfg}
	ours := transport.DefaultPipeName

	fakePipes(t)
	var b bytes.Buffer
	noteOtherEngines(&b, ours)
	if b.Len() != 0 {
		t.Fatalf("no other engine: %q", b.String())
	}

	fakePipes(t, desktopLinuxPipe, defaultEnginePipe, ours)
	if err := store.Ensure(desktopContext, "npipe:////./pipe/dockerDesktopLinuxEngine", "Docker Desktop"); err != nil {
		t.Fatal(err)
	}
	if err := store.Use(contextName); err != nil {
		t.Fatal(err)
	}
	b.Reset()
	noteOtherEngines(&b, ours)
	out := b.String()
	for _, want := range []string{"Docker Desktop's Linux engine (" + desktopLinuxPipe + ")", defaultEnginePipe, "Plain `docker` now uses hyperbutler (context \"hyperbutler\")", "`docker context use desktop-linux` switches back"} {
		if !strings.Contains(out, want) {
			t.Errorf("selected: %q lacks %q", out, want)
		}
	}
	if err := store.Use(desktopContext); err != nil {
		t.Fatal(err)
	}
	b.Reset()
	noteOtherEngines(&b, ours)
	if !strings.Contains(b.String(), "Plain `docker` uses context \"desktop-linux\", not hyperbutler") {
		t.Errorf("not selected: %q", b.String())
	}
	t.Setenv("DOCKER_CONTEXT", "hyperbutler")
	b.Reset()
	noteOtherEngines(&b, ours)
	if !strings.Contains(b.String(), "now uses hyperbutler (context \"hyperbutler\" (DOCKER_CONTEXT in this shell))") {
		t.Errorf("DOCKER_CONTEXT: %q", b.String())
	}
	// With hyperbutler on docker_engine itself, that pipe is not another engine.
	b.Reset()
	t.Setenv("DOCKER_CONTEXT", "")
	fakePipes(t, defaultEnginePipe)
	noteOtherEngines(&b, defaultEnginePipe)
	if b.Len() != 0 {
		t.Errorf("our own docker_engine: %q", b.String())
	}
	// Nothing was selected by the note.
	if cur, _ := store.Current(); cur != desktopContext {
		t.Fatalf("the selection changed to %q", cur)
	}
}

// TestStatusNamesSelectedContext: status says which context docker uses when it is not
// hyperbutler's, and never changes it (REQ-UX-018).
func TestStatusNamesSelectedContext(t *testing.T) {
	_, dockerCfg := testHome(t)
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	store := dockerctx.Store{Dir: dockerCfg}
	if err := store.Ensure(contextName, npipeHost(transport.DefaultPipeName), contextDescription); err != nil {
		t.Fatal(err)
	}
	if err := store.Use(desktopContext); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := runCLI(t, "config", "set", "updateCheck", "false"); code != 0 {
		t.Fatal("config set")
	}
	_, out, _ := runCLI(t, "status")
	if !strings.Contains(out, `context: hyperbutler (not selected; docker uses "desktop-linux")`) {
		t.Fatalf("status: %q", out)
	}
	_, out, _ = runCLI(t, "status", "--json")
	if !strings.Contains(out, `"dockerContextCurrent": "desktop-linux"`) {
		t.Fatalf("status --json: %q", out)
	}
	if cur, _ := store.Current(); cur != desktopContext {
		t.Fatalf("status changed the selection to %q", cur)
	}
}

// TestWarnDefaultPipe: config set pipe \\.\pipe\docker_engine warns when Docker Desktop
// is installed or its pipe exists, and not otherwise (REQ-API-003).
func TestWarnDefaultPipe(t *testing.T) {
	old := desktopInstalled
	t.Cleanup(func() { desktopInstalled = old })
	installed := false
	desktopInstalled = func() bool { return installed }
	fakePipes(t)
	var b bytes.Buffer
	warnDefaultPipe(&b, `\\.\pipe\docker_engine`)
	if b.Len() != 0 {
		t.Fatalf("no Desktop: %q", b.String())
	}
	installed = true
	warnDefaultPipe(&b, `\\.\PIPE\Docker_Engine`)
	if !strings.Contains(b.String(), "Docker Desktop is installed") || !strings.Contains(b.String(), "DOCKER_HOST=npipe:////./pipe/docker_engine_hyperbutler") {
		t.Fatalf("installed: %q", b.String())
	}
	b.Reset()
	warnDefaultPipe(&b, `\\.\pipe\docker_engine_hb2`)
	if b.Len() != 0 {
		t.Fatalf("another pipe: %q", b.String())
	}
	// Through config set.
	testHome(t)
	code, out, _ := runCLI(t, "config", "set", "pipe", `\\.\pipe\docker_engine`)
	if code != 0 || !strings.Contains(out, "Warning: Docker Desktop is installed") {
		t.Fatalf("config set: %d %q", code, out)
	}
}

// TestCredHelperWarnings: a credsStore or credHelpers helper that is not on PATH is
// named with the fix, and config.json is not changed (REQ-UX-019).
func TestCredHelperWarnings(t *testing.T) {
	_, dockerCfg := testHome(t)
	if err := os.MkdirAll(dockerCfg, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := `{"credsStore":"desktop","credHelpers":{"gcr.io":"gcloud","ok.example":"wincred"}}`
	p := filepath.Join(dockerCfg, "config.json")
	if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	old := lookPath
	t.Cleanup(func() { lookPath = old })
	onPath := map[string]bool{"docker-credential-wincred": true}
	lookPath = func(f string) (string, error) {
		if onPath[f] {
			return `C:\bin\` + f + ".exe", nil
		}
		return "", os.ErrNotExist
	}
	var e bytes.Buffer
	warnCredHelpers(&e)
	out := e.String()
	for _, want := range []string{`config.json's credsStore is "desktop", but docker-credential-desktop.exe is not on PATH, so docker login and pull will fail`,
		`Set "credsStore" to "wincred" in ` + p, "names docker-credential-gcloud.exe for gcr.io", `remove the "gcr.io" entry`, "hyperbutler does not change it"} {
		if !strings.Contains(out, want) {
			t.Errorf("%q lacks %q", out, want)
		}
	}
	if strings.Contains(out, "ok.example") {
		t.Errorf("a helper on PATH was named: %q", out)
	}
	// Without wincred, the fix is to remove the line.
	delete(onPath, "docker-credential-wincred")
	e.Reset()
	warnCredHelpers(&e)
	if !strings.Contains(e.String(), `Remove the "credsStore" line`) {
		t.Errorf("no wincred: %q", e.String())
	}
	if b, _ := os.ReadFile(p); string(b) != cfg {
		t.Fatal("config.json changed")
	}
	// All on PATH: nothing.
	onPath["docker-credential-desktop"], onPath["docker-credential-gcloud"], onPath["docker-credential-wincred"] = true, true, true
	e.Reset()
	warnCredHelpers(&e)
	if e.Len() != 0 {
		t.Errorf("all on PATH: %q", e.String())
	}
}
