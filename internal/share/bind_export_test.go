package share

import (
	"errors"
	"testing"
)

func TestParseBind(t *testing.T) {
	for spec, want := range map[string][3]string{
		`pg:/data`:                 {"pg", "/data", ""},
		`pg:/data:ro`:              {"pg", "/data", "ro"},
		`/etc/x:/x:ro,z`:           {"/etc/x", "/x", "ro,z"},
		`C:\src:/src`:              {`C:\src`, "/src", ""},
		`C:/src:/src:ro`:           {`C:/src`, "/src", "ro"},
		`\\?\C:\src:/src`:          {`\\?\C:\src`, "/src", ""},
		`\\server\share\x:/x:ro`:   {`\\server\share\x`, "/x", "ro"},
		`\\wsl$\Ubuntu\home:/h`:    {`\\wsl$\Ubuntu\home`, "/h", ""},
		`\\.\PhysicalDrive0:/d`:    {`\\.\PhysicalDrive0`, "/d", ""},
		`//var/run/docker.sock:/s`: {"//var/run/docker.sock", "/s", ""},
	} {
		src, target, opts, err := ParseBind(spec)
		if err != nil || [3]string{src, target, opts} != want {
			t.Errorf("%s: %q %q %q %v", spec, src, target, opts, err)
		}
	}
	for _, spec := range []string{"", "nope", "pg:", "pg:data", ":/x", `C:\src`, `C:\src:src`, `\\server\share`, "a:/b:c:d"} {
		if src, target, opts, err := ParseBind(spec); err == nil {
			t.Errorf("%q accepted: %q %q %q", spec, src, target, opts)
		}
	}
}

// The import asks the same check the proxy applies (REQ-FS-006).
func TestCheckBindSource(t *testing.T) {
	for _, src := range []string{`\\server\share`, `\\wsl$\Ubuntu`, `\\wsl.localhost\Ubuntu`, `\\.\PhysicalDrive0`, `\\?\UNC\s\x`} {
		var ue *UnsharableError
		if err := CheckBindSource(src); !errors.As(err, &ue) {
			t.Errorf("%s: %v", src, err)
		}
	}
	for _, src := range []string{"/etc", "pg", "//var/run/docker.sock"} {
		if err := CheckBindSource(src); err != nil {
			t.Errorf("%s: %v", src, err)
		}
	}
}
