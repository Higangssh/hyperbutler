//go:build windows

package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Higangssh/hyperbutler/guest/image"
	"github.com/Higangssh/hyperbutler/internal/engine"
	"github.com/Higangssh/hyperbutler/internal/engine/dockerctx"
	"github.com/Higangssh/hyperbutler/internal/transport"
	"github.com/Higangssh/hyperbutler/internal/vm"
)

// Kinds of UninstallItem.
const (
	uninstallEngine  = "engine"         // the running engine process, stopped first
	uninstallVM      = "vm"             // an HCS compute system of this installation
	uninstallACL     = "acl"            // a VM account's access entry on a disk file
	uninstallContext = "docker-context" // the "hyperbutler" docker context
	uninstallPlugins = "docker-plugins" // the bundled cli-plugins entry in the docker CLI's config.json
	uninstallFile    = "file"
	uninstallDir     = "dir" // removed only when empty
)

// uninstallNotes say what uninstall never touches, in every output.
var uninstallNotes = []string{
	"hyperbutler creates no scheduled tasks, services, registry keys, firewall rules or HNS networks, so there are none to remove.",
	"Only files hyperbutler names are removed, never a directory's other contents; a directory goes only when it is empty afterwards.",
	"hyperbutler.exe, its folder and its PATH entry are not removed: delete the folder and the PATH entry yourself.",
}

// How long uninstall waits for a file to be released. Defender scans a freed 64 GiB data
// disk for well over the few seconds `hyperbutler delete` waits. (Variables for the tests.)
var (
	diskRemoveTimeout = 3 * time.Minute
	fileRemoveTimeout = 5 * time.Second
	// A log held by `hyperbutler logs -f` is not released by waiting: report the holder.
	logRemoveTimeout = time.Second
)

// planned is an item with what executing it needs.
type planned struct {
	UninstallItem
	vmID string // acl: revoke through HCS for this compute system...
	sid  string // ...or remove this account's entries from the DACL
	// last: settings.json, removed only when everything before it was.
	last bool
}

// uninstallPlan is what uninstall removes. Every entry is checked to exist, so a
// plan made after a complete uninstall is empty.
type uninstallPlan struct {
	items []planned
	kept  []UninstallItem
	notes []string
	// restore is the docker context to select instead of hyperbutler's ("" for docker's
	// default); restoreCtx says whether hyperbutler's is the selected one.
	restore    string
	restoreCtx bool
}

func (p *uninstallPlan) add(it planned) { p.items = append(p.items, it) }

func (p *uninstallPlan) keep(kind, path, detail string) {
	p.kept = append(p.kept, UninstallItem{Kind: kind, Path: path, Detail: detail})
}

func (p *uninstallPlan) public() []UninstallItem {
	out := []UninstallItem{}
	for _, it := range p.items {
		out = append(out, it.UninstallItem)
	}
	return out
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// dataDirs are the directories that may hold this installation's data disk: the one in
// settings.json, the one the last engine used (state.json), and the state directory. A
// half-finished uninstall that lost settings.json still finds the disk.
func (in *install) dataDirs(st *engine.State) []string {
	var dirs []string
	for _, d := range []string{in.l.Data, stDataDir(st), in.l.Home} {
		if d == "" || !filepath.IsAbs(d) {
			continue
		}
		dup := false
		for _, e := range dirs {
			dup = dup || engine.SamePath(d, e)
		}
		if !dup {
			dirs = append(dirs, d)
		}
	}
	return dirs
}

func stDataDir(st *engine.State) string {
	if st == nil {
		return ""
	}
	return st.DataDir
}

// planUninstall lists what this installation created and still exists; up says whether
// its engine runs (the caller asks, since a caller holding the engine lock knows it does
// not). It changes nothing.
func (in *install) planUninstall(keepData, up bool) *uninstallPlan {
	p := &uninstallPlan{}
	l := in.l
	st, _ := engine.LoadState(l)

	if up {
		what := "hyperbutler engine"
		if st != nil {
			what = fmt.Sprintf("hyperbutler engine, pid %d", st.PID)
		}
		p.add(planned{UninstallItem: UninstallItem{Kind: uninstallEngine, Path: what, Detail: "stopped first, as by hyperbutler stop; it revokes its VM's access to the disks as it stops"}})
	}

	// Compute systems named for this installation (normally none once the engine is gone).
	if list, err := vm.List(engine.VMOwner); err != nil {
		p.notes = append(p.notes, "Could not list Hyper-V compute systems: "+err.Error())
	} else {
		for _, s := range list {
			if strings.HasPrefix(s.ID, l.VMPrefix()) {
				p.add(planned{UninstallItem: UninstallItem{Kind: uninstallVM, Path: s.ID, Detail: "HCS compute system, " + s.State + "; terminated"}})
			}
		}
	}

	dataDirs := in.dataDirs(st)
	if !up {
		in.planACLs(p, st, keepData, dataDirs)
	}
	in.planContext(p)
	in.planPlugins(p)

	// Files, by name, in the order they are removed: the data disk first, settings.json
	// last (and only if everything before it went), so a failed uninstall still knows
	// where everything is and a dry run lists what is left.
	removed := map[string]bool{}
	addFile := func(f, detail string) {
		if !exists(f) || removed[strings.ToLower(f)] {
			return
		}
		removed[strings.ToLower(f)] = true
		it := planned{UninstallItem: UninstallItem{Kind: uninstallFile, Path: f, Detail: detail}}
		it.last = strings.EqualFold(f, l.SettingsPath()) || strings.HasPrefix(strings.ToLower(f), strings.ToLower(l.SettingsPath()+".tmp-"))
		p.add(it)
	}
	for _, d := range dataDirs {
		dl := engine.Layout{Home: l.Home, Data: d}
		files := append([]string{dl.DiskPath(), dl.DiskPath() + ".new", dl.DiskMetaPath()}, tmpSiblings(dl.DiskMetaPath())...)
		for _, f := range files {
			if !exists(f) {
				continue
			}
			if keepData {
				p.keep(uninstallFile, f, "--keep-data; `hyperbutler start"+dataHint(l.Home, d)+"` uses it again")
				continue
			}
			addFile(f, "data disk: all images, containers and volumes")
		}
	}
	for _, f := range append([]string{l.StatePath(), l.DaemonAppliedPath()}, append(tmpSiblings(l.StatePath()), tmpSiblings(l.DaemonAppliedPath())...)...) {
		addFile(f, "")
	}
	for _, f := range []string{l.EngineLogPath(), l.EngineLogPath() + ".1", l.ConsoleLogPath(), l.ConsoleLogPath() + ".1"} {
		addFile(f, "log")
	}
	if exists(l.DaemonConfigPath()) {
		if keepData {
			p.keep(uninstallFile, l.DaemonConfigPath(), "your daemon.json; kept with --keep-data")
		} else {
			addFile(l.DaemonConfigPath(), "your daemon.json (Docker Engine settings)")
		}
	}
	addFile(l.LockPath(), "")
	addFile(l.OpsLockPath(), "")
	for _, f := range append([]string{l.SettingsPath()}, tmpSiblings(l.SettingsPath())...) {
		addFile(f, "settings; removed last")
	}

	// Directories: logs, the data directories and the state directory, each only if
	// nothing else would be left in it. An empty leftover directory is listed too.
	dirs := []string{l.LogDir()}
	for _, d := range dataDirs {
		if !engine.SamePath(d, l.Home) {
			dirs = append(dirs, d)
		}
	}
	dirs = append(dirs, l.Home)
	for _, d := range dirs {
		fi, err := os.Lstat(d)
		if err != nil || !fi.IsDir() {
			continue
		}
		if why := protectedDir(d); why != "" {
			p.keep(uninstallDir, d, why+"; only hyperbutler's files in it are removed")
			continue
		}
		others := leftovers(d, removed)
		if len(others) == 0 {
			removed[strings.ToLower(d)] = true
			p.add(planned{UninstallItem: UninstallItem{Kind: uninstallDir, Path: d, Detail: "empty afterwards"}})
			continue
		}
		p.keep(uninstallDir, d, "not empty: "+strings.Join(others, ", ")+" (not created by hyperbutler, or kept)")
	}

	if exe, err := os.Executable(); err == nil {
		p.keep(uninstallFile, exe, "hyperbutler.exe and its folder are not removed")
	}
	p.notes = append(p.notes, uninstallNotes...)
	return p
}

// planACLs lists the VM access entries (HcsGrantVmAccess, NT VIRTUAL MACHINE\<id>) a
// crashed engine left on files that stay: the guest's system disk (in the image the
// last engine ran, and next to hyperbutler.exe), and the data disk with --keep-data. A
// deleted file takes its entries with it. The last engine's VM is revoked through HCS;
// entries of compute systems that no longer exist, of any earlier crash, are removed
// from the DACL. Entries of a compute system that exists are in use and stay.
func (in *install) planACLs(p *uninstallPlan, st *engine.State, keepData bool, dataDirs []string) {
	var files []string
	if keepData {
		for _, d := range dataDirs {
			files = append(files, engine.Layout{Home: in.l.Home, Data: d}.DiskPath())
		}
	}
	if st != nil {
		files = append(files, systemDisk(st.ImageDir))
	}
	if exe, err := os.Executable(); err == nil {
		files = append(files, systemDisk(filepath.Dir(exe)))
	}
	lastSID := ""
	if st != nil && st.VMID != "" && st.RuntimeID != "" {
		lastSID, _ = vmSID(st.RuntimeID)
	}
	live, liveErr := liveVMSIDs()
	seen := map[string]bool{}
	for _, f := range files {
		if f == "" || seen[strings.ToLower(f)] || !exists(f) {
			continue
		}
		seen[strings.ToLower(f)] = true
		sids, err := vmACEs(f)
		if err != nil {
			p.notes = append(p.notes, "Could not read the access list of "+f+": "+err.Error())
			continue
		}
		for _, sid := range sids {
			switch {
			case live[sid]:
				continue // a compute system that exists uses it
			case sid == lastSID:
				p.add(planned{UninstallItem: UninstallItem{Kind: uninstallACL, Path: f, Detail: "access of " + sid + " (VM " + st.VMID + ", whose engine did not shut down); revoked"}, vmID: st.VMID, sid: sid})
			case liveErr != nil:
				// Without the list of existing compute systems an entry may be in use.
			default:
				p.add(planned{UninstallItem: UninstallItem{Kind: uninstallACL, Path: f, Detail: "access of " + sid + " (a VM that no longer exists); removed"}, sid: sid})
			}
		}
	}
	if liveErr != nil {
		p.notes = append(p.notes, "Could not list compute systems, so VM access entries other than the last engine's are left: "+liveErr.Error())
	}
}

// planContext lists the docker context, if hyperbutler created it for this
// installation's pipe, and what is selected instead when it goes.
// planPlugins lists the bundled clients\cli-plugins entry that start adds to the docker
// CLI's cliPluginsExtraDirs, so that docker stops looking for compose in a folder about
// to be deleted. Only this executable's own entry is touched.
func (in *install) planPlugins(p *uninstallPlan) {
	d := clientsDir()
	if d == "" {
		return
	}
	store, err := dockerctx.DefaultStore()
	if err != nil {
		return
	}
	dir := filepath.Join(d, pluginsDirName)
	has, err := store.HasPluginDir(dir)
	switch {
	case err != nil:
		p.keep(uninstallPlugins, dir, "docker config.json unreadable, left alone: "+err.Error())
	case has:
		p.add(planned{UninstallItem: UninstallItem{Kind: uninstallPlugins, Path: dir, Detail: "in cliPluginsExtraDirs of " + filepath.Join(store.Dir, "config.json")}})
	}
}

func removePluginDir(dir string) error {
	store, err := dockerctx.DefaultStore()
	if err != nil {
		return err
	}
	return store.RemovePluginDir(dir)
}

func (in *install) planContext(p *uninstallPlan) {
	store, err := dockerctx.DefaultStore()
	if err != nil {
		return
	}
	c, err := store.Get(contextName)
	pipe := transport.DefaultPipeName
	if in.settings != nil {
		pipe = in.settings.PipeName()
	}
	switch {
	case errors.Is(err, os.ErrNotExist):
		return
	case err != nil:
		p.keep(uninstallContext, contextName, "unreadable, left alone: "+err.Error())
		return
	case !c.Managed:
		p.keep(uninstallContext, contextName, "not created by hyperbutler, left alone")
		return
	case !strings.EqualFold(c.Host, npipeHost(pipe)):
		p.keep(uninstallContext, contextName, "points at "+c.Host+", another hyperbutler installation's pipe; left alone")
		return
	}
	detail := "in " + store.Dir
	if cur, err := store.Current(); err == nil && cur == contextName {
		p.restoreCtx = true
		prev := ""
		if in.settings != nil && in.settings.PreviousContext != "" && in.settings.PreviousContext != "default" {
			if _, err := store.Get(in.settings.PreviousContext); err == nil {
				prev = in.settings.PreviousContext
			}
		}
		p.restore = prev
		if prev == "" {
			detail += "; selected, so docker goes back to the default context"
		} else {
			detail += fmt.Sprintf("; selected, so docker goes back to %q, selected before hyperbutler", prev)
		}
	}
	p.add(planned{UninstallItem: UninstallItem{Kind: uninstallContext, Path: contextName, Detail: detail}})
}

// selectContext selects hyperbutler's docker context (the function below) and records
// in settings.json the context it replaced, so uninstall can select that one again.
// Only the first switch is recorded: later ones replace the user's choice of
// hyperbutler.
func (in *install) selectContext(pipe string, stdout, stderr io.Writer) {
	before := ""
	store, err := dockerctx.DefaultStore()
	if err == nil {
		before, err = store.Current()
	}
	selectContext(pipe, stdout, stderr)
	if err != nil || before == contextName || in.settings == nil || in.settings.PreviousContext != "" {
		return
	}
	if after, err := store.Current(); err != nil || after != contextName {
		return
	}
	if before == "" {
		before = "default"
	}
	s := *in.settings
	s.PreviousContext = before
	if err := engine.SaveSettings(in.l, &s); err != nil {
		_, _ = fmt.Fprintln(stderr, "hyperbutler: could not record the previous docker context:", err)
		return
	}
	in.settings = &s
}

// dataHint says how to reuse a kept data disk outside the state directory.
func dataHint(home, data string) string {
	if engine.SamePath(data, home) {
		return ""
	}
	return " --data " + data
}

// tmpSiblings are leftovers of engine.WriteFileAtomic for path (path.tmp-<hex>).
func tmpSiblings(path string) []string {
	m, _ := filepath.Glob(path + ".tmp-*")
	return m
}

// leftovers names the entries of dir that the plan does not remove.
func leftovers(dir string, removed map[string]bool) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return []string{"(unreadable: " + err.Error() + ")"}
	}
	var out []string
	for _, e := range ents {
		if !removed[strings.ToLower(filepath.Join(dir, e.Name()))] {
			out = append(out, e.Name())
		}
	}
	const show = 5
	if len(out) > show {
		out = append(out[:show], fmt.Sprintf("and %d more", len(out)-show))
	}
	return out
}

// protectedDir explains why dir must never be removed, even when empty: a volume root, a
// Windows known folder, or the folder of hyperbutler.exe. data.dir and HYPERBUTLER_HOME
// can name any directory.
func protectedDir(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "not an absolute path"
	}
	abs = filepath.Clean(abs)
	if v := filepath.VolumeName(abs); v == "" || strings.EqualFold(abs, v+`\`) || strings.EqualFold(abs, v) {
		return "a volume root"
	}
	var known []string
	if h, err := os.UserHomeDir(); err == nil {
		known = append(known, h)
		for _, sub := range []string{"Desktop", "Documents", "Downloads", "Pictures", "Music", "Videos", "AppData", `AppData\Local`, `AppData\Roaming`, `AppData\LocalLow`, ".docker"} {
			known = append(known, filepath.Join(h, sub))
		}
	}
	for _, env := range []string{"LOCALAPPDATA", "APPDATA", "TEMP", "TMP", "USERPROFILE", "PUBLIC", "ProgramData", "ProgramFiles", "ProgramFiles(x86)", "SystemRoot", "SystemDrive", "OneDrive"} {
		if v := os.Getenv(env); v != "" {
			known = append(known, v)
		}
	}
	if exe, err := os.Executable(); err == nil {
		known = append(known, filepath.Dir(exe))
	}
	for _, k := range known {
		if engine.SamePath(abs, k) {
			return "a system or user folder (" + k + ")"
		}
	}
	return ""
}

// systemDisk is the guest system disk named by imageDir's image.json, or "".
func systemDisk(imageDir string) string {
	if imageDir == "" {
		return ""
	}
	b, err := engine.ReadFileBounded(filepath.Join(imageDir, image.ManifestName))
	if err != nil {
		return ""
	}
	m, err := image.ParseManifest(b)
	if err != nil {
		return ""
	}
	return filepath.Join(imageDir, m.System.Name)
}

func cmdUninstall(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := newFlagSet("uninstall", stderr)
	dryRun := fs.Bool("dry-run", false, "list what would be removed, and remove nothing")
	keepData := fs.Bool("keep-data", false, "keep the data disk (data.vhdx and disk.json: images, containers and volumes) and daemon.json")
	yes := fs.Bool("yes", false, "do not ask for confirmation (needed where input is not a Windows console: Git Bash/mintty, VS Code tasks, scripts)")
	asJSON := fs.Bool("json", false, fmt.Sprintf("print JSON (schema %d)", SchemaVersion))
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	in, err := openInstall()
	if err != nil {
		return err
	}
	result := func(p *uninstallPlan, dry bool) UninstallJSON {
		return UninstallJSON{Schema: SchemaVersion, Home: in.l.Home, DryRun: dry, KeepData: *keepData, Items: p.public(), Kept: p.kept, Notes: p.notes}
	}
	up, _ := in.running()
	p := in.planUninstall(*keepData, up)
	if *dryRun {
		return printUninstall(stdout, *asJSON, result(p, true))
	}
	if len(p.items) == 0 {
		return printUninstall(stdout, *asJSON, result(p, false))
	}
	if !*yes && *asJSON {
		return errors.New("uninstall: --json needs --yes (or --dry-run)")
	}
	if !*yes {
		_, _ = fmt.Fprintf(stdout, "Uninstalling hyperbutler from %s.\n", in.l.Home)
		printPlan(stdout, "This removes:", p.public())
		printPlan(stdout, "It keeps:", p.kept)
		_, _ = fmt.Fprint(stdout, "\nType \"yes\" to uninstall: ")
		if !isConsole(stdin) {
			_, _ = fmt.Fprintln(stdout)
			return errors.New("uninstall: input is not a Windows console (Git Bash, mintty and VS Code tasks are not); pass --yes to confirm, or --dry-run to list")
		}
		line, _ := bufio.NewReader(stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "yes" && a != "y" {
			_, _ = fmt.Fprintln(stdout, "Nothing was removed.")
			return nil
		}
	}

	ops, err := in.opsLock()
	if err != nil {
		return err
	}
	opsHeld := true
	defer func() {
		if opsHeld {
			_ = ops.Unlock()
		}
	}()
	if err := in.reload(); err != nil {
		return err
	}
	if up, err := in.running(); err != nil {
		return err
	} else if up {
		if _, _, err := in.stop(45*time.Second, stderr); err != nil && !errors.Is(err, errNotRunning) {
			return fmt.Errorf("stop before uninstall: %w", err)
		}
	} else {
		in.noteCrash(stderr) // revokes a dead engine's data disk access, ends its VM
	}
	// Under the engine lock, so no engine starts while the files go.
	lk, err := engine.Lock(in.l.LockPath(), 10*time.Second)
	if err != nil {
		return err
	}
	lkHeld := true
	defer func() {
		if lkHeld {
			_ = lk.Unlock()
		}
	}()

	// The plan again, now that the engine is gone, carried out in its order.
	p = in.planUninstall(*keepData, false)
	r := result(p, false)
	r.Items = []UninstallItem{}
	fail := func(it UninstallItem, err error) {
		it.Detail = err.Error()
		r.Failed = append(r.Failed, it)
	}
	terminated := false
	var deferred []planned
	for _, it := range p.items {
		var err error
		switch {
		case it.Kind == uninstallEngine:
			err = errors.New("the engine is still running")
		case it.Kind == uninstallVM:
			if !terminated {
				engine.CleanupOrphans(engine.HCSBackend{}, in.l.VMPrefix(), slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelError})))
				terminated = true
			}
			if list, lerr := vm.List(engine.VMOwner); lerr != nil {
				err = fmt.Errorf("cannot tell whether it was terminated: %w", lerr)
			} else {
				for _, s := range list {
					if s.ID == it.Path {
						err = fmt.Errorf("compute system %s could not be terminated (%s); reboot Windows and run hyperbutler uninstall again", s.ID, s.State)
					}
				}
			}
		case it.Kind == uninstallACL:
			err = revokeACE(it)
		case it.Kind == uninstallContext:
			err = removeContext(p.restoreCtx, p.restore)
		case it.Kind == uninstallPlugins:
			err = removePluginDir(it.Path)
		case it.Kind == uninstallFile && it.last:
			deferred = append(deferred, it)
			continue
		case it.Kind == uninstallFile && (strings.EqualFold(it.Path, in.l.LockPath()) || strings.EqualFold(it.Path, in.l.OpsLockPath())):
			deferred = append(deferred, it) // after the locks are released
			continue
		case it.Kind == uninstallFile:
			timeout := fileRemoveTimeout
			if strings.HasSuffix(strings.ToLower(it.Path), ".vhdx") || strings.HasSuffix(strings.ToLower(it.Path), ".vhdx.new") {
				timeout = diskRemoveTimeout
			} else if strings.HasPrefix(strings.ToLower(it.Path), strings.ToLower(in.l.LogDir()+`\`)) {
				timeout = logRemoveTimeout
			}
			err = removeVisible(it.Path, timeout, stderr)
		case it.Kind == uninstallDir:
			deferred = append(deferred, it)
			continue
		}
		if err != nil {
			fail(it.UninstallItem, err)
			continue
		}
		r.Items = append(r.Items, it.UninstallItem)
	}
	// The lock files, settings.json (only if everything else went: it is what tells a
	// later uninstall where the rest is), then the directories (os.Remove never recurses,
	// so only empty ones go).
	_ = lk.Unlock()
	_ = ops.Unlock()
	lkHeld, opsHeld = false, false
	ok := len(r.Failed) == 0
	for _, it := range deferred {
		if !ok && (it.last || it.Kind == uninstallDir) {
			r.Kept = append(r.Kept, UninstallItem{Kind: it.Kind, Path: it.Path, Detail: "kept because something else could not be removed; run hyperbutler uninstall again"})
			continue
		}
		if err := removeVisible(it.Path, fileRemoveTimeout, stderr); err != nil {
			fail(it.UninstallItem, err)
			continue
		}
		r.Items = append(r.Items, it.UninstallItem)
	}
	if err := printUninstall(stdout, *asJSON, r); err != nil {
		return err
	}
	if len(r.Failed) > 0 {
		return &shownError{err: fmt.Errorf("uninstall: %d item(s) could not be removed", len(r.Failed))}
	}
	return nil
}

// revokeACE removes a VM account's access entry: through HCS for a known compute system
// id (which also removes the worker capability entry it added), else from the DACL.
func revokeACE(it planned) error {
	if it.vmID != "" {
		if err := vm.RevokeVMAccess(it.vmID, it.Path); err != nil {
			return revokeSID(it.Path, it.sid)
		}
		if sids, err := vmACEs(it.Path); err == nil {
			for _, s := range sids {
				if s == it.sid {
					return revokeSID(it.Path, it.sid)
				}
			}
		}
		return nil
	}
	return revokeSID(it.Path, it.sid)
}

// removeContext removes the hyperbutler docker context; if it is selected, it first
// selects restore ("" for docker's default).
func removeContext(selected bool, restore string) error {
	store, err := dockerctx.DefaultStore()
	if err != nil {
		return err
	}
	if selected {
		if cur, err := store.Current(); err == nil && cur == contextName {
			if err := store.Use(restore); err != nil {
				return fmt.Errorf("select %q: %w", restore, err)
			}
		}
	}
	return store.Remove(contextName)
}

func printPlan(w io.Writer, title string, items []UninstallItem) {
	if len(items) == 0 {
		return
	}
	_, _ = fmt.Fprintln(w, title)
	for _, it := range items {
		line := fmt.Sprintf("  %-14s %s", it.Kind, it.Path)
		if it.Detail != "" {
			line += "  (" + it.Detail + ")"
		}
		_, _ = fmt.Fprintln(w, line)
	}
}

func printUninstall(w io.Writer, asJSON bool, r UninstallJSON) error {
	if asJSON {
		return writeJSON(w, r)
	}
	_, _ = fmt.Fprintf(w, "hyperbutler installation: %s\n", r.Home)
	switch {
	case len(r.Items) == 0 && len(r.Failed) == 0:
		_, _ = fmt.Fprintln(w, "Nothing to remove: no trace of this hyperbutler installation is left.")
	case r.DryRun:
		printPlan(w, "uninstall would remove:", r.Items)
	default:
		printPlan(w, "Removed:", r.Items)
		printPlan(w, "Could not remove:", r.Failed)
	}
	printPlan(w, "Kept:", r.Kept)
	for _, n := range r.Notes {
		_, _ = fmt.Fprintln(w, "Note:", n)
	}
	return nil
}
