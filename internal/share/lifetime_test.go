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
	 {"Id":"c","Mounts":[{"Type":"bind","Source":"/mnt/hyperbutlerX/b-0011223344556677"},
	                     {"Type":"bind","Source":"/mnt/hyperbutler/b-00112233445566"},
	                     {"Type":"bind","Source":"/mnt/hyperbutler/b-001122334455667G"},
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

// FuzzSharesInUse: every name returned has the form of a registry share name (a name
// with invalid UTF-8 became U+FFFD in json.Unmarshal and was returned before).
func FuzzSharesInUse(f *testing.F) {
	f.Add([]byte(`[{"Mounts":[{"Type":"bind","Source":"/mnt/hyperbutler/b-0123456789abcdef/x"}]}]`))
	f.Add([]byte(`[{"Mounts":[{"Type":"bind","Source":"/mnt/hyperbutler//"}]},null,{}]`))
	f.Fuzz(func(t *testing.T, b []byte) {
		used, err := SharesInUse(b, "/mnt/hyperbutler")
		if err != nil {
			return
		}
		for n := range used {
			if !isShareName(n) {
				t.Fatalf("name %q", n)
			}
		}
	})
}
