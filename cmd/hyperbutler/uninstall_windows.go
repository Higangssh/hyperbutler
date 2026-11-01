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

	"github.com/Microsoft/go-winio/pkg/guid"

	"github.com/Higangssh/hyperbutler/guest/image"
	"github.com/Higangssh/hyperbutler/internal/engine"
	"github.com/Higangssh/hyperbutler/internal/engine/dockerctx"
	"github.com/Higangssh/hyperbutler/internal/transport"
	"github.com/Higangssh/hyperbutler/internal/vm"
)

// uninstallNotes say what uninstall never touches, in every output.
var uninstallNotes = []string{
	"hyperbutler creates no services, registry keys, firewall rules or HNS networks, so there are none to remove; its only scheduled task is the autostart task (config set autostart true), listed above when it exists.",
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

	if in.settingsErr != nil {
		p.notes = append(p.notes, fmt.Sprintf("settings.json is invalid (%v): it is removed with the rest, and the data disk is looked for where the last engine had it (state.json) and in %s.", in.settingsErr, l.Home))
	}
	dataDirs := in.dataDirs(st)
	in.planACLs(p, st, keepData, up, dataDirs)
	in.planContext(p, st)
	in.planPlugins(p)
	planAutostart(in, p)

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
	addFile(updateRecordPath(l), "the last update check")
	for _, f := range append([]string{l.StatePath(), l.DaemonAppliedPath(), l.CAExtraPath()}, append(append(tmpSiblings(l.StatePath()), tmpSiblings(l.DaemonAppliedPath())...), tmpSiblings(l.CAExtraPath())...)...) {
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
	// The record of the volumes imports verified is about the data disk's volumes: it
	// stays with the disk.
	if keepData && exists(l.ImportsCompletePath()) {
		p.keep(uninstallFile, l.ImportsCompletePath(), "the record of imported volumes on the kept data disk; kept with --keep-data")
	} else {
		for _, f := range append([]string{l.ImportsCompletePath()}, tmpSiblings(l.ImportsCompletePath())...) {
			addFile(f, "")
		}
	}
	addFile(l.LockPath(), "")
	addFile(l.OpsLockPath(), "")
	addFile(l.ImportLockPath(), "")
	for _, f := range append([]string{l.SettingsPath()}, tmpSiblings(l.SettingsPath())...) {
		addFile(f, "settings; removed last")
	}

	// Directories: logs, the data directories and the state directory, each only if
	// nothing else would be left in it. An empty leftover directory is listed too. A
	// data directory outside the state directory goes only if start created it.
	dirs := []string{l.LogDir()}
	created := ""
	if in.settings != nil {
		created = in.settings.CreatedDataDir
	}
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
		if !engine.SamePath(d, l.Home) && !engine.SamePath(d, l.LogDir()) && (created == "" || !engine.SamePath(d, created)) {
			p.keep(uninstallDir, d, "a data directory hyperbutler did not create (or that an older hyperbutler created without recording it); "+
				"only hyperbutler's files in it are removed: delete the directory yourself if you do not need it")
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
// last engine ran, and in the image folder next to hyperbutler.exe), and the data disk
// with --keep-data. A
// deleted file takes its entries with it.
//
// Only this installation's VMs' entries go: the last engine's VM (revoked through HCS)
// and the earlier ones in state.json's pastRuntimeIds, which the engine records before
// each grant. Any other VM account is left alone, even when no compute system has it:
// another installation sharing the system disk grants access before it creates its VM,
// and another Windows user's VMs are not in this user's list. Entries of a compute
// system that exists are in use and stay. While the engine runs (up), its own VM's
// entries are revoked as it stops, and the rest is planned again after that.
func (in *install) planACLs(p *uninstallPlan, st *engine.State, keepData, up bool, dataDirs []string) {
	var files []string
	if keepData {
		for _, d := range dataDirs {
			files = append(files, engine.Layout{Home: in.l.Home, Data: d}.DiskPath())
		}
	}
	if st != nil {
		files = append(files, systemDisk(st.ImageDir))
	}
	if d := defaultImageDir(); d != "" {
		files = append(files, systemDisk(d))
	}
	lastSID := ""
	owned := map[string]bool{}
	if st != nil {
		for _, rid := range st.PastRuntimeIDs {
			if sid, err := runtimeSID(rid); err == nil {
				owned[sid] = true
			}
		}
		if st.VMID != "" && st.RuntimeID != "" {
			if sid, err := runtimeSID(st.RuntimeID); err == nil {
				lastSID, owned[sid] = sid, true
			}
		}
	}
	later := ""
	if up {
		later = "; checked again after the engine stops"
	}
	live, liveErr := liveVMSIDs()
	seen := map[string]bool{}
	foreign := 0
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
			case up && sid == lastSID:
				continue // the running engine's VM: revoked as it stops
			case !owned[sid]:
				foreign++
				p.keep(uninstallACL, f, "access of "+sid+", a VM this installation did not record; left alone")
			case sid == lastSID:
				p.add(planned{UninstallItem: UninstallItem{Kind: uninstallACL, Path: f, Detail: "access of " + sid + " (VM " + st.VMID + ", whose engine did not shut down); revoked" + later}, vmID: st.VMID, sid: sid})
			default:
				p.add(planned{UninstallItem: UninstallItem{Kind: uninstallACL, Path: f, Detail: "access of " + sid + " (an earlier VM of this installation that no longer exists); removed" + later}, sid: sid})
			}
		}
	}
	if foreign > 0 {
		p.notes = append(p.notes, fmt.Sprintf("%d VM access entr%s on these files belong to VMs this installation did not record: another installation that shares the guest image "+
			"may be starting one, another Windows user may run one, or an older hyperbutler left it. They are left alone.", foreign, plural(foreign, "y", "ies")))
	}
	if liveErr != nil {
		p.notes = append(p.notes, "Could not list Hyper-V compute systems ("+liveErr.Error()+"); only entries of this installation's own VMs are removed.")
	}
	if up {
		p.notes = append(p.notes, "The engine is running: its VM's access entries are revoked as it stops, and the access entries are checked again after that.")
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// runtimeSID is the VM account of runtime id rid.
func runtimeSID(rid string) (string, error) {
	g, err := guid.FromString(rid)
	if err != nil {
		return "", err
	}
	return vm.AccountSID(g), nil
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

func (in *install) planContext(p *uninstallPlan, st *engine.State) {
	store, err := dockerctx.DefaultStore()
	if err != nil {
		return
	}
	c, err := store.Get(contextName)
	pipe := transport.DefaultPipeName
	switch {
	case in.settings != nil:
		pipe = in.settings.PipeName()
	case st != nil && st.Pipe != "":
		pipe = st.Pipe // settings.json is gone or invalid: the last engine's pipe
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
		recorded := ""
		if in.settings != nil {
			recorded = in.settings.PreviousContext
		}
		prev := ""
		if recorded != "" && recorded != "default" {
			if _, err := store.Get(recorded); err == nil {
				prev = recorded
			}
		}
		p.restore = prev
		switch {
		case prev != "":
			detail += fmt.Sprintf("; selected, so docker goes back to %q, the context hyperbutler start last replaced", prev)
		case recorded == "default":
			detail += "; selected, so docker goes back to the default context, which hyperbutler start last replaced"
		case recorded != "":
			detail += fmt.Sprintf("; selected, so docker goes back to the default context (%q, which hyperbutler start last replaced, no longer exists)", recorded)
		default:
			detail += "; selected, so docker goes back to the default context (no earlier context is recorded)"
		}
	}
	p.add(planned{UninstallItem: UninstallItem{Kind: uninstallContext, Path: contextName, Detail: detail}})
}

// selectContext selects hyperbutler's docker context (the function below) and records
// in settings.json the context it replaced, so uninstall can select that one again.
// Every switch from another context is recorded: if the user went back to Docker
// Desktop's context and start replaced it again, that is the one to restore. A start
// that finds hyperbutler's context already selected keeps the record.
func (in *install) selectContext(pipe string, stdout, stderr io.Writer) {
	before := ""
	store, err := dockerctx.DefaultStore()
	if err == nil {
		before, err = store.Current()
	}
	selectContext(pipe, stdout, stderr)
	if before == "" {
		before = "default"
	}
	if err != nil || before == contextName || in.settings == nil || in.settings.PreviousContext == before {
		return
	}
	if after, err := store.Current(); err != nil || after != contextName {
		return
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
	in, err := openInstallLenient()
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
		case it.Kind == uninstallTask:
			err = scheduler.Delete(it.Path)
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
			err = uninstallRemove(it.Path, timeout, stderr)
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
	// settings.json (only if everything else went: it is what tells a later uninstall
	// where the rest is), then the directories (os.Remove never recurses, so only empty
	// ones go). All of it under both locks, so that a start waiting for them finds either
	// the whole installation or none of it; only the lock files, and the state directory
	// that holds them, go after the locks are released.
	ok := len(r.Failed) == 0
	finish := func(items []planned) {
		for _, it := range items {
			if !ok && (it.last || it.Kind == uninstallDir) {
				r.Kept = append(r.Kept, UninstallItem{Kind: it.Kind, Path: it.Path, Detail: "kept because something else could not be removed; run hyperbutler uninstall again"})
				continue
			}
			if err := uninstallRemove(it.Path, fileRemoveTimeout, stderr); err != nil {
				fail(it.UninstallItem, err)
				continue
			}
			r.Items = append(r.Items, it.UninstallItem)
		}
	}
	var locked, unlocked []planned
	for _, it := range deferred {
		if in.holdsLock(it) {
			unlocked = append(unlocked, it)
		} else {
			locked = append(locked, it)
		}
	}
	finish(locked)
	_ = lk.Unlock()
	_ = ops.Unlock()
	lkHeld, opsHeld = false, false
	finish(unlocked)
	if err := printUninstall(stdout, *asJSON, r); err != nil {
		return err
	}
	if len(r.Failed) > 0 {
		return &shownError{err: fmt.Errorf("uninstall: %d item(s) could not be removed", len(r.Failed))}
	}
	return nil
}

// holdsLock reports whether it is a lock file, or a directory that holds one: those go
// only after the locks are released.
func (in *install) holdsLock(it planned) bool {
	for _, lf := range []string{in.l.LockPath(), in.l.OpsLockPath()} {
		if strings.EqualFold(it.Path, lf) || (it.Kind == uninstallDir && engine.SamePath(it.Path, filepath.Dir(lf))) {
			return true
		}
	}
	return false
}

// Seams for the tests: how uninstall removes a file or an empty directory, and revokes
// a VM's access through HCS.
var (
	uninstallRemove = removeVisible
	revokeVMAccess  = vm.RevokeVMAccess
)

// revokeACE removes a VM account's access entry: through HCS for a known compute system
// id (which also removes the worker capability entry it added), else from the DACL.
func revokeACE(it planned) error {
	if it.vmID != "" {
		if err := revokeVMAccess(it.vmID, it.Path); err != nil {
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
