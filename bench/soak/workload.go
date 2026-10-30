package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Workload runs the mixed docker loop against one engine endpoint with the stock CLI.
type Workload struct {
	Docker    string        // path to docker.exe
	Host      string        // -H value, e.g. npipe:////./pipe/docker_engine_hyperbutler
	ImageTar  string        // `docker load` input
	Image     string        // name:tag inside ImageTar; holds /bin/busybox
	OpTimeout time.Duration // per docker invocation
	Pause     time.Duration // between iterations
	ConfigDir string        // DOCKER_CONFIG, so the user's contexts and config never apply

	// ShareDir, when set, is a Windows directory the loop bind-mounts (ADR 0007): a
	// container reads a file written on the host and writes one back.
	ShareDir string
	// Port, when non-zero (and ShareDir is set), adds published-port traffic: a busybox
	// httpd container serves ShareDir\www through `-p`, and the host fetches the page.
	// Ports Port .. Port+PortSpan-1 are used in turn.
	Port int

	mu     sync.Mutex
	counts map[string]int
	errs   map[string]int
	first  []string // first few failures, for the report
}

const maxFailureNotes = 10

// PortSpan is how many host ports the publish step rotates through, so a port is not
// reused while the previous container's listener may still be closing.
const PortSpan = 8

// opKinds lists every operation kind, in report order.
var opKinds = []string{"version", "load", "run", "run-named", "logs", "rm", "bind", "publish", "http", "rm-web"}

// Run loops until ctx is done. One iteration is:
//
//	docker version
//	docker load -i IMAGE.tar
//	docker run --rm --network none IMAGE
//	docker run --name soak-N --network none IMAGE
//	docker logs soak-N        (must print what the container printed)
//	docker rm soak-N
//
// and with ShareDir:
//
//	docker run --rm --network none -v SHARE:/mnt IMAGE  (reads in-N.txt, writes out-N.txt)
//
// and with Port as well:
//
//	docker run -d --name soak-N-web -p P:8080 -v SHARE\www:/www:ro IMAGE busybox httpd
//	GET http://127.0.0.1:P/index.html   (must return the page written on the host)
//	docker rm -f soak-N-web
func (w *Workload) Run(ctx context.Context) {
	for i := 0; ctx.Err() == nil; i++ {
		name := fmt.Sprintf("soak-%d-%d", os.Getpid(), i)
		w.op(ctx, "version", nil, "version", "--format", "{{.Server.Version}}")
		w.op(ctx, "load", nil, "load", "-q", "-i", w.ImageTar)
		w.op(ctx, "run", nil, "run", "--rm", "--network", "none", w.Image)
		out, ok := w.op(ctx, "run-named", nil, "run", "--name", name, "--network", "none", w.Image)
		if ok {
			w.op(ctx, "logs", func(logs string) error {
				if strings.TrimSpace(logs) != strings.TrimSpace(out) {
					return fmt.Errorf("logs %q differ from run output %q", logs, out)
				}
				return nil
			}, "logs", name)
		}
		// Always remove, even after a failed run or when the soak is ending or
		// interrupted mid-run, so containers never pile up in the engine.
		w.op(context.WithoutCancel(ctx), "rm", nil, "rm", "-f", name)
		if w.ShareDir != "" {
			w.bind(ctx, name)
			if w.Port != 0 {
				w.publish(ctx, name, w.Port+i%PortSpan)
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(w.Pause):
		}
	}
}

// bind runs one bind-mount round trip through ShareDir.
func (w *Workload) bind(ctx context.Context, name string) {
	in := filepath.Join(w.ShareDir, "in-"+name+".txt")
	out := filepath.Join(w.ShareDir, "out-"+name+".txt")
	defer os.Remove(in)
	defer os.Remove(out)
	if err := os.WriteFile(in, []byte(name+"\n"), 0o600); err != nil {
		w.record("bind", fmt.Errorf("write %s: %w", in, err), "")
		return
	}
	script := fmt.Sprintf("busybox cat /mnt/in-%[1]s.txt && echo %[1]s-back > /mnt/out-%[1]s.txt", name)
	w.op(ctx, "bind", func(stdout string) error {
		if strings.TrimSpace(stdout) != name {
			return fmt.Errorf("container read %q, want %q", stdout, name)
		}
		b, err := os.ReadFile(out)
		if err != nil {
			return fmt.Errorf("the container's write is not on the host: %w", err)
		}
		if strings.TrimSpace(string(b)) != name+"-back" {
			return fmt.Errorf("host read %q back", b)
		}
		return nil
	}, "run", "--rm", "--network", "none", "-v", w.ShareDir+":/mnt", w.Image, "sh", "-c", script)
}

// publish runs a web server with a published port and fetches its page from the host.
func (w *Workload) publish(ctx context.Context, name string, port int) {
	www := filepath.Join(w.ShareDir, "www")
	page := name + " page\n"
	if err := os.MkdirAll(www, 0o700); err == nil {
		err = os.WriteFile(filepath.Join(www, "index.html"), []byte(page), 0o600)
		if err != nil {
			w.record("publish", err, "")
			return
		}
	} else {
		w.record("publish", err, "")
		return
	}
	web := name + "-web"
	defer w.op(context.WithoutCancel(ctx), "rm-web", nil, "rm", "-f", web)
	if _, ok := w.op(ctx, "publish", nil, "run", "-d", "--name", web, "-p", fmt.Sprintf("%d:8080", port),
		"-v", www+":/www:ro", w.Image, "busybox", "httpd", "-f", "-p", "8080", "-h", "/www"); !ok {
		return
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/index.html", port)
	err := fetchUntil(ctx, url, page, 20*time.Second)
	if ctx.Err() != nil {
		return
	}
	w.record("http", err, "")
}

// fetchUntil GETs url until it returns want or wait runs out: the host listener is
// opened when the engine sees the container's start event, a moment after `docker
// run -d` returns.
func fetchUntil(ctx context.Context, url, want string, wait time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	var last error
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := c.Do(req)
		if err == nil {
			b, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
			resp.Body.Close()
			switch {
			case rerr != nil:
				last = rerr
			case resp.StatusCode != http.StatusOK:
				last = fmt.Errorf("HTTP %d", resp.StatusCode)
			case string(b) != want:
				return fmt.Errorf("got %q, want %q", b, want)
			default:
				return nil
			}
		} else {
			last = err
		}
		select {
		case <-ctx.Done():
			return errors.Join(fmt.Errorf("%s did not answer", url), last)
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// op runs one docker command. A command cut short because ctx ended is not counted.
func (w *Workload) op(ctx context.Context, kind string, check func(stdout string) error, args ...string) (string, bool) {
	if ctx.Err() != nil {
		return "", false
	}
	octx, cancel := context.WithTimeout(ctx, w.OpTimeout)
	defer cancel()
	cmd := exec.CommandContext(octx, w.Docker, append([]string{"-H", w.Host}, args...)...) //nolint:gosec // G204: docker path and args come from the operator's flags
	cmd.Env = append(os.Environ(), "DOCKER_CONFIG="+w.ConfigDir, "DOCKER_CLI_HINTS=false")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err == nil && check != nil {
		err = check(stdout.String())
	}
	if ctx.Err() != nil {
		return "", false
	}
	w.record(kind, err, strings.TrimSpace(stderr.String()))
	if err != nil {
		return "", false
	}
	return stdout.String(), true
}

// record counts one operation of kind, failed when err is non-nil.
func (w *Workload) record(kind string, err error, detail string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.counts == nil {
		w.counts, w.errs = map[string]int{}, map[string]int{}
	}
	w.counts[kind]++
	if err != nil {
		w.errs[kind]++
		if len(w.first) < maxFailureNotes {
			w.first = append(w.first, fmt.Sprintf("%s %s: %v: %s", time.Now().Format(time.TimeOnly), kind, err, detail))
		}
	}
}

// Totals returns the operation and error counts so far, and the first failures.
func (w *Workload) Totals() (ops, errs int, byKind string, failures []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var parts []string
	for _, k := range opKinds {
		if w.counts[k] == 0 && (k == "bind" || k == "publish" || k == "http" || k == "rm-web") {
			continue
		}
		ops += w.counts[k]
		errs += w.errs[k]
		parts = append(parts, fmt.Sprintf("%s=%d/%d", k, w.counts[k]-w.errs[k], w.counts[k]))
	}
	return ops, errs, strings.Join(parts, " "), append([]string(nil), w.first...)
}
