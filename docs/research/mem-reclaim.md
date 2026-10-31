# Memory reclaim after a workload (REQ-MEM-002)

- Date: 2026-10-31
- Requirement: REQ-MEM-002. The VM is configured with a 12 GB maximum; a container fills
  4 GB of anonymous memory plus 4 GB of page cache, then exits; within 60 s, without
  restarting the VM, host memory returns to at most the REQ-MEM-001 target + 100 MB
  (400 + 100 = **500 MB = 476.8 MiB** for 0.1; 350 MB for 1.0).
- Machine: Intel Core Ultra 7 155H, 32 GB, Windows 11 Home 10.0.26200. Every command ran
  **non-elevated**. No other VM ran during the runs below (the benches check).
- Guest: the image built from `main` on 2026-10-31 (A2b erofs system disk, Alpine
  `linux-virt` 6.18), `--memory 12GiB`, 8 vCPUs, the persistent data disk.
- Bench: [`bench/mem-reclaim`](../../bench/mem-reclaim/README.md), with the in-container
  half [`bench/mem-guest`](../../bench/mem-guest). Host memory is what REQ-MEM-001
  counts: the private working set of the engine, this VM's `vmmem` and `vmwp`, and the
  engine's children, every 2 s. The page cache is a 4 GiB file on a named volume, so it
  outlives the container.

## Result

| | Before (`main`) | After (this change) |
|---|---|---|
| Baseline, idle, median | 490.5 MiB (514 MB) | 449.1 / 446.7 MiB (471 / 468 MB) |
| Peak | 8870 MiB | 8862 MiB |
| VM (`vmmem` + `vmwp`) after the window − baseline | **+84 MiB** | **−13 / +2 MiB** |
| VM side back at its baseline | never | +46 s / +44 s |
| Engine private working set, baseline → end | 18.8 → 65.8 MiB | 19.5 → 65.1 / 19.3 → 65.9 MiB |
| Total at exit + 60 s | 621.0 MiB (651 MB) | 492.5 / 492.8 MiB (516 / 517 MB) |
| Total at the end (exit + 180 s) | 621.2 MiB (651 MB) | 481.1 / 495.2 MiB (504 / 519 MB) |
| **REQ-MEM-002** | **FAIL** | **FAIL** (2 of 2 runs), by 4 to 18 MiB |

"After" lists run 2 / run 3 of `bench/mem-reclaim`. The guest's own view at the end of
every run: `MemFree` within 10 MiB of its idle value, file cache 35 to 75 MiB, `AnonPages`
about 62 MiB, `Slab` 32 MiB. The fill took 1.4 s for the anonymous memory and 2.2 s for
the file.

**The VM now gives back everything the workload took, within about 45 s.** What still
fails the requirement is the engine process: in all three bench runs its private working
set rose by 46 MiB, from about 19 MiB to its full commit of about 65 MiB (below). Without
that rise the total was 446.5 MiB at +46 s in run 2, under the limit with 30 MiB to spare.

## Where the memory went

### 1. Free memory the guest could not report, +84 MiB, fixed

hv_balloon hands free guest memory back through free page reporting and Hyper-V's cold
discard hint (ADR 0002). By default it reports only free blocks of 2 MiB or more
(`page_reporting_order` 9). After the workload the guest considered its memory free
(`MemFree` back to the idle value), but `vmmem` stayed 50 to 90 MiB above its baseline,
and the gap grew with every further workload:

| `page_reporting_order` | idle `vmmem` | after fill 1 | 2 | 3 | 4 |
|---|---|---|---|---|---|
| 9 (default, 2 MiB) | 450–462 MiB | 528–612 | 564–648 | 610–614 | 616–632 |
| 4 (64 KiB) | 421–431 | 439–452 | 453–466 | | |
| **0 (4 KiB)** | 408–422 | 422–433 | 430–434 | 429–433 | 426–427 |

(`vmmem` private working set 60 to 90 s after each fill; each fill is the full 4 GiB +
4 GiB workload; the order 9 row has the smaller hash tables of section 2; one to two VMs
per row.)

`/proc/buddyinfo` after the workload shows why. Free memory was almost all in order-10
blocks, but about 70 MiB sat in blocks of order 3 to 8 (32 KiB to 1 MiB): the holes
around the slab and kernel pages that the workload left scattered across 2 MiB blocks.
Before the workload those pages had never been touched, so they cost nothing. After it,
they are committed, free, and too small to report. Compaction does not help, because slab
pages cannot move: `drop_caches=3` plus `compact_memory` changed nothing (554 → 570 MiB).

`page_reporting.page_reporting_order=0` on the kernel command line makes hv_balloon
report single pages. Hyper-V accepts them: the after-workload level is back to the idle
level and no longer grows.

Side effects, measured on the same VMs:
- Idle CPU of `vmmem` over 60 to 180 s: 0.7–2.2 % of one core at order 9, 1.1–2.6 % at
  order 0. That is within run-to-run noise. The `vmmem` CPU counter may not include all
  of the guest's time, so this only rules out a large cost.
- The anonymous fill took the same time (1.34–1.45 s for 4 GiB) at every order, so
  faulting reported pages back in costs nothing measurable at this size.
- Writing the 4 GiB file took either about 1.9 s or about 7 s, in both configurations
  (slow in 2 of 10 fills at order 9, 4 of 10 at order 0, 1 of 2 at order 4). That is the disk path, not
  reporting.

### 2. Per-GiB boot cost, −18 MiB, partly fixed

A 12 GiB guest idles about 175 MiB above a 4 GiB one (490 vs 315 MiB in REQ-MEM-001's
run). The kernel reserves 263.8 MiB at boot (`Memory:` line):

- `mem_map`, 64 bytes per 4 KiB page: 192 MiB for 3.1 M pages. Every entry is written at
  boot, so all of it is committed on the host. It cannot shrink while the guest has
  12 GiB of RAM.
- The dentry and inode hash tables: 16 + 8 MiB, sized from RAM and zeroed at boot.

`dhash_entries=524288 ihash_entries=262144` fix the tables at the sizes a 4 GiB guest
gets by default (4 + 2 MiB). The default VM is unchanged, and a 12 GiB one saves 18 MiB.
(The measured runs used 262144 / 131072, which saved 21.5 MiB of reserved memory; the
committed values are the 4 GiB defaults, so a 4 GiB guest's lookups do not change.)

Idle `vmmem` of the same configuration varied by up to 30 MiB from boot to boot (466 to
498 MiB on `main`), so single idle numbers above are ±15 MiB.

### 3. The engine's commit becoming resident, +46 MiB, open

In all three bench runs the engine's private working set jumped from about 19 MiB to
about 65 MiB, its whole commit, once: at +55 s, +55 s and +2 s after the container
exited (4 to 5 minutes after the start), and stayed there. `bench/mem-idle` at 4 GiB,
run afterwards, saw the same jump between 5.5 and 6 minutes after the start. ADR 0003 (Consequences)
explains the 46 MiB: 32 MiB is Go 1.27's FIPS entropy scratch buffer
(`crypto/internal/fips140/drbg.memory`) and most of the rest gopacket's tables, all in
the executable's data section, demand-zero and never touched by the engine; another
process reading the engine's whole address space makes it resident.

The same engine binary, started from a PowerShell script instead of from a bench, did not
jump: ten VMs over 6 to 10 minutes each, idle or running the same 4 + 4 GiB workload up to
twice, stayed at 16 to 20 MiB. ADR 0003 saw the same with the soak. Every bench launches
the engine from a Go program that also starts a hidden PowerShell for each sample (the
CIM query in `bench/internal/hostmon`). So the outside reader
(most likely Defender's behaviour monitoring) seems to react to the bench's process tree.
A non-elevated user cannot see who opens the engine, so this is not proven. It is not a
leak and not something the VM does, but REQ-MEM-002 counts it.

## Changes

- `internal/engine.DefaultCmdLine` adds `page_reporting.page_reporting_order=0
  dhash_entries=524288 ihash_entries=262144`.
- `bench/mem-reclaim` and `bench/mem-guest` are new. `bench/mem-idle` now reads the
  Docker pipe after the start, so a `--pipe` in `-start-args` works.

## What is not measured, and what is left

- **Not measured:** anything inside `vmmem` (Hyper-V's own accounting, the reported
  ranges), which needs elevation; who reads the engine's memory; more than two runs of
  the bench after the change; the first start after a host reboot; a 1.0 target run.
- **The engine's 46 MiB** (section 3). REQ-MEM-002 passes only if the engine stays near
  its idle working set, or if the 32 MiB buffer leaves the binary (Go's to fix, ADR 0003)
  or is not committed. Until then the requirement is not met on this machine.
- **The 1.0 target cannot be reached at 12 GiB as configured.** `mem_map` alone is
  192 MiB, and the idle total is about 447 MiB against a 1.0 limit of 333.8 MiB
  (350 MB). Booting small and hot-adding memory (idle-memory.md, section 5) would cut the
  idle cost, but hv_balloon cannot remove hot-added memory, so after a workload that used
  it the `mem_map` of the added memory would stay. That needs an ADR.
- **REQ-MEM-001 with the new command line** (`bench/mem-idle`, 4 GiB, one run): total
  median 359.5 MiB (377 MB), still under the 0.1 target. `vmmem` 288.6 MiB (292 before),
  `vmwp` 5.4 MiB, and the engine 65.5 MiB because of the jump in section 3 (18.9 MiB
  before it). Without the jump the total was 312.7 MiB.
- **Whether the jump also happens to a user's engine** is the open question that decides
  REQ-MEM-002. A bench that reads the counters in-process (PDH) instead of through
  PowerShell would show whether the bench causes it.
