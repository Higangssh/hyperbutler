package share

import (
	"strings"
	"testing"
)

func TestSharesInUse(t *testing.T) {
	list := `[
	 {"Id":"a","Mounts":[{"Type":"bind","Source":"/mnt/hyperbutler/b-0011223344556677","Destination":"/src"},
	                     {"Type":"volume","Name":"v","Source":"/var/lib/docker/volumes/v/_data"}]},
	 {"Id":"b","Mounts":[{"Type":"bind","Source":"/mnt/hyperbutler/b-8899aabbccddeeff/config.json"}]},
	 {"Id":"c","Mounts":[{"Type":"bind","Source":"/mnt/hyperbutlerX/b-1"},
	                     {"Type":"bind","Source":"/mnt/hyperbutler/.."},
	                     {"Type":"volume","Source":"/mnt/hyperbutler/b-volume-typed"},
	                     {"Type":"bind","Source":"/mnt/hyperbutler/"}]},
	 {"Id":"d"}
	]`
	used, err := SharesInUse([]byte(list), "/mnt/hyperbutler")
	if err != nil {
		t.Fatal(err)
	}
	if len(used) != 2 || !used["b-0011223344556677"] || !used["b-8899aabbccddeeff"] {
		t.Fatalf("used = %v", used)
	}
	if _, err := SharesInUse([]byte(`{"not":"a list"}`), "/mnt/hyperbutler"); err == nil {
		t.Fatal("accepted a non-list")
	}
	if _, err := SharesInUse([]byte("["+strings.Repeat(" ", MaxContainerList)+"]"), "/mnt/hyperbutler"); err == nil {
		t.Fatal("accepted an oversized list")
	}
}

// FuzzSharesInUse: every name returned is a valid share name taken from a bind source
// below the mount point.
func FuzzSharesInUse(f *testing.F) {
	f.Add([]byte(`[{"Mounts":[{"Type":"bind","Source":"/mnt/hyperbutler/b-1/x"}]}]`))
	f.Add([]byte(`[{"Mounts":[{"Type":"bind","Source":"/mnt/hyperbutler//"}]},null,{}]`))
	f.Fuzz(func(t *testing.T, b []byte) {
		used, err := SharesInUse(b, "/mnt/hyperbutler")
		if err != nil {
			return
		}
		for n := range used {
			if checkName(n) != nil || !strings.Contains(string(b), n) && !strings.ContainsRune(string(b), '\\') {
				t.Fatalf("name %q", n)
			}
		}
	})
}
