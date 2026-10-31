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

## Result after the engine fix (25 runs, 2026-10-31 evening)

The engine's jump (section 3) was caused by an outside process reading the engine's
whole address space, and that happened only while the bench started a hidden PowerShell
per sample. With in-process sampling it never happened; with the old sampler it happened
in 12 of 13 runs, always 300 s after the engine started (with the bench's 2 s
sampling). The engine now has half the
commit (the 32 MiB FIPS buffer is gone from the release build) and empties its working
set when such a scan makes all of it resident, so **REQ-MEM-002 is met for 0.1 in every
valid run of the release configuration, with either sampler.**

| Configuration | Sampler | Runs (valid) | Engine jumped | Pass | Total at +60 s | Highest total from +60 s | Engine at the end |
|---|---|---|---|---|---|---|---|
| A: before (engine commit 66 MiB) | `cim` | 5 (4) | **5**, +46 to +48 MiB, at start + 300 to 302 s | **0 of 4** | 486.7–497.8 MiB | 493.3–509.5 | 65.7–66.1 MiB |
| A | `nt` | 5 (4) | 0 | **4 of 4** | 448.9–465.8 | 453.0–474.9 | 18.9–19.3 |
| B: frozen crypto module only (commit 35 MiB) | `cim` | 2 (1) | 2, +14.5 MiB, at start + 300 s | 0 of 1 | 459.9–462.6 | 474.4–480.6 | 33.7–34.2 |
| B | `nt` | 1 (0) | 0 | – | 456.7 | 459.9 | 19.0 |
| **C: release configuration** (B + working-set guard) | `cim` | 5 (4) | 4, to 33.2–34.4 MiB, at start + 300 to 301 s, trimmed 5 to 6 s later | **4 of 4** | 431.5–465.2 | 447.9–471.8 | 13.4–13.9 (19.3 in the run without a scan) |
| **C** | `nt` | 5 (5) | 0 | **5 of 5** | 442.3–456.5 | 447.9–465.5 | 18.9–19.1 |

Limit 476.8 MiB (500 MB). "Valid": no other VM or engine ran during the run; another
session's VMs overlapped five runs (marked not valid by the bench, still listed). B was
stopped after three runs (its console output was lost for the third; its samples were
saved). Every run used
`--memory 12GiB` and the same guest image (built 2026-10-31); the engine was built with
`-trimpath` and its symbol table (for the residency walk), otherwise like the release
(configuration B and C with `GOFIPS140=v1.0.0-c2097c7c`). VM (`vmmem` + `vmwp`) at
baseline (median) 412.4 to 442.5 MiB over all runs, at the end 409.0 to 450.5 MiB.

In C, when the scan came (+54 to +56 s after the exit in these runs, because the
workload started 4 minutes after the engine), the engine stood at 33 to 34 MiB for one
to three 2 s samples before the guard emptied its working set (to 1.4 MiB); it faulted
back to 13 to 14 MiB by the end. The highest total of those transients, 473.6 MiB at
+57 s, was 3.2 MiB under the limit; a scan after +60 s on a run whose VM sits high could
still put one or two samples over it (section 3).

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

### 3. The engine's commit becoming resident, +46 MiB, fixed

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

Over all 25 runs: `cim` 12 jumps in 13 runs, always 300 to 302 s after the start (the one
`cim` run without it, C1, started after 8 minutes of waiting for another session's VM to
go away); `nt` 0 in 11. The 300 s count from the engine's start, not from the workload,
which in these runs ended about 245 s after the start. ADR 0003's soak and the earlier
`bench/mem-idle` runs, which also sampled through PowerShell, saw the same kind of jump.

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

**Fix 1: half the commit.** Release builds link Go's frozen FIPS 140-3 module
`v1.0.0-c2097c7c` (`GOFIPS140`, shipped in `GOROOT/lib/fips140`) instead of the
toolchain's own; its DRBG has no entropy scratch buffer (`v1.26.0`, the other snapshot,
still has it). `//go:debug fips140=off` keeps FIPS mode off, which `GOFIPS140` would
otherwise turn on. The engine's `.data` section is 8.3 MB instead of 41.9 MB and its
commit at idle 33.7 to 35.5 MiB instead of 66 to 67 MiB. A scan now adds 14.5 MiB, not 46
(configuration B). That was not enough on its own: B1 failed because its VM sat 15 MiB
higher than usual (442.5 MiB at baseline).

What a scan still makes resident is the Go runtime's: its 8 MiB heap index
(`mheap_.arenas` level 2, 2^20 pointers for the 4 MiB arenas Go uses on Windows,
allocated with `sysAllocOS` and not counted in `MemStats`), 1 MiB of page-allocator
chunks, 1.4 MiB of profiling hash buckets, idle thread stacks and a few MiB of committed
but untouched heap. A hello-world Go program has the same 8 MiB index.

**Fix 2: the working-set guard** (`internal/diag.WorkingSetGuard`, started by the
engine). Every 5 s it reads its own private working set and commit
(`PROCESS_MEMORY_COUNTERS_EX2`). When the working set has risen by 8 MiB or more over its
lowest value of the last 30 s and is within 4 MiB of the commit, it calls
`SetProcessWorkingSetSizeEx(-1, -1)`, at most once a minute, and logs `working set
emptied`. The engine never touches all of its commit by itself (the heap index alone
keeps 8 MiB out), so that state means an outside read; growth the engine does itself (Go
heap, DLL heaps, thread stacks) raises the commit too and keeps the gap (a unit test
writes 32 MiB of native heap and the rule does not fire). A trim moves pages to the
standby list: the working set REQ-MEM-001/002 count drops at once, the RAM is free when
Windows repurposes it, and the pages the engine uses fault back without I/O. It does not
prevent the scan. In C's four scans the guard fired 5 to 6 s after the jump, at 33.2 to
34.4 MiB, the engine dropped to 1.4 MiB and settled at 13.4 to 13.9 MiB, below its
19 MiB before; it did not fire in any `nt` run.

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
- Engine fix (section 3): release builds link the frozen crypto module
  (`dist/package.ps1`, `GOFIPS140=v1.0.0-c2097c7c`, checked with `go version -m` and a
  16 MiB limit on `.data`; `cmd/hyperbutler/godebug.go` keeps FIPS mode off; CI runs the
  host tests with that module too), and the engine runs `diag.WorkingSetGuard`, whose
  trims show in the engine log and in `/debug/stats` (`working_set_trims`).
- Benches read the counters in process (`hostmon.SampleNT`; `-sampler cim` in
  `bench/mem-reclaim` and `bench/mem-idle` for the old method); `bench/mem-reclaim` logs
  the engine's residency (`-residency`) and records its commit and page faults.

## What is not measured, and what is left

- **Not measured:** anything inside `vmmem` (Hyper-V's own accounting, the reported
  ranges), which needs elevation; who reads the engine's memory (section 3); whether a
  user's engine, started by `hyperbutler start` with no bench around it, is ever scanned
  over hours (the earlier PowerShell-launched runs and every `nt` run say not within
  10 minutes; the guard covers it either way); the first start after a host reboot; a
  1.0 target run; a container that exits with dirty page cache (the bench syncs the file
  first); the cost of 4 KiB discard hints for a long, memory-heavy guest workload (host
  large pages split into 4 KiB mappings; a before/after `bench/fs-seq` or
  `bench/net-perf` run would bound it); the engine's throughput with the frozen crypto
  module (its SHA-256 code differs from Go 1.27's, and the engine hashes files with it).
- **A scan after +60 s.** The guard needs up to one 5 s interval to see a scan, and the
  scan raises the engine by about 15 MiB meanwhile. In these runs the scan came at +54
  to +56 s; a run whose VM sits 15 MiB above its usual level and is scanned after +60 s
  could put one or two samples over the limit, which the bench's "and stays" reading
  counts as a failure.
- **The frozen crypto module is a trade-off.** It is Go 1.24's certified FIPS 140-3
  module and receives no fixes; about 70 files differ from Go 1.27's (ECDSA, RSA, P-256,
  AES-GCM, ML-KEM, SHA-2 assembly). With FIPS mode off the engine uses it for
  `crypto/sha256` (file digests) and through `crypto/tls`, which `net/http` links but the
  engine never uses for a connection; `crypto/rand` reads the OS generator directly. A
  future HTTPS client in the host process would run on that code, so the choice has to
  be revisited then, or when Go allocates the entropy buffer lazily (then drop
  `GOFIPS140`). Not done: `govulncheck -mode=binary` on the packaged exe in the release
  workflow, and a test that keeps TLS clients out of the host process.
- **The 1.0 target cannot be reached at 12 GiB as configured.** `mem_map` alone is
  192 MiB, and the idle total is about 447 MiB against a 1.0 limit of 333.8 MiB
  (350 MB). Booting small and hot-adding memory (idle-memory.md, section 5) would cut the
  idle cost, but hv_balloon cannot remove hot-added memory, so after a workload that used
  it the `mem_map` of the added memory would stay. That needs an ADR.
- **REQ-MEM-001** (`bench/mem-idle`, 4 GiB). With the first runs' code (after first run
  4): total median 309.6 MiB (325 MB), engine 18.8, `vmmem` 285.2, `vmwp` 5.5 MiB (the run
  before it read 359.5 MiB because the engine jumped to 65.5 MiB). With the release
  configuration (two runs, 2026-10-31 night): total median **306.9 MiB (322 MB)**
  with `-sampler nt` (engine 18.6, `vmmem` 282.9, `vmwp` 5.3 MiB; engine commit 33.7 MiB),
  311.3 MiB with `-sampler cim`, under the 0.1 target both. In the `cim` run, which samples
  every 10 s, the scan came at 600 s, on the last sample (engine 18.7 → 31.5 MiB); the
  engine was stopped before the guard's next check, so that trim is not observed.

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

### Second review: the engine fix

The `adversarial-reviewer` agent reviewed the sampler, the residency walk, the frozen
crypto module and the working-set guard on 2026-10-31 against REQ-MEM-002, building the
binaries and running the tests itself. No critical or high findings. Configuration C's
runs were made after the fixes below.

| # | Sev | Finding | Resolution |
|---|---|---|---|
| 1 | Med | The guard's first rule (a rise the Go runtime's mapped memory does not explain) would fire on native DLL heap growth, on heap committed in one window and touched in the next, and on the engine refaulting after its own trim. | **Fixed:** the rule now requires the working set to be within 4 MiB of the commit, which only an outside read reaches (growth the engine does raises the commit too), and keeps the pre-trim low after a trim. Tests for each case, including 32 MiB of real native heap. |
| 2 | Med | CI never runs the code linked with the frozen module, and the FIPS test passes trivially without `GOFIPS140`. | **Fixed:** a CI step runs `./cmd/... ./internal/...` with `GOFIPS140=v1.0.0-c2097c7c`; the test checks the linked module's version when `GOFIPS140` is set; `package.ps1` also fails if `.data` exceeds 16 MiB. |
| 3 | Med | The frozen module receives no fixes and differs from Go 1.27's in about 70 files; record the trade-off and how it is watched. | **Documented:** ADR 0003 (update), "what is left" here, and a release checklist item (`govulncheck -mode=binary`, recheck the Go buffer). Not done: the govulncheck workflow step and a test keeping TLS clients out of the host process. |
| 4 | Med | The guard changes what other benches see: the soak's working-set trend gets a step, and mem-reclaim's summary did not show the commit. | **Fixed/documented:** trims are counted in `/debug/stats` and logged; mem-reclaim prints the engine's commit and the trim lines; the soak README says to read the working-set trend next to private bytes, which a trim does not change. No switch to turn the guard off. |
| 5 | Low | `PageFaultCount` is 32 bits and wraps; `SampleNT` ignores its context. Sampler measures the same counters as CIM (confirmed). | **Documented** on the field; the context is unused because the call does not block. |
| 6 | Low | Residency walk: header pages had no section name; a walk over `maxRegions` was cut silently; a stripped exe shows no symbols without saying so. | **Fixed:** "exe headers", an error past the limit, and "(no symbol table)" in the output. |
| 7 | Low | The engine's log line stated the cause as fact. | **Fixed:** "it rose to its commit (likely an outside memory scan)", with working set and commit. |
| 8 | Low | The guard loop was only start/stop tested; no fuzz test for the PE layout reader. | **Fixed** for the loop (fake OS: one scan, one trim, one callback, counter). Not done: fuzzing `readExeLayout` (bench-only code that reads the engine's own executable). |
| 9 | Info | requirements.md, this document and the READMEs still described the old state. | **Fixed** with the re-measured numbers. |
