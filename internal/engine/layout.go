package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// HomeEnv overrides the state directory (tests, several side-by-side installs).
const HomeEnv = "HYPERBUTLER_HOME"

// DefaultHome returns the state directory: %HYPERBUTLER_HOME% if set, else
// %LOCALAPPDATA%\hyperbutler.
func DefaultHome() (string, error) {
	if h := os.Getenv(HomeEnv); h != "" {
		return filepath.Abs(h)
	}
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		var err error
		if base, err = os.UserCacheDir(); err != nil {
			return "", errors.New("engine: neither LOCALAPPDATA nor a user cache directory is set")
		}
	}
	return filepath.Join(base, "hyperbutler"), nil
}

// Layout names the files of one hyperbutler installation.
//
// Home holds the small files: settings, state, locks and logs. Data holds the data disk.
// Data is Home unless the user chose another location on the first start
// (REQ-DISK-007), and Settings records it.
type Layout struct {
	Home string
	Data string
}

func (l Layout) SettingsPath() string { return filepath.Join(l.Home, "settings.json") }
func (l Layout) StatePath() string    { return filepath.Join(l.Home, "state.json") }

// LockPath is the engine's single-instance lock, held by the engine process for its
// whole life. The OS drops it when the process dies, so it cannot go stale.
func (l Layout) LockPath() string { return filepath.Join(l.Home, "engine.lock") }

// OpsLockPath serializes the CLI commands that change the installation (start, stop,
// delete).
func (l Layout) OpsLockPath() string   { return filepath.Join(l.Home, "ops.lock") }
func (l Layout) LogDir() string        { return filepath.Join(l.Home, "logs") }
func (l Layout) EngineLogPath() string { return filepath.Join(l.LogDir(), "engine.log") }
func (l Layout) ConsoleLogPath() string {
	return filepath.Join(l.LogDir(), "console.log")
}

// DaemonConfigPath is the user's daemon.json for the guest's dockerd (REQ-UX-007).
// hyperbutler reads it and never writes it.
func (l Layout) DaemonConfigPath() string { return filepath.Join(l.Home, "daemon.json") }

// DaemonAppliedPath is the engine's copy of the last daemon.json the guest accepted:
// what stays in effect while the user's file is invalid.
func (l Layout) DaemonAppliedPath() string { return filepath.Join(l.Home, "daemon.applied.json") }

func (l Layout) DiskPath() string     { return filepath.Join(l.Data, "data.vhdx") }
func (l Layout) DiskMetaPath() string { return filepath.Join(l.Data, "disk.json") }

// Tag is a short hash of the home directory. VM names start with "hyperbutler-<tag>-", so
// an engine only ever cleans up compute systems of its own installation.
func (l Layout) Tag() string {
	sum := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(l.Home))))
	return hex.EncodeToString(sum[:4])
}

// VMOwner is the HCS Owner of every hyperbutler VM.
const VMOwner = "hyperbutler"

// VMPrefix is the name prefix of this installation's compute systems.
func (l Layout) VMPrefix() string { return "hyperbutler-" + l.Tag() + "-" }

// SamePath reports whether a and b name the same directory, comparing cleaned,
// absolute paths without regard to case (NTFS is case-insensitive by default).
func SamePath(a, b string) bool {
	aa, err1 := filepath.Abs(a)
	bb, err2 := filepath.Abs(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return strings.EqualFold(filepath.Clean(aa), filepath.Clean(bb))
}
