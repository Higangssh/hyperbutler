package source

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/Higangssh/hyperbutler/internal/dockerapi"
)

// recorder answers every request with {} (201 for a create) and records it.
type recorder struct {
	mu       sync.Mutex
	reqs     []string
	body     map[string]any
	delQuery string
}

func newClient(t *testing.T) (*Client, *recorder) {
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.reqs = append(rec.reqs, r.Method+" "+r.URL.Path)
		if r.Method == http.MethodDelete {
			rec.delQuery = r.URL.Query().Encode()
		}
		if r.URL.Path == "/containers/create" {
			_ = json.NewDecoder(r.Body).Decode(&rec.body)
		}
		rec.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/containers/create" {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"Id":"abc123"}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	c := New(dockerapi.New(func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", srv.Listener.Addr().String())
	}))
	t.Cleanup(c.Close)
	return c, rec
}

func (r *recorder) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.reqs...)
}

func TestAllowedGET(t *testing.T) {
	for _, p := range []string{"/_ping", "/info", "/version", "/images/json", "/images/get", "/volumes", "/volumes/pg",
		"/containers/json", "/containers/abc/json", "/containers/abc/archive", "/system/df", "/images/busybox/json",
		"/images/ghcr.io/o/app:1/json", "/images/sha256:abc/json", "/networks/web"} {
		if !allowedGET(p) {
			t.Errorf("%s refused", p)
		}
	}
	for _, p := range []string{"/containers/abc/attach/ws", "/containers/abc/logs", "/events", "/exec/abc/json",
		"/containers/abc/export", "/images/../x/json", "/images//json", "/volumes/", "/containers//json",
		"/images/a/history", "/session", "/grpc", "/build", "/containers/abc/stats", "/plugins", "/networks", "/networks/", "/networks/web/connect"} {
		if allowedGET(p) {
			t.Errorf("%s allowed", p)
		}
	}
}

// The source client sends nothing but allowed GETs, one helper create and its removal.
func TestSourceClientRejectsWrites(t *testing.T) {
	c, rec := newClient(t)
	ctx := context.Background()
	for _, p := range []string{"/containers/abc/attach/ws", "/events", "/containers/abc/logs"} {
		resp, err := c.Get(ctx, p, nil)
		if resp != nil {
			_ = resp.Body.Close()
		}
		if !errors.Is(err, ErrNotAllowed) {
			t.Errorf("GET %s: %v", p, err)
		}
		if err := c.GetJSON(ctx, p, nil, &struct{}{}); !errors.Is(err, ErrNotAllowed) {
			t.Errorf("GetJSON %s: %v", p, err)
		}
	}
	if err := c.RemoveHelper(ctx, "someones-container"); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("remove a container it did not create: %v", err)
	}
	if len(rec.list()) != 0 {
		t.Fatalf("refused calls reached the engine: %v", rec.list())
	}
	// A stale "helper" whose inspect does not show the label is left alone.
	if err := c.RemoveStaleHelper(ctx, "abc", func(string) bool { return false }); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("stale: %v", err)
	}
	run := NewRunID()
	for _, bad := range []string{"../x", "a/b", "-v"} {
		if _, err := c.CreateHelper(ctx, "busybox", bad, run); err == nil {
			t.Errorf("volume %q accepted", bad)
		}
	}
	if _, err := c.CreateHelper(ctx, "../busybox", "pg", run); err == nil {
		t.Error("bad image accepted")
	}
	if _, err := c.CreateHelper(ctx, "busybox", "pg", "x"); err == nil {
		t.Error("bad run id accepted")
	}

	id, err := c.CreateHelper(ctx, "busybox", "pg", run)
	if err != nil || id != "abc123" {
		t.Fatalf("create: %q %v", id, err)
	}
	mounts := rec.body["HostConfig"].(map[string]any)["Mounts"].([]any)
	m := mounts[0].(map[string]any)
	if m["ReadOnly"] != true || m["Source"] != "pg" || m["VolumeOptions"].(map[string]any)["NoCopy"] != true {
		t.Errorf("mount %v", m)
	}
	labels := rec.body["Labels"].(map[string]any)
	if labels[HelperLabel] != HelperLabelValue || labels[RunLabel] != run || rec.body["NetworkDisabled"] != true {
		t.Errorf("body %v", rec.body)
	}
	if !reflect.DeepEqual(c.Helpers(), []string{"abc123"}) {
		t.Errorf("helpers %v", c.Helpers())
	}
	if err := c.RemoveHelper(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveHelper(ctx, id); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("removed twice: %v", err)
	}
	// A helper goes with any anonymous volume its image would have made, and unforced.
	rec.mu.Lock()
	q := rec.delQuery
	rec.mu.Unlock()
	if q != "force=0&v=1" {
		t.Errorf("remove query %q", q)
	}
	for _, r := range rec.list() {
		method, path, _ := strings.Cut(r, " ")
		ok := method == "GET" || (method == "POST" && path == "/containers/create") || (method == "DELETE" && path == "/containers/abc123")
		if !ok {
			t.Errorf("sent %s", r)
		}
	}
}

// The Client's method set is the whole surface: a new method needs a review here.
func TestClientMethods(t *testing.T) {
	want := []string{"Close", "CreateHelper", "Get", "GetJSON", "Helpers", "RemoveHelper", "RemoveStaleHelper"}
	ty := reflect.TypeFor[*Client]()
	var got []string
	for i := range ty.NumMethod() {
		got = append(got, ty.Method(i).Name)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("methods %v, want %v: review that a new one cannot change Docker Desktop", got, want)
	}
}
