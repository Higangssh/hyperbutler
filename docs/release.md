# Releasing hyperbutler

This is the checklist for every release, and the description of what the release pipeline
does. A release is a Git tag `vX.Y.Z` (or `vX.Y.Z-pre`) on `main`, a draft GitHub release
built from it by `.github/workflows/release.yml`, and a maintainer who publishes the draft
after every gate below has passed.

Nothing here runs on a developer machine. Release binaries and guest images come only from
GitHub-hosted runners (ADR 0005, Decision 4).

## What a release contains

| Asset | Produced by | What it is |
|---|---|---|
| `hyperbutler-<tag>-windows-amd64.zip` | `dist/package.ps1` | The portable install, below |
| `hyperbutler-<tag>-guest-sources.tar` | `scripts/guest-image/fetch-sources.sh` | Corresponding source of the guest image (GPL and others) |
| `hyperbutler-<tag>-guest-inputs.tar` | `scripts/guest-image/ci-entry.sh` | The signed Alpine packages the image was built from (our mirror) |
| `hyperbutler-<tag>-windows-amd64.spdx.json` | syft 1.55.0 | SBOM of the zip contents |
| `SHA256SUMS` | `release.yml` | SHA-256 of the four files above |
| Build provenance | `actions/attest-build-provenance` | SLSA provenance for every file in `SHA256SUMS`, signed through Sigstore. Verify with `gh attestation verify <file> --repo Higangssh/hyperbutler` |

The zip has one top-level folder:

| File | Notes |
|---|---|
| `hyperbutler.exe` | `CGO_ENABLED=0 go build -trimpath -buildvcs=true -ldflags "-s -w -buildid="`. The version is the tag, taken from Git by the Go toolchain; `package.ps1` fails if `go version -m hyperbutler.exe` reports anything else |
| `vmlinuz-virt`, `initramfs-stage1.gz`, `system.vhd`, `image.json` | The guest image (ADR 0005 A2b), from the guest-image job. `hyperbutler start` finds them next to `hyperbutler.exe` and checks them against `image.json`. `package.ps1 -ImageFiles` names them |
| `LICENSE`, `NOTICE` | hyperbutler's own |
| `THIRD-PARTY/` | License texts of every Go module linked into `hyperbutler.exe` or the guest binaries, of the Go standard library, and of every guest package (from the upstream sources), with an index in `README.txt` |
| `SOURCE-OFFER.txt` | Names the source bundle by file name, SHA-256 and URL |
| `README.txt`, `BUILDINFO.txt`, `SHA256SUMS` | Quick start; versions, flags, commit and hashes; checksums of every file in the zip |

**Size (REQ-UX-002, ≤ 150 MB):** `package.ps1` fails above 150 MB. The first dry run
(2026-10-30, commit `326f048`, prototype image) gave **79.7 MB zipped** (83,541,023 bytes;
87.6 MB unpacked): initramfs 65.8 MB, `hyperbutler.exe` 13.3 MB, `vmlinuz-virt` 12.6 MB. The
initramfs is already gzip, so zipping saves almost nothing. Record the size of every
release here.

| Release | Zip size | Notes |
|---|---|---|
| (dry run) | 79.7 MB | prototype A2a image, snapshot |
| (dry run, 2026-10-31) | 84.9 MB | A2b image with the Alpine engine, snapshot (89,026,173 bytes; 113.1 MB unpacked) |

There is no installer and nothing needs admin rights to install: unzip, add the folder to
the user PATH, run `hyperbutler start` (REQ-PLAT-003). The one-time prerequisites (the Virtual
Machine Platform feature, and Hyper-V Administrators membership until the ADR 0004 broker
ships) do need an administrator; the README says so.

## How the pipeline works

`release.yml` runs only on a pushed tag matching `v*.*.*`. Its jobs:

1. **guest-image** (`guest-image.yml`, reusable, also run by `ci.yml` on every PR):
   - `ubuntu-latest`, inside `alpine:3.24.3` pinned by index digest. The container's
     `/etc/apk/keys` is the trust root.
   - `fetch-verify.sh` downloads every entry of the lock file (https only), checks its
     SHA-256 pin, and runs `apk verify` on every `.apk`. That checks the RSA signature
     against the Alpine keys and the hash chain over the package contents. It also checks
     that the signed `.PKGINFO` names the pinned file.
   - **Strict mode** rejects any input that is not a signed Alpine package, because
     neither its signature nor its source can be verified. It is the workflow's default,
     and both `ci.yml` (every PR and push) and `release.yml` set it: since the engine
     comes from Alpine's packages (ADR 0005, "Docker Engine from Alpine"), the lock has
     no other kind of input, and a PR that adds one fails.
   - The build script (input `build-script`, default `scripts/guest-image/build-system.sh`)
     builds the A2b image: `vmlinuz-virt`, `initramfs-stage1.gz`, `system.vhd` and
     `image.json`. Its contract is in that file. It is the Linux port of
     `guest/prototype/build.ps1` and runs the same `mkfs-erofs.sh`, with `erofs-utils`
     at the version both pin.
   - `fetch-sources.sh` takes each package's origin and aports commit from its signed
     `.PKGINFO`, fetches that aports directory (the commit hash pins its content), and
     runs `abuild fetch verify`, which downloads the upstream archives and checks the
     APKBUILD's sha512sums. The result is the source bundle and the license texts.
   - Outputs: image files, `guest-inputs.tsv`, `BUILDINFO.txt`, `licenses/`,
     `guest-sources.tar`, `guest-inputs.tar`, `SHA256SUMS`. The job passes the digest of
     `SHA256SUMS` to the next job as a job output.
2. **package** (`windows-latest`):
   - Checks that the tag is a semver tag, that it is HEAD, and that it is on `origin/main`.
   - Checks the guest artifact against the digest from job 1, then runs `dist/package.ps1`
     without `-Snapshot`, so every check is fatal.
3. **sbom** (`ubuntu-latest`, no permissions at all): syft, installed from the release
   tarball checked against its pinned SHA-256, scans the unpacked zip.
4. **publish** (`ubuntu-latest`, environment `release`):
   - The only job with write permissions (`contents`, `id-token`, `attestations`). It does
     not check out the repository and runs no repository code.
   - It checks every asset against the digests from the earlier jobs, writes
     `SHA256SUMS`, attests it, and creates a **draft** release with `gh release create
     --draft --verify-tag`.

Supply-chain rules the workflows follow:

- Every action is pinned by full commit SHA, with the version in a comment. Downloaded
  tools (golangci-lint, syft, shellcheck for local checks) are pinned by SHA-256.
- Top-level `permissions: {}` in `release.yml`, `contents: read` elsewhere. Write
  permissions only in `publish`.
- `persist-credentials: false` on every checkout.
- No caches in the guest-image and release jobs, so a PR run cannot poison a release
  build. `setup-go` runs with `cache: false` there.
- Workflow inputs reach shell code only through environment variables, and
  `ci-entry.sh` validates them (relative paths inside the repository, plain file names).
- Artifacts are only exchanged within one run, and each consumer checks them against a
  digest from a job output.

Known limits of the image build:

- The build tools in the container (`curl`, GNU `tar`, `gzip`, `xz`, `git`, `abuild`)
  come from the pinned Alpine release's current index, signed but not version-pinned.
  Their versions are recorded in `BUILDINFO.txt`. The image inputs themselves are
  pinned; the tools are not, so the build is not hermetic across days. `erofs-utils`,
  whose output is the system disk, is version-pinned (`1.9.1-r0`, signed, not
  hash-pinned; ADR 0005 Follow-up 8): when Alpine drops that revision the build fails
  instead of changing.
- Go comes from `actions/setup-go` on the runner (glibc host) and is mounted into the
  musl container. The official toolchain binaries are statically linked and the guest
  binaries are built with `CGO_ENABLED=0`; the first CI run confirms it.

Maintainer settings that the files cannot enforce:

- Give the `release` environment required reviewers and restrict it to tags `v*`.
- Protect `v*` tags (a tag ruleset) so only maintainers can create them.
- Keep "Allow GitHub Actions to create and approve pull requests" off.

## Checklist

Tick every item in the release PR or issue, with a link to the evidence. A gate that is not
met blocks the release. Changing a gate needs an ADR change, not a waiver here.

### 1. Release gates from the ADRs

**ADR 0003 (implementation language: leak guardrails)**
- [ ] The **24 h soak** (`bench/soak`) passed on self-hosted Hyper-V hardware against the
  release candidate, with the diag endpoint on: no positive trend in private working set,
  handles, threads or goroutines. Attach the report.
- [ ] The 45 MB working set seen under the soak workload is profiled and explained
  (ADR 0003, Consequences) before any memory budget is claimed.
- [ ] `go test -race ./...` and the goleak meta-test pass on Windows and Linux (CI).

**ADR 0002 (HCS backend)**
- [ ] **Host disk full:** the free-space guard test (`docs/research/disk-reclaim.md`,
  "Free-space guard") is repeated with the released `hyperbutler` CLI on a small test volume:
  the guest gets `ENOSPC`, no ext4 errors, no lost files, and the VM pauses at the floor
  (REQ-DISK-004/005).
- [ ] **Deferred commit under host memory pressure** is tested (ADR 0002, Consequences).
- [ ] The integration tests (`go test -tags integration ./...`) pass on real hardware with
  the current Windows 11 build, on Home, and on Pro if available, because the HCS schema
  fields hyperbutler uses are undocumented.
- [ ] REQ-MEM-002 (`bench/mem-reclaim`, 12 GB guest) is measured and recorded.

**ADR 0005 (guest image)**
- [ ] The image comes from the release run's guest-image job in strict mode, so every
  input is a signed Alpine package. Since 2026-10-31 the engine is Alpine's
  `docker-engine`, `containerd`, `runc` and `tini-static` (ADR 0005, "Docker Engine from
  Alpine"), and CI runs strict on every PR, so this holds unless that job was skipped.
- [ ] Moby, containerd and runc LICENSE and NOTICE files are in `THIRD-PARTY/guest/`
  (Apache-2.0 §4(a) and (d)): `guest/docker/moby-<ver>/`, `guest/containerd/...`,
  `guest/runc/...`. `fetch-sources.sh` takes them from the upstream tarballs.
- [ ] The Alpine `docker`, `containerd` and `runc` packages in the lock are not behind an
  upstream security release by more than 7 days (ADR 0005, revisit rule). Bump the lock
  if they are; move to building from source if Alpine has not caught up.
- [ ] `hyperbutler.exe` checks the guest image files against hashes embedded at build time
  (Decision 4). Not implemented yet.
- [ ] REQ-MEM-001: idle memory ≤ 400 MB (`bench/mem-idle`). Today 580 MiB; the A2b erofs
  layout is the planned fix.
- [ ] REQ-START-001 is re-measured with the release image (20 runs, warm), and the first
  start after a host reboot is reported separately.

**ADR 0006 (guest networking)**
- [ ] Every row of the ADR 0006 acceptance gate reads "Yes" ("Gate results"), or ADR 0006
  is changed to the HNS NAT fallback. The rows are: guest-to-host TCP ≥ 50% of raw hvsock
  on the same boot, under the shipped buffer bound; host CPU ≤ 1 core per Gbit/s; a LAN
  registry pull within 20% of HNS NAT; idle stack memory within REQ-MEM-001; split DNS,
  public DNS and a VPN-only address in one run with the VPN connected; p99 small-request
  latency recorded idle and under load next to raw hvsock (the open p99.9 tail of about
  11 ms explained). Today the throughput and LAN registry rows are not met.
- [ ] `bench/net-vpn`, `bench/coexist` and `bench/noadmin` have run (ADR 0006,
  "What stays unverified").

**Requirements**
- [ ] REQ-UX-002: the zip is ≤ 150 MB (`package.ps1` enforces it). Record the size in the
  table above.
- [ ] REQ-UX-001: on a clean Windows 11 Home account, the README Quick Start works as
  written: unzip, PATH, `hyperbutler start`, `docker run --rm hello-world`, `hyperbutler stop`.
- [ ] REQ-PLAT-005 (from the second release on): upgrading from every release since the
  last minor keeps images, volumes and containers.
- [ ] The README "Current status" table matches the measured numbers for this release.
  Nothing in the README claims a target that is not met.

### 2. GPL and license obligations

The guest image contains GPL-2.0 code (Linux, BusyBox, e2fsprogs, iptables, libnftnl) and LGPL
code (libblkid, parts of e2fsprogs, libseccomp, libmnl). hyperbutler distributes it, so hyperbutler owes the source.

- [ ] `hyperbutler-<tag>-guest-sources.tar` is attached to the same release as the zip. GPLv2
  §3 lets us meet the source duty by "equivalent access to copy the source code from the
  same place"; that is why the bundle is a release asset and not a link to Alpine's
  servers. Never delete it while the zip is offered.
- [ ] The bundle holds, for every package in `MANIFEST.tsv`, the aports directory at the
  commit in the package's signed `.PKGINFO` (APKBUILD, patches, the kernel config) and
  every upstream archive it names. Spot-check: the kernel tarball for the `linux-virt`
  version, the BusyBox tarball, e2fsprogs, util-linux, iptables and libseccomp.
- [ ] `SOURCE-OFFER.txt` names the bundle with the right SHA-256 and URL.
- [ ] `THIRD-PARTY/README.txt` lists every Go module and guest package, and each one has
  its license text. `package.ps1` fails on a Go module without one; `fetch-sources.sh`
  fails on a guest origin without one unless `dist/licenses/<origin>/` supplies it.
- [ ] `NOTICE` is current: new Go modules, new guest packages.
- [ ] No GPL code is linked into `hyperbutler.exe` (the SBOM lists its modules).
- [ ] "Docker" is used only descriptively ("Docker Engine (Moby)"), with the trademark
  note in the README.

### 3. Cut the release

1. Merge everything for the release into `main`, CI green (including the guest-image job).
2. Tag the release commit on `main` and push the tag (maintainer only):
   `git tag -s vX.Y.Z -m "hyperbutler vX.Y.Z"`, then `git push origin vX.Y.Z`.
3. Wait for `release.yml`. Approve the `release` environment when asked.
4. Check the draft:
   - [ ] Download all assets; `sha256sum -c SHA256SUMS`.
   - [ ] `gh attestation verify <each asset> --repo Higangssh/hyperbutler`.
   - [ ] `hyperbutler version` prints the tag.
   - [ ] The zip size is recorded above.
   - [ ] Run the Quick Start on a clean Windows 11 Home account.
5. Write the release notes: what changed, the measured status, and the known gaps.
6. Publish the draft.

If a gate fails after tagging, delete the draft, fix on `main`, and tag a new version.
Never move a published tag.

## Local dry run

`dist/package.ps1 -Snapshot` builds the same zip from a local image
(`guest/prototype/build.ps1`). It marks the zip as a snapshot that must not be
redistributed (no source bundle, no image provenance), and relaxes the clean-tree and
version checks. The size limit still applies. See CONTRIBUTING.md.

## Adversarial review

The adversarial reviewer reviewed the workflows, the scripts, `package.ps1`, the
README, NOTICE and this file on 2026-10-30. It confirmed every action SHA, the syft
checksum and the Alpine digest against upstream, found no script injection, token exposure
or cache poisoning, and checked the README numbers against the research documents. Its
findings:

| # | Sev | Finding | Resolution |
|---|---|---|---|
| 1 | High | `publish` downloads `release-*`, which also matched the guest artifact `release-guest-image`, so the asset whitelist failed on the first run. | **Fixed:** the guest artifact is `guest-image-release`. |
| 2 | High | `fetch-sources.sh` took the file name of a `name::url` source from the URL, but abuild saves it under `name`, so e2fsprogs and libeconf had no license texts (and Moby's would be missed later). | **Fixed:** the local name is used. |
| 3 | High | The ca-certificates upstream archive has no license file, and `dist/licenses/` did not exist, so every release run would fail. | **Fixed:** `dist/licenses/ca-certificates/` holds the MPL-2.0 text (from mozilla.org) and a note on which part is MIT. |
| 4 | High | `Exec` might emit `$null` for no output, so a clean tree would count as dirty. | **Fixed defensively** (`Exec` drops nulls). A non-snapshot run on a clean tree had already passed locally, so it was not reproducible. |
| 5 | Med | The strict and with-sources paths ran only on a tag. | **Fixed:** `ci.yml` builds the source bundle on every push to main. Strict mode stayed release-only while the Docker static tarball was in the lock; since 2026-10-31 CI runs it on every PR too. |
| 6 | Med | Partial clone with anonymous URL remotes was fragile. | **Fixed:** named remotes, a plain shallow fetch of one commit. |
| 7 | Med | The README claimed one guest configuration for all rows. | **Fixed:** the spike rows say which harness and guest size. |
| 8 | Med | This file listed two of the six ADR 0006 gate rows. | **Fixed:** all six. |
| 9 | Low | Container build tools are not version-pinned. | **Accepted and documented** above ("Known limits"). They are signed and recorded in `BUILDINFO.txt`. |
| 10 | Low | NOTICE and the zip README describe a compliant image, but a snapshot zip carries the Docker static binaries. | **Fixed:** `package.ps1` refuses non-Alpine inputs outside `-Snapshot`, and NOTICE says snapshots are not releases. |
| 11 | Low | `$env:USERNAME` in the admin's shell adds the wrong account when the admin is another user. | **Fixed:** the README uses `<user>` from `whoami`. |
| 12 | Low | A versioned folder on PATH goes stale on upgrade. | **Fixed:** the Quick Start installs into a fixed folder and documents the upgrade. |
| 13 | Low | The glibc Go toolchain mounted into Alpine is unverified. | **Accepted and documented** ("Known limits"). The official toolchain is static; the first CI run confirms it. |
| 14 | Low | This section was a placeholder. | **Fixed:** this table. |
