//go:build windows

package desktopimport

import (
	"os"
	"path/filepath"

	"github.com/Higangssh/hyperbutler/internal/engine"
	"github.com/Higangssh/hyperbutler/internal/vm"
)

// installSpace reads the installation in home: the guest's last guestFs report (while
// the engine runs) and the free space of the host volume of its data directory.
func installSpace(home string) Space {
	s := Space{HostKeep: engine.GuardConfig.Margin}
	if home == "" {
		return s
	}
	l := engine.Layout{Home: home, Data: home}
	if st, err := engine.LoadState(l); err == nil && st.Phase.Live() && st.GuestFS != nil {
		s.GuestFree, s.GuestKnown = st.GuestFS.FreeBytes, true
	}
	if set, err := engine.LoadSettings(l); err == nil && set.DataDir != "" {
		s.HostDir = nearestDir(set.DataDir)
		if f, err := vm.FreeBytes(s.HostDir); err == nil {
			s.HostFree, s.HostKnown = f, true
		}
	}
	return s
}

// nearestDir is dir, or its nearest parent that exists.
func nearestDir(dir string) string {
	for {
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			return dir
		}
		p := filepath.Dir(dir)
		if p == dir {
			return dir
		}
		dir = p
	}
}
