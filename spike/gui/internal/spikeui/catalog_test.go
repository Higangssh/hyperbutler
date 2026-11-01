package spikeui

import (
	"errors"
	"strings"
	"testing"

	"github.com/Higangssh/hyperbutler/internal/leaktest"
)

func TestMain(m *testing.M) {
	leaktest.VerifyTestMain(m)
}

// TestCatalogComplete is the completeness check REQ-UX-013 asks of the real catalog:
// every language has every English key and no key English lacks.
func TestCatalogComplete(t *testing.T) {
	en := Catalog["en"]
	for lang, msgs := range Catalog {
		for k := range en {
			if strings.TrimSpace(msgs[k]) == "" {
				t.Errorf("%s: missing %q", lang, k)
			}
		}
		for k := range msgs {
			if _, ok := en[k]; !ok {
				t.Errorf("%s: %q is not an English key", lang, k)
			}
		}
	}
}

func TestMessagesFallback(t *testing.T) {
	if got := Messages("ko-KR")[KeyStart]; got != Catalog["ko"][KeyStart] {
		t.Errorf("ko-KR start = %q", got)
	}
	if got := Messages("fr-FR")[KeyStart]; got != Catalog["en"][KeyStart] {
		t.Errorf("fr-FR start = %q, want the English text", got)
	}
}

func TestParseStatus(t *testing.T) {
	s, err := ParseStatus(strings.NewReader(`{"schema":1,"running":true,"setUp":true,"memoryMB":4096,"cpus":4,
		"diskHostBytes":2147483648,"diskSizeBytes":68719476736,"state":{"phase":"running"},"home":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	v := MakeView(Messages("en"), s, nil)
	if !v.Live || v.Engine != "Running" || v.Memory != "4096 MB" || v.CPUs != "4" || v.Disk != "2.0 GB" {
		t.Errorf("view = %+v", v)
	}
	if _, err := ParseStatus(strings.NewReader(`{"schema":2}`)); err == nil {
		t.Error("schema 2 accepted")
	}
	if v := MakeView(Messages("ko"), Status{}, errors.New("boom")); v.Live || v.Note != "boom" {
		t.Errorf("error view = %+v", v)
	}
}
