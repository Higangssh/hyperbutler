package mcpserver

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeEngine is a Docker API with canned answers, served by httptest (the server under
// test reaches it through Options.Dial, as it reaches the engine's pipe).
type fakeEngine struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	requests []string // "METHOD /path?query"
	bodies   map[string][]byte

	containers []apiContainer
	inspect    map[string]apiInspect
	logs       map[string][]byte // the raw logs answer per container id
	images     []apiImage
	volumes    []apiVolume
	df         apiDF
	pull       string // the pull answer stream
	stats      apiStats
}

func newFakeEngine(t *testing.T) *fakeEngine {
	f := &fakeEngine{t: t, inspect: map[string]apiInspect{}, logs: map[string][]byte{}, bodies: map[string][]byte{}}
	mux := http.NewServeMux()
	j := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	notFound := func(w http.ResponseWriter, what string) {
		w.WriteHeader(http.StatusNotFound)
		j(w, map[string]string{"message": "No such container: " + what})
	}
	lookup := func(ref string) (apiInspect, bool) {
		f.mu.Lock()
		defer f.mu.Unlock()
		for id, in := range f.inspect {
			if id == ref || strings.TrimPrefix(in.Name, "/") == ref || strings.HasPrefix(id, ref) {
				return in, true
			}
		}
		return apiInspect{}, false
	}
	mux.HandleFunc("GET /containers/json", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		list := f.containers
		f.mu.Unlock()
		if r.URL.Query().Get("all") != "1" {
			var run []apiContainer
			for _, c := range list {
				if c.State == "running" {
					run = append(run, c)
				}
			}
			list = run
		}
		if list == nil {
			list = []apiContainer{}
		}
		j(w, list)
	})
	mux.HandleFunc("GET /containers/{id}/json", func(w http.ResponseWriter, r *http.Request) {
		in, ok := lookup(r.PathValue("id"))
		if !ok {
			notFound(w, r.PathValue("id"))
			return
		}
		j(w, in)
	})
	mux.HandleFunc("GET /containers/{id}/logs", func(w http.ResponseWriter, r *http.Request) {
		in, ok := lookup(r.PathValue("id"))
		if !ok {
			notFound(w, r.PathValue("id"))
			return
		}
		f.mu.Lock()
		b := f.logs[in.ID]
		f.mu.Unlock()
		_, _ = w.Write(b)
	})
	mux.HandleFunc("GET /containers/{id}/stats", func(w http.ResponseWriter, _ *http.Request) { j(w, f.stats) })
	mux.HandleFunc("POST /containers/{id}/{verb}", func(w http.ResponseWriter, r *http.Request) {
		in, ok := lookup(r.PathValue("id"))
		if !ok {
			notFound(w, r.PathValue("id"))
			return
		}
		verb := r.PathValue("verb")
		f.mu.Lock()
		defer f.mu.Unlock()
		switch verb {
		case "start", "restart":
			if verb == "start" && in.State.Running {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			in.State.Running, in.State.Status = true, "running"
		case "stop":
			if !in.State.Running {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			in.State.Running, in.State.Status, in.State.ExitCode = false, "exited", 137
		default:
			w.WriteHeader(http.StatusNotFound)
			return
		}
		f.inspect[in.ID] = in
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /containers/create", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Image string `json:"Image"`
		}
		_ = json.Unmarshal(f.body(r), &body)
		if body.Image == "missing:1" {
			w.WriteHeader(http.StatusNotFound)
			j(w, map[string]string{"message": "No such image: missing:1"})
			return
		}
		id := strings.Repeat("ab", 32)
		name := r.URL.Query().Get("name")
		if name == "" {
			name = "eager_turing"
		}
		f.mu.Lock()
		in := apiInspect{ID: id, Name: "/" + name}
		in.State.Status = "created"
		f.inspect[id] = in
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		j(w, map[string]any{"Id": id, "Warnings": []string{}})
	})
	mux.HandleFunc("DELETE /containers/{id}", func(w http.ResponseWriter, r *http.Request) {
		in, ok := lookup(r.PathValue("id"))
		if !ok {
			notFound(w, r.PathValue("id"))
			return
		}
		if in.State.Running && r.URL.Query().Get("force") != "1" {
			w.WriteHeader(http.StatusConflict)
			j(w, map[string]string{"message": "cannot remove a running container"})
			return
		}
		f.mu.Lock()
		delete(f.inspect, in.ID)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /images/json", func(w http.ResponseWriter, _ *http.Request) { j(w, f.images) })
	mux.HandleFunc("DELETE /images/{name...}", func(w http.ResponseWriter, r *http.Request) {
		j(w, []map[string]string{{"Untagged": r.PathValue("name")}, {"Deleted": "sha256:" + strings.Repeat("cd", 32)}})
	})
	mux.HandleFunc("POST /images/create", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, f.pull) })
	mux.HandleFunc("GET /volumes", func(w http.ResponseWriter, _ *http.Request) {
		j(w, map[string]any{"Volumes": f.volumes, "Warnings": nil})
	})
	mux.HandleFunc("DELETE /volumes/{name}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("GET /system/df", func(w http.ResponseWriter, _ *http.Request) { j(w, f.df) })
	mux.HandleFunc("GET /info", func(w http.ResponseWriter, _ *http.Request) {
		j(w, apiInfo{ServerVersion: "29.0.0", MemTotal: 4 << 30, NCPU: 4})
	})
	mux.HandleFunc("POST /{kind}/prune", func(w http.ResponseWriter, r *http.Request) {
		j(w, map[string]any{"ContainersDeleted": []string{"a", "b"}, "SpaceReclaimed": 1234})
	})
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		s := r.Method + " " + r.URL.EscapedPath()
		if r.URL.RawQuery != "" {
			s += "?" + r.URL.RawQuery
		}
		f.requests = append(f.requests, s)
		f.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// body reads and records a request body.
func (f *fakeEngine) body(r *http.Request) []byte {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.bodies[r.Method+" "+r.URL.Path] = b
	f.mu.Unlock()
	return b
}

func (f *fakeEngine) reqs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

// addContainer adds a container to the fake.
func (f *fakeEngine) addContainer(id, name, state, policy string, tty bool, ports ...apiPort) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.containers = append(f.containers, apiContainer{ID: id, Names: []string{"/" + name}, Image: "busybox:1.37", State: state, Status: state, Created: 1700000000, Ports: ports,
		Labels: map[string]string{"com.docker.compose.project": "demo"}})
	in := apiInspect{ID: id, Name: "/" + name}
	in.Config.Tty = tty
	in.State.Status, in.State.Running = state, state == "running"
	in.HostConfig.RestartPolicy.Name = policy
	f.inspect[id] = in
}

// frame is one frame of Docker's multiplexed log stream.
func frame(stream byte, s string) []byte {
	h := make([]byte, 8, 8+len(s))
	h[0] = stream
	binary.BigEndian.PutUint32(h[4:], uint32(len(s)))
	return append(h, s...)
}

// fakeHost is a Host with canned answers.
type fakeHost struct {
	mu        sync.Mutex
	running   bool
	pipe      string
	status    string
	settings  string
	diagnose  string
	engineLog string
	calls     []string
	block     chan struct{} // EngineLog waits on it, ignoring ctx, when set
}

func (h *fakeHost) call(s string) {
	h.mu.Lock()
	h.calls = append(h.calls, s)
	h.mu.Unlock()
}

func (h *fakeHost) Calls() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.calls...)
}

func (h *fakeHost) Status(context.Context) ([]byte, error) {
	h.call("status")
	if h.status != "" {
		return []byte(h.status), nil
	}
	return fmt.Appendf(nil, `{"schema":1,"running":%v,"setUp":true,"home":"C:\\Users\\u\\AppData\\Local\\hyperbutler","memoryMB":4096,"diskHostBytes":1000,"diskSizeBytes":2000,"hostFreeBytes":3000,"disk":"D:\\data.vhdx"}`, h.running), nil
}

func (h *fakeHost) Settings(context.Context) ([]byte, error) {
	h.call("settings")
	return []byte(h.settings), nil
}

func (h *fakeHost) Diagnose(context.Context) ([]byte, error) {
	h.call("diagnose")
	return []byte(h.diagnose), nil
}

func (h *fakeHost) EngineLog(_ context.Context, console bool, lines int) ([]byte, error) {
	h.call(fmt.Sprintf("enginelog console=%v n=%d", console, lines))
	if h.block != nil {
		<-h.block
	}
	return []byte(h.engineLog), nil
}

func (h *fakeHost) Pipe(context.Context) (string, error) {
	if !h.running {
		return "", ErrEngineStopped
	}
	return h.pipe, nil
}

func (h *fakeHost) Start(context.Context) (string, error) {
	h.call("start")
	return "hyperbutler is running: the Docker API answered after 2.0 s (engine pid 1).\n", nil
}

func (h *fakeHost) Stop(context.Context) (string, error) {
	h.call("stop")
	return "hyperbutler stop: 1 running container stops; 0 come back at the next start, 1 stays stopped.\n", nil
}

func (h *fakeHost) ConfigSet(_ context.Context, key, value string) (string, error) {
	h.call("config set " + key + " " + value)
	return key + ": 4096 -> " + value + " (file)\n", nil
}

// testOptions are server options against f and h.
func testOptions(f *fakeEngine, h *fakeHost) Options {
	h.pipe = `\\.\pipe\fake`
	return Options{Host: h, Version: "test", Dial: func(ctx context.Context, pipe string) (net.Conn, error) {
		if pipe != h.pipe {
			return nil, errors.New("wrong pipe " + pipe)
		}
		var d net.Dialer
		return d.DialContext(ctx, "tcp", f.srv.Listener.Addr().String())
	}}
}

// session connects an in-memory client to a server with o.
func session(t *testing.T, o Options) *mcp.ClientSession {
	t.Helper()
	s, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	st, ct := mcp.NewInMemoryTransports()
	ss, err := s.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1"}, nil)
	cs, err := c.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cs.Close()
		_ = ss.Wait()
		cancel()
	})
	return cs
}

// call calls a tool and returns its text and whether it failed.
func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if args == nil {
		args = map[string]any{}
	}
	r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	var b strings.Builder
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String(), r.IsError
}

// callJSON calls a tool that must succeed and decodes its result.
func callJSON(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any, v any) string {
	t.Helper()
	s, isErr := call(t, cs, name, args)
	if isErr {
		t.Fatalf("%s failed: %s", name, s)
	}
	if v != nil {
		if err := json.Unmarshal([]byte(s), v); err != nil {
			t.Fatalf("%s: %v in %s", name, err, s)
		}
	}
	return s
}
