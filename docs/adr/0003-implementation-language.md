# 0003. Implementation language

- Status: accepted (2026-10-30; release-blocking guardrails listed under Consequences)
- Date: 2026-10-30
- Requirements: REQ-MEM-001, REQ-PLAT-003, REQ-API-001, REQ-DISK-004

## Context

winbox has a few long-running parts:
- the host process `winbox.exe`;
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

`winbox-dev` is the Go host process: an HCS VM, a named-pipe server and the hvsock proxy.
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
   winbox uses `go-winio` and reads `hcsshim`'s schema and behaviour as its reference. It
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
   - Binaries built with `-s -w`: `winbox-dev.exe` 7.3 MB, `winbox-broker.exe` 2.6 MB, guest
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
- **Guardrail status on 2026-10-30:**
  - Bounded resources (2): in place in the broker and transport.
  - Goroutine-leak checks (1): only in `internal/transport`, as goroutine-count deltas.
    `goleak` is not yet wired into every package.
  - Soak gate (3) and runtime limits (4): not built yet.
  - These are the next work items, and a release is blocked until 1, 3 and 4 exist.
- The idle measurement above did not follow REQ-MEM-001's method (sampling every 10 s from
  minute 5 to minute 10). It will be redone with `bench/mem-idle`, recording
  `runtime.MemStats` heap numbers next to the private working set, so that a leak can be told
  apart from GC slack.
- Go's runtime cost (a few MB, GC) is accepted. If REQ-MEM-001's 1.0 budget of 250 MB is
  missed, the host process is not the first suspect, but it will be measured.

## Adversarial review

The adversarial review on 2026-10-30, arguing the strongest case for Rust, raised seven findings.
**Verdict: no blocking objection.**

1. **High. "No mature Rust HCS binding" was false.** `windows-sys` exposes the HCS and HCN
   functions, and winbox itself calls the DLLs directly, not through hcsshim. **Fixed:**
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
