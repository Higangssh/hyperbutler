package dockerctx

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

const host = "npipe:////./pipe/docker_engine_winbox"

func TestEnsureCreatesManagedContext(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	if err := s.Ensure("winbox", host, "winbox"); err != nil {
		t.Fatal(err)
	}
	// Where `docker context` looks: meta/<sha256("winbox")>/meta.json.
	p := filepath.Join(s.Dir, "contexts", "meta", "e357a9c858c4388f8e18f7a2a36e017445a29e5b26fb318d136c1116ece1e27a", "meta.json")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("meta.json not at the docker CLI's path: %v", err)
	}
	var m struct {
		Name      string
		Metadata  map[string]any
		Endpoints map[string]map[string]any
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m.Name != "winbox" || m.Metadata[ManagedKey] != true || m.Endpoints["docker"]["Host"] != host {
		t.Fatalf("meta.json = %s", b)
	}
	c, err := s.Get("winbox")
	if err != nil || !c.Managed || c.Host != host {
		t.Fatalf("Get = %+v, %v", c, err)
	}
	// Ours: updated in place.
	if err := s.Ensure("winbox", "npipe:////./pipe/other", "winbox"); err != nil {
		t.Fatal(err)
	}
	if c, _ := s.Get("winbox"); c.Host != "npipe:////./pipe/other" {
		t.Fatalf("not updated: %+v", c)
	}
}

func TestEnsureLeavesForeignContext(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	foreign := []byte(`{"Name":"winbox","Metadata":{"Description":"mine"},"Endpoints":{"docker":{"Host":"tcp://10.0.0.5:2376","SkipTLSVerify":false}}}`)
	if err := os.MkdirAll(s.metaDir("winbox"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.metaPath("winbox"), foreign, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Ensure("winbox", host, "winbox"); !errors.Is(err, ErrNotOurs) {
		t.Fatalf("Ensure over a foreign context: %v", err)
	}
	if err := s.Remove("winbox"); !errors.Is(err, ErrNotOurs) {
		t.Fatalf("Remove of a foreign context: %v", err)
	}
	if b, _ := os.ReadFile(s.metaPath("winbox")); !bytes.Equal(b, foreign) {
		t.Fatalf("foreign context changed: %s", b)
	}
	// "Managed" must be a real true, not a string someone typed.
	for _, v := range []string{`"true"`, `1`, `false`, `null`} {
		b := []byte(`{"Name":"winbox","Metadata":{"` + ManagedKey + `":` + v + `},"Endpoints":{}}`)
		_ = os.WriteFile(s.metaPath("winbox"), b, 0o600)
		if err := s.Ensure("winbox", host, "winbox"); !errors.Is(err, ErrNotOurs) {
			t.Errorf("managed=%s counted as ours: %v", v, err)
		}
	}
	// An unreadable context is not ours either.
	_ = os.WriteFile(s.metaPath("winbox"), []byte("{"), 0o600)
	if err := s.Ensure("winbox", host, "winbox"); !errors.Is(err, ErrNotOurs) {
		t.Fatalf("corrupt meta.json: %v", err)
	}
}

func TestRemove(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	if err := s.Remove("winbox"); err != nil {
		t.Fatalf("remove missing: %v", err)
	}
	if err := s.Ensure("winbox", host, "winbox"); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove("winbox"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("winbox"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("still there: %v", err)
	}
}

func TestUseKeepsOtherKeys(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	if cur, err := s.Current(); err != nil || cur != "" {
		t.Fatalf("no config: %q %v", cur, err)
	}
	cfg := `{"auths":{"registry.example":{"auth":"dXNlcjpwYXNz"}},"credsStore":"desktop","currentContext":"desktop-linux","plugins":{"x":{"a":[1,2]}}}`
	if err := os.WriteFile(filepath.Join(s.Dir, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Use("winbox"); err != nil {
		t.Fatal(err)
	}
	if cur, _ := s.Current(); cur != "winbox" {
		t.Fatalf("current %q", cur)
	}
	var before, after map[string]any
	_ = json.Unmarshal([]byte(cfg), &before)
	b, _ := os.ReadFile(filepath.Join(s.Dir, "config.json"))
	if err := json.Unmarshal(b, &after); err != nil {
		t.Fatal(err)
	}
	delete(before, "currentContext")
	delete(after, "currentContext")
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("other keys changed:\n%v\n%v", before, after)
	}
	if err := s.Use(""); err != nil {
		t.Fatal(err)
	}
	if cur, _ := s.Current(); cur != "" {
		t.Fatalf("after Use(\"\"): %q", cur)
	}
	// A config.json winbox cannot parse is left alone.
	_ = os.WriteFile(filepath.Join(s.Dir, "config.json"), []byte("{oops"), 0o600)
	if err := s.Use("winbox"); err == nil {
		t.Fatal("rewrote an unparsable config.json")
	}
	if b, _ := os.ReadFile(filepath.Join(s.Dir, "config.json")); string(b) != "{oops" {
		t.Fatalf("config.json changed: %s", b)
	}
}

// FuzzParseMeta: never panics; a context counts as managed only with a JSON true.
func FuzzParseMeta(f *testing.F) {
	f.Add([]byte(`{"Name":"winbox","Metadata":{"Description":"x","WinboxManaged":true},"Endpoints":{"docker":{"Host":"npipe:////./pipe/x"}}}`))
	f.Add([]byte(`{"Name":"winbox","Metadata":{"WinboxManaged":"true"},"Endpoints":{"docker":7}}`))
	f.Add([]byte(`{"Name":"","Metadata":null}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		c, err := ParseMeta(b)
		if err != nil {
			return
		}
		if c.Name == "" {
			t.Fatal("accepted a context without a name")
		}
		if c.Managed {
			var m struct{ Metadata map[string]json.RawMessage }
			_ = json.Unmarshal(b, &m)
			var v bool
			if json.Unmarshal(m.Metadata[ManagedKey], &v) != nil || !v {
				t.Fatalf("managed without a JSON true: %s", b)
			}
		}
	})
}

// FuzzSetCurrentContext: the result parses, selects exactly name, and keeps every other
// key's value.
func FuzzSetCurrentContext(f *testing.F) {
	f.Add([]byte(`{"auths":{},"currentContext":"default"}`), "winbox")
	f.Add([]byte(``), "winbox")
	f.Add([]byte(`null`), "")
	f.Add([]byte(`{"currentContext":"winbox","currentContext":"x"}`), "winbox")
	f.Add([]byte(`{"a":"<&>"}`), "w ")
	f.Fuzz(func(t *testing.T, cfg []byte, name string) {
		out, err := SetCurrentContext(cfg, name)
		if err != nil {
			return
		}
		got, err := CurrentContext(out)
		if err != nil || got != name {
			t.Fatalf("selected %q (%v), want %q", got, err, name)
		}
		in, _ := parseConfig(cfg)
		res, err := parseConfig(out)
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range in {
			if k == "currentContext" {
				continue
			}
			var a, b any
			_ = json.Unmarshal(v, &a)
			_ = json.Unmarshal(res[k], &b)
			if !reflect.DeepEqual(a, b) {
				t.Fatalf("key %q changed: %s -> %s", k, v, res[k])
			}
		}
	})
}
