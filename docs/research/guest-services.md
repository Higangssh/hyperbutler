# Guest services: the host-guest control channel and the guest policies

- Date: 2026-10-30
- Requirements: REQ-DISK-001, REQ-DISK-004, REQ-MEM-002 (REQ-MEM-001 for the metric)
- Machine: Windows 11 Home 10.0.26200, user in Hyper-V Administrators, **non-elevated**.
  The guest is Alpine `linux-virt` 6.18.58 (`vmlinuz-virt` SHA-256 `17FBD3EF…`).
- Code:
  - `internal/guestctl`: the protocol, the host `Client` and `Supervisor`, and the guest
    `Server`.
  - `guest/cmd/winboxd`: the guest daemon.
  - `guest/internal/fsguard`: the guard reservation and FITRIM. It was moved out of
    `guest/cmd/spaceguard`, which now uses it.
  - `guest/internal/policy`: the pure trim and cache policies and the `/proc` parsers.
  - `spike/vhdx` (`hcs -ctl`, `-mode ctl`): the HCS harness.

## Why a control channel

The free-space guard (`docs/research/disk-reclaim.md`, "Free-space guard") carried its
budget as text lines on a second COM port. That was a spike transport: it needed a COM
port per VM, overlapped-pipe workarounds and line parsing. REQ-DISK-001 and REQ-MEM-002
also need guest-side policies the host can see and poke. `guestctl` replaces COM2 with
hvsock on its own vsock port, **2380**. It is a separate HCS service-table entry from the
Docker API relay (2375), with the same user-only connect descriptor and deny-all bind.

## Protocol

Every message is one frame:

```
u32 BE  n          body length, 8 <= n <= 256
u8      version    1
u8      type       0x01-0x05 host->guest, 0x81-0x86 guest->host
u16     reserved   0
u32     seq        host request number; replies echo it; status uses the guest's counter
payload            fixed size per type, padding 0
```

| Type | Dir | Payload |
|---|---|---|
| `Hello` 0x01 | H→G | min, max version (u16 each). Must be the first host frame. |
| `Budget` 0x02 | H→G | flags (bit 0 = unlimited), 7 zero bytes, bytes, backed (u64) |
| `Trim` 0x03 / `CacheTrim` 0x04 | H→G | none |
| `Ping` 0x05 / `Pong` 0x85 | both | nonce (u64) |
| `HelloReply` 0x81 | G→H | version, features (guard, trim, cache), instance (u64) |
| `Status` 0x82 | G→H | guard state and errno, plus 17 u64 values: guard sizes, trim counters, meminfo, cache counters, guard step counter. Sent every 1 s. |
| `TrimResult` 0x83 / `CacheResult` 0x84 | G→H | errno, bytes, duration in ms (and the method for cache) |
| `Error` 0x86 | G→H | busy or unsupported, plus the request type |

**Strict and canonical.** A frame decodes only if re-encoding gives the same bytes. That
means no unknown types for the direction, no wrong payload sizes, no non-zero padding or
reserved bits, booleans and enums in range, errno ≤ 4095, durations that fit in a
`time.Duration`, and no unlimited budget with values. `ReadFrame` reads into a fixed
256-byte buffer and never allocates.

**Host limits on the guest.** These come from `ClientConfig`:
- 5 s handshake and write timeouts, and a 10 s read timeout (the guest reports every second);
- a token bucket of 20 frames/s, burst 40;
- 8 requests in flight;
- every reply must match a pending seq, the expected type and the ping nonce.

Any violation closes the connection. The `Supervisor` redials with a backoff from 100 ms
to 5 s. The backoff is reset only after a connection that lasted 5 s, and cut short when
the host resumes a paused VM.

**Guest limits on the host.** The host's Hello is due within 5 s, and writes time out
after 5 s. At most one trim and one cache trim run at a time; a second request gets
`Error busy`. A newer host connection replaces the older one. Each connection carries a
generation number, so events from a replaced connection are dropped (see the review, M1).

## Guest policies (winboxd)

1. **Guard (REQ-DISK-004).** This is the `internal/spaceguard` policy, unchanged.
   - It fails closed without a budget, and once its own ticks say the budget is 30 s
     stale.
   - After the connection drops, it keeps the last budget for `-lost-grace` (1 s), then
     fails closed. The budget is exact for the guest's own writes. The grace therefore
     only lets other host writers use the margin for 1 s, and the host floor pause still
     applies.
   - Before growing the reservation by more than the hysteresis band, it runs FITRIM.
2. **fstrim (REQ-DISK-001).** `policy.Trim` samples `statfs` free space every second. A
   delete is a rise of 64 MiB or more over the lowest free space seen since the last trim.
   - It trims once free space has been stable for 6 s, which is longer than ext4's 5 s
     journal commit, since FITRIM only sees committed frees.
   - It trims after at most 30 s if free space keeps rising.
   - It trims every 2 min regardless.
   - Trims are at least 5 s apart. Guard and host trims reset the policy.
   - FITRIM on ext4 skips groups that were already trimmed, so a repeated trim costs
     milliseconds.
3. **Page cache (REQ-MEM-002).** `policy.Cache` samples `/proc/stat`, `/proc/vmstat` and
   `/proc/meminfo` every 2 s. The guest counts as idle when CPU is below 10 % busy and
   paging is below 4 MiB/s.
   - After 10 s idle, if file cache (`Active(file)+Inactive(file)`, so Shmem and the
     initramfs are excluded) exceeds keep (32 MiB) + threshold (64 MiB), it reclaims the
     excess.
   - It reclaims through cgroup v2 `memory.reclaim` with `swappiness=0`. That is LRU
     ordered, so the hottest pages stay. Without cgroup v2 it falls back to
     `vm.drop_caches=1`.
   - It then writes `vm.compact_memory`. hv_balloon reports free pages only in order-9
     (2 MiB) blocks, and compaction makes them.
   - Reclaims are at least 30 s apart. The host can force one with `CacheTrim`.
   - Better kernel knobs that were checked:
     - `CONFIG_DAMON` (DAMON_RECLAIM) is off in `linux-virt`.
     - MGLRU is on, but it has no proactive-reclaim trigger of its own.
     - `memory.reclaim` is the best knob this kernel has.

## Results (HCS, non-elevated)

The ctl and guard runs below use the **final code** (after the review fixes).

**Setup:**
- `spike/vhdx hcs -ctl -mode ctl`, on a 4 GiB sparse VHDX with 1 MiB blocks on C:.
- 3072 MiB guest with `EnableDeferredCommit`, `EnableHotHint` and
  `EnableColdDiscardHint`, and `hv_balloon` loaded.
- Memory is the private working set of **this VM's** vmmem process, from
  `Win32_PerfRawData_PerfProc_Process.WorkingSetPrivate`.
  - The process is found by its PID appearing across `HcsCreateComputeSystem`.
  - A non-elevated caller gets access denied from `GetOwner`, so it cannot match the
    owner, and other VMs were running on the host at the same time.

**Page cache (REQ-MEM-002):**
- The guest writes 2 GiB, drops the cache once as setup, and reads everything twice.
- **Before:** at ready, 260 MiB private; with 2 GiB cached, 2370 MiB.
- winboxd reclaimed 2049 → 31 MiB in 20 ms, 16 s after the read: 10 s idle detection plus
  sampling.
- vmmem private working set:

  | Time after the read | Private working set |
  |---|---|
  | +16 s | 1608 MiB |
  | +22 s | 456 MiB |
  | **+27 s** | **350 MiB** |
  | +59 s | 348 MiB (stable) |

- **Result:** within 60 s, with ready + 100 MiB reached at +27 s.

**Variants**, all with 1.5 GiB cached and a 258–260 MiB baseline at ready:

| Method | Lowest private WS | Reached at |
|---|---|---|
| `memory.reclaim` keep 64 MiB, no compaction | 424 MiB | +27 s |
| `memory.reclaim` keep 64 MiB + compaction | 372 MiB | +22 s |
| `drop_caches=1` + compaction | 312 MiB | +27 s |
| **final:** `memory.reclaim` keep 32 MiB + compaction (2 GiB) | **350 MiB** | +27 s |

- Compaction returns about 50 MiB more.
- Most of the rest is the deliberately kept cache.

**Side effects** (measured in the same runs):
- The reclaim and compaction take 7–20 ms of guest time.
- The next read of the 2 GiB that went idle is cold: 0.61 s, against 0.51 s for the first
  cold read and 0.01 s hot. Only data idle for 10 s or more is evicted.
- Since the kept 32 MiB is LRU-ordered, recently used binaries stay hot.

**Delete reclaim (REQ-DISK-001):** the same run then deletes the 2 GiB. There was no
manual fstrim, and the guest only waits.
- winboxd's delete trigger trimmed 6–10 s after the delete.
- The VHDX went from 2056 MiB to 24 MiB, against 8 MiB before the write. It was measured
  10 s after the delete, in 5 s polls.
- It was reclaimed within 10 s in all 6 ctl runs.
- REQ-DISK-001 allows 5 min.

**Guard (REQ-DISK-004)**, on `T:\guestctl`:
- 1 GiB sparse VHDX. The 3 GiB test volume had only about 170 MiB free, because files
  from other tests sit at its root.
- Margin 32 MiB, floor 12 MiB, budget over guestctl.

| Run | Writes | Min host free | Pauses | ext4/IO errors | `e2fsck` (before / after mount) | Files kept → read back | Manifest |
|---|---|---|---|---|---|---|---|
| Guard over guestctl | 268 OK, then **10/10 `ENOSPC`**, 0 `EIO` | 101.7 MiB | 0 | 0 | 0 / 0 | 132 → 132 | match |
| Plus a host filler that holds the volume at 5.9 MiB for 15 s | 268 OK, then 10/10 `ENOSPC`, 0 `EIO` | 5.9 MiB | 1 (15 s, resumed) | 0 | 0 / 0 | 132 → 132 | match |

An earlier run with a 2 GiB disk and a 64 MiB margin reserved the whole disk: there was
no room for data, and `ENOSPC` came on the first write. That is correct. The unbacked
ext4 metadata (about 100 MiB) and the margin used up the 168 MiB free.

**Finding: `HcsPauseComputeSystem` aborts the host's hvsock connections.**
- The host read failed with `WSAECONNABORTED` in the same 100 ms as the pause.
- After the resume, the guest logged the lost channel and started its 1 s grace.
- The Supervisor, kicked by the resume, reconnected in the same tick (19.5 s). So the
  guard never failed closed.
- Before the kick, the reconnect depended on where the backoff stood (0.9 s in that run;
  up to 5 s is possible).
- `Supervisor.SetPaused` and `Client.Suspend` also keep a connection that survives a
  pause from timing out. That case was not seen on HCS, but the code handles it.

## Fuzzing

Each target ran for 2 minutes on the final code:

| Target | Execs | What it checks |
|---|---|---|
| `FuzzDecode` | 25.6M | canonical re-encode, direction gating, size bounds |
| `FuzzReadFrame` | 19.3M | exact consumption, bounds, same result byte-by-byte |
| `FuzzServeConn` (guest handler) | 3.65M | returns, ConnLost exactly once, nothing before a valid Hello, replies match requests, bounded output |
| `FuzzClient` (host handler of guest input) | 0.45M | no deadlock or panic, requests return the right type or an error, Close is idempotent, statuses re-encode |
| `FuzzPolicies`, `FuzzParseProc` | 10.7M, 42.0M | trim never inside MinInterval, reclaim ≤ cache − keep, parsers bounded |

The corpus is in `internal/guestctl/testdata/fuzz/`. No crashes were found, either in the
first rounds (`fuzz-engineer`: 26.5M, 27.3M, 2.67M and 0.77M execs) or in the re-run.

## Adversarial review

| # | Finding | Resolution |
|---|---|---|
| H1 | Status comes from the server's own goroutine, so a stuck guard loop still looks alive, and the host keeps pausing at the floor instead of the margin. | **Fixed:** `Status.GuardSteps` counts guard steps. The host treats a guard as live only when `Guard == OK` and the counter advanced. |
| H2 | A host pause longer than 10 s leads to a read timeout, a reconnect, ConnLost and a full-disk reservation, and the host oscillates between pause and resume while the silence clock runs. | **Fixed:** `Supervisor.SetPaused` / `Client.Suspend` turn the read timeout off and hold budgets back while paused, and resume kicks an immediate redial. The host resets the silence clock on resume. The guest has a 1 s lost-grace. Tested with a real 15 s pause (above). |
| M1 | A stale-generation ConnLost could evict the current connection's budget from the 1-slot channel. | **Fixed:** `mailbox` drops events from replaced connections when they are posted. Unit tests cover the review's sequence. |
| M2 | A guest that completes the handshake and drops gets redialed every 100 ms forever. | **Fixed:** the backoff resets only after a connection that lasted `MaxBackoff`. Test: at most 12 dials in 1.5 s. |
| M3 | The Supervisor could resend an older budget over a newer one on connect. | **Fixed:** the initial budget is sent under the Supervisor lock. |
| M4 | Every reconnect fails the guard closed (reserve everything, then release). | **Fixed:** `-lost-grace` (1 s), as above. |
| L1 | A guard-driven trim did not reset the trim policy. | **Fixed:** `FSTrimmer.OnTrim` records every trim. |
| L2 | A grow within `TrimEvery` of a trim may reserve still-backed blocks. | **Fixed:** a grow by more than the hysteresis band trims unconditionally. |
| L3 | A guest reporting `Short != 0` got a log line per status. | **Fixed:** status logging is rate-limited regardless of content. |
| L4 | An idle working set above keep + threshold is reclaimed every 30 s and re-faulted on wake. | **Rejected:** reclaiming it is the point of REQ-MEM-002. It happens only after 10 s of idle, and the cost was measured (a cold re-read). Keep and threshold are flags. |
| Gaps | No ADR; no tests for winboxd; the measurements are a spike harness rather than an `integration` test. | The **decisions are recorded here** for the orchestrator to promote to an ADR with the production design. **winboxd's event logic is now unit-tested** (`mailbox_test.go`). The task allowed the harness in `spike/vhdx`, and it records the edition (this file). |

**Host trust of guest numbers.** No guest-reported value feeds the budget or the floor
pause:
- The budget is computed from host measurements only: `GetDiskFreeSpaceEx` and the VHDX
  allocation.
- Guest status affects only one thing: whether the host pauses at the **margin** instead
  of the floor when the guard looks dead. A lying guest can only make the host pause
  *earlier* (by going silent) or keep the floor (by faking progress). The floor is always
  enforced.
- Status values are logged, rate-limited, and never parsed further.

The broader guest-root attacks from the guard work still stand. For example, the guest
can write into the reservation file. The hard floor is the guarantee against a hostile
guest.

## Integration steps for the orchestrator

These are not applied here: `guest/prototype` and `cmd/winbox-dev` belong to another
session.

1. **Build:** add `winboxd` to the guest image build next to `agent`
   (`GOOS=linux CGO_ENABLED=0 go build ./guest/cmd/winboxd`) and install it to `/usr/bin`.
2. **Modules:** `guest/prototype/init` already loads `hv_sock` and `hv_balloon`
   (`modprobe -a hv_storvsc sd_mod hv_sock hv_balloon hv_utils`), and `vsock` comes in
   as a dependency. Keep `hv_balloon`: REQ-MEM-002 depends on free page reporting.
3. **cgroup v2:** init already mounts it at `/sys/fs/cgroup`, which `memory.reclaim`
   needs.
4. **Init line:** add a restart loop like the agent's, after the data disk is mounted at
   its mount point:

   ```sh
   (while :; do
   	winboxd -mount /var/lib/docker -dev /dev/sdX -file /var/lib/docker/.winbox-reserve >/dev/console 2>&1
   	t "winboxd-exit $?"
   	sleep 1
   done) &
   ```

   Until the prototype has a data disk (today `/var/lib/docker` is tmpfs), start it
   without the guard and the trim, with cache only: `winboxd -no-trim`.
5. **VM document** (`internal/vm`, used by `cmd/winbox-dev`):
   - add `vm.Service{ID: guestctl.ServiceID, ConnectSDDL: <user-only SDDL>}` next to the
     2375 service;
   - set the memory flags `EnableDeferredCommit`, `EnableHotHint` and
     `EnableColdDiscardHint` (ADR 0002).

   `internal/vm.Config` has no memory-hint or SCSI fields yet.
6. **Host:** in `cmd/winbox-dev run`, after `RuntimeID`:
   - start `guestctl.Supervisor{Dial: guestctl.HvsockDialer(id)}`;
   - feed it `spaceguard.HostBudget` every 100 ms from the VHDX volume (see
     `spike/vhdx/guard_windows.go`);
   - call `SetPaused` around `HcsPause/ResumeComputeSystem`.

   Treat `Status` as display and liveness data only.
7. **Multiple VHDXs on one volume:** each still gets the full spare. Divide it before
   there is more than one data disk (still open from the guard work).

## Open

- The REQ-MEM-002 bench as specified (12 GB VM, 4 GB anonymous + 4 GB cache) has not been
  run. The runs here cover 2 GiB of cache on a 3 GiB VM.
- REQ-DISK-001's 24 h hold, and 10 GB of images through dockerd.
- `-race` needs cgo, which this host lacks. Run `go test -race ./internal/guestctl/` in
  Linux CI.
- Trimming during heavy writes is cheap on ext4, but this has not been measured under
  load.
