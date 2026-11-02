//go:build windows

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Higangssh/hyperbutler/internal/engine"
	"github.com/Higangssh/hyperbutler/internal/engine/dockerctx"
	"github.com/Higangssh/hyperbutler/internal/transport"
	"github.com/Higangssh/hyperbutler/internal/vm"
)

// `hyperbutler doctor` (REQ-UX-019): a quick, read-only list of what hyperbutler needs
// from the machine and the docker CLI's setup, each with the fix when it is wrong. It
// starts nothing and changes no file. The only pipe it connects to is the broker
// service's, while the SCM reports that service running, for its version (brokerCheck).

// Results of a doctor check.
const (
	doctorOK   = "ok"
	doctorInfo = "info" // worth knowing, nothing to fix
	doctorWarn = "warn" // works, but something will fail or surprise
	doctorFail = "fail" // hyperbutler start cannot work until it is fixed
)

// guestFullPercent is when the guest file system counts as nearly full (REQ-DISK-009).
const guestFullPercent = 90

// hostLowFree is the host free space below which doctor warns: the free-space guard
// pauses the VM long before the volume is full (REQ-DISK-004).
const hostLowFree = 5 << 30

// msgArgs pairs the names and values a check's catalog sentence fills in.
func msgArgs(kv ...string) map[string]string {
	m := make(map[string]string, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

func cmdDoctor(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("doctor", stderr)
	asJSON := fs.Bool("json", false, fmt.Sprintf("print JSON (schema %d)", SchemaVersion))
	if err := parseArgs(fs, args); err != nil {
		return err
	}
	in, err := openInstallLenient()
	if err != nil {
		return err
	}
	r := runDoctor(in)
	if *asJSON {
		if err := writeJSON(stdout, r); err != nil {
			return err
		}
	} else {
		printDoctor(stdout, r)
	}
	if !r.OK {
		return &shownError{err: errors.New("doctor: a check failed")}
	}
	return nil
}

func printDoctor(w io.Writer, r DoctorJSON) {
	for _, c := range r.Checks {
		_, _ = fmt.Fprintf(w, "%-5s %-18s %s\n", strings.ToUpper(c.Status), c.ID, c.Summary)
		if c.Fix != "" {
			for _, l := range strings.Split(c.Fix, "\n") {
				_, _ = fmt.Fprintf(w, "%-24s %s\n", "", l)
			}
		}
	}
	if r.OK {
		_, _ = fmt.Fprintln(w, "\nNothing blocks hyperbutler start.")
	} else {
		_, _ = fmt.Fprintln(w, "\nFix the FAIL lines first: hyperbutler start cannot work until then.")
	}
}

// runDoctor makes every check.
func runDoctor(in *install) DoctorJSON {
	r := DoctorJSON{Schema: SchemaVersion, Home: in.l.Home, OK: true}
	add := func(c DoctorCheck) {
		r.Checks = append(r.Checks, c)
		if c.Status == doctorFail {
			r.OK = false
		}
	}
	for _, c := range virtualizationChecks(virtualization()) {
		add(c)
	}
	if member, err := inHyperVAdmins(); err == nil && !member && engine.HyperVAdminsSignInPending() {
		add(DoctorCheck{ID: "hyperv-admins", Status: doctorFail,
			Summary: "your account is in Hyper-V Administrators, but this sign-in started before it was added, so HCS still refuses",
			Fix:     "Sign out of Windows and sign back in (or restart): group changes apply at sign-in.",
			Msg:     "doctor.hyperv-admins.pending", FixMsg: "doctor.fix.signIn"})
	} else if err == nil && !member {
		add(DoctorCheck{ID: "hyperv-admins", Status: doctorFail,
			Summary: "your sign-in token is not a member of Hyper-V Administrators, so HCS refuses to create the VM",
			Fix:     `An administrator runs: net localgroup "Hyper-V Administrators" <you> /add` + "\nthen sign out and back in (README, Prerequisites).",
			Msg:     "doctor.hyperv-admins.no", FixMsg: "doctor.fix.hypervAdmins"})
	} else if err == nil {
		add(DoctorCheck{ID: "hyperv-admins", Status: doctorOK, Summary: "your sign-in token is a member of Hyper-V Administrators", Msg: "doctor.hyperv-admins.ok"})
	}

	if c, ok := installerSetupCheck(); ok {
		add(c)
	}
	add(brokerCheck(machineBroker))

	st, _ := engine.LoadState(in.l)
	up, _ := in.running()
	pipe := transport.DefaultPipeName
	if in.settings != nil {
		pipe = in.settings.PipeName()
	}
	add(pipeCheck(pipe, up, st))

	cli := findDockerCLI()
	add(cliCheck(cli))
	store, serr := dockerctx.DefaultStore()
	if serr != nil {
		add(DoctorCheck{ID: "docker-context", Status: doctorWarn, Summary: "the docker CLI's config directory is unknown: " + serr.Error()})
	} else {
		add(contextCheck(in, store, pipe))
		add(credCheck(store))
	}
	add(otherEnginesCheck(pipe))
	for _, c := range diskChecks(in, st, up) {
		add(c)
	}
	add(autostartCheck(in))
	return r
}

// virtualizationChecks reads diagnose's virtualization facts (the same registry and
// DLL checks): Virtual Machine Platform, the HCS service, the hypervisor launch type
// and whether HCS answers.
func virtualizationChecks(v map[string]any) []DoctorCheck {
	const fixVMP = "In an elevated PowerShell: dism.exe /online /enable-feature /featurename:VirtualMachinePlatform /all /norestart\nthen reboot (README, Prerequisites)."
	var out []DoctorCheck
	if s, _ := v["computecore"].(string); s != "loadable" {
		out = append(out, DoctorCheck{ID: "vm-platform", Status: doctorFail, Summary: "Virtual Machine Platform is not enabled (computecore.dll " + s + ")", Fix: fixVMP,
			Msg: "doctor.vm-platform.off", FixMsg: "doctor.fix.vmp", Args: msgArgs("state", s)})
	} else {
		out = append(out, DoctorCheck{ID: "vm-platform", Status: doctorOK, Summary: "Virtual Machine Platform is enabled (computecore.dll loads)", Msg: "doctor.vm-platform.ok"})
	}
	services, _ := v["services"].(map[string]any)
	switch s := services["vmcompute"].(type) {
	case string:
		if s == "absent" {
			out = append(out, DoctorCheck{ID: "hcs-service", Status: doctorFail, Summary: "the Host Compute Service (vmcompute) is not installed", Fix: fixVMP,
				Msg: "doctor.hcs-service.absent", FixMsg: "doctor.fix.vmp"})
		}
	case map[string]any:
		if n, _ := s["start"].(uint64); n == 4 {
			out = append(out, DoctorCheck{ID: "hcs-service", Status: doctorFail, Summary: "the Host Compute Service (vmcompute) is disabled",
				Fix: "An administrator sets its startup type back to Manual (services.msc, Hyper-V Host Compute Service).",
				Msg: "doctor.hcs-service.disabled", FixMsg: "doctor.fix.hcsDisabled"})
		} else {
			out = append(out, DoctorCheck{ID: "hcs-service", Status: doctorOK, Summary: "the Host Compute Service (vmcompute) is installed", Msg: "doctor.hcs-service.ok"})
		}
	}
	if n, ok := v["hypervisorLaunchType"].(uint64); ok && n == 0 {
		out = append(out, DoctorCheck{ID: "hypervisor", Status: doctorFail, Summary: "the hypervisor is set not to start (hypervisorlaunchtype off)",
			Fix: "In an elevated PowerShell: bcdedit /set hypervisorlaunchtype auto\nthen reboot.",
			Msg: "doctor.hypervisor.off", FixMsg: "doctor.fix.hypervisor"})
	}
	switch cs := v["computeSystems"].(type) {
	case string:
		out = append(out, DoctorCheck{ID: "hcs", Status: doctorFail, Summary: cs,
			Fix:    "Check the lines above (Virtual Machine Platform, Hyper-V Administrators), and that virtualization is on in the firmware.",
			FixMsg: "doctor.fix.hcs"})
	case []string:
		out = append(out, DoctorCheck{ID: "hcs", Status: doctorOK, Summary: fmt.Sprintf("HCS answers (%d compute systems)", len(cs)),
			Msg: "doctor.hcs.ok", Args: msgArgs("n", fmt.Sprint(len(cs)))})
	}
	return out
}

// pipeCheck says whether the Docker API pipe is free, this installation's, or taken.
func pipeCheck(pipe string, up bool, st *engine.State) DoctorCheck {
	c := DoctorCheck{ID: "pipe", Args: msgArgs("pipe", pipe)}
	exists := pipeExists(pipe)
	switch {
	case up && st != nil && st.Phase.Live() && strings.EqualFold(st.Pipe, pipe):
		c.Status, c.Summary = doctorOK, fmt.Sprintf("%s is served by this hyperbutler (engine pid %d)", pipe, st.PID)
		c.Msg, c.Args["pid"] = "doctor.pipe.ours", fmt.Sprint(st.PID)
	case exists && up:
		c.Status, c.Summary = doctorInfo, fmt.Sprintf("%s exists and hyperbutler is running (starting, or with another pipe until a restart)", pipe)
		c.Msg = "doctor.pipe.starting"
	case exists:
		c.Status, c.Summary = doctorFail, pipe+" is in use by another program, so hyperbutler start fails with pipe-in-use"
		c.Fix = `Stop that program, or give hyperbutler another pipe: hyperbutler config set pipe \\.\pipe\docker_engine_hyperbutler2`
		c.Msg, c.FixMsg = "doctor.pipe.taken", "doctor.fix.pipeTaken"
	default:
		c.Status, c.Summary = doctorOK, pipe+" is free"
		c.Msg = "doctor.pipe.free"
	}
	return c
}

func cliCheck(cli dockerCLI) DoctorCheck {
	c := DoctorCheck{ID: "docker-cli"}
	if cli.Path == "" {
		c.Status, c.Summary, c.Msg = doctorWarn, "no docker CLI on PATH", "doctor.docker-cli.none"
		if cli.Bundled != "" {
			c.FixMsg, c.Args = "doctor.fix.cliBundled", msgArgs("dir", filepath.Dir(cli.Bundled))
			c.Fix = "hyperbutler comes with one, " + cli.Bundled + "; add its folder to your PATH, or for this shell:\n" + pathAdvice(filepath.Dir(cli.Bundled))
		} else {
			c.Fix = "Install a docker CLI (19.03 or later), or use the one in the hyperbutler release zip (clients\\docker.exe)."
			c.FixMsg = "doctor.fix.cliInstall"
		}
		return c
	}
	which := "on PATH"
	if cli.usesBundled() {
		which = "bundled"
	}
	major, minor, ok := cliVersion(cli.Path)
	// The catalog has one sentence per place: bundled, or found on PATH.
	where := "Path"
	if cli.usesBundled() {
		where = "Bundled"
	}
	c.Args = msgArgs("path", cli.Path, "version", fmt.Sprintf("%d.%d", major, minor), "min", minCLIVersion)
	switch {
	case !ok:
		c.Status, c.Summary = doctorInfo, fmt.Sprintf("%s (%s); its version is unknown", cli.Path, which)
		c.Msg = "doctor.docker-cli.unknown" + where
	case cliTooOld(major, minor):
		c.Status, c.Summary = doctorFail, fmt.Sprintf("%s (%s) is version %d.%d; hyperbutler's Docker Engine needs %s or later", cli.Path, which, major, minor, minCLIVersion)
		c.Fix = "Put a newer docker CLI first on PATH (the one in the release zip's clients\\ folder)."
		c.Msg, c.FixMsg = "doctor.docker-cli.old"+where, "doctor.fix.cliNewer"
	default:
		c.Status, c.Summary = doctorOK, fmt.Sprintf("%s (%s, %d.%d)", cli.Path, which, major, minor)
		c.Msg = "doctor.docker-cli.ok" + where
	}
	return c
}

func contextCheck(in *install, store dockerctx.Store, pipe string) DoctorCheck {
	c := DoctorCheck{ID: "docker-context"}
	t, err := currentTarget(os.Getenv, store, pipe)
	if err != nil {
		c.Status, c.Summary = doctorWarn, "the docker CLI's config.json is unreadable: "+err.Error()
		c.Fix = "Fix or remove " + store.ConfigPath() + "; docker commands fail on it too."
		return c
	}
	c.Args = msgArgs("what", t.What, "name", contextName)
	switch {
	case t.Ours:
		c.Status, c.Summary = doctorOK, "plain docker uses hyperbutler ("+t.What+")"
		c.Msg = "doctor.docker-context.ours"
	case in.settings != nil && !in.settings.ContextManaged():
		c.Status, c.Summary = doctorInfo, "plain docker uses "+t.What+"; context.manage is off, so hyperbutler start does not select its context"
		c.Msg = "doctor.docker-context.unmanaged"
	case t.FromEnv:
		c.Status, c.Summary = doctorWarn, "plain docker in this shell uses "+t.What+", not hyperbutler"
		c.Fix = "Remove DOCKER_HOST or DOCKER_CONTEXT from this shell, or use docker --context " + contextName + " ..."
		c.Msg, c.FixMsg = "doctor.docker-context.env", "doctor.fix.contextEnv"
	default:
		c.Status, c.Summary = doctorWarn, "plain docker uses "+t.What+", not hyperbutler"
		c.Fix = "hyperbutler start selects the " + contextName + " context, or: docker context use " + contextName
		c.Msg, c.FixMsg = "doctor.docker-context.other", "doctor.fix.contextUse"
	}
	return c
}

// lookPath finds a program on PATH; a variable for the tests.
var lookPath = exec.LookPath

// credProblem is a credential helper config.json names that is not on PATH.
type credProblem struct {
	Message, Fix string
}

// credProblems checks that every helper config.json's credsStore and credHelpers name
// is on PATH (REQ-UX-019). It reads config.json and never changes it.
func credProblems(store dockerctx.Store) ([]credProblem, int, error) {
	hs, err := store.CredHelpers()
	if err != nil {
		return nil, 0, err
	}
	var out []credProblem
	for _, h := range hs {
		if p, err := lookPath(h.Program()); err == nil && p != "" {
			continue
		}
		prog := h.Program() + ".exe"
		if h.Registry == "" {
			fix := fmt.Sprintf(`Remove the "credsStore" line from %s (docker then keeps logins in that file),`+
				` or put docker-credential-wincred.exe (docker/docker-credential-helpers releases) on PATH and set it to "wincred".`, store.ConfigPath())
			if _, err := lookPath("docker-credential-wincred"); err == nil && h.Name != "wincred" {
				fix = fmt.Sprintf(`Set "credsStore" to "wincred" in %s (docker-credential-wincred.exe is on PATH), or remove the line.`, store.ConfigPath())
			}
			out = append(out, credProblem{
				Message: fmt.Sprintf("config.json's credsStore is %q, but %s is not on PATH, so docker login and pull will fail (\"error getting credentials\").", h.Name, prog),
				Fix:     fix + " hyperbutler does not change it.",
			})
			continue
		}
		out = append(out, credProblem{
			Message: fmt.Sprintf("config.json's credHelpers names %s for %s, which is not on PATH, so docker login and pull from %s will fail.", prog, h.Registry, h.Registry),
			Fix:     fmt.Sprintf("Install that helper, or remove the %q entry of credHelpers in %s. hyperbutler does not change it.", h.Registry, store.ConfigPath()),
		})
	}
	return out, len(hs), nil
}

func credCheck(store dockerctx.Store) DoctorCheck {
	c := DoctorCheck{ID: "credential-helper"}
	probs, n, err := credProblems(store)
	switch {
	case err != nil:
		c.Status, c.Summary = doctorWarn, "cannot read the credential helpers: "+err.Error()
	case len(probs) > 0:
		var msgs, fixes []string
		for _, p := range probs {
			msgs, fixes = append(msgs, p.Message), append(fixes, p.Fix)
		}
		c.Status, c.Summary, c.Fix = doctorWarn, strings.Join(msgs, " "), strings.Join(fixes, "\n")
	case n == 0:
		c.Status, c.Summary, c.Msg = doctorOK, "config.json names no credential helper", "doctor.credential-helper.none"
	default:
		c.Status, c.Summary = doctorOK, fmt.Sprintf("the %d %s config.json names %s on PATH", n, plural(n, "credential helper", "credential helpers"), plural(n, "is", "are"))
		c.Msg, c.Args = "doctor.credential-helper.some", msgArgs("n", fmt.Sprint(n))
	}
	return c
}

// warnCredHelpers is start's warning for missing credential helpers.
func warnCredHelpers(stderr io.Writer) {
	store, err := dockerctx.DefaultStore()
	if err != nil {
		return
	}
	probs, _, err := credProblems(store)
	if err != nil {
		return
	}
	for _, p := range probs {
		_, _ = fmt.Fprintf(stderr, "hyperbutler: warning: %s\n        %s\n", p.Message, p.Fix)
	}
}

func otherEnginesCheck(pipe string) DoctorCheck {
	others := otherEngines(pipe)
	if len(others) == 0 {
		return DoctorCheck{ID: "other-engines", Status: doctorOK, Summary: "no other Docker engine's pipe exists", Msg: "doctor.other-engines.none"}
	}
	var s []string
	for _, e := range others {
		s = append(s, e.Who+" ("+e.Pipe+")")
	}
	return DoctorCheck{ID: "other-engines", Status: doctorInfo, Summary: "another Docker engine is running: " + strings.Join(s, ", ") +
		"; plain docker reaches whichever context is selected, and the engines share the host's ports",
		Msg: "doctor.other-engines.some", Args: msgArgs("list", strings.Join(s, ", "))}
}

func diskChecks(in *install, st *engine.State, up bool) []DoctorCheck {
	var out []DoctorCheck
	dir := in.l.Data
	if f, err := vm.FreeBytes(nearestDir(dir)); err == nil {
		c := DoctorCheck{ID: "host-disk", Status: doctorOK, Summary: fmt.Sprintf("%s free on the volume of %s", engine.HumanSize(f), dir),
			Msg: "doctor.host-disk.free", Args: msgArgs("free", engine.HumanSize(f), "dir", dir)}
		if f < hostLowFree {
			c.Status = doctorWarn
			c.Fix = "Free space on that volume: the VM is paused when it runs out (REQ-DISK-004). docker system prune frees the guest's share."
			c.FixMsg = "doctor.fix.hostLow"
		}
		out = append(out, c)
	}
	switch {
	case up && st != nil && st.Phase.Live() && st.GuestFS != nil:
		g := st.GuestFS
		c := DoctorCheck{ID: "guest-disk", Status: doctorOK, Summary: guestFSLine(g), Msg: "doctor.guest-disk.used",
			Args: msgArgs("used", engine.HumanSize(g.UsedBytes()), "size", engine.HumanSize(g.SizeBytes), "pct", fmt.Sprintf("%.0f", g.UsedPercent()))}
		if g.UsedPercent() > guestFullPercent {
			c.Status, c.Fix = doctorWarn, "docker system prune (and docker builder prune) frees space; growing the data disk is not supported yet."
			c.FixMsg = "doctor.fix.guestFull"
		}
		out = append(out, c)
	case up:
		out = append(out, DoctorCheck{ID: "guest-disk", Status: doctorInfo, Summary: "the guest has not reported its file system yet", Msg: "doctor.guest-disk.waiting"})
	default:
		out = append(out, DoctorCheck{ID: "guest-disk", Status: doctorInfo, Summary: "known while hyperbutler runs", Msg: "doctor.guest-disk.stopped"})
	}
	return out
}

// guestFSLine is how full the guest data file system is ("12.3 GiB of 62.7 GiB used in
// the VM (20%)").
func guestFSLine(g *engine.GuestFSState) string {
	return fmt.Sprintf("%s of %s used in the VM (%.0f%%)", engine.HumanSize(g.UsedBytes()), engine.HumanSize(g.SizeBytes), g.UsedPercent())
}
