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

## Still to measure before writing

- Side by side with Docker Desktop on WSL2 on the same machine (9P baselines need WSL).
- The LAN registry pull on a gigabit link.
