# Contributing to hyperbutler

Thank you for taking the time to contribute. Bug reports, testing on your machine,
translations, docs and code are all welcome, and you don't need to be a Hyper-V expert to
help.

By taking part you agree to follow our [Code of Conduct](CODE_OF_CONDUCT.md).

## Ways to contribute

### Report a bug

Search [existing issues](https://github.com/Higangssh/hyperbutler/issues) first. If it's
new, open a [bug report](https://github.com/Higangssh/hyperbutler/issues/new/choose) and
include:

- your Windows edition and build (`winver`); behavior differs between Home and Pro
- the hyperbutler version (`hyperbutler version`)
- what you ran, what you expected, and what happened
- the output of `hyperbutler doctor`, if the engine doesn't start

**Security vulnerabilities** must not be reported in public issues. Follow
[SECURITY.md](SECURITY.md) instead.

### Suggest a feature or ask a question

Open a [feature request or a question](https://github.com/Higangssh/hyperbutler/issues/new/choose).
For a feature, describe the problem you have before the solution you'd like; that makes
it easier to find the right fit.

### Test on your setup

We can't test every machine. Running hyperbutler on Windows 11 Pro or Enterprise, behind a
corporate proxy or VPN, next to Docker Desktop, or with JetBrains IDEs, Testcontainers or
VS Code Dev Containers, and telling us what worked and what didn't, is a real
contribution.

### Translate the GUI

The settings window's text lives in `internal/gui/i18n/messages/<lang>.json`. To improve an
existing language, send a PR. To add a new one, open an issue first; see
[docs/gui.md](docs/gui.md#languages-req-ux-013) for the format.

### Improve the docs

If something in the [README](README.md) or [docs/](docs/) was unclear or wrong for you,
a fix is very welcome, small ones included.

### Write code

Issues labeled
[`good first issue`](https://github.com/Higangssh/hyperbutler/labels/good%20first%20issue)
are a good place to start, and
[`help wanted`](https://github.com/Higangssh/hyperbutler/labels/help%20wanted) are open to
anyone. Leave a comment on the issue before you start, so nobody does the same work twice.

For anything larger than a small fix, please open an issue first and agree on the
approach. This matters most for the parts that guard the security boundary, which also get
an adversarial review before merge:

- the broker (`internal/broker`, `cmd/hyperbutler-broker`), the only process that calls HCS
- the VM template and HCS calls (`internal/vmtemplate`, `internal/vm`)
- the agent sandbox (`internal/sandbox`, `guest/cmd/sbxd`)
- the guest images' pinned inputs (`guest/prototype/sources.lock`, `guest/sandbox/debian.lock`)
- file sharing and bind mounts (`internal/share`, `guest/internal/fsguard`)

## Development setup

You need Windows 11 (Home, Pro or Enterprise) on x64 with virtualization enabled, the
**Virtual Machine Platform** and **Windows Hypervisor Platform** features, Go (the version
in `go.mod`) and Git.

```powershell
git clone https://github.com/Higangssh/hyperbutler
cd hyperbutler
go build ./...
go test ./...
```

[docs/development.md](docs/development.md) covers the rest: running a development build
without touching your own installation, building the guest images, integration and fuzz
tests, what CI runs, and how to run the same checks locally.

## Pull requests

1. Fork the repository and create a branch from `main`.
2. Make your change, with tests (see below).
3. Run the checks from [docs/development.md](docs/development.md#run-the-checks-locally).
4. Open a PR, fill in the template, and link the issue (`Fixes #123`) and any requirement
   IDs (`REQ-...`) it addresses.

Keep each PR focused on one thing. A maintainer reviews every PR; CI must pass before it
is merged. New dependencies need a reason in the PR description and a license compatible
with Apache-2.0. Changes that claim a performance effect include numbers from `bench/`.

### Tests

- Unit tests for new logic, and the race detector for concurrent code.
- A fuzz test for every parser, decoder and protocol handler (wire framing, guest agent
  messages, config files, port and mount specs).
- Tests that need a real hypervisor use the `integration` build tag; say in the PR which
  Windows edition you ran them on.
- A bug fix includes a test that fails without the fix.
- Tests bind to `127.0.0.1`, never to `0.0.0.0` or `::`.

### Code style

- `gofmt`/`goimports` and `golangci-lint` must be clean.
- Return errors with context (`fmt.Errorf("start vm: %w", err)`); don't panic in library
  code.
- Windows-only code goes in `_windows.go` files. Keep packages buildable on Linux where
  possible so guest code and tests run there too.
- Comments explain why. Messages shown to users say what happened and what to do next.

### Commit messages

```
area: what the change does, lower case, no period

Why the change is needed and what it does, wrapped at about 72
columns.

Refs: REQ-DISK-001
Signed-off-by: Your Name <you@example.com>
```

`area` is the package or part of the project the change is about (`broker`, `engine`,
`gui`, `sandbox`, `docs`, `ci`, ...); `git log --oneline` has plenty of examples. One
logical change per commit.

### Sign your commits (DCO)

Every commit must be signed off with `git commit -s`. This certifies the
[Developer Certificate of Origin](https://developercertificate.org/): that you wrote the
change or otherwise have the right to submit it under the project's license.

Code written with the help of AI tools is welcome. You are responsible for it like any
other code you submit: understand it, test it, and sign it off yourself.

## Design decisions

Significant design choices are recorded as ADRs in [docs/adr/](docs/adr/), and
requirements in [docs/requirements.md](docs/requirements.md). To propose a change of
direction, open a PR with a new ADR from the template; a PR with only the ADR is fine.

## License

hyperbutler is licensed under [Apache-2.0](LICENSE). By contributing, you agree that your
contributions are licensed under the same terms.
