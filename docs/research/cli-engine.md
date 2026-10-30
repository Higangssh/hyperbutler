# The winbox CLI and engine

- Date: 2026-10-30
- Requirements: REQ-UX-001, REQ-API-001, REQ-START-001, REQ-MEM-001, REQ-DISK-001,
  REQ-DISK-003/004/005/007
- Machine: Windows 11 Home 10.0.26200, 8 logical CPUs, NTFS `C:`. The user is in Hyper-V
  Administrators and every command ran from a **non-elevated** PowerShell.
- Guest: the `guest/prototype` initramfs (Alpine `linux-virt` 6.18.58, Docker 29.9.2) with
  winboxd and e2fsprogs 1.47.4 added.
- Code:
  - `cmd/winbox`: the user-facing CLI.
  - `internal/engine`: the VM lifecycle (`Run`), the background engine (`Serve`), the
    state directory and its files, the lock, the host free-space guard and the data disk.
  - `internal/engine/dockerctx`: the docker context, written as the docker CLI writes it.
  - `cmd/winbox-dev`: now a thin wrapper over `engine.Run`.
  - Small additions: `internal/vm` (SCSI disks, memory hints, pause/resume/shutdown,
    open/enumerate, VM access grants, sparse VHDX), `internal/guestctl` (an orderly
    shutdown request), `guest/cmd/winboxd`, `guest/prototype`.

## User flow

```powershell
winbox start              # first start: creates %LOCALAPPDATA%\winbox and a 64 GiB sparse data disk
docker run --rm hello-world
docker run -d -p 8080:80 nginx; curl http://127.0.0.1:8080/
winbox status
winbox logs [--console] [-n 100] [-f]
winbox stop               # orderly guest power-off; killed after --timeout (45 s)
winbox delete [--yes]     # removes the VM state and the data disk
```

- `start` takes `--data DIR` (first start only), `--image-dir`, `--memory`, `--cpus`,
  `--disk-size` (first start only), `--pipe`, `--timeout`, `--no-context`, and
  `--memlimit` / `--debug-pipe` (internal/diag, off by default).
  - The choices are kept in `settings.json`.
  - A different `--data` later is refused; moving the data (`winbox move`, REQ-DISK-007)
    is not implemented yet.
- **Guest artifacts:** `vmlinuz-virt` and `initramfs-winbox.gz` come from `--image-dir`,
  next to `winbox.exe` by default.
- `start` spawns `winbox.exe engine` **detached** (`DETACHED_PROCESS`, a new process group,
  and `CREATE_BREAKAWAY_FROM_JOB` where the job allows it). Its output goes to
  `logs\engine.log`. `start` waits until the engine reports the Docker API answers, then:
  - creates and selects the `winbox` docker context
    (`npipe:////./pipe/docker_engine_winbox`);
  - never touches a context of that name it did not create, and says so instead.
- That is REQ-UX-002's process budget: `winbox.exe` (the engine) and `vmmem`.
- **When HCS denies access,** `start` explains it: the Hyper-V Administrators group, the
  one-time `net localgroup` command an administrator runs, sign-out, and the planned ADR
  0004 broker. It also says whether the current token has the group.

### State directory

`%LOCALAPPDATA%\winbox` (`WINBOX_HOME` overrides it) holds:

| File | Written by | What |
|---|---|---|
| `settings.json` | CLI | data dir, disk size, memory, CPUs, pipe |
| `state.json` | engine | phase, PID + creation time, VM id, runtime id, pipe, timings, last error and its code |
| `engine.lock` | engine | `LockFileEx` single-instance lock, held for the engine's life |
| `ops.lock` | CLI | serializes `start`, `stop` and `delete` |
| `logs\engine.log`, `logs\console.log` | engine | console log capped at 2 x 8 MiB |
| `<data>\data.vhdx`, `<data>\disk.json` | engine | data disk and its creation-time metadata size |

- Every file is written atomically (temporary file, flush, rename).
- The parsers are strict: one object, known fields, current version, every field in range.
  They are fuzzed.
- A directory winbox creates gets a protected DACL (user, SYSTEM, Administrators).
  Reparse points are refused for the data dir, the lock files and the disk.

### Engine lifecycle

1. Take `engine.lock`. If another engine holds it, the code is `already-running`.
2. Write `state.json` (`starting`).
3. Create the stop event `Local\winbox-engine-stop-<pid>-<created>`, with a user-only DACL.
   A name that already exists is refused.
4. Terminate compute systems named `winbox-<home hash>-*` with owner `winbox`. Normally
   there are none: every VM has `ShouldTerminateOnLastHandleClosed`.
5. `Run`, in this order:
   - listen on the pipe (fails before any VM exists if it is taken);
   - prepare the data disk;
   - `HcsGrantVmAccess` on the VHDX;
   - create the VM (SCSI disk with `AlwaysAllowSparseFiles`; `EnableDeferredCommit`,
     `EnableHotHint`, `EnableColdDiscardHint`; hvsock 2375 and 2380 with user-only connect,
     and the network service with user-only bind);
   - start the guestctl `Supervisor` and the host guard;
   - wait for `/_ping`, then serve the pipe. The phase becomes `running`.
6. **Stop:** the event, Ctrl+C, or sign-out/shutdown. Sign-out and shutdown arrive through
   a hidden window: the engine has no console. `Run` then:
   - closes the guard, resuming the VM if the guard had paused it;
   - sends guestctl `Shutdown` → winboxd sends SIGTERM to init → dockerd stops the
     containers → `umount` → `poweroff -f`;
   - waits up to 25 s, then terminates the VM.
   - `HcsShutDownComputeSystem` is only a fallback. It fails with `ERROR_NOT_SUPPORTED`
     here: there is no shutdown integration service.
7. Write `stopped` (or `failed` with the code), revoke the VHDX grant, release the lock.

### Data disk

- **Creation** (first start) is crash-ordered:
  1. `CreateVirtualDisk` writes `data.vhdx.new`: dynamic, 64 GiB, 1 MiB blocks.
  2. `FSCTL_SET_SPARSE` marks it sparse.
  3. `disk.json` records the allocation as the metadata size.
  4. A rename makes it `data.vhdx`.

  A `disk.json` without a disk, or a disk without `disk.json`, is an error, never a silent
  reformat.
- **The guest formats only when all three hold:**
  - the host passes `winbox.newdisk=1`, which it does only while the VHDX allocation still
    equals its creation-time metadata, so the guest has never written to it;
  - a complete read of the first MiB succeeds and is all zero;
  - `blkid` finds no file system.
- **The format:** `mke2fs -t ext4 -m 0 -J size=128 -E nodiscard,lazy_itable_init=1,lazy_journal_init=1`,
  then a mount with `noatime,noinit_itable`. Otherwise `e2fsck -p` runs first; a result of
  4 or more powers the VM off with the reason on the console.
- `/var/lib/docker` lives on this disk: images, containers, volumes and **json-file logs**.
  The earlier tmpfs filled RAM with container logs; that is fixed.
- **Free-space guard (REQ-DISK-004/005):** the host sends `spaceguard.HostBudget` every
  100 ms.
  - Margin 5 GiB: guest writes get ENOSPC before the host volume drops below 5 GiB, which
    is REQ-DISK-005's default reserve.
  - Floor 1 GiB: the VM pauses there.
  - Before winboxd's first report only the floor applies. After it, a guard that stops
    showing progress for 5 s moves the pause up to the margin.

## Measurements

All runs: 4096 MiB guest memory, 8 vCPUs, a non-elevated token, a second install side by
side (`--pipe \\.\pipe\docker_engine_winbox_cli`, `WINBOX_HOME` and `DOCKER_CONFIG` in a
temp directory, because another session's soak test held the default pipe).

**End to end:**

| Step | Result |
|---|---|
| `winbox delete --yes`, then the first `winbox start` (VHDX created, mkfs) → `docker info` OK | **1.97 s**. mkfs took 40 ms (guest 0.79 → 0.83 s), winboxd ready at guest 0.98 s |
| `docker run --rm hello-world` (pull) | 3.2 s, "Hello from Docker!" |
| `docker run -d -p 8080:80 nginx` (pull), then `curl 127.0.0.1:8080` | answered 56 ms after `run` returned |
| `winbox stop` | 1.7 s; the guest powered off in order in 1.1 s (dockerd exit, unmount) |
| After stop | this VM's vmmem (PID 19300) gone; `hcsdiag list` shows no `winbox-65fd2a1a-*` |
| `winbox start` again → `docker info` | 1.96 s; `hello-world` and `nginx` still listed (persistence) |

**Disk reclaim (REQ-DISK-001):** `node:22` and `python:3.13` were pulled (3.01 GiB as docker
counts it), then `docker rmi` and `docker system prune -f`. No manual step.

| Run | VHDX before the pull | With the images | After the delete | Time to settle |
|---|---|---|---|---|
| 1 (disk formatted with the default journal) | 282 MiB | 3353 MiB | 604 MiB | ≤ 11 s |
| 2 (fresh disk, `-J size=128`) | 264 MiB | 3186 MiB | 1107 MiB | ≤ 11 s, unchanged to +301 s |

- The shrink comes from winboxd's delete-triggered FITRIM, 6–10 s after the delete.
- Both runs meet REQ-DISK-001's "within 1 GB of baseline in ≤ 5 min": 322 and 843 MiB above
  the baseline.
- **Run 2's residual is not a trim gap.** A manual `fstrim -v` afterwards trimmed the whole
  free space and returned nothing more, and the file system held 246 MiB. What stays
  allocated are 1 MiB VHDX blocks that still hold live data (the kept images, metadata, the
  journal) next to the freed space.
- In run 1 the residual was mostly the ext4 journal: the default for 64 GiB is several
  times 128 MiB, and its blocks, once written, never become free. Hence `-J size=128`.
- See "Open".

**REQ-START-001**, 20 runs of `winbox start` → `docker info` OK → `winbox stop`, warm host
cache, existing disk: one warm-up run discarded, n = 20:
**median 1.95 s, p95 1.96 s** (min 1.94, max 1.97). The 0.1 target is ≤ 8 s / p95 ≤ 12 s, and
1.0 is ≤ 5 s / ≤ 8 s. `winbox stop` took a median of 1.33 s. Every stop was orderly.

**REQ-MEM-001** (private working set from `Win32_PerfRawData_PerfProc_Process`, every 10 s
from minute 5 to minute 10 of idle, VM running, no containers): 28 samples:

| Process | Median | Max |
|---|---|---|
| `winbox.exe` engine | 15.8 MiB | 16.6 MiB |
| this VM's vmmem | 564 MiB | 564 MiB |
| **Total** | **580 MiB** | |

This **misses REQ-MEM-001** (median ≤ 400 MB at 0.1). Almost all of it is the guest. The
prototype keeps its whole root file system in RAM: a 66 MB gzip initramfs, the dockerd
and containerd binaries unpacked as Shmem. A 4 GiB guest also has more page tables and
kernel structures than the 3 GiB VM of guest-services.md (260 MiB at ready). The engine
itself is 16 MiB. ADR 0005's erofs system disk is the planned fix; see "Open".

## Tests and fuzzing

Unit tests:
- the state files: round trip, every rejection, atomic writes, size bounds;
- `ParseSize`;
- the lock: exclusive, rides over a probe, refuses a junction, **released when the holder
  is killed**;
- `OpenProcess`: PID reuse detected through the creation time;
- the stop event: refuses an existing name;
- `prepareDisk`: every crash point, a missing disk or meta, a non-sparse file;
- `MkdirPrivate`: refuses a junction;
- the host guard: pause at the floor, resume, the margin rule only after a first report,
  fail closed, resume on close; the shipped config meets REQ-DISK-005;
- `CleanupOrphans`: prefix and owner;
- the session-end window;
- dockerctx: a foreign context is never touched, `config.json` keys are kept;
- the CLI without a hypervisor: fresh install, crash-left state, a fixed data location, a
  fixed disk size, and delete removing only its own files;
- guestctl: shutdown round trip, feature gating, refusal.

Every new test package has the leaktest `TestMain`. Fuzz targets, 45 s each on this
machine:

| Target | Execs | Result |
|---|---|---|
| `engine` FuzzParseState / Settings / DiskMeta / Size | 0.44M / 0.16M / 1.8M / 1.9M | ok |
| `dockerctx` FuzzParseMeta / FuzzSetCurrentContext | 0.10M / – | **crasher**: a non-UTF-8 name came back as U+FFFD. Fixed (rejected); entry `invalid_utf8_name` committed |
| `cmd/winbox` FuzzPrintable / ParseMemoryMB / LastLines | 0.26M / 0.69M / 1.1M | ok |
| `vm` FuzzParseSummaries / FuzzParseState | 4.5M / 4.6M | ok |
| `guestctl` FuzzDecode / ServeConn / Client (after the shutdown messages) | 4.3M / 0.11M / 0.05M | ok; shutdown seeds added |

## Adversarial review

| # | Finding | Resolution |
|---|---|---|
| 1 high | Init took a **failed read** of the data disk for a blank disk and would `mke2fs -F` it. | **Fixed:** the three-way blank check above (host `newdisk`, a full successful read, `blkid`). |
| 2 med-high | Settings were read before the ops lock, so two concurrent first `start`s could re-pin `--data`. | **Fixed:** `start`, `stop` and `delete` re-read the settings under the ops lock. |
| 3 medium | `waitReady` matched `state.json` by PID only; a reused PID gave a stale verdict. | **Fixed:** it matches PID and creation time, taken while holding the child's handle. |
| 4 medium | The console-less engine never sees sign-out or shutdown, so the guest died uncleanly. | **Fixed:** a hidden top-level window handles `WM_QUERYENDSESSION`/`WM_ENDSESSION`, sets a shutdown block reason, and holds `WM_ENDSESSION` up to 20 s for the orderly stop. Tested with synthetic messages; a real sign-out was not run. |
| 5 medium | Fuzz crasher in `SetCurrentContext`. | **Fixed**, with the corpus entry committed. |
| 6 medium | The guard used the 1 GiB spike margin, not REQ-DISK-005's 5 GB reserve. | **Fixed:** `GuardConfig` (margin 5 GiB, floor 1 GiB) and a test. |
| 7 low-med | (a) The guard counted itself dead 5 s after creation, before winboxd could report, so a boot could pause and oscillate. (b) A guard-paused VM could not be shut down in order. | **Fixed:** the margin rule applies only after the first report, and `close` resumes. Tests for both. |
| 8 low | The `Local\` stop event cannot be reached from another logon session (OpenSSH, task scheduler); `stop` then kills the engine. | **Rejected for now:** `Global\` objects need SeCreateGlobalPrivilege, which a standard interactive user lacks. Recorded under Open: the broker's per-user pipe is the real fix. |
| 9 low | `Held` reads as "running" while a CLI holds the engine lock for orphan cleanup. | **Rejected:** cosmetic and short (one HCS enumerate). `status` is informational and takes no lock; `start`, `stop` and `delete` serialize on the ops lock. |
| 10 low | VM-account ACEs pile up on `data.vhdx` after crashes. | **Fixed:** crash recovery revokes the dead engine's VM id. |
| 11 low | The `start` timeout did not kill an engine that never wrote a state. | **Fixed:** it is killed through the child handle. |
| 12 low | Dangling reference to this document. | **Fixed:** this file. |

Also checked by the reviewer and found fine:
- lock semantics and handle inheritance;
- `CleanupOrphans` prefix and owner matching;
- dockerctx ownership and path hashing;
- `prepareDisk` ordering;
- the VM document;
- the shutdown path;
- the pipe ACL;
- LF guest files.

## Open

- **REQ-DISK-001 residual:** up to ~0.8 GiB of 1 MiB VHDX blocks stay allocated when live
  and deleted data shared them. It is within the requirement, but the 10 GB / 24 h bench
  (`bench/disk-reclaim`) has not been run. Options: an ext4 allocation policy that keeps
  image layers apart, or an occasional offline compaction on `winbox stop`.
- **The stop event is per session** (finding 8).
- **Real sign-out and shutdown** have not been exercised end to end.
- **Hyper-V Administrators only:** no broker (ADR 0004). `Backend` is the seam where it
  plugs in, and it will build the VM document itself.
- **A guest that cannot shut down in order** (old image without `FeatureShutdown`) is
  terminated after 25 s; ext4's journal and `e2fsck -p` cover it.
- **REQ-MEM-001 is missed:** 580 MiB idle against the 400 MB target, nearly all of it
  the in-RAM initramfs root file system. ADR 0005 A2b (an erofs system disk) is the fix.
  The initramfs share was not split out from the rest of the guest here.
- **REQ-START-001 first boot after a host reboot** was not measured. The first boot with a
  new disk took 1.97 s, the same as a warm one.
- **`-race`** needs cgo, which this machine lacks; CI runs it.
