package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Workload runs the mixed docker loop against one engine endpoint with the stock CLI.
type Workload struct {
	Docker    string        // path to docker.exe
	Host      string        // -H value, e.g. npipe:////./pipe/docker_engine_winbox
	ImageTar  string        // `docker load` input
	Image     string        // name:tag inside ImageTar
	OpTimeout time.Duration // per docker invocation
	Pause     time.Duration // between iterations
	ConfigDir string        // DOCKER_CONFIG, so the user's contexts and config never apply

	mu     sync.Mutex
	counts map[string]int
	errs   map[string]int
	first  []string // first few failures, for the report
}

const maxFailureNotes = 10

// Run loops until ctx is done. One iteration is:
//
//	docker version
//	docker load -i IMAGE.tar
//	docker run --rm --network none IMAGE
//	docker run --name soak-N --network none IMAGE
//	docker logs soak-N        (must print what the container printed)
//	docker rm soak-N
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
		select {
		case <-ctx.Done():
		case <-time.After(w.Pause):
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
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.counts == nil {
		w.counts, w.errs = map[string]int{}, map[string]int{}
	}
	w.counts[kind]++
	if err != nil {
		w.errs[kind]++
		if len(w.first) < maxFailureNotes {
			w.first = append(w.first, fmt.Sprintf("%s %s: %v: %s", time.Now().Format(time.TimeOnly), kind, err,
				strings.TrimSpace(stderr.String())))
		}
		return "", false
	}
	return stdout.String(), true
}

// Totals returns the operation and error counts so far, and the first failures.
func (w *Workload) Totals() (ops, errs int, byKind string, failures []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var parts []string
	for _, k := range []string{"version", "load", "run", "run-named", "logs", "rm"} {
		ops += w.counts[k]
		errs += w.errs[k]
		parts = append(parts, fmt.Sprintf("%s=%d/%d", k, w.counts[k]-w.errs[k], w.counts[k]))
	}
	return ops, errs, strings.Join(parts, " "), append([]string(nil), w.first...)
}
