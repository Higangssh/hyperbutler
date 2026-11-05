package review

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCheckPathAllows(t *testing.T) {
	for _, p := range []string{
		"main.go", "src/app/index.ts", ".github/workflows/ci.yml", "a b/c-d_e.f+g@h#i",
		"한글/파일.txt", "日本語.md", "Ünïcödé", "Москва/файл", "αβγ.txt", "x/.hidden", "v1.2.3/a", "CONSOLE.txt",
		"com10", "lpt", "nul_", "a..b", strings.Repeat("a", 255),
	} {
		if _, err := CheckPath(p); err != nil {
			t.Errorf("%q: %v", p, err)
		}
	}
}

func TestCheckPathRefuses(t *testing.T) {
	for _, p := range []string{
		"", "/etc/passwd", "a//b", "a/", "./a", "a/../b", "..", ".",
		`a\b`, "C:/x", "c:x", "a:stream", "file::$DATA", "a*", "a?", `a"b`, "a<b", "a>b", "a|b",
		"a.", "a ", " a", "dir./x", "CON", "con.txt", "Nul", "aux.tar.gz", "COM1", "lpt9.log",
		"COM¹", "CONIN$", "conout$.x", "PRN .txt",
		"a\x00b", "a\nb", "a\x7f", "\xff", "a\u202eb", "a\u200db", "a\u00a0b", "a\u3000b",
		"a\uf03ab", "e\u0301", "\u1112\u1161\u11ab", "\u212a", "\uff0e\uff0e", "a\u2215b",
		strings.Repeat("a", 256), strings.Repeat("a/", MaxDepth) + "a",
		strings.Repeat(strings.Repeat("b", 200)+"/", 6) + "c",
	} {
		if segs, err := CheckPath(p); err == nil {
			t.Errorf("%q accepted as %q", p, segs)
		} else if !errors.Is(err, ErrPath) {
			t.Errorf("%q: %v does not wrap ErrPath", p, err)
		}
	}
}

func TestEscape(t *testing.T) {
	for in, want := range map[string]string{
		"plain.go":   "plain.go",
		"한글":         "한글",
		`a\b`:        `a\\b`,
		"a\u202eb":   `a\u202eb`,
		"\x1b[31m":   `\u001b[31m`,
		"\xff":       `\xff`,
		"tab\there":  `tab\u0009here`,
		"nb\u00a0sp": `nb\u00a0sp`,
		"\U000e0041": `\U000e0041`,
	} {
		if got := Escape(in); got != want {
			t.Errorf("Escape(%q) = %q, want %q", in, got, want)
		}
	}
	if got := Shorten(strings.Repeat("x", 100), 10); got != "xxxxxxxxxx… (100 bytes)" {
		t.Errorf("Shorten: %q", got)
	}
}

func TestClassify(t *testing.T) {
	for p, want := range map[string]Risk{
		"src/main.go":                 ``,
		".git/hooks/pre-commit":       RiskGitInner,
		".GIT/config":                 RiskGitInner,
		"sub/.git/hooks/pre-commit":   RiskGitInner,
		"vendor/lib/.git":             RiskGitInner,
		"a/.Git/config":               RiskGitInner,
		"sub/.vscode/tasks.json":      RiskEditor,
		"pkg/.github/workflows/x.yml": RiskCI,
		"assets/desktop.ini":          RiskExec,
		"Dockerfile":                  RiskBuild,
		"docs/AGENTS.md":              RiskAgent,
		"x/evil.scf":                  RiskExec,
		".github/workflows/x.yml":     RiskCI,
		".vscode/tasks.json":          RiskEditor,
		"web/package.json":            RiskPackage,
		"Makefile":                    RiskBuild,
		".env":                        RiskSecret,
		".env.local":                  RiskSecret,
		".env.example":                ``,
		"deploy/id_ed25519":           RiskSecret,
		"deploy/id_ed25519.pub":       ``,
		"tools/run.PS1":               RiskExec,
		"docs/shortcut.lnk":           RiskExec,
		".husky/pre-push":             RiskHooks,
		"sub/.gitattributes":          RiskGit,
		"infra/prod.tfvars":           RiskSecret,
		"App.code-workspace":          RiskEditor,
		"README.md":                   ``,
		".github/ISSUE_TEMPLATE/x.y":  ``,
	} {
		if got := Classify(p, false); got != want {
			t.Errorf("Classify(%q) = %q, want %q", p, got, want)
		}
	}
	if Classify("bin/tool", true) != RiskExec {
		t.Error("a new executable is not listed")
	}
}

func FuzzCheckPath(f *testing.F) {
	for _, s := range []string{"a/b", "../x", "CON", "a\u202e", "한글/x", "a:b", "x.", "COM¹.txt"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, p string) {
		segs, err := CheckPath(p)
		if err != nil {
			return
		}
		if strings.Join(segs, "/") != p {
			t.Fatalf("%q split into %q", p, segs)
		}
		for _, s := range segs {
			if s == "" || s == "." || s == ".." || strings.ContainsAny(s, "\\:*?\"<>|\x00") ||
				strings.HasSuffix(s, ".") || strings.HasSuffix(s, " ") || isDeviceName(s) {
				t.Fatalf("%q accepted with segment %q", p, s)
			}
			for _, r := range s {
				if r < 0x20 || r == 0x7f || (r >= 0x80 && !allowedRune(r)) {
					t.Fatalf("%q accepted with %U", p, r)
				}
			}
		}
		// An accepted path shows unchanged: nothing in it needs escaping.
		if e := Escape(p); e != p {
			t.Fatalf("%q accepted but shown as %q", p, e)
		}
	})
}

func FuzzEscape(f *testing.F) {
	f.Add("a\u202eb\\c\xff")
	f.Fuzz(func(t *testing.T, s string) {
		e := Escape(s)
		if !utf8.ValidString(e) {
			t.Fatalf("Escape(%q) = %q is not valid UTF-8", s, e)
		}
		for _, r := range e {
			if r != ' ' && !shownAsIs(r) {
				t.Fatalf("Escape(%q) = %q shows %U", s, e, r)
			}
		}
	})
}
