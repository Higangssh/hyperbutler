//go:build !windows

package engine

import (
	"os"
	"testing"
)

// checkUserOnly fails unless path is 0600.
func checkUserOnly(t *testing.T, path string) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("%s: mode %v", path, fi.Mode())
	}
}
