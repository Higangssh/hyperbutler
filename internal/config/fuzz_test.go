package config

import (
	"encoding/json"
	"testing"

	"github.com/Higangssh/hyperbutler/internal/engine"
)

// FuzzSet: whatever value is set on whatever key, Set never panics, never changes its
// input, and either refuses or returns settings that validate, survive a save and load,
// and read back the same through Get.
func FuzzSet(f *testing.F) {
	for i, k := range Keys {
		for _, v := range []string{"", "0", "1", "4GiB", "4096", "true", "off", `\\.\pipe\x`, `C:\data`, "64GiB", "\x00", "-1", "99999999999999999999", "all", "80,443", "8000-8100, 22", "1-65535"} {
			f.Add(uint8(i), v, uint8(0))
		}
		f.Add(uint8(i), k.def(Env{Home: `C:\h`, HostCPUs: 4}), uint8(1))
	}
	f.Fuzz(func(t *testing.T, ki uint8, v string, flags uint8) {
		e := Env{Home: `C:\h`, HostCPUs: int(flags>>2) + 1, DiskExists: func(string) bool { return flags&1 != 0 }}
		k := Keys[int(ki)%len(Keys)]
		var base *engine.Settings
		if flags&2 != 0 {
			base = NewSettings(e)
			base.MemoryMB = 2048
		}
		var before engine.Settings
		if base != nil {
			before = *base
		}
		n, err := k.Set(base, v, e)
		if base != nil && *base != before {
			t.Fatal("Set changed its input")
		}
		if err != nil {
			if n != nil {
				t.Fatal("settings with an error")
			}
			return
		}
		if err := n.Validate(); err != nil {
			t.Fatalf("Set(%s, %q) gave invalid settings: %v", k.Name, v, err)
		}
		b, err := json.Marshal(n)
		if err != nil {
			t.Fatal(err)
		}
		back, err := engine.ParseSettings(b)
		if err != nil || !Equal(back, n) {
			t.Fatalf("does not round-trip through settings.json: %v", err)
		}
		// The value reads back, and setting it again changes nothing.
		it := k.Get(n, e)
		again, err := k.Set(n, it.Value, e)
		if err != nil || !Equal(again, n) {
			t.Fatalf("%s: value %q read back as %q does not set again: %v", k.Name, v, it.Value, err)
		}
	})
}
