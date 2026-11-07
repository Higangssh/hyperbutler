package leaktest

import (
	"go/build/constraint"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	VerifyTestMain(m)
}

// pending lists test packages that do not run the leak check yet, with the reason.
// Remove an entry when its package gets a TestMain; the test fails if an entry is
// stale, so this list can only shrink.
var pending = map[string]string{}

// verifyMain matches a TestMain whose first statement is the leak check, with no
// options other than the narrow, documented ones exported by this package.
var verifyMain = regexp.MustCompile(`func TestMain\(m \*testing\.M\) \{\s*leaktest\.VerifyTestMain\(m(, *leaktest\.Ignore[A-Za-z0-9]+)*\)`)

// selfMain is the same check inside this package, where the call is unqualified.
var selfMain = regexp.MustCompile(`func TestMain\(m \*testing\.M\) \{\s*VerifyTestMain\(m\)`)

type srcFile struct {
	dir, path, src string
	test           bool
}

func walkModule(t *testing.T) []srcFile {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root not found at %s: %v", root, err)
	}
	var files []srcFile
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || name == "testdata" || name == "vendor") {
				return filepath.SkipDir
			}
			// A module of its own (a bench's samples) cannot import this one's leaktest.
			if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, filepath.Dir(path))
		files = append(files, srcFile{
			dir: filepath.ToSlash(rel), path: path,
			src:  strings.ReplaceAll(string(raw), "\r", ""),
			test: strings.HasSuffix(path, "_test.go"),
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// neverBuiltInCI reports whether the file's //go:build line excludes it from every CI
// build (windows and linux, no extra tags such as integration).
func neverBuiltInCI(src string) bool {
	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(line, "package ") {
			break
		}
		if !constraint.IsGoBuild(line) {
			continue
		}
		expr, err := constraint.Parse(line)
		if err != nil {
			return false
		}
		// Excluded if no CI platform (with no extra tags) satisfies the expression.
		for _, goos := range []string{"windows", "linux"} {
			if expr.Eval(func(t string) bool { return t == goos || t == "amd64" || t == "arm64" || t == "cgo" }) {
				return false
			}
		}
		return true
	}
	return false
}

// Every directory with tests must call leaktest.VerifyTestMain from a TestMain in a
// file CI compiles, so a new package cannot skip guardrail 1 by omission.
func TestEveryTestPackageChecksLeaks(t *testing.T) {
	hasTests := map[string]bool{}
	checks := map[string]bool{}
	for _, f := range walkModule(t) {
		if !f.test {
			continue
		}
		// Integration-only test files still make the package need a leak check,
		// but a TestMain there never runs in CI and does not count.
		hasTests[f.dir] = true
		pattern := verifyMain
		if f.dir == "internal/leaktest" {
			pattern = selfMain
		}
		if !neverBuiltInCI(f.src) && pattern.MatchString(f.src) {
			checks[f.dir] = true
		}
	}
	if len(hasTests) < 10 {
		t.Fatalf("found only %d test packages; the walk is broken", len(hasTests))
	}
	for dir := range hasTests {
		_, isPending := pending[dir]
		switch {
		case checks[dir] && isPending:
			t.Errorf("%s now checks leaks; remove it from pending", dir)
		case !checks[dir] && !isPending:
			t.Errorf("%s has tests but no TestMain calling leaktest.VerifyTestMain (options: only leaktest.Ignore*)", dir)
		}
	}
	for dir := range pending {
		if !hasTests[dir] {
			t.Errorf("pending entry %s has no tests; remove it", dir)
		}
	}
}

// Only this package may use goleak, so every exception lives in one reviewed place,
// and goleak is never linked into a shipped binary.
func TestGoleakOnlyHere(t *testing.T) {
	fset := token.NewFileSet()
	for _, f := range walkModule(t) {
		af, err := parser.ParseFile(fset, f.path, f.src, parser.ImportsOnly)
		if err != nil {
			t.Errorf("%s: %v", f.path, err)
			continue
		}
		for _, imp := range af.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Errorf("%s: bad import %s", f.path, imp.Path.Value)
				continue
			}
			if path == "go.uber.org/goleak" && f.dir != "internal/leaktest" {
				t.Errorf("%s imports goleak directly; use internal/leaktest", f.path)
			}
			if !f.test && path == "github.com/Higangssh/hyperbutler/internal/leaktest" {
				t.Errorf("%s: non-test file imports internal/leaktest", f.path)
			}
		}
	}
}

func TestVerifyMainPattern(t *testing.T) {
	for _, ok := range []string{
		"func TestMain(m *testing.M) {\n\tleaktest.VerifyTestMain(m)\n}",
		"func TestMain(m *testing.M) {\n\tleaktest.VerifyTestMain(m, leaktest.IgnoreIOCP)\n}",
	} {
		if !verifyMain.MatchString(ok) {
			t.Errorf("rejected %q", ok)
		}
	}
	for _, bad := range []string{
		"func TestMain(m *testing.M) {\n\tgoleak.VerifyTestMain(m)\n}",
		"func TestMain(m *testing.M) {\n\tleaktest.VerifyTestMain(m, goleak.IgnoreCurrent())\n}",
		"func TestMain(m *testing.M) {\n\tleaktest.VerifyTestMain(m, leaktest.IgnoreIOCP, goleak.IgnoreTopFunction(\"x\"))\n}",
		"func TestMain(m *testing.M) {\n\tos.Exit(m.Run())\n}",
		// An unqualified call could be a local look-alike outside this package.
		"func TestMain(m *testing.M) {\n\tVerifyTestMain(m)\n}",
	} {
		if verifyMain.MatchString(bad) {
			t.Errorf("accepted %q", bad)
		}
	}
	if !neverBuiltInCI("//go:build integration && windows\n\npackage x") ||
		!neverBuiltInCI("//go:build windows && integration\npackage x") ||
		neverBuiltInCI("//go:build windows\npackage x") ||
		neverBuiltInCI("package x") {
		t.Error("neverBuiltInCI misclassifies build lines")
	}
}
