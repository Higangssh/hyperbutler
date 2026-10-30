# Bind mounts from Windows paths

- Date: 2026-10-30
- Requirements: REQ-FS-001, REQ-FS-002, REQ-FS-003, REQ-FS-004 (pain points F1, F2)
- Decision record: [ADR 0007](../adr/0007-bind-mounts.md)
- Code: `internal/share` (host file server), `internal/share/relay` (guest relay),
  `spike/share` (measurement harness and the `docker run -v` demo), an additive
  `Plan9` option in `internal/vm/config.go` (measurement only).
- Machine: the ADR 0005 machine, Windows 11 Home 10.0.26200, NTFS `C:`, Defender in passive
  mode (real-time scanning off; REQ-PLAT-007 needs a rerun with it on). The account is in
  Hyper-V Administrators; **every run was non-elevated**.
- Guest: HCS VM, 4096 MiB, 4 vCPUs, Alpine `linux-virt` 6.18.58, the hyperbutler initramfs with
  `fuse.ko` and the 9p modules added. Most numbers are **one run each**; treat differences
  under 20 % as noise.

## Candidates

| | How | Works without elevation? | Verdict |
|---|---|---|---|
| (a) HCS Plan9 | `Devices.Plan9.Shares` in the VM document. The VM worker (vmwp) serves 9P2000.L over hvsock; the guest dials host port 564 and mounts `9p` with `trans=fd`. WSL's drvfs and LCOW work this way. | **Yes**, after `HcsGrantVmAccess` on the shared directory. | Works, slow, no change notifications. |
| (b) virtio-fs | No such device in the HCS schema. `VirtioFs` and other unknown keys are rejected at create (`0x8037010D`, "Construct"). `vmvirtio.dll` is only a helper library for *external* device emulators attached through `FlexibleIov`, which needs a registered emulator COM class (WSL ships one, `wsldevicehost.dll`; this machine has none). A `FlexibleIov` entry with an unregistered `EmulatorId` is rejected the same way. | n/a | **Not available** without writing and registering an undocumented out-of-process device emulator. |
| (c) hyperbutler file server over hvsock | The guest kernel's FUSE client; a tiny guest relay copies `/dev/fuse` frames to hvsock unchanged; the host decodes FUSE and performs it on NTFS (`internal/share`). Changes on Windows are pushed as FUSE invalidations and replayed so inotify fires. | **Yes** (a user-only `BindSecurityDescriptor` on one hvsock service). | **Recommended.** |
| (c') 9P2000.L server in Go | Same transport, kernel `9p` client. | Yes | Not built: 9P has no server-to-client messages, so it can neither invalidate a cache nor deliver events, and its client has no per-request pid for replay. It would inherit (a)'s two biggest problems. |
| (d) sync (mutagen-style) | Copy the tree into guest ext4 once, then push each Windows change (`ReadDirectoryChangesW`) into the copy; guest writes would have to be synced back. | Yes | Fastest inside the guest; costs double disk, an initial copy, and a two-way sync engine with conflicts. Measured one-way only. |
| SMB/CIFS to the host, NFS | Needs a share configured on the host (an admin and system-setting change) and the network path. HCS `VirtualSmb` is for Windows guests. | No | Rejected. |

## Transport ceiling (hvsock)

A raw guest-to-host AF_VSOCK stream on this machine (`share-spike -vsockbw`):

| Connections | Up (guest→host) | Down (host→guest) |
|---|---|---|
| 1 | 550–700 MB/s | 370–450 MB/s |
| 2 | 1040 MB/s | 880 MB/s |
| 4 | 1780 MB/s | 1510 MB/s |
| 8 | 2030 MB/s | 1640 MB/s |

- One-byte round trip: **p50 ≈ 47 µs**, p99 ≈ 80–100 µs. Guest `SO_SNDBUF`/`SO_RCVBUF`
  change nothing.
- So the relay opens **4 connections** (queues) and the server treats them as one session.
  Every metadata operation that has to reach the host costs at least one ~50 µs round trip,
  against about 1 µs for ext4 in the guest.

## Measurements

`share-spike -targets ext4,fuse,plan9[,sync] -files 20000 -mb 1024 -notify 20 -compat`.

- **ext4:** an ext4 file system on a sparse VHDX on SCSI (the guest's own disk).
- **fuse:** this design, 4 queues, 1 h cache TTLs with host invalidation, no writeback cache,
  4 MiB readahead.
- **plan9:** HCS Plan9, `msize=65536` (LCOW's value), default `cache=none`.
- **sync:** ext4, with the tree pushed from Windows. Its benchmark rows are ext4's, since the
  work runs on ext4.

The untar tree is node_modules-like: 20,000 files of mostly 0.5–8 KiB in about 2,860 directories,
182 MiB in total. `walk` is what `git status` costs without git: readdir every directory and
lstat every entry. git itself is not in the guest image.

| Workload | ext4 | fuse (c) | ×ext4 | plan9 (a) | ×ext4 |
|---|---|---|---|---|---|
| Sequential write 1 GiB (+fsync) | 2740 MB/s | 282 MB/s (488 with writeback cache) | 9.7× (5.6×) | 191 MB/s | 14× |
| Sequential read 1 GiB, cold guest cache | 4710 MB/s | 860 MB/s (990 with 16 MiB readahead) | 5.5× | 59 MB/s | 80× |
| Untar 20k files | 0.36 s | 36.2 s | 100× | 96.2 s | 265× |
| Walk, cold guest cache | 0.052 s | 2.30 s | 44× | 56.7 s | 1090× |
| Walk, warm | 0.027 s | 1.47 s | 54× | 54.9 s | 2030× |
| lstat storm, p50 / p99 | 0.80 / 1.5 µs | 0.92 / 1.9 µs | **1.2×** | 1960 / 5400 µs | 2450× |
| open+close, p50 / p99 | 1.1 / 1.6 µs | 149 / 289 µs | 135× | 2380 / 45300 µs | 2200× |
| lstat of a missing name, p50 | 0.39 µs | 0.46 µs | **1.2×** | 1480 µs | 3800× |
| 10k files: create / stat / delete | 0.23 / 0.015 / 0.079 s | 22.1 / 2.33 / 9.5 s | 96× / 155× / 120× | 51 / 22 / 30 s | 220× / 1500× / 370× |
| rm -rf the tree | 0.11 s | 10.9 s | 97× | 23.2 s | 205× |

What the numbers say:

- **Reads of metadata the guest has seen are at ext4 speed.** A stat storm, or a lookup of a
  name that does not exist, is answered from the guest's dentry and attribute caches. Those
  stay valid for an hour because the host invalidates them when Windows changes something.
  This is the case for `git status` after the first run, for module resolution, and for
  watchers. Plan9 asks the host every time (about 2 ms each, through vmwp).
- **Anything that must reach NTFS is bound by round trips and NTFS itself.** A create is
  LOOKUP + CREATE + WRITE + RELEASE + GETATTR, five hops of ~50 µs, plus about 220 µs of
  NTFS create time on the host (`share-spike -trace`). That makes about 1 ms per file, against
  20 µs on ext4. No caching hides a create. **REQ-FS-002's 3× is not reachable for
  write-heavy work over any VM boundary we have**, and REQ-FS-001's 2× is not reached either.
  Reads stop at hvsock's ~1.5 GB/s; a single writer without writeback cache keeps one WRITE
  in flight.
- **Native Windows is the floor for creates.** The same 20k-file tree written by a Windows Go
  program (`os.MkdirAll` + `os.WriteFile`) takes **24.1 s** on this machine. So the 36.2 s
  untar over (c) is **1.5× native NTFS**; the 100× against ext4 is mostly NTFS, not the VM
  boundary. Option (d)'s initial copy of that tree into guest ext4 (a tar stream over hvsock)
  took 4.9 s.
- **Warm walks** still issue one READDIRPLUS per directory: the kernel did not reuse its
  readdir cache (`FOPEN_CACHE_DIR`), and why is still open. The host now lists a directory
  on the first READDIR, not at OPENDIR. A host-side listing cache would cut the ~220 µs of
  NTFS time per directory. A cache hit in the guest would make this ext4-like.
- **Writeback cache** doubles large sequential writes (488 MB/s) but adds two SETATTRs per
  small file (create of 2,000 files: 1.95 s → 2.94 s). It stays off by default and can be
  turned on per share.
- Plan9 is 2–20× slower than (c) on every row.

### Change notifications (REQ-FS-003)

20 events of each kind, made by Windows (`os.WriteFile`, create, delete, rename). Latency is
measured from just before the Windows call to the arrival of the guest's inotify event on the
host (this includes one guest→host hop). Two FUSE runs:

| | modify | create | delete | rename | missed |
|---|---|---|---|---|---|
| fuse (c), run 1 | p50 2.4 ms, p95 3.2 ms | 2.5 / 4.0 ms | 1.6 / 3.0 ms | 2.0 / 8.3 ms | **0 / 80** |
| fuse (c), run 2 | 4.3 / 46 ms | 1.9 / 8.2 ms | 1.8 / 3.1 ms | 3.0 / 6.0 ms | **0 / 80** |
| sync (d) | 1.2 / 1.7 ms | 1.4 / 2.0 ms | 0.7 / 1.1 ms | 1.3 / 2.8 ms | 0 / 80 |
| plan9 (a) | — | — | — | — | **80 / 80** |

- With (c), a guest `cat` right after the event reads the new content (coherence check
  passed). REQ-FS-003's 200 ms p95 is met with a wide margin. The Vite/webpack pinned-version
  check is not done.
- In the `docker run -v` demo, `inotifyd` inside an alpine container saw `IN_MODIFY` and
  `IN_CLOSE_WRITE` for a file edited in Windows, `IN_CREATE` for a file created there, and
  `IN_DELETE` for its removal.
- **How events are delivered.** inotify only reports operations that pass through the guest
  VFS, and a FUSE invalidation is not one (`FUSE_NOTIFY_INVAL_*` fire nothing). So the host
  watcher (`ReadDirectoryChangesW`, recursive, one per share) does two things:
  1. It sends invalidations.
  2. It asks the relay's dedicated injector thread to **replay** the change on the mount:
     - open `O_WRONLY` + `ftruncate` to the current size + close, which gives `IN_MODIFY` and
       `IN_CLOSE_WRITE`;
     - `mknod` or `mkdir`, which gives `IN_CREATE`;
     - `unlink`, which gives `IN_DELETE`;
     - `rename`, which gives `IN_MOVED_FROM`/`TO`.

  The host recognizes the injector's requests by their pid field: FUSE puts the calling
  *thread's* id there, and the relay sends that id in its HELLO. It answers them from a
  pending record without touching NTFS. Echoes of the guest's own changes are suppressed by
  marking each changed name *before* the host operation (the watcher can report a change
  before the operation returns; the first version raced and replayed 763 of 2,000 creates).
- A watcher-buffer overflow invalidates everything cached for the share. Without a full
  rescan, events in that window are lost (the same as on Linux with `IN_Q_OVERFLOW`).

### Behaviour (REQ-FS-004)

`compat` writes the names and links in the guest and then lists what Windows sees.

| Check | ext4 | fuse (c) | plan9 (a) |
|---|---|---|---|
| Case | sensitive | **case-insensitive, case-preserving**: `CASE.TXT` opens and overwrites `Case.txt` | same as (c) |
| Symlinks: relative, absolute, dangling, readlink | ok | ok (stored as WSL `LX_SYMLINK` reparse points; no privilege needed) | ok |
| chmod 755/600, chown 1000:1000 | ok | ok (WSL `$LXMOD/$LXUID/$LXGID` EAs; Windows-created files show 0777 root) | ignored (0777, root), no LinuxMetadata flag |
| Non-root container user creates a file | ok | ok, owned by 1000 on the host EAs | — |
| Names with space, Unicode, emoji, `:`, `\`, `?`, trailing `.`, `CON`, `aux.js` | ok | ok (`:\?` and a trailing dot stored as U+F0xx, as drvfs does) | ok, but the trailing dot is lost on Windows |
| Path deeper than 260 characters | ok | ok | ok |
| Hard link, rename over existing | ok | ok | ok |
| mkfifo | ok | ok (WSL `LX_FIFO` tag) | not supported |
| xattrs | ok | not supported (ENOSYS → EOPNOTSUPP) | ok |
| Read an unlinked open file | ok | **fails** (ENOENT): POSIX delete on NTFS still loses the open read | ok |

## Security properties of (c)

Reviewed adversarially (ADR 0007 lists the findings and how each was handled).

- **No host path is ever followed.**
  - Every open is relative to the share-root handle, with `OBJ_DONT_REPARSE` (no reparse
    point anywhere in the path) and `FILE_OPEN_REPARSE_POINT`.
  - When a path crosses a reparse point that is not a link (for example a cloud-files
    directory), the walk goes one component at a time, by handle, and refuses name
    surrogates.
  - Rename and link targets are a single name relative to a verified parent handle.
  - Junctions and NTFS symlinks are shown to the guest as symlinks, and the guest resolves
    them inside the guest. Tests cover a crafted guest that uses a junction's node as a
    directory for lookup, create, mkdir, readdir, rename and link.
- **Names:**
  - one component;
  - valid UTF-8;
  - never `.` or `..`;
  - `/` and NUL refused;
  - `\ : * ? " < > |`, control characters and a trailing dot or space mapped to U+F0xx, so
    they can never act as separators or name streams.
- **Host files are never devices or setuid binaries in the guest.** The mount is
  `nosuid,nodev`, and WSL char/block device files are shown as empty regular files.
- **Bounds:**
  - frames of 1 MiB + 4 KiB;
  - 4096 handles and 1M nodes per server;
  - 4M directory entries across open directory handles;
  - 8 queues and 16 connections;
  - 64Ki queued notifications (dropped beyond that, never blocking);
  - handle close deferred until in-flight I/O ends.
- **No handles held on Windows files beyond the share root and the files the guest has
  open,** so Explorer and editors can rename and delete freely.

## Prototype status

- `internal/share`:
  - FUSE protocol (ABI 7.31 negotiated);
  - server with node table, multi-queue sessions, invalidation and replay;
  - NTFS backend;
  - recursive watcher;
  - Docker create-body rewrite (`RewriteCreate`);
  - on-demand share registry (`Registry`).
- Tests: unit tests on real NTFS (junction escape, single-file and read-only shares, name
  mapping, replay, watcher), review regression tests, and 11 fuzz targets run ≥ 130 s each (four bugs found and fixed, see ADR 0007). The race detector
  could not run: the machine has no cgo toolchain.
- `internal/share/relay`: the guest side (Linux), multi-queue, readahead, with replay through
  `openat2(RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS)`.
- `spike/share -demo`:
  - boots the real hyperbutler image through `engine.Run`, plus a wrapper that starts the relay and
    then execs `/init`;
  - serves a translating Docker API on `\\.\pipe\hyperbutler-share-demo`.
  - Verified: `docker run -v "C:\...\demo src:/src"` (a path with a space) reads and writes;
    `--user 1000:1000` writes; `--mount type=bind,source=C:\...`; `:ro` is enforced; a
    single-file bind shows only that file; inotify fires in the container.

## Reproduce

```powershell
go build -o $env:TEMP\hb-dev\share\share-spike.exe .\spike\share
& $env:TEMP\hb-dev\share\share-spike.exe -targets ext4,fuse,plan9,sync -files 20000 -mb 1024 -notify 20 -compat
& $env:TEMP\hb-dev\share\share-spike.exe -targets ext4 -suite "" -vsockbw
& $env:TEMP\hb-dev\share\share-spike.exe -probe '{"VirtioFs":{"Tag":"x","Path":"C:/Windows/Temp"}}'
& $env:TEMP\hb-dev\share\share-spike.exe -demo     # then docker -H npipe:////./pipe/hyperbutler-share-demo run -v C:\x:/x ...
```

The harness expects the ADR 0005 build in `%TEMP%\hb-dev\wbcli\out` and Alpine's
`linux-virt` module tree in `%TEMP%\hb-dev\hyperbutler-spike\linux-virt`.
