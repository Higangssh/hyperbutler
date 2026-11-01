//go:build windows

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/Higangssh/hyperbutler/internal/engine"
	"github.com/Higangssh/hyperbutler/internal/engine/dockerctx"
)

// The release zip carries the docker CLI and the compose plugin in clients\ next to
// hyperbutler.exe (dist/clients.lock): clients\docker.exe and
// clients\cli-plugins\docker-compose.exe. The README puts the hyperbutler folder on PATH,
// and clients\ only for someone without a docker CLI, so a docker CLI installed later
// is not shadowed. hyperbutler never copies them anywhere and never changes PATH: a
// docker.exe already on PATH (Docker Desktop's, a standalone install, a package
// manager's) stays the one `docker` runs, and the hyperbutler context works with any
// of them.
const (
	clientsDirName = "clients"
	bundledCLIName = "docker.exe"
	composePlugin  = "docker-compose.exe"
	pluginsDirName = "cli-plugins"
)

// minCLIMajor and minCLIMinor: the oldest docker CLI the guest's dockerd accepts. Docker
// Engine 29.9.2 refuses API versions below 1.40 unless configured otherwise
// (daemon/config/config.go: defaultMinAPIVersion = "1.40"; a running guest reports
// MinAPIVersion 1.40), and API 1.40 is docker 19.03. Newer CLIs negotiate down to the
// engine's version, so there is no upper limit.
const (
	minCLIMajor, minCLIMinor = 19, 3
	minCLIVersion            = "19.03"
	minAPIVersion            = "1.40"
)

// dockerCLI is the docker.exe that a plain `docker` runs in this environment.
type dockerCLI struct {
	Path    string // "" if there is none on PATH
	Bundled string // clients\docker.exe next to hyperbutler.exe, if there is one
}

func (c dockerCLI) usesBundled() bool { return c.Path != "" && engine.SamePath(c.Path, c.Bundled) }

func exeDir() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Dir(exe)
}

// clientsDir is where the release zip puts the bundled clients, or "" if unknown.
func clientsDir() string {
	if d := exeDir(); d != "" {
		return filepath.Join(d, clientsDirName)
	}
	return ""
}

func findDockerCLI() dockerCLI {
	var c dockerCLI
	if d := clientsDir(); d != "" {
		if p := filepath.Join(d, bundledCLIName); isRegular(p) {
			c.Bundled = p
		}
	}
	// LookPath refuses a docker.exe in the current directory (exec.ErrDot), as it should.
	if p, err := exec.LookPath("docker"); err == nil {
		if abs, err := filepath.Abs(p); err == nil {
			c.Path = abs
		}
	}
	return c
}

var dockerVersionRE = regexp.MustCompile(`(?m)^Docker version v?(\d+)\.(\d+)`)

// parseCLIVersion reads `docker --version` output ("Docker version 29.9.2, build a82e528").
func parseCLIVersion(out string) (major, minor int, ok bool) {
	m := dockerVersionRE.FindStringSubmatch(out)
	if m == nil {
		return 0, 0, false
	}
	major, err1 := strconv.Atoi(m[1])
	minor, err2 := strconv.Atoi(m[2])
	return major, minor, err1 == nil && err2 == nil
}

func cliTooOld(major, minor int) bool {
	return major < minCLIMajor || major == minCLIMajor && minor < minCLIMinor
}

// cliVersion runs `docker --version` (which contacts no engine) and parses it; ok is
// false when it fails or prints something else (a wrapper, an unusual build).
func cliVersion(path string) (major, minor int, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return 0, 0, false
	}
	return parseCLIVersion(string(out))
}

// pathAdvice is the PowerShell line that puts dir first on this shell's PATH.
func pathAdvice(dir string) string { return fmt.Sprintf(`$env:Path = "%s;$env:Path"`, dir) }

// checkDockerCLI says which docker CLI `docker` runs and warns when it is too old for
// the guest's dockerd, with the fix. It runs only `docker --version`, which does not
// contact any engine.
func checkDockerCLI(c dockerCLI, stdout, stderr io.Writer) {
	if c.Path == "" {
		if c.Bundled != "" {
			_, _ = fmt.Fprintf(stderr, "hyperbutler: note: no docker CLI on PATH. hyperbutler comes with one, %s; add its folder to your PATH (README, Quick start), or for this shell:\n          %s\n",
				c.Bundled, pathAdvice(filepath.Dir(c.Bundled)))
		} else {
			_, _ = fmt.Fprintln(stderr, "hyperbutler: note: no docker CLI on PATH; the Docker API is still served on the pipe above.")
		}
		return
	}
	major, minor, ok := cliVersion(c.Path)
	which := "on PATH"
	if c.usesBundled() {
		which = "bundled"
	}
	if !ok {
		_, _ = fmt.Fprintf(stdout, "docker CLI: %s (%s).\n", c.Path, which)
		return // a wrapper or an unusual build: nothing more to say
	}
	if !cliTooOld(major, minor) {
		_, _ = fmt.Fprintf(stdout, "docker CLI: %s (%s, %d.%d).\n", c.Path, which, major, minor)
		return
	}
	_, _ = fmt.Fprintf(stderr, "hyperbutler: warning: the docker CLI on PATH, %s, is version %d.%d. hyperbutler's Docker Engine needs API %s (docker %s) or later, so its commands will fail with \"client version ... is too old\".\n",
		c.Path, major, minor, minAPIVersion, minCLIVersion)
	if c.Bundled != "" {
		_, _ = fmt.Fprintf(stderr, "        Use the docker CLI that comes with hyperbutler, %s: put its folder first on PATH, or for this shell:\n          %s\n",
			c.Bundled, pathAdvice(filepath.Dir(c.Bundled)))
	} else {
		_, _ = fmt.Fprintln(stderr, "        Update the docker CLI, or use the one in the hyperbutler release zip (clients\\docker.exe).")
	}
}

// syncCompose offers the bundled compose plugin to the docker CLI as a fallback (see
// dockerctx.SyncPluginDir): it is used only while no other docker-compose is in the
// CLI's plugin search. It runs with the context management (context.manage), since it
// edits the same config.json.
func syncCompose(store dockerctx.Store, stdout, stderr io.Writer) {
	d := clientsDir()
	if d == "" || !isRegular(filepath.Join(d, pluginsDirName, composePlugin)) {
		return
	}
	var system []string
	if pf := os.Getenv("ProgramFiles"); pf != "" {
		system = append(system, filepath.Join(pf, "Docker", pluginsDirName))
	}
	r, err := store.SyncPluginDir(filepath.Join(d, pluginsDirName), composePlugin, system)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "hyperbutler: docker compose plugin:", err)
		return
	}
	switch {
	case r.Changed && r.Listed:
		_, _ = fmt.Fprintf(stdout, "docker compose: the bundled plugin is registered (%s, in %s).\n", r.Active, filepath.Join(store.Dir, "config.json"))
	case r.Changed:
		_, _ = fmt.Fprintf(stdout, "docker compose: %s takes over; the bundled plugin is no longer registered.\n", r.Active)
	}
}
