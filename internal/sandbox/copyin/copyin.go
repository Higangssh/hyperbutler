// Package copyin decides which files of a project are copied into a sandbox session
// (ADR 0009, "Bringing the project in", REQ-SBX-004): the working tree git would track
// (or, outside a repository, every file), less files named like secrets and files that
// hold a known token format or a private key. A skipped file is listed with the reason
// and can be brought in with --include. The scan is a net, not a guarantee.
package copyin

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/Higangssh/hyperbutler/internal/sandbox/review"
)

// Default limits; above them the caller asks before copying.
const (
	DefaultMaxFile  = 256 << 20
	DefaultMaxTotal = 4 << 30
	// scanLimit is how much of each file the content scan reads.
	scanLimit = 8 << 20
	maxFiles  = 1_000_000
)

// Entry is one file to copy.
type Entry struct {
	Path string // '/'-separated, relative to the project
	Size int64
	// Link is the target of a symbolic link, copied as a link and never followed.
	Link string
}

// Skipped is a file left out, and why.
type Skipped struct {
	Path   string
	Reason string
}

// Plan is what a copy-in would do.
type Plan struct {
	Files   []Entry
	Skipped []Skipped
	Total   int64
	// TooBig lists files above the per-file limit; they are in Files, and the caller
	// asks before copying them.
	TooBig []Entry
	// FromGit says the list came from git (tracked files plus untracked ones git does
	// not ignore); otherwise every file below the directory was taken.
	FromGit bool
}

// Options are the limits and the files the user brought in by hand.
type Options struct {
	MaxFile int64
	// Include are project-relative paths copied even if they look like secrets.
	Include []string
	// Git is the git executable, or "" to look it up on PATH.
	Git string
}

// Select builds the plan for dir.
func Select(dir string, o Options) (Plan, error) {
	if o.MaxFile == 0 {
		o.MaxFile = DefaultMaxFile
	}
	include := map[string]bool{}
	for _, p := range o.Include {
		include[filepath.ToSlash(p)] = true
	}
	var p Plan
	names, unread, fromGit, err := list(dir, o.Git)
	if err != nil {
		return Plan{}, err
	}
	p.FromGit = fromGit
	p.Skipped = unread
	for _, rel := range names {
		// git reads names from the index, which a repository that came as an archive
		// may have crafted: a name must stay below dir.
		if rel == "" || path.Clean(rel) != rel || path.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, "../") ||
			strings.ContainsAny(rel, `\:`) {
			p.Skipped = append(p.Skipped, Skipped{rel, "a name outside the project"})
			continue
		}
		if inGitDir(rel) {
			continue // history comes in as a bundle
		}
		full := filepath.Join(dir, filepath.FromSlash(rel))
		fi, err := os.Lstat(full)
		if errors.Is(err, fs.ErrNotExist) {
			continue // deleted but still in the index
		}
		if err != nil {
			p.Skipped = append(p.Skipped, Skipped{rel, "unreadable: " + err.Error()})
			continue
		}
		switch m := fi.Mode(); {
		case m&fs.ModeSymlink != 0:
			t, err := os.Readlink(full)
			if err != nil {
				p.Skipped = append(p.Skipped, Skipped{rel, "unreadable link"})
				continue
			}
			p.Files = append(p.Files, Entry{Path: rel, Link: filepath.ToSlash(t)})
			continue
		case m&fs.ModeIrregular != 0 && isLink(full):
			p.Skipped = append(p.Skipped, Skipped{rel, "a junction or other link (not followed)"})
			continue
		case m&fs.ModeIrregular != 0:
			// Another reparse point: a cloud file placeholder (OneDrive), a deduplicated
			// file. It reads as the file it stands for.
			fi, err = os.Stat(full)
			if err != nil || !fi.Mode().IsRegular() {
				p.Skipped = append(p.Skipped, Skipped{rel, "unreadable reparse point"})
				continue
			}
		case !m.IsRegular():
			continue // a directory (a submodule's), a device
		}
		e := Entry{Path: rel, Size: fi.Size()}
		if !include[rel] {
			if review.LooksLikeSecret(strings.ToLower(path.Base(rel))) {
				p.Skipped = append(p.Skipped, Skipped{rel, "named like a secret"})
				continue
			}
			if why, err := scanFile(full); err != nil {
				p.Skipped = append(p.Skipped, Skipped{rel, "unreadable: " + err.Error()})
				continue
			} else if why != "" {
				p.Skipped = append(p.Skipped, Skipped{rel, "contains " + why})
				continue
			}
		}
		p.Files = append(p.Files, e)
		p.Total += e.Size
		if e.Size > o.MaxFile {
			p.TooBig = append(p.TooBig, e)
		}
	}
	return p, nil
}

// list returns the project's files, from git when dir is a work tree.
// In a work tree without git it fails rather than walking: a walk would take the files
// .gitignore keeps out, where secrets and build output usually are.
func list(dir, git string) ([]string, []Skipped, bool, error) {
	if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
		if git == "" {
			git, _ = exec.LookPath("git")
		}
		if git == "" {
			return nil, nil, false, errors.New("this is a git work tree and git is not installed: without it the files .gitignore keeps out cannot be told apart")
		}
		names, err := gitFiles(dir, git)
		if err != nil {
			return nil, nil, false, fmt.Errorf("list the files git tracks: %w", err)
		}
		return names, nil, true, nil
	}
	var names []string
	var unread []Skipped
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if err != nil {
			if p == dir {
				return err
			}
			unread = append(unread, Skipped{rel, "unreadable: " + err.Error()})
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if strings.EqualFold(d.Name(), ".git") {
				return filepath.SkipDir
			}
			return nil
		}
		if len(names) >= maxFiles {
			return fmt.Errorf("more than %d files", maxFiles)
		}
		names = append(names, rel)
		return nil
	})
	sort.Strings(names)
	return names, unread, false, err
}

// gitFiles lists tracked files and untracked files git does not ignore (.gitignore,
// .git/info/exclude and the global excludes file), without running any configured
// filter or hook: ls-files reads the index and the work tree only.
func gitFiles(dir, git string) ([]string, error) {
	cmd := exec.Command(git, "-c", "core.fsmonitor=false", "-C", dir, "ls-files", "-z", "--cached", "--others", "--exclude-standard", "--deduplicate") //nolint:gosec // G204: git (from PATH or the caller) with fixed arguments
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("%w: %s", err, bytes.TrimSpace(ee.Stderr))
		}
		return nil, err
	}
	var names []string
	for _, n := range bytes.Split(out, []byte{0}) {
		if len(n) == 0 {
			continue
		}
		if len(names) >= maxFiles {
			return nil, fmt.Errorf("more than %d files", maxFiles)
		}
		names = append(names, string(n))
	}
	sort.Strings(names)
	return names, nil
}

// Token formats with a published prefix, and private keys. Only formats precise enough
// not to fire on ordinary source code: a variable named password is not a secret, a
// ghp_ token is. internal/redact's list is wider because a log line is not source.
var secretFormats = []struct {
	name string
	re   *regexp.Regexp
}{
	{"a private key", regexp.MustCompile(`-----BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY-----`)},
	{"a GitHub token", regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{50,})`)},
	{"a GitLab token", regexp.MustCompile(`\bglpat-[A-Za-z0-9_\-]{20,}`)},
	{"a Docker Hub token", regexp.MustCompile(`\bdckr_pat_[A-Za-z0-9_\-]{20,}`)},
	{"an AWS access key", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{"a Slack token", regexp.MustCompile(`\bxox[abposr]-[0-9]{6,}-[A-Za-z0-9\-]{10,}`)},
	{"an Anthropic API key", regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_\-]{20,}`)},
	{"an OpenAI API key", regexp.MustCompile(`\bsk-(?:proj-|svcacct-|admin-)?[A-Za-z0-9_\-]{20,}T3BlbkFJ[A-Za-z0-9_\-]{20,}`)},
	{"a Google API key", regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}\b`)},
	{"a Stripe live key", regexp.MustCompile(`\b[rs]k_live_[0-9A-Za-z]{20,}`)},
	{"an npm token", regexp.MustCompile(`\bnpm_[A-Za-z0-9]{36}\b`)},
	{"a Hugging Face token", regexp.MustCompile(`\bhf_[A-Za-z0-9]{34,}\b`)},
	{"a service account key", regexp.MustCompile(`"type"\s*:\s*"service_account"`)},
}

// scanFile returns what secret format the start of the file holds, or "".
func scanFile(name string) (string, error) {
	f, err := os.Open(name) //nolint:gosec // G304: a file of the project the user chose to copy
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	return Scan(bufio.NewReaderSize(io.LimitReader(f, scanLimit), 64<<10))
}

// Scan returns the first secret format found in r, or "". Lines are scanned one at a
// time, up to 1 MiB each; a longer line is scanned in pieces that overlap.
func Scan(r io.Reader) (string, error) {
	const maxLine = 1 << 20
	const overlap = 256
	br := bufio.NewReaderSize(r, maxLine)
	var carry []byte
	for {
		line, err := br.ReadSlice('\n')
		if len(line) > 0 {
			buf := append(carry, line...)
			for _, f := range secretFormats {
				if f.re.Match(buf) {
					return f.name, nil
				}
			}
			carry = carry[:0]
			if errors.Is(err, bufio.ErrBufferFull) {
				carry = append(carry, buf[max(0, len(buf)-overlap):]...)
			}
		}
		switch {
		case err == nil, errors.Is(err, bufio.ErrBufferFull):
		case errors.Is(err, io.EOF):
			return "", nil
		default:
			return "", err
		}
	}
}

// inGitDir reports whether rel is a .git file or directory, or below one, at any depth
// and in any case.
func inGitDir(rel string) bool {
	for _, seg := range strings.Split(rel, "/") {
		if strings.EqualFold(seg, ".git") {
			return true
		}
	}
	return false
}
