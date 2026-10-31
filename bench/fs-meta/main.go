// Command fs-meta measures REQ-FS-002 and REQ-FS-005 on a bind-mounted checkout of a
// pinned reference repository:
//
//   - REQ-FS-002, once the guest has seen the tree: lstat of every path, lstat of a
//     missing name next to every path, and git status, each within 3x of the same work
//     on a copy of the checkout on the guest's ext4 (a named volume);
//
//   - REQ-FS-005: npm ci in the checkout, and creating, stat-ing and deleting 10k files,
//     each at least 3x as fast as on WSL 2's 9P mount of the same directory (-wsl).
//     Without -wsl the 9P rows say "not measured"; the guest ext4 times are reported
//     for context.
//
//     fs-meta -docker C:\...\docker.exe [-files 10000] [-npm=true]
//
// It needs git on Windows and network access (the checkout, git and the npm packages).
package main

import (
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Higangssh/hyperbutler/bench/internal/fsbench"
)

func main() {
	fsbench.Exit("fs-meta", run())
}

// The pinned reference repository: a JavaScript project with a package-lock.json.
const (
	defaultRepo   = "https://github.com/axios/axios.git"
	defaultRef    = "v1.7.9"
	defaultCommit = "b2cb45d5a533a5465c99559b16987e4d5fc08cbc"
)

func run() error {
	var c fsbench.Config
	fs := flag.NewFlagSet("fs-meta", flag.ExitOnError)
	c.Flags(fs)
	repo := fs.String("repo", defaultRepo, "reference repository")
	ref := fs.String("ref", defaultRef, "tag or branch to check out")
	commit := fs.String("commit", defaultCommit, "commit -ref must resolve to")
	files := fs.Int("files", 10000, "files to create, stat and delete")
	rounds := fs.Int("rounds", 5, "measured rounds of the tree workload after the warm-up")
	npm := fs.Bool("npm", true, "time npm ci (needs network the first time; the npm cache is kept in a volume)")
	node := fs.String("node-image", "node:22-alpine", "image for git status and npm ci")
	_ = fs.Parse(os.Args[1:])
	if err := c.Prepare(); err != nil {
		return err
	}
	base := filepath.Join(c.Work, "meta")
	co := filepath.Join(base, "checkout")
	if err := checkout(co, *repo, *ref, *commit); err != nil {
		return err
	}
	if err := c.ResetVolume(); err != nil {
		return err
	}
	bind := []fsbench.Mount{fsbench.Bind(base, "/w")}
	both := append(bind, fsbench.Mount{"-v", c.Volume + ":/v"})
	npmCache := fsbench.Mount{"-v", c.Volume + "-npm:/root/.npm"}
	const git = "apk add --no-cache git >/dev/null 2>&1 || exit 3; "
	tree := "/bench/fs-guest tree -git -rounds " + strconv.Itoa(*rounds) + " -dir "
	res := map[string]fsbench.Results{}
	var err error
	var out []byte
	step := func(name, image string, mounts []fsbench.Mount, script string) error {
		fmt.Fprintf(os.Stderr, "fs-meta: %s\n", name)
		res[name], out, err = c.Run(image, mounts, script)
		if err != nil {
			return fmt.Errorf("%s: %w: %s", name, err, out)
		}
		return nil
	}
	if err := step("tree-bind", *node, bind, git+tree+"/w/checkout"); err != nil {
		return err
	}
	if err := step("tree-ext4", *node, both, git+"cp -a /w/checkout /v/checkout && "+tree+"/v/checkout"); err != nil {
		return err
	}
	fcmd := func(d string) string { return "/bench/fs-guest files -n " + strconv.Itoa(*files) + " -dir " + d }
	if err := step("files-bind", c.Image, bind, fcmd("/w")); err != nil {
		return err
	}
	if err := step("files-ext4", c.Image, both, fcmd("/v")); err != nil {
		return err
	}
	if c.WSL != "" {
		res["files-9p"], err = c.WSLRun(base, "files", "-n", strconv.Itoa(*files), "-dir", "{dir}")
		if err != nil {
			fmt.Fprintln(os.Stderr, "fs-meta: 9P files:", err)
		}
	}
	if *npm {
		ci := "npm ci --ignore-scripts --no-audit --no-fund --prefer-offline --loglevel=error"
		ex := func(name, d string) string {
			return "rm -rf " + d + "/node_modules && /bench/fs-guest exec -name " + name + " -dir " + d + " -- " + ci
		}
		// The first run fills the npm cache volume (network); it is not measured.
		if err := step("npm-warm", *node, append(both, npmCache), "cp -a /w/checkout /v/npm && "+ex("warm", "/v/npm")); err != nil {
			return err
		}
		if err := step("npm-ext4", *node, append(both, npmCache), ex("npm-ci", "/v/npm")); err != nil {
			return err
		}
		if err := step("npm-bind", *node, append(bind, npmCache), ex("npm-ci", "/w/checkout")+
			" && /bench/fs-guest exec -name rm-node-modules -dir /w/checkout -- rm -rf node_modules"); err != nil {
			return err
		}
		if c.WSL != "" {
			res["npm-9p"], err = c.WSLRun(co, "exec", "-name", "npm-ci", "-dir", "{dir}", "--", "sh", "-c", "rm -rf node_modules && "+ci)
			if err != nil {
				fmt.Fprintln(os.Stderr, "fs-meta: 9P npm ci:", err)
			}
			if _, err := c.WSLRun(co, "exec", "-name", "rm", "-dir", "{dir}", "--", "rm", "-rf", "{dir}/node_modules"); err != nil {
				fmt.Fprintln(os.Stderr, "fs-meta: 9P cleanup:", err)
			}
		}
	}
	get := func(set, name, field string) float64 {
		if r, ok := res[set][name]; ok {
			return r.Num(field)
		}
		return math.NaN()
	}
	var rows []fsbench.Row
	slower := func(req, what, name, field string, scale float64) {
		b, x := get("tree-bind", name, field)*scale, get("tree-ext4", name, field)*scale
		row := fsbench.Row{Req: req, What: what, Bind: fsbench.Dur(b), Base: "ext4 " + fsbench.Dur(x), Limit: "≤ 3× ext4"}
		row.Ratio, row.Verdict = fsbench.Slower(b, x, 3)
		rows = append(rows, row)
	}
	// The same work: the total over every path and round (p50 and p99 are in -json).
	slower("REQ-FS-002", "lstat of every path, total", "lstat", "s", 1)
	slower("REQ-FS-002", "lstat of a missing name next to every path, total", "lstat-missing", "s", 1)
	slower("REQ-FS-002", "git status, median", "git-status", "p50_us", 1e-6)
	walk := fsbench.Row{What: "walk (readdir + lstat), warm, median", Bind: fsbench.Dur(get("tree-bind", "walk-warm", "p50_us") * 1e-6),
		Base: "ext4 " + fsbench.Dur(get("tree-ext4", "walk-warm", "p50_us")*1e-6)}
	walk.Ratio, walk.Verdict = fsbench.Slower(get("tree-bind", "walk-warm", "p50_us"), get("tree-ext4", "walk-warm", "p50_us"), 0)
	rows = append(rows, walk)
	faster := func(what, bindSet, name, baseSet string) {
		b, x, e := get(bindSet, name, "s"), get(baseSet, name, "s"), get(strings.Replace(bindSet, "bind", "ext4", 1), name, "s")
		row := fsbench.Row{Req: "REQ-FS-005", What: what, Bind: fsbench.Dur(b), Base: "9P " + fsbench.Dur(x) + "; ext4 " + fsbench.Dur(e), Limit: "≥ 3× faster than 9P"}
		row.Ratio, row.Verdict = fsbench.Faster(b, x, 3)
		rows = append(rows, row)
	}
	n := strconv.Itoa(*files)
	faster("create "+n+" files", "files-bind", "create", "files-9p")
	faster("stat "+n+" files", "files-bind", "stat", "files-9p")
	faster("delete "+n+" files", "files-bind", "delete", "files-9p")
	if *npm {
		faster("npm ci", "npm-bind", "npm-ci", "npm-9p")
		rm := fsbench.Row{What: "rm -rf node_modules (context)", Bind: fsbench.Dur(get("npm-bind", "rm-node-modules", "s")), Ratio: "", Verdict: "info"}
		rows = append(rows, rm)
	}
	entries := get("tree-bind", "walk-cold", "entries")
	fsbench.Report(os.Stdout, fmt.Sprintf("bench/fs-meta: %s@%s (%.0f paths), %d rounds after a warm-up", *repo, *ref, entries, *rounds), rows)
	if err := c.Save(map[string]any{"machine": c.Machine(), "rows": rows, "raw": res}); err != nil {
		return err
	}
	return fsbench.Verdict(rows)
}

// checkout clones repo at ref into dir (once) and checks the commit. Line endings are
// kept as committed, so git status in the guest sees a clean tree.
func checkout(dir, repo, ref, commit string) error {
	if _, err := os.Stat(filepath.Join(dir, ".git")); os.IsNotExist(err) {
		cmd := exec.Command("git", "-c", "core.autocrlf=false", "clone", "-q", "--depth", "1", "--branch", ref, repo, dir) //nolint:gosec // G204: command and arguments come from the operator's own flags or this tool's work directory; exec passes argv, no shell
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git clone: %w: %s", err, out)
		}
	}
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output() //nolint:gosec // G204: command and arguments come from the operator's own flags or this tool's work directory; exec passes argv, no shell
	if err != nil {
		return fmt.Errorf("git rev-parse: %w", err)
	}
	if got := strings.TrimSpace(string(out)); got != commit {
		return fmt.Errorf("%s is at %s, want %s", dir, got, commit)
	}
	return nil
}
