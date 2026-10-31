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

| | Before (`main`) | After, run 2 / run 3 | After, run 4 (committed code) |
|---|---|---|---|
| Baseline, idle, median | 490.5 MiB (514 MB) | 449.1 / 446.7 MiB | 432.0 MiB (453 MB) |
| Peak | 8870 MiB | 8862 / 8862 MiB | 8848 MiB |
| VM (`vmmem` + `vmwp`), end − baseline | **+84 MiB** | −13 / +2 MiB | +18 MiB |
| Total under the limit for good from | never | never | **+44 s** |
| Engine private working set, baseline → end | 18.8 → 65.8 MiB | 19.5 → 65.1 / 19.3 → 65.9 MiB | 19.0 → 19.3 MiB |
| Total at exit + 60 s | 621.0 MiB (651 MB) | 492.5 / 492.8 MiB (516 / 517 MB) | **443.9 MiB (465 MB)** |
| Total at the end (exit + 180 s) | 621.2 MiB (651 MB) | 481.1 / 495.2 MiB (504 / 519 MB) | 450.4 MiB (472 MB) |
| **REQ-MEM-002** | **FAIL** | **FAIL**, by 4 to 18 MiB | **PASS**, 33 MiB under the limit at +60 s |

Runs 2 and 3 used a test build whose hash tables were smaller (2^18 / 2^17 buckets, 2 + 1
MiB) than the committed ones (2^19 / 2^18, 4 + 2 MiB, section 2); run 4 is the committed
code. The guest's own view at the end of every run: `MemFree` within 10 MiB of its idle
value, file cache 35 to 80 MiB, `AnonPages` about 63 MiB, `Slab` 32 MiB. The fill took
1.4 s for the anonymous memory and 2.2 to 2.5 s for the file.

**The VM now gives back what the workload took, within about 45 s.** The VM side ends
within 20 MiB of its baseline, which is inside the boot-to-boot spread of the idle level
(section 2). **Whether a run passes depends on the engine process:** in runs 1 to 3 its
private working set rose once by 46 MiB, from about 19 MiB to its full commit of about
65 MiB (section 3), and those runs fail; in run 4 it did not, and the run passes. So
REQ-MEM-002 is met by the VM, and met overall only while the engine's untouched commit
stays out of its working set.
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

For a guest above 4 GiB the engine now adds `dhash_entries=524288 ihash_entries=262144`,
the sizes a 4 GiB guest gets by default (4 + 2 MiB). A 12 GiB guest saves 18 MiB; a guest
of 4 GiB or less keeps the kernel's own sizes (`engine.HashTableArgs`). The cost is longer
hash chains in a large guest that caches millions of dentries: at 2^19 buckets, two
million cached dentries make chains of four on average. (The diagnosis runs and bench runs
2 and 3 used 262144 / 131072, which saved 21.5 MiB of reserved memory.)

Idle `vmmem` of the same configuration varied by up to 30 MiB from boot to boot (466 to
498 MiB on `main`), so single idle numbers above are ±15 MiB.

### 3. The engine's commit becoming resident, +46 MiB, open

In bench runs 1 to 3 the engine's private working set jumped from about 19 MiB to about
65 MiB, its whole commit, once: at +55 s, +55 s and +5 s after the container exited (4 to
5 minutes after the start), and stayed there. The `bench/mem-idle` run after them saw the
same jump 7.3 to 7.5 minutes after the start. In run 4 and in the `bench/mem-idle` run
after it, it did not happen (the latter rose to 32 MiB for one sample). ADR 0003 (Consequences)
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
A non-elevated user cannot see who opens the engine, so neither the reader nor its
trigger is proven, and run 4 shows a bench-launched engine without the jump. It is not a
leak and not something the VM does, but REQ-MEM-002 counts it.

## Changes

- `internal/engine.DefaultCmdLine` adds `page_reporting.page_reporting_order=0`; above
  4 GiB of guest memory the engine also adds `dhash_entries=524288 ihash_entries=262144`.
- `bench/mem-reclaim` and `bench/mem-guest` are new. `bench/mem-idle` now reads the
  Docker pipe after the start, so a `--pipe` in `-start-args` works.

## What is not measured, and what is left

- **Not measured:** anything inside `vmmem` (Hyper-V's own accounting, the reported
  ranges), which needs elevation; who reads the engine's memory; more than one run of
  the committed code (runs 2 and 3 used the test build); the first start after a host
  reboot; a 1.0 target run; a container that exits with dirty page cache (the bench syncs
  the file first); the cost of 4 KiB discard hints for a long, memory-heavy guest workload
  (host large pages split into 4 KiB mappings; a before/after `bench/fs-seq` or
  `bench/net-perf` run would bound it).
- **The engine's 46 MiB** (section 3). REQ-MEM-002 passes only while the engine stays
  near its idle working set, or once the 32 MiB buffer leaves the binary (Go's to fix,
  ADR 0003) or is not committed. Until then the requirement is met in some runs and not
  in others. **Next step:** sample the counters in-process (PDH) instead of through a
  PowerShell per sample, and run the bench several times: if the jump disappears, the
  bench was the trigger.
- **The 1.0 target cannot be reached at 12 GiB as configured.** `mem_map` alone is
  192 MiB, and the idle total is about 447 MiB against a 1.0 limit of 333.8 MiB
  (350 MB). Booting small and hot-adding memory (idle-memory.md, section 5) would cut the
  idle cost, but hv_balloon cannot remove hot-added memory, so after a workload that used
  it the `mem_map` of the added memory would stay. That needs an ADR.
- **REQ-MEM-001 with the committed command line** (`bench/mem-idle`, 4 GiB, after run 4):
  total median 309.6 MiB (325 MB), under the 0.1 target. Engine 18.8, `vmmem` 285.2,
  `vmwp` 5.5 MiB. (The run before it, with the test build, read 359.5 MiB because the
  engine jumped to 65.5 MiB; 312.7 MiB before the jump.)
- **Whether the jump also happens to a user's engine** is the open question that decides
  REQ-MEM-002 (the next step above).

## Adversarial review

The `adversarial-reviewer` agent reviewed the bench, the command line change and this
document on 2026-10-31 against REQ-MEM-002. No critical or high findings.

| # | Sev | Finding | Resolution |
|---|---|---|---|
| 1 | Med | The "after" numbers came from a test build with smaller hash tables than the committed ones. | **Fixed:** run 4 and the second `bench/mem-idle` run use the committed code; runs 2 and 3 are labelled. |
| 2 | Med | Fixed hash sizes made 1–2 GiB guests' tables larger than the kernel's default and capped huge guests hard. | **Fixed:** the cap applies only above 4 GiB (`engine.HashTableArgs`, with a test); the chain-length cost for large guests is stated in section 2. |
| 3 | Med | `requirements.md` stated the engine rise's cause as fact and did not name the bench as a possible trigger. | **Fixed:** the cause is marked unproven everywhere, and the PDH sampling run is the named next step. |
| 4 | Low | Jump timings in section 3 were wrong (mem-idle 7.3–7.5 min, run 3 +5 s). | **Fixed.** |
| 5 | Low | The exit time was taken after the in-flight sample, so "after" times were up to one sample early. | **Fixed:** the exit is when `docker run` returns, and a workload sample taken after it counts as after. |
| 6 | Low | One late sample above the limit fails a run. | **Kept, documented:** "returns to" is read as "and stays" (README, requirements.md), with a test. |
| 7 | Low | Ctrl+C with `-keep-running` left the container and the volume. | **Fixed:** the container is named and removed before the volume. |
| 8 | Low | The page cache is synced clean before the exit. | **Documented** under "not covered" (README) and "not measured" here. |
| 9 | Low | Building mem-guest needs the source tree; a `-trimpath` bench needs `-guest`. | **Documented** in the README. |
| 10 | Info | Host large-page splitting by 4 KiB discard hints is not measured. | **Documented** under "not measured". |
