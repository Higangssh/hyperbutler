//go:build windows

package main

import (
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Higangssh/hyperbutler/internal/engine"
	"github.com/Higangssh/hyperbutler/internal/updatecheck"
)

// releaseAPI is a test server in place of GitHub's release API, injected through
// newUpdateChecker with the check's real client.
type releaseAPI struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []http.Header
}

func newReleaseAPI(t *testing.T, status int, body string) *releaseAPI {
	t.Helper()
	a := &releaseAPI{}
	a.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.reqs = append(a.reqs, r.Header.Clone())
		a.mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(a.srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(a.srv.Certificate())
	old := newUpdateChecker
	newUpdateChecker = func(version string, _ *engine.Settings) *updatecheck.Checker {
		return &updatecheck.Checker{URL: a.srv.URL + "/repos/Higangssh/hyperbutler/releases/latest", Version: version, Client: updatecheck.NewClient(nil, pool)}
	}
	t.Cleanup(func() { newUpdateChecker = old })
	return a
}

func (a *releaseAPI) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.reqs)
}

func setUpdateNow(t *testing.T, now *time.Time) {
	t.Helper()
	old := updateNow
	updateNow = func() time.Time { return *now }
	t.Cleanup(func() { updateNow = old })
}

// TestStatusUpdateCheck: status asks once a day, with only the User-Agent header, shows
// the result in --json, and asks nothing with updateCheck off (REQ-UX-022).
func TestStatusUpdateCheck(t *testing.T) {
	home, _ := testHome(t)
	api := newReleaseAPI(t, http.StatusOK, `{"tag_name":"v9.0.0","html_url":"https://github.com/Higangssh/hyperbutler/releases/tag/v9.0.0"}`)
	now := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	setUpdateNow(t, &now)

	// Not set up yet: no check.
	runCLI(t, "status")
	if api.count() != 0 {
		t.Fatal("checked before the first setup")
	}
	if code, _, _ := runCLI(t, "config", "set", "memory", "4GiB"); code != 0 {
		t.Fatal("config set")
	}
	_, out, _ := runCLI(t, "status", "--json")
	var r StatusJSON
	if err := json.Unmarshal([]byte(out), &r); err != nil || r.Update == nil || r.Update.Latest != "v9.0.0" ||
		r.Update.URL != "https://github.com/Higangssh/hyperbutler/releases/tag/v9.0.0" || !r.Update.Checked.Equal(now) {
		t.Fatalf("status --json: %v %q", err, out)
	}
	// A development build (as the tests are) never prints the notice.
	_, out, _ = runCLI(t, "status")
	if strings.Contains(out, "is available") {
		t.Fatalf("a development build nags: %q", out)
	}
	now = now.Add(23 * time.Hour)
	runCLI(t, "status")
	if n := api.count(); n != 1 {
		t.Fatalf("%d requests within a day", n)
	}
	now = now.Add(time.Hour)
	runCLI(t, "status")
	if n := api.count(); n != 2 {
		t.Fatalf("%d requests after a day", n)
	}
	api.mu.Lock()
	for i, h := range api.reqs {
		var names []string
		for k := range h {
			names = append(names, k)
		}
		sort.Strings(names)
		if strings.Join(names, ",") != "User-Agent" || !strings.HasPrefix(h.Get("User-Agent"), "hyperbutler/") {
			t.Errorf("request %d headers %v", i, h)
		}
	}
	api.mu.Unlock()
	if _, err := os.Stat(updateRecordPath(engine.Layout{Home: home})); err != nil {
		t.Fatal(err)
	}

	// Off: no request, no update field, whatever the time.
	if code, _, _ := runCLI(t, "config", "set", "updateCheck", "false"); code != 0 {
		t.Fatal("config set updateCheck false")
	}
	now = now.Add(48 * time.Hour)
	_, out, _ = runCLI(t, "status", "--json")
	if api.count() != 2 || strings.Contains(out, `"update"`) {
		t.Fatalf("checked with updateCheck off: %d %q", api.count(), out)
	}
}

// TestUpdateCheckFailureSilent: a failed check (here GitHub's 404 for a repository
// without releases) prints nothing, unless HYPERBUTLER_DEBUG is set.
func TestUpdateCheckFailureSilent(t *testing.T) {
	testHome(t)
	newReleaseAPI(t, http.StatusNotFound, `{"message":"Not Found"}`)
	if code, _, _ := runCLI(t, "config", "set", "memory", "4GiB"); code != 0 {
		t.Fatal("config set")
	}
	t.Setenv("HYPERBUTLER_DEBUG", "")
	_, out, e := runCLI(t, "status")
	if strings.Contains(out+e, "update") {
		t.Fatalf("a failed check was shown: %q %q", out, e)
	}
	var now = time.Now().Add(25 * time.Hour)
	setUpdateNow(t, &now)
	t.Setenv("HYPERBUTLER_DEBUG", "1")
	_, _, e = runCLI(t, "status")
	if !strings.Contains(e, "hyperbutler: debug: update check: updatecheck: no release") {
		t.Fatalf("debug line: %q", e)
	}
}

func TestUpdateProxy(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, updatecheck.ReleaseURL, nil)
	u, err := updateProxy(&engine.Settings{HTTPSProxy: "http://user:pw@proxy.corp:3128"})(req)
	if err != nil || u == nil || u.Host != "proxy.corp:3128" {
		t.Fatalf("proxy.https: %v %v", u, err)
	}
	if updateProxy(nil) == nil || updateProxy(&engine.Settings{}) == nil {
		t.Fatal("no environment proxy function")
	}
}
