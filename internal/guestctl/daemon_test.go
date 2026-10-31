package guestctl

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// daemonHandler is a fakeHandler that installs daemon.json files.
type daemonHandler struct {
	*fakeHandler
	mu       sync.Mutex
	got      [][]byte
	restarts []bool
	res      DaemonResult
}

func (h *daemonHandler) ApplyDaemonConfig(_ context.Context, data []byte, restart bool) DaemonResult {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.got = append(h.got, bytes.Clone(data))
	h.restarts = append(h.restarts, restart)
	return h.res
}

func TestDaemonMessagesRoundTrip(t *testing.T) {
	for _, m := range []Msg{
		DaemonChunk{Offset: 0, Data: []byte("{}")},
		DaemonChunk{Offset: MaxDaemonConfig - 1, Data: []byte("x")},
		DaemonChunk{Offset: 232, Data: bytes.Repeat([]byte{0}, DaemonChunkData)},
		DaemonCommit{Size: 2, Flags: DaemonRestart, SHA256: sha256.Sum256([]byte("{}"))},
		DaemonCommit{},
		DaemonResult{Code: DaemonRolledBack, Flags: DaemonChanged | DaemonRestarted, Duration: 3 * time.Second, Message: "dockerd exited"},
		DaemonResult{Message: strings.Repeat("é", MaxDaemonMessage/2)},
	} {
		f, err := Encode(5, m)
		if err != nil {
			t.Fatalf("%#v: %v", m, err)
		}
		if len(f)-4 > MaxBody {
			t.Fatalf("%v: %d-byte body", m.Type(), len(f)-4)
		}
		seq, got, err := Decode(f[4:], dirOf(m))
		if err != nil || seq != 5 || !reflect.DeepEqual(got, m) {
			t.Fatalf("got %d %#v %v, want %#v", seq, got, err, m)
		}
		if _, _, err := Decode(f[4:], 1-dirOf(m)); err == nil {
			t.Errorf("%v decoded in the wrong direction", m.Type())
		}
	}
}

func TestDaemonMessagesRejected(t *testing.T) {
	for _, m := range []Msg{
		DaemonChunk{},
		DaemonChunk{Data: make([]byte, DaemonChunkData+1)},
		DaemonChunk{Offset: MaxDaemonConfig, Data: []byte("x")},
		DaemonChunk{Offset: 0xffffffff, Data: []byte("x")},
		DaemonCommit{Size: MaxDaemonConfig + 1},
		DaemonCommit{Flags: 2},
		DaemonResult{Code: 5},
		DaemonResult{Flags: 4},
		DaemonResult{Duration: -1},
		DaemonResult{Message: strings.Repeat("x", MaxDaemonMessage+1)},
	} {
		if _, err := Encode(1, m); err == nil {
			t.Errorf("Encode(%#v) succeeded", m)
		}
	}
	// Non-zero bytes past a chunk's data, or in reserved fields, are not canonical.
	f, _ := Encode(1, DaemonChunk{Data: []byte("ab")})
	for _, i := range []int{4 + HeaderLen + 6, 4 + HeaderLen + 8 + 2, len(f) - 1} {
		b := bytes.Clone(f[4:])
		b[i-4] = 1
		if _, _, err := Decode(b, FromHost); err == nil {
			t.Errorf("chunk with byte %d set decoded", i)
		}
	}
	r, _ := Encode(1, DaemonResult{Message: "hi"})
	for _, i := range []int{4 + HeaderLen + 5, len(r) - 1} {
		b := bytes.Clone(r[4:])
		b[i-4] = 1
		if _, _, err := Decode(b, FromGuest); err == nil {
			t.Errorf("result with byte %d set decoded", i)
		}
	}
}

func TestDaemonConfigTransfer(t *testing.T) {
	h := &daemonHandler{fakeHandler: &fakeHandler{}, res: DaemonResult{Flags: DaemonChanged | DaemonRestarted}}
	c, _ := startPairHandler(t, h, &Server{Features: FeatureDaemonConfig})
	ctx := context.Background()
	// Several chunks, then a one-chunk file, then a removal.
	big := []byte(`{"insecure-registries":["` + strings.Repeat("a", 1000) + `"]}`)
	for _, data := range [][]byte{big, []byte(`{"debug":true}`), nil} {
		r, err := c.DaemonConfig(ctx, data, data != nil)
		if err != nil || r != h.res {
			t.Fatalf("DaemonConfig(%d bytes) = %#v, %v", len(data), r, err)
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.got) != 3 || !bytes.Equal(h.got[0], big) || string(h.got[1]) != `{"debug":true}` || len(h.got[2]) != 0 {
		t.Fatalf("handler got %q", h.got)
	}
	if !reflect.DeepEqual(h.restarts, []bool{true, true, false}) {
		t.Fatalf("restart flags %v", h.restarts)
	}
	if _, err := c.DaemonConfig(ctx, make([]byte, MaxDaemonConfig+1), false); err == nil {
		t.Fatal("an oversized file was sent")
	}
}

func TestDaemonConfigUnsupported(t *testing.T) {
	// The guest does not advertise the feature: the client does not even send.
	h := &daemonHandler{fakeHandler: &fakeHandler{}}
	c, _ := startPairHandler(t, h, &Server{})
	var re *RefusedError
	if _, err := c.DaemonConfig(context.Background(), []byte("{}"), false); !errors.As(err, &re) || re.Code != ErrUnsupported {
		t.Fatalf("err = %v", err)
	}
	// It advertises the feature but its handler cannot apply: refused by the server.
	c2, _ := startPair(t, &fakeHandler{}, &Server{Features: FeatureDaemonConfig}, ClientConfig{})
	if _, err := c2.DaemonConfig(context.Background(), []byte("{}"), false); !errors.As(err, &re) || re.Code != ErrUnsupported {
		t.Fatalf("err = %v", err)
	}
}

func TestDaemonServerChecksTransfer(t *testing.T) {
	h := &daemonHandler{fakeHandler: &fakeHandler{}}
	srv := &Server{Features: FeatureDaemonConfig}
	c, errc := startPairHandler(t, h, srv)
	// A commit whose hash does not match the chunks is answered without calling the
	// handler.
	if err := c.post(context.Background(), DaemonChunk{Data: []byte("{}")}); err != nil {
		t.Fatal(err)
	}
	m, err := c.request(context.Background(), DaemonCommit{Size: 2}, RDaemon, 0)
	if err != nil || m.(DaemonResult).Code != DaemonBadTransfer {
		t.Fatalf("got %#v, %v", m, err)
	}
	// A size-0 commit drops an abandoned transfer and removes the file.
	_ = c.post(context.Background(), DaemonChunk{Data: []byte("{")})
	m, err = c.request(context.Background(), DaemonCommit{SHA256: sha256.Sum256(nil)}, RDaemon, 0)
	if err != nil || m.(DaemonResult).Code != DaemonOK {
		t.Fatalf("got %#v, %v", m, err)
	}
	h.mu.Lock()
	if len(h.got) != 1 || len(h.got[0]) != 0 {
		t.Fatalf("handler got %q", h.got)
	}
	h.mu.Unlock()
	// A chunk that leaves a gap breaks the protocol.
	_ = c.post(context.Background(), DaemonChunk{Offset: 7, Data: []byte("x")})
	select {
	case err := <-errc:
		if !errors.Is(err, ErrMalformed) {
			t.Fatalf("server ended with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server accepted a chunk with a gap")
	}
}
