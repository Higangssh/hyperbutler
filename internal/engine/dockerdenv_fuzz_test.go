package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Higangssh/hyperbutler/internal/dockerenv"
)

// FuzzBuildDockerdEnvNoProxy (review M-A): whatever proxy.noProxy `config set` accepts
// and whatever registries daemon.json lists, the document is valid, so it is sent with
// the proxy and the certificates, and the user's whole list is in it.
func FuzzBuildDockerdEnvNoProxy(f *testing.F) {
	f.Add("corp.example,10.0.0.0/8", "192.168.1.5:5000\nhttps://mirror.lan:5000")
	f.Add(longNoProxy(4096), "registry.lan:5000")
	f.Add(longNoProxy(4000), strings.Repeat("r.lan.example\n", 200))
	f.Add("LOCALHOST,*,.x", "localhost\n[::1]:5000\nhttp://[fe80::1%25eth0]:80")
	f.Fuzz(func(t *testing.T, user, regs string) {
		if user != "" && (dockerenv.ValidateNoProxy(user) != nil || dockerenv.NormalizeNoProxy(user) != user) {
			return // config set and Settings.Validate refuse it
		}
		dj, err := json.Marshal(map[string][]string{"insecure-registries": strings.Split(regs, "\n")})
		if err != nil {
			return
		}
		noStores(t)
		off := false
		s := &Settings{CAImport: &off, HTTPProxy: "http://user:pw@127.0.0.1:3128", HTTPSProxy: "http://proxy.corp:3128", NoProxy: user}
		doc, st := BuildDockerdEnv(s, testEnvNet, dj, "")
		if err := doc.Validate(); err != nil {
			t.Fatalf("invalid document: %v", err)
		}
		if doc.HTTPProxy == "" || doc.HTTPSProxy == "" || st.HTTPSProxy == "" || st.NoProxy != doc.NoProxy {
			t.Fatalf("proxy dropped: %+v (%s)", st, st.Error)
		}
		if strings.Contains(st.Error, "not used") {
			t.Fatal(st.Error)
		}
		have := map[string]bool{}
		for _, e := range strings.Split(doc.NoProxy, ",") {
			have[strings.ToLower(e)] = true
		}
		for _, e := range strings.Split(user, ",") {
			if e != "" && !have[strings.ToLower(e)] {
				t.Fatalf("user entry %q left out", e)
			}
		}
		if _, err := doc.Encode(); err != nil {
			t.Fatal(err)
		}
	})
}
