//go:build windows

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Higangssh/hyperbutler/internal/broker"
	"github.com/Higangssh/hyperbutler/internal/broker/proto"
	"github.com/Higangssh/hyperbutler/internal/sandboxmsi"
)

// fakeLocked is what the fake install hands to its Exec.
type fakeLocked struct {
	path, sum string
}

func (l fakeLocked) Path() string            { return l.path }
func (l fakeLocked) SHA256() (string, error) { return l.sum, nil }
func (l fakeLocked) Close() error            { return nil }

// setupFixture is a sandboxSetup whose machine is a set of values: an installed copy with a
// pinned image, the image missing until the fake msiexec runs.
type setupFixture struct {
	*sandboxSetup
	exeDir   string
	image    []byte
	verified error
	fetched  int
	execs    int
	exit     uint32
	execErr  error
	reinst   bool
	url      string
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func newSetupFixture(t *testing.T) *setupFixture {
	t.Helper()
	exeDir := t.TempDir()
	msi := []byte("the msi")
	fx := &setupFixture{exeDir: exeDir, image: []byte(`{"image":1}`)}
	s := &sandboxSetup{
		manifest:    filepath.Join(exeDir, "sandbox-image", "image.json"),
		manifestPin: sum(fx.image),
		msi:         sandboxmsi.Pin{SHA256: sum(msi), Name: "x.msi", Bytes: int64(len(msi))},
		version:     "v0.2.0",
		exeDir:      exeDir,
		dir:         filepath.Join(t.TempDir(), "sandbox-msi"),
		product:     func() machineProduct { return machineProduct{installDir: exeDir + `\`, service: true} },
		console:     func(io.Reader) bool { return true },
	}
	s.verify = func() error {
		if fx.verified != nil {
			return fx.verified
		}
		b, err := os.ReadFile(s.manifest)
		if err != nil {
			return err
		}
		if !bytes.Equal(b, fx.image) {
			return errors.New("mismatch")
		}
		return nil
	}
	s.fetch = func(url string, _ io.Writer) func(context.Context, string, int64) error {
		fx.url = url
		return func(_ context.Context, path string, _ int64) error {
			fx.fetched++
			return os.WriteFile(path, msi, 0o600)
		}
	}
	s.run = func(ctx context.Context, i *sandboxmsi.Installer) (sandboxmsi.Result, error) {
		fx.reinst = i.Reinstall
		i.Lock = func(path string) (sandboxmsi.Locked, error) {
			b, err := os.ReadFile(path)
			if err != nil {
				return nil, err
			}
			return fakeLocked{path: path, sum: sum(b)}, nil
		}
		i.Exec = func(context.Context, string, bool) (uint32, error) {
			fx.execs++
			if fx.execErr != nil {
				return 0, fx.execErr
			}
			if fx.exit == 0 || fx.exit == 3010 {
				fx.place(t, fx.image)
			}
			return fx.exit, nil
		}
		return i.Run(ctx)
	}
	fx.sandboxSetup = s
	return fx
}

func (fx *setupFixture) place(t *testing.T, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(fx.manifest), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fx.manifest, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSandboxInstallFirstTime(t *testing.T) {
	fx := newSetupFixture(t)
	var out, errb bytes.Buffer
	if err := fx.install(context.Background(), "", &out, &errb); err != nil {
		t.Fatal(err)
	}
	if fx.fetched != 1 || fx.execs != 1 || fx.reinst {
		t.Errorf("fetched %d, execs %d, reinstall %v", fx.fetched, fx.execs, fx.reinst)
	}
	if want := "https://github.com/Higangssh/hyperbutler/releases/download/v0.2.0/x.msi"; fx.url != want {
		t.Errorf("url %q", fx.url)
	}
	if !strings.Contains(out.String(), "installed") || !strings.Contains(errb.String(), "x.msi") {
		t.Errorf("out %q, err %q", out.String(), errb.String())
	}
	if ents, err := os.ReadDir(fx.dir); err != nil || len(ents) != 0 {
		t.Errorf("temp folder: %v %v", ents, err)
	}
}

func TestSandboxInstallAlreadyThere(t *testing.T) {
	fx := newSetupFixture(t)
	fx.place(t, fx.image)
	var out bytes.Buffer
	if err := fx.install(context.Background(), "", &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if fx.fetched != 0 || fx.execs != 0 || !strings.Contains(out.String(), "already installed") {
		t.Errorf("fetched %d, execs %d, %q", fx.fetched, fx.execs, out.String())
	}
}

// TestSandboxInstallSameVersionRewrites: the same MSI over a broken image needs REINSTALL,
// and another version must not get it, or it would install nothing.
func TestSandboxInstallSameVersionRewrites(t *testing.T) {
	for installed, want := range map[string]bool{"v0.2.0": true, "v0.2.0-rc.1": false, "v0.1.9": false, "": false} {
		fx := newSetupFixture(t)
		fx.msi.Name = "hyperbutler-v0.2.0-sandbox-windows-amd64.msi"
		fx.place(t, []byte("old"))
		fx.product = func() machineProduct {
			return machineProduct{installDir: fx.exeDir, service: true, sandboxVersion: installed}
		}
		if err := fx.install(context.Background(), "", io.Discard, io.Discard); err != nil {
			t.Fatal(err)
		}
		if fx.reinst != want {
			t.Errorf("installed %q: reinstall %v", installed, fx.reinst)
		}
	}
}

func TestSandboxInstallRefuses(t *testing.T) {
	for name, c := range map[string]struct {
		change func(*setupFixture)
		is     error
		text   string
	}{
		"no manifest pin": {func(fx *setupFixture) { fx.manifestPin = "" }, errSandboxDevBuild, ""},
		"no msi pin":      {func(fx *setupFixture) { fx.msiErr = sandboxmsi.ErrNoPin }, errSandboxDevBuild, ""},
		"malformed pin":   {func(fx *setupFixture) { fx.msiErr = errors.New("malformed") }, nil, "malformed"},
		"zip, no main product": {func(fx *setupFixture) {
			fx.product = func() machineProduct { return machineProduct{} }
		}, errSandboxNeedsMSI, ""},
		"zip beside an installed product": {func(fx *setupFixture) {
			fx.product = func() machineProduct {
				return machineProduct{installDir: `C:\Program Files\hyperbutler\`, service: true}
			}
		}, errSandboxNeedsMSI, ""},
		"no service": {func(fx *setupFixture) {
			fx.product = func() machineProduct { return machineProduct{installDir: fx.exeDir} }
		}, errSandboxNeedsMSI, ""},
		"not a release": {func(fx *setupFixture) { fx.version = "(devel)" }, nil, "--msi"},
		"pseudo-version": {func(fx *setupFixture) {
			fx.version = "v0.1.2-0.20261107010101-abcdefabcdef"
		}, nil, "--msi"},
		"declined": {func(fx *setupFixture) { fx.execErr = sandboxmsi.ErrDeclined }, sandboxmsi.ErrDeclined, ""},
		"canceled": {func(fx *setupFixture) { fx.exit = 1602 }, sandboxmsi.ErrCanceled, ""},
		"busy":     {func(fx *setupFixture) { fx.exit = 1618 }, sandboxmsi.ErrBusy, ""},
		"failed":   {func(fx *setupFixture) { fx.exit = 1603 }, nil, "1603"},
		"wrong hash": {func(fx *setupFixture) {
			fx.msi.SHA256 = strings.Repeat("0", 64)
		}, sandboxmsi.ErrHash, ""},
	} {
		t.Run(name, func(t *testing.T) {
			fx := newSetupFixture(t)
			c.change(fx)
			var out bytes.Buffer
			err := fx.install(context.Background(), "", &out, io.Discard)
			if err == nil || (c.is != nil && !errors.Is(err, c.is)) || !strings.Contains(err.Error(), c.text) {
				t.Errorf("%v", err)
			}
			if strings.Contains(out.String(), "is installed") {
				t.Errorf("says installed: %q", out.String())
			}
		})
	}
}

func TestSandboxInstallLocalFile(t *testing.T) {
	fx := newSetupFixture(t)
	fx.version = "(devel)" // no download possible, as in a snapshot build
	src := filepath.Join(t.TempDir(), "given.msi")
	if err := os.WriteFile(src, []byte("the msi"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fx.install(context.Background(), src, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if fx.fetched != 0 || fx.execs != 1 {
		t.Errorf("fetched %d, execs %d", fx.fetched, fx.execs)
	}
	// Only the hash lets a given file through.
	fx = newSetupFixture(t)
	if err := os.WriteFile(src, []byte("evil 1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fx.install(context.Background(), src, io.Discard, io.Discard); !errors.Is(err, sandboxmsi.ErrHash) || fx.execs != 0 {
		t.Errorf("%v, execs %d", err, fx.execs)
	}
}

func TestSandboxEnsure(t *testing.T) {
	ctx := context.Background()
	t.Run("right image asks nothing", func(t *testing.T) {
		fx := newSetupFixture(t)
		fx.place(t, fx.image)
		var out bytes.Buffer
		if err := fx.ensure(ctx, strings.NewReader(""), &out, io.Discard); err != nil || out.Len() != 0 {
			t.Errorf("%v %q", err, out.String())
		}
	})
	t.Run("development rules apply to a build with no pin", func(t *testing.T) {
		fx := newSetupFixture(t)
		fx.manifestPin = ""
		fx.console = func(io.Reader) bool { t.Error("asked"); return false }
		if err := fx.ensure(ctx, strings.NewReader(""), io.Discard, io.Discard); err != nil {
			t.Error(err)
		}
	})
	t.Run("a developer's image is left alone", func(t *testing.T) {
		fx := newSetupFixture(t)
		fx.custom = true
		if err := fx.ensure(ctx, strings.NewReader(""), io.Discard, io.Discard); err != nil {
			t.Error(err)
		}
	})
	t.Run("redirected stdin prints the command and fails", func(t *testing.T) {
		fx := newSetupFixture(t)
		fx.console = func(io.Reader) bool { return false }
		err := fx.ensure(ctx, strings.NewReader("y\n"), io.Discard, io.Discard)
		if err == nil || !strings.Contains(err.Error(), sandboxInstallCommand) || fx.fetched != 0 {
			t.Errorf("%v, fetched %d", err, fx.fetched)
		}
	})
	for answer, wantInstall := range map[string]bool{"y\n": true, "YES\r\n": true, "n\n": false, "\n": false, "": false, "maybe\n": false} {
		t.Run(fmt.Sprintf("answer %q", answer), func(t *testing.T) {
			fx := newSetupFixture(t)
			var out bytes.Buffer
			err := fx.ensure(ctx, strings.NewReader(answer), &out, io.Discard)
			if !strings.Contains(out.String(), "KiB") && !strings.Contains(out.String(), " B)") {
				t.Errorf("the question names no size: %q", out.String())
			}
			if wantInstall {
				if err != nil || fx.fetched != 1 || fx.execs != 1 {
					t.Errorf("%v, fetched %d, execs %d", err, fx.fetched, fx.execs)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), sandboxInstallCommand) || fx.fetched != 0 {
				t.Errorf("%v, fetched %d", err, fx.fetched)
			}
		})
	}
	t.Run("a mismatch says so", func(t *testing.T) {
		fx := newSetupFixture(t)
		fx.place(t, []byte("old"))
		var out bytes.Buffer
		_ = fx.ensure(ctx, strings.NewReader("n\n"), &out, io.Discard)
		if !strings.Contains(out.String(), "does not match") {
			t.Errorf("%q", out.String())
		}
	})
	t.Run("a zip explains and does not ask", func(t *testing.T) {
		fx := newSetupFixture(t)
		fx.product = func() machineProduct { return machineProduct{} }
		var out bytes.Buffer
		err := fx.ensure(ctx, strings.NewReader("y\n"), &out, io.Discard)
		if !errors.Is(err, errSandboxNeedsMSI) || out.Len() != 0 || fx.fetched != 0 {
			t.Errorf("%v %q", err, out.String())
		}
	})
}

func TestSandboxBootError(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "image.json")
	good := []byte("{}")
	pin := sum(good)
	boom := errors.New("boom")
	refused := fmt.Errorf("engine: the broker did not start the VM: %w", &broker.ReplyError{Code: proto.CodeImageRejected})
	otherRefusal := fmt.Errorf("engine: %w", &broker.ReplyError{Code: proto.CodeDiskRejected})

	friendly := func(err error) bool { return err != nil && strings.Contains(err.Error(), sandboxInstallCommand) }
	if err := sandboxBootError(refused, false, manifest, ""); !friendly(err) || strings.Contains(err.Error(), "did not start") {
		t.Errorf("broker refusal: %v", err)
	}
	if err := sandboxBootError(refused, true, manifest, pin); !errors.Is(err, refused) || friendly(err) {
		t.Errorf("a developer's image keeps its own error: %v", err)
	}
	if err := sandboxBootError(boom, false, manifest, pin); !friendly(err) {
		t.Errorf("no image, in this process: %v", err)
	}
	if err := os.WriteFile(manifest, good, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := sandboxBootError(boom, false, manifest, pin); !errors.Is(err, boom) {
		t.Errorf("the image is right, so it is about something else: %v", err)
	}
	if err := sandboxBootError(otherRefusal, false, manifest, pin); !errors.Is(err, otherRefusal) {
		t.Errorf("another refusal: %v", err)
	}
	if err := sandboxBootError(nil, false, manifest, pin); err != nil {
		t.Errorf("no error: %v", err)
	}
}

func TestSandboxImageCheck(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "image.json")
	good := []byte("{}")
	pin := sum(good)
	if _, ok := sandboxImageCheck(manifest, "", true); ok {
		t.Error("a development build has no line")
	}
	if _, ok := sandboxImageCheck(manifest, pin, false); ok {
		t.Error("a zip without the image has no line")
	}
	if c, ok := sandboxImageCheck(manifest, pin, true); !ok || c.Status != doctorInfo {
		t.Errorf("not installed: %+v %v", c, ok)
	}
	if err := os.WriteFile(manifest, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, ok := sandboxImageCheck(manifest, pin, false); !ok || c.Status != doctorWarn || !strings.Contains(c.Fix, sandboxInstallCommand) {
		t.Errorf("mismatch: %+v %v", c, ok)
	}
	if err := os.WriteFile(manifest, good, 0o600); err != nil {
		t.Fatal(err)
	}
	if c, ok := sandboxImageCheck(manifest, pin, true); !ok || c.Status != doctorOK {
		t.Errorf("match: %+v %v", c, ok)
	}
}

func TestSandboxProductNote(t *testing.T) {
	if n := sandboxProductNote(""); n != "" {
		t.Errorf("%q", n)
	}
	if n := sandboxProductNote("v0.2.0"); !strings.Contains(n, "v0.2.0") || !strings.Contains(n, "Apps") {
		t.Errorf("%q", n)
	}
}

// TestUninstallSandbox: uninstall takes a download a killed install left, and the state
// directory with it, and says what it leaves to Apps.
func TestUninstallSandbox(t *testing.T) {
	home, dockerCfg := testHome(t)
	fakeInstall(t, home, home, dockerCfg)
	dead := filepath.Join(home, sandboxMSIDirName, "dead-x.msi")
	writeFile(t, dead, "x")
	record := filepath.Join(home, "sandbox", "record.json")
	writeFile(t, record, "{}")
	saved := installedProduct
	t.Cleanup(func() { installedProduct = saved })
	installedProduct = func() machineProduct { return machineProduct{sandboxVersion: "v0.2.0"} }

	r := uninstallJSON(t, "--dry-run")
	if !strings.Contains(strings.Join(r.Notes, "\n"), "hyperbutler agent sandbox") {
		t.Errorf("no note: %q", r.Notes)
	}
	r = uninstallJSON(t, "--yes")
	if len(r.Failed) != 0 {
		t.Fatalf("failed: %+v", r.Failed)
	}
	for _, p := range []string{dead, record, home} {
		if exists(p) {
			t.Errorf("%s is still there; kept %+v", p, r.Kept)
		}
	}
	installedProduct = func() machineProduct { return machineProduct{} }
	if r := uninstallJSON(t, "--dry-run"); strings.Contains(strings.Join(r.Notes, "\n"), "hyperbutler agent sandbox") {
		t.Errorf("a note without the product: %q", r.Notes)
	}
}

// The wxs is the contract of what `hyperbutler sandbox install` needs: its name, where it
// looks, what it records.
func TestSandboxWXS(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "dist", "msi", "sandbox.wxs"))
	if err != nil {
		t.Fatal(err)
	}
	main, err := os.ReadFile(filepath.Join("..", "..", "dist", "msi", "hyperbutler.wxs"))
	if err != nil {
		t.Fatal(err)
	}
	wxs := string(b)
	for _, want := range []string{
		`Name="hyperbutler agent sandbox"`,
		`Name="` + sandboxImageDirName + `"`,
		`Key="` + hbRegKey + `" Name="` + regInstallDir + `"`,
		`Name="` + regSandboxVersion + `"`,
		`Name="` + regSandboxProductCode + `"`,
		`Id="MSIRESTARTMANAGERCONTROL" Value="Disable"`,
		`AllowSameVersionUpgrades="yes"`,
		`Schedule="afterInstallInitialize"`,
		// The same read access for VMs as the main MSI's image folder (vm.VMGroupCanRead).
		`(A;OICI;0x1200a9;;;S-1-5-83-0)`,
	} {
		if !strings.Contains(wxs, want) {
			t.Errorf("sandbox.wxs has no %s", want)
		}
	}
	// A standard user may start a repair, whose SYSTEM part must not take a folder from them.
	if strings.Contains(wxs, `Id="HBMAINDIR" Secure`) {
		t.Error("HBMAINDIR is a Secure property")
	}
	sddl := `Sddl="D:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;0x1200a9;;;BU)(A;OICI;0x1200a9;;;S-1-5-83-0)"`
	if !strings.Contains(wxs, sddl) || !strings.Contains(string(main), sddl) {
		t.Errorf("the two image folders' access differs: want %s in both", sddl)
	}
	if strings.Contains(string(main), `Name="sandbox-image"`) {
		t.Error("the main MSI must not carry the sandbox image")
	}
	// The main MSI's uninstall refuses while the sandbox product is there.
	actions, _, _ := readWXS(t)
	const want = `REMOVE~="ALL" AND NOT UPGRADINGPRODUCTCODE AND HBSANDBOXPRODUCT AND NOT IGNORESANDBOX`
	if c := actions["HbRefuseUninstallSandbox"]; c == nil || c.condition != want {
		t.Errorf("HbRefuseUninstallSandbox: %+v, want %s", c, want)
	}
}
