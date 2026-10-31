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
  engine 17.7 + vmmem 292 + vmwp 5.5 MiB, no other VM running. The engine's commit is
  65 MiB; a process that reads its whole memory (likely Defender) can make all of it
  resident, which would add up to about 47 MiB.
- REQ-MEM-002: **not run.** Its method is defined, but `bench/mem-reclaim` (12 GB VM, 4 GB
  anonymous + 4 GB page cache) does not exist yet.

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
| REQ-UX-002 | Lightweight install. | The installer is ≤ 150 MB. There is no GUI and no tray process. At most 3 resident host processes while running: `hyperbutler.exe`, the VMM (or `vmmem`), and one optional helper (`bench/footprint`). | P1 | U2 |
| REQ-UX-003 | Failures are diagnosable. | `hyperbutler doctor` detects and explains, with a fix, at least: missing Windows features, virtualization disabled in firmware, a conflicting hypervisor, a full host disk, and a named-pipe conflict (`bench/doctor`). | P1 | U1 P5 |

## Non-goals

- Windows containers (process-isolated or Hyper-V-isolated Windows images).
- A GUI or tray app.
- Bundled Kubernetes. Users can run kind or k3d on top.
- GPU passthrough before 1.0.
- Being a general-purpose Linux dev environment that replaces WSL.
- Windows 10.
