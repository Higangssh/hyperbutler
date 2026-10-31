# Contributing to hyperbutler

Thanks for your interest! hyperbutler is early-stage, so please open an issue to discuss larger
changes before writing code.

## Development setup

- Windows 11 (Home, Pro, or Enterprise), x64 or ARM64
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

Please state your Windows edition and build (`winver`) in bug reports and in PRs that touch
VM backends. Behavior differs between Home and Pro.

## Commit messages

We use [Conventional Commits](https://www.conventionalcommits.org/):

```
<type>(<scope>): <summary in imperative mood, ≤ 72 chars>

<body: what and why, wrapped at 72 columns>

Refs: REQ-DISK-001
Signed-off-by: Your Name <you@example.com>
```

- **Types:** `feat`, `fix`, `perf`, `refactor`, `test`, `docs`, `build`, `ci`, `chore`
- **Scopes:** `backend`, `hcs`, `whp`, `qemu`, `guest`, `agent`, `transport`, `net`,
  `fsshare`, `disk`, `cli`, `bench`, `docs`
- Breaking changes: add `!` after the type (`feat(cli)!: ...`) and a `BREAKING CHANGE:` footer.
- One logical change per commit.

### Developer Certificate of Origin

Every commit must be signed off (`git commit -s`), which certifies the
[DCO](https://developercertificate.org/): you wrote the change or have the right to submit it
under the project's license.

## Pull requests

- Keep PRs focused. Link the issue and any requirement IDs (`REQ-...`) it addresses.
- CI must pass. It runs mostly on `windows-latest` (windows/amd64) with the Go version
  from `go.mod`, and is defined in `.github/workflows/ci.yml`:
  - `go vet ./...` and a `gofmt -l` check, plus `go vet` cross-compiled for windows/arm64,
    linux/amd64, and linux/arm64 (platforms with no packages yet are skipped)
  - `go test -race ./...` (this also runs every fuzz seed corpus as a regular test)
  - fuzz smoke: every `Fuzz*` target in every package is fuzzed for 30 seconds. Crashing
    inputs are uploaded in the `fuzz-crashers` artifact (with the rest of `testdata/fuzz/`).
    Commit the crasher under `testdata/fuzz/` with the fix. When you add a Fuzz target,
    raise `FUZZ_MIN_TARGETS` in `ci.yml` so the job fails if a target goes missing.
  - `golangci-lint` with the v2 config in `.golangci.yml`, including its gofmt/goimports
    formatter check
  - `govulncheck ./...`
  - on `ubuntu-latest`: `go test -race` for the Linux-only guest packages (`./guest/...`:
    `netfwd`, `hyperbutlerd`, `guest/internal/...`)
  - on `ubuntu-latest`: the canonical guest image build (`guest-image.yml`), the same one
    the release uses. It runs in a digest-pinned Alpine container, checks every input in
    `guest/prototype/sources.lock` against its SHA-256 pin, and checks every Alpine
    package's signature with `apk verify`. The image is uploaded as the `guest-image`
    artifact. Images for releases are built only there, never on a developer machine.

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
the PR to contain only the ADR.

## Code style

- `gofmt`/`goimports`, and `golangci-lint` must be clean.
- Return errors with context (`fmt.Errorf("start vm: %w", err)`). Don't panic in library code.
- Windows-only code goes in `_windows.go` files. Keep packages buildable on Linux where
  possible so guest-side code and tests run there too.

## Code of Conduct

This project follows the [Code of Conduct](CODE_OF_CONDUCT.md).
