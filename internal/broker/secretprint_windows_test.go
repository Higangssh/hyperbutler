//go:build windows

package broker

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Higangssh/hyperbutler/internal/vmsecret"
)

// hcsVM keeps its secret in an unexported field, which fmt would print raw.
func TestHCSVMNeverPrintsSecret(t *testing.T) {
	v := hcsVM{name: "hyperbutler-broker-x", secret: vmsecret.Secret{0xab, 0xcd, 0xef}}
	for _, out := range []string{fmt.Sprintf("%v %+v %#v %x %d", v, v, v, v, v), fmt.Sprintf("%+v", &v)} {
		if strings.Contains(out, "abcdef") || strings.Contains(out, "171 205") || strings.Contains(out, "0xab") {
			t.Fatalf("secret printed: %s", out)
		}
	}
}
