package bridge_test

import (
	"strings"
	"testing"

	"github.com/Higangssh/hyperbutler/internal/engine"
	"github.com/Higangssh/hyperbutler/internal/gui/bridge"
	"github.com/Higangssh/hyperbutler/internal/gui/i18n"
)

// TestBrokerCodes: each broker failure of the engine has its own GUI code, and
// HB-VM-005, which covers every way an installed service cannot be used (disabled, a
// start refused, stopped again, not answering, a sign-in it does not serve), gives steps
// that hold for all of them: not "wait a minute" or "restart it", which fit only some.
func TestBrokerCodes(t *testing.T) {
	for code, want := range map[string]string{
		engine.CodeBrokerLost:        bridge.CodeBrokerLost,
		engine.CodeBrokerUnavailable: bridge.CodeBrokerDown,
		engine.CodeBrokerRefused:     bridge.CodeBrokerRefused,
		engine.CodeBrokerSquatted:    bridge.CodeBrokerSquat,
		engine.CodeBrokerOld:         bridge.CodeBrokerOld,
	} {
		if got := bridge.EngineCode(code); got != want {
			t.Errorf("%s maps to %s, want %s", code, got, want)
		}
	}
	for _, lang := range i18n.Languages() {
		cat := i18n.Raw(lang)
		steps := cat["error."+bridge.CodeBrokerDown+".step1"] + " " + cat["error."+bridge.CodeBrokerDown+".step2"]
		if !strings.Contains(steps, "hyperbutler doctor") {
			t.Errorf("%s: HB-VM-005's steps do not point to hyperbutler doctor: %q", lang, steps)
		}
		for _, wrong := range []string{"sc.exe stop", "minute", "1분"} {
			if strings.Contains(steps, wrong) {
				t.Errorf("%s: HB-VM-005's steps say %q, which does not fit every cause: %q", lang, wrong, steps)
			}
		}
	}
}
