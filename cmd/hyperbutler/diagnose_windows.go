//go:build windows

package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/Higangssh/hyperbutler/internal/engine"
	"github.com/Higangssh/hyperbutler/internal/engine/dockerctx"
	"github.com/Higangssh/hyperbutler/internal/redact"
	"github.com/Higangssh/hyperbutler/internal/vm"
)

// diagLogWindow is how much of the end of each log the bundle takes.
const diagLogWindow = 4 << 20

// diagJSONMax bounds the JSON files the user edits (daemon.json, the docker CLI's
// config.json), which the state files' 64 KiB bound would leave out: an oversized
// daemon.json is itself worth seeing.
const diagJSONMax = 1 << 20

// bundle is the content of a diagnose zip, in order.
type bundle struct {
	files   []bundleFile
	missing []DiagnoseMissing
	// originals describes each JSON file as it is on disk (files.json): redaction
	// re-indents it, which would hide a BOM or CRLF line ends.
	originals map[string]fileFacts
}

// fileFacts is one original file: its size and the encoding details redaction hides.
// There is no hash of the original: with a guess at a password, a hash of a file that
// holds one would confirm the guess offline. RedactedSHA256 is that of the redacted
// copy in the zip.
type fileFacts struct {
	Path           string `json:"path"`
	Bytes          int    `json:"bytes"`
	RedactedSHA256 string `json:"redactedSha256"`
	BOM            bool   `json:"utf8Bom"`
	CRLF           bool   `json:"crlf"`
}

type bundleFile struct {
	name string
	data []byte
}

func (b *bundle) add(name string, data []byte) {
	b.files = append(b.files, bundleFile{name: name, data: data})
}

func (b *bundle) skip(name, reason string) {
	b.missing = append(b.missing, DiagnoseMissing{Name: name, Reason: reason})
}

// addJSON adds v as indented JSON. Everything added goes through redact first.
func (b *bundle) addJSON(name string, v any) {
	j, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		b.skip(name, err.Error())
		return
	}
	b.add(name, redact.JSON(j))
}

// addFile adds a JSON file of the installation (read up to limit bytes), redacted, or
// records why it is not there.
func (b *bundle) addFile(name, path string, limit int) {
	data, err := engine.ReadFileLimit(path, limit)
	switch {
	case errors.Is(err, os.ErrNotExist):
		b.skip(name, "not present ("+path+")")
	case errors.Is(err, engine.ErrTooLarge):
		size := "more"
		if fi, serr := os.Stat(path); serr == nil {
			size = fmt.Sprint(fi.Size())
		}
		b.skip(name, fmt.Sprintf("omitted: %s is %s bytes, over the %s diagnose reads", path, size, engine.HumanSize(uint64(limit)))) //nolint:gosec // G115: a positive constant
	case err != nil:
		b.skip(name, err.Error())
	default:
		red := redact.JSON(data)
		b.add(name, red)
		sum := sha256.Sum256(red)
		if b.originals == nil {
			b.originals = map[string]fileFacts{}
		}
		b.originals[name] = fileFacts{Path: path, Bytes: len(data), RedactedSHA256: hex.EncodeToString(sum[:]),
			BOM: bytes.HasPrefix(data, []byte("\xef\xbb\xbf")), CRLF: bytes.Contains(data, []byte("\r\n"))}
	}
}

// addLog adds the last diagLogWindow bytes of a log, redacted.
func (b *bundle) addLog(name, path string) {
	data, err := readTail(path, diagLogWindow)
	switch {
	case errors.Is(err, os.ErrNotExist):
		b.skip(name, "not present ("+path+")")
	case err != nil:
		b.skip(name, err.Error())
	default:
		b.add(name, redact.Text(data))
	}
}

func readTail(path string, n int64) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // G304: a log file in the user's own data layout
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() // read-only
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if fi.Size() <= n {
		return io.ReadAll(io.LimitReader(f, n))
	}
	// One byte before the window: if it is a line end, the window starts a line.
	if _, err := f.Seek(fi.Size()-n-1, io.SeekStart); err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(f, n+1))
	if err != nil {
		return nil, err
	}
	// Drop the partial first line: half a line can be half a secret, which redaction
	// would no longer recognize ("…rd=hunter2" without "passwo").
	if i := bytes.IndexByte(b, '\n'); i >= 0 {
		return b[i+1:], nil
	}
	return nil, nil // one line longer than the window
}

func cmdDiagnose(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("diagnose", stderr)
	out := fs.String("out", "", "zip file to write (default: hyperbutler-diagnose-YYYYMMDD-HHMMSS.zip in the current directory)")
	asJSON := fs.Bool("json", false, fmt.Sprintf("print JSON (schema %d)", SchemaVersion))
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	now := time.Now()
	path := *out
	if path == "" {
		path = "hyperbutler-diagnose-" + now.Format("20060102-150405") + ".zip"
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	b := collect(now)
	aclKept, err := writeBundle(path, now, b)
	if err != nil {
		return fmt.Errorf("diagnose: %w", err)
	}
	r := DiagnoseJSON{Schema: SchemaVersion, File: path, Files: []string{}, Missing: b.missing}
	var size uint64
	for _, f := range b.files {
		r.Files = append(r.Files, f.name)
		size += uint64(len(f.data))
	}
	if !aclKept {
		// On stderr, so --json output stays one document.
		_, _ = fmt.Fprintf(stderr, "hyperbutler: note: the file system of %s keeps no access lists (FAT32 or exFAT), so the zip is not private: anyone who can open the drive can read it.\n", filepath.VolumeName(path))
	}
	if *asJSON {
		return writeJSON(stdout, r)
	}
	_, _ = fmt.Fprintf(stdout, "Wrote %s (%d files, %s before compression).\n", path, len(r.Files), engine.HumanSize(size))
	if len(b.missing) > 0 {
		_, _ = fmt.Fprintf(stdout, "Not included (%d): see summary.txt in the zip.\n", len(b.missing))
	}
	_, _ = fmt.Fprintln(stdout, "Nothing was uploaded. Passwords, tokens, registry auth and proxy credentials are redacted;\n"+
		"paths (with your user name) are not. Review the zip before you attach it to an issue.")
	return nil
}

// collect gathers what exists; nothing in it needs a running engine. It changes no file
// of the installation.
func collect(now time.Time) *bundle {
	b := &bundle{}
	v := versionInfo()
	b.addJSON("version.json", v)

	home, herr := engine.DefaultHome()
	l := engine.Layout{Home: home, Data: home}
	var settingsErr error
	if herr == nil {
		if s, err := engine.LoadSettings(l); err == nil {
			l.Data = s.DataDir
		} else if !errors.Is(err, os.ErrNotExist) {
			settingsErr = err
		}
	}

	// status --json, exactly as the command prints it.
	if herr == nil && settingsErr == nil {
		var sb bytes.Buffer
		if _, err := cmdStatus([]string{"--json"}, &sb, io.Discard); err == nil {
			b.add("status.json", redact.JSON(sb.Bytes()))
		} else {
			b.skip("status.json", err.Error())
		}
	} else {
		b.skip("status.json", "status cannot run: "+errors.Join(herr, settingsErr).Error())
	}

	var st *engine.State
	if herr == nil {
		b.addFile("settings.json", l.SettingsPath(), engine.MaxStateFile)
		b.addFile("daemon.json", l.DaemonConfigPath(), diagJSONMax)
		b.addFile("daemon.applied.json", l.DaemonAppliedPath(), diagJSONMax)
		b.addFile("state.json", l.StatePath(), engine.MaxStateFile)
		b.addFile("disk.json", l.DiskMetaPath(), engine.MaxStateFile)
		for _, lf := range []struct{ name, path string }{
			{"logs/engine.log", l.EngineLogPath()}, {"logs/engine.log.1", l.EngineLogPath() + ".1"},
			{"logs/console.log", l.ConsoleLogPath()}, {"logs/console.log.1", l.ConsoleLogPath() + ".1"},
		} {
			b.addLog(lf.name, lf.path)
		}
		if s, err := engine.LoadState(l); err == nil {
			st = s
		}
	}
	if st != nil && st.Phase == engine.PhaseFailed {
		b.addJSON("last-start-failure.json", map[string]any{
			"phase": st.Phase, "error": st.Error, "errorCode": st.ErrorCode,
			"explanation": engine.Describe(st.ErrorCode), "updated": st.Updated,
		})
	} else {
		b.skip("last-start-failure.json", "the last run did not fail")
	}

	b.addJSON("host.json", hostInfo(l, herr == nil))
	b.addJSON("docker-contexts.json", dockerContexts())
	// The docker CLI's config.json, through redact.JSON like every JSON file: it holds
	// registry credentials ("auth", "identitytoken"), which never leave.
	if store, err := dockerctx.DefaultStore(); err == nil {
		b.addFile("docker-config.json", filepath.Join(store.Dir, "config.json"), diagJSONMax)
	}
	if len(b.originals) > 0 {
		b.addJSON("files.json", b.originals)
	}

	var sum strings.Builder
	_, _ = fmt.Fprintf(&sum, "hyperbutler diagnose, %s\n", now.Format(time.RFC3339))
	_, _ = fmt.Fprintf(&sum, "hyperbutler %s (%s, %s/%s)\n", v.Version, v.GoVersion, v.OS, v.Arch)
	if herr != nil {
		_, _ = fmt.Fprintf(&sum, "state directory: unknown (%v)\n", herr)
	} else {
		_, _ = fmt.Fprintf(&sum, "state directory: %s\ndata directory:  %s\n", l.Home, l.Data)
	}
	if settingsErr != nil {
		_, _ = fmt.Fprintf(&sum, "settings.json is invalid: %v\n", settingsErr)
	}
	if st != nil {
		_, _ = fmt.Fprintf(&sum, "last engine: %s (pid %d, started %s)\n", st.Phase, st.PID, st.Started.Format(time.RFC3339))
		if st.Error != "" {
			_, _ = fmt.Fprintf(&sum, "last error: %s %s\n", st.ErrorCode, st.Error)
		}
	}
	_, _ = fmt.Fprintln(&sum, "\nSecrets (passwords, tokens, registry auth, proxy credentials, Authorization headers)")
	_, _ = fmt.Fprintln(&sum, "are replaced by "+redact.Mark+". Paths are kept. Nothing was uploaded.")
	if len(b.missing) > 0 {
		_, _ = fmt.Fprintln(&sum, "\nNot included:")
		for _, m := range b.missing {
			_, _ = fmt.Fprintf(&sum, "  %s: %s\n", m.Name, m.Reason)
		}
	}
	b.files = append([]bundleFile{{name: "summary.txt", data: redact.Text([]byte(sum.String()))}}, b.files...)
	return b
}

// hostInfo is host.json: the Windows build and edition, the virtualization features,
// and free space.
func hostInfo(l engine.Layout, haveHome bool) map[string]any {
	h := map[string]any{"logicalCPUs": runtime.NumCPU()}
	ver := map[string]any{}
	if v := windows.RtlGetVersion(); v != nil {
		ver["major"], ver["minor"], ver["build"] = v.MajorVersion, v.MinorVersion, v.BuildNumber
		// ProductName still says "Windows 10" on Windows 11; the build tells them apart.
		ver["windows11"] = v.MajorVersion == 10 && v.BuildNumber >= 22000
	}
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE); err == nil {
		for _, name := range []string{"ProductName", "EditionID", "DisplayVersion", "CurrentBuild", "InstallationType"} {
			if s, _, err := k.GetStringValue(name); err == nil {
				ver[name] = s
			}
		}
		if n, _, err := k.GetIntegerValue("UBR"); err == nil {
			ver["UBR"] = n
		}
		_ = k.Close()
	}
	h["windows"] = ver
	if member, err := inHyperVAdmins(); err == nil {
		h["hyperVAdministrators"] = member
	}
	h["elevated"] = windows.GetCurrentProcessToken().IsElevated()
	h["virtualization"] = virtualization()
	free := map[string]any{}
	if haveHome {
		for _, d := range []string{l.Home, l.Data} {
			if n, err := vm.FreeBytes(nearestDir(d)); err == nil {
				free[d] = n
			}
		}
	}
	h["freeBytes"] = free
	return h
}

// nearestDir is dir or its nearest existing parent (free space of a directory that does
// not exist yet is that of its volume).
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

// virtualization reports what HCS needs, without a shell, DISM or elevation: whether
// computecore.dll (Virtual Machine Platform) loads, the HCS and hypervisor services and
// the hypervisor launch type in the registry, the CPU features Windows reports, and
// whether HCS enumerates compute systems.
func virtualization() map[string]any {
	v := map[string]any{}
	if err := windows.NewLazySystemDLL("computecore.dll").Load(); err != nil {
		v["computecore"] = "not loadable: " + err.Error()
	} else {
		v["computecore"] = "loadable"
	}
	services := map[string]any{}
	for _, name := range []string{"vmcompute", "hvservice", "HvHost", "vmms"} {
		k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Services\`+name, registry.QUERY_VALUE)
		if err != nil {
			services[name] = "absent"
			continue
		}
		if n, _, err := k.GetIntegerValue("Start"); err == nil {
			services[name] = map[string]any{"start": n} // 2 automatic, 3 on demand, 4 disabled
		} else {
			services[name] = "present"
		}
		_ = k.Close()
	}
	v["services"] = services
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\Hypervisor`, registry.QUERY_VALUE); err == nil {
		if n, _, err := k.GetIntegerValue("HypervisorLaunchType"); err == nil {
			v["hypervisorLaunchType"] = n
		}
		_ = k.Close()
	}
	v["cpu"] = map[string]bool{
		"virtFirmwareEnabled":           windows.IsProcessorFeaturePresent(windows.PF_VIRT_FIRMWARE_ENABLED),
		"secondLevelAddressTranslation": windows.IsProcessorFeaturePresent(windows.PF_SECOND_LEVEL_ADDRESS_TRANSLATION),
	}
	if list, err := vm.List(engine.VMOwner); err != nil {
		v["computeSystems"] = "HcsEnumerateComputeSystems failed: " + err.Error()
	} else {
		ids := []string{}
		for _, s := range list {
			ids = append(ids, s.ID+" "+s.State)
		}
		v["computeSystems"] = ids
	}
	return v
}

// dockerContexts lists the docker CLI's contexts (name, endpoint, whether hyperbutler
// manages it; meta.json only, never the TLS material under contexts\tls) and the
// selected one.
func dockerContexts() map[string]any {
	r := map[string]any{}
	store, err := dockerctx.DefaultStore()
	if err != nil {
		r["error"] = err.Error()
		return r
	}
	r["dockerConfig"] = store.Dir
	if cur, err := store.Current(); err != nil {
		r["currentError"] = err.Error()
	} else {
		if cur == "" {
			cur = "default"
		}
		r["current"] = cur
	}
	for _, env := range []string{"DOCKER_HOST", "DOCKER_CONTEXT"} {
		if v := os.Getenv(env); v != "" {
			r[env] = v
		}
	}
	type ctxInfo struct {
		Name    string `json:"name"`
		Host    string `json:"host"`
		Managed bool   `json:"managedByHyperbutler"`
	}
	list := []ctxInfo{}
	metas, _ := filepath.Glob(filepath.Join(store.Dir, "contexts", "meta", "*", "meta.json"))
	for _, m := range metas {
		data, err := engine.ReadFileBounded(m)
		if err != nil {
			continue
		}
		if c, err := dockerctx.ParseMeta(data); err == nil {
			list = append(list, ctxInfo{Name: c.Name, Host: c.Host, Managed: c.Managed})
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	r["contexts"] = list
	return r
}

// writeBundle writes the zip to a new file (never over an existing one) that only the
// current user, SYSTEM and Administrators can open, on a file system that keeps access
// lists. aclKept is false when it does not (FAT32, exFAT): the zip is then as open as
// the drive.
func writeBundle(path string, now time.Time, b *bundle) (aclKept bool, err error) {
	f, err := createPrivateFile(path)
	if err != nil {
		return false, err
	}
	aclKept = persistentACLs(f)
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			_ = os.Remove(path)
		}
	}()
	zw := zip.NewWriter(f)
	for _, bf := range b.files {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: bf.name, Method: zip.Deflate, Modified: now})
		if err != nil {
			return aclKept, err
		}
		if _, err := w.Write(bf.data); err != nil {
			return aclKept, err
		}
	}
	return aclKept, zw.Close()
}

// persistentACLs reports whether f's file system stores access lists
// (FILE_PERSISTENT_ACLS: NTFS and ReFS do; FAT32 and exFAT do not). When it cannot
// tell, it says yes.
func persistentACLs(f *os.File) bool {
	var flags uint32
	if err := windows.GetVolumeInformationByHandle(windows.Handle(f.Fd()), nil, 0, nil, nil, &flags, nil, 0); err != nil {
		return true
	}
	return flags&windows.FILE_PERSISTENT_ACLS != 0
}

// createPrivateFile creates path, which must not exist, with a protected DACL for the
// current user, SYSTEM and Administrators (as engine.MkdirPrivate does for directories),
// so the bundle does not inherit a share's or a volume root's wider access. That holds
// only on a file system with access lists (persistentACLs): FAT32 and exFAT drop the
// DACL, and anyone who can open the drive can read the file.
func createPrivateFile(path string) (*os.File, error) {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + tu.User.Sid.String() + ")(A;;FA;;;SY)(A;;FA;;;BA)")
	if err != nil {
		return nil, err
	}
	sa := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_WRITE, 0, sa, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "create", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}
