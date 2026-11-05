package review

import (
	"path"
	"strings"
)

// Risk says why a change is listed ahead of ordinary edits: applying it can run code, or
// point a tool at a server, the next time the user builds, tests, opens the project or
// runs git. An empty Risk is an ordinary edit.
type Risk string

const (
	RiskGit      Risk = "git configuration"
	RiskCI       Risk = "CI workflow"
	RiskEditor   Risk = "editor tasks or settings"
	RiskBuild    Risk = "build or test runner configuration"
	RiskPackage  Risk = "package manifest or registry settings"
	RiskEnv      Risk = "environment or tool versions"
	RiskHooks    Risk = "commit hooks"
	RiskExec     Risk = "executable or shortcut"
	RiskSecret   Risk = "looks like a secret"
	RiskAgent    Risk = "instructions or settings for the next coding agent"
	RiskGitInner Risk = "inside .git (never applied)"
)

// riskyNames are file names (compared without case) and what applying them can do.
var riskyNames = map[string]Risk{
	".gitattributes": RiskGit, ".gitmodules": RiskGit, ".lfsconfig": RiskGit,
	".gitlab-ci.yml": RiskCI, "azure-pipelines.yml": RiskCI, "jenkinsfile": RiskCI,
	".travis.yml": RiskCI, "bitbucket-pipelines.yml": RiskCI, "appveyor.yml": RiskCI,
	"makefile": RiskBuild, "gnumakefile": RiskBuild, "justfile": RiskBuild,
	"taskfile.yml": RiskBuild, "cmakelists.txt": RiskBuild, "build.gradle": RiskBuild,
	"build.gradle.kts": RiskBuild, "settings.gradle": RiskBuild, "pom.xml": RiskBuild,
	"build.rs": RiskBuild, "conftest.py": RiskBuild, "pytest.ini": RiskBuild,
	"tox.ini": RiskBuild, "noxfile.py": RiskBuild, "setup.py": RiskBuild,
	"package.json": RiskPackage, ".npmrc": RiskPackage, ".yarnrc": RiskPackage,
	".yarnrc.yml": RiskPackage, ".pnpmfile.cjs": RiskPackage, "pyproject.toml": RiskPackage,
	"setup.cfg": RiskPackage, "pip.conf": RiskPackage, ".pypirc": RiskPackage,
	"cargo.toml": RiskPackage, "go.mod": RiskPackage, "gemfile": RiskPackage,
	"composer.json": RiskPackage, "nuget.config": RiskPackage, "directory.build.props": RiskBuild,
	"directory.build.targets": RiskBuild,
	".envrc":                  RiskEnv, ".tool-versions": RiskEnv, ".node-version": RiskEnv,
	".nvmrc": RiskEnv, ".python-version": RiskEnv, ".ruby-version": RiskEnv,
	"rust-toolchain": RiskEnv, "rust-toolchain.toml": RiskEnv, "mise.toml": RiskEnv,
	".pre-commit-config.yaml": RiskHooks, "lefthook.yml": RiskHooks,
	"desktop.ini": RiskExec, "autorun.inf": RiskExec,
	"dockerfile": RiskBuild, "containerfile": RiskBuild, "compose.yaml": RiskBuild, "compose.yml": RiskBuild,
	"docker-compose.yml": RiskBuild, "docker-compose.yaml": RiskBuild, ".dockerignore": RiskBuild,
	"agents.md": RiskAgent, ".cursorrules": RiskAgent, ".windsurfrules": RiskAgent,
	"copilot-instructions.md": RiskAgent,
}

// riskyDirs are directories (compared without case) whose every file is listed first.
var riskyDirs = map[string]Risk{
	".github/workflows": RiskCI, ".circleci": RiskCI, ".buildkite": RiskCI,
	".vscode": RiskEditor, ".idea": RiskEditor, ".devcontainer": RiskEditor,
	".husky": RiskHooks, ".githooks": RiskHooks, ".cargo": RiskPackage,
	".cursor": RiskAgent, ".codex": RiskAgent, ".gemini": RiskAgent, ".continue": RiskAgent,
}

// riskyExt are extensions that run when opened from Explorer, or tell a tool where to
// fetch from.
var riskyExt = map[string]Risk{
	".exe": RiskExec, ".dll": RiskExec, ".com": RiskExec, ".bat": RiskExec, ".cmd": RiskExec,
	".ps1": RiskExec, ".psm1": RiskExec, ".vbs": RiskExec, ".lnk": RiskExec,
	".url": RiskExec, ".scr": RiskExec, ".msi": RiskExec, ".hta": RiskExec, ".cpl": RiskExec,
	".jar": RiskExec, ".reg": RiskExec, ".appref-ms": RiskExec, ".library-ms": RiskExec,
	".scf": RiskExec, ".settingcontent-ms": RiskExec, ".wsf": RiskExec, ".wsh": RiskExec,
	".jse": RiskExec, ".vbe": RiskExec, ".psd1": RiskExec, ".msc": RiskExec, ".pif": RiskExec,
	".iso": RiskExec, ".img": RiskExec, ".vhd": RiskExec, ".vhdx": RiskExec, ".chm": RiskExec,
	".diagcab": RiskExec, ".search-ms": RiskExec, ".searchconnector-ms": RiskExec,
	".code-workspace": RiskEditor, ".sln": RiskBuild, ".csproj": RiskBuild,
	".vcxproj": RiskBuild, ".props": RiskBuild, ".targets": RiskBuild,
}

// Classify returns why a change to p (a path CheckPath accepted) is listed first, or ""
// for an ordinary edit. executable is whether the guest file has an execute bit and is
// new. Changes to a .git file or directory at any depth (a submodule's, or one the agent
// planted in a subdirectory, which git would honour there) are never applied; Classify
// says so rather than the apply step failing on them.
func Classify(p string, executable bool) Risk {
	low := strings.ToLower(p)
	for _, seg := range strings.Split(low, "/") {
		if seg == ".git" {
			return RiskGitInner
		}
	}
	for dir, r := range riskyDirs {
		if strings.HasPrefix(low, dir+"/") || strings.Contains(low, "/"+dir+"/") {
			return r
		}
	}
	name := path.Base(low)
	if r, ok := riskyNames[name]; ok {
		return r
	}
	if LooksLikeSecret(name) {
		return RiskSecret
	}
	if r := riskyExt[path.Ext(name)]; r != "" {
		return r
	}
	if executable {
		return RiskExec
	}
	return ""
}

// LooksLikeSecret reports whether a file name (lower-cased, no directory) is one of the
// names secrets are kept in. It is used both to keep such files out of a session
// (REQ-SBX-004) and to list a new one the agent wrote. It is a net, not a guarantee.
func LooksLikeSecret(name string) bool {
	switch name {
	case ".npmrc", ".pypirc", ".netrc", "_netrc", ".git-credentials", "kubeconfig",
		"credentials", "credentials.json", ".dockercfg", "secrets.yml", "secrets.yaml",
		"terraform.tfstate", "terraform.tfstate.backup":
		return true
	}
	if name == ".env" || strings.HasPrefix(name, ".env.") {
		return !strings.HasSuffix(name, ".example") && !strings.HasSuffix(name, ".sample") &&
			!strings.HasSuffix(name, ".template")
	}
	if strings.HasPrefix(name, "id_") && !strings.HasSuffix(name, ".pub") {
		return true
	}
	switch path.Ext(name) {
	case ".pem", ".key", ".pfx", ".p12", ".jks", ".keystore", ".tfstate", ".tfvars", ".kdbx",
		".ppk", ".asc", ".gpg":
		return true
	}
	return false
}
