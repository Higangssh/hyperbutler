# 0007. Bind mounts from Windows paths

- Status: accepted. Integration steps 1, 2, 3, 5 and 6 are done (2026-10-31); `-v C:\…`
  works through `hyperbutler start`. Step 4 (broker) is open. Measured with `bench/fs-*`
  on 2026-10-31 ([below](#performance-pass-and-benchfs--2026-10-31)).
- Date: 2026-10-30, integrated 2026-10-31
- Requirements: REQ-FS-001, REQ-FS-002, REQ-FS-003, REQ-FS-004, REQ-FS-005, REQ-API-001, REQ-PLAT-003
- Evidence: [docs/research/bind-mounts.md](../research/bind-mounts.md)

## Context

`docker run -v C:\src:/src` is the workflow WSL-based Docker makes painful:
- 9P bind mounts are slow (F1);
- file watchers never fire for edits made in Windows (F2).

hyperbutler owns its VM (HCS, [ADR 0002](0002-primary-vm-backend.md)), so it can pick its own
mechanism. Constraints:
- No elevation in daily use.
- [ADR 0004](0004-hcs-access-broker.md):
  - the broker's VM template must not take caller-supplied share definitions or paths;
  - the data plane runs in the user's own process over hvsock.
- The guest is untrusted: nothing it sends may reach host files outside what the user
  shared.
- Windows 11 Home.

## Options

1. **HCS Plan9 shares** (what WSL and LCOW use). Works non-elevated after `HcsGrantVmAccess`.
   - Measured 14–2000× slower than guest ext4.
   - Every `stat` costs ~2 ms through vmwp.
   - No change notifications at all: 80 of 80 events missed.
   - Shares live in the VM document or are added through `HcsModifyComputeSystem`, so the
     broker would have to accept host paths from callers, which ADR 0004 forbids.
2. **virtio-fs.** Not offered by HCS:
   - the schema has no such device, and HCS rejects unknown keys;
   - the only route is an external `FlexibleIov` device emulator, an undocumented COM plugin
     that must be registered (WSL ships its own).
3. **A hyperbutler FUSE server over hvsock** (`internal/share`). The guest kernel's FUSE client is
   relayed unchanged over 4 hvsock connections. The host performs requests on NTFS relative
   to a share-root handle and never follows a link. Windows changes come back as FUSE
   invalidations plus replayed operations, so inotify fires.
   - Cached metadata reads run at ext4 speed: lstat and negative lookups at 1.2×.
   - Events arrive in 2–4 ms p50 with none missed.
   - Creates and writes are 10–100× ext4, limited by round trips and NTFS. Native Windows
     needs 24 s for the same 20k-file tree that (c) untars in 36 s, so this is 1.5× native.
4. **Sync into guest ext4** (mutagen-style). Guest work runs at ext4 speed and Windows edits
   arrive in ~1–2 ms. The costs:
   - a full copy of the tree, held twice on disk;
   - an initial copy;
   - a two-way sync engine with conflict rules. Only the host-to-guest half was built.
5. **SMB or NFS from the guest.** Rejected: both need an admin to configure a host share,
   which is a system setting.

## Decision

**Option 3 is the bind-mount mechanism.** Option 4 is a later, opt-in mode for write-heavy
trees.

- The guest mounts one FUSE file system at `/mnt/hyperbutler` (`nosuid,nodev,default_permissions,
  allow_other`). Its root lists only the registered shares.
- A Windows bind source becomes a share on first use, named `b-<hash of final path>`:
  - a directory is shared as itself;
  - a file is shared through its directory with only that name visible (`Share.Only`);
  - a missing source is created as a directory, as dockerd does.
- **Docker API mapping: path translation in the proxy, shares registered on the fly.** The
  named-pipe proxy rewrites `POST /containers/create` (`HostConfig.Binds` entries whose
  source is `X:\…`, `X:/…` or `\\?\X:\…`, and `Mounts` with `Type: bind`) to
  `/mnt/hyperbutler/<share>[/<file>]` (`share.RewriteCreate` + `share.Registry.Resolve`).
  - Named volumes and guest paths pass through unchanged. `:ro` is kept and enforced by
    dockerd's read-only bind.
  - No VM change is needed per share, so the broker is untouched: the share server, like
    the network stack, is part of the user-process data plane.
- Caching: 1 h entry and attribute TTLs, negative entries cached, `KEEP_CACHE`, 4 MiB
  readahead, no writeback cache. No directory handles (OPENDIR answers ENOSYS), so the
  kernel keeps its readdir cache. Coherence comes from host invalidations, not timeouts.
- POSIX metadata, symlinks and FIFOs use WSL's on-disk formats:
  - `$LXUID/$LXGID/$LXMOD` EAs;
  - `LX_SYMLINK`, `LX_FIFO` and `AF_UNIX` reparse tags.

  So no privilege is needed, and WSL sees the same files.
- What the requirements get:
  - **REQ-FS-003 is met.** p95 is 3–46 ms against a 200 ms target, and inotify works in
    containers.
  - **REQ-FS-004 is met** except for two documented deviations:
    - names are case-insensitive, as on Windows;
    - an open file that was unlinked can no longer be read.
  - **REQ-FS-001 and REQ-FS-002 are not met, and no candidate on HCS meets them for writes.**
    The requirement text should be split:
    - cached metadata reads within 3× (met: 1.2×);
    - creates and writes measured against WSL's 9P baseline, where option 3 is about 3–10×
      better than Plan9;
    - for node_modules-style trees, a named volume or the sync mode of option 4.

### Integration steps (files owned by other tracks)

1. **Guest image (`guest/prototype`, ADR 0005). Done.**
   - add `kernel/fs/fuse/fuse.ko` to `modules.txt`;
   - have hyperbutlerd run `relay.Start` after `hv_sock` is loaded: four AF_VSOCK connections to
     host port **2385**, mount point `/mnt/hyperbutler`, retried while the host is not listening.

   As built: `fuse` is in `modules.txt` and the init loads it with `hv_sock`, before
   hyperbutlerd starts. The engine passes `hyperbutler.share=2385`; only then does the
   init give hyperbutlerd `-share-port 2385 -share-mount /mnt/hyperbutler`, so an image
   started by an older host runs no relay. hyperbutlerd's loop
   (`guest/cmd/hyperbutlerd/share.go`) detaches a dead mount left by an earlier
   hyperbutlerd, dials four connections (non-blocking connect, 3 s bound), mounts, and
   starts over after a failure or a session end, with a backoff from 100 ms to 5 s and
   each distinct failure logged once. SIGTERM (the init's shutdown, after dockerd)
   unmounts and exits. `/mnt/hyperbutler` is an empty directory of the read-only system
   disk. The wrapper PID 1 of the demo (`spike/share/guest/wrap_linux.go`) is not used.
2. **Engine (`internal/engine`). Done** (`share_windows.go`).
   - add `vm.Service{ID: transport.VsockServiceID(2385), BindSDDL: <user-only>}` to the
     fixed service list;
   - after `RuntimeID`, listen with `winipc.ListenHvsock` and run `share.Server.Serve` for
     each accepted connection (as `gnet.start` does);
   - call `Server.Close` on stop.

   As built: `Config.Share` (on by default, `Off` to disable). Stop closes the listener
   and every live guest connection, waits for their goroutines and then calls
   `Server.Close`; it runs after the VM is gone, so the guest keeps its shares through the
   orderly shutdown. A create with a Windows source while no guest session is live is
   refused with a clear error (`Server.Mounted`).
3. **Docker proxy (`internal/transport`). Done** (`rewrite.go`, `Proxy.RewriteCreate`).
   - the proxy is byte-level today. It needs an HTTP-aware path for
     `POST /v*/containers/create` that calls `share.RewriteCreate(body, registry.Resolve)`;
   - everything else, including hijacked attach streams, passes through as now.

   As built, not with `httputil.ReverseProxy` (which re-serializes every request): the
   client-to-guest direction follows request boundaries with net/http's own parser and
   framing rules and copies every other byte verbatim, including keep-alive and
   pipelined requests, chunked bodies (`docker build`) and `Expect`. A request that can
   take the connection over (`Upgrade`, attach, a non-detached exec start, `/session`, `/grpc`), or input
   the filter cannot follow exactly like net/http, switches the rest of the connection to
   plain byte copying; a later create on it then reaches dockerd untranslated and fails
   there, which is safe. The guest-to-client direction is never parsed. A rewritten create
   gets a correct `Content-Length` (a chunked one is de-chunked); bodies over 4 MiB get 413,
   rewrite errors 400, both in dockerd's JSON error format after dockerd has answered
   every earlier request on the connection. `FuzzRequestFilter` checks the filter against
   net/http's view of the same bytes, and `FuzzScanChunked` the chunk scanner against
   net/http's decoder.
4. **Broker (ADR 0004):** add the share service id to the fixed template. No new verb is
   needed. **Open.**
5. **Share lifetime. Done** (2026-10-31, `internal/engine/share_windows.go`,
   `share.Registry.Prune`, `share.SharesInUse`).
   - A share's name includes the identity of its root directory, so a source deleted and
     created again gets a new share instead of the old one, which kept serving the deleted
     directory (regression test `TestRegistryRecreatedDirectoryGetsNewShare`).
   - The engine follows dockerd's container `destroy` events. After a burst of them (2 s
     debounce), whenever the stream (re)opens, and every 10 minutes, it lists every
     container (`GET /containers/json?all=1`). It then removes the shares that no
     container's bind mount comes from, in any container state.
   - A share that a create resolved less than a minute before the listing started is kept,
     so a container dockerd has not recorded yet never loses its share. `Prune` and
     `Resolve` take the same lock.
   - At most 1024 live shares (`Registry.MaxShares`); past that a new source is refused
     with an error that says why.
   - The list is untrusted guest data: it is bounded to 32 MiB, only bind sources below the
     mount point with a registry-shaped name count, and it is fuzzed (`FuzzSharesInUse`). A
     lying guest can only keep the user's own shares alive or remove them under its own
     containers.
   - Ended event streams back off to a minute.
   - End to end: five containers on a source deleted and recreated between runs, then one
     long-running container. After the grace period the four stale shares were removed and
     the running container kept reading its mount.
6. **Requirements. Done.** REQ-FS-001 and REQ-FS-002 are amended as above and REQ-FS-005
   is added; the readdir-cache and host listing-cache work is in the backlog list in
   [requirements.md](../requirements.md#file-sharing) (warm walk is 54× ext4 today).

### End to end through `hyperbutler start` (2026-10-31)

Reference machine of the README, non-elevated, 4 GiB / 8 vCPU guest, Docker CLI 29 on
Windows, the image built by `guest/prototype/build.ps1`. The relay mounted the shares
before hyperbutlerd reported ready (0.38 s into the guest boot); `docker info` answered
1.3 s after `hyperbutler start`. All 14 checks passed, twice in a row (the second run
deletes and recreates the source directory):

| Check | Result |
|---|---|
| `-v C:\…\w:/data`, read a file written on Windows | ok, `docker run` 365–760 ms |
| write in the container, read on Windows | ok |
| `-v …:/data:ro` and `--mount …,readonly` | writes fail with `Read-only file system`, nothing created |
| `--mount type=bind,src=C:\…,dst=/m` | ok |
| single-file bind `-v C:\…\hello.txt:/etc/hello.txt:ro` | ok |
| inotify (`inotifyd`) on Windows-side creates | 5 of 5, 52–71 ms including `docker logs` polling |
| inotify on a Windows-side append to an existing file | `IN_MODIFY` and `IN_CLOSE_WRITE`, 60–74 ms incl. polling |
| named volume and guest-path bind in the same create | unchanged |
| `hello-world`; `-p 18080:80` nginx from Windows | ok; HTTP 200 |
| `docker run -i` with 2000 piped lines, and `echo ping \| docker run -i … cat` | `2000`, `ping`: hijacked streams and half-close intact |

The event latencies above are a smoke test, not the REQ-FS-003 measurement (`bench/fs-notify`
with pinned Vite and webpack, no polling), which has not run through `hyperbutler start`.
After the integration review two more checks were added and pass (16 of 16): `-v Q:\nope:/x`
and `--mount` of a missing source come back as `Error response from daemon: hyperbutler: …`
through the named pipe.

Numbers from one container through the bind mount, against the same script on the
container's own ext4 (10 ms resolution): 1000 creates 850 ms (20 ms), reading them back
340 ms (10 ms), `ls -l` 150 ms (0), 256 MiB write with fsync 4.7 s (90 ms), the same
without fsync 56 MB/s, re-read 603 MB/s; a fresh container's `ls -lR` of the 1000 files
10 ms. The sequential write rate is far below what hvsock carries and is in the backlog.

## Performance pass and `bench/fs-*` (2026-10-31)

Same machine and guest as above (4 GiB / 8 vCPU, guest kernel 6.18.58, Defender in passive
mode), the image built by `guest/prototype/build.ps1` from this branch. Each run measured
`docker run -v` against the same work on a named volume (guest ext4).

**Why sequential writes had dropped to 56 MB/s (the spike reached 282).** The host spent
under 1 ms on each WRITE (instrumented `Server.Serve`). An ftrace of the guest
(`fuse:fuse_request_send/end`, `raw_syscalls`, `sched`) showed the cause:
- every WRITE of 64 KiB or more took about 15 ms from request to reply;
- the reply sat in the hvsock ring until the Go runtime's background poll (`sysmon`,
  10 ms) noticed it;
- the relay read `/dev/fuse` with a blocking read(2) but drove the hvsock connections
  through the Go poller. The thread that waited in epoll ran the goroutine it woke (the
  writer of a frame too big for the ring in one write), that goroutine then blocked in
  read(2) of `/dev/fuse`, and nothing waited in epoll any more;
- the spike's relay used blocking sockets, so it never hit this. Locking the readers to
  their threads did not help, and making `/dev/fuse` poller-driven did, at the cost of
  waking every queue for each request (4 KiB writes 30 % slower).

The fix makes every relay wait a blocking system call: the share connections are blocking
after a bounded non-blocking connect, and their Close shuts the socket down first. `Close`
aborts the FUSE connection through fusectl (found via `/proc/self/mountinfo`, not a stat of
the mount), so blocked readers and guest processes waiting for a reply return. It closes
`/dev/fuse` only after the relay's goroutines have ended.

**Walks.** The guest kernel did reuse its readdir cache; a warm walk still paid OPENDIR and
RELEASEDIR, two round trips per directory. OPENDIR now answers ENOSYS. The kernel then
stops sending both and opens every directory with `FOPEN_KEEP_CACHE | FOPEN_CACHE_DIR`.
READDIR comes with fh 0; a READDIR at offset 0 reads the directory from the host and keeps
the listing on the node for the READDIRs that continue it. A host listing cache across
listings would only help when the guest dropped its cache, and would add a second cache to
keep coherent, so it was not built.

**Creates.** A create is LOOKUP, a GETATTR of the parent (the kernel drops the directory's
attributes after every change, and `default_permissions` needs them), CREATE, WRITE and an
asynchronous RELEASE. Host time per create is about 0.45 ms, of which NTFS
`NtCreateFile` with the WSL EAs is 0.14–0.16 ms when called directly. Two things were found
and fixed on the way:
- each create also invalidated its directory in the guest: NTFS reports the directory as
  modified, and that echo was passed on. A directory modified event within the echo window
  of a guest change inside it is now dropped;
- `bench/fs-notify` found that Windows edits of files the guest had only listed (plain
  READDIR past the first chunk creates no nodes) were not replayed. 88 of 100 edits got
  no IN_MODIFY. They are replayed now.

Inlining LOOKUP and GETATTR on the connection's reader (no worker handoff) changed nothing
measurable, so it was not kept.

**Writeback cache stays off and is not offered as an option.** With `FUSE_WRITEBACK_CACHE`
the kernel trusts its own size and times for cached regular files and ignores the host's
(`fuse_get_cache_mask`), so a Windows-side append is not seen while the inode is cached,
and dirty pages race Windows edits. That breaks the coherence this design is chosen for.
`Options.Writeback` remains a measurement knob only.

| `docker run -v`, one container | Before | After | Guest ext4 |
|---|---|---|---|
| `dd bs=1M` write, 64 MiB | 54–56 MB/s | 304 MB/s | 5.3 GB/s |
| `dd bs=64k` write | 4.1 MB/s | 212 MB/s | 5.0 GB/s |
| `dd bs=4k` write | 31 MB/s | 34 MB/s | 2.7 GB/s |
| 256 MiB, 1 MiB writes + fsync (`fs-seq`) | 4.9 s (52 MB/s) | 0.91–1.0 s (256–280 MiB/s) | 0.11–0.14 s |
| Read, guest cache dropped | 590–620 MB/s | 450–866 MiB/s | 4.4 GiB/s |
| Read, guest cache warm | 10.3 GiB/s | 10.1 GiB/s | 11.3 GiB/s |
| 1000 creates (`os.WriteFile`) | 1.10–1.25 s | 0.88–0.96 s | 11 ms |
| 1000 deletes | 0.45–0.58 s | 0.42–0.47 s | 3.4 ms |
| Warm walk, 2100 entries in 101 directories | 15–17 ms | 2.4 ms | 2.1 ms |
| Windows write to IN_MODIFY in the container | 52–71 ms incl. `docker logs` polling | p50 2.3 ms, p95 4.8 ms | – |

`bench/fs-*`, as the requirements specify:

| Requirement | Measurement | Result | Verdict |
|---|---|---|---|
| REQ-FS-001 | read with the guest cache warm, 256 MiB (`fs-seq`) | 1.12× ext4 | **met** |
| REQ-FS-001 | write + fsync against WSL 2's 9P mount | 259 MiB/s; no WSL on this machine | not measured. HCS Plan9, the same 9P server, gave 191 MB/s in the spike: 1.4×, which would **not** meet 3× |
| REQ-FS-002 | `fs-meta` on axios v1.7.9 (340 paths): lstat of every path / of a missing name next to each | 2.1× / 0.9× ext4 | **met** |
| REQ-FS-002 | `git status`, median of 5 after a warm-up | 3.1× (5.3 vs 1.7 ms); 6.1× in an earlier run | **not met** |
| REQ-FS-003 | `fs-notify`: IN_MODIFY of 100 Windows writes | p50 2.3 ms, p95 4.8 ms, none missed | **met** |
| REQ-FS-003 | Vite 5.4.11 dev server and webpack 5.97.1 `--watch`, default config, a Windows edit | HMR message after 24 ms; recompiled after 44 ms | **met** |
| REQ-FS-005 | 10k creates / stats / deletes | 11.6 s / 1.6 s / 5.2 s (ext4: 0.12 / 0.008 / 0.037 s) | not measured against 9P. Against the spike's HCS Plan9 numbers (51 / 22 / 30 s) these are 4.4× / 14× / 5.7× |
| REQ-FS-005 | `npm ci` in the checkout (2115 packages, warm npm cache) | 65 s (ext4: 8.8 s) | not measured against 9P |

What `git status` still pays for is about 17 OPEN/RELEASE pairs (index, HEAD, refs, config),
the `index.lock` create and unlink, and re-reading `.git` after them. Every opened file
costs an OPEN round trip even when its pages are cached. The next step is
`FUSE_NO_OPEN_SUPPORT`, with a host-side handle cache that closes idle handles. Its risks
are Windows sharing modes held past the guest's close, and O_TRUNC, which must then come
as SETATTR. 4 KiB writes and creates stay bound by round trips (about 0.1 ms each) and
NTFS: named volumes stay the advice for `node_modules`. (Done in the next section:
`git status` is 1.7× ext4.)

Reproduce (the docker CLI and endpoint are explicit; the user's context is never touched):

```powershell
go run ./bench/fs-seq    -docker C:\path\docker.exe -host npipe:////./pipe/docker_engine_hyperbutler
go run ./bench/fs-meta   -docker C:\path\docker.exe -host ...      # git, network for the checkout and npm
go run ./bench/fs-notify -docker C:\path\docker.exe -host ... -frameworks
# add -wsl <distro> to measure the 9P baselines; without it those rows are "not measured"
# and the command exits 2 (incomplete)
```

## Opens without OPEN (`FUSE_NO_OPEN_SUPPORT`) and the size-0 report (2026-10-31)

Same machine, guest and image as above (the image built from this branch is byte-identical
to main's: the change is host-side only, `internal/share/openfile.go`).

**Profile.** A per-opcode trace of the share server (a temporary build, not committed)
counted, per warm `git status` of the axios checkout: 16 OPEN, 17 RELEASE, 1 CREATE, 1
UNLINK (`index.lock`), 2 GETATTR (the directory after each change, for
`default_permissions`), 1 LOOKUP and 1 FORGET. After the change: CREATE, UNLINK, 2 GETATTR,
LOOKUP and FORGET. The LOOKUP and GETATTRs are the kernel's own revalidation after a
change and cannot be answered from the server side.

**Design.**
- OPEN of a file and FLUSH answer ENOSYS. The kernel then sends no OPEN, RELEASE or
  FLUSH for the connection, opens files with `FOPEN_KEEP_CACHE`, and sends READ, WRITE and
  FSYNC with fh 0. `FUSE_ATOMIC_O_TRUNC` is no longer negotiated, so O_TRUNC arrives as a
  path SETATTR inside open(2). O_APPEND is unchanged: the kernel already chose the offset.
  `RealOpen`, `NoCache` and `Writeback` keep the old behaviour.
- READ and WRITE with fh 0 are served by node from a host handle cache. Handles share read,
  write and delete. A read opens read-only. Idle handles close after 100 ms
  (`Options.FileIdle`); at most 256 are kept, least recently used first. A Windows removal,
  a replacement or a lost-event overflow closes them at once. Closes happen after the server
  lock is released: closing the last handle of a deleted file is when NTFS deletes it.
- CREATE answers fh 0 as well and caches its handle. With a real fh, this guest kernel
  (6.18.58) sent no RELEASE for CREATE handles once OPEN had answered ENOSYS:
  `fuse_request_send` in ftrace showed none, and 4096 creates filled the handle table
  (EMFILE). v6.18's `fuse_create_open` allocates release arguments, so the cause is not
  understood; with fh 0 a RELEASE, if one comes, is a no-op (tested).
- POSIX "unlinked while open". A guest unlink or rename-over pins the file's cached handle
  until the kernel forgets the inode. The kernel forgets it once no process has the file
  open. A read-only handle is reopened read-write first, for O_RDWR users and mmap
  writeback. A file without a cached handle is not opened for this: the first open with
  read access of a file just written costs about 0.5 ms here (real-time scanning), and doing
  it on every delete made 10k deletes 2.6× slower. Such a file serves its cached pages,
  and gets ESTALE for anything else.

**What Windows programs can still do** (`openfile_windows_test.go`):
- edit, rename and delete a file whose handle is cached, at once;
- POSIX deletes (`DeleteFileW` on this Windows 11) free the name at once, and a classic
  delete completes within FileIdle;
- an exclusive open, or an atomic save (temp file, then `MoveFileEx` over the file), fails
  while a handle is open, whatever its share flags. With OPEN this window was "while a guest
  process has the file open". Now it also lasts for FileIdle after the guest's last READ or
  WRITE that reached the host: about 100 ms in the test.

FileIdle is short because a cache hit saves little. Page-cache hits never reach the server,
and the READs or WRITEs of a stream arrive back to back.

**The size-0 report.** It did not reproduce on main. 530 checks found no file of size 0 or
missing:
- Windows creates and writes new files (at once, and with 2 ms or 20 ms between create and
  write);
- guest side: `docker run ... ls -l; cat` in fresh containers, a container already
  listing or stat-ing the directory in a loop, and `docker exec`.

The same script run on this branch made 360 more checks, also clean. Two windows were
found in the code, though. Both are fixed, with regression tests:
- a LOOKUP (or READDIRPLUS, or replayed create) reads the host and then makes the node. A
  change reported in between finds no node to invalidate, and the guest cached the old
  attributes (or a negative entry) for an hour. Each host event now bumps a per-name
  sequence. A reply whose host read is older than a change of that name carries no
  attribute or entry timeout, and READDIRPLUS gives that name no node;
- the kernel applies a LOOKUP reply after waking the caller. An invalidation of a node made
  in the last 2 s can overtake it, so it is sent again after 50 ms and 1 s (batched, at most
  4096 waiting).

| `docker run -v`, median of 3 runs (interleaved) | Before | After | Guest ext4 |
|---|---|---|---|
| `git status`, median of 5 per run | 5.60 ms (3.92×) | **2.39 ms (1.68×)** | 1.42–1.48 ms |
| lstat of every path, total | 1.81 ms (1.67×) | 1.81 ms (2.20×) | 0.82–1.09 ms |
| lstat of a missing name, total | 1.14 ms (1.10×) | 1.04 ms (1.22×) | 0.85–1.03 ms |
| warm walk, median | 1.14 ms | 1.09 ms | 0.52–0.53 ms |
| 10k creates / stats / deletes | 11.1 / 1.37 / 5.5 s | 11.5 / 1.49 / 5.2 s | 0.11 / 0.007 / 0.037 s |
| 256 MiB write + fsync (9 rounds) | 271 MiB/s | 278 MiB/s | 2188 MiB/s |
| read, guest cache dropped / warm | 427 / 10200 MiB/s | 457 / 10100 MiB/s | 4540 / 11400 MiB/s |
| Windows write → IN_MODIFY (100 edits) | p50 2.9, p95 3.8 ms, 0 missed | p50 2.4, p95 3.4 ms, 0 missed | – |
| `dd bs=4k` / `bs=64k` write, 32 / 64 MiB, median of 3 | 32.9 / 201 MB/s | 33.0 / 217 MB/s | – |

The lstat ratios moved because the ext4 side varied (0.82 ms against 1.09 ms); the bind-mount
times are the same. Creates did not get faster. Each create is still a LOOKUP, a GETATTR of
the directory, a CREATE and a WRITE, and the deletes an UNLINK and a GETATTR, all round
trips the kernel waits for. Batching them would need the kernel to stop revalidating the
directory after each change; with `default_permissions` it must. Named volumes stay the
advice for write-heavy trees.

## Adversarial review

### Opens without OPEN (2026-10-31)

The adversarial review of the change raised these findings.

| # | Sev | Finding | Resolution |
|---|---|---|---|
| N1 | High | A cached handle, read or write, makes a Windows atomic save fail with "access denied": a rename over the file, as editors, git and npm do. At 1 s idle this lasted a second after any guest I/O. | **Fixed:** FileIdle 100 ms; `TestNoOpenWindowsAtomicSave` (the save succeeds after about 100 ms). **Accepted:** the window itself remains, as it did while a guest process had the file open. |
| N2 | Med | A pinned read-only handle could not serve a write after the unlink, nor mmap writeback (ESTALE, silently lost dirty pages). | **Fixed:** the handle is reopened read-write before the unlink or replace. Test: `TestNoOpenWriteAfterUnlinkOfReadFile`. **Accepted:** with no cached handle at all there is still no pin (cost, above). |
| N3 | Med | The claim that the kernel sends no RELEASE for CREATE handles contradicts the v6.18 source. | **Recorded as observed, cause unknown** (ftrace, EMFILE after 4096 creates). fh 0 makes either behaviour safe; `TestNoOpenReleaseOfFh0`. |
| N4 | Low-Med | Repeated invalidations of fresh nodes made two timers per event; a Windows checkout under a freshly walked tree could make thousands and flood the notification queue. | **Fixed:** one batch and one timer per delay, at most 4096 waiting. Test: `TestFreshNodeReinvalidationBounded`. |
| N5 | Low | FSYNC with fh 0 hid every error and replaced a cached read handle with a write handle. | **Fixed:** it flushes a cached write handle, or opens one only for the flush (write-only opens are cheap); EIO is returned. Test: `TestNoOpenFsyncKeepsReadHandle`. |
| N6 | Low | The replaying thread's READ, WRITE and FSYNC with fh 0 reached host files, and its records lingered (no RELEASE). | **Fixed:** they touch no host file, and its truncate ends the record. Test: `TestNoOpenInjectorTouchesNoHostFile`. |
| N7 | Low | Windows writers that share reads only fail while a write handle is cached. | **Accepted**, bounded by FileIdle like N1. |

Found sound:
- the lock and close discipline: every unlock goes through `unlock()`, which closes
  dropped files after releasing the lock;
- reference counting: no use after close and no double close;
- the sweep timer: generation-guarded, stops when empty;
- the bounds against a hostile guest;
- the kernel behaviour relied on, checked against v6.18: `FOPEN_KEEP_CACHE` for no-open
  files, `write_inode_now` before `no_flush`, O_TRUNC as SETATTR without atomic_o_trunc;
- no write can land in a replaced file;
- the race-sequence logic;
- read-only shares.

Fuzzing: `FuzzNoOpen`, new, drives guest requests and host events against the handle
cache. It checks for use after close, double close, the cache bounds and leaks. It found
that turning a cached handle into a pin ignored the pin bound; the corpus entry is
committed. `FuzzDispatch` got seeds with fh 0 and a no-leak check after Close. Both ran
2 min 10 s after the last change, clean. The race detector still needs CI (no cgo here).

### Performance pass and share lifetime (2026-10-31)

| # | Sev | Finding | Resolution |
|---|---|---|---|
| P1 | High | `FuzzSharesInUse`'s oracle failed: `json.Unmarshal` turns invalid UTF-8 into U+FFFD, which `checkName` accepted. | **Fixed.** Only registry-shaped names (`b-` and 16 hex digits) count. The corpus entry is committed. |
| P2 | High | `fs-meta` removed `node_modules` in the current directory of `wsl.exe`, not in the checkout. | **Fixed:** the cleanup runs on the checkout's WSL path. |
| P3 | Med | The relay learned its FUSE connection with a stat of the mount, a FUSE request. It hung `Start` if the host went away first, and a failed stat left `Close` unable to abort, leaking readers on every remount. | **Fixed:** the device comes from `/proc/self/mountinfo` before the readers start (`FuzzMountDev`), and the abort file is opened `O_WRONLY`. |
| P4 | Med | Bench verdicts passed vacuously: unmeasured requirement rows exited 0, and `fs-notify` without `-frameworks` said nothing about Vite/webpack. | **Fixed:** exit 2 ("incomplete") when any requirement row is not measured; a "not checked" row for frameworks. |
| P5 | Med-Low | `fs-notify` timed a late IN_MODIFY of one edit as the next edit's. | **Fixed:** one file per edit. This exposed the unlooked-up-file replay gap (fixed, `TestModifyOfUnlookedFileReplayed`). |
| P6 | Low | REQ-FS-002 lstat rows compared p50 latency, not the same work. | **Fixed:** totals over every path and round; p50/p99 stay in `-json`. |
| P7 | Low | Two processes listing one directory share the node's snapshot; one's offset 0 replaces the other's. | **Accepted.** A continuation then reads the new listing. Names can only repeat or be missed if the directory changed meanwhile, which POSIX leaves unspecified. Both listers also share the kernel's readdir cache, which already serializes most cases. |
| P8 | Low | A failing event stream kicked a full container list every second. `Prune` holds the registry lock across `RemoveShare`. A guest that answers `[]` removes every share. | **Fixed:** backoff to a minute. **Accepted:** the lock is held only for bounded watcher closes. A lying guest can only remove shares under its own containers. |
| P9 | Info | The cached-read row of REQ-FS-001 measures the guest page cache on both sides. | **Accepted:** that is what the amended requirement asks; the cold read is reported next to it. |

Found sound: the echo drop for a directory's own modified event (changes inside it, and its
own removal or rename, still pass); `Share.Only` with handle-less listings; the listing
budget accounting; root invalidation after prunes; engine stop ordering; `blockingConn`.

### Integration (2026-10-31)

The adversarial review of the integration (steps 1, 2, 3, 6) raised these findings.

| # | Sev | Finding | Resolution |
|---|---|---|---|
| I1 | Med | A detached exec start (`{"Detach":true}`, a plain POST the Docker client sends on a pooled connection) matched the hijack paths, so the connection went raw and a later create on it was not rewritten. | **Fixed.** An exec start without `Upgrade` and with a small body is followed when it is detached. Test: `TestFilterFollowsDetachedExecStart`; the fuzz oracle models it. |
| I2 | Low | `Expect: 100-continue` on a create is not answered: the filter waits for the body before dockerd sees the head. | **Accepted.** The Docker CLI and SDKs never send it; curl sends the body after its 1 s timeout. Answering it would need a write serialized with the guest-to-client copy. |
| I3 | Low | Stop can wait up to 5 s: `Server.Serve` polls for queue 0 for 5 s on a queue-N connection and closing the connection does not end the wait. | **Accepted.** Bounded, needs a guest that sends only queue ≥ 1 HELLOs. |
| I4 | Low | One failed hvsock `Accept` ended the share listener for the rest of the run. | **Fixed.** Non-close errors are retried with a backoff (5 ms to 1 s). Test: `TestShareServiceSurvivesAcceptError`. |
| I5 | Low | The refusal path was tested over TCP only, not the named pipe. | **Fixed end to end:** two refusal checks through `hyperbutler start` (above). |
| I6 | Low | The README claimed REQ-FS-003 from a smoke test. | **Fixed:** now "not measured as specified". |
| I7 | Info | `--mount type=bind` created a missing source; dockerd refuses it. | **Fixed.** `share.RewriteCreateMounts` resolves `Mounts` with `Registry.ResolveExisting`. Test: `TestRewriteMountMissingSource`. |

Found sound: the framing against net/http (the filter is never more lenient, and where
it is stricter it goes raw after forwarding exactly the bytes it consumed), bounded
memory per connection, the refusal ordering in `forward`, the share service lifecycle,
the guest relay loop, and the root identity in share names (no security downside; the
cost is one share per delete-and-recreate until step 5).

### Share server (2026-10-30)

The adversarial review of the code (2026-10-30) raised these findings. Each is fixed with a
regression test, or answered.

| # | Sev | Finding | Resolution |
|---|---|---|---|
| 1 | High | `Share.Only` bypass: host watcher events for siblings of a single-file share created replay records and nodes, so a container could read a sibling file, and sibling names leaked in frames. | **Fixed.** `HostChanged` drops events for hidden names, and a rename to a hidden name is treated as a removal. `injectCreate` re-checks. Test: `TestOnlyShareIgnoresSiblingEvents`. |
| 2 | High | RELEASE racing READ/WRITE on one fh could close the Windows handle mid-I/O; a recycled handle value could then receive guest data. | **Fixed.** Handles are reference-counted, and the file closes when the last in-flight request ends. SETATTR no longer borrows the fh at all. Test: `TestReleaseWaitsForInflightIO`. |
| 3 | Med | A stale or second queue-0 connection reset the live session's node table; the connection count was unbounded. | **Fixed.** Only the current session's end resets the table; a new queue 0 deliberately takes over (guest reboot). At most 16 connections. Test: `TestStaleSessionDoesNotResetNew`. |
| 4 | Med | Blocking notification writes could hang the watcher, `RemoveShare` and shutdown if the guest stopped reading. | **Fixed.** A per-session queue of 64Ki notifications feeds a writer goroutine; overflow is dropped, never blocking. Test: `TestNotifyNeverBlocks`. |
| 5 | Med | Directory snapshots were unbounded across 4096 handles. | **Fixed.** Listings are loaded on the first READDIR, with a 4M-entry budget across all directory handles (ENOMEM beyond it). |
| 6 | Med | Relay replay resolved paths through possibly guest-planted symlinks, so guest root could act outside the mount. | **Fixed.** Replays use `openat2(RESOLVE_BENEATH\|RESOLVE_NO_SYMLINKS\|RESOLVE_NO_XDEV)` on the mount and then `*at` calls. |
| 7 | Low | The rename fallback removes an empty target directory before retrying, so a failed retry leaves it removed. | **Accepted.** POSIX rename over an empty directory removes it anyway; the failure window is only an NTFS error on the second call. |
| 8 | Low | Registry shares are never removed (a root handle and a watcher per distinct path). | **Accepted for now.** This is integration step 5. |
| 9 | Low | Cloud placeholder and dedup files are opened as reparse points, bypassing hydration, so they may read empty. | **Open.** The fix is to reopen non-surrogate leaves without `FILE_OPEN_REPARSE_POINT` and verify the final path is inside the root. A REQ-FS-004 test is needed on a OneDrive folder. |
| 10 | Low | A host-created FIFO was replayed as a regular file. | **Fixed:** the relay replays the real type. |
| 11 | — | Fuzz gaps: relay `parseInject`, `statLx`, `insideRoot`. | **Fixed:** `FuzzParseInject` (moved to portable code) and `FuzzStatAndRoot` were added. |

Fuzzing found four more bugs. Each is fixed, and its corpus entry is committed under
`internal/share/testdata/fuzz/`:
- `FuzzDispatch`: READ with an offset ≥ 2^63 reached `ReadAt` as a negative `int64`.
- `FuzzSplitBind`: a doubled `\\?\` prefix was accepted as a bind source.
- `FuzzReparse`: an NTFS link target containing NUL was passed to the guest, which would
  silently truncate it.
- `FuzzStatAndRoot`: a `/` inside a link component passed the inside-the-share check.

The reviewer found these sound:
- the junction, symlink, `..`, `\`, `:` and trailing-dot handling, through
  `OBJ_DONT_REPARSE` and handle-relative walks;
- 8.3 and case aliases, which cannot widen a share (they only alias names already inside it);
- the FUSE ABI layouts and lookup-count accounting.

Arguments against the decision itself, and the answers:
- *"A user-space server in the user's process is slower than vmwp's kernel-assisted Plan9."*
  Measured otherwise: option 3 is faster on every row and is the only option with events.
- *"Replaying events by performing operations on the mount is a hack."* It is the only way
  to make a stock kernel's inotify fire for remote changes. It is confined to one thread,
  answered from host-side records, and never changes host data. A guest that lies about
  the thread id can only make the host skip work and report current state.
- *"Case-insensitivity breaks Linux tools."* It is the same on WSL and Docker Desktop.
  Per-directory case sensitivity cannot be turned on without WSL's optional component. A
  strict-case mode (refusing a lookup whose case differs) is a possible later option.

## Consequences

- **What becomes possible:**
  - Windows-side editing with live reload in containers, without polling;
  - stat-heavy tooling (git status, module resolution) at near-ext4 speed once warm;
  - no admin step, no broker change;
  - WSL-compatible metadata.
- **New attack surface:**
  - a FUSE decoder fed by the guest, in the user's process. It is fuzzed (11 targets, each
    ≥ 2 min) and bounded (frames, nodes, handles, listings, connections, queued
    notifications);
  - the race detector has not run yet (no cgo toolchain on the dev machine). CI must run
    `go test -race ./internal/share/...`.
- **Performance gaps that stay:**
  - creates, deletes and writes are 10–100× guest ext4;
  - sequential I/O peaks near hvsock's ~1.5–2 GB/s with 4–8 connections.

  Users with write-heavy trees should be pointed at named volumes, or at the future sync
  mode.
- **What would make us revisit:**
  - HCS gaining a documented virtio-fs device, or a shared-memory transport;
  - warm walks are within 1.2–1.7× ext4 since the readdir cache is reused (2026-10-31);
    a regression past 5× would reopen this;
  - Defender real-time scanning (REQ-PLAT-007) multiplying the create costs.
