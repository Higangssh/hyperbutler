// Command fs-notify measures REQ-FS-003: a Windows process edits a bind-mounted file and
// a container watching it with inotify (no polling) gets IN_MODIFY within 200 ms p95.
// The latency is measured on Windows, from just before the write to the arrival of the
// container's event line through the docker CLI, so it includes that hop.
//
// With -frameworks it also checks that pinned versions of Vite and webpack, with their
// default configuration, rebuild when a Windows process edits a source file: Vite's dev
// server must push an HMR message to a connected client, and webpack --watch must
// compile again. Their packages live in a named volume (node_modules on a bind mount is
// what REQ-FS-005 measures, not this).
//
//	fs-notify -docker C:\...\docker.exe [-n 100] [-frameworks]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Higangssh/hyperbutler/bench/internal/fsbench"
)

func main() {
	fsbench.Exit("fs-notify", run())
}

// Pinned framework versions (REQ-FS-003).
const (
	viteVersion       = "5.4.11"
	webpackVersion    = "5.97.1"
	webpackCLIVersion = "5.1.4"
)

const (
	inModify = 0x2
	inCreate = 0x100
)

func run() error {
	var c fsbench.Config
	fs := flag.NewFlagSet("fs-notify", flag.ExitOnError)
	c.Flags(fs)
	n := fs.Int("n", 100, "edits of each kind")
	gap := fs.Duration("gap", 50*time.Millisecond, "pause between edits")
	frameworks := fs.Bool("frameworks", false, "also run the Vite and webpack hot-reload check (needs network the first time)")
	node := fs.String("node-image", "node:22-alpine", "image for the framework check")
	_ = fs.Parse(os.Args[1:])
	if err := c.Prepare(); err != nil {
		return err
	}
	dir := filepath.Join(c.Work, "notify", "watched")
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o750); err != nil {
		return err
	}
	// One file per modify edit, made before the watcher starts: a single Windows write
	// can produce more than one IN_MODIFY, and a late one must not be taken for the
	// next edit's.
	editFile := func(i int) string { return filepath.Join(dir, "sub", fmt.Sprintf("edit-%d.txt", i)) }
	for i := 1; i <= *n; i++ {
		if err := os.WriteFile(editFile(i), []byte("0\n"), 0o600); err != nil {
			return err
		}
	}
	st, err := c.Start("hyperbutler-fs-notify", c.Image, []fsbench.Mount{fsbench.Bind(dir, "/w")}, "exec /bench/fs-guest notify -dir /w")
	if err != nil {
		return err
	}
	defer st.Stop()
	if _, err := st.Wait(30*time.Second, func(s string) bool { return strings.Contains(s, `"ready"`) }); err != nil {
		return fmt.Errorf("watcher: %w", err)
	}
	type event struct {
		Name string `json:"name"`
		Path string `json:"path"`
		Mask uint32 `json:"mask"`
	}
	is := func(path string, mask uint32) func(string) bool {
		return func(s string) bool {
			var e event
			return json.Unmarshal([]byte(s), &e) == nil && e.Name == "event" && e.Path == path && e.Mask&mask != 0
		}
	}
	measure := func(kind string, do func(i int) (string, uint32, error)) (lat []float64, missed int, err error) {
		for i := 1; i <= *n; i++ {
			time.Sleep(*gap)
			st.Drain()
			t0 := time.Now()
			path, mask, err := do(i)
			if err != nil {
				return nil, 0, err
			}
			l, err := st.Wait(2*time.Second, is(path, mask))
			if err != nil {
				missed++
				continue
			}
			lat = append(lat, l.At.Sub(t0).Seconds())
		}
		return lat, missed, nil
	}
	modLat, modMissed, err := measure("modify", func(i int) (string, uint32, error) {
		return fmt.Sprintf("/w/sub/edit-%d.txt", i), inModify, os.WriteFile(editFile(i), []byte(fmt.Sprintf("%d\n", i)), 0o600)
	})
	if err != nil {
		return err
	}
	createLat, createMissed, err := measure("create", func(i int) (string, uint32, error) {
		name := fmt.Sprintf("new-%d.txt", i)
		return "/w/sub/" + name, inCreate, os.WriteFile(filepath.Join(dir, "sub", name), nil, 0o600)
	})
	if err != nil {
		return err
	}
	st.Stop()
	row := func(what string, lat []float64, missed int, limit float64) fsbench.Row {
		p50, p95 := fsbench.Quantile(append([]float64(nil), lat...), 0.5), fsbench.Quantile(append([]float64(nil), lat...), 0.95)
		r := fsbench.Row{What: what, Bind: fmt.Sprintf("p50 %s, p95 %s, missed %d of %d", fsbench.Dur(p50), fsbench.Dur(p95), missed, *n)}
		if limit > 0 {
			r.Req, r.Limit, r.Verdict = "REQ-FS-003", fmt.Sprintf("p95 ≤ %s, none missed", fsbench.Dur(limit)), "pass"
			if missed > 0 || !(p95 <= limit) {
				r.Verdict = "FAIL"
			}
		} else {
			r.Verdict = "info"
		}
		return r
	}
	rows := []fsbench.Row{
		row("IN_MODIFY after a Windows write", modLat, modMissed, 0.2),
		row("IN_CREATE after a Windows create (context)", createLat, createMissed, 0),
	}
	if *frameworks {
		fr, err := frameworkCheck(&c, *node)
		if err != nil {
			return err
		}
		rows = append(rows, fr...)
	} else {
		rows = append(rows, fsbench.Row{Req: "REQ-FS-003", What: "Vite and webpack hot reload", Verdict: "not checked (pass -frameworks)"})
	}
	fsbench.Report(os.Stdout, fmt.Sprintf("bench/fs-notify: %d edits of each kind, %v apart, latency measured on Windows", *n, *gap), rows)
	if err := c.Save(map[string]any{"machine": c.Machine(), "rows": rows, "modify_s": modLat, "create_s": createLat}); err != nil {
		return err
	}
	return fsbench.Verdict(rows)
}

// viteClient loads the page's module into Vite's module graph and prints every HMR
// message Vite sends to a connected client (Node 22 has a global WebSocket).
const viteClient = `
const base = 'http://127.0.0.1:5173';
for (let i = 0; ; i++) {
  try { await fetch(base + '/'); break; } catch (e) { if (i > 300) throw e; await new Promise(r => setTimeout(r, 100)); }
}
await (await fetch(base + '/src/main.js')).text();
const ws = new WebSocket('ws://127.0.0.1:5173/', 'vite-hmr');
ws.onmessage = (m) => {
  const d = JSON.parse(m.data);
  if (d.type === 'connected') console.log('vite-ready');
  if (d.type === 'update' || d.type === 'full-reload') console.log('vite-hmr ' + d.type);
};
`

func frameworkCheck(c *fsbench.Config, node string) ([]fsbench.Row, error) {
	app := filepath.Join(c.Work, "notify", "app")
	if err := os.MkdirAll(filepath.Join(app, "src"), 0o750); err != nil {
		return nil, err
	}
	pkg := fmt.Sprintf(`{"private":true,"devDependencies":{"vite":"%s","webpack":"%s","webpack-cli":"%s"}}`, viteVersion, webpackVersion, webpackCLIVersion)
	files := map[string]string{
		"package.json":    pkg,
		"index.html":      `<!doctype html><script type="module" src="/src/main.js"></script>`,
		"src/main.js":     "console.log('main 0')\n",
		"src/index.js":    "console.log('index 0')\n",
		"vite-client.mjs": viteClient,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(app, name), []byte(body), 0o600); err != nil {
			return nil, err
		}
	}
	mounts := []fsbench.Mount{fsbench.Bind(app, "/app"), {"-v", c.Volume + "-node-modules:/app/node_modules"}, {"-v", c.Volume + "-npm:/root/.npm"}}
	fmt.Fprintln(os.Stderr, "fs-notify: npm install (vite, webpack)")
	if _, err := c.DockerCmd(append(append([]string{"run", "--rm"}, flat(mounts)...), "-w", "/app", node,
		"npm", "install", "--no-audit", "--no-fund", "--no-package-lock", "--loglevel=error")...); err != nil {
		return nil, err
	}
	var rows []fsbench.Row
	check := func(what, name, script, ready, rebuilt, file string) error {
		st, err := c.Start(name, node, mounts, "cd /app && "+script)
		if err != nil {
			return err
		}
		defer st.Stop()
		if _, err := st.Wait(90*time.Second, func(s string) bool { return strings.Contains(s, ready) }); err != nil {
			return fmt.Errorf("%s did not start: %w", what, err)
		}
		time.Sleep(500 * time.Millisecond)
		st.Drain()
		t0 := time.Now()
		if err := os.WriteFile(filepath.Join(app, file), []byte(fmt.Sprintf("console.log('edited %d')\n", t0.UnixNano())), 0o600); err != nil {
			return err
		}
		r := fsbench.Row{Req: "REQ-FS-003", What: what, Limit: "rebuilds after a Windows edit"}
		if l, err := st.Wait(10*time.Second, func(s string) bool { return strings.Contains(s, rebuilt) }); err != nil {
			r.Bind, r.Verdict = "no rebuild within 10 s", "FAIL"
		} else {
			r.Bind, r.Verdict = "rebuilt after "+fsbench.Dur(l.At.Sub(t0).Seconds()), "pass"
		}
		rows = append(rows, r)
		return nil
	}
	if err := check("Vite "+viteVersion+" dev server, default config: HMR message", "hyperbutler-fs-vite",
		"(node_modules/.bin/vite --host 127.0.0.1 --port 5173 --strictPort >/dev/null 2>&1 &) && node vite-client.mjs",
		"vite-ready", "vite-hmr", "src/main.js"); err != nil {
		return nil, err
	}
	if err := check("webpack "+webpackVersion+" --watch, default config: recompiles", "hyperbutler-fs-webpack",
		"node_modules/.bin/webpack --watch --mode development 2>&1",
		"compiled", "compiled", "src/index.js"); err != nil {
		return nil, err
	}
	return rows, nil
}

func flat(ms []fsbench.Mount) []string {
	var out []string
	for _, m := range ms {
		out = append(out, m...)
	}
	return out
}
