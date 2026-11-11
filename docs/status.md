# Status

What hyperbutler aims for, and what is measured so far.

## Goals

| Target | 0.1 preview | 1.0 |
|---|---|---|
| Disk space | returned to the host automatically, no manual compaction, no admin | same |
| WSL | not required, and unaffected by WSL updates | same |
| Start until `docker info` works | ≤ 8 s median | ≤ 5 s |
| Idle host RAM | ≤ 400 MB | ≤ 250 MB |
| Bind-mount I/O | n/a | within 2× of in-VM ext4; metadata within 3× |
| `docker` CLI and Compose | core commands | full |
| Windows editions | Home is first-class | Home, Pro, Enterprise |
| Download size | ≤ 150 MB | same |

These are targets. The next section says which ones are met. The full list is in
[docs/requirements.md](requirements.md).

## Current status

Measured on one machine: Windows 11 Home 26200, Core Ultra 7 155H, non-elevated account in
Hyper-V Administrators. The `hyperbutler` CLI rows used a 4 GiB / 8 vCPU guest; rows marked
*spike* come from development harnesses with other guest sizes, as noted. One machine and
one Windows build is not a general result. The details and methods are in the linked
documents.

| Area | Result | Target | Met? |
|---|---|---|---|
| Start, `hyperbutler start` → `docker info` (`bench/start`, 2026-11-03, v0.1.0-rc.1 through the broker service) | median **1.44 s**, p95 1.45 s, stop 1.56 s (preview.2: 1.94 s, of which 0.42 s waited on the guest's first disk report, fixed in rc.1); 2026-11-01 in process, without the broker: 1.48 s (n = 20 after one warm-up, warm host cache, existing disk, no other VM running). Earlier (cli-engine): first start with a new data disk 1.97 s | ≤ 8 s median, p95 ≤ 12 s | Yes, also the 1.0 target. The first start after a host reboot is not measured yet |
| `hyperbutler stop` | median 1.25 s, orderly guest shutdown every time | none | – |
| Image pull over the internet, *spike*: `hyperbutler-dev`, 1 GiB / 2 vCPU guest ([guest-networking](research/guest-networking.md)) | `alpine` 2.7–3.1 s, `ubuntu:24.04` 3.5–3.7 s, `nginx` 4.7–5.3 s, about the same as the host | none | – |
| Container networking throughput, *spike*: `hyperbutler-dev`, 2 GiB / 2 vCPU guest (`bench/net-perf`, [ADR 0006](adr/0006-guest-networking.md#gate-results-2026-10-31), 2026-10-31) | from a host server into a container **10.2 Gbit/s** (was 2.7), container to host 8.3 (3.3), `-p` 7.5 down / 11.6 up (3.2 / 4.5); 6000 connections/s (failed before); engine 0.11–0.20 cores per Gbit/s, private working set 46–55 MB under load. Small round trips p50/p99 187/392 µs | ADR 0006: ≥ 50% of raw hvsock guest to host | Yes on 5 of 6 boots (49–92%, median 69%; raw hvsock varies 8–22 Gbit/s by boot). ADR 0006 accepted; the LAN-registry gate is deferred to the release checklist (no second machine) |
| Disk space returned after `docker rmi` + `prune` (cli-engine) | 3 GiB of images deleted: the data disk shrank back to within 322 and 843 MiB of the baseline in ≤ 11 s (2 runs), with no manual step | within 1 GB in ≤ 5 min (REQ-DISK-001) | Yes for this test. The full 10 GB / 24 h bench has not been run |
| Host disk full, *spike* ([disk-reclaim](research/disk-reclaim.md#free-space-guard-req-disk-004-non-elevated-2026-10-30)) | with the free-space guard: 10/10 writes got `ENOSPC`, no file system errors, no lost files | REQ-DISK-004 | Yes, on a 3 GiB test volume in a spike harness. Not yet repeated with the `hyperbutler` CLI |
| Idle memory, minutes 5–10 (`bench/mem-idle`, [ADR 0003](adr/0003-implementation-language.md#consequences), 2026-11-03, v0.1.0-rc.1 from the MSI) | **319.3 MiB** (335 MB): engine 20.5 MiB, vmmem 286.9 MiB, vmwp 5.0 MiB, broker service 6.9 MiB (preview.2: 360.8 MiB, its broker 38.8 MiB until the service ran the working-set guard). Without the broker (the zip), 2026-11-01: 313.9 MiB | ≤ 400 MB (REQ-MEM-001); 1.0: ≤ 250 MB | Yes for 0.1, **no** for 1.0. The engine commits 65 MiB; a process that reads all of its memory (likely Defender) can make all of it resident |
| Memory back to the host after a workload, *spike*: `spike/vhdx`, 3 GiB guest ([guest-services](research/guest-services.md)) | 2 GiB of page cache: VM back to 350 MiB (260 MiB at ready) 27 s after the read | REQ-MEM-002 | Partly: the 12 GB / 8 GB bench has not been run |
| Bind mounts from Windows paths, `docker run -v C:\…` and `--mount type=bind` ([ADR 0007](adr/0007-bind-mounts.md#opens-without-open-fuse_no_open_support-and-the-size-0-report-2026-10-31)) | work through `hyperbutler start`: read/write, `:ro`, single files; shares no container uses are removed; opens cost no round trip. `bench/fs-*`: cached reads 1.12× ext4, sequential write + fsync 278 MiB/s; lstat 2.2×, missing names 1.2×, `git status` 1.7× ext4; a Windows edit reaches inotify in p95 3.4 ms with none missed, and pinned Vite and webpack rebuild; 10k creates 11.5 s (ext4: 0.11 s) | 1.0: REQ-FS-001/002/005; REQ-FS-003 ≤ 200 ms p95 | REQ-FS-002 and REQ-FS-003 **yes**. REQ-FS-001 reads yes; REQ-FS-001 writes and REQ-FS-005 not measured (no WSL 2 for the 9P baseline) |
| Download size | v0.2.0: the zip is 146.1 MB, the MSI 146.2 MB (v0.1.0-preview.2: 120.3 and 125.4 MB; the bundled buildx plugin is most of the growth). The sandbox image's MSI, fetched only on request, is 271.5 MB | ≤ 150 MB (REQ-UX-002) | Yes, with 4 MB to spare |
| Soak (ADR 0003), `hyperbutler start`, incl. bind mounts and `-p` | 1 h (2026-11-01, before the broker), 12,327 docker commands, none failed: the raw private working set rose 3.4 MiB (allowed 2.0), which is Go reusing pages it had returned; with them added back (`ws_private_adj`) +0.70 MiB against 2.24 MiB. Private bytes, handles, threads, goroutines, heap, vmmem and vmwp flat ([release.md](release.md)) | 24 h, no positive trend | **24 h, 2026-11-03 to 11-04, v0.1.0-rc.1 from the MSI:** 261,338 docker commands, none failed; engine memory, handles, threads and goroutines flat. Two flags explained, not leaks: the Go heap swings with no trend, and the broker keeps a bounded number of Windows RPC thread handles ([release.md](release.md)). Accepted for v0.1.0. **24 h, 2026-11-10 to 11-11, v0.2.0:** 292,074 commands, none failed; commit, heap, handles, threads and goroutines flat; one working-set step after a Defender update, no change in private bytes. Accepted for v0.2.0 |

Released as v0.1.0 (2026-11-04) without these tests ([docs/release.md](release.md) lists every gate):

- the MSI and its broker service on a clean PC (install, upgrade from preview.1, repair and
  uninstall ran on one machine);
- a real sleep and resume (S3 / Modern Standby), tested in unit tests only;
- the first start after a host reboot, with the broker (idle memory and warm start: done);
- `bench/noadmin`, `bench/net-vpn` and `bench/coexist`;
- REQ-MEM-002 (12 GB guest), the host-disk-full test with the `hyperbutler` CLI, and the
  deferred commit under host memory pressure;
- the integration tests on real hardware (the p99.9 latency tail of ADR 0006 is recorded, not a gate: it is inside the VM, about 10 ms for one request in a thousand while a bulk connection starts);
- the 45 MB working set seen under the soak, profiled and explained.

v0.1.1 (2026-11-05) is a patch release without a new soak: the share file server's change
of direction (bind mounts about twice as fast, [ADR 0007](adr/0007-bind-mounts.md)), the
broker's slots for sessions (off until the sandbox image exists,
[ADR 0009](adr/0009-agent-sandbox.md)) and a retry in the broker client. It was tested from
the release package, through the engine alone and through a foreground broker: `start`,
`hello-world`, a bind mount read and write (about 500 and 880 MB/s for 128 MiB), Compose
with a published port and a bind mount, `stop`. The next soak runs before v0.2.

v0.2.0-rc.1 (2026-11-08) is a pre-release with what came after v0.1.1: `hyperbutler sandbox`
([docs/sandbox.md](sandbox.md), [ADR 0009](adr/0009-agent-sandbox.md)), whose image is a second
MSI that `hyperbutler sandbox install` fetches, with headless Chromium in a session; containers
in `hyperbutler import` (REQ-UX-024); the bundled buildx plugin, so `docker build` uses
BuildKit; `bench/compat-tools` ([compat.md](compat.md)), nine Testcontainers and SDK samples.
Tested on one machine before the tag: both MSIs installed, repaired, upgraded and removed in
each order; a session through the installed broker service; the terminal (keys, resize,
Ctrl+C, leaving and coming back) through a real pseudo console; Playwright's Chromium on
github.com in `dev` mode. Its 24 h soak (2026-11-08 to 11-09, 283,947 docker commands, none
failed) found a slow leak in the share server, about 3.6 MiB of Go heap a day from names
answered "missing" being kept after their bind mount was gone; it is fixed in v0.2.0,
which ships on this run and the fix, and the next 24 h soak runs against v0.2.0
([release.md](release.md)).

v0.2.0 (2026-11-09) is that release candidate with the leak fix. Its own MSIs were checked
before publishing: the Quick Start on a second standard account outside Hyper-V
Administrators, through the broker service, and a sandbox session. Its own 24 h soak
(2026-11-10 to 11-11, 292,074 docker commands, none failed) showed engine commit, Go heap,
goroutines, handles and threads flat; the tool flagged one 14 MiB working-set step that
followed a Defender update and the engine's working-set guard, with no change in private
bytes. Accepted as the gate on 2026-11-12 ([release.md](release.md)).

Bind mounts from Windows paths work but are not yet fast for write-heavy trees: use a
named volume for `node_modules`.
