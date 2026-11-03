package diag

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"

	"github.com/Higangssh/hyperbutler/internal/leaktest"
)

func TestMain(m *testing.M) {
	leaktest.VerifyTestMain(m, leaktest.IgnoreIOCP)
}

func TestParseMemoryLimit(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int64
		ok   bool
	}{
		{"0", 0, true},
		{"1", 1, true},
		{"268435456", 268435456, true},
		{"512B", 512, true},
		{"64KiB", 64 << 10, true},
		{"512MiB", 512 << 20, true},
		{"2GiB", 2 << 30, true},
		{"1TiB", 1 << 40, true},
		{"off", math.MaxInt64, true},
		{"9223372036854775807", math.MaxInt64, true},
		{"8388607TiB", 8388607 << 40, true},

		{"", 0, false},
		{"B", 0, false},
		{"MiB", 0, false},
		{"-1", 0, false},
		{"+1", 0, false},
		{" 1", 0, false},
		{"1 MiB", 0, false},
		{"1.5GiB", 0, false},
		{"1MB", 0, false},
		{"1mib", 0, false},
		{"1M", 0, false},
		{"OFF", 0, false},
		{"0x10", 0, false},
		{"9223372036854775808", 0, false},
		{"8388608TiB", 0, false},
		{"1GiBB", 0, false},
		{"1KiBMiB", 0, false},
	} {
		got, err := ParseMemoryLimit(tc.in)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("ParseMemoryLimit(%q) = %d, %v; want %d, ok=%v", tc.in, got, err, tc.want, tc.ok)
		}
	}
}

func TestApplyMemoryLimit(t *testing.T) {
	const current = int64(1 << 40)
	for _, tc := range []struct {
		name    string
		env     string
		envSet  bool
		limit   string
		want    int64
		source  LimitSource
		wantSet int64 // -1 means set was never called with a new limit
		wantErr bool
	}{
		{name: "env wins over config", env: "1GiB", envSet: true, limit: "256MiB", want: current, source: SourceEnv, wantSet: -1},
		{name: "empty env is unset", env: "", envSet: true, limit: "256MiB", want: 256 << 20, source: SourceConfig, wantSet: 256 << 20},
		{name: "config applied", limit: "64MiB", want: 64 << 20, source: SourceConfig, wantSet: 64 << 20},
		{name: "config off", limit: "off", want: math.MaxInt64, source: SourceConfig, wantSet: math.MaxInt64},
		{name: "nothing set", want: current, source: SourceDefault, wantSet: -1},
		{name: "below minimum", limit: "1MiB", wantErr: true, wantSet: -1},
		{name: "zero", limit: "0", wantErr: true, wantSet: -1},
		{name: "garbage", limit: "lots", wantErr: true, wantSet: -1},
		{name: "env set, bad config ignored", env: "off", envSet: true, limit: "lots", want: current, source: SourceEnv, wantSet: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setTo := int64(-1)
			lookup := func(k string) (string, bool) {
				if k != "GOMEMLIMIT" {
					t.Errorf("looked up %q", k)
				}
				return tc.env, tc.envSet
			}
			set := func(n int64) int64 {
				if n < 0 {
					return current
				}
				setTo = n
				return current
			}
			got, src, err := applyMemoryLimit(tc.limit, lookup, set)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if setTo != tc.wantSet {
				t.Errorf("SetMemoryLimit called with %d, want %d", setTo, tc.wantSet)
			}
			if tc.wantErr {
				return
			}
			if got != tc.want || src != tc.source {
				t.Errorf("got %d from %s, want %d from %s", got, src, tc.want, tc.source)
			}
		})
	}
}

func TestStartOffByDefault(t *testing.T) {
	t.Setenv("GOMEMLIMIT", "off") // keep the zero Config from touching the real limit
	s, err := Start(Config{})
	if err != nil || s != nil {
		t.Fatalf("Start(zero Config) = %v, %v; want nil, nil", s, err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("nil Server Close: %v", err)
	}
}

func TestSnapshot(t *testing.T) {
	s := Snapshot()
	if s.Goroutines < 1 {
		t.Errorf("Goroutines = %d", s.Goroutines)
	}
	if s.Handles < 1 && (s.Handles != -1 || runtime.GOOS == "windows" || runtime.GOOS == "linux") {
		t.Errorf("Handles = %d", s.Handles)
	}
	if s.MemStats.HeapAlloc == 0 || s.MemStats.Sys == 0 || s.MemoryLimit <= 0 {
		t.Errorf("implausible snapshot %+v", s)
	}
	switch o := s.OS; {
	case runtime.GOOS != "windows":
		if o != nil {
			t.Errorf("OS memory off Windows: %+v", o)
		}
	case o == nil:
		t.Error("no OS memory on Windows")
	default:
		// The process heap always exists; native heaps and Go's own mappings are both
		// part of the private bytes.
		if o.Heaps < 1 || o.HeapCommitted == 0 || o.HeapAllocated > o.HeapCommitted ||
			o.PrivateBytes < o.HeapCommitted || o.WorkingSet == 0 {
			t.Errorf("implausible OS memory %+v", o)
		}
	}
}

func TestParseSeconds(t *testing.T) {
	for _, v := range []string{"0", "1", "0.5", "30", "60", "60.0", "1e1"} {
		if _, err := ParseSeconds(v); err != nil {
			t.Errorf("ParseSeconds(%q): %v", v, err)
		}
	}
	for _, v := range []string{"", "-1", "61", "60.001", "NaN", "Inf", "+Inf", "1e9", "abc", "0x10p0x", "9999999999999999999999999999999999"} {
		if _, err := ParseSeconds(v); err == nil {
			t.Errorf("ParseSeconds(%q) accepted", v)
		}
	}
}

func TestHandlerRoutes(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()
	c := srv.Client()
	defer c.CloseIdleConnections()

	for _, tc := range []struct {
		method, path string
		code         int
	}{
		{"GET", "/debug/stats", 200},
		{"GET", "/debug/pprof/", 200},
		{"GET", "/debug/pprof/goroutine?debug=1", 200},
		{"GET", "/debug/pprof/heap", 200},
		{"GET", "/debug/pprof/cmdline", 200},
		{"GET", "/debug/pprof/profile?seconds=61", 400},
		{"GET", "/debug/pprof/trace?seconds=-1", 400},
		{"GET", "/debug/pprof/trace?seconds=0.05", 200},
		{"GET", "/debug/pprof/goroutine?seconds=70", 400},
		{"GET", "/debug/pprof/heap?seconds=1&seconds=99", 400},
		{"GET", "/debug/pprof/profile?seconds=1;x", 400},
		{"POST", "/debug/stats", 405},
		{"GET", "/", 404},
		{"GET", "/debug/vars", 404},
	} {
		req, err := http.NewRequest(tc.method, srv.URL+tc.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", tc.method, tc.path, err)
		}
		if resp.StatusCode != tc.code {
			t.Errorf("%s %s = %d, want %d", tc.method, tc.path, resp.StatusCode, tc.code)
		}
		if tc.path == "/debug/stats" && tc.code == 200 {
			var s Stats
			if err := json.NewDecoder(resp.Body).Decode(&s); err != nil || s.Goroutines < 1 {
				t.Errorf("stats body: %+v, %v", s, err)
			}
		}
		resp.Body.Close()
	}
}
