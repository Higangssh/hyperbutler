# Pain points: Docker on Windows via WSL2

Research snapshot, October 2026. It covers Docker Desktop (WSL2 backend), dockerd in a WSL
distro, Rancher Desktop, and Podman machine. Each item lists the symptom, the root cause, why
today's workaround is bad, and the sources. Sources marked † are blog or forum posts with weak
methodology or unverified details. Treat their numbers as rough orders of magnitude. Every
requirement in [requirements.md](../requirements.md) links back to an anchor here.

**Common root cause.** Every alternative listed above runs inside the same WSL2 utility VM. They
all inherit WSL's file sharing, memory, disk, networking, and update behavior. A runtime that
owns its own VM can fix these problems at the source.

---

## 1. Disk

### D1. The VHDX grows but never shrinks
- **Symptom:** `ext4.vhdx` or `docker_data.vhdx` stays at its high-water mark after images and
  volumes are deleted. One report shows a 377 GB file holding about 30 GB of real data.†
- **Root cause:** A dynamic VHDX allocates blocks on first write. The guest mounts with
  `discard`, and Hyper-V turns those discards into trims of the VHDX's block table. But unless
  the VHDX is in sparse mode, that space is only marked free *inside* the file. The NTFS file
  shrinks only after an offline compaction.
- **How widely reported:** Very widely. [microsoft/WSL#4699](https://github.com/microsoft/WSL/issues/4699)
  was opened in 2019, is still open, and has about 380 comments. Its top workaround comment has
  more than 1,000 reactions. [#8226](https://github.com/microsoft/WSL/issues/8226) is a duplicate
  with reports through 2025.
- Sources: [MS disk-space docs](https://learn.microsoft.com/en-us/windows/wsl/disk-space),
  [MS thin provisioning](https://learn.microsoft.com/en-us/windows-hardware/drivers/storage/thin-provisioning),
  [dev.to†](https://dev.to/hamada147/docker-on-windows-eating-your-c-drive-how-i-reclaimed-over-350gb-from-bloated-vhdx-files-11dm)

### D2. `docker system prune` frees no space on the host
- **Symptom:** Prune reports gigabytes reclaimed, but the host file doesn't change. In one case
  `docker system df` showed 12.4 GB while the VHDX was 15.9 GB and growing about 1.5 GB per
  Docker Desktop update.
- **Root cause:** D1, plus a Docker Desktop leak that prune can't see: old ISO blobs piling up
  in `isocache`. The leak was fixed in 4.76, but the space it already used was never given back.
  BuildKit cache is often the biggest consumer, about 200 GB in one #8226 report. Docker Desktop
  has no build-cache garbage-collection setting in its UI.
- Sources: [forums.docker.com†](https://forums.docker.com/t/docker-desktop-wsl2-vhdx-grows-by-1-5gb-after-every-update-no-way-to-reclaim-space/151729),
  [desktop-feedback#375](https://github.com/docker/desktop-feedback/issues/375)

### D3. Reclaiming space is a manual, admin-only ritual
- **Steps:** Quit Docker, run `wsl --shutdown`, run `fstrim`, then `Optimize-VHD -Mode Full`
  as admin. `Optimize-VHD` needs the Hyper-V module, which **doesn't exist on Windows Home**.
  The fallback is `diskpart` → `compact vdisk`, also as admin.
- **Why it's bad:** It needs admin rights, which locked-down corporate laptops don't have. It
  stops every distro and container. It's slow, and the results vary: some users get 0 bytes
  back. One VHDX was reported corrupted after `Optimize-VHD` (#4699).
- **Vendor attempts:** Docker Desktop 4.34 (Aug 2024) added automatic WSL disk reclaim, and
  4.59 (Feb 2026) *disabled it again for stability*. WSL has a draft `wsl --manage --compact`,
  but it works only while the distro is stopped.
- Sources: [Docker release notes](https://docs.docker.com/desktop/release-notes/), #4699

### D4. Sparse VHD is unsafe and unreliable
- **Symptom:** Sparse mode returns space while the VM runs. In one measurement, 20 GB came back
  within about 5 s.† But since WSL 2.5.6 (Apr 2025) it's gated behind `--allow-unsafe` because
  of possible data corruption.
- **Evidence:** Corruption reports go back to [#10609](https://github.com/microsoft/WSL/issues/10609)
  (2023), and the root cause was never confirmed. A new one,
  [#41837](https://github.com/microsoft/WSL/issues/41837) (WSL 3.0.1, Oct 2026), shows a sparse
  VHDX corrupted when the host disk filled up. Reclaim also stops working on large disks
  ([#12103](https://github.com/microsoft/WSL/issues/12103)). `diskpart` refuses to compact sparse
  disks. Backup and AV tools can fill the holes back in.
- Sources: [#13075](https://github.com/microsoft/WSL/issues/13075), [VRAM Lab†](https://vramlab.com/posts/wsl2-sparse-vhd-cannot-compact/)

### D5. No real disk limit, and a full host disk corrupts data
- **Symptom:** The default virtual maximum is 1 TB, so `df` inside the guest shows plenty of free
  space until C: is full. Docker's "Disk usage limit" setting doesn't apply on WSL. One user set
  64 GiB and ended up with a 264 GB file. When the host disk filled, the VHDX broke and Docker
  offered to format it. Release notes 4.80–4.97 contain a long run of disk-full recovery fixes.
- Sources: [MS disk-space docs](https://learn.microsoft.com/en-us/windows/wsl/disk-space),
  [desktop-feedback#728](https://github.com/docker/desktop-feedback/issues/728)

### D6. Moving data off C: is fragile
- **Symptom:** `wsl --manage --move`, export/import, and Docker's disk-location setting are all
  fragile. Bugs included moves to another drive always failing (fixed in 4.92), an interrupted
  move deleting the disk image (fixed in 4.91), and access-denied errors after `--move`. Moving
  also doesn't shrink the file.
- Sources: [Docker release notes](https://docs.docker.com/desktop/release-notes/)

## 2. File sharing and performance

### F1. Bind mounts from Windows paths are slow
- **Root cause:** drvfs serves Windows files over 9P on hvsock. Every `stat`, `open`, and
  `readdir` is a round trip across the VM boundary, so metadata-heavy workloads suffer most.
- **Numbers:** `/mnt/c` vs ext4:
  - 1 GiB fsync'd write: 125 vs 850 MB/s, about 7× slower.†
  - Creating and deleting 200 files: 1.16 vs 0.02 s, about 60× slower.†
  - `npm ci`: 65–100 s vs 12–20 s.†
  - `git status`: 2.5–5 s vs 0.05–0.1 s.†
  - `find` over a repo: about 12 s on WSL2 even with virtiofs, vs about 1 s on WSL1 (July 2026
    comment).
- **How widely reported:** [microsoft/WSL#4197](https://github.com/microsoft/WSL/issues/4197) has
  been open for 7 years with about 500 comments.
- **Workaround:** Docker [recommends](https://docs.docker.com/desktop/features/wsl/best-practices/)
  keeping code in ext4. Windows IDEs and tools then go through `\\wsl$`, which is slow in the
  other direction. Microsoft's virtiofs (WSL Containers) claims about 2× over 9P. That's still
  far from ext4 on metadata.
- Sources: [brainwagon†](https://brainwagon.org/blog/2026_07_11_wsl2_filesystem_speed), #4197

### F2. File watching is broken
- **Symptom:** Edits made on the Windows side don't trigger webpack, Vite, nodemon, Django, or
  Next.js reloads. Nothing reports an error.
- **Root cause:** The 9P server doesn't forward change notifications. Microsoft confirmed this is
  a regression from WSL1. [microsoft/WSL#4739](https://github.com/microsoft/WSL/issues/4739) has
  been open since 2019 with about 190 comments.
- **Workaround:** Polling (`CHOKIDAR_USEPOLLING`, Vite `usePolling`), which costs CPU and adds
  latency. The alternative is moving code into WSL.

### F3. Cold start is slow
- **Numbers:** Docker Desktop takes about 20–60 s to become ready.† WSL itself takes 1–10 s, and
  40–60 s with systemd is common ([#10966](https://github.com/microsoft/WSL/issues/10966),
  [#8569](https://github.com/microsoft/WSL/issues/8569)). Docker Desktop sometimes hangs at
  "Starting the Docker Engine" ([for-win#12576](https://github.com/docker/for-win/issues/12576)).
- Sources: [devtoolreviews†](https://www.devtoolreviews.com/reviews/docker-desktop-vs-podman-vs-rancher-desktop)

## 3. Memory

### M1. `vmmem` keeps RAM after containers stop
- **Symptom:** `free` inside the guest shows about 360 MiB used while Vmmem holds several GB.
- **Root cause:** The guest page cache and freed pages aren't returned to the host.
  `autoMemoryReclaim` is still `[experimental]`. It now defaults to `dropCache`, which helps only
  partly and slowly.
- **Workaround:** `wsl --shutdown`, which kills everything, or running `drop_caches` from cron.
  [#10143](https://github.com/microsoft/WSL/issues/10143) was bot-closed in July 2025 without a fix.
- **Docker Resource Saver:** It saves only CPU on WSL2, because Docker doesn't own the VM. It
  froze every distro ([WSL#11066](https://github.com/microsoft/WSL/issues/11066),
  [for-win#14656](https://github.com/docker/for-win/issues/14656)), and 4.42.1 turned it off on
  WSL2.
- Sources: [wsl-config docs](https://learn.microsoft.com/en-us/windows/wsl/wsl-config),
  [Resource Saver docs](https://docs.docker.com/desktop/use-desktop/resource-saver/)

### M2. Idle footprint is large
- **Numbers:**
  - Docker Desktop: about 2.5–3 GB right after start on Windows 11 ([forum†](https://forums.docker.com/t/docker-desktop-idle-memory-usage/138540)).
  - Podman: about 0.8–1.2 GB.†
  - Rancher Desktop: about 1.2 GB, plus about 500 MB for k3s.†
  - WSL's default memory cap is 50% of host RAM.
- **Gap:** We found no rigorous idle-memory benchmark on Windows. Publishing a reproducible one
  is itself useful.

## 4. Networking

### N1. Port forwarding is flaky
- **NAT mode:** The localhost relay drops ports and leaks CLOSE_WAIT connections
  ([#5942](https://github.com/microsoft/WSL/issues/5942), [#9751](https://github.com/microsoft/WSL/issues/9751)).
  `::1` hangs while `127.0.0.1` works ([#41860](https://github.com/microsoft/WSL/issues/41860)).
  LAN access needs `netsh portproxy`.
- **Mirrored mode:** It breaks Docker `-p`: Docker's NAT rules assume loopback traffic never comes
  from outside Linux, so the ports can't be reached ([moby#48056](https://github.com/moby/moby/issues/48056),
  [WSL#10494](https://github.com/microsoft/WSL/issues/10494)). Other bugs: the relay forwards to
  wrong ports ([#41137](https://github.com/microsoft/wsl/issues/41137)), and WSL 2.9.3 regressed
  ([#40984](https://github.com/microsoft/WSL/issues/40984)).
- Source: [MS networking docs](https://learn.microsoft.com/en-us/windows/wsl/networking)

### N2. VPN and corporate DNS break
- **Symptom:** The generated `resolv.conf` breaks when a VPN connects, and VPN routes clobber the
  WSL subnet. The usual hack is `generateResolvConf=false` with hard-coded nameservers, which
  breaks split DNS.
- **Partial fixes:** `dnsTunneling` and `autoProxy`. Userspace network stacks (gvisor-tap-vsock,
  wsl-vpnkit) avoid the problem by sending traffic out through Windows sockets.
- Sources: [WSL#4855](https://github.com/microsoft/WSL/issues/4855), [Rancher network tunnel](https://docs.rancherdesktop.io/1.9/ui/preferences/wsl/network/)

## 5. Sleep and resume

### S1. The guest clock drifts after sleep
- **Symptom:** The guest clock is minutes to hours behind after sleep or hibernate. TLS fails, and
  apt reports "Release file is not valid yet" ([#13867](https://github.com/microsoft/WSL/issues/13867),
  open).
- **Workaround:** Run `hwclock` by hand, set up chrony, or `wsl --shutdown`.

### S2. Hangs and CPU spikes after resume
- **Symptom:** vmmem goes to 100% CPU and WSL or Docker hangs. Sometimes even `wsl --shutdown`
  hangs.
- **How widely reported:** [for-win#12968](https://github.com/docker/for-win/issues/12968) has been
  open since 2022. Related: [WSL#9855](https://github.com/microsoft/WSL/issues/9855),
  [#8696](https://github.com/microsoft/WSL/issues/8696) (a 2026 report of vmmem above 13 GB).
- **Workaround:** Kill `wslservice.exe` or reboot.

## 6. Updates and operations

### U1. WSL updates break Docker
- **WSL 2.5.7 (May 2025):** Docker Desktop failed to start with "context deadline exceeded".
  Regressions were still being reported through Aug 2026
  ([for-win#14804](https://github.com/docker/for-win/issues/14804)).
- **WSL 3.0.1 auto-update (Oct 2026):** `iso9660` went missing and the engine hung on "starting".
  The fix was to uninstall WSL and install an older MSI
  ([WSL#41759](https://github.com/microsoft/WSL/issues/41759)).
- **Docker Desktop's own updates:** 4.75 silently disabled distro integration, and 4.92 timed out
  waiting for the WSLg mount.†
- **Common recovery advice:** `wsl --unregister docker-desktop` or deleting `docker_data.vhdx`.
  Both **destroy data**.

### U3. Alternatives have compatibility gaps
- **Symptom:** The tools people switch to don't fully replace the Docker API and Compose
  workflow:
  - WSL Containers (`wslc`) makes no Docker API or Compose compatibility claim.
  - podman-compose double-prefixes Windows paths, and mount propagation fixes must be redone
    after each restart.†
  - The Lima WSL2 driver supports only some options.
- **Why it matters:** Users want the stock `docker` CLI, Compose, and Docker API tools to keep
  working, with zero setup. See section 8 for details.

### U2. Licensing and weight
- **Licensing:** Docker Desktop needs a paid per-user subscription above 250 employees or $10M
  revenue ([install docs](https://docs.docker.com/desktop/setup/install/windows-install/)).
- **Weight:** It installs multiple VMs and processes plus a GUI. We found no reliable numbers for
  installer size or process count, so `bench/` should measure them.

## 7. Platform

### P1. Windows Home
- **Docker's support:** Its requirements list Pro, Enterprise, and Education, with an ambiguous
  note that Home runs Linux containers only.
- **Disk tooling:** `Optimize-VHD` (D3) isn't available on Home.

### P2. Admin rights
- **Today:** A per-user Docker install needs no admin, but enabling WSL needs admin once per
  machine, and some settings need elevation. Disk compaction always needs admin (D3).

### P3. AV/EDR
- **Symptom:** Real-time scanning of VHDX files and build output slows I/O. IT often refuses
  folder exclusions.†

### P4. ARM64 and GPU
- **ARM64:** Docker's Windows on ARM support is still "Early Access" more than two years after the
  4.31 beta.
- **GPU:** Only NVIDIA through WSL GPU paravirtualization is documented.

### P5. Shared hypervisor
- **Symptom:** WSL2, Windows Sandbox, and Memory Integrity share the Windows hypervisor. That
  causes conflicts with VirtualBox and VMware.†

## 8. Competitive landscape

- **Microsoft WSL Containers (`wslc.exe`):** GA on 2026-09-29 in WSL 3.0.1, with virtiofs by
  default, a NuGet API, and Intune and Defender integration. It still runs on WSL, so it inherits
  U1 (the same release broke Docker Desktop). We found no Docker API or Compose compatibility
  claim. ([MS docs](https://learn.microsoft.com/en-us/windows/wsl/wsl-container),
  [devblog](https://devblogs.microsoft.com/commandline/wsl-container-is-now-available-for-public-preview/))
- **Podman machine (WSL):** Inherits D1–D6 and M1. Windows path translation bugs in
  podman-compose, and mount-propagation fixes that must be reapplied after each restart.†
- **Rancher Desktop:** Hangs at "Initializing", conflicts with wsl-vpnkit, and is heavier because
  of k3s.†
- **Lima (WSL2 driver):** Experimental, needs a tar rootfs, and supports only some options.
  colima has no Windows support ([Lima docs](https://lima-vm.io/docs/config/vmtype/wsl2/)).

## 9. Implications for winbox's design

1. **Own the VM and its disk.** Don't depend on WSL, so WSL updates can't break winbox (U1).
2. **Return disk space online:**
   - Use a disk file winbox owns, marked NTFS-sparse (`FSCTL_SET_SPARSE`).
   - Expose virtio-blk discard to the guest and turn discards into `FSCTL_SET_ZERO_DATA` hole
     punches.
   - Run `fstrim` on a timer and enable BuildKit GC by default.
   - Treat host disk-full as a first-class, safe state.
   - Open question: can an HCS-managed VHDX get the same unmap → hole-punch path outside Hyper-V?
     The spike must test it (ADR 0002).
3. **Return memory:** Use virtio-balloon free page reporting and page cache control, so that
   stopped containers give RAM back without a shutdown.
4. **File sharing:** Make metadata latency and change notifications the primary metrics, not
   sequential throughput.
5. **Networking:** Use a userspace stack (gvisor-tap-vsock) that sends traffic out through
   Windows sockets. Do host-side port forwarding for `127.0.0.1`, `::1`, and opt-in LAN access.
6. **Resume:** Sync the clock on resume and test sleep/resume explicitly.
7. **Never require admin** after the one-time enabling of Windows features.
