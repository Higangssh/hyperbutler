//go:build windows

package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/engine/dockerctx"
	"github.com/Higangssh/hyperbutler/internal/transport"
)

// Other Docker engines on this machine (REQ-UX-018): hyperbutler only looks at whether
// their pipes exist, never connects to them.
const (
	desktopLinuxPipe  = `\\.\pipe\dockerDesktopLinuxEngine`
	defaultEnginePipe = `\\.\pipe\docker_engine`
)

// enginePipe is another engine's pipe and who serves it.
type enginePipe struct {
	Pipe string `json:"pipe"`
	Who  string `json:"who"`
}

var otherEnginePipes = []enginePipe{
	{desktopLinuxPipe, "Docker Desktop's Linux engine"},
	{defaultEnginePipe, "Docker Desktop, or Docker Engine for Windows containers"},
}

// pipeExists reports whether a pipe of that name exists, by listing the pipe file
// system (FindFirstFile), which neither connects to it nor uses up one of its
// instances. A variable for the tests.
var pipeExists = func(pipe string) bool {
	p, err := windows.UTF16PtrFromString(pipe)
	if err != nil {
		return false
	}
	var fd windows.Win32finddata
	h, err := windows.FindFirstFile(p, &fd)
	if err != nil {
		return false
	}
	_ = windows.FindClose(h)
	return true
}

// otherEngines lists the other engines' pipes that exist; ours (the pipe this
// installation serves, which may be docker_engine by choice) is not one of them.
func otherEngines(ours string) []enginePipe {
	var out []enginePipe
	for _, e := range otherEnginePipes {
		if strings.EqualFold(e.Pipe, ours) {
			continue
		}
		if pipeExists(e.Pipe) {
			out = append(out, e)
		}
	}
	return out
}

// dockerTarget says what a plain `docker` in this shell talks to: DOCKER_HOST, else
// DOCKER_CONTEXT, else config.json's selected context. hyperbutler is true when that is
// this installation (its context, or its pipe in DOCKER_HOST).
type dockerTarget struct {
	What    string // `context "desktop-linux"`, `DOCKER_HOST=npipe:...`
	Context string // the context name, "" for DOCKER_HOST
	FromEnv bool   // DOCKER_HOST or DOCKER_CONTEXT of this shell decides
	Ours    bool
}

func currentTarget(getenv func(string) string, store dockerctx.Store, ourPipe string) (dockerTarget, error) {
	if h := getenv("DOCKER_HOST"); h != "" {
		return dockerTarget{What: "DOCKER_HOST=" + h, FromEnv: true, Ours: strings.EqualFold(h, npipeHost(ourPipe))}, nil
	}
	if c := getenv("DOCKER_CONTEXT"); c != "" {
		return dockerTarget{What: fmt.Sprintf("context %q (DOCKER_CONTEXT in this shell)", c), Context: c, FromEnv: true, Ours: c == contextName}, nil
	}
	cur, err := store.Current()
	if err != nil {
		return dockerTarget{}, err
	}
	if cur == "" {
		cur = "default"
	}
	t := dockerTarget{What: fmt.Sprintf("context %q", cur), Context: cur, Ours: cur == contextName}
	if cur == "default" && strings.EqualFold(ourPipe, defaultEnginePipe) {
		t.Ours = true // docker's default endpoint is this installation's pipe
	}
	return t, nil
}

// noteOtherEngines is start's note when another Docker engine runs (REQ-UX-018): which
// one, and which engine a plain `docker` now reaches. It changes nothing.
func noteOtherEngines(w io.Writer, ourPipe string) {
	others := otherEngines(ourPipe)
	if len(others) == 0 {
		return
	}
	for _, e := range others {
		_, _ = fmt.Fprintf(w, "Another Docker engine is running: %s (%s).\n", e.Who, e.Pipe)
	}
	store, err := dockerctx.DefaultStore()
	if err != nil {
		return
	}
	t, err := currentTarget(os.Getenv, store, ourPipe)
	if err != nil {
		return
	}
	back := "docker context use default"
	if _, err := store.Get(desktopContext); err == nil {
		back = "docker context use " + desktopContext
	}
	switch {
	case t.Ours:
		_, _ = fmt.Fprintf(w, "Plain `docker` now uses hyperbutler (%s); `%s` switches back. The engines keep separate images, containers and volumes.\n", t.What, back)
	case t.Context == "default":
		_, _ = fmt.Fprintf(w, "Plain `docker` uses %s, which is %s, not hyperbutler. `docker context use %s` switches to hyperbutler, or `docker --context %s ...` for one command.\n", t.What, defaultEnginePipe, contextName, contextName)
	default:
		_, _ = fmt.Fprintf(w, "Plain `docker` uses %s, not hyperbutler. `docker context use %s` switches to hyperbutler, or `docker --context %s ...` for one command.\n", t.What, contextName, contextName)
	}
}

// desktopInstalled reports whether Docker Desktop is installed (its Docker Desktop.exe
// in %ProgramFiles%\Docker\Docker). A variable for the tests.
var desktopInstalled = func() bool {
	pf := os.Getenv("ProgramFiles")
	if pf == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(pf, "Docker", "Docker", "Docker Desktop.exe"))
	return err == nil
}

// warnDefaultPipe is `config set pipe \\.\pipe\docker_engine`'s warning (REQ-API-003):
// Docker Desktop serves that pipe too, so whichever starts second fails.
func warnDefaultPipe(w io.Writer, pipe string) {
	if !strings.EqualFold(pipe, defaultEnginePipe) {
		return
	}
	running := pipeExists(defaultEnginePipe)
	if !running && !desktopInstalled() {
		return
	}
	why := "Docker Desktop is installed and serves the same pipe"
	if running {
		why = "another engine serves that pipe right now"
	}
	_, _ = fmt.Fprintf(w, "Warning: %s: whichever of the two starts second fails (hyperbutler start with pipe-in-use).\n"+
		"Tools that ignore docker contexts can use DOCKER_HOST=%s instead (docs/tools.md).\n", why, npipeHost(transport.DefaultPipeName))
}
