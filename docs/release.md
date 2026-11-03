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
| `hyperbutler.exe` | `CGO_ENABLED=0 go build -trimpath -buildvcs=true -ldflags "-s -w -buildid= -X ...imageset.pinnedManifest=<sha256 of image.json>"`. The version is the tag, taken from Git by the Go toolchain; `package.ps1` fails if `go version -m hyperbutler.exe` reports anything else, or if `hyperbutler.exe version --json` does not report the image pin |
| `hyperbutler-broker.exe` | The HCS access broker (ADR 0004), built like `hyperbutler.exe` with the same image pin and `-X main.version=<tag>`; `package.ps1` fails unless `hyperbutler-broker.exe -version-json` reports both. In the zip (listed in `BUILDINFO.txt` and `SHA256SUMS`), and in the MSI, which installs it as the `hyperbutler-broker` service |
| `image/vmlinuz-virt`, `image/initramfs-stage1.gz`, `image/system.vhd`, `image/image.json` | The guest image (ADR 0005 A2b), from the guest-image job. In `image\`, not next to `hyperbutler.exe`, where Explorer shows `system.vhd` as a disk image and a double-click offers to mount it. `hyperbutler start` finds them in the `image\` folder next to `hyperbutler.exe` (`--image-dir` names another), says to extract the zip again if they are missing there and names any left directly next to `hyperbutler.exe` by an older layout, checks `image.json` against the SHA-256 linked into `hyperbutler.exe` and the other files against `image.json` (ADR 0005, Decision 4). `package.ps1 -ImageFiles` names them |
| `clients/docker.exe`, `clients/cli-plugins/docker-compose.exe` | The docker CLI and the compose plugin, built by `package.ps1` from the docker/cli and docker/compose source modules pinned in `dist/clients.lock` (module proxy, sum.golang.org and the lock's `h1:` hash; upstream build tags and version flags, `CGO_ENABLED=0 -trimpath`). Not next to `hyperbutler.exe`, so they never shadow a docker CLI on PATH ([compat.md](compat.md#the-docker-cli-and-docker-desktop)). Their license texts are in `THIRD-PARTY/clients/` |
| `LICENSE`, `NOTICE` | hyperbutler's own |
| `THIRD-PARTY/` | License texts of every Go module linked into `hyperbutler.exe`, the guest binaries or the bundled clients, of the Go standard library, and of every guest package (from the upstream sources), with an index in `README.txt`. The docker/cli and docker/compose LICENSE and NOTICE files (Apache-2.0 §4(a), (d)) are in `THIRD-PARTY/clients/<name>/` |
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
| (dry run, 2026-11-01) | 106.7 MB | A2b image (local build, 88 MiB system disk) plus the docker CLI 29.9.2 and compose v5.6.1 built from source (30.4 and 30.0 MB), snapshot (111,857,104 bytes; 173.7 MB unpacked). Docker's prebuilt `docker.exe` would have been 44.9 MB |
| v0.1.0-preview.1 | 116.4 MB | release run (116,415,311 bytes; MSI 121,729,372 bytes) |
| v0.1.0-preview.2 | 120.3 MB | release run, with `hyperbutler-broker.exe` (120,298,176 bytes; 196.7 MB unpacked; MSI 125,358,540 bytes) |

The release is x64 only. Docker publishes no Windows arm64 docker CLI; since it is built
from source here, an arm64 release would only need `GOARCH=arm64`, but the guest image is
x86_64 and does not boot on ARM64, so there is no arm64 release yet (on ARM64 Windows the
x64 programs would run under emulation; the VM would not).

There is no installer and nothing needs admin rights to install: unzip, add the folder to
the user PATH, run `hyperbutler start` (REQ-PLAT-003). The one-time prerequisites (the Virtual
Machine Platform feature, and Hyper-V Administrators membership, since the zip installs
no ADR 0004 broker service; the MSI does, and needs no membership) do need an
administrator; the README says so.

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
     without `-Snapshot`, so every check is fatal. It links the SHA-256 of the checked
     `image.json` into `hyperbutler.exe` and builds the docker CLI and compose from the
     source modules in `dist/clients.lock` (through the Go module proxy, checked against
     sum.golang.org and the lock).
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
  tools (golangci-lint, syft, shellcheck for local checks) are pinned by SHA-256. The
  bundled docker CLI and compose are built from source modules pinned by their `h1:`
  hash (`dist/clients.lock`), never downloaded as binaries.
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

**Preview releases** (`vX.Y.Z-preview.N`, decided 2026-11-02) are published as GitHub
pre-releases before every gate is met, so people can try hyperbutler while the gates run.
For a preview: sections 2 (licenses) and 3 (cut the release) apply in full; section 1's
gates that are not met yet are listed by name in the release notes and in the README's
status, never left out. The release `vX.Y.Z` itself still needs every gate below; the 24 h
soak runs separately and its result goes here.

### 1. Release gates from the ADRs

**ADR 0003 (implementation language: leak guardrails)**
- [ ] The **24 h soak** (`bench/soak`) passed on self-hosted Hyper-V hardware against the
  release candidate, with the diag endpoint on: no positive trend in private working set,
  handles, threads or goroutines. Attach the report. Since 2026-11-01 the engine's
  private working set and private bytes are judged with `heap_released` added back
  (`ws_private_adj`, `private_bytes_adj`; ADR 0003, guardrail 3).
  - 1 h main gate, 2026-11-01, engine built from main at 0012298 (the commits after it to e232f5c change only benches, tests and docs), local A2b image, with bind mounts and a
    published port: 12327 docker commands, 0 failed. **FAIL on the raw private working
    set** (`ws_private`, judged at the time: +3.4 MiB over the 50 min window against an
    allowed 2.0 MiB). The adjusted metric was flat: re-judged from the CSV
    (`soak -judge`), `ws_private_adj` grew +0.70 MiB against an allowed 2.24 MiB and
    `private_bytes_adj` +0.49 MiB against 4.65 MiB, so the run **passes** under the
    current gate. Private
    bytes, handles, threads, goroutines, heap in use, vmmem and vmwp passed. The Go heap
    profiles at the end of the warm-up and of the run are the same (diff ≈ 0), and Go
    `Sys` is flat at 41.3 MiB from minute 35. About 2.4 MiB of the rise is Go reusing
    pages it had returned (`heap_released` 7.2 → 4.8 MiB), which is bounded by the heap
    size; private bytes outside Go grew about 2 MiB/h in the second half and were
    flattening. Explained afterwards: Go `Sys` and the Windows heaps
    (1.27 MiB) were flat after the warm-up, the growth was inside Go's heap arenas being
    recommitted, and `private_bytes + heap_released` went from 93.77 to 93.80 MiB over
    the window. Not a leak; the 24 h run is still the release gate.
  - 45 min run, 2026-11-01, the same engine build and image, with bind mounts and a published port, 10 min warm-up (35 min window): 9160 docker commands, 0 failed.
    **FAIL on the raw private working set** (+3.6 MiB against an allowed 2.0 MiB), the
    rest passed. Re-judged from the CSV: `ws_private_adj` +0.54 MiB against 2.25 MiB,
    `private_bytes_adj` +0.07 MiB against 4.69 MiB: **pass**.
- [ ] The 45 MB working set seen under the soak workload is profiled and explained
  (ADR 0003, Consequences) before any memory budget is claimed.
- [x] `go test -race ./...` and the goleak meta-test pass on Windows and Linux (CI).
  2026-11-03, v0.1.0-rc.1: CI on 9ae259c, d54b020 and a12c89a green: `go test -race ./...`
  on Windows (every TestMain runs goleak), `./guest/... ./internal/leaktest/...` on Linux.

**ADR 0002 (HCS backend)**
- [ ] **Host disk full:** the free-space guard test (`docs/research/disk-reclaim.md`,
  "Free-space guard") is repeated with the released `hyperbutler` CLI on a small test volume:
  the guest gets `ENOSPC`, no ext4 errors, no lost files, and the VM pauses at the floor
  (REQ-DISK-004/005).
- [ ] **Deferred commit under host memory pressure** is tested (ADR 0002, Consequences).
- [ ] The integration tests (`go test -tags integration ./...`) pass on real hardware with
  the current Windows 11 build, on Home, and on Pro if available, because the HCS schema
  fields hyperbutler uses are undocumented.
- [ ] REQ-MEM-002 (`bench/mem-reclaim`, 12 GB guest) is measured against the release
  candidate and recorded in `docs/research/mem-reclaim.md`, with the engine and VM split
  the bench prints. 2026-10-31 to 11-01, release configuration (the engine's
  working-set guard): 5 of 5 runs with `-sampler nt` passed (441.8 to 457.3 MiB at
  +60 s, at most 461.5 MiB from +60 s on, against 476.8 MiB); 2 of 3 with `-sampler cim`,
  which provokes an outside read of the engine's memory: the read puts the total over the
  limit for about 4 s until the guard trims, and one trim landed at +60.1 s. Run it
  several times, with both samplers, and record every run, not the best one. Check the
  engine log for `working set emptied` lines in the `-sampler nt` runs (none so far) and
  that the engine logs `partialRule=true` at start.

**ADR 0006 (guest networking)**
- [ ] `bench/net-perf` against the release candidate: bulk paths not below the ADR's
  2026-10-31 numbers by more than run-to-run noise, connections per second with no
  errors.
- [ ] **LAN registry pull** (manual, `bench/net-lan`, when a second machine on a wired
  link is available): a large image from `registry:2` on that machine, through the stack
  and through HNS NAT, within 20% of each other (the gate ADR 0006 deferred to here).
  It blocks the release.
  - Partial run (2026-10-31), not the gate: `registry:2` on a Raspberry Pi 5 (1 Gbit wired),
    the Windows host on a 100 Mbit USB adapter. Three 100 MB blobs fetched in parallel over
    HTTP, three runs each: host `curl.exe` 94.5 Mbit/s, a container through the stack
    94.0 Mbit/s (99.5%). Both hit the 100 Mbit link, so this only shows the stack costs
    nothing at that speed. Not a `docker pull`: there is no way yet to set
    `insecure-registries` for the guest's dockerd. The gate still needs a 1 Gbit host link
    and the HNS NAT comparison.
  - 1 Gbit run (2026-11-01): the same Pi, the Windows host on a 1 Gbit USB adapter
    (Realtek USB GbE, link 1 Gbps), an amd64 image of three 256 MiB random layers in
    `registry:2`, the Pi's page cache warmed. Three blobs in parallel, three runs each:
    host `curl.exe` 916-941 Mbit/s, a container through the stack (busybox `wget`)
    929-931 Mbit/s (99% of the host). A real `docker pull` through the stack
    (`insecure-registries` in daemon.json; download, digest check and extraction):
    735-861 Mbit/s over 30 back-to-back pull/rmi cycles, 0 failed. HNS NAT was not run:
    it needs an HNS network created on the host, a system change. HNS NAT is a VM path
    too, so it cannot beat the host's own rate, and the stack is within 1% of that.
    This meets the gate's intent against an upper bound of NAT, and on 2026-11-01 the
    maintainer accepted the host's own rate as the baseline (ADR 0006, "Gate
    amendment"). **Met.** Repeat the 30-cycle pull loop against the release candidate.
  - The same run found a silent corruption bug in the host-to-guest link: about 1 in 10
    back-to-back pulls failed with "unexpected commit digest" until it was fixed
    (036c062; [guest-networking.md](research/guest-networking.md), "Layer digest
    mismatches on LAN pulls (2026-11-01)"). The 30 clean cycles above are after the fix.

**ADR 0005 (guest image)**
- [x] The image comes from the release run's guest-image job in strict mode, so every
  input is a signed Alpine package. Since 2026-10-31 the engine is Alpine's
  `docker-engine`, `containerd`, `runc` and `tini-static` (ADR 0005, "Docker Engine from
  Alpine"), and CI runs strict on every PR, so this holds unless that job was skipped.
  2026-11-03, v0.1.0-rc.1: the release run 41620846473 built it with `strict: true` /
  `HYPERBUTLER_STRICT: 1`.
- [x] Moby, containerd and runc LICENSE and NOTICE files are in `THIRD-PARTY/guest/`
  (Apache-2.0 §4(a) and (d)): `guest/docker/moby-<ver>/`, `guest/containerd/...`,
  `guest/runc/...`. `fetch-sources.sh` takes them from the upstream tarballs.
  2026-11-03, v0.1.0-rc.1: the zip has `THIRD-PARTY/guest/docker/moby-29.9.2`,
  `docker/cli-29.9.2`, `containerd/containerd-2.3.7` and `runc/runc-1.4.3`, each with
  LICENSE and NOTICE.
- [x] The Alpine `docker`, `containerd` and `runc` packages in the lock are not behind an
  upstream security release by more than 7 days (ADR 0005, revisit rule). Bump the lock
  if they are; move to building from source if Alpine has not caught up.
  2026-11-03, v0.1.0-rc.1: checked against upstream releases and advisories:
  moby/docker-cli 29.9.2 (latest), containerd 2.3.7 (fixes the 2026-10-13 advisories),
  runc 1.4.3 (no advisory since 2026-06-13, fixed in 1.4.3) are current; Alpine v3.24
  ships exactly these. Not in this rule but found with it: linux-virt 6.18.59 is one point
  release behind 6.18.60 (2026-10-30, in v3.24); Alpine drops superseded kernels from the
  mirrors (6.18.58 went that way): bumped to 6.18.60-r0 the same day, for rc.2.
- [ ] `hyperbutler.exe` checks the guest image files against hashes embedded at build time
  (Decision 4): `hyperbutler version --json` of the released binary says
  `"imagePinned": true` with the `image.json` SHA-256 from the guest-image job, and a
  start with one byte of `image.json` changed is refused. Implemented 2026-11-01
  (`internal/engine/imagepin.go`); `package.ps1` fails when the pin is missing.

**Bundled clients (`dist/clients.lock`)**
- [x] `docker-cli` follows the guest's Docker Engine version and `docker-compose` is the
  current release, neither behind an upstream security release by more than 7 days.
  2026-11-03, v0.1.0-rc.1: docker-cli 29.9.2 = the guest engine; compose v5.7.0 is the
  latest (2026-11-02).
- [x] `clients\docker.exe version` and `docker compose version` from the zip print the
  pinned versions, against the release candidate.
  2026-11-03, v0.1.0-rc.1: `Docker version 29.9.2, build a82e528`, `Docker Compose version
  v5.7.0` from the zip.
- [x] REQ-MEM-001: idle memory ≤ 400 MB (`bench/mem-idle`) against the release candidate.
  2026-11-01, main at 0012298 with a local A2b image: median 313.9 MiB (329 MB; engine 19.1,
  vmmem 289.3, vmwp 5.5 MiB), PASS. The engine's working set rose from 7.3 to 19.8 MiB over
  the window (+21.7 MiB/h); the 24 h soak must show whether that levels off.
  2026-11-03, v0.1.0-preview.2 from its MSI, through the broker service, `bench/mem-idle`
  counting the broker: median **360.8 MiB (378 MB)**, PASS (engine 20.1, vmmem 296.5,
  vmwp 5.4, broker service 38.8 MiB). The broker is a third of the margin left. The same
  broker binary in the foreground holds 3.5 MiB (5 MiB with a VM): as a service its
  untouched 32 MiB FIPS buffer had become resident, the outside-read signature the
  engine's working-set guard already handles. Changed: the service empties its working
  set once at start (a scan as it is created would otherwise set the guard's cold level)
  and then runs that guard; it logs the guard's starting readings to the Application
  event log. To verify with the release candidate: the broker's private working set at
  idle, and any "working set emptied" events.
  2026-11-03, v0.1.0-rc.1 from its MSI (upgrade over preview.2), same bench: median
  **319.3 MiB (335 MB)**, PASS (engine 20.5, vmmem 286.9, vmwp 5.0, broker service
  6.9 MiB). Verified: the service logged "working-set guard started (partial rule true;
  trimmed first true: private working set 430080, …, data section 114688)", and the
  broker went from 38.8 to 6.9 MiB.
- [ ] REQ-START-001 is re-measured with the release image (20 runs, warm), and the first
  start after a host reboot is reported separately. 2026-11-01, same build and image:
  median 1.48 s, p95 1.49 s over 20 runs (1 warm-up discarded), stop median 1.28 s, PASS.
  Not a first start after a reboot (host up 48 h).
  2026-11-03, v0.1.0-preview.2 from its MSI, through the broker service: median **1.94 s**,
  p95 1.96 s over 20 runs (1 warm-up discarded), stop median 1.66 s, PASS. The broker adds
  about 0.46 s to the start and 0.38 s to the stop against the in-process start, with
  almost no spread (1.93 to 1.96 s). Explained the same day: not the broker (a
  foreground broker and the service gave the same 1.86 s), but `start` waiting for the
  new VM's first guest file-system report for the REQ-DISK-009 warning, about 0.42 s on
  every start (hyperbutlerd's report at connect predates the guard's first budget and is
  not published; the next comes a second later). Fixed: a fresh start judges the warning
  from the previous run's last report of the same data disk, which cannot change while
  no VM runs; with no such report for an existing disk it still waits. Re-measure with
  the release candidate.
  2026-11-03, v0.1.0-rc.1 from its MSI, through the broker service: median **1.44 s**,
  p95 1.45 s over 20 runs (1 warm-up discarded), stop median 1.56 s, PASS. The 0.42 s
  wait is gone; the start through the broker now matches the in-process 1.48 s.

**ADR 0006 (guest networking)**
- [x] Every row of the ADR 0006 acceptance gate reads "Yes" ("Gate results"), or ADR 0006
  is changed to the HNS NAT fallback. The rows are: guest-to-host TCP ≥ 50% of raw hvsock
  on the same boot, under the shipped buffer bound; host CPU ≤ 1 core per Gbit/s; a LAN
  registry pull within 20% of HNS NAT; idle stack memory within REQ-MEM-001; split DNS,
  public DNS and a VPN-only address in one run with the VPN connected; p99 small-request
  latency recorded idle and under load next to raw hvsock (the open p99.9 tail of about
  11 ms explained). On 2026-11-01 every row but the p99.9 tail explanation reads "Yes":
  the throughput row since the 2026-10-31 work (median 69% of raw hvsock), and the LAN row
  against the host's own rate. The tail of about 11 ms under a reconnecting load is still
  an open item in ADR 0006.
  2026-11-03: the tail row is changed from "explained" to "recorded" (ADR 0006, after
  "Gate results"): measured on the host side alone it does not occur (p99.9 0.6-0.9 ms
  with 35 reconnects), so it is inside the VM; the guest-side experiments come after
  v0.1.0. One request in a thousand is delayed by about 10 ms while a bulk connection
  starts, which no person notices; p50 and p99 meet the gate. Every row now reads "Yes"
  or "Recorded". **Met.**
- [ ] `bench/net-vpn`, `bench/coexist` and `bench/noadmin` have run (ADR 0006,
  "What stays unverified").
  - `bench/noadmin`, 2026-11-03, v0.1.0-preview.2 from its MSI, the owner's engine on a
    test home through the broker, `hbtest` (standard, not in Hyper-V Administrators,
    medium integrity) through `runas`: 40 checks, 39 passed at the time. Every way into
    the owner's engine was refused with access denied, before and after `hbtest`'s own
    run: the Docker API pipe and the per-user broker endpoint (open and adding an
    instance), the engine's stop, daemon and settings events, and AF_HYPERV to the
    owner's VM on 2375, 2380, 1024 and 2385. `hbtest`'s own `hyperbutler start --backend
    broker` worked on its own pipe and VM, `hello-world` ran, doctor said `hcs` and
    `hyperv-admins` info and `broker` ok, stop and uninstall passed. The one FAIL was the
    bench's own matcher: `docker -H` to the owner's pipe was refused, but the docker CLI
    says "permission denied while trying to connect" instead of Windows' text; the check
    accepts both since. **noadmin: met.**

**Requirements**
- [x] REQ-UX-002: the zip is ≤ 150 MB (`package.ps1` enforces it). Record the size in the
  table above.
  2026-11-03, v0.1.0-rc.1: zip 120,645,009 bytes (115 MiB), MSI 125,600,284 bytes.
- [ ] REQ-UX-010: `bench/uninstall` passes with the released `hyperbutler.exe` and image,
  against a test installation (it sets its own HYPERBUTLER_HOME, data directory, pipe and
  DOCKER_CONFIG under `%TEMP%\hb-dev\uninstall`):
  `powershell -File bench\uninstall\run.ps1 -Hyperbutler ... -Docker ... -ImageDir ...`.
  Afterwards `uninstall --dry-run` lists nothing and the docker context is gone.
  2026-11-01, development build on Windows 11 Home 26200: all 28 checks passed.
- [ ] REQ-UX-001: on a clean Windows 11 Home account, the README Quick Start works as
  written: unzip, PATH (with `clients\`, since a clean account has no docker CLI),
  `hyperbutler start`, `docker run --rm hello-world`, `docker compose version`,
  `hyperbutler stop`.
  2026-11-02, snapshot zip of e42d435 (CRC guest image) on a new local standard account,
  Windows 11 Home 26200, no docker CLI and no `~\.docker`: install, first start (2.3 s,
  data disk and context created, bundled compose registered), hello-world, nginx on
  8080, compose nginx+redis on 8081, a bind mount from the profile, status, doctor (all
  OK), restart and stop all passed; the GUI opened in Korean from Explorer. Before the
  account joined Hyper-V Administrators, `start` failed at once, as expected; its
  message was not captured. Recheck with the release zip. Found on the way: the image
  files (`system.vhd`) sat next to `hyperbutler.exe`, and a double-click on
  `system.vhd` asked Windows to mount it. Fixed: they are in `image\` now.
- [ ] REQ-UX-005: `bench/no-telemetry` passes against the release candidate (default
  5 minute idle, fresh `-home`), and `internal/notelemetry` is green in CI. 2026-11-01:
  PASS on two runs, nothing seen outside the bench's own pull.
- [x] REQ-UX-004: no feature needs an account (review of the release's changes; the
  2026-11-01 review is in [requirements.md](requirements.md)).
  2026-11-03, v0.1.0-rc.1: reviewed the changes since 2026-11-01 (broker service, MSI,
  MCP, start notes): nothing signs in anywhere; the only login-related code is doctor's
  check of docker's own credential helpers.
- [ ] REQ-PLAT-005 (from the second release on): upgrading from every release since the
  last minor keeps images, volumes and containers.
- [x] The README "Current status" table matches the measured numbers for this release.
  Nothing in the README claims a target that is not met.
  2026-11-03, v0.1.0-rc.1: README and status.md carry the rc.1 numbers (319 MiB idle, 1.44
  s start).

### 2. GPL and license obligations

The guest image contains GPL-2.0 code (Linux, BusyBox, e2fsprogs, iptables, libnftnl) and LGPL
code (libblkid, parts of e2fsprogs, libseccomp, libmnl). hyperbutler distributes it, so hyperbutler owes the source.

- [x] `hyperbutler-<tag>-guest-sources.tar` is attached to the same release as the zip. GPLv2
  §3 lets us meet the source duty by "equivalent access to copy the source code from the
  same place"; that is why the bundle is a release asset and not a link to Alpine's
  servers. Never delete it while the zip is offered.
  2026-11-03, v0.1.0-rc.1: attached to the draft, 232,192,000 bytes, SHA-256 matches
  SHA256SUMS.
- [x] The bundle holds, for every package in `MANIFEST.tsv`, the aports directory at the
  commit in the package's signed `.PKGINFO` (APKBUILD, patches, the kernel config) and
  every upstream archive it names. Spot-check: the kernel tarball for the `linux-virt`
  version, the BusyBox tarball, e2fsprogs, util-linux, iptables and libseccomp.
  2026-11-03, v0.1.0-rc.1: 396 entries; MANIFEST.tsv present; spot-checked the aports
  directory and upstream archive for linux-lts (APKBUILD, linux-6.18.tar.xz),
  busybox-1.37.0, e2fsprogs-1.47.4, util-linux-2.42.3, iptables-1.8.13 and
  libseccomp-2.6.1.
- [x] `SOURCE-OFFER.txt` names the bundle with the right SHA-256 and URL.
  2026-11-03, v0.1.0-rc.1: names `hyperbutler-v0.1.0-rc.1-guest-sources.tar`, sha256
  5deb0f70…, the release URL; matches SHA256SUMS.
- [x] `THIRD-PARTY/README.txt` lists every Go module and guest package, and each one has
  its license text. `package.ps1` fails on a Go module without one; `fetch-sources.sh`
  fails on a guest origin without one unless `dist/licenses/<origin>/` supplies it.
  2026-11-03, v0.1.0-rc.1: lists the 16 Go modules of hyperbutler.exe, the docker CLI and
  compose modules, WebView2Loader and 15 guest origins, each with its license files.
- [x] `NOTICE` is current: new Go modules, new guest packages.
  2026-11-03, v0.1.0-rc.1: every module `go version -m hyperbutler.exe` prints is in NOTICE.
- [x] No GPL code is linked into `hyperbutler.exe` (the SBOM lists its modules).
  2026-11-03, v0.1.0-rc.1: no GPL/LGPL/AGPL license text among the license files of the
  modules linked into hyperbutler.exe, the broker (golang.org/x/sys only) or the clients.
- [x] "Docker" is used only descriptively ("Docker Engine (Moby)"), with the trademark
  note in the README.
  2026-11-03, v0.1.0-rc.1: README uses "Docker" for the API, CLI and Docker Desktop only,
  with the trademark note.

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
7. The winget manifest: copy the previous version's folder under `dist/winget/`, set the
   version, URL and SHA-256, and submit it. Since the MSI installs the broker service (ADR
   0004, Installer (M4)) it has no `InstallerSwitches` `Custom: ADDHYPERVADMIN=1`, which
   v0.1.0-preview.1's had: winget users need no group membership.

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

### Review of the image pin and the bundled clients (2026-11-01)

A self-review, then an independent adversarial review, of the embedded image hash
(`internal/engine/imagepin.go`, `image_windows.go`), the bundled clients
(`dist/clients.lock`, `package.ps1`, `cmd/hyperbutler/dockercli_windows.go`,
`internal/engine/dockerctx/plugins.go`) and these docs.

| # | Sev | Finding | Resolution |
|---|---|---|---|
| S1 | Med | HCS opens the kernel, initrd and system disk by path after the check. With a junction or directory symlink on that path, the link could be retargeted while the checked files stay open (the open handles block a rename of the real directories, not a change of the link). | **Fixed:** the engine hands HCS the final path of each held handle (`GetFinalPathNameByHandle`), so links are resolved before the check is done. Tests: `TestOpenImageResolvesJunction`, `TestOpenImageHoldsDirectory` (the image directory and its parent cannot be renamed while held). Booted on Hyper-V with it. |
| S2 | Low | `image.json` was read by path, not held, while the other files were. | **Fixed:** opened deny-write and kept open like the others; the pinned hash is over the same bytes that are parsed. Reparse points are refused as before. |
| S3 | Low | Case, 8.3 names, streams, hard links. | **Checked:** manifest names are `[A-Za-z0-9._-]`, so no `:` streams; every check is over content, so case and short names cannot swap a file; a hard link shares the deny-write state. |
| S4 | Low | A renamed `pinnedManifest` would make `-X` a silent no-op and ship an unpinned build. | **Fixed:** `package.ps1` runs `hyperbutler.exe version --json` and fails unless it reports the pin; `TestImagePinDefault` keeps the variable a plain string. |
| S5 | Info | Whoever can write the install folder can replace `hyperbutler.exe` itself. | **Accepted** (ADR 0005, A2b): the pin stops a replaced or half-copied image, not an attacker who owns the folder. |
| M5 | Med | After a side-by-side upgrade, `restart` passed the old folder (state.json's `imageDir`) to the new pinned build, which refused it and blamed the user. | **Fixed:** a pinned build uses its own image unless `--image-dir` is given, and says so; the refusal names both folders. Test: `TestRestartImageDir`; run with two unpacked copies. |
| M6 | Med | Is "too old" really below 19.03? (Moby 29 was thought to require API 1.44.) | **Checked against the source:** moby `docker-v29.9.2`, `daemon/config/config.go`: `defaultMinAPIVersion = "1.40"`; the running guest reports `MinAPIVersion 1.40`. The warning uses 19.03 (API 1.40). The guest's dockerd environment is unchanged. |
| M7 | Med | `docker.exe` next to `hyperbutler.exe` would shadow a docker CLI installed later, since that folder is on PATH. | **Fixed:** the clients are in `clients\`, which the README adds to PATH only when there is no docker CLI; `start` and `status` print the path in use and mark it `(bundled)`. |
| M8 | Med | Docker's prebuilt Windows CLI zip has no published checksum, signature or provenance and `docker.exe` is not Authenticode-signed, so a SHA-256 pin was trust on first use; compose's checksum file comes from the same release as the binary. | **Fixed:** both are built from source in `package.ps1`: the docker/cli and docker/compose modules through the Go module proxy, checked by go against sum.golang.org and by `package.ps1` against the lock's `h1:` hash and commit, dependencies against the source's own `go.sum`, upstream tags and version flags, `CGO_ENABLED=0 -trimpath`. No binary is downloaded, so no Sigstore check is needed. Upstream's Windows docker CLI is built with cgo (only for the `osusergo` tag); this one is not. |
| L6 | Low | Windows ARM64 was not mentioned. | **Fixed:** README, above and compat.md: x64 only; the x64 programs would run under emulation, the x86_64 guest image would not boot. |
| C1 | Low | `cliPluginsExtraDirs` comes first in the CLI's plugin search, so registering the bundled compose there could shadow Docker Desktop's or the user's. | **Fixed:** it is listed only while no other `docker-compose.exe` is in the search, and taken out when one appears; `delete` removes it. Test: `TestSyncPluginDir`. |
| C2 | Low | Docker Desktop side by side: `stop` leaves the `hyperbutler` context selected and `delete` resets to `default`, not to `desktop-linux`. | **Open** ([compat.md](compat.md#the-docker-cli-and-docker-desktop), "Not covered"); not tested with Docker Desktop installed. |
| C3 | Low | After a side-by-side upgrade the old folder's `cli-plugins` stays registered until it disappears (the CLI skips missing directories, and the next `start` registers the new one). | **Accepted;** the README upgrades in place. |

### Review of diagnose, uninstall, redaction, the telemetry scan and packaging (2026-11-01)

An adversarial review of `hyperbutler diagnose` and `uninstall`, `internal/redact`,
`internal/notelemetry`, `bench/no-telemetry`, the image pin's restart path and
`package.ps1`. After the fixes `bench/uninstall` passed all 29 checks on a test
installation (Windows 11 Home, real VMs; the runtime id recorded before the grant was
the one the VM got), and `bench/no-telemetry -idle 1m` passed (vmwp identified, no
request of hyperbutler's own; the bench's pull is the user's traffic).

| ID | Finding | Outcome |
|---|---|---|
| H-1 | `files.json` in the diagnose zip carried the SHA-256 of the unredacted daemon.json and docker config.json: an offline oracle for a guessed proxy or registry password. | **Fixed:** size, BOM and CRLF stay; the hash is of the redacted copy. Test: `TestDiagnose` (no zip entry contains the original's hash). |
| M-1 | Text redaction missed `password='x'`, `export DOCKER_PASSWORD='x'`, `password = x`, `PASSWORD : x`, `token:x`, `docker login -p x`, `--password -x` and a JSON member split across lines. | **Fixed.** `-p` and `-u` are redacted only on lines with the word `login` (elsewhere `-p 8080:80` is a port), documented in the package. FuzzString has templates for each and base64 secrets. Tests: `TestString`, `FuzzString`, `TestLongLines`; `BenchmarkLog` 16.8 MB/s before, 16.3 after. |
| M-2 | The uninstall ACL sweep removed every VM account entry without a live compute system, including that of a VM another installation sharing system.vhd has granted access but not yet created (grant comes before Create), and another Windows user's VMs. | **Fixed:** the engine learns the runtime id before the first grant (a probe grant on a scratch file), records it in state.json `pastRuntimeIds` (last 16); uninstall removes only those and the last engine's, and keeps the rest with a note. Tests: `TestUninstallStaleVMAccess`, `TestGranterRecordsBeforeGrant`, `TestRecordRuntimeID`, `TestAccountSID`; `bench/uninstall`. Entries an engine from before this change left stay. |
| M-3 | An invalid settings.json made uninstall (and its dry run and `--json`) impossible. | **Fixed:** treated like a missing one; the data disk is found through state.json and the state directory; settings.json is listed. Test: `TestUninstallInvalidSettings`. |
| M-4 | Uninstall released both locks before settings.json and the directories went. | **Fixed:** only the lock files and the directory holding them go after unlock. Test: `TestUninstallRemovesSettingsUnderLock`. |
| L-1 | A dry run while the engine runs left out the access entries. | **Fixed:** listed, as checked again after the engine stops. Test: `TestUninstallDryRunWhileUp`. |
| L-2 | Uninstall removed an empty data directory the user had made (`--data D:\docker-data`). | **Fixed:** start records a data directory it creates (`createdDataDir` in settings.json, where the data directory is recorded); only that one goes. Older installs keep theirs, and the output says so. Tests: `TestUninstallCreatedDataDir`, `TestUninstallFindsDataFromState`. |
| L-3 | A read-only data.vhdx waited the full 3 minutes. | **Fixed:** fails at once and says how to clear the attribute, which it never clears. Test: `TestUninstallReadOnlyDisk`. |
| L-4 | A log tail could start mid-line. | **Fixed:** the partial first line is dropped. Test: `TestReadTail`. |
| L-5 | docker-config.json and daemon.json over 64 KiB were left out of the zip. | **Fixed:** both are read up to 1 MiB; a larger file is listed in `missing` and summary.txt with its size. Test: `TestDiagnoseLargeConfig`. |
| L-6 | The notelemetry scan missed dot imports, network tools in exec arguments (`cmd /c curl`), UDP listeners and `net.Resolve*Addr`, ws2_32.dll and dnsapi.dll, and cgo; one allowlist line covered every use in a file. | **Fixed:** each rule has a synthetic-source test; allowlist lines carry an exact use count. Tests: `TestScanGoEvasions`, `TestAllowlistCounts`, `TestNoUnreviewedNetworkUse`. |
| L-7 | `bench/no-telemetry` passed when the VM's vmwp was not found. | **Fixed:** a FAIL unless `-allow-no-vmwp`. Test: `TestNoVMWP`. |
| L-8 | settings.md's uninstall kinds lacked `docker-plugins`. | **Fixed:** the kinds are pinned, and checked against settings.md. Test: `TestUninstallKinds`. |
| L-9 | A pinned restart dropped an explicit `--image-dir` that matched the pin. | **Fixed:** an image directory that passes the pin is kept; the fallback says why. Tests: `TestRestartImageDir`, `TestCheckImageDirPin`. |
| L-10 | `TestUninstallStaleVMAccess` skipped whenever a compute system could not be opened. | **Fixed:** the live VM list and HCS revocation are test seams; the test needs no HCS. |
| L-11 | The diagnose zip's private ACL is lost on FAT32 and exFAT. | **Fixed:** diagnose checks `FILE_PERSISTENT_ACLS` on the new file and says the zip is not private; the doc comment is conditional. Test: `TestPersistentACLs` (the NTFS side; the FAT side is not tested here). |
| L-12 | `package.ps1` took GOPROXY, GOSUMDB, GONOSUMDB, GOPRIVATE, GOINSECURE and GOFLAGS from the caller for the client builds and go-winres; a `-Snapshot -Version vX` zip had a release's name. | **Fixed:** those steps run with `GOENV=off`, proxy.golang.org and sum.golang.org, the bypass variables cleared; a snapshot zip's name ends in `-snapshot`. Checked by a `-Snapshot -NoClients -Version v9.9.9` run under `GOSUMDB=off` (no test harness for the script). |
| L-13 | start recorded only the first context it replaced. | **Fixed:** every replacement of another context is recorded, and the dry run says which one is restored. Test: `TestSelectContextRecordsLatest`. |
