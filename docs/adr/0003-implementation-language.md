# 0003. Implementation language

- Status: accepted (2026-10-30; release-blocking guardrails listed under Consequences)
- Date: 2026-10-30
- Requirements: REQ-MEM-001, REQ-PLAT-003, REQ-API-001, REQ-DISK-004

## Context

hyperbutler has a few long-running parts:
- the host process `hyperbutler.exe`;
- the HCS access broker, which runs with high privilege ([ADR 0004](0004-hcs-access-broker.md));
- small guest daemons: the hvsock relay agent, the control and space-guard daemon, and the
  network forwarder.

These must not leak memory and must stay small. The memory budget for everything on the host
is 400 MB in 0.1 and 250 MB in 1.0 (REQ-MEM-001), and the VM itself takes most of it.

Rewriting infrastructure in Rust is a strong trend, so "why Go?" is a fair question and needs
a measured answer. Everything built so far is Go: the HCS spikes, `internal/vm`,
`internal/transport`, `internal/broker`, `internal/spaceguard`, `internal/hcn` and the guest
binaries.

## Options

1. **Go for everything.**
2. **Go, with the broker in Rust.** The broker is the privileged part.
3. **Go, with the guest daemons in Rust.**
4. **Rust for everything.**

## What the languages do and don't guarantee

- **Memory safety** (no use-after-free, double free or out-of-bounds writes). Both give it:
  Rust through ownership, Go through a garbage collector and bounds checks. Both need
  `unsafe` code at the Win32 and HCS boundary. That code must be small, reviewed and fuzzed in
  either language.
- **Freedom from leaks.** Neither language guarantees it. Rust treats leaking as safe
  (`std::mem::forget`, `Rc` cycles, collections that only grow). In Go, leaks come from
  goroutines that never end, maps that grow without bound, and handles left open. Leaks in a
  daemon that runs for weeks are design bugs. They are prevented by bounding every resource
  and caught by tests that measure. Choosing a language does not remove them.

## Measurements (2026-10-30, Windows 11 Home, this repository)

`hyperbutler-dev` is the Go host process: an HCS VM, a named-pipe server and the hvsock proxy.
Measured as private working set, the REQ-MEM-001 metric:

| State | Private WS | Threads |
|---|---|---|
| Idle, VM running | 3.3 MB | 12 |
| After 200 × `docker version` | 5.2 MB | 13 |
| After 50 × `docker run` | 7.2 MB | 16 |
| 20 s idle after that | 7.2 MB | 16 |

- A Rust host process would likely sit around 1–2 MB, so the difference is about 5 MB. That
  is about 1–2 % of the 0.1 budget. The VM's `vmmem` is around 200 MB at idle.
- The growth from 3.3 MB to 7.2 MB has not yet been shown to level off. The soak gate below
  exists to answer that.
- Inside the guest, dockerd and containerd are themselves large Go programs, so a Rust guest
  daemon would save a few MB in a VM whose baseline is about 200 MB.

## Decision

**Go for everything (option 1), with leak guardrails that are required to merge.** Rust is
reconsidered for one component only if a measurement shows that the component misses its
budget or leaks, and the problem can be traced to the language rather than to the design.

Why Go here specifically:
1. **The Windows container stack is Go.** Microsoft's high-level HCS wrapper (`hcsshim`), its
   hvsock and named-pipe library (`go-winio`), containerd's Windows support and moby are Go.
   hyperbutler uses `go-winio` and reads `hcsshim`'s schema and behaviour as its reference. It
   calls `computecore.dll` and `computenetwork.dll` directly, not through hcsshim.
   - Rust has raw bindings for the same DLLs in Microsoft's `windows-sys` crate. There is
     also a small `hcs-rs` crate.
   - So this is a **weak** reason. Rust would lose the reference implementation in the same
     language and the `go-winio` pipe and hvsock helpers, not access to HCS itself.
2. **The network stack is Go.** The guest networking design (ADR 0006) embeds
   gvisor-tap-vsock as a library, as Podman does. In Rust it would be a separate process or a
   rewrite.
3. **The measured cost is small.**
   - About 5 MB of host RAM.
   - Binaries built with `-s -w`: `hyperbutler-dev.exe` 7.3 MB, `hyperbutler-broker.exe` 2.6 MB, guest
     `agent` 2.4 MB. These fit easily in REQ-UX-002's 150 MB.
   - GC pause latency has **not been measured**. The relay paths reuse pooled 64 KiB buffers
     (`internal/transport`) and allocate one 32 KiB buffer per connection (guest agent), so
     pauses are expected to be small. p99 relay latency is added to the ADR 0006 gate to
     check this.
4. **There is one toolchain.** CI, fuzzing (Go native), cross-compiling static Linux guest
   binaries from Windows, and contributors from the Docker and Kubernetes world all use Go.
   Two languages would double the build, CI and review surface.

### Leak and resource guardrails (required)

1. **Goroutine leak checks:** every package with goroutines runs its tests under a
   goroutine-leak check (`go.uber.org/goleak` or equivalent). A goroutine left over at the end
   of a test fails it.
2. **Bounded resources:** every queue, map, buffer and connection set has a fixed or
   configured maximum. Unbounded growth is a review blocker. The broker and transport already
   follow this.
3. **Soak gate:** `bench/soak` drives the real stack for 24 h with mixed `docker` operations,
   samples private working set and handle and goroutine counts, and fails on any positive
   trend beyond noise. It must pass before every release. A shorter run, for example 1 h,
   gates main on self-hosted hardware.
4. **Runtime limits:** long-running Go processes set `GOMEMLIMIT` and expose `pprof` and
   handle counts on a local debug endpoint, off by default.
5. **Small `unsafe` surface:** Win32 and HCS calls live in a few files per package, are
   reviewed adversarially and are covered by fuzz tests where they parse input.

### Option 2, the Rust broker, judged on its own

Reasons 1 and 2 above don't apply to the broker. It has no data plane (ADR 0004 rule 8) and
depends only on the standard library and `x/sys`. The real trade-off is narrower.

**For Rust:**
- No GC and a much smaller runtime inside a high-privilege process.
- Less attack surface from the runtime.

**Against Rust:**
- About 1,800 lines would need a second toolchain, CI path, fuzzing setup and set of reviewers.
- Most of the Docker Desktop broker CVEs listed in ADR 0004 were logic bugs: trusting paths,
  symlink races, pipe squatting. Rust would not prevent those. ADR 0004's design rules and the
  fuzzing of the broker are what address them.

For 0.1 that trade favours Go. Two things would re-open it:
- a security audit of the broker before 1.0;
- any memory-safety-class finding in its `unsafe` code.

### Considered and rejected

- **TinyGo:** it has no Windows `x/sys` syscall support and cannot build gvisor's netstack.

## Consequences

- The README states this reasoning in short, linking here, and says which guardrails exist
  today and which are still being built.
- Option 2 (a Rust broker) stays the first candidate if the broker's audit or a measurement
  argues for it. The broker's wire protocol (`internal/broker/proto`) is small and specified,
  so it could be reimplemented without changing clients.
- **Guardrail status on 2026-10-31 (updated after the measurement work):**
  - Bounded resources (2): in place in the broker, transport and `internal/diag`.
  - Goroutine-leak checks (1): **in place.** Every test package's `TestMain` calls
    `internal/leaktest` (goleak), and a meta-test fails CI if a test package lacks it,
    passes any exception other than `leaktest.Ignore*`, or imports goleak directly. The
    one exception is go-winio's process-lifetime I/O completion goroutine. The pending
    list is empty: `cmd/hyperbutler-dev` has its `TestMain`. A Linux CI job runs the guest
    packages' tests under goleak and `-race`. Gap: the goroutine loops in
    `guest/cmd/hyperbutlerd` and `guest/cmd/spaceguard` (`_linux.go`) have no tests yet, so
    goleak does not exercise them.
  - Runtime limits (4): **in place.** `internal/diag` (`GOMEMLIMIT` from config unless set
    in the environment; pprof, MemStats, goroutine, handle and Windows commit/heap counts
    on a current-user-only named pipe, off by default) is wired into the engine that
    `hyperbutler start` spawns: a 256 MiB soft limit by default, `--memlimit` kept in
    `settings.json`, the endpoint on with `--debug-pipe` or `HYPERBUTLER_DEBUG_PIPE` for one
    start, refused in an elevated engine. Also in `hyperbutler-dev run` and
    `hyperbutler-broker`.
  - Soak gate (3): **built and run for 1 h by hand** (`bench/soak`, unit- and fuzz-tested in
    CI). It now starts the engine with `hyperbutler start` itself, samples the diag
    endpoint, and its loop includes a bind mount of a Windows directory and a `-p`
    published port fetched from the host. The 24 h release run and the 1 h main run still
    need self-hosted hardware. **1 h run, 2026-10-31** (this machine, 4 GiB / 8 vCPU guest,
    10 min warm-up, 5 min buckets, 12,827 docker commands, none failed; no other VM ran):

    | Metric (50 min window) | Median | Theil–Sen slope | Allowed growth | Verdict |
    |---|---|---|---|---|
    | Engine private working set | 80.2 MiB | +0.56 MiB/h | 4.0 MiB | pass |
    | Engine private bytes | 82.6 MiB | +0.51 MiB/h | 4.1 MiB | pass |
    | Go heap in use | 11.9 MiB | +0.09 MiB/h | 1.2 MiB | pass |
    | Handles / threads / goroutines | 439 / 28 / 126 | 0 / 0 / 0 per h | 22 / 2 / 5 | pass |
    | vmmem / vmwp private working set | 492 / 5.4 MiB | +8.0 / +0.02 MiB/h | 64 / 8 MiB | pass |

    **No leak** is visible at this resolution (a 50-minute window catches growth above
    about 5 MiB/h in the engine). Heap profiles from the end of the warm-up and the end of
    the run hold 11.1 and 9.0 MiB, with the same owners (the share server's buffers, the
    netstack, gopacket's tables); 2.6 GB were allocated over the run in 278 GCs.
  - **The soak's working set is explained.** Under the soak the engine's private working
    set (45 MB in the earlier 13-minute run, 80 MiB now) equals its commit charge, while
    the Go runtime has mapped only 37 MiB (`Sys`) and the Windows heaps hold 1 MiB.
    Walking the process with `VirtualQueryEx`/`QueryWorkingSetEx`: 40 MiB of it is the
    executable's own writable data section, of which **32 MiB is
    `crypto/internal/fips140/drbg.memory`**, Go 1.27's scratch buffer for the FIPS 140-3
    entropy source, linked in through `net/http` → `crypto/tls`, plus about 7 MiB of
    gopacket lookup tables. The buffer is demand-zero and only used in FIPS mode, which is
    off, so it is normally never resident: idle, the engine's private working set is
    17.7 MiB against a commit of 65 MiB. In the runs where the soak launched the engine,
    every committed page became resident within seconds. Making the 32 MiB range
    `PAGE_NOACCESS` from outside cut the launched soak's working set from 78 to 49 MiB and
    the engine never faulted, so no engine code touches it: another process read the
    engine's whole address space (most likely Defender's behaviour-monitoring memory scan;
    a non-elevated user cannot list who holds the handle, so this is not proven). It is
    not a leak and not GC slack. Consequences: the engine's private working set can be
    anywhere between about 18 MiB and its commit (65 MiB idle, 83 MiB under load)
    depending on outside readers, so REQ-MEM-001 budgets should assume the commit. The
    32 MiB is Go's to fix (or avoidable only by dropping every `crypto/tls` importer).
    Twice, at about minute 10 after start, an idle engine's working set also jumped by
    14 MiB with no workload; this looks like the same outside reader but was not traced.
  - **Update, 2026-10-31 evening** ([research/mem-reclaim.md](../research/mem-reclaim.md),
    section 3): the outside read was provoked by the benches' hidden PowerShell per
    sample (12 of 13 runs, always 300 s after the engine started; 0 of 11 with in-process
    sampling). Two changes decided there: release builds link Go's frozen FIPS 140-3
    module `v1.0.0-c2097c7c` with FIPS mode off, which has no entropy buffer (commit 66 →
    35 MiB), at the cost of crypto code that receives no fixes (acceptable while the host
    process uses crypto only for SHA-256 digests and makes no TLS connection; revisit when
    either changes or Go makes the buffer lazy); and the engine runs
    `diag.WorkingSetGuard`, which empties its working set when an outside read makes it
    equal to the commit. The 32 MiB "Go's to fix" above is therefore avoided, not fixed.
- REQ-MEM-001 was re-measured with `bench/mem-idle` as specified (every 10 s from minute 5
  to minute 10, 31 samples, `runtime.MemStats` from the diag endpoint per sample), on
  2026-10-31 with no other VM running: total **315 MiB (331 MB)**, under the 0.1 target of
  400 MB and over the 1.0 target of 250 MB. Engine 17.7 MiB (Go heap in use 5.4 MiB, of it
  2.5 MiB free but retained: GC slack; stacks 1.4 MiB), this VM's vmmem 292 MiB, vmwp
  5.5 MiB. Two earlier runs that overlapped another VM read 299 and 301 MiB. The table at
  the top of this ADR predates the method.
- Go's runtime cost (a few MB, GC) is accepted. If REQ-MEM-001's 1.0 budget of 250 MB is
  missed, the host process is not the first suspect, but it will be measured.
- **Bug found by the soak (bind mounts, ADR 0007):** when a Windows program creates a file
  in a shared directory and writes it right away, the guest can cache the size it had at
  creation (0) for the attribute TTL (1 h): `docker run -v` then reads an empty file.
  Reproduced 1–8 times in 15 tries; a 2 s pause after the write does not help; writing a
  temporary file and renaming it into place does (0 in 25). Likely cause: `HostChanged`
  answers the replayed create with the attributes it read when the create event arrived,
  and drops the following modify event because the node is not yet known
  (`internal/share/inject.go`). The soak writes via rename until this is fixed; the fix
  belongs to the bind-mount track.

## Adversarial review

The adversarial review on 2026-10-30, arguing the strongest case for Rust, raised seven findings.
**Verdict: no blocking objection.**

1. **High. "No mature Rust HCS binding" was false.** `windows-sys` exposes the HCS and HCN
   functions, and hyperbutler itself calls the DLLs directly, not through hcsshim. **Fixed:**
   reason 1 is reworded and marked weak.
2. **High. The README described guardrails as if they existed.** **Fixed:** the README now
   says which guardrails exist and which are planned, and Consequences records their status.
   A release is blocked until they land.
3. **Medium. The measurement did not follow REQ-MEM-001's method.** **Accepted:** it will be
   redone with `bench/mem-idle`, including heap numbers.
4. **Medium. "No observable GC pauses" had no data.** **Fixed:** now stated as not measured,
   and p99 relay latency is added to the ADR 0006 gate.
5. **Medium. The Rust-broker option was dismissed with arguments that don't apply to the
   broker.** **Fixed:** there is now a dedicated section with the real trade-off and the
   triggers that would re-open it.
6. **Low. Binary sizes were missing.** **Fixed:** sizes are recorded.
7. **Low. TinyGo was not mentioned.** **Fixed:** listed as considered and rejected.

### Review of the measurement tooling (2026-10-31)

A adversarial review of the diag wiring and of `bench/mem-idle`, `bench/start` and `bench/soak`
raised ten findings. Fixed: `bench/start` stopped an installation it had not started; a
start cut short left the detached engine running; foreign VMs were only looked for at the
sample instants (now a background watcher from the start command on); `vmmemWSL` and
similar were not counted as foreign; the vmmem PID could be reused unnoticed (each sample
now checks it is still a vmmem under its vmwp); a schedule could overflow; retries with
`-keep-running`; profiles after Ctrl+C; no notice that `--memlimit` waits for the next
start. Accepted: `HeapSummary` on a heap without serialization can be skewed while a DLL
allocates; it runs only when the debug endpoint is asked.
