# Requirements

Each requirement traces to a pain point in
[research/wsl2-docker-pain-points.md](research/wsl2-docker-pain-points.md) (column "Pain").
IDs are stable once published, so never renumber them. A requirement that is no longer
wanted is marked *withdrawn*, not deleted.

- **ID format:** `REQ-<AREA>-NNN`
- **Areas:** `START`, `MEM`, `DISK`, `FS`, `NET`, `API`, `PLAT`, `PWR` (sleep/resume), `UX`
- **Priority:**
  - **P0:** required for 0.1, the first public preview. The P0 set proves the core thesis: own
    VM, disk space comes back, no WSL.
  - **P1:** required for 1.0.
  - **P2:** nice to have.
- **Targets:** Where 0.1 and 1.0 targets differ, both are given as `0.1 / 1.0`.

## How things are measured

- **Where results come from:** Every criterion names a `bench/` scenario. Most are scripts.
  Ones marked *(manual)* have a written procedure. Results include the machine, Windows
  edition and build, the hyperbutler backend, and whether Defender real-time protection was on.
- **Reference machine:** The maintainer's Windows 11 Home laptop (Core Ultra 7 155H, 32 GB,
  NVMe). `bench/` also measures Docker Desktop and Podman on the same machine. The research
  baselines marked † are not rigorous, so these runs replace them.
- **Host memory:** The sum of the Private Working Set of `hyperbutler.exe`, the VMM process (`vmmem`
  for HCS, the VMM for WHP), and any hyperbutler helper process (for example the network stack).
  It is read from performance counters.
- **Host disk usage:** Bytes actually allocated on disk (`GetCompressedFileSize`, which matches
  `fsutil file layout`), summed over the hyperbutler data directory. This is not the file length.
- **Defender:** Disk and file-sharing criteria are measured with Defender real-time protection
  **on**, with no exclusions, unless a row says otherwise.

## Disk

| ID | Requirement | Acceptance criterion (how measured) | Pri | Pain |
|---|---|---|---|---|
| REQ-DISK-001 | Disk space freed in the guest is returned to the host automatically, while the VM is running. | Pull/build 10 GB of images from a local registry, delete them, then run `docker system prune -af`. Host disk usage falls back to within 1 GB of the pre-test baseline in ≤ 5 min, and is still there after 24 h. No manual step, no VM restart, no admin (`bench/disk-reclaim`). | P0 | D1 D2 D3 D4 |
| REQ-DISK-002 | Reclaiming disk space needs neither admin rights nor Hyper-V tools. | REQ-DISK-001 passes from a standard user account on Windows Home. Neither the Hyper-V PowerShell module nor `diskpart` is ever invoked (`bench/disk-reclaim` run as a standard user). HCS on Home needs Administrators or Hyper-V Administrators ([ADR 0002](adr/0002-primary-vm-backend.md)). So "standard user" here means one served by the broker in [ADR 0004](adr/0004-hcs-access-broker.md), and the reclaim path itself (`fstrim` in the guest) needs no host privilege. | P0 | D3 P1 P2 |
| REQ-DISK-003 | Reclaiming space never risks data integrity. | **0.1 smoke:** 100 rounds of write, delete, and reclaim, with the VMM process killed (`TerminateProcess`) at a random point in each round. Every round ends with 0 errors from `e2fsck -n`, and no committed image is lost. An image is committed if its `docker pull`/`build` exited 0 before the kill. **1.0:** 1,000 kill rounds, plus 200 power-loss replays inside the guest (`dm-log-writes`), plus 10 documented hard-reset cycles *(manual)* (`bench/disk-torture`). | P0 smoke / P1 full | D4 |
| REQ-DISK-004 | A full host disk is a safe, recoverable state. | When the host volume fills during writes, the guest gets `ENOSPC` and no data file is corrupted. After space is freed, `docker ps` works again without reformatting or deleting data (`bench/disk-full`). | P0 | D5 |
| REQ-DISK-005 | Real usage is reported, and a hard cap is enforced. | `hyperbutler status` shows host disk usage to within ±1%. A configured cap is never exceeded by more than 1%. Writes are refused before the host volume drops below a reserve (default 5 GB) (`bench/disk-cap`). | P1 | D5 |
| REQ-DISK-006 | Build cache stays bounded by default. | With default settings, 30 `docker compose build` cycles of a reference project keep BuildKit cache at ≤ 10 GB, or the configured limit (`bench/buildcache`). | P1 | D2 |
| REQ-DISK-007 | The data location can be chosen at first run and moved safely later, without admin. | `hyperbutler start --data D:\hyperbutler` works. `hyperbutler move` runs as a standard user. Killed at any point (once every 1 s across a run), it leaves either the old or the new location fully usable (`bench/disk-move`). | P1 | D6 |
| REQ-DISK-008 | Upgrades don't leak disk. | After upgrading hyperbutler, host disk usage grows by no more than the size of the new guest image. Old guest images are removed (`bench/upgrade`). | P1 | D2 |

## Start-up and memory

| ID | Requirement | Acceptance criterion (how measured) | Pri | Pain |
|---|---|---|---|---|
| REQ-START-001 | Fast start. | From `hyperbutler start` (no VM running) until `docker info` succeeds: median ≤ 8 s / ≤ 5 s, p95 ≤ 12 s / ≤ 8 s, over 20 runs. The host file cache is warm: discard one warm-up run per boot (`bench/start`). Measure the first start after a reboot separately and report it. | P0 | F3 |
| REQ-MEM-001 | Small idle footprint. | Host memory, sampled every 10 s from minute 5 to minute 10 of idle, with the VM running and no containers. Median ≤ 400 MB / ≤ 250 MB (`bench/mem-idle`). | P0 | M2 |
| REQ-MEM-002 | Memory goes back to the host after workloads stop. | The VM is configured with a 12 GB maximum. A container fills 4 GB of anonymous memory plus 4 GB of page cache, then exits. Within 60 s, without restarting the VM, host memory returns to ≤ the REQ-MEM-001 target + 100 MB (`bench/mem-reclaim`). | P1 | M1 |
| REQ-MEM-003 | The VM can be capped without breaking workloads. | With `memory: 2GB`, a container that allocates 3 GB is OOM-killed inside the guest. The VM and the other containers keep running (`bench/mem-cap`). | P1 | M1 |
| REQ-MEM-004 | Automatic idle suspend. | With no containers running for N minutes (default 10), the VM is paused or stopped, and host memory falls to ≤ 20 MB. The next `docker` command resumes it in ≤ 3 s (`bench/idle-suspend`). | P2 | M1 M2 |

Status on the reference machine (2026-10-31, `hyperbutler start`, 4 GiB / 8 vCPU guest; details in
[ADR 0003](adr/0003-implementation-language.md#consequences)):
- REQ-START-001: **met** for 0.1 and 1.0 (`bench/start`): median 1.44 s, p95 1.45 s over 20 runs
  after one warm-up, no other VM running; `hyperbutler stop` median 1.25 s. The first start
  after a reboot is still not measured (the host had been up 20 h).
- REQ-MEM-001: **met for 0.1, not for 1.0** (`bench/mem-idle`): median 315 MiB (331 MB) =
  engine 17.7 + vmmem 292 + vmwp 5.5 MiB, no other VM running. A process that reads the
  engine's whole memory makes its whole commit (about 66 MiB) resident; the engine
  empties its working set a few seconds after that
  ([research/mem-reclaim.md](research/mem-reclaim.md), section 3).
- REQ-MEM-002: **met for 0.1 unless something reads the engine's memory in the 60 s
  window** (`bench/mem-reclaim`, 2026-10-31 to 11-01, 12 GiB guest, 4 + 4 GiB workload,
  release configuration; details in [research/mem-reclaim.md](research/mem-reclaim.md)).
  Without such a read: 5 of 5 runs, total at +60 s 441.8 to 457.3 MiB, highest sample
  from +60 s to +180 s 461.5 MiB, against 476.8 MiB (500 MB). With a read provoked by the
  old PowerShell sampler: 2 of 3; the read holds the engine at its full commit and the
  total 13 to 27 MiB over the limit for about 4 s until the engine's working-set guard
  trims, and in the failed run the trim landed at +60.1 s. The VM side: since the guest
  reports free memory in single pages (`page_reporting_order=0`), `vmmem` + `vmwp` are
  back within about 20 MiB of their idle level by +45 s. The engine side: its working set
  used to jump once by 46 MiB to its full commit and stay there. The trigger was the
  bench itself: sampling through a hidden PowerShell per sample made an outside process
  (not identified; likely the antivirus) read the engine's whole address space 300 s
  after its start, in 19 of 21 runs; with in-process sampling, in none of 26 (one partial
  read in a `bench/mem-idle` run). Linking Go's frozen crypto module would halve the
  commit and keep even a read under the limit, but was not chosen (it receives no fixes,
  and an HTTPS update check is planned for 0.2). Open: a shorter guard interval (1 s)
  would shrink the over-limit transient to about a second. Idle at 12 GiB is about 430
  to 450 MiB, mostly `mem_map` (192 MiB), so the 1.0 limit (350 MB) is out of reach at
  this size. The bench reads "returns to" as "and stays there until +180 s".
- REQ-MEM-001 re-run with the REQ-MEM-002 kernel command line (4 GiB, `bench/mem-idle`):
  309.6 MiB (325 MB), still met for 0.1. A run before it read 359.5 MiB because of the same
  engine rise (312.7 MiB before it). With the release configuration (2026-11-01,
  in-process sampling): 296.7 MiB (311 MB), engine 18.8 MiB, commit 66.6 MiB.

## File sharing

| ID | Requirement | Acceptance criterion (how measured) | Pri | Pain |
|---|---|---|---|---|
| REQ-FS-001 | Fast sequential I/O on bind mounts from Windows paths. | Sequential reads of a bind-mounted Windows file are within 2× of the guest's own ext4 once cached. Sequential writes are at least 3× as fast as WSL 2's 9P mount of the same directory on the same machine (`bench/fs-seq`). *Amended 2026-10-31 ([ADR 0007](adr/0007-bind-mounts.md)): no mechanism HCS offers gets writes within 2× of ext4, so writes are measured against the 9P baseline.* | P1 | F1 |
| REQ-FS-002 | Fast metadata reads on bind mounts. | On a bind-mounted Windows checkout of a pinned reference repo, once the guest has seen the tree: `lstat`, negative lookups and `git status` are each within 3× of the same work on guest ext4 (`bench/fs-meta`). *Amended 2026-10-31 (ADR 0007): creates and deletes moved to REQ-FS-005.* | P1 | F1 |
| REQ-FS-003 | File changes made on Windows reach containers as inotify events. | When a Windows process edits a bind-mounted file, the container gets an `IN_MODIFY` event within 200 ms p95, without polling. A scripted check confirms that pinned versions of Vite and webpack hot-reload with default config (`bench/fs-notify`). | P1 | F2 |
| REQ-FS-004 | Mounts behave correctly on Windows paths. | The `bench/fs-compat` suite passes. It covers case-sensitivity rules, symlinks, permission mapping, paths longer than 260 characters, and paths with spaces or non-ASCII characters. | P1 | F1 |
| REQ-FS-005 | Creates and deletes on bind mounts beat 9P. | On the REQ-FS-002 checkout, `npm ci` and creating, stat-ing and deleting 10k files are each at least 3× as fast as on WSL 2's 9P mount of the same directory on the same machine (`bench/fs-meta`). The docs point write-heavy trees (`node_modules`) at a named volume, or at the sync mode once it exists. *Added 2026-10-31 (ADR 0007): the write half of the old REQ-FS-002.* | P1 | F1 |

Status of the bind-mount requirements, measured with `bench/fs-*` through `hyperbutler start`
on the reference machine (2026-10-31, [ADR 0007](adr/0007-bind-mounts.md#performance-pass-and-benchfs--2026-10-31)):
- REQ-FS-001: cached reads met (1.12× ext4). Writes not measured against 9P (no WSL on
  the machine); 259 MiB/s against the spike's 191 MB/s for HCS Plan9 would not meet 3×.
- REQ-FS-002: met. lstat 2.2×, missing names 1.2×, and `git status` 1.7× ext4
  (2.39 ms against 1.48 ms, median of 3 runs; it was 3.9× before opens stopped costing a
  round trip, [ADR 0007](adr/0007-bind-mounts.md#opens-without-open-fuse_no_open_support-and-the-size-0-report-2026-10-31)).
- REQ-FS-003: met (IN_MODIFY p95 3.4 ms, none missed; pinned Vite and webpack rebuild,
  measured before the change, not re-run).
- REQ-FS-005: not measured against 9P. Against the spike's HCS Plan9 numbers, 10k
  creates, stats and deletes are 4.4×, 15× and 5.8× faster; `npm ci` has no baseline.

Backlog for bind mounts (work items, not requirements; ADR 0007):
- creates and 4 KiB writes are round-trip bound (LOOKUP, GETATTR of the directory, CREATE,
  WRITE per create); nothing on the server side removes those;
- a 9P baseline run (`-wsl`) on a machine with WSL 2, for REQ-FS-001 writes and REQ-FS-005;
- `go test -race ./internal/share/...` in CI (no cgo toolchain on the dev machine).

Done (2026-10-31): the guest readdir cache is reused (warm walks 1.2–1.7× ext4, from 54×);
shares no container uses are removed (integration step 5); sequential writes 56 → 260–300
MB/s (the guest relay waited for the Go runtime's 10 ms background poll); opens without
a round trip (`FUSE_NO_OPEN_SUPPORT` with a host handle cache that closes idle handles after
100 ms), so `git status` 3.9× → 1.7× ext4.

## Networking

| ID | Requirement | Acceptance criterion (how measured) | Pri | Pain |
|---|---|---|---|---|
| REQ-NET-001 | Published ports work on loopback. | `docker run -p 8080:80` is reachable from Windows at `127.0.0.1:8080`, `[::1]:8080`, and `localhost:8080`. **0.1:** 1 h at 10k connections/min with 0 failures. **1.0:** the same for 24 h, plus no growth in CLOSE_WAIT connections (`bench/net-ports`). | P0 | N1 |
| REQ-NET-002 | VPN and corporate DNS work with zero configuration. | With at least one common VPN client connected (tested clients are listed in `docs/compat.md`), containers reach VPN routes. Split DNS works: VPN suffixes resolve through the VPN's resolver and everything else through the default one. Connecting or disconnecting the VPN needs no hyperbutler restart (`bench/net-vpn`, *manual*). | P1 | N2 |
| REQ-NET-003 | Published ports can be exposed on the LAN, opt-in. | Behind a flag, published ports are reachable from another machine at the host's LAN IP. Off by default (`bench/net-lan`). | P2 | N1 |
| REQ-NET-004 | Containers reach the host as `host.docker.internal`. | As in Docker Desktop, `host.docker.internal` (and `--add-host NAME:host-gateway`) resolves in every container, and a connection to it on any TCP port reaches a service listening on the Windows host's `127.0.0.1` on that port. It reaches nothing else: not other host addresses, not the LAN. A setting turns it off or limits it to listed ports (`bench/host-gateway`). | P0 | N1 |

Status on the reference machine (2026-10-31, `hyperbutler-dev` with the user-mode stack of
[ADR 0006](adr/0006-guest-networking.md#gate-results-2026-10-31), 2 GiB / 2 vCPU guest, and
`hyperbutler start` for the checks):
- REQ-NET-001: **works, the criterion is not run.** `-p` serves on `127.0.0.1` and `[::1]`.
  `bench/net-perf` measured 5475 connections/s through `-p` for 5 s with no errors, and
  7.5/11.6 Gbit/s down/up. The 1 h run at 10k connections/min (`bench/net-ports`) does
  not exist yet.
- REQ-NET-002: **met for Tailscale** (manual): split DNS, public DNS and tailnet
  addresses from a container in one run. Other VPN clients are untested.
- REQ-NET-003: not run (no second machine).
- REQ-NET-004: **met** (`bench/host-gateway`, all checks pass): `host.docker.internal`,
  `gateway.docker.internal` and `--add-host NAME:host-gateway` reach `127.0.0.1` (and
  `[::1]`-only servers) on any port, TCP and UDP, from the default bridge and a user network;
  other subnet and host addresses are refused; `hostGateway.enabled` and `hostGateway.ports`
  apply live (0.2 s, same VM). The host's LAN address against a `0.0.0.0` server (`-wildcard`)
  was not run.

## Docker API compatibility

| ID | Requirement | Acceptance criterion (how measured) | Pri | Pain |
|---|---|---|---|---|
| REQ-API-001 | The stock Docker CLI and Compose work unchanged. | Through the auto-registered `hyperbutler` context, the stock `docker` and `docker compose` CLIs pass `bench/compat`. **0.1** covers run, exec, logs, pull, build (BuildKit), volumes, and compose up/down. **1.0** adds attach, push, networks, and compose watch. Known gaps are listed in `docs/compat.md`. | P0 | U3 |
| REQ-API-002 | Common tools that talk to the Docker API work. | VS Code Dev Containers, Testcontainers (Java/Go/Node), and the Docker SDK for Python and Go pass their own basic samples against the `npipe` endpoint (`bench/compat-tools`). | P1 | U3 |

## Platform

| ID | Requirement | Acceptance criterion (how measured) | Pri | Pain |
|---|---|---|---|---|
| REQ-PLAT-001 | Works on Windows 11 Home. | All P0 criteria pass on Home ([ADR 0001](adr/0001-windows-home-first-class.md)). | P0 | P1 |
| REQ-PLAT-002 | Doesn't depend on WSL. | All P0 criteria pass with the WSL feature disabled. Installing, updating, or uninstalling WSL never changes how hyperbutler behaves (`bench/no-wsl`). | P0 | U1 |
| REQ-PLAT-003 | No admin rights after the one-time setup. | The one-time setup enables the required Windows features and installs the HCS broker service ([ADR 0004](adr/0004-hcs-access-broker.md)). After that, install, start, stop, upgrade, and uninstall of the CLI and guest images all work as a standard user (`bench/noadmin`). The only exceptions are a release that changes the broker itself and removing the broker service; each needs admin once. | P1 | P2 |
| REQ-PLAT-004 | Coexists with WSL, Docker Desktop, and Windows Sandbox. | With those installed and running, hyperbutler and they keep working and neither breaks the other (`bench/coexist`, *manual*). | P1 | P5 |
| REQ-PLAT-005 | Upgrades keep data, and recovery never needs a destructive reset. | Upgrading across every release since the last minor keeps images, volumes, and containers. No documented recovery step deletes user data (release checklist). | P1 | U1 |
| REQ-PLAT-006 | Runs natively on Windows on ARM64. | P0 criteria pass on an ARM64 Windows 11 device with an arm64 guest. | P2 | P4 |
| REQ-PLAT-007 | Usable under real-time antivirus. | With Defender real-time protection on and no exclusions, REQ-FS-002 and REQ-START-001 still pass within 1.5× of their limits (`bench/av`). Evidence for P3 is weak (†), so the first `bench/av` run also checks whether this requirement is needed. | P1 | P3 |

## Sleep and resume

| ID | Requirement | Acceptance criterion (how measured) | Pri | Pain |
|---|---|---|---|---|
| REQ-PWR-001 | The guest clock stays correct across sleep. | Within 2 s of the host resuming from sleep or hibernate, the guest clock is within 1 s of the host (`bench/resume`, *manual*). | P1 | S1 |
| REQ-PWR-002 | No hangs or CPU spikes after resume. | Over 50 sleep/resume cycles, the `docker` CLI responds within 3 s after each resume, and VM CPU drops below 2% within 10 s (`bench/resume`, *manual*). | P1 | S2 |

## User experience

| ID | Requirement | Acceptance criterion (how measured) | Pri | Pain |
|---|---|---|---|---|
| REQ-UX-001 | One command to start, no tuning. | From a fresh install, `hyperbutler start` then `docker run hello-world` works with no config file and no other steps. The `hyperbutler` docker context is registered and selected (`bench/firstrun`). | P0 | U3 |
| REQ-UX-002 | Lightweight install. | The installer is ≤ 150 MB. With default settings and no GUI window open, no GUI or tray process is running. At most 3 resident host processes while running: `hyperbutler.exe`, the VMM (or `vmmem`), and one optional helper (`bench/footprint`). The budget is measured with the GUI closed; with the GUI open it is measured and reported separately ([ADR 0008](adr/0008-settings-gui-mcp.md)). | P1 | U2 A5 |
| REQ-UX-003 | Failures are diagnosable. | `hyperbutler doctor` detects and explains, with a fix, at least: missing Windows features, virtualization disabled in firmware, a conflicting hypervisor, a full host disk, and a named-pipe conflict (`bench/doctor`). | P1 | U1 P5 |
| REQ-UX-004 | No account. | No feature needs a Docker, hyperbutler, or any other account. Registry logins stay the docker CLI's business (review of every release). | P0 | A4 |
| REQ-UX-005 | No telemetry. | With default settings, hyperbutler makes no network requests of its own except the update check, and that check can be turned off. Image pulls and container traffic are the user's. Verified by `bench/no-telemetry` (the engine's sockets and the guest network's flow log over an idle run, *manual*) and in CI by `internal/notelemetry` (no unreviewed network API in production code). | P0 | A12 |
| REQ-UX-006 | Every setting has a command. | `hyperbutler config list/get/set/unset/path` reads and changes every user setting. `list` shows each key's value, default, source and when it applies. `set` validates first, writes atomically and says what restart, if any, it needs; it never restarts silently. An invalid file is never overwritten (unit and fuzz tests). | P0 | A6 |
| REQ-UX-007 | Docker Engine settings pass through. | A user `daemon.json` reaches the guest's dockerd byte for byte. It is validated before use; keys hyperbutler owns are refused with a message; an invalid file leaves the previous one in effect. Applying it restarts dockerd only. `insecure-registries` to a LAN registry works end to end (`bench/daemon-json`). | P0 | A7 |
| REQ-UX-008 | Machine-readable output. | `status`, `config list/get`, `version` and `diagnose` take `--json` with a documented, versioned schema, so scripts and AI tools can use them (unit tests on the schema). | P0 | A17 |
| REQ-UX-009 | Corporate CA works. | Behind a TLS-inspecting proxy whose root CA is in the Windows certificate store, `docker pull` works with no extra steps once `ca.import` is on (default on). A manual proxy for dockerd is a setting (`bench/corp-proxy`, *manual*). | P0 | A8 N2 |
| REQ-UX-010 | Clean uninstall. | `hyperbutler uninstall --dry-run` lists everything hyperbutler created (data, settings, docker context, logs, ACLs, tasks); `uninstall` removes it, keeping data only if asked. Afterwards a dry run lists nothing (`bench/uninstall`). | P0 | A15 |
| REQ-UX-011 | On-demand GUI. | A GUI shows status, containers (grouped by Compose project) with start/stop, logs, images, volumes, real disk and memory use, settings and `daemon.json`, and troubleshooting. It is not Electron, does not poll while hidden, and leaves no process behind within 2 s of its window closing (`bench/footprint`). A tray icon, if offered, is off by default. | P1 | A5 A17 |
| REQ-UX-012 | MCP server for AI tools. | `hyperbutler mcp` serves the Model Context Protocol over stdio and exits with its client, so it adds no resident process. Read-only tools by default (status, containers, logs, images, disk and memory use, settings, diagnose); tools that change state need `--allow-write`, and deletions need their own flag. Secrets are redacted from what it returns (`bench/mcp` and a review). | P1 | A17 |
| REQ-UX-013 | The GUI speaks the user's language; logs stay English. | Every GUI string comes from a message catalog. English and Korean ship with the first GUI, and Japanese and Simplified Chinese by 1.0. The language follows Windows and can be overridden. Logs, `diagnose`, CLI output and MCP responses stay English. Each GUI error shows a stable error code and can copy its English original ([ADR 0008](adr/0008-settings-gui-mcp.md) "Languages"; a catalog completeness test and a pseudo-locale layout check). | P1 | A17 |

Status of the settings requirements (2026-10-31, `hyperbutler start`; details in
[settings.md](research/settings.md)):

- REQ-UX-006: met. `config list/get/set/unset/path` over settings.json, with apply time and
  source per key; `set` validates, writes atomically under the ops lock and restarts only
  with `--apply`; an invalid file is never written (unit and fuzz tests). `config edit` and
  `reset` are 0.2.
- REQ-UX-007: met on one machine. daemon.json reaches dockerd byte for byte over the guest
  control channel; owned keys and invalid files are refused and the previous file stays in
  effect; `config daemon apply` restarts dockerd only (same VM, same guest boot, 1.9 s).
  `bench/daemon-json` ran against an HTTP registry at a non-loopback guest address; a
  registry on a second LAN machine is not tested yet.
- REQ-UX-008: met for `status`, `config list/get/path` and `version` (`"schema": 1`, field
  names pinned by tests). `diagnose` does not exist yet.

Status of REQ-UX-004 and REQ-UX-005 (2026-11-01, for 0.1):

- REQ-UX-004: met (review of the tree). No command, setting or guest component asks for or
  stores an account. The Docker API proxy passes `X-Registry-Auth` and every other header
  through unchanged, so `docker login` and pulls stay the docker CLI's and dockerd's
  business. `internal/engine/dockerctx` reads the user's `config.json` only for
  `currentContext` and writes back every other key (credentials included) untouched. The
  only "account" the tool mentions is the Windows account's Hyper-V Administrators
  membership (until ADR 0004's broker) and the per-VM virtual account HCS uses.
- REQ-UX-005: met. There is no update check in 0.1 (it comes in 0.2, with its off
  switch). Statically, `internal/notelemetry` fails `go test ./...` on any network API in
  production code (Windows and guest) that its allowlist does not justify, on a linked
  module nobody reviewed, and on a guest init that runs a network tool or turns on
  dockerd's `metrics-addr` or OpenTelemetry export. Today's network uses are the engine's
  HTTP clients to dockerd over hvsock, the guest network's dialer and resolver (the
  guest's own traffic) and the host gateway's loopback dialer. The guest runs no NTP or
  DHCP client (its clock comes from Hyper-V's time sync, `hv_utils`; its address is on
  the kernel command line), and dockerd gets no daemon.json by default, so its metrics
  endpoint is off. At run time, `bench/no-telemetry` (5 min idle with one container run,
  engine socket tables every 250 ms and the guest network's flow log) saw nothing on two
  runs: no socket of the engine, its children or the VM's vmwp other than the pull's,
  no guest connection, UDP flow or DNS query in the boot, idle and container phases, and
  no guest frame at all at boot. The only remote endpoints were the pull of
  `busybox:1.37` the bench made itself (registry-1.docker.io, auth.docker.io,
  production.cloudfront.docker.com over 443), shown and not counted.

## Non-goals

- Windows containers (process-isolated or Hyper-V-isolated Windows images).
- A resident GUI. A GUI exists only as an on-demand program that exits when its window
  closes, and a tray icon, if any, is off by default ([ADR 0008](adr/0008-settings-gui-mcp.md)).
- Account sign-in, telemetry, forced or silent updates, features gated by plan, an
  extensions marketplace, or a built-in AI assistant. AI tools connect through the MCP
  server (REQ-UX-012) instead.
- Bundled Kubernetes. Users can run kind or k3d on top.
- GPU passthrough before 1.0.
- Being a general-purpose Linux dev environment that replaces WSL.
- Windows 10.
