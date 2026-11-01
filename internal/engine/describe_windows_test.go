//go:build windows

package engine

import (
	"strings"
	"testing"

	"github.com/Higangssh/hyperbutler/internal/transport"
)

// S5.1 (REQ-UX-020, proposed): the pipe-in-use explanation names the pipe this
// installation was configured with, says who may hold it, and gives the fix.
func TestDescribePipeInUse(t *testing.T) {
	const other = `\\.\pipe\docker_engine_hbtest`
	d := Describe(CodePipeInUse, other)
	if !strings.Contains(d, "Docker API pipe "+other+".") {
		t.Errorf("does not name the configured pipe:\n%s", d)
	}
	if strings.Contains(d, transport.DefaultPipeName) {
		t.Errorf("names the default pipe although another is configured:\n%s", d)
	}
	for _, want := range []string{
		"another Windows user's hyperbutler",
		"another hyperbutler installation of yours",
		`hyperbutler config set pipe \\.\pipe\docker_engine_<name>`,
	} {
		if !strings.Contains(d, want) {
			t.Errorf("missing %q:\n%s", want, d)
		}
	}
	if d := Describe(CodePipeInUse, ""); !strings.Contains(d, "Docker API pipe "+transport.DefaultPipeName+".") {
		t.Errorf("no pipe given: does not name the default:\n%s", d)
	}
	if Describe("no-such-code", other) != "" {
		t.Error("an unknown code is explained")
	}
}
