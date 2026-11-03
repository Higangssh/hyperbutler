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
| Start, `hyperbutler start` → `docker info` (`bench/start`, 2026-10-31) | median **1.44 s**, p95 1.45 s (n = 20 after one warm-up, warm host cache, existing disk, no other VM running). Earlier (cli-engine): first start with a new data disk 1.97 s | ≤ 8 s median, p95 ≤ 12 s | Yes, also the 1.0 target. The first start after a host reboot is not measured yet |
| `hyperbutler stop` | median 1.25 s, orderly guest shutdown every time | none | – |
| Image pull over the internet, *spike*: `hyperbutler-dev`, 1 GiB / 2 vCPU guest ([guest-networking](research/guest-networking.md)) | `alpine` 2.7–3.1 s, `ubuntu:24.04` 3.5–3.7 s, `nginx` 4.7–5.3 s, about the same as the host | none | – |
| Container networking throughput, *spike*: `hyperbutler-dev`, 2 GiB / 2 vCPU guest (`bench/net-perf`, [ADR 0006](adr/0006-guest-networking.md#gate-results-2026-10-31), 2026-10-31) | from a host server into a container **10.2 Gbit/s** (was 2.7), container to host 8.3 (3.3), `-p` 7.5 down / 11.6 up (3.2 / 4.5); 6000 connections/s (failed before); engine 0.11–0.20 cores per Gbit/s, private working set 46–55 MB under load. Small round trips p50/p99 187/392 µs | ADR 0006: ≥ 50% of raw hvsock guest to host | Yes on 5 of 6 boots (49–92%, median 69%; raw hvsock varies 8–22 Gbit/s by boot). ADR 0006 accepted; the LAN-registry gate is deferred to the release checklist (no second machine) |
| Disk space returned after `docker rmi` + `prune` (cli-engine) | 3 GiB of images deleted: the data disk shrank back to within 322 and 843 MiB of the baseline in ≤ 11 s (2 runs), with no manual step | within 1 GB in ≤ 5 min (REQ-DISK-001) | Yes for this test. The full 10 GB / 24 h bench has not been run |
| Host disk full, *spike* ([disk-reclaim](research/disk-reclaim.md#free-space-guard-req-disk-004-non-elevated-2026-10-30)) | with the free-space guard: 10/10 writes got `ENOSPC`, no file system errors, no lost files | REQ-DISK-004 | Yes, on a 3 GiB test volume in a spike harness. Not yet repeated with the `hyperbutler` CLI |
| Idle memory, minutes 5–10 (`bench/mem-idle`, [ADR 0003](adr/0003-implementation-language.md#consequences)) | **315 MiB** (331 MB): engine 17.7 MiB (Go heap 5.4 MiB), vmmem 292 MiB, vmwp 5.5 MiB, with the erofs system disk | ≤ 400 MB (REQ-MEM-001); 1.0: ≤ 250 MB | Yes for 0.1, **no** for 1.0. The engine commits 65 MiB; a process that reads all of its memory (likely Defender) can make all of it resident |
| Memory back to the host after a workload, *spike*: `spike/vhdx`, 3 GiB guest ([guest-services](research/guest-services.md)) | 2 GiB of page cache: VM back to 350 MiB (260 MiB at ready) 27 s after the read | REQ-MEM-002 | Partly: the 12 GB / 8 GB bench has not been run |
| Bind mounts from Windows paths, `docker run -v C:\…` and `--mount type=bind` ([ADR 0007](adr/0007-bind-mounts.md#opens-without-open-fuse_no_open_support-and-the-size-0-report-2026-10-31)) | work through `hyperbutler start`: read/write, `:ro`, single files; shares no container uses are removed; opens cost no round trip. `bench/fs-*`: cached reads 1.12× ext4, sequential write + fsync 278 MiB/s; lstat 2.2×, missing names 1.2×, `git status` 1.7× ext4; a Windows edit reaches inotify in p95 3.4 ms with none missed, and pinned Vite and webpack rebuild; 10k creates 11.5 s (ext4: 0.11 s) | 1.0: REQ-FS-001/002/005; REQ-FS-003 ≤ 200 ms p95 | REQ-FS-002 and REQ-FS-003 **yes**. REQ-FS-001 reads yes; REQ-FS-001 writes and REQ-FS-005 not measured (no WSL 2 for the 9P baseline) |
| Download size | the zip is 109 MB, the MSI 116 MB (snapshot of the release candidate, 2026-11-02) | ≤ 150 MB (REQ-UX-002) | Yes (measured on a local build) |
| Soak (ADR 0003), `hyperbutler start`, incl. bind mounts and `-p` | 1 h, 12,827 docker commands, none failed: engine working set +0.56 MiB/h (median 80 MiB), Go heap +0.09 MiB/h, handles, threads and goroutines flat; no leak above about 5 MiB/h | 24 h, no positive trend | 1 h main gate: passed by hand. **24 h: not run.** It blocks the first release |

What is missing for 0.1: the 24-hour soak, and the installed broker service's first run
on a clean machine (from the MSI, it replaces Hyper-V Administrators membership; the zip
still needs the membership, see below). Bind mounts from Windows paths work but are not yet fast for write-heavy
trees: use a named volume for `node_modules`. [docs/release.md](release.md) lists every
release gate.

