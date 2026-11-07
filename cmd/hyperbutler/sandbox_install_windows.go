//go:build windows

package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows/registry"

	"github.com/Higangssh/hyperbutler/guest/image"
	"github.com/Higangssh/hyperbutler/internal/broker"
	"github.com/Higangssh/hyperbutler/internal/broker/proto"
	"github.com/Higangssh/hyperbutler/internal/engine"
	"github.com/Higangssh/hyperbutler/internal/imageset"
	"github.com/Higangssh/hyperbutler/internal/sandboxmsi"
	"github.com/Higangssh/hyperbutler/internal/updatecheck"
)

// The sandbox image ships as an MSI of its own (ADR 0009, "Shipping the image"): the main
// installer has REQ-UX-002's limit, and most users never start a session. These are the
// command that installs it, the offer `sandbox run` and `start` make when it is missing, and
// what the user reads when the broker refuses the image.

const (
	// sandboxInstallCommand is what every message tells the user to run.
	sandboxInstallCommand = "hyperbutler sandbox install"
	// sandboxMSIDirName is the private folder under the state directory for the download.
	sandboxMSIDirName = "sandbox-msi"
	// hbRegKey holds what the installers record; the main MSI writes InstallDir, the sandbox
	// MSI its version and product code (dist/msi/sandbox.wxs).
	hbRegKey              = `SOFTWARE\hyperbutler`
	regInstallDir         = "InstallDir"
	regSandboxVersion     = "SandboxVersion"
	regSandboxProductCode = "SandboxProductCode"
)

func (in *install) sandboxMSIDir() string { return filepath.Join(in.l.Home, sandboxMSIDirName) }

// errSandboxNeedsMSI is a zip install or any other copy that is not the installed one. The
// image goes next to the broker service, which a zip does not have.
var errSandboxNeedsMSI = errors.New("the agent sandbox needs the installed hyperbutler: its image goes next to the broker service in Program Files, " +
	"and this copy is not that one. Install the hyperbutler MSI (docs/install.md), or, for a development build, name your own image with HYPERBUTLER_SANDBOX_IMAGE")

// errSandboxDevBuild is a build with no installer pin to check a download against.
var errSandboxDevBuild = errors.New("this is a development build: it carries no pin for the sandbox image's installer, so it installs nothing. " +
	"Build an image with guest/sandbox/build.ps1 and name it with HYPERBUTLER_SANDBOX_IMAGE, or use a release")

// machineProduct is what this machine's installers recorded.
type machineProduct struct {
	installDir     string // the main MSI's folder; empty when it is not installed
	service        bool   // the broker service exists
	sandboxVersion string // the sandbox MSI's version; empty when it is not installed
	// sandboxProductCode is that MSI's product code, with braces.
	sandboxProductCode string
}

// readMachineProduct reads it. A key that cannot be read counts as not installed.
func readMachineProduct() machineProduct {
	var m machineProduct
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, hbRegKey, registry.QUERY_VALUE); err == nil {
		defer func() { _ = k.Close() }()
		m.installDir, _, _ = k.GetStringValue(regInstallDir)
		m.sandboxVersion, _, _ = k.GetStringValue(regSandboxVersion)
		m.sandboxProductCode, _, _ = k.GetStringValue(regSandboxProductCode)
	}
	st, err := machineService.query()
	m.service = err == nil && st.Installed
	return m
}

// imageState is what the image next to this executable looks like.
type imageState int

const (
	imageOK imageState = iota
	imageMissing
	imageMismatch
)

// checkSandboxManifest compares image.json with the pin, without the files it names:
// enough for a quick check, since the manifest holds their hashes and the broker checks
// those. An unreadable file counts as a mismatch.
func checkSandboxManifest(path, pin string) imageState {
	b, err := engine.ReadFileLimit(path, 64<<10)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return imageMissing
	case err != nil:
		return imageMismatch
	}
	if s := sha256.Sum256(b); hex.EncodeToString(s[:]) != pin {
		return imageMismatch
	}
	return imageOK
}

// sandboxSetup is everything the install decides from, with what touches the machine as
// fields for the tests.
type sandboxSetup struct {
	manifest    string // image.json next to this executable
	manifestPin string
	msi         sandboxmsi.Pin
	msiErr      error
	version     string
	exeDir      string
	dir         string
	custom      bool // a developer's own image or broker: none of this applies
	product     func() machineProduct
	// verify checks the whole image set; console says stdin can answer a question.
	verify  func() error
	console func(io.Reader) bool
	run     func(ctx context.Context, i *sandboxmsi.Installer) (sandboxmsi.Result, error)
	fetch   func(url string, stderr io.Writer) func(ctx context.Context, path string, max int64) error
}

func newSandboxSetup(in *install) (*sandboxSetup, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	s := &sandboxSetup{
		manifest: filepath.Join(filepath.Dir(exe), sandboxImageDirName, image.ManifestName),
		version:  versionInfo().Version,
		exeDir:   filepath.Dir(exe),
		dir:      in.sandboxMSIDir(),
		custom:   os.Getenv("HYPERBUTLER_SANDBOX_IMAGE") != "" || os.Getenv("HYPERBUTLER_SANDBOX_BROKER") != "",
		product:  readMachineProduct,
		console:  isConsole,
		run:      func(ctx context.Context, i *sandboxmsi.Installer) (sandboxmsi.Result, error) { return i.Run(ctx) },
		fetch: func(url string, stderr io.Writer) func(context.Context, string, int64) error {
			client := sandboxmsi.NewClient(updateProxy(in.settings), nil)
			return sandboxmsi.Download(client, url, updatecheck.UserAgent(versionInfo().Version), 0, downloadProgress(stderr))
		},
	}
	s.manifestPin, _ = imageset.SandboxPin()
	sum, name, size, code := imageset.SandboxMSIPin()
	s.msi, s.msiErr = sandboxmsi.ParsePin(sum, name, size, code)
	s.verify = func() error {
		set, err := imageset.Open(s.manifest, s.manifestPin)
		if err != nil {
			return err
		}
		return set.Close()
	}
	return s, nil
}

// installable says why this copy cannot install the image, if it cannot.
func (s *sandboxSetup) installable() error {
	if s.manifestPin == "" || errors.Is(s.msiErr, sandboxmsi.ErrNoPin) {
		return errSandboxDevBuild
	}
	if s.msiErr != nil {
		return s.msiErr
	}
	m := s.product()
	if !m.service || m.installDir == "" || !engine.SamePath(filepath.Clean(m.installDir), s.exeDir) {
		return errSandboxNeedsMSI
	}
	return nil
}

// install is `sandbox install`: nothing when the image is already right, otherwise fetch,
// check and install. localMSI, from --msi, replaces the download. Run decides nothing about
// the file: it is the same whichever way it came, and only its hash lets it through.
func (s *sandboxSetup) install(ctx context.Context, localMSI string, stdout, stderr io.Writer) error {
	if err := s.installable(); err != nil {
		return err
	}
	if s.verify() == nil {
		_, _ = fmt.Fprintf(stdout, "The agent sandbox image is already installed and matches this hyperbutler (%s).\n", filepath.Dir(s.manifest))
		return nil
	}
	var fetch func(context.Context, string, int64) error
	if localMSI != "" {
		fetch = sandboxmsi.Copy(localMSI)
	} else {
		if updatecheck.IsDevBuild(s.version) {
			return fmt.Errorf("this build's version (%s) is not a release, so it has no download to fetch; give the MSI of its package run with --msi FILE", s.version)
		}
		url, err := sandboxmsi.ReleaseURL(s.version, s.msi.Name)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stderr, "Downloading %s (%s) from github.com...\n", s.msi.Name, engine.HumanSize(uint64(s.msi.Bytes))) //nolint:gosec // G115: ParsePin keeps it positive
		fetch = s.fetch(url, stderr)
	}
	if err := engine.MkdirPrivate(s.dir); err != nil {
		return err
	}
	res, err := s.run(ctx, &sandboxmsi.Installer{
		Pin: s.msi, Dir: s.dir, Fetch: fetch, Lock: sandboxmsi.Lock, Exec: sandboxmsi.Exec, Verify: s.verify,
		// The product already there with this product code is this MSI, which msiexec would
		// otherwise only open in maintenance mode and leave as it is. Anything else is a major
		// upgrade, for which that would remove the old product and install nothing.
		Reinstall: strings.EqualFold(strings.Trim(s.product().sandboxProductCode, "{}"), s.msi.ProductCode),
		Note:      func(f string, a ...any) { _, _ = fmt.Fprintf(stderr, f+"\n", a...) },
	})
	if err != nil {
		return err
	}
	if res.RestartNeeded {
		_, _ = fmt.Fprintln(stdout, "The agent sandbox image is installed. Windows asks for a restart to finish replacing a file that was in use.")
		return nil
	}
	_, _ = fmt.Fprintln(stdout, "The agent sandbox image is installed. `hyperbutler sandbox run DIR` starts a session.")
	return nil
}

// sandboxInstall is `hyperbutler sandbox install`.
func sandboxInstall(in *install, args []string, stdout, stderr io.Writer) (int, error) {
	fs := flag.NewFlagSet("sandbox install", flag.ContinueOnError)
	fs.SetOutput(stderr)
	local := fs.String("msi", "", "install this file instead of downloading it; its hash must still match")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	if fs.NArg() != 0 {
		return 2, errors.New("sandbox install takes no arguments")
	}
	s, err := newSandboxSetup(in)
	if err != nil {
		return 1, err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := s.install(ctx, *local, stdout, stderr); err != nil {
		return 1, err
	}
	return 0, nil
}

// ensure is what `sandbox run` and `start` do first: nothing while the image is right,
// which a development build's rules decide alone. Otherwise it asks, in a console only, and
// never downloads unasked.
func (s *sandboxSetup) ensure(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) error {
	if s.custom || s.manifestPin == "" {
		return nil
	}
	state := checkSandboxManifest(s.manifest, s.manifestPin)
	if state == imageOK {
		return nil
	}
	if err := s.installable(); err != nil {
		return err
	}
	what := "is not installed"
	if state == imageMismatch {
		what = "does not match this hyperbutler"
	}
	if !s.console(stdin) {
		return fmt.Errorf("the agent sandbox image %s. Run `%s` to install it (%s from github.com)", what, sandboxInstallCommand, engine.HumanSize(uint64(s.msi.Bytes))) //nolint:gosec // G115: ParsePin keeps it positive
	}
	// On stderr: stdout may be redirected while stdin is the console, and a question nobody
	// sees looks like a hang.
	_, _ = fmt.Fprintf(stderr, "The agent sandbox image %s. Download it from github.com (%s) and install it? Windows asks for permission once. [y/N] ", what, engine.HumanSize(uint64(s.msi.Bytes))) //nolint:gosec // G115: ParsePin keeps it positive
	line, _ := bufio.NewReader(stdin).ReadString('\n')
	if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
		return fmt.Errorf("the agent sandbox image %s. `%s` installs it when you want it", what, sandboxInstallCommand)
	}
	return s.install(ctx, "", stdout, stderr)
}

// ensureSandboxImage is ensure for the running command.
func ensureSandboxImage(in *install, stdin io.Reader, stdout, stderr io.Writer) error {
	s, err := newSandboxSetup(in)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return s.ensure(ctx, stdin, stdout, stderr)
}

// downloadProgress shows a download on stderr, on one rewritten line in a console and every
// 30 seconds otherwise.
func downloadProgress(w io.Writer) func(done, total int64) {
	f, ok := w.(*os.File)
	console := ok && isConsole(f)
	every := 30 * time.Second
	if console {
		every = 250 * time.Millisecond
	}
	var last time.Time
	return func(done, total int64) {
		end := done >= total
		if !end && time.Since(last) < every {
			return
		}
		last = time.Now()
		line := fmt.Sprintf("  %d%% of %s", done*100/max(total, 1), engine.HumanSize(uint64(max(total, 0))))
		switch {
		case console && end:
			_, _ = fmt.Fprintf(w, "\r%s\n", line)
		case console:
			_, _ = fmt.Fprintf(w, "\r%s", line)
		default:
			_, _ = fmt.Fprintln(w, line)
		}
	}
}

// sandboxBootError is the message of a session that did not boot because of its image: the
// broker says only that its image failed its checks, and has no idea what a user can do.
// custom: a developer's own image, whose own error says more.
func sandboxBootError(err error, custom bool, manifest, pin string) error {
	if err == nil || custom {
		return err
	}
	refused := broker.IsCode(err, proto.CodeImageRejected)
	if !refused && pin != "" {
		refused = checkSandboxManifest(manifest, pin) != imageOK
	}
	if !refused {
		return err
	}
	return fmt.Errorf("the agent sandbox image is missing, or is not the one this hyperbutler was built with: run `%s`", sandboxInstallCommand)
}

// sandboxImageCheck is doctor's line about the image, for the builds that can install one:
// not for a development build, and on a copy that cannot (a zip) only when an image is
// there at all. A missing image is a note: most people never start a session.
func sandboxImageCheck(manifest, pin string, installed bool) (c DoctorCheck, ok bool) {
	if pin == "" {
		return c, false
	}
	state := checkSandboxManifest(manifest, pin)
	if state == imageMissing && !installed {
		return c, false
	}
	switch state {
	case imageOK:
		return DoctorCheck{ID: "sandbox-image", Status: doctorOK, Summary: "the agent sandbox image is installed and matches this hyperbutler",
			Msg: "doctor.sandbox-image.ok"}, true
	case imageMismatch:
		return DoctorCheck{ID: "sandbox-image", Status: doctorWarn, Summary: "the agent sandbox image does not match this hyperbutler, so sessions cannot start",
			Fix: "Run: " + sandboxInstallCommand,
			Msg: "doctor.sandbox-image.mismatch", FixMsg: "doctor.fix.sandboxImage"}, true
	}
	return DoctorCheck{ID: "sandbox-image", Status: doctorInfo, Summary: "the agent sandbox image is not installed (it is a separate download, for hyperbutler sandbox only)",
		Msg: "doctor.sandbox-image.absent"}, true
}

// sandboxProductNote is what uninstall says about the sandbox's own product, which is not
// this user's data and which the MSI's own uninstall will not leave behind.
func sandboxProductNote(version string) string {
	if version == "" {
		return ""
	}
	return "The agent sandbox image (\"hyperbutler agent sandbox\" " + version + " in Apps) is a product of its own, for this whole PC: uninstall it there before you uninstall hyperbutler, whose installer refuses while it is there (docs/install.md)."
}
