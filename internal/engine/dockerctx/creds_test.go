package dockerctx

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCredHelpers(t *testing.T) {
	got, err := CredHelpers([]byte(`{"auths":{"x":{"auth":"c2VjcmV0"}},"credsStore":"desktop","credHelpers":{"gcr.io":"gcloud","b.example":"ecr-login","empty.example":""}}`))
	want := []CredHelper{{Name: "desktop"}, {Registry: "b.example", Name: "ecr-login"}, {Registry: "gcr.io", Name: "gcloud"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v %v", got, err)
	}
	if got[0].Program() != "docker-credential-desktop" {
		t.Fatal(got[0].Program())
	}
	for _, cfg := range []string{``, `{}`, `null`, `{"credsStore":""}`} {
		if got, err := CredHelpers([]byte(cfg)); err != nil || len(got) != 0 {
			t.Errorf("%q: %+v %v", cfg, got, err)
		}
	}
	for _, cfg := range []string{`{"credsStore":"..\\evil"}`, `{"credsStore":"a/b"}`, `{"credsStore":1}`, `{"credHelpers":{"r":"x y"}}`, `{"credHelpers":[]}`, `{`} {
		if _, err := CredHelpers([]byte(cfg)); err == nil {
			t.Errorf("%q accepted", cfg)
		}
	}
}

// FuzzCredHelpers: whatever config.json holds, every helper reported has a name the
// docker CLI could look up on PATH, never a path.
func FuzzCredHelpers(f *testing.F) {
	f.Add([]byte(`{"credsStore":"desktop","credHelpers":{"gcr.io":"gcloud"}}`))
	f.Add([]byte(`{"credsStore":"..\\x","credHelpers":{"":""}}`))
	f.Add([]byte(`{"credHelpers":null,"credsStore":null}`))
	f.Fuzz(func(t *testing.T, cfg []byte) {
		hs, err := CredHelpers(cfg)
		if err != nil {
			return
		}
		if len(hs) > maxCredHelpers+1 {
			t.Fatalf("%d helpers", len(hs))
		}
		for _, h := range hs {
			if !ValidHelperName(h.Name) || strings.ContainsAny(h.Program(), `\/:`) {
				t.Fatalf("helper %q", h.Name)
			}
		}
	})
}

// TestStoreCredHelpersReadOnly: reading the helpers leaves config.json byte for byte.
func TestStoreCredHelpersReadOnly(t *testing.T) {
	dir := t.TempDir()
	s := Store{Dir: dir}
	if got, err := s.CredHelpers(); err != nil || got != nil {
		t.Fatalf("no config.json: %v %v", got, err)
	}
	cfg := "{\r\n  \"credsStore\": \"wincred\"\r\n}\r\n"
	p := filepath.Join(dir, "config.json")
	if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := s.CredHelpers()
	if err != nil || len(got) != 1 || got[0].Name != "wincred" {
		t.Fatalf("%+v %v", got, err)
	}
	if b, _ := os.ReadFile(p); string(b) != cfg || !strings.EqualFold(s.ConfigPath(), p) {
		t.Fatal("config.json changed")
	}
}
