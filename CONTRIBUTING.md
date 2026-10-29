# Contributing to winbox

Thanks for your interest! winbox is early-stage, so please open an issue to discuss larger
changes before writing code.

## Development setup

- Windows 11 (Home, Pro, or Enterprise), x64 or ARM64
- Virtualization enabled in firmware, plus the **Virtual Machine Platform** and
  **Windows Hypervisor Platform** optional features (`optionalfeatures.exe`, then reboot)
- Go (version in `go.mod` once it exists), Git

```powershell
go build ./...
go test ./...
```

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
- CI must pass. CI is being set up along with the first Go code. It will run build, `go vet`,
  `golangci-lint`, tests with `-race` (windows/amd64 and linux; `-race` isn't supported on
  windows/arm64), a short fuzz run, and `govulncheck`.
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
