# Contributing to hyperbutler

Thanks for your interest. hyperbutler is young and small, so a short issue before a large
change saves everyone time: say what you want to change and why, and we agree on the shape
before you write the code.

Bugs, ideas and questions go in [issues](https://github.com/Higangssh/hyperbutler/issues/new/choose),
which has a form for each.
Security problems do not: report them privately as [SECURITY.md](SECURITY.md) describes.

## Where to start

Issues labeled [`good first issue`](https://github.com/Higangssh/hyperbutler/labels/good%20first%20issue)
are small and self-contained; [`help wanted`](https://github.com/Higangssh/hyperbutler/labels/help%20wanted)
are larger but open to anyone. Comment on an issue before you start so two people don't
do the same work.

Help that needs no deep knowledge of the engine and is very welcome:

- **Testing on machines we don't have.** Windows 11 Pro and Enterprise, corporate
  proxies, VPNs, Docker Desktop installed next to hyperbutler, JetBrains IDEs,
  Testcontainers, VS Code Dev Containers. A report that says what you ran, on which
  Windows build (`winver`), and what happened is a contribution on its own.
- **GUI translations.** The window's text is in `internal/gui/i18n/messages/<lang>.json`;
  [docs/gui.md](docs/gui.md#languages-req-ux-013) describes the format. Open an issue
  for a new language first, since it also needs its plural forms and the settings value.
- **Docs.** Anything in [docs/](docs/) that was unclear to you, wrong, or missing.

Some areas guard the security boundary, and changes there need an issue and a design
discussion before any code, and an adversarial review before merge:

- the broker (`internal/broker`, `cmd/hyperbutler-broker`), the only process that calls HCS
- the VM template and the HCS calls (`internal/vmtemplate`, `internal/vm`)
- the agent sandbox's copy, review, apply and wire format (`internal/sandbox`, `guest/cmd/sbxd`)
- the guest image's pinned inputs (`guest/prototype/sources.lock`, `guest/sandbox/debian.lock`)
- file sharing and bind-mount policy (`internal/share`, `guest/internal/fsguard`)

## Development setup

- Windows 11 (Home, Pro or Enterprise) on x64. Releases are x64 only; the code is
  vetted for windows/arm64 in CI but nobody has run it there yet.
- Virtualization enabled in firmware, plus the **Virtual Machine Platform** and
  **Windows Hypervisor Platform** optional features (`optionalfeatures.exe`, then reboot)
- Go (the version in `go.mod`), Git

```powershell
go build ./...
go test ./...
```

`go generate ./cmd/hyperbutler` adds the icon and version resource to `hyperbutler.exe`
(it runs the pinned `go-winres`; the generated `.syso` files are git-ignored). Builds work
without it, just with no icon. Release builds stamp the tag's version in `dist/package.ps1`.

### Keep your own installation out of it

A development build should never touch the hyperbutler or Docker you use day to day. Point
it at a scratch directory and its own pipe:

```powershell
$env:HYPERBUTLER_HOME = "$env:TEMP\hb-dev\home"
$env:DOCKER_CONFIG    = "$env:TEMP\hb-dev\docker"
$env:HYPERBUTLER_NO_LAUNCHER = "1"
.\hyperbutler.exe start --pipe \\.\pipe\hb_dev --no-context
docker -H npipe:////./pipe/hb_dev info
```

Tests must not listen on `0.0.0.0` or `::`; bind to `127.0.0.1`.

### Guest images

The Docker VM's image is built by `guest/prototype/build.ps1`, the agent sandbox's by
`guest/sandbox/build.ps1`. Both run in a digest-pinned container and need a Docker
engine (hyperbutler itself works). Images you build locally are for testing only; release
images are built only in CI.

Please state your Windows edition and build (`winver`) in bug reports and in PRs that touch
VM code. Behavior differs between Home and Pro.

## Commit messages

```
area: what the change does, lower case, no period

Why the change is needed and what it does, wrapped at about 72
columns. Say what was measured or tested when it matters.

Refs: REQ-DISK-001
Signed-off-by: Your Name <you@example.com>
```

- **area** is the package or part the change is about: `broker`, `engine`, `vmtemplate`,
  `sandbox`, `gui`, `guest`, `mcp`, `docs`, `ci`, ... `git log --oneline` shows plenty.
- Write the summary as what the change does, for example
  `broker: refuse a data disk that is a differencing VHDX`.
- One logical change per commit. Name requirement IDs (`REQ-...`) the change addresses.

### Developer Certificate of Origin

Every commit must be signed off (`git commit -s`), which certifies the
[DCO](https://developercertificate.org/): you wrote the change or have the right to submit it
under the project's license (Apache-2.0).

Code written with the help of AI tools is fine. You are responsible for it like any other
code you send: understand it, test it, and sign it off yourself.

## Pull requests

- Keep PRs focused. Link the issue and any requirement IDs (`REQ-...`) it addresses.
- CI must pass. It runs mostly on `windows-latest` (windows/amd64) with the Go version
  from `go.mod`, and is defined in `.github/workflows/ci.yml`:
  - `go vet ./...` and a `gofmt -l` check, plus `go vet` cross-compiled for windows/arm64,
    linux/amd64, and linux/arm64 (platforms with no packages yet are skipped)
  - `go test -race ./...` (this also runs every fuzz seed corpus as a regular test)
  - fuzz smoke: every `Fuzz*` target in every package is fuzzed for 10 seconds
    (`scripts/ci/fuzz-all.ps1`). `fuzz-weekly.yml` runs the same script for 1 minute per
    target once a week, or for any time you pass when started by hand. Crashing inputs are
    uploaded in the `fuzz-crashers` artifact (with the rest of `testdata/fuzz/`). Commit
    the crasher under `testdata/fuzz/` with the fix. When you add a Fuzz target, raise
    `FUZZ_MIN_TARGETS` in both workflows so the job fails if a target goes missing.
  - `golangci-lint` with the v2 config in `.golangci.yml`, including its gofmt/goimports
    formatter check
  - `govulncheck ./...`
  - on `ubuntu-latest`: `go test -race` for the Linux-only guest packages (`./guest/...`)
  - on `ubuntu-latest`: the canonical guest image build (`guest-image.yml`), the same one
    the release uses. It runs in a digest-pinned Alpine container, checks every input in
    `guest/prototype/sources.lock` against its SHA-256 pin, and checks every Alpine
    package's signature with `apk verify`. The image is uploaded as the `guest-image`
    artifact.

  Tests tagged `integration` are never run in CI because they need a hypervisor. Run them
  locally with `go test -tags integration ./...` and say in the PR which Windows edition
  you used. There is no windows/arm64 test job yet.

  Releases are built by `.github/workflows/release.yml` from a version tag; the checklist
  and the gates are in [docs/release.md](docs/release.md). To try the packaging locally
  with an image from `guest/prototype/build.ps1` (the result is marked as a snapshot, not
  for redistribution):

  ```powershell
  powershell -ExecutionPolicy Bypass -File guest\prototype\build.ps1 -Out $env:TEMP\guest
  powershell -ExecutionPolicy Bypass -File dist\package.ps1 -ImageDir $env:TEMP\guest -Snapshot
  ```

  It builds the bundled docker CLI and compose from `dist/clients.lock`, which needs the
  Go module proxy (or a filled module cache); `-NoClients` leaves them out.

  To run the same checks locally:

  ```powershell
  go vet ./...
  gofmt -l .
  go test -race ./...    # needs cgo and gcc on PATH; drop -race without them
  golangci-lint config verify
  golangci-lint run ./...
  golangci-lint fmt --diff ./...
  go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...   # version pinned in ci.yml
  ```
- Include benchmark numbers (from `bench/`) for changes that claim a performance effect.
- New dependencies need a reason in the PR description and a compatible license.
- A maintainer reviews every PR. Changes in the areas listed under
  [Where to start](#where-to-start) as guarding the security boundary also get an
  adversarial review: someone tries to break the change on purpose, and the PR records
  each finding and what happened to it.

## Testing expectations

- **Unit tests** for all new logic.
- **Fuzz tests** for every parser, decoder, and protocol handler. This includes wire framing,
  guest agent messages, config files, and port/mount specs. Commit useful corpus entries under
  `testdata/fuzz/`.
- **Race detector** for concurrent code.
- **Integration tests** that need a real hypervisor use the `integration` build tag.
- Bug fixes include a test that fails without the fix.

## Design decisions

Significant choices (VM backend, file-sharing approach, guest image, protocols) are written
up as ADRs in `docs/adr/` using the template there. Propose one in a PR. It's fine for
the PR to contain only the ADR. Requirements are in [docs/requirements.md](docs/requirements.md).

## Code style

- `gofmt`/`goimports`, and `golangci-lint` must be clean.
- Return errors with context (`fmt.Errorf("start vm: %w", err)`). Don't panic in library code.
- Windows-only code goes in `_windows.go` files. Keep packages buildable on Linux where
  possible so guest-side code and tests run there too.
- Comments say why, in full sentences. User-facing text says what happened and what to do.
- The module path matters to package initialization: `internal/mcpenv` must initialize
  before the MCP SDK (`github.com/modelcontextprotocol/go-sdk`), which panics on a
  malformed `MCPGODEBUG`, and Go initializes ready packages in import path order. Renaming
  the module to a path that sorts after `github.com/modelcontextprotocol` breaks that, and
  every hyperbutler command would crash with a stray `MCPGODEBUG=x`. The subprocess tests
  `TestMalformedMCPGODEBUG` (`cmd/hyperbutler`) and `TestMalformedMCPGODEBUGSubprocess`
  (`internal/mcpserver`, every platform) fail if it happens; do not skip them.

## Code of Conduct

This project follows the [Code of Conduct](CODE_OF_CONDUCT.md).
