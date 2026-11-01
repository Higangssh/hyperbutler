package dockerctx

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// The docker CLI's plugin search (docker/cli v29, cli-plugins/manager): the directories
// in config.json's "cliPluginsExtraDirs", then <config dir>\cli-plugins, then
// %ProgramFiles%\Docker\cli-plugins on Windows. The first directory that holds a
// plugin wins; there is no search next to docker.exe itself.
const pluginDirsKey = "cliPluginsExtraDirs"

// ExtraPluginDirs returns config.json's "cliPluginsExtraDirs".
func ExtraPluginDirs(config []byte) ([]string, error) {
	m, err := parseConfig(config)
	if err != nil {
		return nil, err
	}
	raw, ok := m[pluginDirsKey]
	if !ok {
		return nil, nil
	}
	var dirs []string
	if err := json.Unmarshal(raw, &dirs); err != nil {
		return nil, fmt.Errorf("dockerctx: config.json %s: %w", pluginDirsKey, err)
	}
	return dirs, nil
}

// SetExtraPluginDir returns config with dir added to (want) or removed from
// "cliPluginsExtraDirs". Other entries and every other key keep their values; the key
// is dropped when removing dir leaves it empty. Paths compare as Windows does
// (cleaned, case-insensitive).
func SetExtraPluginDir(config []byte, dir string, want bool) ([]byte, error) {
	if dir == "" || !utf8.ValidString(dir) || !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("dockerctx: plugin directory %q must be an absolute UTF-8 path", dir)
	}
	dirs, err := ExtraPluginDirs(config)
	if err != nil {
		return nil, err
	}
	var out []string
	found := false
	for _, d := range dirs {
		if samePath(d, dir) {
			found = true
			if !want {
				continue
			}
		}
		out = append(out, d)
	}
	if found == want {
		return config, nil
	}
	if want {
		out = append(out, dir)
	}
	m, err := parseConfig(config)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		delete(m, pluginDirsKey)
	} else {
		v, err := json.Marshal(out)
		if err != nil {
			return nil, err
		}
		m[pluginDirsKey] = v
	}
	return json.MarshalIndent(m, "", "\t")
}

func samePath(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

// PluginSync is the result of Store.SyncPluginDir.
type PluginSync struct {
	// Active is the plugin file the docker CLI runs after the sync, or "" if none.
	Active string
	// Listed says whether the directory is in cliPluginsExtraDirs now; Changed, whether
	// config.json was rewritten for it.
	Listed, Changed bool
}

// SyncPluginDir makes the docker CLI find plugin (a file name such as
// "docker-compose.exe") in dir only as a fallback: dir is listed in
// cliPluginsExtraDirs while no other directory of the CLI's search holds that plugin,
// and is taken out again once one does (a later Docker Desktop install, say). An
// extra directory comes first in the search, so listing it next to another copy would
// shadow that copy. systemDirs are the CLI's fixed directories after <config
// dir>\cli-plugins (%ProgramFiles%\Docker\cli-plugins). Nothing is copied anywhere.
func (s Store) SyncPluginDir(dir, plugin string, systemDirs []string) (PluginSync, error) {
	var r PluginSync
	if plugin == "" || strings.ContainsAny(plugin, `\/:`) {
		return r, fmt.Errorf("dockerctx: bad plugin name %q", plugin)
	}
	b, err := readBounded(s.configPath())
	if errors.Is(err, os.ErrNotExist) {
		b, err = nil, nil
	}
	if err != nil {
		return r, err
	}
	extras, err := ExtraPluginDirs(b)
	if err != nil {
		return r, err
	}
	var search []string
	for _, d := range extras {
		if !samePath(d, dir) {
			search = append(search, d)
		}
	}
	search = append(search, filepath.Join(s.Dir, "cli-plugins"))
	search = append(search, systemDirs...)
	for _, d := range search {
		if d == "" || !filepath.IsAbs(d) {
			continue // the CLI resolves relative entries against its own working directory
		}
		if p := filepath.Join(d, plugin); isFile(p) {
			r.Active = p
			break
		}
	}
	mine := filepath.Join(dir, plugin)
	want := r.Active == "" && isFile(mine)
	out, err := SetExtraPluginDir(b, dir, want)
	if err != nil {
		return r, err
	}
	r.Listed = want
	if want {
		r.Active = mine
	}
	if bytes.Equal(out, b) {
		return r, nil
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return r, err
	}
	if err := writeAtomic(s.configPath(), out); err != nil {
		return r, err
	}
	r.Changed = true
	return r, nil
}

func isFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}
