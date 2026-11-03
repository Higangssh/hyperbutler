//go:build windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"

	"github.com/Higangssh/hyperbutler/internal/broker"
	"github.com/Higangssh/hyperbutler/internal/guid"
	"github.com/Higangssh/hyperbutler/internal/hvaddr"
	"github.com/Higangssh/hyperbutler/internal/vmtemplate"
	"github.com/Higangssh/hyperbutler/internal/winipc"
)

// guestPorts are the services of the VM's hvsock table (vmtemplate): the second user's
// connects to each must be refused by the connect descriptor (ADR 0004 rule 6).
var guestPorts = []struct {
	port uint32
	name string
}{
	{vmtemplate.DockerPort, "docker relay"},
	{vmtemplate.ControlPort, "guest control"},
	{vmtemplate.NetPort, "network link"},
	{vmtemplate.SharePort, "share"},
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var checks []check
	var err error
	switch os.Args[1] {
	case "record":
		checks, err = record(os.Args[2:])
	case "probe":
		checks, err = probe(os.Args[2:])
	case "groups":
		checks, err = groups()
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "noadmin:", err)
		os.Exit(2)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(checks); err != nil {
		fmt.Fprintln(os.Stderr, "noadmin:", err)
		os.Exit(2)
	}
	for _, c := range checks {
		if !c.OK {
			os.Exit(1)
		}
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: noadmin record -status FILE -out FILE [-debug-pipe NAME] | probe -owner FILE -expect allow|deny | groups")
	os.Exit(2)
}

// record writes owner.json from the owner's `hyperbutler status --json`.
func record(args []string) ([]check, error) {
	fs := flag.NewFlagSet("record", flag.ContinueOnError)
	status := fs.String("status", "", "the output of `hyperbutler status --json`")
	out := fs.String("out", "", "owner.json to write")
	debugPipe := fs.String("debug-pipe", "", "the engine's debug pipe, if it was started with one")
	shared := fs.String("shared", broker.ServicePipe, "the broker's shared endpoint")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if *status == "" || *out == "" {
		return nil, errors.New("record: -status and -out are required")
	}
	b, err := os.ReadFile(*status)
	if err != nil {
		return nil, err
	}
	o, err := ownerFromStatus(b)
	if err != nil {
		return []check{{Name: "owner: hyperbutler is running", Detail: err.Error()}}, nil
	}
	checks := []check{{Name: "owner: hyperbutler is running", OK: true, Detail: fmt.Sprintf("engine pid %d, VM %s (%s)", o.EnginePID, o.VMName, o.VMRuntimeID)}}
	if _, err := guid.FromString(o.VMRuntimeID); err != nil {
		return nil, fmt.Errorf("record: runtime id: %w", err)
	}
	u, err := user.Current()
	if err != nil {
		return nil, err
	}
	o.User, o.SID, o.Recorded = u.Username, u.Uid, time.Now().UTC()
	o.DebugPipe = *debugPipe
	o.BrokerShared = *shared
	for _, name := range listPipes() {
		if full := `\\.\pipe\` + name; broker.ValidUserPipeName(*shared, o.SID, full) {
			o.BrokerPipes = append(o.BrokerPipes, full)
		}
	}
	checks = append(checks, check{Name: "owner: has a per-user broker endpoint",
		OK: len(o.BrokerPipes) > 0, Detail: fmt.Sprintf("%d endpoint(s) of %s next to %s", len(o.BrokerPipes), o.SID, *shared)})
	j, err := json.MarshalIndent(o, "", "  ")
	if err != nil {
		return nil, err
	}
	// The file takes the folder's inherited ACL, which lets the second user read it.
	if err := os.WriteFile(*out, append(j, '\n'), 0o600); err != nil {
		return nil, err
	}
	return append(checks, check{Name: "owner: owner.json written", OK: true, Detail: *out}), nil
}

// probe opens the owner's pipes, events and VM services, and checks each outcome
// against expect.
func probe(args []string) ([]check, error) {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	ownerPath := fs.String("owner", "", "owner.json")
	expect := fs.String("expect", "", "allow (run as the owner) or deny (run as another user)")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if *expect != "allow" && *expect != "deny" {
		return nil, errors.New("probe: -expect must be allow or deny")
	}
	allow := *expect == "allow"
	b, err := os.ReadFile(*ownerPath)
	if err != nil {
		return nil, err
	}
	var o Owner
	if err := json.Unmarshal(b, &o); err != nil {
		return nil, fmt.Errorf("probe: %s: %w", *ownerPath, err)
	}
	if o.Schema != OwnerSchema {
		return nil, fmt.Errorf("probe: %s has schema %d, want %d", *ownerPath, o.Schema, OwnerSchema)
	}
	vmid, err := guid.FromString(o.VMRuntimeID)
	if err != nil {
		return nil, fmt.Errorf("probe: runtime id: %w", err)
	}
	u, err := user.Current()
	if err != nil {
		return nil, err
	}
	var checks []check
	add := func(c check) { checks = append(checks, c) }
	sameUser := strings.EqualFold(u.Uid, o.SID)
	add(check{Name: "caller: " + map[bool]string{true: "is the owner", false: "is not the owner"}[allow],
		OK: sameUser == allow, Detail: fmt.Sprintf("%s (%s); owner %s (%s)", u.Username, u.Uid, o.User, o.SID)})

	pipes := []struct{ what, name string }{{"the Docker API pipe", o.DockerPipe}}
	if o.DebugPipe != "" {
		pipes = append(pipes, struct{ what, name string }{"the engine's debug pipe", o.DebugPipe})
	}
	for _, p := range o.BrokerPipes {
		pipes = append(pipes, struct{ what, name string }{"the owner's broker endpoint", p})
	}
	if len(o.BrokerPipes) == 0 {
		add(check{Name: "pipe: the owner's broker endpoint is recorded", Detail: "owner.json lists none; was the owner's engine started through the broker?"})
	}
	for _, p := range pipes {
		c, got := openPipe(p.what, p.name, allow)
		add(c)
		if !allow && got != "not found" {
			add(addInstance(p.what, p.name))
		}
	}
	for _, ev := range o.EngineEvents {
		add(openEvent(ev, allow))
	}
	for _, gp := range guestPorts {
		if allow && gp.port != vmtemplate.DockerPort {
			// The owner's positive control dials only the Docker relay, which takes any
			// number of connections: the control channel and the guest-initiated
			// services are the engine's alone.
			continue
		}
		add(dialGuest(vmid, gp.port, gp.name, allow))
	}
	return checks, nil
}

// outcome sorts the error of an open: nil, access denied, or a name that does not
// exist (which says nothing about access, so it never passes a deny check).
func outcome(err error) string {
	switch {
	case err == nil:
		return "opened"
	case errors.Is(err, windows.ERROR_ACCESS_DENIED), errors.Is(err, windows.WSAEACCES):
		return "access denied"
	case errors.Is(err, windows.ERROR_FILE_NOT_FOUND), errors.Is(err, windows.ERROR_PATH_NOT_FOUND):
		return "not found"
	case errors.Is(err, windows.WSAEADDRNOTAVAIL):
		return "no such VM"
	}
	return "other"
}

func verdict(allow bool, got string) bool {
	if allow {
		return got == "opened"
	}
	return got == "access denied"
}

func openPipe(what, name string, allow bool) (check, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := winipc.DialPipe(ctx, name)
	if c != nil {
		_ = c.Close()
	}
	got := outcome(err)
	return check{Name: "pipe: open " + what, OK: verdict(allow, got), Detail: describe(name, got, err)}, got
}

// addInstance tries to create a server instance of the owner's pipe, as a squatter
// would: it must fail. probe skips it for a name that does not exist, which it would
// create.
func addInstance(what, name string) check {
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return check{Name: "pipe: add an instance to " + what, Detail: err.Error()}
	}
	h, err := windows.CreateNamedPipe(n, windows.PIPE_ACCESS_DUPLEX, windows.PIPE_TYPE_BYTE, windows.PIPE_UNLIMITED_INSTANCES, 4096, 4096, 0, nil)
	if err == nil {
		_ = windows.CloseHandle(h)
		return check{Name: "pipe: add an instance to " + what, Detail: name + ": created"}
	}
	return check{Name: "pipe: add an instance to " + what, OK: true, Detail: describe(name, outcome(err), err)}
}

// openEvent opens an engine control event with the access `hyperbutler stop` uses to
// set it. It is never set.
func openEvent(name string, allow bool) check {
	n, err := windows.UTF16PtrFromString(name)
	if err == nil {
		var h windows.Handle
		h, err = windows.OpenEvent(windows.EVENT_MODIFY_STATE|windows.SYNCHRONIZE, false, n)
		if err == nil {
			_ = windows.CloseHandle(h)
		}
	}
	got := outcome(err)
	return check{Name: "engine: open its control event " + eventKind(name), OK: verdict(allow, got), Detail: describe(name, got, err)}
}

func eventKind(name string) string {
	for _, k := range []string{"stop", "daemon", "settings"} {
		if strings.Contains(name, "-engine-"+k+"-") {
			return k
		}
	}
	return name
}

// dialGuest connects to the VM's service on port and closes the connection at once,
// having sent nothing.
func dialGuest(vmid guid.GUID, port uint32, name string, allow bool) check {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := winipc.DialHvsock(ctx, winipc.HvsockAddr{VMID: vmid, ServiceID: hvaddr.VsockServiceID(port)})
	if c != nil {
		_ = c.Close()
	}
	got := outcome(err)
	return check{Name: fmt.Sprintf("hvsock: connect to the VM's %s (port %d)", name, port), OK: verdict(allow, got),
		Detail: describe(vmid.String(), got, err)}
}

func describe(target, got string, err error) string {
	if err == nil {
		return target + ": " + got
	}
	return fmt.Sprintf("%s: %s (%v)", target, got, err)
}

// groups checks that the caller is a standard user, from whoami and from the token.
func groups() ([]check, error) {
	var out bytes.Buffer
	// The full path: a POSIX whoami earlier on PATH (Git for Windows) takes no /groups.
	cmd := exec.Command(filepath.Join(os.Getenv("SystemRoot"), "System32", "whoami.exe"), "/groups", "/fo", "csv", "/nh") //nolint:gosec // G204: whoami from the system directory, fixed arguments
	cmd.Stdout, cmd.Stderr = &out, io.Discard
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("whoami /groups: %w", err)
	}
	sids, err := parseWhoamiGroups(&out)
	if err != nil {
		return nil, err
	}
	checks := judgeGroups(sids)
	tok := windows.GetCurrentProcessToken()
	checks = append(checks, check{Name: "token: not elevated", OK: !tok.IsElevated(), Detail: fmt.Sprintf("elevated %v", tok.IsElevated())})
	tg, err := tokenGroupSIDs(tok)
	if err != nil {
		return nil, err
	}
	// The same two group checks on the token itself (its groups carry no label, so the
	// label check is whoami's alone).
	bad := judgeGroups(append(tg, sidMediumIntegrity))[:2]
	for _, c := range bad {
		c.Name = strings.Replace(c.Name, "whoami:", "token:", 1)
		checks = append(checks, c)
	}
	return checks, nil
}

// tokenGroupSIDs lists the token's groups, whatever their attributes (a deny-only
// Administrators group is listed).
func tokenGroupSIDs(tok windows.Token) ([]string, error) {
	g, err := tok.GetTokenGroups()
	if err != nil {
		return nil, fmt.Errorf("token groups: %w", err)
	}
	var out []string
	for _, sa := range g.AllGroups() {
		out = append(out, sa.Sid.String())
	}
	return out, nil
}

// listPipes lists the names in \\.\pipe\.
func listPipes() []string {
	var out []string
	var fd windows.Win32finddata
	p, _ := windows.UTF16PtrFromString(`\\.\pipe\*`)
	h, err := windows.FindFirstFile(p, &fd)
	if err != nil {
		return nil
	}
	defer func() { _ = windows.FindClose(h) }()
	for {
		out = append(out, windows.UTF16ToString(fd.FileName[:]))
		if windows.FindNextFile(h, &fd) != nil {
			return out
		}
	}
}
