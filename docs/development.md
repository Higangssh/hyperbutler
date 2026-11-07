# Development

How to build, run and test hyperbutler from source. [CONTRIBUTING.md](../CONTRIBUTING.md)
says how to send a change.

## Requirements

- Windows 11 (Home, Pro or Enterprise) on x64. Releases are x64 only; CI vets the code
  for windows/arm64, but nobody has run it there yet.
- Virtualization enabled in firmware, plus the **Virtual Machine Platform** and
  **Windows Hypervisor Platform** optional features (`optionalfeatures.exe`, then reboot)
- Go (the version in `go.mod`), Git
- For `go test -race`: cgo and gcc on `PATH`

```powershell
go build ./...
go test ./...
```

`go generate ./cmd/hyperbutler` adds the icon and version resource to `hyperbutler.exe`
(it runs the pinned `go-winres`; the generated `.syso` files are git-ignored). Builds work
without it, just with no icon. Release builds stamp the tag's version in `dist/package.ps1`.

## Run a development build

A development build should never touch the hyperbutler or Docker you use day to day. Give
it its own home, Docker config and pipe:

```powershell
$env:HYPERBUTLER_HOME = "$env:TEMP\hb-dev\home"
$env:DOCKER_CONFIG    = "$env:TEMP\hb-dev\docker"
$env:HYPERBUTLER_NO_LAUNCHER = "1"
go build -o $env:TEMP\hb-dev\hyperbutler.exe ./cmd/hyperbutler
& $env:TEMP\hb-dev\hyperbutler.exe start --pipe \\.\pipe\hb_dev --no-context
docker -H npipe:////./pipe/hb_dev info
```

`--no-context` leaves your `docker context` alone; `HYPERBUTLER_NO_LAUNCHER=1` turns off
the double-click launcher.

## Guest images

- The Docker VM's image: `guest/prototype/build.ps1`. Its inputs are pinned by SHA-256 in
  `guest/prototype/sources.lock`.
- The agent sandbox's image: `guest/sandbox/build.ps1`, a Debian system pinned to a
  snapshot in `guest/sandbox/debian.lock` (ADR 0009).

Both build inside a digest-pinned container and need a Docker engine (hyperbutler itself
works). Images you build locally are for testing only; release images are built only in
CI (`.github/workflows/guest-image.yml` and `sandbox-image.yml`).

## Tests

- `go test ./...` runs the unit tests and every fuzz seed corpus.
- `go test -race ./...` adds the race detector.
- Tests tagged `integration` need a real hypervisor and never run in CI. Run them with
  `go test -tags integration ./...` and say in the PR which Windows edition you used.
- Fuzz a target with `go test -fuzz=FuzzName ./path/to/package`. Commit inputs that
  found a bug under that package's `testdata/fuzz/`, together with the fix. When you add a
  `Fuzz*` target, raise `FUZZ_MIN_TARGETS` in `ci.yml` and `fuzz-weekly.yml`, so CI fails
  if a target goes missing.

## Run the checks locally

```powershell
go vet ./...
gofmt -l .
go test -race ./...
golangci-lint config verify
golangci-lint run ./...
golangci-lint fmt --diff ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...   # version pinned in ci.yml
```

## What CI runs

`.github/workflows/ci.yml`, mostly on `windows-latest` with the Go version from `go.mod`:

- `go vet ./...` and a `gofmt -l` check, plus `go vet` cross-compiled for windows/arm64,
  linux/amd64 and linux/arm64
- `go test -race ./...`
- fuzz smoke: every `Fuzz*` target for 10 seconds (`scripts/ci/fuzz-all.ps1`).
  `fuzz-weekly.yml` runs each for 1 minute once a week. Crashing inputs are uploaded as the
  `fuzz-crashers` artifact.
- `golangci-lint` with the v2 config in `.golangci.yml`, including its formatter check
- `govulncheck ./...`
- on `ubuntu-latest`: `go test -race ./guest/...` for the Linux-only guest packages
- on `ubuntu-latest`: the canonical guest image build (`guest-image.yml`), the same one
  the release uses. It runs in a digest-pinned Alpine container, checks every input
  against its pin and every Alpine package's signature, and uploads the `guest-image`
  artifact.

There is no windows/arm64 test job yet.

## Packaging

Releases are built by `.github/workflows/release.yml` from a version tag; the checklist and
the gates are in [release.md](release.md). To try the packaging locally with an image from
`guest/prototype/build.ps1` (the result is marked as a snapshot, not for redistribution):

```powershell
powershell -ExecutionPolicy Bypass -File guest\prototype\build.ps1 -Out $env:TEMP\guest
powershell -ExecutionPolicy Bypass -File dist\package.ps1 -ImageDir $env:TEMP\guest -Snapshot
```

It builds the bundled docker CLI, compose and buildx from `dist/clients.lock`, which needs the Go
module proxy (or a filled module cache); `-NoClients` leaves them out. `-SandboxImageDir` with an
image from `guest/sandbox/build.ps1` and `-Wix` with WiX 5's `wix.exe` also build the agent
sandbox image's MSI and pin it ([install.md](install.md#the-agent-sandbox-image)).

## The module path and MCPGODEBUG

`internal/mcpenv` must initialize before the MCP SDK
(`github.com/modelcontextprotocol/go-sdk`), which panics on a malformed `MCPGODEBUG`, and Go
initializes ready packages in import path order. Renaming the module to a path that sorts
after `github.com/modelcontextprotocol` breaks that, and every hyperbutler command would
crash with a stray `MCPGODEBUG=x`. The subprocess tests `TestMalformedMCPGODEBUG`
(`cmd/hyperbutler`) and `TestMalformedMCPGODEBUGSubprocess` (`internal/mcpserver`, every
platform) fail if it happens; do not skip them.
