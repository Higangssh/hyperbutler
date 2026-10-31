# Pain points: Docker on Windows via WSL2

Research snapshot, October 2026. It covers Docker Desktop (WSL2 backend), dockerd in a WSL
distro, Rancher Desktop, and Podman machine. Each item lists the symptom, the root cause, why
today's workaround is bad, and the sources. Sources marked † are blog or forum posts with weak
methodology or unverified details. Treat their numbers as rough orders of magnitude. Every
requirement in [requirements.md](../requirements.md) links back to an anchor here.

Sections 1–7 are about the engine and WSL2. Section 8 is about the Docker Desktop
*application* itself: its window, settings, updates, account, and support flow. What
hyperbutler should do about section 8 is in [settings-and-gui.md](settings-and-gui.md).

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
  working, with zero setup. See section 9 for details.

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

## 8. Docker Desktop app

This section covers the Docker Desktop application on Windows, 2023–2026: the window, tray,
settings, updates, account, and support flow. It doesn't repeat the engine and WSL2 problems
above. Where an item overlaps one of them, it links to it and adds only the app's side.

- **Reaction counts:** Taken from GitHub on 2026-10-31. "r" means reactions and "c" means
  comments.
- **How common:** GitHub reactions undercount, because most users never file an issue. A
  count tells you an issue is real. A missing count doesn't tell you it's rare.
- **Reddit:** We could not read Reddit directly, so Reddit is under-represented here. The
  HN, Stack Overflow, and Docker forum threads cover the same ground.

### A1. "Starting the Docker Engine…" hangs, and quitting hangs too
- **What happens:** The window shows a spinner and never reaches "running". Users quit, kill
  processes, run `wsl --shutdown`, or reboot several times. Quitting can hang the same way
  ("Docker Desktop stopping"). F3 covers the cold-start time. This item is about the app
  getting stuck.
- **Evidence:**
  - Stack Overflow ["Docker Desktop Starting…" forever](https://stackoverflow.com/questions/71238673):
    78 votes, about 128k views.
  - [SO 71084718](https://stackoverflow.com/questions/71084718), "Docker Desktop stopped…"
    right after install: 88 votes, about 389k views.
  - [for-win#14552](https://github.com/docker/for-win/issues/14552) (2025, 16r, open): the user
    must reboot several times before it starts.
  - [desktop-feedback#537](https://github.com/docker/desktop-feedback/issues/537) (2025, 13r,
    open).
  - [for-win#12576](https://github.com/docker/for-win/issues/12576) (69r, open): endless
    "stopping".
  - Some hangs come from the app's own config files. In the
    [release notes](https://docs.docker.com/desktop/release-notes/), 4.84 fixed a hang on a
    malformed `install-settings.json`, 4.85 fixed a ~7-minute delay before the error appeared,
    and 4.90 fixed a hang after a WinGet upgrade. All are from 2026.
  - Docker is still working on how startup *looks*. 4.91 (Sep 2026) shows a skeleton layout
    "instead of a blank window".
- **How common:** Very common. It's the most-viewed Docker Desktop problem on Stack Overflow.

### A2. Updates break working setups, and they come every week
- **What happens:**
  - Releases come often. Docker's
    [release FAQ](https://docs.docker.com/desktop/troubleshoot-and-support/faqs/releases/)
    says new releases come "every week". The release notes list 51 releases in 2026 up to
    4.97 (2026-10-26), 42 of them minor versions.
  - Old versions are not supported. Pro and Team get support for the latest version only.
    Business gets six months.
  - Many updates break something. Of the 27 docker/for-win issues opened since 2023 with ≥ 20
    reactions, 13 name a specific Docker Desktop release as the trigger in their title. Two
    more blame a WSL update (U1).
- **Evidence:**
  - [for-win#13824](https://github.com/docker/for-win/issues/13824): 4.26 broke WSL2 (63r).
  - [for-win#14187](https://github.com/docker/for-win/issues/14187): 4.32 (50r, open).
  - [for-win#14060](https://github.com/docker/for-win/issues/14060): 4.30 stopped file updates
    (36r).
  - [for-win#14644](https://github.com/docker/for-win/issues/14644): 4.39 won't start (29r,
    open).
  - [for-win#14563](https://github.com/docker/for-win/issues/14563): 4.38 (27r).
  - [for-win#14453](https://github.com/docker/for-win/issues/14453): 4.36 (26r).
  - [for-win#14240](https://github.com/docker/for-win/issues/14240): 4.33.1 (24r, open).
  - The updater fails too. The Windows MSI could not update versions 4.56–4.65. The fix was to
    uninstall first, and the 4.66 notes say uninstalling "removes all associated data".
  - The app vanished after an update in
    [desktop-feedback#212](https://github.com/docker/desktop-feedback/issues/212) (13r, 42c)
    and [for-win#13941](https://github.com/docker/for-win/issues/13941). The same complaint is
    in [for-win#1310](https://github.com/docker/for-win/issues/1310) (48r), open since 2017.
  - The demand for control is old and loud:
    - [roadmap#183](https://github.com/docker/roadmap/issues/183), "don't upgrade docker
      without asking": 136r. It is from 2020, before this research window.
    - [roadmap#216](https://github.com/docker/roadmap/issues/216), forced updates break dev
      environments: 33r.
    - [roadmap#263](https://github.com/docker/roadmap/issues/263), schedule updates for later:
      24r, open.
- **How common:** Very common. This is the biggest app-level theme in the issue tracker. U1
  covers breakage caused by *WSL* updates.

### A3. Update nags, silent component updates, and notifications
- **What happens:**
  - Update checks are on by default. Error and new-release notifications are always on,
    according to the [settings docs](https://docs.docker.com/desktop/settings-and-maintenance/settings/).
  - Since 4.46 (Sep 2025), Compose, Scout, Gordon, and Model Runner can update silently in the
    background ([Docker blog](https://www.docker.com/blog/docker-desktop-silent-component-updates/)).
    There is an opt-out toggle.
  - 4.97 fixed a failed update putting back shortcuts the user had deleted on purpose.
- **Evidence:**
  - Docker forum, Aug 2026: ["Spammy notifications"](https://forums.docker.com/t/spammy-notifications/152276)†.
    The user complains about badges for minor events such as an update download.
  - [roadmap#263](https://github.com/docker/roadmap/issues/263): users are reminded while in
    the middle of work.
- **How common:** Anecdotal. We found no count of how many people mind the nags. It is mainly
  an annoyance, unlike A2, where updates actually break things.

### A4. Sign-in prompts and account pressure
- **What happens:** Signing in is optional for most features, but the app keeps asking, signs
  users out unexpectedly, and ties some local features to an account.
- **Evidence:**
  - [desktop-feedback#51](https://github.com/docker/desktop-feedback/issues/51) (2025, 30r): a
    "You are signed out" notification on every start for a user who chose not to sign in.
  - [for-win#12745](https://github.com/docker/for-win/issues/12745) (2022, 100r, 144c): logged
    out every day.
  - [desktop-feedback#466](https://github.com/docker/desktop-feedback/issues/466) (Jun 2026,
    59r): a *local* tool (`docker sandbox`) needs an account.
  - [roadmap#816](https://github.com/docker/roadmap/issues/816) (9r): AI tools can't be removed
    without a Business license.
  - A Docker forum thread (2024) found that the "host networking" beta forced sign-in
    ([forum†](https://forums.docker.com/t/why-am-i-being-forced-to-log-in/141519)).
  - 2026 [release notes](https://docs.docker.com/desktop/release-notes/) fix unexpected
    sign-outs again and again: 4.69, 4.72, 4.79, 4.84 (an unrelated credential delete erased
    the Hub tokens), and 4.92.
  - Org-enforced sign-in is a separate, intended feature
    ([docs](https://docs.docker.com/enterprise/security/enforce-sign-in/)). It has its own bugs,
    such as [for-win#14571](https://github.com/docker/for-win/issues/14571).
- **How common:** Common. The reaction counts are moderate, but the theme recurs across 2022–2026.

### A5. Heavy GUI and many background processes
- **What happens:**
  - The dashboard is an Electron app. It runs next to `com.docker.backend`, `com.docker.build`,
    a service, and the WSL VM.
  - Closing the window doesn't stop them.
    [for-win#14242](https://github.com/docker/for-win/issues/14242) (7r, open): after closing,
    the GUI won't reopen until a reboot.
  - Docker has admitted the idle cost. 4.82 (Jul 2026) "lowered the priority" of background
    processes and turned on Windows Efficiency Mode while idle.
  - The UI itself burns CPU. An Ask Gordon badge animation caused idle CPU use (fixed in 4.79
    and 4.92).
  - 4.43/4.44 fixed a leaked `com.docker.build` process that blocked startup
    ([for-win#14840](https://github.com/docker/for-win/issues/14840)).
  - On macOS, the UI polled the engine and woke the VM every 20 minutes with the window closed
    ([desktop-feedback#406](https://github.com/docker/desktop-feedback/issues/406)). That is
    macOS-only, but the design lesson applies on Windows too.
- **Evidence:**
  - A Docker forum report (4.22.1, 2023) saw 500–1,000 processes after a few days, and about
    3 GB for Docker Desktop on top of `vmmem`
    ([forum†](https://forums.docker.com/t/docker-desktop-for-windows-creates-a-whole-load-of-processes-and-eventually-crashes/137609)).
  - The installer is about 580–600 MB according to download aggregators†
    ([techdows†](https://techdows.com/download-docker-desktop-for-pc-windows-11-10-mac.html)).
  - HN users praise alternatives for *not* being Electron. One says OrbStack's GUI "launches
    instantly" ([HN](https://news.ycombinator.com/item?id=47152648)). Another likes Colima for
    having no Electron GUI ([HN](https://news.ycombinator.com/item?id=42721798)).
- **How common:** Widely felt but poorly measured. We found **no reliable numbers** for the
  GUI's RAM or the resident process count on Windows. `bench/footprint` should measure both
  (see U2).

### A6. Settings are scattered, need restarts, and sometimes get overwritten
- **What happens:**
  - Settings live in several places:
    - the GUI;
    - `%APPDATA%\Docker\settings-store.json` (renamed from `settings.json` in 4.35);
    - `install-settings.json`;
    - `admin-settings.json`;
    - `~/.docker/daemon.json`;
    - `~/.docker/config.json`;
    - `.wslconfig`.
  - The memory, CPU, and disk sliders don't apply under WSL2. The
    [settings docs](https://docs.docker.com/desktop/settings-and-maintenance/settings/) send
    users to `.wslconfig` instead.
  - Changes needed **Apply & restart**. Only since 4.43 (Jul 2025) is the VM no longer
    restarted.
- **Evidence:**
  - No CLI for settings. [roadmap#172](https://github.com/docker/roadmap/issues/172), "Control
    Docker Desktop via CLI", has 40r and has been open since 2020. Docker's
    [Desktop CLI](https://docs.docker.com/desktop/features/desktop-cli/) (GA in 4.39, Mar 2025)
    has start/stop/status/logs/enable/disable, but no general `config set`.
  - User settings get overwritten:
    - `credsStore` was reset to `desktop` on restart
      ([desktop-feedback#418](https://github.com/docker/desktop-feedback/issues/418), 48r).
      This was fixed in 4.34, then again for WSL distros in 4.93 (Sep 2026).
    - A malformed `daemon.json` was "silently overwritten" with defaults, fixed in 4.83
      (Jul 2026).
    - A `settings-store.json` or `daemon.json` with a UTF-8 BOM crashed startup, fixed in 4.93.
    - Under Settings Management, keys not set in `admin-settings.json` are reset at start
      (4.27).
  - Startup options are ignored:
    - [for-win#13887](https://github.com/docker/for-win/issues/13887) (14r): "open dashboard at
      startup" was ignored.
    - [for-win#2950](https://github.com/docker/for-win/issues/2950) (58r): "start at login" was
      ignored.
    - [roadmap#502](https://github.com/docker/roadmap/issues/502) (12r): users asked for "start
      minimized" so a window doesn't open on every boot.
  - Resource limits:
    - [SO 62405765](https://stackoverflow.com/questions/62405765) (62 votes): how to limit
      memory after moving to WSL2.
    - A forum report says Vmmem grew past the `.wslconfig` cap
      ([forum†](https://forums.docker.com/t/docker-desktop-eats-ram-past-wsl-limit-set-in-wslconfig-file/128150)).
- **How common:** Common. Each issue has moderate counts, but the pattern repeats across many
  releases.

### A7. Editing `daemon.json` through the app is fragile
- **What happens:** The app's "Docker Engine" tab is a raw JSON editor, and the app owns the
  file.
- **Evidence:** All from the [release notes](https://docs.docker.com/desktop/release-notes/):
  - 4.79: the editor opened minified JSON.
  - 4.88: validation errors vanished while typing.
  - 4.88: a `daemon.json` full of null bytes after a crash blocked startup.
  - 4.83: invalid files were silently replaced with defaults.
  - 4.35: `proxies` in `daemon.json` overrode the app's proxy settings.
- **How common:** We found no high-reaction issue. It's real but narrow, and it mostly hits
  power users (registry mirrors, `insecure-registries`, log drivers).

### A8. Proxy and corporate CA setup
- **What happens:** Behind a TLS-inspecting proxy (Zscaler and similar), pulls fail with
  `x509: certificate signed by unknown authority`.
- **Evidence:**
  - Docker keeps a dedicated [Zscaler guide](https://docs.docker.com/guides/zscaler/), which
    suggests the problem is common.
  - Docker Desktop builds its CA bundle from the Windows store. Users still report failures
    after a restart
    ([forum†](https://forums.docker.com/t/zscaler-docker-pull-and-failed-to-verify-certificate-x509-certificate-signed-by-unknown-authority/149339)).
  - Images and builds need the CA *inside* the container too, which the guide covers by hand.
  - Kerberos/NTLM proxy auth is a Business-only feature
    ([settings docs](https://docs.docker.com/desktop/settings-and-maintenance/settings/)).
  - App bugs:
    - [for-win#10894](https://github.com/docker/for-win/issues/10894) (9r, open): build ignores
      the proxy while pull works.
    - 4.39: a PAC file hung sign-in.
    - 4.39: [CVE-2025-1696](https://www.cve.org/CVERecord?id=CVE-2025-1696) wrote proxy
      credentials to logs in plaintext.
    - 4.73: the app called `mcp.docker.com` with the feature off, causing proxy prompts.
    - 4.92: Send Diagnostics hung behind a proxy.
- **How common:** Common in companies, rare at home. We found no high-reaction Windows issue.
  The evidence is the vendor guide and many forum threads.

### A9. Credential helper problems
- **What happens:** `~/.docker/config.json` points at `docker-credential-desktop(.exe)`. When
  that binary is missing (WSL distros, a removed Desktop, or sudo's PATH), every `docker
  login`, `pull`, and `compose` fails.
- **Evidence:**
  - [SO 67642620](https://stackoverflow.com/questions/67642620), "docker-credential-desktop not
    installed or not available in PATH": 147 votes, about 169k views.
  - Docker forum thread on the WSL2 `.exe not found` error
    ([forum†](https://forums.docker.com/t/docker-credential-desktop-exe-executable-file-not-found-in-path-using-wsl2/100225)).
  - [for-win#9061](https://github.com/docker/for-win/issues/9061) (8r) and
    [for-win#10243](https://github.com/docker/for-win/issues/10243) (7r).
  - The app also rewrites the setting (see A6). 4.33 fixed `wincred` entries surviving
    uninstall.
- **How common:** Common. It's one of the most-viewed Docker Desktop questions on Stack
  Overflow.

### A10. Elevation prompts after install
- **What happens:** UAC prompts appear for the "Docker Desktop Privileged Helper" after every
  reboot, even though the user approved them once. P2 covers admin rights in general.
- **Evidence:**
  - [for-win#13806](https://github.com/docker/for-win/issues/13806) (2023, 33r, 51c).
  - [desktop-feedback#651](https://github.com/docker/desktop-feedback/issues/651) (Sep 2026,
    10r): the prompt appears at every boot. Fixed in 4.94.
  - [for-win#14240](https://github.com/docker/for-win/issues/14240) (24r, open): the engine
    doesn't start because of the helper and elevation.
- **How common:** Moderate, and it comes back as regressions.

### A11. Diagnostics and the "factory reset" habit
- **What happens:**
  - The troubleshoot flow has **Restart**, **Clean up data** (deletes all containers and
    images), **Reset to factory defaults**, and **Gather diagnostics**
    ([troubleshoot docs](https://docs.docker.com/desktop/troubleshoot-and-support/troubleshoot/)).
  - Gathering diagnostics uploads a bundle to Docker to get an ID.
  - Direct support needs a paid subscription. Everyone else posts the ID on GitHub.
- **Evidence:**
  - Reset is often the only way out. 4.91 (Sep 2026) fixed a corrupt network database that
    "left a factory reset" as the only recovery.
  - The reset itself has bugs:
    - 4.69 deleted CLI plugins;
    - 4.82 left containers behind;
    - 4.97 deleted data without stopping the engines first.
  - Diagnostics could take "several minutes" until 4.97.
  - 4.69 stopped sign-in files from going into diagnostic bundles. 4.94 stopped API keys from
    going into logs and bundles.
  - [for-win#5458](https://github.com/docker/for-win/issues/5458) (16r): users want a reset
    command on the CLI.
  - U1 lists the community advice that destroys data.
- **How common:** We found no count of how often users are told to reset. The pattern shows up
  in many threads, so treat it as common but anecdotal.

### A12. Telemetry that is on by default and hard to fully turn off
- **What happens:** "Send usage statistics" defaults to on
  ([settings reference](https://docs.docker.com/enterprise/security/hardened-desktop/settings-management/settings-reference/)).
  Opting out didn't always stop data being sent.
- **Evidence:**
  - 4.71 (Apr 2026) fixed error tracking that kept sending session data after opt-out
    ([for-mac#7768](https://github.com/docker/for-mac/issues/7768)).
  - 4.85 (Aug 2026) fixed crash reports going to Bugsnag with analytics disabled.
  - [for-win#13722](https://github.com/docker/for-win/issues/13722) (2023, 10r): with Bugsnag
    blocked by DNS, the app made more than 100k lookups in 10 minutes.
  - [for-win#12718](https://github.com/docker/for-win/issues/12718) (10r, open): "how to
    completely disable telemetry?"
- **How common:** Low reaction counts. It matters to privacy-minded users and to companies
  with egress rules.

### A13. Feature bloat: extensions, AI, onboarding
- **What happens:** The app keeps adding products, and each one is something more to load,
  update, or turn off:
  - Extensions;
  - Ask Gordon (GA with paid plans in 4.74, May 2026);
  - MCP Toolkit, Model Runner, Offload, sandboxes;
  - Scout;
  - Kubernetes;
  - a welcome survey (4.50), walkthroughs, and a learning center.
- **Evidence:**
  - Docker made Extensions off by default in 4.74, after 4.72 added a notice that they run with
    "host-level privileges" and are not audited.
  - [roadmap#816](https://github.com/docker/roadmap/issues/816) (9r): users want to remove AI
    without a Business license.
  - [desktop-feedback#154](https://github.com/docker/desktop-feedback/issues/154) (7r): an
    "Ask Gordon" action still showed with Gordon disabled.
  - On HN, one user dislikes the "AI crap in the UI"
    ([HN](https://news.ycombinator.com/item?id=47152238)).
- **How common:** Low counts, and the evidence is mostly opinion. A13 is a reason people
  *leave* (see section 9) more than a reason they file issues.

### A14. Licensing friction for companies
- **What happens:** U2 covers the basic rule: a paid seat is needed above 250 employees or
  $10M revenue.
  - Team went from $9 to $15 per user per month (annual) in late 2024
    ([devclass](https://www.devclass.com/containers/2024/09/12/docker-ups-subscription-prices-by-up-to-80-bundles-more-services-to-compensate/1629993)).
  - Older versions are supported only on Business (A2).
  - Kerberos proxy auth (A8) and settings lockdown are Business-only.
- **Evidence:**
  - HN users say they switched to Colima or Rancher Desktop to avoid seats
    ([HN](https://news.ycombinator.com/item?id=47154151)).
  - One user warns that Docker enforces the license
    ([HN](https://news.ycombinator.com/item?id=42725368)). That is anecdotal.
- **How common:** Common in companies. It's the main reason given for trying alternatives.

### A15. Uninstall leaves things behind
- **What happens:** Uninstalling leaves data and settings behind:
  - multi-GB VHDX files under `%LOCALAPPDATA%\Docker\wsl`;
  - the `docker-desktop` WSL distros;
  - `C:\ProgramData\Docker`;
  - hosts-file entries;
  - credentials and registry keys.
- **Evidence:**
  - [for-win#7489](https://github.com/docker/for-win/issues/7489) (2020, 97r): unable to
    uninstall.
  - [for-win#1544](https://github.com/docker/for-win/issues/1544) (14r, open):
    `ProgramData\Docker` can't be removed.
  - [for-win#5220](https://github.com/docker/for-win/issues/5220): hosts-file entries stay.
  - A Docker forum user found 13.4 GB left behind
    ([forum†](https://forums.docker.com/t/how-to-completelly-remove-docker-and-all-leftovers/134189)).
  - Recent fixes:
    - 4.33: `wincred` values;
    - 4.67: processes left running;
    - 4.84: an uninstall that failed midway could lock users out of their own app-data folders;
    - 4.91: a stale registry key caused reboot prompts;
    - 4.96: the install folder was left behind.
- **How common:** Common, judging by the number of cleanup guides. The issue counts are
  moderate.

### A16. The app after sleep, shutdown, and idle
- **What happens:** S2 covers the VM side. On the app side:
  - Resource Saver froze every WSL distro
    ([for-win#14656](https://github.com/docker/for-win/issues/14656), 23r;
    [for-win#14827](https://github.com/docker/for-win/issues/14827), 13r).
  - Since 4.36, a manual pause no longer auto-resumes.
  - 4.88 fixed HTTP 500s on waking from idle shutdown.
  - The app crashed when Windows shut down
    ([for-win#13697](https://github.com/docker/for-win/issues/13697), 2023, 51r).
- **How common:** Moderate. It overlaps M1 and S2.

### A17. What people actually use the GUI for
- **Gap:** We found **no survey** of which Docker Desktop features people use. The signals
  below are indirect.
- **Signals:**
  - **Container list, start/stop, logs:** The [dashboard docs](https://docs.docker.com/desktop/use-desktop/logs/)
    center on these. Docker added a unified Logs view in 4.65.
    - One forum user says searching logs is "a little nicer" in a GUI
      ([Lemmy†](https://lemmy.klein.ruhr/post/24048/131834)).
    - The TUI lazydocker (53k GitHub stars) and Portainer (39k) do exactly this list, logs, and
      start/stop job without Docker Desktop.
  - **Open a published port in the browser:**
    [for-win#9537](https://github.com/docker/for-win/issues/9537) (27r).
  - **Image and disk cleanup:** The Images view shows "reclaimable" space. Release notes
    4.94–4.96 fixed bugs in it, and the disk footer was added in 4.35. This lines up with D1–D2.
  - **Volumes:** [roadmap#215](https://github.com/docker/roadmap/issues/215) (25r).
  - **Not used much:** Some developers say they never needed a GUI that shows "how many images
    I have" ([HN](https://news.ycombinator.com/item?id=47152324)). Others run Docker Desktop
    only for the VM it provides ([Lemmy†](https://lemmy.klein.ruhr/post/24048/131834)).
- **Takeaway:** The GUI is used mostly as a status light plus a container, log, and cleanup
  viewer. It is not used for the extra products in A13.

## 9. Competitive landscape

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

## 10. Implications for hyperbutler's design

1. **Own the VM and its disk.** Don't depend on WSL, so WSL updates can't break hyperbutler (U1).
2. **Return disk space online:**
   - Use a disk file hyperbutler owns, marked NTFS-sparse (`FSCTL_SET_SPARSE`).
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
8. **Keep the app layer small and honest** (section 8): no account, no telemetry, no forced
   updates, settings in one file with a CLI, and a GUI that runs only while its window is open.
   See [settings-and-gui.md](settings-and-gui.md).
