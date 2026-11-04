# Blog and talk ideas

Engineering stories from building hyperbutler that are worth writing up for launch and
later. Each entry has the hook, the numbers, and where the evidence lives, so a post can be
written from the repo alone. Add to this list whenever a measurement turns up something
surprising. Numbers are from the maintainer's Windows 11 Home laptop unless noted.

## Strong candidates

### 1. The same hvsock, 20x faster by changing who dials
- **Hook:** Windows sends slowly on a Hyper-V socket it *accepted*: about 1 ms per ~600 KB,
  against about 50 µs on one it *dialed*. Not documented anywhere we found.
- **Story:** the network link carried all container traffic over one hvsock that the guest
  dialed. Swapping the roles (the guest listens, the host dials) plus segmentation offload
  took container downloads from 2.7 to 10.2 Gbit/s.
- **Evidence:** [ADR 0006](../adr/0006-guest-networking.md),
  [guest-networking.md](../research/guest-networking.md) "Throughput work (2026-10-31)",
  `bench/net-perf`.
- **Audience:** anyone building WSL-like tools, VM agents, Kata/containers on Hyper-V.

### 2. A user-mode network stack at 10 Gbit/s: the stack was waiting, not computing
- **Hook:** profiling showed the engine and the guest mostly idle during bulk transfers.
- **Story:** four waits, each measured on its own: 1460-byte segments (fixed with
  virtio_net_hdr TSO, 64 KiB segments), the accepted-hvsock send path (#1), autotuned
  loopback socket buffers (67% of the time blocked), and 256 KiB gVisor windows against
  1–5 ms RTT under load. Plus a 60 s TIME_WAIT that made connection-rate tests fail outright,
  and an 11.6 ms p99.9 that turned out to be the benchmark reconnecting.
- **Ruled out by measurement:** GC, per-frame syscalls, buffer size, RACK, split links.
- **Evidence:** same as #1; before/after table in the ADR.

### 3. Why your VM doesn't give memory back: 2 MiB holes
- **Hook:** hv_balloon free page reporting only returns free blocks of order 9 (2 MiB). After
  a workload about 70 MiB sat free in smaller blocks, counted free by Linux and kept by the
  host, and it grew with every run.
- **Story:** `page_reporting.page_reporting_order=0` brought vmmem back to idle after every
  fill (+84 MiB → about 0). Also: capping the dentry/inode hash tables saves 18 MiB on a
  12 GiB guest.
- **Evidence:** [mem-reclaim.md](../research/mem-reclaim.md), `bench/mem-reclaim`,
  `/proc/buddyinfo` captures.
- **Audience:** WSL users wondering about vmmem, Hyper-V and KVM people.

### 4. Antivirus made our Go process 3x bigger (and our benchmark caused it)
- **Hook:** the engine's working set jumped from 19 to 65 MiB, its whole commit, in 12 of 13
  runs, always 300 s or 600 s after start.
- **Story:** an outside process read the engine's entire address space (fits AV behaviour
  monitoring; not proven), which makes every committed page resident, including Go 1.27's
  32 MiB FIPS DRBG buffer and debug sections never read. The trigger was our own bench
  starting a hidden PowerShell per sample. Fixes: sample in-process with
  NtQuerySystemInformation, and a working-set guard that trims after an outside scan.
  Reproduced with a same-user ReadProcessMemory pass (8.8 → 53.8 MiB).
- **Side story:** the frozen FIPS module has no 32 MiB buffer, and why we still didn't
  use it (it gets no fixes, and an HTTPS update check is coming).
- **Evidence:** [mem-reclaim.md](../research/mem-reclaim.md) second half,
  [ADR 0003](../adr/0003-implementation-language.md), `bench/internal/hostmon`.

### 5. Docker on Windows Home without WSL, in 1.1 seconds
- **Hook:** HCS (the API under WSL2 and Windows Sandbox) can boot a Linux kernel directly,
  and it's available on Home.
- **Story:** LinuxKernelDirect boot, a 0.9 MB stage-1 initramfs that verifies a read-only
  erofs system disk, dockerd from signed Alpine packages, three hvsock links, no vEthernet
  adapter, no WinNAT, no admin after setup.
- **Evidence:** ADRs [0001](../adr/0001-windows-home-first-class.md),
  [0002](../adr/0002-primary-vm-backend.md), [0005](../adr/0005-guest-image.md), `bench/start`.

### 6. Bind mounts over FUSE and hvsock: from 56 to 304 MB/s
- **Story:** FUSE over hvsock with a host handle cache; `git status` went from 3.9x to 1.7x
  the in-VM ext4 time, file-change notifications at 2.3 ms p50 with none missed.
- **Evidence:** [ADR 0007](../adr/0007-bind-mounts.md), `bench/fs-*`.

### 7. What people hate about Docker Desktop, from 2,000+ issue reactions
- **Story:** updates that break setups (13 of 27 top issues since 2023), start hangs, seven
  places for settings, sign-in pressure, telemetry, factory reset as the fix. And what the
  GUI is actually used for. How that shaped hyperbutler: no account, no telemetry, an
  on-demand GUI that leaves no process behind, an MCP server instead of a built-in AI.
- **Evidence:** [wsl2-docker-pain-points.md](../research/wsl2-docker-pain-points.md) §8,
  [settings-and-gui.md](../research/settings-and-gui.md), [ADR 0008](../adr/0008-settings-gui-mcp.md).

### 8. "Corrupted layer after rmi": we blamed fstrim, it was a buffer swap
- **Hook:** 1 pull in 10 failed with `unexpected commit digest`, right after `docker rmi`
  and a 2 GiB `fstrim`, and never with a 15 s pause. Everything pointed at the disk.
- **Story:** containerd hashes a fresh ingest as it streams in, so a disk cannot change
  that digest: the bytes were wrong before they were written. The host's link queue
  kept a "spare" buffer for the next batch; a batch that grew too big was not handed
  back, so the spare stayed aliased to the live queue and the next hvsock write sent
  memory the stack was refilling. The guest trusts offloaded checksums (`NEEDS_CSUM`),
  so TCP never noticed. The busy guest after `rmi` was only what made the queue fill.
  A one-line fix; a CRC around each write counted 1-22 buffers changed in flight per
  engine before it and 0 after.
- **Evidence:** [guest-networking.md](../research/guest-networking.md) "Layer digest
  mismatches on LAN pulls", `TestGuestLinkSpareNotLive`.
- **Audience:** anyone writing a user-mode network stack, a virtio backend or a
  double-buffered writer; "how to debug data corruption without guessing".
### 8. One flag halved our WebView2 GUI, and fixed its shutdown
- **Hook:** the same home screen in a WebView2 window used 172 MiB private working set
  across 7 processes; with `--disable-gpu` it used 77 MiB. Pure Win32 + GDI+ drew it in
  5.4 MiB and one process.
- **Story:** the GPU process held about 95 MiB for a page with four cards. With the GPU on,
  5 of 72 window closes left 4 to 6 `msedgewebview2.exe` processes alive for 5 to 10 s,
  even inside a kill-on-close job object (single-thread processes stuck while exiting);
  with software compositing 0 of 31 did. Also: a freshly copied `.exe` takes about 500 ms
  longer to start, before its first instruction runs (the antivirus scan), so "cold start"
  is mostly first-run-after-install.
- **Evidence:** [ADR 0008](../adr/0008-settings-gui-mcp.md) "Toolkit spike results",
  `spike/gui/measure`.
- **Audience:** anyone choosing WebView2 / Tauri / Wails against native on Windows.

### 10. Our VM booted from AppData and failed from Program Files
- **Hook:** the same `hyperbutler.exe` and guest image start a VM in 1.5 s from a folder
  the user owns, and fail with access denied from `C:\Program Files`, for a user who is in
  Hyper-V Administrators.
- **Story:** HCS opens a VM's disks as a per-VM virtual account, and `HcsGrantVmAccess`
  adds that account to the file's DACL, so it needs WRITE_DAC: fine on your own files,
  impossible on an installer's. The fix is one inherited ACE for `NT VIRTUAL MACHINE\Virtual
  Machines` (S-1-5-83-0) on the image folder, set by the MSI, and an engine that skips the
  per-VM grant when the group can already read. Proved without admin first, with an
  `OWNER RIGHTS` ACE that takes away the owner's implicit WRITE_DAC. Plus the other things
  only a real install found: WiX naming a file `!(bindpath.stage)hyperbutler.exe`, and an
  uninstall step that recreated the registry key it had just deleted.
- **Evidence:** [docs/install.md](../install.md) "Test on a real machine", commit history
  of `dist/msi` and `internal/vm/account_windows.go` (`VMGroupCanRead`).
- **Audience:** anyone shipping a Hyper-V or HCS tool (WSL-like, Kata, sandboxes) with an
  installer.

### 11. Designing a privileged Windows service so its bugs can't become your root
- **Hook:** privileged container helpers on Windows have shipped a run of local privilege
  escalations (Docker Desktop's com.docker.service CVEs, ADR 0004 lists five). hyperbutler
  needs one too, because HCS is closed to normal users, so it was designed backwards from
  those bug classes.
- **Story:** the broker takes no paths, command lines or HCS JSON, only seven typed TLV
  requests with canonical decoding (fuzzed); identity comes from the pipe client's token,
  never the message; the data directory arrives as a handle, reopened relative to itself
  with OBJ_DONT_REPARSE and checked for owner, links and volume type on the same handle, and
  held without FILE_SHARE_DELETE while the VM lives; the image is pinned by a hash compiled
  in; the starting connection is the VM's lease; hvsock services are limited to the
  owner's SID. The adversarial review still found two cross-user problems (a global lock an
  oplock could stall for 35 s, and one user's start revoking another's ACE on the shared
  system disk) and CI found a third (an elevated shell's files are owned by Administrators,
  not the user). Compare with WSL's wslservice.
- **Evidence:** [ADR 0004](../adr/0004-hcs-access-broker.md) (rules, decisions, review
  findings), `internal/broker`, `internal/broker/proto` fuzzers, `internal/vmtemplate`.
- **Audience:** Windows systems programmers, security reviewers, anyone writing a service
  that acts for unprivileged users. Best written once the per-user pipes and the
  multi-account tests (M3, M6) are done.

## Smaller posts and tips

- **Changing daemon.json without restarting the VM:** delivered over the hvsock control
  channel, validated with `dockerd --validate`, rolled back in about 3 s if dockerd won't
  start. [settings.md](../research/settings.md), `bench/daemon-json`.
- **host.docker.internal, safely:** forward any port to the host's loopback and nothing
  else; DNS rebinding and flood limits. ADR 0006 "Host gateway".
- **A read-only, hash-verified guest from signed Alpine packages, reproducibly.**
  [ADR 0005](../adr/0005-guest-image.md).
- **Why `go test` keeps popping Windows Firewall prompts:** a test binding 0.0.0.0, rebuilt
  into a new temp path every run.
- **PowerShell gotcha:** `--arch amd64,arm64` passed to a native tool through a function
  becomes `System.Object[]`, and the tool silently did nothing.
- **A LAN test on a 100 Mbit adapter:** both paths hit 94 Mbit/s, so it proved only "no
  loss at this speed" (the cheap USB-C hub's RTL8152 chip).
- **What a crashed VM leaves on your files:** HcsGrantVmAccess adds
  `NT VIRTUAL MACHINE\<runtime id>` (S-1-5-83-1-...) before the VM exists, because the
  runtime id is a name-based GUID; a crash leaves the entry behind, and
  HcsRevokeVmAccess also drops the shared worker-capability entry other VMs added.
  Matching entries against live compute systems is not enough: another installation
  sharing the image has granted its VM access before creating it, and another user's VMs
  are invisible. So the engine learns its VM's runtime id before the grant (a probe grant
  on a scratch file), records it in state.json, and `hyperbutler uninstall` removes only
  those. `bench/uninstall`.
- **"Deny delete" that denies nothing:** the HCS broker holds the user's data disk open
  without FILE_SHARE_DELETE so it cannot be renamed under the VM, yet a test renamed it
  at once. A handle without data access (read, write or delete) takes no part in Windows
  share-mode checks at all, so its share mode is ignored; the handle needs FILE_READ_DATA
  for the missing FILE_SHARE_DELETE to bite. ADR 0004 "HCS backend",
  `internal/broker/diskfile_windows.go`, `TestDataDiskAccepted`.
- **Your Windows trusts CAs Linux has never heard of:** 18 of the 29 valid roots in a
  stock Windows 11 store are missing from Alpine's Mozilla bundle, and the store changes
  by itself (AuthRoot fills on demand), which is why the import runs only at boot and on
  an explicit apply: re-reading it on a reconnect would restart dockerd and every
  container.
- **`localhost` and `127.0.0.1` can be two different servers:** Windows lets one
  process bind `0.0.0.0:8080` (or `[::1]:8080`) while another holds `127.0.0.1:8080`. So
  a "is this port free?" probe on the wildcard passes, the engine's `127.0.0.1` bind
  fails, its `[::1]` bind succeeds, and `curl 127.0.0.1:8080` reaches the host program
  while `curl localhost:8080` reaches the container. `docker run` itself succeeded.
  Docker Desktop and a second engine side by side hit the same rule. The fix asks
  instead of binding: the system's TCP listener table (`GetExtendedTcpTable`) shows every
  listener on every address, opens no socket and so can never raise the Windows Firewall
  prompt that a wildcard probe bind can; the API proxy then fails the container start
  with Docker's own "Ports are not available" error
  ([usage-scenarios.md](../research/usage-scenarios.md#s34-both-publish-the-same-port)).
- **A CRC on a link that cannot corrupt:** hvsock delivers what it is given, and with
  checksum offload neither the guest kernel nor gVisor checks a TCP checksum, so one
  reused buffer on the host turned `docker pull` into "unexpected commit digest". The
  per-frame CRC-32C (33 GB/s with SSE4.2, 1-2% of the engine's CPU at 8 Gbit/s) now
  drops such frames instead.
  Replaying the bug showed what corruption on a framed stream looks like: whole frames
  in the wrong place (harmless, TCP takes them for retransmissions), frames mixed from
  two (every one caught by the CRC), and, most often, a length read from the middle of
  a payload, which no CRC can save but which ends the link loudly.
- **Same words, different error:** a test that passed on Windows failed on Linux
  because gVisor's reset error and Linux's `ECONNRESET` are both "connection reset by
  peer", and the relay told the two apart by text. On Windows the socket error reads
  differently, so the bug only existed on the platform the code does not ship on.
- **Proving a copy is a copy:** importing from Docker Desktop needs one write there,
  because the Docker API reads a volume only through a container. The import creates a
  never-started, read-only helper, hashes every volume it reads before, during and after
  the copy, and compares Docker Desktop's images, volumes and containers before and
  after every run. The three hashes also catch a volume that changes under the copy.
  Tested against a second hyperbutler serving Docker Desktop's pipe name, and a run
  killed mid-copy is cleaned up by the next one.

- **The browser that phoned home only on its second launch.** The GUI's footprint bench
  checks that the window's processes hold no socket. The first launch passed; every
  later one held two TLS connections to Microsoft from the WebView2 browser process, to
  its experimentation service (`config.edge.skype.com`). A resolver rule that fails
  every lookup, a dead proxy and the variations-fetch switch changed nothing: the
  connection is made outside Chromium's network stack, and only a group policy turns
  it off. Bisecting the profile pointed at one file, `Local State`: removing it before
  each start makes every launch a first launch, and 50 of 50 runs then held no socket
  ([gui.md](../gui.md#no-network-requests-req-ux-005)).

- **The commit hash that outranked the release.** Installing v0.1.0-preview.1 over an
  early development build left a product without its main executable, and the log said
  nothing failed. That build's version was a bare commit, `26a483c`; the resource tool
  read its leading digits as file version 26.0.0.0. Windows Installer saw an installed
  `hyperbutler.exe` "newer" than the release's 0.1.0.0, skipped the copy, and a moment
  later the upgrade's removal of the old product deleted the old file. One line in the
  package script, and a reminder that Windows Installer compares four numbers, not what
  you meant ([install.md](../install.md#run-of-2026-11-03)).

- **The handle leak that wasn't.** The 24 h soak failed on the broker's handle count:
  +44 at 13.5 h, and after that about two more every few engine start/stop cycles. A
  per-type handle count of the SYSTEM service showed only Thread and Event handles
  growing, and every extra Thread handle pointed at a live thread. The Windows RPC
  runtime keeps a duplicate thread handle and an event for each OS thread that makes its
  first RPC call (HCS is RPC to vmcompute underneath), until that thread exits. Go
  never retires its OS threads. So the count follows the highest number of threads the
  broker has ever needed at once, and it stopped at 461 after 93 cycles. Neither half
  leaks on its own, but together they look exactly like a leak
  ([release.md](../release.md#1-release-gates-from-the-adrs)).

## Still to measure before writing

- Side by side with Docker Desktop on WSL2 on the same machine (9P baselines need WSL).
- The LAN registry pull on a gigabit link.
