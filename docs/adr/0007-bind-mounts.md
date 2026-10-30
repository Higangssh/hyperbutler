# 0007. Bind mounts from Windows paths

- Status: proposed
- Date: 2026-10-30
- Requirements: REQ-FS-001, REQ-FS-002, REQ-FS-003, REQ-FS-004, REQ-API-001, REQ-PLAT-003
- Evidence: [docs/research/bind-mounts.md](../research/bind-mounts.md)

## Context

`docker run -v C:\src:/src` is the workflow WSL-based Docker makes painful:
- 9P bind mounts are slow (F1);
- file watchers never fire for edits made in Windows (F2).

winbox owns its VM (HCS, [ADR 0002](0002-primary-vm-backend.md)), so it can pick its own
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
3. **A winbox FUSE server over hvsock** (`internal/share`). The guest kernel's FUSE client is
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

- The guest mounts one FUSE file system at `/mnt/winbox` (`nosuid,nodev,default_permissions,
  allow_other`). Its root lists only the registered shares.
- A Windows bind source becomes a share on first use, named `b-<hash of final path>`:
  - a directory is shared as itself;
  - a file is shared through its directory with only that name visible (`Share.Only`);
  - a missing source is created as a directory, as dockerd does.
- **Docker API mapping: path translation in the proxy, shares registered on the fly.** The
  named-pipe proxy rewrites `POST /containers/create` (`HostConfig.Binds` entries whose
  source is `X:\…`, `X:/…` or `\\?\X:\…`, and `Mounts` with `Type: bind`) to
  `/mnt/winbox/<share>[/<file>]` (`share.RewriteCreate` + `share.Registry.Resolve`).
  - Named volumes and guest paths pass through unchanged. `:ro` is kept and enforced by
    dockerd's read-only bind.
  - No VM change is needed per share, so the broker is untouched: the share server, like
    the network stack, is part of the user-process data plane.
- Caching: 1 h entry and attribute TTLs, negative entries cached, `KEEP_CACHE`, 4 MiB
  readahead, no writeback cache by default. Coherence comes from host invalidations, not
  timeouts.
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

1. **Guest image (`guest/prototype`, ADR 0005):**
   - add `kernel/fs/fuse/fuse.ko` to `modules.txt`;
   - have winboxd run `relay.Start` after `hv_sock` is loaded: four AF_VSOCK connections to
     host port **2385**, mount point `/mnt/winbox`, retried while the host is not listening.

   The demo instead uses a wrapper PID 1 that forks the relay and execs `/init`
   (`spike/share/guest/wrap_linux.go`).
2. **Engine (`internal/engine`):**
   - add `vm.Service{ID: transport.VsockServiceID(2385), BindSDDL: <user-only>}` to the
     fixed service list;
   - after `RuntimeID`, listen with `winio.ListenHvsock` and run `share.Server.Serve` for
     each accepted connection (as `gnet.start` does);
   - call `Server.Close` on stop.
   - `spike/share/demo_windows.go` does exactly this through `ExtraServices` and `OnCreated`.
3. **Docker proxy (`internal/transport` or the engine):**
   - the proxy is byte-level today. It needs an HTTP-aware path for
     `POST /v*/containers/create` that calls `share.RewriteCreate(body, registry.Resolve)`;
   - everything else, including hijacked attach streams, passes through as now. The demo
     does this with `httputil.ReverseProxy`.
4. **Broker (ADR 0004):** add the share service id to the fixed template. No new verb is
   needed.
5. **Share lifetime:** shares now live until the engine stops. Before 1.0, reference-count
   them by container through the Docker events stream and call `Server.RemoveShare`.
6. **Requirements:** amend REQ-FS-001/002 as above. Add the readdir-cache and host
   listing-cache work to the backlog (warm walk is 54× ext4 today).

## Adversarial review

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
  - the readdir cache and a host listing cache not bringing warm walks within 5× ext4;
  - Defender real-time scanning (REQ-PLAT-007) multiplying the create costs.
