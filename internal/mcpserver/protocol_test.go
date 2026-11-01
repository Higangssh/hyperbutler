package mcpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

// stdio is a client's end of Run's stdin and stdout.
type stdio struct {
	t    *testing.T
	in   *io.PipeWriter
	out  *bufio.Reader
	done chan struct{} // closed when Run returned, with err
	err  error
}

func runStdio(t *testing.T, o Options) *stdio {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	s := &stdio{t: t, in: inW, out: bufio.NewReader(outR), done: make(chan struct{})}
	go func() {
		s.err = Run(context.Background(), o, inR, outW)
		_ = outW.Close()
		close(s.done)
	}()
	t.Cleanup(func() {
		_ = inW.Close()
		_ = outR.Close()
		select {
		case <-s.done:
		case <-time.After(10 * time.Second):
			t.Error("Run did not return")
		}
	})
	return s
}

func (s *stdio) send(msg string) {
	s.t.Helper()
	if _, err := io.WriteString(s.in, msg+"\n"); err != nil {
		s.t.Fatal(err)
	}
}

// recv reads the next message, which must answer id.
func (s *stdio) recv(id int) map[string]any {
	s.t.Helper()
	type line struct {
		b   []byte
		err error
	}
	ch := make(chan line, 1)
	go func() {
		b, err := s.out.ReadBytes('\n')
		ch <- line{b, err}
	}()
	var l line
	select {
	case l = <-ch:
	case <-time.After(10 * time.Second):
		s.t.Fatal("no answer")
	}
	if l.err != nil {
		s.t.Fatal(l.err)
	}
	var m map[string]any
	if err := json.Unmarshal(l.b, &m); err != nil {
		s.t.Fatalf("%v: %s", err, l.b)
	}
	if m["id"] != float64(id) {
		s.t.Fatalf("answer %s, want id %d", l.b, id)
	}
	return m
}

// The wire: initialize, initialized, tools/list, tools/call, ping, then EOF on stdin
// ends Run.
func TestProtocolOverStdio(t *testing.T) {
	f, h := newFakeEngine(t), &fakeHost{running: true}
	f.addContainer("9999999999999999", "web", "running", "always", false)
	s := runStdio(t, testOptions(f, h))
	s.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"raw","version":"1"}}}`)
	init := s.recv(1)
	res, _ := init["result"].(map[string]any)
	if res == nil || res["protocolVersion"] != "2025-06-18" || res["serverInfo"].(map[string]any)["name"] != "hyperbutler" {
		t.Fatalf("initialize = %v", init)
	}
	if !strings.Contains(res["instructions"].(string), "untrustedText") {
		t.Errorf("instructions = %v", res["instructions"])
	}
	s.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	s.send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	list := s.recv(2)
	tools := list["result"].(map[string]any)["tools"].([]any)
	if len(tools) != len(readTools) {
		t.Errorf("%d tools, want %d", len(tools), len(readTools))
	}
	s.send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"containers","arguments":{}}}`)
	r := s.recv(3)["result"].(map[string]any)
	if r["isError"] == true || !strings.Contains(r["content"].([]any)[0].(map[string]any)["text"].(string), `"name":"web"`) {
		t.Errorf("containers = %v", r)
	}
	if sc, _ := r["structuredContent"].(map[string]any); sc["total"] != float64(1) {
		t.Errorf("structuredContent = %v", r["structuredContent"])
	}
	s.send(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"stop","arguments":{}}}`)
	if m := s.recv(4); m["error"] == nil && m["result"].(map[string]any)["isError"] != true {
		t.Errorf("stop on a read-only server = %v", m)
	}
	s.send(`{"jsonrpc":"2.0","id":5,"method":"ping"}`)
	if m := s.recv(5); m["error"] != nil {
		t.Errorf("ping = %v", m)
	}
	s.send(`{"jsonrpc":"2.0","id":6,"method":"nope/nope"}`)
	if m := s.recv(6); m["error"] == nil {
		t.Errorf("unknown method = %v", m)
	}
	_ = s.in.Close()
	select {
	case <-s.done:
		if s.err != nil {
			t.Errorf("Run = %v after EOF", s.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after stdin closed")
	}
	if c := h.Calls(); len(c) != 0 {
		t.Errorf("host calls %v", c)
	}
}

// Closing stdin ends Run even while a tool call is still running and ignores its
// context: the server leaves nothing behind for the client to wait on.
func TestStdinCloseDuringCall(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	h := &fakeHost{block: block}
	s := runStdio(t, testOptions(newFakeEngine(t), h))
	s.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"raw","version":"1"}}}`)
	s.recv(1)
	s.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	s.send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"engine_logs","arguments":{}}}`)
	deadline := time.Now().Add(5 * time.Second)
	for len(h.Calls()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	_ = s.in.Close()
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after stdin closed during a call")
	}
}

// A request line over maxRequestLine ends the session instead of being buffered.
func TestOversizedRequest(t *testing.T) {
	s := runStdio(t, testOptions(newFakeEngine(t), &fakeHost{}))
	big := `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"x":"` + strings.Repeat("a", maxRequestLine) + `"}}`
	go func() { _, _ = io.WriteString(s.in, big+"\n") }()
	select {
	case <-s.done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run kept reading an oversized line")
	}
}
