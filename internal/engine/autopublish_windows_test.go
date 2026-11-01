//go:build windows

package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// fakeDockerd answers container inspects: "web" and testID are a stopped container
// that publishes ports, anything else is unknown.
func fakeDockerd(t *testing.T, ports ...uint16) *http.Client {
	t.Helper()
	var bindings, settings []string
	for i, p := range ports {
		b := fmt.Sprintf(`"%d/tcp":[{"HostIp":"","HostPort":"%d"}]`, 80+i, p)
		bindings = append(bindings, b)
		settings = append(settings, b)
	}
	doc := `{"Id":"` + testID + `","State":{"Running":false},"HostConfig":{"PortBindings":{` + strings.Join(bindings, ",") +
		`}},"NetworkSettings":{"Ports":{` + strings.Join(settings, ",") + `}}}`
	mux := http.NewServeMux()
	for _, name := range []string{"web", testID} {
		mux.HandleFunc("/containers/"+name+"/json", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, doc) })
	}
	hs := httptest.NewServer(mux)
	t.Cleanup(hs.Close)
	u, _ := url.Parse(hs.URL)
	c := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", u.Host)
		},
		DisableKeepAlives: true,
	}}
	t.Cleanup(c.CloseIdleConnections)
	return c
}

func testPublisher(t *testing.T, client *http.Client, check func(uint16) error, on func([]UnpublishedPort)) *autoPublisher {
	t.Helper()
	g, _, _, err := setupNet(NetConfig{OnUnpublished: on}, "D:P(A;;FA;;;SY)", slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.st.Close)
	a := &autoPublisher{g: g, client: client, step: func(string, ...any) {}, open: map[string][]io.Closer{},
		failed: map[string][]UnpublishedPort{}, check: check}
	g.ap = a
	t.Cleanup(a.closeAll)
	return a
}

// The start check (the API proxy's StartChecker) refuses a container whose port a
// host program holds on 127.0.0.1, with Docker's wording, and lets everything it
// cannot judge through.
func TestCheckStartRefusesTakenPort(t *testing.T) {
	_, p := hostProgram(t)
	q := freePort(t)
	a := testPublisher(t, fakeDockerd(t, q, p), func(port uint16) error { return hostPortFreeExcept(port, 0) }, nil)
	err := a.g.checkStart(context.Background(), "web")
	var pu *PortsUnavailable
	if !errors.As(err, &pu) || pu.Port != p {
		t.Fatalf("start of a container publishing %d (taken) and %d: %v", p, q, err)
	}
	if want := fmt.Sprintf("Ports are not available: exposing port TCP 127.0.0.1:%d -> 0.0.0.0:0: ", p); !strings.HasPrefix(err.Error(), want) {
		t.Errorf("message %q", err)
	}
	if err := a.g.checkStart(context.Background(), "missing"); err != nil {
		t.Errorf("unknown container: %v; dockerd should answer it", err)
	}

	free := testPublisher(t, fakeDockerd(t, q), func(port uint16) error { return hostPortFreeExcept(port, 0) }, nil)
	if err := free.g.checkStart(context.Background(), "web"); err != nil {
		t.Errorf("free port: %v", err)
	}
	var none guestNet
	if err := none.checkStart(context.Background(), "web"); err != nil {
		t.Errorf("without auto-publish: %v", err)
	}
}

// A container started without the check (a restart policy, or a host program that
// won the race) whose port is taken on 127.0.0.1 gets no listener at all, and
// State.UnpublishedPorts lists all its ports until it stops.
func TestAddPublishesAllOrNothing(t *testing.T) {
	_, p := hostProgram(t)
	q := freePort(t)
	var mu sync.Mutex
	var reported [][]UnpublishedPort
	a := testPublisher(t, fakeDockerd(t, q, p), noCheck, func(u []UnpublishedPort) {
		mu.Lock()
		reported = append(reported, u)
		mu.Unlock()
	})
	a.add(context.Background(), testID)
	a.mu.Lock()
	cs, tried := a.open[testID]
	n := a.count
	a.mu.Unlock()
	if !tried || len(cs) != 0 || n != 0 {
		t.Fatalf("open %v (tried %v), count %d: want nothing published", cs, tried, n)
	}
	for _, ip := range loopbacks {
		if ip == loopbacks[1] && !ipv6Available() {
			continue
		}
		ln, err := net.Listen("tcp", netip.AddrPortFrom(ip, q).String())
		if err != nil {
			t.Errorf("%s port %d still bound: %v", ip, q, err)
			continue
		}
		_ = ln.Close()
	}
	mu.Lock()
	if len(reported) != 1 || len(reported[0]) != 2 {
		t.Fatalf("reported %+v", reported)
	}
	for _, u := range reported[0] {
		want := "Ports are not available: "
		if u.Port == q {
			want = fmt.Sprintf("held back with port %d", p)
		}
		if u.Container != testID[:12] || !strings.HasPrefix(u.Reason, want) {
			t.Errorf("unpublished %+v", u)
		}
	}
	mu.Unlock()

	a.remove(testID)
	mu.Lock()
	defer mu.Unlock()
	if len(reported) != 2 || len(reported[1]) != 0 {
		t.Errorf("after the container stopped: %+v", reported)
	}
}

// A container whose ports are free gets both loopbacks for each.
func TestAddPublishes(t *testing.T) {
	q := freePort(t)
	a := testPublisher(t, fakeDockerd(t, q), func(port uint16) error { return hostPortFreeExcept(port, 0) }, nil)
	a.add(context.Background(), testID)
	a.mu.Lock()
	n := len(a.open[testID])
	a.mu.Unlock()
	want := 2
	if !ipv6Available() {
		want = 1
	}
	if n != want {
		t.Fatalf("%d listeners, want %d", n, want)
	}
	c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", q))
	if err != nil {
		t.Fatalf("127.0.0.1:%d: %v", q, err)
	}
	_ = c.Close()
}
