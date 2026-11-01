package i18n

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Higangssh/hyperbutler/internal/config"
	"github.com/Higangssh/hyperbutler/internal/engine"
	"github.com/Higangssh/hyperbutler/internal/gui/bridge"
	"github.com/Higangssh/hyperbutler/internal/leaktest"
)

// TestMain fails the package's tests if any goroutine is still running after
// they finish (ADR 0003, guardrail 1).
func TestMain(m *testing.M) {
	leaktest.VerifyTestMain(m)
}

// baseKey strips a plural suffix.
func baseKey(k string) (string, string) {
	if i := strings.LastIndexByte(k, '#'); i >= 0 {
		return k[:i], k[i+1:]
	}
	return k, ""
}

// TestCatalogComplete is REQ-UX-013's completeness test: every language has every
// source key (with the plural forms it uses), no key the source lacks, the same
// placeholders, and no empty text.
func TestCatalogComplete(t *testing.T) {
	src := Raw(Source)
	if len(src) < 100 {
		t.Fatalf("the source catalog has %d keys", len(src))
	}
	if !reflect.DeepEqual(Languages(), append([]string{Source}, engine.GUILanguages[1:]...)) {
		t.Fatalf("shipped languages %v, but gui.language accepts %v", Languages(), engine.GUILanguages)
	}
	for _, lang := range Languages() {
		cat := Raw(lang)
		forms := PluralForms[lang]
		if len(forms) == 0 {
			t.Fatalf("%s: no plural forms listed in PluralForms", lang)
		}
		// Each source key, or each plural base, must be there.
		seen := map[string]bool{}
		for k, v := range src {
			b, form := baseKey(k)
			if form == "" {
				got, ok := cat[k]
				if !ok {
					t.Errorf("%s: missing %q", lang, k)
					continue
				}
				if !reflect.DeepEqual(Placeholders(got), Placeholders(v)) {
					t.Errorf("%s: %q has placeholders %v, the source %v", lang, k, Placeholders(got), Placeholders(v))
				}
				continue
			}
			if seen[b] {
				continue
			}
			seen[b] = true
			for _, f := range forms {
				got, ok := cat[b+"#"+f]
				if !ok {
					t.Errorf("%s: missing plural form %q", lang, b+"#"+f)
					continue
				}
				if want := Placeholders(src[b+"#other"]); !reflect.DeepEqual(Placeholders(got), want) {
					t.Errorf("%s: %q has placeholders %v, the source %v", lang, b+"#"+f, Placeholders(got), want)
				}
			}
		}
		for k, v := range cat {
			b, form := baseKey(k)
			switch {
			case form == "" && src[k] == "":
				t.Errorf("%s: %q is not in the source catalog", lang, k)
			case form != "" && !slices.Contains(forms, form):
				t.Errorf("%s: %q: %s does not use the plural form %q", lang, k, lang, form)
			case form != "" && src[b+"#other"] == "":
				t.Errorf("%s: %q is not a plural in the source catalog", lang, k)
			}
			if strings.TrimSpace(v) == "" || !utf8.ValidString(v) {
				t.Errorf("%s: %q is empty or not UTF-8", lang, k)
			}
			if strings.ContainsAny(v, "<>") {
				t.Errorf("%s: %q holds markup; the page shows text only", lang, k)
			}
		}
	}
}

var (
	reCall = regexp.MustCompile(`\bt[p]?\("([a-zA-Z0-9.#_-]+)"`)
	reAttr = regexp.MustCompile(`data-k(?:-aria|-alt)?="([a-zA-Z0-9.#_-]+)"`)
)

// TestPageKeys checks that every key the page names is in the source catalog: the
// literal ones in app.js and index.html, and the families app.js builds at run time.
func TestPageKeys(t *testing.T) {
	src := Raw(Source)
	has := func(k string) bool {
		if _, ok := src[k]; ok {
			return true
		}
		_, ok := src[k+"#other"]
		return ok
	}
	dir := filepath.Join("..", "web")
	var keys []string
	for _, f := range []string{"app.js", "index.html"} {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range reCall.FindAllStringSubmatch(string(b), -1) {
			keys = append(keys, m[1])
		}
		for _, m := range reAttr.FindAllStringSubmatch(string(b), -1) {
			keys = append(keys, m[1])
		}
	}
	if len(keys) < 80 {
		t.Fatalf("found only %d literal keys in the page", len(keys))
	}
	phases := []string{"notsetup", "stopped", "starting", "running", "stopping", "failed", "paused", "restarting"}
	for _, p := range phases {
		keys = append(keys, "phase."+p, "home.title."+p)
		if p != "running" {
			keys = append(keys, "home.sub."+p)
		}
	}
	for _, op := range []string{"engine.start", "engine.stop", "engine.restart", "engine.restartDockerd"} {
		keys = append(keys, "done."+op, "fail."+op)
	}
	for _, w := range []string{"engine.stop", "engine.restart", "dockerd"} {
		keys = append(keys, "confirm."+w+".title", "confirm."+w+".body.stopped", "confirm."+w+".ok")
	}
	for _, a := range []string{"stop", "restart"} {
		keys = append(keys, "confirm.container."+a+".title", "confirm.container."+a+".body")
	}
	for _, a := range []string{"start", "stop", "restart"} {
		keys = append(keys, "done.container."+a, "fail.container."+a, "action."+a)
	}
	for _, th := range []string{"back", "maybe", "stays"} {
		keys = append(keys, "stop.then."+th)
	}
	for _, s := range []string{bridge.ImportNew, bridge.ImportDone, bridge.ImportPartial, bridge.ImportConflict} {
		keys = append(keys, "import.status."+s)
	}
	for _, s := range []string{"ok", "info", "warn", "fail"} {
		keys = append(keys, "check."+s)
	}
	for _, a := range bridge.ApplyTimes {
		keys = append(keys, "apply."+string(a))
	}
	// Keys the host uses (cmd/hyperbutler/gui_windows.go): the window title, the note at
	// the first close while the engine runs, and the missing-runtime message.
	keys = append(keys, "app.title", "gui.closeNote", "gui.noRuntime")
	for _, c := range bridge.Codes {
		keys = append(keys, "error."+c)
	}
	for _, k := range config.Keys {
		keys = append(keys, "setting."+k.Name, "setting."+k.Name+".help")
	}
	for _, k := range keys {
		if strings.HasSuffix(k, ".") {
			continue // the start of a key app.js completes at run time (a family above)
		}
		if !has(k) {
			t.Errorf("the page uses %q, which the source catalog lacks", k)
		}
	}
}

// TestPseudoLonger checks the pseudo-locale is at least 30% longer than English for
// every message, so the layout check exercises REQ-UX-013's room for longer text.
func TestPseudoLonger(t *testing.T) {
	ps := Messages(Pseudo)
	for k, v := range Raw(Source) {
		p := ps[k]
		if utf8.RuneCountInString(p) < utf8.RuneCountInString(v)*13/10 {
			t.Errorf("%q: pseudo %q is not 30%% longer than %q", k, p, v)
		}
		if !reflect.DeepEqual(Placeholders(p), Placeholders(v)) {
			t.Errorf("%q: pseudo lost placeholders: %q", k, p)
		}
	}
}

func TestResolve(t *testing.T) {
	for _, c := range []struct {
		setting string
		windows []string
		want    string
	}{
		{"", []string{"ko-KR", "en-US"}, "ko"},
		{"auto", []string{"fr-FR", "ko-KR"}, "ko"},
		{"", []string{"fr-FR"}, "en"},
		{"en", []string{"ko-KR"}, "en"},
		{"ko", nil, "ko"},
		{"xx", []string{"ko-KR"}, "ko"},
		{"pseudo", nil, "pseudo"},
	} {
		if got := Resolve(c.setting, c.windows); got != c.want {
			t.Errorf("Resolve(%q, %v) = %q, want %q", c.setting, c.windows, got, c.want)
		}
	}
	if m := Messages("ko"); m["tab.home"] != "홈" || m["app.title"] != "hyperbutler" {
		t.Errorf("ko messages: %q %q", m["tab.home"], m["app.title"])
	}
}
