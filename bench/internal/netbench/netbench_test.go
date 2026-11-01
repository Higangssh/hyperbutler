package netbench

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/Higangssh/hyperbutler/internal/leaktest"
)

func TestMain(m *testing.M) { leaktest.VerifyTestMain(m) }

func servers(t *testing.T) func(int) string {
	t.Helper()
	var addrs [Kinds]string
	for k := 0; k < Kinds; k++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		go func(k int) { _ = Serve(ln, k) }(k)
		addrs[k] = ln.Addr().String()
	}
	return func(k int) string { return addrs[k] }
}

func TestMeasurements(t *testing.T) {
	addr := servers(t)
	ctx := context.Background()
	d := 200 * time.Millisecond
	r, err := Download(ctx, addr(Source), d)
	if err != nil || r.Bytes == 0 || r.Gbit <= 0 || r.Corrupt != 0 {
		t.Fatalf("download: %+v %v", r, err)
	}
	r, err = Upload(ctx, addr(Sink), d)
	if err != nil || r.Bytes == 0 || r.Gbit <= 0 || r.Corrupt != 0 {
		t.Fatalf("upload: %+v %v", r, err)
	}
	r, err = RoundTrips(ctx, addr(Echo), 200, 64, "")
	if err != nil || r.N != 200 || r.P50us <= 0 || r.MaxUs < r.P99us {
		t.Fatalf("rr: %+v %v", r, err)
	}
	r, err = RoundTrips(ctx, addr(Echo), 200, 64, addr(Source))
	if err != nil || r.Name != "rr-loaded" || r.N != 200 {
		t.Fatalf("rr loaded: %+v %v", r, err)
	}
	r, err = ConnsPerSecond(ctx, addr(Ping), d, 2)
	if err != nil || r.N == 0 || r.Errors != 0 {
		t.Fatalf("cps: %+v %v", r, err)
	}
	// Let the servers see the last connections close before the leak check.
	time.Sleep(100 * time.Millisecond)
}

func TestResultRoundTrip(t *testing.T) {
	in := Result{Name: "download", Bytes: 5, Gbit: 1.5}
	out, err := ParseResult(in.JSON())
	if err != nil || out != in {
		t.Fatalf("%+v %v", out, err)
	}
	if _, err := ParseResult([]byte(`{"gbit":1}`)); err == nil {
		t.Fatal("result without a name accepted")
	}
}

func TestLatency(t *testing.T) {
	ns := make([]int64, 1000)
	for i := range ns {
		ns[i] = int64(1000-i) * 1000
	}
	r := Latency(ns)
	if r.N != 1000 || r.P50us != 500 || r.P99us != 990 || r.P999us != 999 || r.MaxUs != 1000 {
		t.Fatalf("%+v", r)
	}
}
