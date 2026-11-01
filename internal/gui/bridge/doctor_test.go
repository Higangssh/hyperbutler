package bridge

import (
	"strings"
	"testing"

	"github.com/Higangssh/hyperbutler/internal/schema"
)

// bidiOverride is U+202E, RIGHT-TO-LEFT OVERRIDE.
const bidiOverride = rune(0x202e)

// TestDoctorKeys: the page gets a check's catalog key and values only in their shape,
// cleaned like the summary.
func TestDoctorKeys(t *testing.T) {
	args := map[string]string{"pipe": `\\.\pipe\x` + string(bidiOverride), "Bad-Name": "x", "n": "2"}
	for i := 0; i < 12; i++ {
		args["extra"+strings.Repeat("a", i)] = "v"
	}
	v := MakeDoctor(schema.Doctor{OK: true, Checks: []schema.DoctorCheck{
		{ID: "pipe", Status: "ok", Summary: "s", Msg: "doctor.pipe.free", FixMsg: "fail.engine.start", Args: args},
		{ID: "hcs", Status: "ok", Summary: "s", Msg: "doctor.x\" onclick"},
	}})
	c := v.Checks[0]
	if c.Msg != "doctor.pipe.free" || c.FixMsg != "" {
		t.Errorf("keys: %q %q", c.Msg, c.FixMsg)
	}
	if len(c.Args) > 8 {
		t.Errorf("%d args", len(c.Args))
	}
	if _, ok := c.Args["Bad-Name"]; ok {
		t.Error("an argument with a bad name went through")
	}
	if p, ok := c.Args["pipe"]; ok && strings.ContainsRune(p, bidiOverride) {
		t.Errorf("pipe not cleaned: %q", p)
	}
	if v.Checks[1].Msg != "" {
		t.Errorf("a malformed key went through: %q", v.Checks[1].Msg)
	}
}
