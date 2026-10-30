# 0005. Guest image: kernel, root file system, and how we build it

- Status: proposed
- Date: 2026-10-30
- Requirements: REQ-START-001, REQ-MEM-001, REQ-MEM-002, REQ-DISK-001, REQ-DISK-008,
  REQ-PLAT-002, REQ-PLAT-005, REQ-UX-002, REQ-API-001

## Context

winbox boots one Linux guest that runs dockerd. The guest image is the kernel, whatever it
boots into, and the disks it mounts. It has to:

- Boot under HCS (primary, [ADR 0002](0002-primary-vm-backend.md)) and under QEMU-WHPX (the
  fallback) from the same artifacts.
- Reach "Docker API ready" well inside REQ-START-001 (median ≤ 8 s for 0.1, ≤ 5 s for 1.0).
  That budget also has to cover the host side.
- Leave room for REQ-MEM-001 (idle host memory ≤ 400 MB for 0.1, ≤ 250 MB for 1.0). Anything
  the guest keeps resident counts against it.
- Be built and published without WSL (REQ-PLAT-002 applies to contributors too). The
  maintainer's machine is Windows 11 Home with WSL disabled.
- Be reproducible, and meet the licenses of what it ships. winbox is Apache-2.0, but the
  image carries GPL-2.0 code (Linux, BusyBox).

### Required kernel features

These were checked against the stock Alpine `linux-virt` 6.18.58-0 config
(`config-6.18.58-0-virt` from the v3.24 netboot directory). "Run-time" means the prototype
below exercised the feature on QEMU-WHPX. Nothing was exercised on HCS yet: that needs an
elevated run (ADR 0002), which this work wasn't allowed to do.

| Feature | Needed for | linux-virt 6.18.58 | Verified |
|---|---|---|---|
| hv_sock (`HYPERV_VSOCKETS`) + `VSOCKETS` | Docker API and agent transport on HCS | `=m` / `=m` | config only (HCS not run) |
| virtio vsock (`VIRTIO_VSOCKETS`) | the same on QEMU | `=m` | module loads, `PF_VSOCK` registered. QEMU 11.1 for Windows has **no vsock device**, see Findings |
| vmbus (`HYPERV`), `HYPERV_UTILS`, `HYPERV_TIMER` | HCS at all, time sync, shutdown | `=y`, `=m`, `=y` | config only. The vmbus core is built in |
| Hyper-V storage (`HYPERV_STORAGE`, hv_storvsc) + SCSI disk (`BLK_DEV_SD`, sd_mod) | SCSI disks on HCS. hv_storvsc alone gives a SCSI host with no `/dev/sdX` | `=m` / `=m` | config only |
| Hyper-V net (`HYPERV_NET`, hv_netvsc) | NIC on HCS | `=y` | config only |
| virtio PCI, blk, net, console, rng | QEMU | `=y`, `=m`, `=m`, `=y`, `=m` | run-time (net, rng); blk loads |
| overlayfs (`OVERLAY_FS`) | containerd overlayfs snapshotter | `=m` | run-time: `docker info` reports driver `overlayfs`, and hello-world ran |
| cgroup v2 (`CGROUPS`, `MEMCG`, `CGROUP_BPF`, `CPUSETS`, `CGROUP_PIDS`, `BLK_CGROUP`, `CFS_BANDWIDTH`) | resource limits, REQ-MEM-003 | all `=y` | run-time: `docker info` reports `cgroupfs` / v2 |
| Free page reporting (`PAGE_REPORTING`) + balloon (`VIRTIO_BALLOON`, `HYPERV_BALLOON`) | REQ-MEM-001/002 | `=y`, `=m`, `=m` | guest side run-time ("Free page reporting enabled"). The **host side does nothing on QEMU-WHPX**, see Findings |
| erofs / squashfs (`EROFS_FS` with zstd and lzma, `SQUASHFS`) | read-only system image (Decision) | `=m` | config only |
| ext4 | data disk | `=m` | module loads |
| Discard path: ext4 `discard`/`fstrim`, SCSI UNMAP through hv_storvsc, `virtio-blk` discard | REQ-DISK-001 (freed space back to the host) | ext4, sd, and virtio_blk support it in-kernel | **not verified.** Whether HCS turns UNMAP on a dynamic VHDX into host deallocation is the disk track's question, see Follow-ups |
| Netfilter (`NF_TABLES`, `NFT_NAT`, `NFT_MASQ`, `NFT_COMPAT`, `IP_NF_*`, `BRIDGE`, `BRIDGE_NETFILTER`, `VETH`) | Docker bridge networking and published ports | `=m` | bridge, br_netfilter, and veth load. No iptables/nft userspace yet, so the prototype ran with `--iptables=false` |
| `virtio-fs`, `9p` | file sharing (fs-sharing track) | `=m` | not available in QEMU for Windows, see Findings |

Also relevant from the same config: `MODULE_SIG=y` without `MODULE_SIG_FORCE`,
`MODULE_COMPRESS_GZIP=y` without `MODULE_DECOMPRESS` (so whoever loads a module has to
gunzip it), `INIT_ON_ALLOC_DEFAULT_ON=y`, `TMPFS_XATTR=y`, `RD_GZIP`/`RD_XZ`/`RD_LZ4`/`RD_ZSTD=y`.

## Options

Two choices are mostly independent. One is what the guest boots into, the root file system
(A). The other is which kernel it uses (B).

### A1. LinuxKit

`linuxkit build` assembles a kernel and an initramfs from a YAML file. Every component is an
OCI image. The init runs `onboot` and `services` entries as containerd containers. Docker
Desktop's VM came from this lineage.

- **Pros.** It's purpose-built for "minimal VM that runs containers". The project documents
  `kernel+initrd` output as reproducible
  ([docs/reproducible-builds.md](https://github.com/linuxkit/linuxkit/blob/master/docs/reproducible-builds.md)).
  Its packages are signed images pinned by content hash. Its 6.12.x kernel config already has
  hv_storvsc, hv_netvsc, hv_balloon, virtio_balloon, `PAGE_REPORTING`, and overlayfs built in,
  with hv_sock and virtio vsock as modules. It has Hyper-V and QEMU platform docs.
- **Cons.**
  - Building means pulling OCI images. `linuxkit pkg build`, needed for our own agent package,
    needs Docker with buildx. On Windows without WSL that's the chicken and the egg, so
    building is CI-only.
  - The default `kernel+initrd` output keeps the whole root file system in guest RAM. That's
    the problem the prototype measured below. LinuxKit also has `kernel+squashfs` and
    `kernel+erofs` outputs, which give roughly the A2b layout. So RAM alone doesn't rule it out.
    The documented reproducibility covers only `tar`, `tar-kernel-initrd`, `docker`, and
    `kernel+initrd`, though.
  - Services run as containers, which adds a containerd hop before dockerd can start.
  - We'd take LinuxKit's kernel cadence, a handful of lines from 5.4 to 6.12 in `kernel/`.
    Its `config-x86_64` does not set `VIRTIO_FS`.
  - The last tagged release is v1.8.2 (2025-09-16), although master is still active
    (pushed 2026-10-27).
  - Licensing doesn't change. We'd still redistribute a GPL kernel and owe its source.
- **Evidence.** Not prototyped. The pros and cons come from the upstream repository, docs,
  and kernel config as of 2026-10-30.

### A2. Alpine-based initramfs plus disks (the prototype)

Here we write our own `/init`. Userland comes from Alpine packages: `busybox-static`, the
kernel modules from `linux-virt`, and CA certificates. Docker Engine binaries come either
from Docker's static tarball or from Alpine's `docker-engine`, `containerd`, and `runc`. There
are two layouts:

- **A2a. Everything in the initramfs.** This is what the prototype does: 61.7 MB gzip,
  175 MB unpacked.
  - Pros: one artifact besides the kernel. It's trivially buildable on Windows (below).
  - Cons: the unpacked initramfs is tmpfs/rootfs, so it stays in guest RAM for the VM's
    lifetime (measured `Shmem` 174 MB). Unpacking it is the largest single boot phase we
    measured (≈ 0.95 s). `pivot_root(2)` doesn't work on rootfs, so the prototype runs runc
    with `DOCKER_RAMDISK=true` (chroot instead of pivot_root), which is weaker isolation.
    `switch_root` into a tmpfs copy would fix that, but at the cost of one more 175 MB copy. It also won't fit a small VM:
    at `-m 512` the uncompressed variant failed with "Initramfs unpacking failed: write error".
- **A2b. Slim initramfs, read-only system disk, data disk.** The initramfs holds only BusyBox,
  `/init` or the agent, and the modules (≈ 9 MB unpacked: the prototype's BusyBox, modules,
  and relay add up to that). It mounts an erofs (or squashfs) system image from a read-only disk: SCSI via
  hv_storvsc on HCS, virtio-blk on QEMU. That image holds dockerd, containerd, runc,
  iptables/nft, e2fsprogs, and CA certificates. `/init` then switches root into an
  overlay (erofs lower, tmpfs upper), so runc can use `pivot_root` normally.
  `/var/lib/docker` lives on an ext4 data disk (dynamic VHDX), which the guest formats on
  first boot. Windows can't create ext4 itself.
  - Pros: binaries become page cache, so it's reclaimable and only touched pages are read.
    No large unpack. The system image is replaced on upgrade and the data disk is kept
    (REQ-DISK-008, REQ-PLAT-005). The data disk is where REQ-DISK-001 trim/compaction work
    happens.
  - Cons: three artifacts. Building the erofs image needs `mkfs.erofs`, which doesn't run on
    Windows, so that part is built on Linux (CI or a QEMU-WHPX Alpine VM). It also adds a disk
    device to both backends.

### B1. Stock Alpine `linux-virt` kernel

- **Pros.**
  - Every feature in the table above is present.
  - Alpine signs and ships it, with security updates on Alpine's schedule.
  - The `.apk` gives a verifiable chain: an RSA signature over the control segment, then
    `datahash` over the data segment. The pinned `.apk`s were checked this way once, with a
    one-off script (openssl plus a gzip-member splitter) that is **not** in the repo. Today
    `build.ps1` only compares SHA-256 pins. The kernel inside is byte-identical to the netboot
    `vmlinuz-virt` used in ADR 0002.
  - Its modules come from the same signed package.
  - GPL source is a known aports commit plus kernel.org tarballs.
- **Cons.**
  - Most of what we need is `=m`, so we must ship and load modules. Measured cost: 0.04 s
    for 26 modules.
  - Alpine keeps only the newest package in the index, so the pin must point at an exact
    file. The 6.18.58-r0 `.apk` was still downloadable after 6.18.59-r0 replaced it in the
    index, but that's not guaranteed. **We must mirror** the pinned `.apk` and its source.
  - It carries and probes drivers we don't need (DRM, sound, USB, ATA: `ata_piix` finds
    QEMU's DVD-ROM). How much kernel time a trimmed config would save isn't measured.
  - We don't control config changes between Alpine releases.

### B2. Custom kernel config (own build, or LinuxKit's kernel)

- **Pros.**
  - Hyper-V, virtio, vsock, overlayfs, erofs, and ext4 can be built in, so there are no
    modules and no `modprobe` step. Everything else can be dropped, giving a smaller image and
    a faster init.
  - We can enable `MODULE_DECOMPRESS`, pick the initramfs compression, and set defaults
    (for example `INIT_ON_ALLOC_DEFAULT_ON=n`, at a security cost).
  - We control when the version moves.
- **Cons.**
  - We become a kernel distributor: CVE tracking, rebuilds, testing on both backends, and
    publishing the exact source and config for every binary.
  - A kernel build needs Linux. It doesn't build natively on Windows, and would take tens of
    minutes in a 2-vCPU QEMU-WHPX VM (an estimate, not measured).
  - The savings we can see today are small. Module loading is 0.04 s. All of kernel init up
    to the initramfs is 0.45 s on QEMU, out of 2.2 s total, and that is the ceiling for what
    trimming the config could save.

## Prototype

Files are under `guest/` (throwaway, but builds and tests on its own):

- `guest/cpio`: a newc cpio reader and writer, plus a spec-file builder close to the
  kernel's `gen_init_cpio` format. The output is deterministic: sorted tree walk, sequential
  inodes, a fixed mtime, and a zero gzip header. There are unit tests and fuzz tests for the
  reader and the spec parser.
- `guest/cmd/mkinitramfs`: builds or lists an initramfs. Runs on Windows.
- `guest/cmd/sockfwd`: a TCP→Unix socket relay, cross-compiled with `GOOS=linux`. It stands
  in for the agent's vsock relay.
- `guest/prototype/`:
  - `init`: PID 1. It mounts the API file systems and cgroup2, loads modules for whichever
    bus it finds (vmbus or virtio), sets up a static network, and starts dockerd on its Unix
    socket.
  - `initramfs.spec`, `modules.txt`, `rootfs/etc/*`
  - `sources.lock`: pinned URLs and SHA-256s.
  - `build.ps1`: fetch, verify, unpack with Windows' own `tar.exe`, resolve the module
    closure from `modules.dep`, gunzip the modules, cross-compile, and build the cpio.
  - `run-qemu.ps1`: boot and measure.
  - Since the A2b implementation (below): `initramfs-init` (stage 1), `modules-boot.txt`,
    `system.spec`; `init` became the system disk's `/sbin/init`.
- `guest/image`: the fixed-VHD footer and the `image.json` manifest, with fuzz tests.
- `guest/cmd/mksysdisk`: wraps the erofs image as `system.vhd` and writes `image.json`.

Reproduce (PowerShell, non-elevated, Go on `PATH`):

```powershell
powershell -ExecutionPolicy Bypass -File guest\prototype\build.ps1
powershell -ExecutionPolicy Bypass -File guest\prototype\run-qemu.ps1 -Runs 11
powershell -ExecutionPolicy Bypass -File guest\prototype\run-qemu.ps1 -Runs 1 -Smoke -IdleSeconds 60
```

### Measurements

**Setup**

- **Machine:** Core Ultra 7 155H, Windows 11 Home 26200, on AC power.
- **Defender:** in passive mode (real-time protection off). Note that the requirements doc
  measures with it on, so `bench/` must redo these runs.
- **QEMU:** 11.1.0 run non-elevated with `-accel whpx -m 512 -smp 2`, the default `pc`
  machine, `virtio-net-pci` (user networking with hostfwd), `virtio-balloon-pci`
  (free-page-reporting=on), and `virtio-rng-pci`.
- **Cmdline:** `console=ttyS0 panic=-1`.
- **Guest:** kernel `linux-virt` 6.18.58-r0, Docker static 29.9.2 (containerd 2.4.2).

**dockerd-ready time**

This is measured from launching `qemu-system-x86_64.exe` until `GET /_ping` returns `OK`
through hostfwd → sockfwd → `/run/docker.sock`. Each poll uses a fresh TCP connection.

**Result: median 2198 ms (n = 11, min 2169, max 2301).**

This number is indicative, not a REQ-START-001 result:

- REQ-START-001 asks for 20 runs with a warm-up run discarded, measured from `winbox start`
  to `docker info`. Here it's 11 runs with no discard, measured from the VMM launch to
  `/_ping`.
- Before the harness fix (Findings 5), the same image measured ≈ 2.9 s.
- While other sessions loaded the machine, single runs went up to 9 s. Their guest-side marks
  were slow too, so the cause was host contention, not the image.

**Where the time goes**

The kernel phases use printk timestamps. The `/init` and dockerd phases use the `winbox-t`
marks (from `/proc/uptime`) and dockerd's own log. The two guest clocks disagree by
≈ 0.15 s: "Run /init" prints at 1.42 s, while `init-start` reads 1.27 s. So the rows don't
add up exactly. The host-side part is derived.

| Phase | Time |
|---|---|
| QEMU start, firmware, kernel load (host) | ≈ 0.3 s |
| Kernel init until "Unpacking initramfs" | 0.45 s |
| Initramfs unpack (61.7 MB gzip → 175 MB), "Unpacking initramfs" to "Freeing initrd memory" | **≈ 0.95 s** |
| `/init`: mounts, 26 modules, network | 0.08 s |
| dockerd exec → "API listen" (managed containerd boots in 0.13–0.16 s of this) | ≈ 0.55 s |

The unpack runs asynchronously (`initramfs_async`, on by default). Device probing and
keyring setup overlap with it, and `/init` waits for it to finish. Removing the unpack
therefore saves up to 0.95 s but probably less. The real saving is unmeasured; see
Follow-up 2.

**What moved the number**

These are medians of 5–11 runs on the same machine, with some harness changes between them,
so read the deltas as approximate.

| Change | Effect |
|---|---|
| dockerd with a `-H tcp://` listener, even with `--tls=false` | dockerd's own log shows **exactly 1.0 s** between "Starting up" and "Listener created". Moving to a Unix socket plus relay cut the median from 3.22 s to 2.24 s. |
| `virtio-rng-pci` + `virtio_rng` | dockerd's first log line waits for `crng init done` (the Go runtime blocks in `getrandom`). Adding the device moved crng init from uptime 1.70 s to ≈ 1.41 s and the median from 3.40 s to 3.22 s. Under HCS, `crng init done` already appears at 0.008 s (ADR 0002 log), so it isn't needed there. The likely root cause is that QEMU's default `qemu64` CPU model has no RDRAND. `-cpu host` (or `+rdrand`) may remove the need for the device and also fix Findings 4. Untested. |
| Uncompressed cpio (175 MB) instead of gzip, at `-m 1024` | 3.03 s vs 3.24 s. It saves ≈ 0.2 s of unpack but costs 115 MB more initrd. At `-m 512` it doesn't boot at all. |

**Smoke test**

hello-world was pulled through slirp and run via the API in 2.7 s: exit 0, and it printed
"Hello from Docker!". This exercises overlayfs, cgroup v2, runc (under `DOCKER_RAMDISK`), and
TLS with Alpine's CA bundle.

**Idle memory**

These were taken 60 s after ready, with no containers. They are **not** REQ-MEM-001 numbers.
The requirement sums Private Working Set from performance counters over winbox.exe, the VMM,
and helpers, over minutes 5 to 10. The harness reads `Process.PrivateMemorySize64` (commit)
and `WorkingSet64` (which includes shared pages) after one minute.

| Where | Value |
|---|---|
| QEMU process, private bytes | **573 MB** |
| QEMU process, working set | **453 MB** |
| Guest `MemTotal` | 479 MB |
| Guest `MemFree` | 257 MB |
| Guest `Shmem` (the unpacked initramfs) | **174 MB** |

The working set is the same with free page reporting on or off (439 vs 442 MB).

**Image size**

| Artifact | Size |
|---|---|
| `vmlinuz-virt` | 12.6 MB |
| Initramfs (gzip -9) | 61.7 MB |
| Initramfs, unpacked | 175 MB |
| `dockerd` (not stripped) | 103 MB |
| `containerd` | 39 MB |
| `runc` (with debug info) | 14 MB |
| `containerd-shim-runc-v2` | 8 MB |
| `docker-proxy` | 2.7 MB |
| `docker-init` | 0.7 MB |
| Modules (26, decompressed) | 5.9 MB |
| `busybox.static` | 1.0 MB |
| `sockfwd` | 2.3 MB |
| CA bundle | 0.2 MB |

Alpine's `docker-engine` 29.9.2 package is about the same size (102.5 MB installed), so
switching vendors doesn't shrink this.

**Reproducibility**

A second build from an empty cache (fresh downloads, different output directory) produced
the identical initramfs: SHA-256 `2d2282867a4fe236363aa212c4a57ba72308228692a047c326a24dfa4ef0911d`.
Some limits on that:

- Both builds were from the same checkout, on the same machine, with the same Go 1.27.0.
  Go's `compress/flate` output is only stable within a Go version.
- The first version embedded VCS stamps (commit, dirty flag) in `sockfwd`, which would have
  changed the hash on every commit. It's now built with `-buildvcs=false`, but builds from
  two different commits haven't been compared yet.

### Findings that affect other tracks

1. **QEMU-WHPX does not return freed guest memory.** With free page reporting on, QEMU's
   stderr fills with
   `ram_block_discard_shared_range: MADVISE not available pc.ram:... (-40)`. The Windows
   build has no discard, so the guest's reports are dropped. Private bytes stay at `-m` plus
   overhead, and the working set never shrinks. On the QEMU fallback, REQ-MEM-001/002 can
   only be met by giving the VM little memory. This strengthens HCS-primary in ADR 0002, and
   it's a requirement for any future WHP-native VMM: discard via `WHvUnmapGpaRange` plus
   `DiscardVirtualMemory`/`VirtualFree`, or similar.
2. **QEMU 11.1 for Windows has no `vhost-vsock`, `virtio-fs`, or `virtio-9p` device**
   (`-device help`). It does have `virtio-serial`. The QEMU fallback's transport must be
   virtio-serial or forwarded TCP (as here), not vsock. File sharing on QEMU needs another
   design.
3. **dockerd must not listen on TCP itself** (the 1 s sleep). The agent should relay hvsock
   or vsock to the Unix socket, which is also the safer design.
4. The default QEMU CPU model is `qemu64` ("QEMU Virtual CPU version 2.5+"). Images built
   for x86-64-v2 or later may not run. `-cpu host` with WHPX is untested (qemu track).
5. Harness pitfall: with PowerShell `Start-Process`, every QEMU launch after the first one in
   the same session reached first serial output ≈ 0.7 s later (2.95 s vs 2.25 s overall).
   `Process.Start` doesn't show this. The cause wasn't investigated. `bench/start` should
   launch the VMM the way `winbox.exe` will.

## Decision

1. **Root file system: A2b**, a slim initramfs plus a read-only erofs system disk plus an
   ext4 data disk.

   Not A2a. Its 174 MB of unreclaimable `Shmem` is the biggest memory cost we measured, and
   its unpack is the biggest single boot phase (up to 0.95 s, actual saving unmeasured). The
   layout doesn't scale to the 1.0 targets.

   Not LinuxKit, even though its `kernel+erofs` output could give a similar layout:
   - Our own agent would have to be a LinuxKit package, and `linuxkit pkg build` needs
     Docker with buildx. So there would be no Windows-native path at all.
   - Its services start as containerd containers, which puts a hop before dockerd.
   - Its documented reproducibility doesn't cover the erofs/squashfs outputs.
   - Its release cadence is slow.

   Its good ideas (pinned, content-addressed inputs; reproducible kernel+initrd) are adopted
   directly. This choice rests on build-path and control arguments, not on a measured LinuxKit
   boot. A LinuxKit `kernel+erofs` boot under the same harness would settle it if A2b
   disappoints.
2. **Kernel: B1, stock Alpine `linux-virt`, pinned by exact package and SHA-256 and
   mirrored by us**, for 0.1. Revisit B2 if any of these happens:
   - a required feature is missing or regresses in `linux-virt`;
   - `bench/start` on HCS shows kernel init above 0.5 s, or module loading above 0.2 s;
   - Alpine's cadence blocks a security fix for more than 7 days.
3. **Userland source:** Alpine packages (`busybox-static`, `docker-engine`, `containerd`,
   `runc`, `iptables` or `nftables`, `e2fsprogs`, `ca-certificates-bundle`), all from
   one pinned Alpine release, signed with the same keys. Note that this is two repositories:
   `main` (kernel, BusyBox, CA bundle, e2fsprogs, iptables) and `community` (`docker-engine`,
   `containerd`, `runc`). `community` has a shorter support promise, and both must be
   mirrored. The Docker static tarball has no published
   checksum or signature (TOFU over HTTPS). Its `dockerd` and `runc` are static glibc builds
   (the ELF files carry glibc's GNU/Linux ABI note). It stays in the prototype only.
4. **Build and publish:** the canonical build runs on **GitHub Actions Linux runners**,
   inside an `alpine:3.24` container pinned by digest, with every `.apk` pinned by SHA-256
   and its signature chain verified by `apk`.
   It produces the kernel, initramfs, and erofs image, plus a GPL source bundle. All of these
   go to GitHub Releases as assets (never git), with SHA-256 sums and build provenance
   attestations. `winbox.exe` embeds the expected hashes.

   Two paths for contributors without WSL:
   - The initramfs can be rebuilt natively on Windows. That works today, with `guest/cmd/mkinitramfs`.
   - The erofs image is built by `mkfs.erofs` in a pinned Alpine container on any Docker
     Engine, winbox's own included (see "A2b implementation"). No QEMU VM is needed.
5. **Kernel command line:** keep the serial console and no `quiet` (needed to debug boots).
   Revisit log volume once boot time is measured on HCS.

### Boot-time budget

REQ-START-001 runs from `winbox start` until `docker info` succeeds: median ≤ 8 s (0.1) and
≤ 5 s (1.0). We budget against the 1.0 number, so 0.1 has a wide margin. `bench/start`
checks each line separately, using the `winbox-t` marks.

| Phase | Budget | QEMU-WHPX, A2a (measured) | HCS reference (ADR 0002 log) |
|---|---|---|---|
| VMM create and start, firmware, kernel load | 0.4 s | ≈ 0.3 s | 0.16 s create+start, first serial at 0.31 s |
| Kernel init up to the initramfs | 0.5 s | 0.45 s | ≈ 0.33 s |
| Initramfs unpack | 0.1 s | **≈ 0.95 s** (over budget, hence A2b) | 0.09 s for Alpine's 9.4 MB initramfs |
| `/init` or agent: modules, mounts, system disk, network | 0.3 s | 0.08 s (no disks yet) | not measured |
| dockerd to "API listen" | 1.0 s | 0.55 s | not measured |
| Host: `winbox.exe`, transport, the `docker info` round trip | 0.7 s | not measured (the harness pings `/_ping` over slirp) | not measured |
| **Total** | **3.0 s** | 2.2 s to `/_ping` | |
| First boot only: format the data disk | + 1.0 s | not measured | reported separately, as REQ-START-001 asks |

That leaves 2 s of headroom against the 1.0 target for things we haven't built yet:
iptables/nft setup, the guest agent handshake, and disk checks.

### Licensing obligations

| Component | License | What we must do |
|---|---|---|
| Linux kernel and modules | GPL-2.0-only | Ship the complete corresponding source with each release. That's the kernel.org tarball plus Alpine's patches and config from the pinned aports commit, as a release asset next to the binaries. GPLv2 §3 counts equivalent access from the same place as distributing source, which avoids a 3-year written offer. A link to Alpine's servers is not enough. |
| BusyBox | GPL-2.0-only | Same: a source tarball for the exact `busybox` version and aports build files. |
| musl | MIT | It's linked into busybox-static and Alpine's binaries. Include the license text. |
| Moby/dockerd, containerd, runc, docker-proxy | Apache-2.0 | Include their LICENSE (Apache-2.0 §4(a)) and NOTICE files (§4(d)) in the image and the release. Don't use "Docker" as our product name or imply endorsement. Refer to "Docker Engine (Moby)" only descriptively. |
| tini (`docker-init`) | MIT | License text. |
| libseccomp (in runc) | LGPL-2.1 | Alpine links it dynamically, which keeps it replaceable. Ship the license and offer source with the rest. The Docker static tarball's binaries are static glibc (LGPL-2.1+) builds, and the relinking duty that brings is another reason not to ship it. |
| CA bundle | MPL-2.0 | Include the license. Point to the source form, Mozilla's `certdata.txt` at the matching revision. The PEM file is derived from it. |
| Go standard library and modules (in sockfwd, the future agent, and the Go binaries above) | BSD-3-Clause, plus each module's own license | License texts, generated from `go version -m` and the module licenses. |

The image is a "mere aggregation" with winbox's Apache-2.0 code. GPL obligations attach to
the GPL components, not to winbox. The release pipeline must generate a `THIRD-PARTY.md`, the
license texts, and the source bundle from the lock file, so nothing ships without them.
Today's `sources.lock` records only binary packages. It must gain a pinned source entry
(aports commit, kernel.org and BusyBox tarball hashes) for every GPL binary, plus a check that
fails the build when one is missing (Follow-up 6).

## A2b implementation (2026-10-30)

Decision 1 is now built and measured under HCS. Numbers and method are in
[docs/research/idle-memory.md](../research/idle-memory.md).

**Layout.** One image directory holds four files, all release artifacts (never git):

| File | What | Size |
|---|---|---|
| `vmlinuz-virt` | the pinned Alpine kernel, unchanged | 12.6 MB |
| `initramfs-stage1.gz` | stage 1 (a new name, so an older `winbox.exe` that knows only `initramfs-winbox.gz` fails with "file not found" instead of booting a stage 1 without its system disk): BusyBox, `initramfs-init`, the modules to reach the disk (`modules-boot.txt`: hv_storvsc, sd_mod, virtio_blk, erofs) | 0.9 MB |
| `system.vhd` | a fixed VHD (raw erofs, lz4hc, padded to whole MiB, plus the 512-byte footer) holding everything else: `init`, BusyBox and its applet links, dockerd, containerd, runc, the guest services, e2fsprogs, the CA bundle, all modules | 88 MiB |
| `image.json` | the manifest: name, size and SHA-256 of the three files | 0.5 KB |

**Boot.** Stage 1 loads its modules, finds the system disk by SCSI LUN
(`winbox.system=lun:1`, or a device name such as `vda` on QEMU), mounts it as erofs
read-only and `switch_root`s into it, which deletes the initramfs. The root is then a
real, read-only mount: runc uses `pivot_root` and `DOCKER_RAMDISK` is gone. What must be
writable is a mount (`/run`, `/tmp`, `/dev/shm` as tmpfs, `/var/lib/docker` on the data
disk) or a link into one (`/etc/resolv.conf` → `/run/resolv.conf`). The data disk is now
`winbox.data=lun:0` as well: the kernel named the system disk `sda` in some boots and
`sdb` in others, so device names are not stable with two disks. No overlay is needed.

**Build.** `build.ps1` builds the system tree as an uncompressed, deterministic cpio
(`system.spec`, `guest/cmd/mkinitramfs`), streams it into `mkfs.erofs` (erofs-utils
1.9.1-r0) in `alpine:3.24` pinned by digest on any Docker Engine (`-DockerHost`), and wraps
the result with `guest/cmd/mksysdisk`. `mkfs.erofs` gets `-T0`, `--all-root`, `-x-1` and a
UUID derived from the cpio's SHA-256; the VHD footer's id comes from the erofs bytes and
its timestamp from `-mtime`. Two builds from empty caches gave byte-identical files (all
four SHA-256 equal). Building takes about 12 s.

**Integrity.** Each start, the engine reads `image.json`, opens the three files with
`FILE_SHARE_READ` only, refuses reparse points and non-files, checks size and SHA-256
against the manifest, and checks the VHD footer (fixed type, checksum, size equal to the
bytes before it). It keeps the handles open until the VM is gone. So:

- a half-copied update or files from two builds never boot (the start fails with the
  file name);
- nothing can rewrite or delete a file between the check and HCS opening it, or while the
  guest reads its system disk (`TestOpenImage` checks the sharing violation);
- the guest cannot write the system disk: HCS attaches it `ReadOnly`, and the root is
  mounted `ro`.

The check costs about 70 ms (105 MB of SHA-256 from the host file cache) on each start.

**dm-verity: not now.** It verifies each block on read against a root hash, which only
helps if the root hash comes from somewhere the attacker cannot write. Here it would come
from the initramfs, which sits in the same directory as the disk and is checked by the same
manifest. What dm-verity would add is protection against a change to `system.vhd` while it
is held deny-write and attached read-only. Doing that takes a kernel-mode or
administrator-level attacker, who also owns the hypervisor host. Its costs are
`dm-mod` + `dm-verity` in stage 1, a hash tree (about 1% of the image), userland or a
device-mapper ioctl helper, and hashing on every read. Revisit it if a less-trusted party
ever caches or serves the image, or if one image is shared between users.

The manifest beside the image is not a trust anchor. Anyone who can write the image
directory can also replace `winbox.exe`. The release build should embed the manifest's
SHA-256 in `winbox.exe` (Decision 4) so a replaced image is refused even with a matching
`image.json`. That is not done yet.

**Update path.**

- An update replaces the four files and writes `image.json` last (`build.ps1` does).
- While winbox runs, the files cannot be overwritten, renamed or deleted. An updater must
  stop winbox first, and it gets a sharing violation instead of a torn image if it does
  not.
- A crash mid-copy leaves a set that fails the manifest check, with a clear error, until
  the copy is redone. Nothing boots a mixed set.
- The data disk is untouched by an image change (REQ-DISK-008). Its format (ext4, made by
  the guest) is the same for every image so far.
- Docker Engine upgrades migrate `/var/lib/docker` forward only. A rollback to an older
  image after a newer dockerd ran is not guaranteed to work, as with any Docker install.
- Better later: versioned image directories plus a pointer, switched atomically, so the
  old set stays as the rollback (installer track).
- The engine needs `HcsGrantVmAccess` on `system.vhd` (as on the data disk), which changes
  the file's DACL. That works in a user-owned directory. Under `Program Files` it needs the
  ADR 0004 broker or an install-time ACE for the VM group.

**Review of the A2b implementation.** The adversarial reviewer got the diff, the
requirements and the measurement scripts. Disposition:

| # | Finding | Disposition |
|---|---|---|
| A1 high | Lowering `read_ahead_kb` 10 s after boot does nothing for dockerd and containerd: a file's readahead is copied at `open()`. The M5 log showed the old 33–114 MiB swing. | **Fixed.** `init` reads the four big binaries once with 128 KiB, then sets 16 KiB *before* dockerd starts. Re-measured (idle-memory.md, M6). |
| A2 medium | The memory script omitted `vmwp.exe` (the VM worker), and a bug left the "new vmmem" filter empty, so attribution rested on creation time alone. | **Fixed.** The script snapshots `vmmem` before the start, takes `vmwp` as its parent, sums engine + vmmem + vmwp, and checks that the `vmmem` is gone after `winbox stop`. Before and after re-measured with it. |
| A3 medium | Start-time numbers had no saved log; REQ-START-001's first start after a reboot is not reported. | **Fixed:** the runs are logged and the doc records the run set, the phases and the dial-timeout cause (raw logs stay out of git); **cold start rejected for now:** it needs a host reboot, which this session must not do. Listed under Open. |
| A4 medium | `apk add erofs-utils=1.9.1-r0` is not pinned by hash, and Alpine drops old revisions. | **Accepted as a known gap.** It fails closed (the build stops when the revision disappears; it cannot silently change), apk checks the signature, and two clean builds were byte-identical. Follow-up 8: a builder image pinned by digest with erofs-utils in it, or the `.apk`s in `sources.lock`. |
| A5 medium | An older `winbox.exe` with a new image directory would boot stage 1 without `winbox.system=` and only time out. | **Fixed.** Stage 1 is now `initramfs-stage1.gz`, a name older engines do not look for, so they fail at once with "not found". Test: `TestGuestArtifacts`. |
| A6 low | No committed fuzz corpus; no test for `guestArtifacts`; LUN resolution and reparse rejection untested. | **Fixed** for the corpus (`guest/image/testdata/fuzz`) and `guestArtifacts`. **Rejected** for the shell LUN logic and reparse points: the shell is exercised on every boot (and the probes show the right disk at `/` and `/var/lib/docker` even when the kernel swapped `sda` and `sdb`), and creating a symlink needs Developer Mode or admin here. |
| A7 low | The manifest is not a trust anchor; VM-account ACEs pile up on `system.vhd` after crashes. | **Accepted.** Both are stated above (embedding the hash in `winbox.exe`; the same ACE issue exists on `data.vhdx`). Open item. |
| A8 low | The research doc still had placeholders. | **Fixed.** |

The reviewer also checked and found fine: manifest and footer parsing (no traversal,
geometry re-derived), `openDenyWrite` sharing and reparse handling, the `ReadOnly`
attachment and LUN mapping, that the blank-disk check can never target the system disk,
the `winbox.*` argument validation, the read-only root's writable paths, `swiotlb=noforce`
on non-isolated Hyper-V, THP `madvise`, the 400 ms dial timeout, and the dm-verity
rejection.

**Old images still work.** A directory without `image.json` is an initramfs-only image:
the engine boots it as before, with `winbox.data=sda`. The QEMU harness (`run-qemu.ps1`)
now attaches `system.vhd` as `vda`. This was not run: QEMU was not part of this work.

## Adversarial review

The adversarial reviewer got the diff and the
requirement. It returned 25 findings: 6 major and 19 minor. It confirmed every kernel-config
value in the features table, the HCS reference numbers, the MADVISE finding, and the newc
layout. It found no path traversal and no copied GPL code. Disposition of each finding:

| # | Finding | Disposition |
|---|---|---|
| 1 | LinuxKit has `kernel+squashfs`/`kernel+erofs`, so "RAM problem" is a strawman | **Fixed.** A1 and Decision 1 now acknowledge it. The rejection rests on build path, containerd hop, and reproducibility coverage, and says it isn't a measured comparison. |
| 2 | Unpack is ≈ 0.95 s, not 0.8, and it's async, so the saving is overstated | **Fixed.** The numbers are corrected, and the async overlap is stated. The saving is marked unmeasured (Follow-up 2). |
| 3 | sockfwd embedded VCS stamps, so the hash changes per commit | **Fixed** with `-buildvcs=false` (checked with `go version -m`). Cross-commit comparison is Follow-up 5. |
| 4 | `sd_mod` missing, so hv_storvsc disks would never appear | **Fixed.** Added to the table, `modules.txt`, and `init`. |
| 5 | REQ-DISK-001 cited, but the discard path isn't discussed | **Fixed.** Added a table row and Follow-up 7. |
| 6 | No GPL source pins in `sources.lock` | **Fixed in the ADR.** Stated as a gap and added as Follow-up 6. The lock format change waits for the CI build, which is where the source bundle is produced. |
| 7 | The `.apk` signature check isn't in the repo | **Fixed** (reworded). The CI build uses `apk`'s own verification (Decision 4). A Go verifier isn't worth writing for a throwaway Windows path. |
| 8 | "One repository" is wrong: main and community | **Fixed** (Decision 3). |
| 9 | `BRIDGE_NETFILTER`; stale `${KVER}` comment | **Fixed.** |
| 10 | Late crng is from no RDRAND on `qemu64` | **Fixed** (noted as the likely cause; `-cpu host` is untested). |
| 11 | Wrong memory counters for REQ-MEM-001 | **Fixed.** Stated explicitly. |
| 12 | Kernel and init timestamps come from two clocks | **Fixed.** The offset is stated. Unifying the clocks is for `bench/start`. |
| 13 | n = 11 with no warm-up discard isn't the REQ-START-001 protocol | **Fixed.** Marked as indicative. |
| 14 | Spec builder silently accepts file/dir conflicts and a later explicit dir | **Fixed.** These are now errors, with tests (`TestBuildRejectsConflicts`). |
| 15 | Writer accepts the name `TRAILER!!!` and over-long symlink targets | **Fixed**, with tests. |
| 16 | Backslashes in spec names | **Fixed.** Rejected, with a test and a fuzz invariant. |
| 17 | `-list` stops at the first archive | **Fixed.** `Reader.Concatenated`, with `TestConcatenated`. |
| 18 | `modules.dep` and `sources.lock` are parsed in untested PowerShell | **Rejected.** The prototype runs on pinned, hash-checked inputs, and the canonical build moves to Linux CI (Decision 4). If the Windows path survives, that parsing moves into Go with fuzz tests. |
| 19 | The symlink comment is wrong, and a bad download stays cached | **Fixed.** Comment corrected, and the file is deleted on a hash mismatch. |
| 20 | The unauthenticated relay exposes dockerd to containers and local users | **Accepted for the prototype only.** Noted in `init`. Finding 3 already requires the agent to replace it. |
| 21 | vmbus-dir detection is fragile | **Rejected.** QEMU has no vmbus device, so the directory only exists under Hyper-V. The agent will replace this script anyway. |
| 22 | "pivot_root impossible" overstated for A2a | **Fixed.** Now a trade-off (switch_root plus a copy). |
| 23 | Apache §4(a) vs §4(d); Go stdlib; certdata.txt | **Fixed.** |
| 24 | Should be 0003, since 0003/0004 don't exist | **Rejected.** The maintainer assigned 0005 for this track. |
| 25 | Default cache path names the AI tooling directory | **Fixed.** The default is now `%TEMP%\winbox-guest-build`. |

Strongest remaining arguments against the decision, accepted as risks:

- A2b is chosen without being measured.
- Nothing has run under HCS.
- Without discard on QEMU-WHPX, the fallback can't meet the memory targets regardless of the
  image layout.

## Consequences

**Easier**

- Upgrades swap two read-only artifacts and keep the data disk.
- Guest binaries are page cache, not pinned RAM.
- The kernel and userland come from one signed, pinned source with a verifiable chain.
- The initramfs (and later a per-boot config cpio) can be built by `winbox.exe` itself on
  Windows. The kernel accepts concatenated cpio archives. The prototype booted an initramfs
  with an overlay cpio appended (a wrapper `/init` plus a test binary), and the overlay's
  binary ran.

**Harder**

- Three artifacts and two disk devices per backend.
- A Linux build step (CI) for the system image.
- We must mirror Alpine packages and sources.
- First boot formats the data disk.
- The QEMU fallback can't meet the memory targets with stock QEMU, and needs a different
  transport than HCS.

**Follow-ups**

These are owned by the guest track unless noted.

1. Run the same initramfs under HCS. That needs a non-elevated path or explicit approval to
   run elevated (ADR 0002). Verify hv_storvsc, hv_sock, hv_balloon, and free page reporting
   end to end.
2. ~~Prototype A2b and measure it against A2a.~~ Done under HCS, see "A2b
   implementation": `Shmem` 178 → 0 MiB, first console output 0.65 → 0.08 s.
3. ~~Run the image build inside a QEMU-WHPX Alpine VM.~~ Superseded: the build runs
   `mkfs.erofs` in a container on winbox's own Docker Engine, in about 12 s.
4. Add iptables or nft userspace and turn Docker's firewalling back on. Measure the
   start-time delta.
5. Pin the Go toolchain (`toolchain` in `go.mod`) so initramfs bytes are reproducible
   across machines. Then check this by building from two different commits, and on CI.
6. Add GPL source entries to the lock (aports commit, kernel.org and BusyBox tarballs with
   SHA-256), and make the build fail when a GPL binary has no source entry.
7. (disk track) Measure the discard path on both backends: ext4 `discard` vs periodic
   `fstrim`, hv_storvsc UNMAP on a dynamic VHDX attached without admin, and virtio-blk
   discard on QEMU-WHPX. QEMU may share the missing-MADVISE problem for raw files.
8. Pin the erofs builder completely: an image by digest with erofs-utils installed, or the
   erofs-utils `.apk`s (with SHA-256) in `sources.lock`. Today it is a version pin.
9. Embed the image manifest's SHA-256 in release `winbox.exe` builds, so `image.json` stops
   being self-certifying.

**What would make us revisit**

- HCS can't attach the read-only disk from a non-elevated or broker setup.
- erofs page-cache behavior under HCS dynamic memory turns out worse than tmpfs.
- Alpine stops shipping `linux-virt` with the features above.
