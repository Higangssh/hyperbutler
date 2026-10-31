package engine

import (
	"strings"
	"testing"
)

func TestHashTableArgs(t *testing.T) {
	for _, mb := range []int{MinMemoryMB, 2048, 4096} {
		if a := HashTableArgs(mb); a != "" {
			t.Errorf("%d MiB: %q, want the kernel's own sizes", mb, a)
		}
	}
	for _, mb := range []int{4097, 12288, 65536, MaxMemoryMB} {
		a := HashTableArgs(mb)
		if !strings.HasPrefix(a, " ") || !strings.Contains(a, "dhash_entries=524288") || !strings.Contains(a, "ihash_entries=262144") {
			t.Errorf("%d MiB: %q", mb, a)
		}
	}
}
