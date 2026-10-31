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

## Result after the engine fix (47 runs, 2026-10-31 evening to 2026-11-01 morning)

The engine's jump (section 3) was caused by an outside process reading the engine's
whole address space. With the bench's old sampler (a hidden PowerShell per 2 s sample)
it happened in 19 of 21 runs, always 300 to 302 s after the engine started; with
in-process sampling in none of 26 `bench/mem-reclaim` runs (one `bench/mem-idle` run saw
a smaller read, section 3). The release configuration (F: the toolchain's crypto, as
before, plus the engine's working-set guard) empties the engine's working set a few
seconds after such a read.

**REQ-MEM-002 for 0.1:** met in all 5 runs of the release configuration without a scan,
and in 2 of 3 runs with a provoked scan. The scan itself holds the engine at its full
commit (about 66 MiB) and the total 13 to 27 MiB over the limit for about 4 s, until the
guard trims. The scans came 300 s after the engine started, which in these runs was +54
to +56 s after the container's exit; in F2 the trim landed at +60.1 s and the bench
failed the run by 0.1 s. A scan later in the window would fail the run the same way. The
requirement is therefore met as long as nothing reads the engine's memory in the 60 s
after the exit; when something does, the engine recovers within one guard interval
(5 s), and its working set ends lower than before (13.8 MiB).

| Configuration | Sampler | Runs (valid) | Engine jumped | Pass | Total at +60 s | Highest total from +60 s | Engine at the end |
|---|---|---|---|---|---|---|---|
| A: before (engine commit 66 MiB) | `cim` | 5 (4) | **5**, +46 to +48 MiB, at start + 300 to 302 s | **0 of 4** | 486.7–497.8 MiB | 493.3–509.5 | 65.7–66.1 MiB |
| A | `nt` | 5 (4) | 0 | **4 of 4** | 448.9–465.8 | 453.0–474.9 | 18.9–19.3 |
| B: frozen crypto module only (commit 35 MiB; not chosen) | `cim` | 2 (1) | 2, +14.5 MiB, at start + 300 s | 0 of 1 | 459.9–462.6 | 474.4–480.6 | 33.7–34.2 |
| B | `nt` | 1 (0) | 0 | – | 456.7 | 459.9 | 19.0 |
| C: B + working-set guard (not chosen) | `cim` | 5 (4) | 4, to 33.2–34.4 MiB, trimmed 5 to 6 s later | **4 of 4** | 431.5–465.2 | 447.9–471.8 | 13.4–13.9 (19.3 in the run without a scan) |
| C | `nt` | 5 (5) | 0 | **5 of 5** | 442.3–456.5 | 447.9–465.5 | 18.9–19.1 |
| D: A + guard, full rule only | `cim` | 3 (3) | 3, to 64.5–65.3 MiB, trimmed 4 s later | 3 of 3 | 428.8–444.1 | 444.6–458.0 | 13.6–13.8 |
| D | `nt` | 5 (5) | 0 | 5 of 5 | 444.2–465.1 | 450.5–475.7 | 18.8–19.3 |
| E: D + partial rule (data section read from the exe file) | `cim` | 3 (3) | 2, to 66.2–66.5 MiB, trimmed 4 s later | 3 of 3 | 432.0–440.0 | 450.5–452.6 | 13.7–13.8 (19.4 without a scan) |
| E | `nt` | 5 (5) | 0 | 5 of 5 | 448.9–457.6 | 454.2–465.2 | 19.2–20.0 |
| **F: release configuration** (E, data section read from the loaded image) | `cim` | 3 (3) | 3, to 65.1–65.9 MiB, trimmed 4 s later | **2 of 3** | 425.9–439.3 | 443.6–456.6 | 13.8 |
| **F** | `nt` | 5 (5) | 0 | **5 of 5** | 441.8–457.3 | 447.3–461.5 | 19.0–19.6 |

Limit 476.8 MiB (500 MB). "Valid": no other VM or engine ran during the run; another
session's VMs overlapped five runs (marked not valid by the bench, still listed). B was
stopped after three runs (its console output was lost for the third; its samples were
saved). Every run used `--memory 12GiB` and the same guest image (built 2026-10-31); the
engine was built with `-trimpath` and its symbol table (for the residency walk),
otherwise like the release (B and C with `GOFIPS140=v1.0.0-c2097c7c`, the others with
the toolchain's crypto). VM (`vmmem` + `vmwp`) at baseline (median) 412.4 to 442.5 MiB
over all runs, at the end 409.0 to 450.5 MiB. D3 (`nt`) came within 1.1 MiB of the limit
at its highest sample, with the engine at 19 MiB: that margin is the VM's run-to-run
spread, not the engine. E's engine idled at 22.7 MiB instead of 19.3 because the guard
parsed the executable's symbol table at start (3.3 MiB allocated); F reads the headers
from memory and idles at 19.0 to 19.3 MiB, as before the guard.

In D to F, when the scan came, the engine stood at 64.5 to 66.5 MiB for two 2 s samples
and the total at **489.6 to 507.1 MiB, over the limit**, until the guard emptied the
working set (to 1.0 to 1.4 MiB); the engine faulted back to 13.6 to 13.8 MiB by the end.
Every trim used the full rule (working set within 4 MiB of the commit). In C (frozen
module) the same transient was 33 to 34 MiB and stayed under the limit (at most
473.6 MiB).

## First runs (before the engine fix)

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
stays out of its working set. (Fixed since: section 3 and the result above.)
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

### 3. The engine's commit becoming resident, +46 MiB, undone by the engine

In the first bench runs 1 to 3 the engine's private working set jumped from about 19 MiB
to about 65 MiB, its whole commit, once, and stayed there; run 4 did not. ADR 0003
(Consequences) had traced the 46 MiB to memory the engine never touches: Go 1.27's FIPS
entropy scratch buffer (`crypto/internal/fips140/drbg.memory`, 32 MiB in `.noptrbss`)
and other committed but untouched pages, which another process reading the engine's
address space makes resident.

**The trigger is the bench's PowerShell sampling.** Every bench read the counters
through a hidden `powershell.exe` per sample (a CIM query). `bench/internal/hostmon` now
reads the same counters in process (`SampleNT`: `NtQuerySystemInformation`, class
`SystemProcessInformation`, which the perf counters are computed from; no process is
opened and nothing is started), and `-sampler cim` keeps the old method as a control.
Alternating the two on the same engine binary (configuration A in the table above):

| Sampler | Runs | Engine jumped | When (after the engine started) |
|---|---|---|---|
| `cim` | 5 | 5 | 301, 300, 302, 300, 300 s |
| `nt` | 5 | 0 | |

Over all 47 runs: `cim` 19 jumps in 21 runs, always 300 to 302 s after the start (C1,
which started after 8 minutes of waiting for another session's VM to go away, and E2 had
none); `nt` none in 26. The 300 s count from the engine's start, not from the workload,
which in these runs ended about 245 s after the start. With `bench/mem-idle`'s 10 s
sampling the `cim` scan came at 600 s. ADR 0003's soak and the earlier `bench/mem-idle`
runs, which also sampled through PowerShell, saw the same kind of jump.

One read came without the PowerShell sampler: in a `bench/mem-idle` run with `-sampler
nt` (configuration D, 4 GiB guest, no workload), at 455 s, the engine rose from 18.9 to
51.9 MiB, about the size of the FIPS buffer, with its commit flat at 66.8 MiB, and stayed
there to the end (that bench has no residency walk). The guard's rule at the time needed
the working set within 4 MiB of the commit and did not fire; the partial rule (below) was
added for it. Its cause is not known.

**What becomes resident is everything.** The bench now walks the engine with
`VirtualQueryEx` and `QueryWorkingSetEx`, which do not touch pages, after any 8 MiB rise
(`hostmon.ResidencyOf`; resident / committed MiB). At the jump in A, run 2:

| Region | End of baseline | At the jump |
|---|---|---|
| exe `.data` (incl. BSS) | 6.7 / 39.9 | **39.9 / 39.9** |
| of it `drbg.memory` | 0 / 32.0 | **32.0 / 32.0** |
| other private memory (Go heap, the runtime's heap index, stacks, DLL heaps) | 12.3 / 26.3 | 25.0 / 25.3 |
| exe `.zdebug_*`, `.symtab` (debug data the program never reads; shared pages) | 0 | 5.5, all of it |

The read takes about 2 s, private memory first, then the image (A4 caught both halves in
consecutive samples, +12.3 then +34.7 MiB). Reading a demand-zero page from outside gives
the owner a private zero page in its working set, so a process that reads the engine's
whole address space makes the whole commit resident. Reproduced with a same-user test
program that reads a Go process's committed memory with `ReadProcessMemory`: private
working set 8.8 → 53.8 MiB (commit 54.6 MiB); without the guard below it stayed at the
commit.

Who reads is not proven: a non-elevated user cannot see which process opened the engine,
and testing the Defender hypothesis directly would need its logs or an exclusion, which
this work does not touch. The pattern (a full read of a process near scripting activity,
at a fixed delay after the process started) fits antivirus behaviour monitoring. What
matters for REQ-MEM-002 is that the bench caused it and that the engine is now robust to
it.

**Evaluated, not chosen: half the commit.** Linking Go's frozen FIPS 140-3 module
`v1.0.0-c2097c7c` (`GOFIPS140`, shipped in `GOROOT/lib/fips140`) instead of the
toolchain's own removes the buffer: its DRBG has no entropy scratch buffer (`v1.26.0`,
the other snapshot, still has it), and `//go:debug fips140=off` keeps FIPS mode off,
which `GOFIPS140` would otherwise turn on. The engine's `.data` section is then 8.3 MB
instead of 41.9 MB and its commit at idle 33.7 to 35.5 MiB instead of 66 to 67 MiB; a scan
adds 14.5 MiB, not 46 (configurations B and C). It was not enough on its own (B1 failed
because its VM sat 15 MiB higher than usual, 442.5 MiB at baseline), and **the
maintainers decided against it**: the module is Go 1.24's and receives no fixes (about
70 files differ from Go 1.27's: ECDSA, RSA, P-256, AES-GCM, ML-KEM, SHA-2 assembly), and
an HTTPS update check is planned for 0.2, which would run its TLS on that code. The
release build keeps the toolchain's crypto and the 32 MiB buffer; it is committed but
untouched unless something reads it, and then the guard below takes it out again.

Besides the buffer, a scan makes the Go runtime's untouched commit resident: its 8 MiB heap index
(`mheap_.arenas` level 2, 2^20 pointers for the 4 MiB arenas Go uses on Windows,
allocated with `sysAllocOS` and not counted in `MemStats`), 1 MiB of page-allocator
chunks, 1.4 MiB of profiling hash buckets, idle thread stacks and a few MiB of committed
but untouched heap. A hello-world Go program has the same 8 MiB index.

**The fix: the working-set guard** (`internal/diag.WorkingSetGuard`, started by the
engine). Every 5 s it reads its own private working set and commit
(`PROCESS_MEMORY_COUNTERS_EX2`) and compares them with the lowest working set of the
last 30 s:
- *Full rule:* the working set rose by 8 MiB or more and is within 4 MiB of the commit.
  The engine never touches all of its commit by itself (the heap index alone keeps 8 MiB
  out), so that state means an outside read; growth the engine does itself (Go heap, DLL
  heaps, thread stacks) raises the commit too and keeps the gap (a unit test writes
  32 MiB of native heap and the rule does not fire).
- *Partial rule:* the working set rose by 16 MiB or more with the commit up by at most
  1 MiB, and the executable's `.data` section (read with `QueryWorkingSetEx` on the
  engine's own pages; the section table comes from the loaded image's headers) has
  16 MiB more resident pages than when the guard started. The engine faulting its own
  pages back after Windows or the guard trimmed it looks the same in the working set, but
  it never touches the FIPS buffer, so its `.data` only returns to its start level. In
  effect this rule detects the FIPS buffer becoming resident. With
  `GODEBUG=fips140=on` the engine fills the buffer itself and the rule trims once.

Either rule calls `SetProcessWorkingSetSizeEx(-1, -1)`, at most once a minute, and the
engine logs `working set emptied` with the rule. The engine also logs at start whether
the partial rule is on. A trim moves pages to the standby list: the working set
REQ-MEM-001/002 count drops at once, the RAM is free when Windows repurposes it, and the
pages the engine uses fault back without I/O. It does not prevent the read. In D to F
the full rule fired about 4 s after each of eight scans, at 64.5 to 66.5 MiB; the engine
dropped to 1.0 to 1.4 MiB and settled at 13.6 to 13.8 MiB, below its 19 MiB before. It
did not fire in any `nt` run. The partial rule has not fired in a bench run yet: the one
partial read was seen before it existed.

Evaluated and not used:
- `GOMEMLIMIT`, `debug.FreeOSMemory`: what a scan adds is not reclaimable heap (the
  scavenger already decommits released heap on Windows), so they would not change it.
- `VirtualProtect(PAGE_NOACCESS)` on the FIPS buffer (ADR 0003 tried it from outside):
  the program cannot find its address (`go:linkname` to it is refused by the linker, and
  a release build has no symbol table).
- `MEM_RESET` / `DiscardVirtualMemory`: they apply to private allocations, not to an
  image's data section, and the heap index belongs to the runtime.
- Trimming after every workload instead of on the scan's signature: it costs refaults
  after each `docker run` and misses a scan at any other time.

## Changes

- `internal/engine.DefaultCmdLine` adds `page_reporting.page_reporting_order=0`; above
  4 GiB of guest memory the engine also adds `dhash_entries=524288 ihash_entries=262144`.
- `bench/mem-reclaim` and `bench/mem-guest` are new. `bench/mem-idle` now reads the
  Docker pipe after the start, so a `--pipe` in `-start-args` works.
- Engine fix (section 3): the engine runs `diag.WorkingSetGuard` (full and partial
  rules), whose trims show in the engine log and in `/debug/stats`
  (`working_set_trims`). The frozen crypto module was committed and reverted (maintainer
  decision, section 3); the release build's flags are unchanged.
- Benches read the counters in process (`hostmon.SampleNT`; `-sampler cim` in
  `bench/mem-reclaim` and `bench/mem-idle` for the old method); `bench/mem-reclaim` logs
  the engine's residency (`-residency`) and records its commit and page faults.

## What is not measured, and what is left

- **Not measured:** anything inside `vmmem` (Hyper-V's own accounting, the reported
  ranges), which needs elevation; who reads the engine's memory (section 3); whether a
  user's engine, started by `hyperbutler start` with no bench around it, is read over
  hours (the earlier PowerShell-launched runs and the `nt` runs say rarely within
  10 minutes, one partial read in 27; the guard covers it either way); the partial rule
  firing on a real partial read; the first start after a host reboot; a
  1.0 target run; a container that exits with dirty page cache (the bench syncs the file
  first); the cost of 4 KiB discard hints for a long, memory-heavy guest workload (host
  large pages split into 4 KiB mappings; a before/after `bench/fs-seq` or
  `bench/net-perf` run would bound it).
- **A scan in the 60 s window fails the run.** Until the guard trims, a scan holds the
  engine at its full commit, about 47 MiB more, which puts the total 13 to 27 MiB over
  the limit for about 4 s (D to F). In these runs the scan came at +54 to +56 s; in F2 the
  trim landed at +60.1 s and the run failed by 0.1 s. A scan later in the window fails
  for the same reason. The scan is not tied to the workload (300 s after the engine
  started, under the PowerShell sampler), so this is about when an outside reader
  strikes, not about reclaim. Not done, cheap to try: a 1 s guard interval (one
  `K32GetProcessMemoryInfo` call per second). The full rule cannot fire half-way through
  a scan, because until the FIPS buffer has been read the working set is far from the
  commit, so a shorter interval would cut the transient to about 1 s after the scan.
- **The partial rule's reference** is the `.data` level when the guard starts. A read in
  the engine's first seconds, before the guard runs, would raise that reference and turn
  the rule off for the process's lifetime (the full rule still works).
- **The 32 MiB buffer stays** in the release build (section 3, not chosen). Revisit if Go
  allocates the entropy buffer lazily.
- **The 1.0 target cannot be reached at 12 GiB as configured.** `mem_map` alone is
  192 MiB, and the idle total is about 447 MiB against a 1.0 limit of 333.8 MiB
  (350 MB). Booting small and hot-adding memory (idle-memory.md, section 5) would cut the
  idle cost, but hv_balloon cannot remove hot-added memory, so after a workload that used
  it the `mem_map` of the added memory would stay. That needs an ADR.
- **REQ-MEM-001** (`bench/mem-idle`, 4 GiB). With the first runs' code (after first run
  4): total median 309.6 MiB (325 MB), engine 18.8, `vmmem` 285.2, `vmwp` 5.5 MiB (the run
  before it read 359.5 MiB because the engine jumped to 65.5 MiB). With the release
  configuration F (`-sampler nt`, 2026-11-01): total median **296.7 MiB (311 MB)**, engine
  18.8, `vmmem` 272.5, `vmwp` 5.5 MiB, engine commit 66.6 MiB, no read. Earlier configurations, one run each with `-sampler nt`: C
  306.9 MiB (engine 18.6, commit 33.7 MiB; with `-sampler cim` 311.3 MiB, the scan at
  600 s on the last sample), D 314.3 MiB (engine 18.9 MiB, max 52.1 after the partial
  read above), E 310.5 MiB (engine 18.9 MiB).

## Adversarial review

The adversarial review covered the bench, the command line change and this
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

### Second review: the engine fix

The adversarial review covered the sampler, the residency walk, the frozen
crypto module and the working-set guard on 2026-10-31 against REQ-MEM-002, with the
binaries built and the tests run. No critical or high findings. Configurations C to F
were measured after the fixes below. Reverting the module restored the package script's
earlier, reviewed state. The partial rule was reviewed twice more (see the third review).

| # | Sev | Finding | Resolution |
|---|---|---|---|
| 1 | Med | The guard's first rule (a rise the Go runtime's mapped memory does not explain) would fire on native DLL heap growth, on heap committed in one window and touched in the next, and on the engine refaulting after its own trim. | **Fixed:** the rule now requires the working set to be within 4 MiB of the commit, which only an outside read reaches (growth the engine does raises the commit too), and keeps the pre-trim low after a trim. Tests for each case, including 32 MiB of real native heap. |
| 2 | Med | CI never runs the code linked with the frozen module, and the FIPS test passes trivially without `GOFIPS140`. | **Fixed, then moot:** a CI step and a version check were added; they were reverted with the module (maintainer decision, section 3). |
| 3 | Med | The frozen module receives no fixes and differs from Go 1.27's in about 70 files; record the trade-off and how it is watched. | **Resolved by not using the module:** the maintainers kept the toolchain's crypto because of the HTTPS update check planned for 0.2 (ADR 0003 update, section 3). |
| 4 | Med | The guard changes what other benches see: the soak's working-set trend gets a step, and mem-reclaim's summary did not show the commit. | **Fixed/documented:** trims are counted in `/debug/stats` and logged; mem-reclaim prints the engine's commit and the trim lines; the soak README says to read the working-set trend next to private bytes, which a trim does not change. No switch to turn the guard off. |
| 5 | Low | `PageFaultCount` is 32 bits and wraps; `SampleNT` ignores its context. Sampler measures the same counters as CIM (confirmed). | **Documented** on the field; the context is unused because the call does not block. |
| 6 | Low | Residency walk: header pages had no section name; a walk over `maxRegions` was cut silently; a stripped exe shows no symbols without saying so. | **Fixed:** "exe headers", an error past the limit, and "(no symbol table)" in the output. |
| 7 | Low | The engine's log line stated the cause as fact. | **Fixed:** "it rose to its commit (likely an outside memory scan)", with working set and commit. |
| 8 | Low | The guard loop was only start/stop tested; no fuzz test for the PE layout reader. | **Fixed** for the loop (fake OS: one scan, one trim, one callback, counter). Not done: fuzzing `readExeLayout` (bench-only code that reads the engine's own executable). |
| 9 | Info | requirements.md, this document and the READMEs still described the old state. | **Fixed** with the re-measured numbers. |

### Third review: the partial rule

The adversarial review looked at the partial rule as first written (a 16 MiB rise with
a flat commit), then at its two follow-ups, with unit tests only (a bench was running).

| # | Sev | Finding | Resolution |
|---|---|---|---|
| 1 | High | A rise with a flat commit is also what the engine refaulting looks like after any drop of its working set (the guard's own trim, a trim by Windows under memory pressure, resume from sleep); the rule would have trimmed the real working set again, up to once a minute. | **Fixed:** the rule also requires the `.data` section's resident pages to be 16 MiB above their start level, which a refault cannot reach. Both traces are tests. |
| 2 | Med | The pre-trim low lived 30 s but the rate limit 60 s, so it protected nothing. | **Moot** with the `.data` check; the trace is a test. |
| 3 | Med | The +33 MiB read is not shown to come from outside. | **Documented:** cause not known (code comment, section 3). |
| 4 | Med | The partial rule in effect means "the FIPS buffer became resident"; a read that skips `.data` is caught only by the full rule. | **Documented** in the code. |
| 5 | Low | `GODEBUG=fips140=on` makes the engine fill the buffer itself: one harmless trim. | **Documented** in the code and section 3. |
| 6 | Low | The `.data` reference is taken once, at start. | **Documented** under "what is left". |
| 7 | Low | Parsing the executable file with `debug/pe` could fail silently, and it parsed the whole symbol table (3.3 MiB allocated; E's idle engine sat at 22.7 MiB). | **Fixed:** the headers are read from the loaded image (F), and the engine logs at start whether the partial rule is on. |
| 8 | Low | Header bounds checks could overflow on a 32-bit `int`; no fuzz target; no test for headers without `.data`. | **Fixed:** overflow-free checks, `FuzzParseDataSection` (it found that an accepted section could wrap past 4 GiB; fixed, input kept as a seed), tests for no `.data` and no sections. The attribute field is a `ULONG_PTR`. |
