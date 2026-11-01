//go:build windows

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Higangssh/hyperbutler/internal/engine"
	"github.com/Higangssh/hyperbutler/internal/transport"
)

// REQ-UX-017: which restart policies bring a container back after a clean stop.
func TestComesBack(t *testing.T) {
	for p, want := range map[string]bool{
		"always": true, "unless-stopped": true, "on-failure": false, "no": false, "": false,
	} {
		if got := comesBack(p); got != want {
			t.Errorf("comesBack(%q) = %v", p, got)
		}
	}
	for p, want := range map[string]string{
		"always":         "comes back (restart always)",
		"unless-stopped": "comes back (restart unless-stopped)",
		"on-failure":     "comes back only if it stops with an error, as when killed (restart on-failure)",
		"no":             "stays stopped (no restart policy)",
		"":               "stays stopped (no restart policy)",
	} {
		c := RunningContainer{RestartPolicy: p, ComesBack: comesBack(p)}
		if got := afterText(c); got != want {
			t.Errorf("%q: %q, want %q", p, got, want)
		}
	}
}

func TestWriteContainerList(t *testing.T) {
	list := []RunningContainer{
		{Name: "web", Image: "nginx:1.27", RestartPolicy: "always", ComesBack: true},
		{Name: "job-17", Image: "busybox", RestartPolicy: "no"},
		{Name: "db", Image: "postgres:17", RestartPolicy: "on-failure"},
	}
	var b bytes.Buffer
	writeContainerList(&b, "hyperbutler stop", "at the next start", list, nil)
	out := b.String()
	for _, want := range []string{
		"hyperbutler stop: 3 running containers stop; 1 comes back at the next start, 1 only if it stops with an error (on-failure), 1 stays stopped.\n",
		"CONTAINER", "IMAGE", "THEN",
		"web", "nginx:1.27", "comes back (restart always)",
		"job-17", "stays stopped (no restart policy)",
		"db", "comes back only if it stops with an error",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	b.Reset()
	writeContainerList(&b, "dockerd restarts", "when it is up again", list[:1], nil)
	if !strings.HasPrefix(b.String(), "dockerd restarts: 1 running container stop; 1 comes back when it is up again, 0 stay stopped.\n") {
		t.Errorf("one container:\n%s", b.String())
	}
	b.Reset()
	writeContainerList(&b, "hyperbutler stop", "at the next start", nil, nil)
	if b.Len() != 0 {
		t.Errorf("no containers: %q", b.String())
	}
	b.Reset()
	writeContainerList(&b, "hyperbutler stop", "at the next start", nil, errors.New("no pipe"))
	if !strings.Contains(b.String(), "could not list the running containers (no pipe)") {
		t.Errorf("error: %q", b.String())
	}
	// Names come from dockerd: they are printed escaped.
	b.Reset()
	writeContainerList(&b, "x", "y", []RunningContainer{{Name: "a\x1b[2Jb", Image: "i"}}, nil)
	if strings.Contains(b.String(), "\x1b") {
		t.Errorf("control character printed: %q", b.String())
	}
}

func TestContainerName(t *testing.T) {
	if n := containerName("0123456789abcdef", []string{"/web"}); n != "web" {
		t.Fatal(n)
	}
	// A link alias ("/other/alias") is not the container's own name.
	if n := containerName("0123456789abcdef", []string{"/other/alias", "/db"}); n != "db" {
		t.Fatal(n)
	}
	if n := containerName("0123456789abcdef", nil); n != "0123456789ab" {
		t.Fatal(n)
	}
}

// fakeDockerAPI serves the two Docker API calls the list needs on a pipe of its own.
func fakeDockerAPI(t *testing.T, h http.Handler) string {
	t.Helper()
	pipe := fmt.Sprintf(`\\.\pipe\hyperbutler-test-containers-%d`, time.Now().UnixNano())
	ln, err := transport.ListenPipe(pipe)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(h)
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return pipe
}

func TestListRunningContainers(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /containers/json", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("all") != "" {
			t.Errorf("listed all containers: %s", r.URL)
		}
		_, _ = fmt.Fprint(w, `[{"Id":"aaaaaaaaaaaaaaaa","Names":["/web"],"Image":"nginx"},{"Id":"bbbbbbbbbbbbbbbb","Names":["/job"],"Image":"busybox"}]`)
	})
	mux.HandleFunc("GET /containers/{id}/json", func(w http.ResponseWriter, r *http.Request) {
		policy := map[string]string{"aaaaaaaaaaaaaaaa": "unless-stopped", "bbbbbbbbbbbbbbbb": "no"}[r.PathValue("id")]
		_, _ = fmt.Fprintf(w, `{"HostConfig":{"RestartPolicy":{"Name":%q,"MaximumRetryCount":0}}}`, policy)
	})
	pipe := fakeDockerAPI(t, mux)
	got, err := listRunningContainers(context.Background(), pipe)
	if err != nil {
		t.Fatal(err)
	}
	want := []RunningContainer{
		{Name: "web", Image: "nginx", RestartPolicy: "unless-stopped", ComesBack: true},
		{Name: "job", Image: "busybox", RestartPolicy: "no"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}

	// noteContainers prints it for a running engine, and nothing for one that is not.
	var b bytes.Buffer
	st := &engine.State{Phase: engine.PhaseRunning, Pipe: pipe}
	if l := noteContainers(st, &b, "hyperbutler stop", "at the next start"); len(l) != 2 || !strings.Contains(b.String(), "2 running containers stop; 1 comes back") {
		t.Fatalf("note: %v %q", l, b.String())
	}
	b.Reset()
	st.Phase = engine.PhaseStarting
	if l := noteContainers(st, &b, "hyperbutler stop", "at the next start"); l != nil || b.Len() != 0 {
		t.Fatalf("starting engine: %v %q", l, b.String())
	}
}

// An engine whose API fails or is not there is said so, and the stop goes on.
func TestListRunningContainersUnreachable(t *testing.T) {
	pipe := fakeDockerAPI(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "paused", http.StatusServiceUnavailable)
	}))
	if _, err := listRunningContainers(context.Background(), pipe); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("err %v", err)
	}
	var b bytes.Buffer
	st := &engine.State{Phase: engine.PhaseRunning, Pipe: `\\.\pipe\hyperbutler-test-containers-none`}
	if l := noteContainers(st, &b, "hyperbutler stop", "at the next start"); l != nil || !strings.Contains(b.String(), "could not list the running containers") {
		t.Fatalf("missing pipe: %v %q", l, b.String())
	}
}
