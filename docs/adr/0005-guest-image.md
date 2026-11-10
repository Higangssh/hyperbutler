# 0005. Guest image: kernel, root file system, and how we build it

- Status: proposed
- Date: 2026-10-30
- Requirements: REQ-START-001, REQ-MEM-001, REQ-MEM-002, REQ-DISK-001, REQ-DISK-008,
  REQ-PLAT-002, REQ-PLAT-005, REQ-UX-002, REQ-API-001

## Context

hyperbutler boots one Linux guest that runs dockerd. The guest image is the kernel, whatever it
boots into, and the disks it mounts. It has to:

- Boot under HCS (primary, [ADR 0002](0002-primary-vm-backend.md)) and under QEMU-WHPX (the
  fallback) from the same artifacts.
- Reach "Docker API ready" well inside REQ-START-001 (median ≤ 8 s for 0.1, ≤ 5 s for 1.0).
  That budget also has to cover the host side.
- Leave room for REQ-MEM-001 (idle host memory ≤ 400 MB for 0.1, ≤ 250 MB for 1.0). Anything
  the guest keeps resident counts against it.
- Be built and published without WSL (REQ-PLAT-002 applies to contributors too). The
  maintainer's machine is Windows 11 Home with WSL disabled.
- Be reproducible, and meet the licenses of what it ships. hyperbutler is Apache-2.0, but the
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

- REQ-START-001 asks for 20 runs with a warm-up run discarded, measured from `hyperbutler start`
  to `docker info`. Here it's 11 runs with no discard, measured from the VMM launch to
  `/_ping`.
- Before the harness fix (Findings 5), the same image measured ≈ 2.9 s.
- While other sessions loaded the machine, single runs went up to 9 s. Their guest-side marks
  were slow too, so the cause was host contention, not the image.

**Where the time goes**

The kernel phases use printk timestamps. The `/init` and dockerd phases use the `hyperbutler-t`
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
The requirement sums Private Working Set from performance counters over hyperbutler.exe, the VMM,
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
   launch the VMM the way `hyperbutler.exe` will.

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
   (the ELF files carry glibc's GNU/Linux ABI note). **Done 2026-10-31:** the image no
   longer uses it; see "Docker Engine from Alpine" for the options weighed and the
   measurements.
4. **Build and publish:** the canonical build runs on **GitHub Actions Linux runners**,
   inside an `alpine:3.24` container pinned by digest, with every `.apk` pinned by SHA-256
   and its signature chain verified by `apk`.
   It produces the kernel, initramfs, and erofs image, plus a GPL source bundle. All of these
   go to GitHub Releases as assets (never git), with SHA-256 sums and build provenance
   attestations. `hyperbutler.exe` embeds the expected hashes (since 2026-11-01: the
   SHA-256 of `image.json`, which carries the others; see "A2b implementation").

   The agent sandbox image (ADR 0009) follows the same rule by another path: CI only, its
   own pinned container and lock (Debian, `guest/sandbox/debian.lock`), and its own source
   bundle, from `.github/workflows/sandbox-image.yml`. It boots this image's kernel and
   stage 1, and takes this build's `kernel-modules.tar`.

   Two paths for contributors without WSL:
   - The initramfs can be rebuilt natively on Windows. That works today, with `guest/cmd/mkinitramfs`.
   - The erofs image is built by `mkfs.erofs` in a pinned Alpine container on any Docker
     Engine, hyperbutler's own included (see "A2b implementation"). No QEMU VM is needed.
5. **Kernel command line:** keep the serial console and no `quiet` (needed to debug boots).
   Revisit log volume once boot time is measured on HCS.

### Boot-time budget

REQ-START-001 runs from `hyperbutler start` until `docker info` succeeds: median ≤ 8 s (0.1) and
≤ 5 s (1.0). We budget against the 1.0 number, so 0.1 has a wide margin. `bench/start`
checks each line separately, using the `hyperbutler-t` marks.

| Phase | Budget | QEMU-WHPX, A2a (measured) | HCS reference (ADR 0002 log) |
|---|---|---|---|
| VMM create and start, firmware, kernel load | 0.4 s | ≈ 0.3 s | 0.16 s create+start, first serial at 0.31 s |
| Kernel init up to the initramfs | 0.5 s | 0.45 s | ≈ 0.33 s |
| Initramfs unpack | 0.1 s | **≈ 0.95 s** (over budget, hence A2b) | 0.09 s for Alpine's 9.4 MB initramfs |
| `/init` or agent: modules, mounts, system disk, network | 0.3 s | 0.08 s (no disks yet) | not measured |
| dockerd to "API listen" | 1.0 s | 0.55 s | not measured |
| Host: `hyperbutler.exe`, transport, the `docker info` round trip | 0.7 s | not measured (the harness pings `/_ping` over slirp) | not measured |
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

The image is a "mere aggregation" with hyperbutler's Apache-2.0 code. GPL obligations attach to
the GPL components, not to hyperbutler. The release pipeline must generate a `THIRD-PARTY.md`, the
license texts, and the source bundle from the lock file, so nothing ships without them.
Today's `sources.lock` records only binary packages. It must gain a pinned source entry
(aports commit, kernel.org and BusyBox tarball hashes) for every GPL binary, plus a check that
fails the build when one is missing (Follow-up 6).

## A2b implementation (2026-10-30)

Decision 1 is now built and measured under HCS. Numbers and method are in
[docs/research/idle-memory.md](../research/idle-memory.md).

**Layout.** One image directory holds four files, all release artifacts (never git). In the
release zip it is the `image\` folder next to `hyperbutler.exe`, which `hyperbutler start`
uses without `--image-dir` (2026-11-02: not the exe's own folder, where Explorer shows
`system.vhd` as a disk image and a double-click offers to mount it):

| File | What | Size |
|---|---|---|
| `vmlinuz-virt` | the pinned Alpine kernel, unchanged | 12.6 MB |
| `initramfs-stage1.gz` | stage 1 (a new name, so an older `hyperbutler.exe` that knows only `initramfs-hyperbutler.gz` fails with "file not found" instead of booting a stage 1 without its system disk): BusyBox, `initramfs-init`, the modules to reach the disk (`modules-boot.txt`: hv_storvsc, sd_mod, virtio_blk, erofs) | 0.9 MB |
| `system.vhd` | a fixed VHD (raw erofs, lz4hc, padded to whole MiB, plus the 512-byte footer) holding everything else: `init`, BusyBox and its applet links, dockerd, containerd, runc, the guest services, e2fsprogs, the CA bundle, all modules | 88 MiB |
| `image.json` | the manifest: name, size and SHA-256 of the three files | 0.5 KB |

**Boot.** Stage 1 loads its modules, finds the system disk by SCSI LUN
(`hyperbutler.system=lun:1`, or a device name such as `vda` on QEMU), mounts it as erofs
read-only and `switch_root`s into it, which deletes the initramfs. The root is then a
real, read-only mount: runc uses `pivot_root` and `DOCKER_RAMDISK` is gone. What must be
writable is a mount (`/run`, `/tmp`, `/dev/shm` as tmpfs, `/var/lib/docker` on the data
disk) or a link into one (`/etc/resolv.conf` → `/run/resolv.conf`). The data disk is now
`hyperbutler.data=lun:0` as well: the kernel named the system disk `sda` in some boots and
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
directory can also replace `hyperbutler.exe`. The release build embeds the manifest's
SHA-256 in `hyperbutler.exe` (Decision 4) so a replaced image is refused even with a matching
`image.json`. **Done 2026-11-01:** `dist/package.ps1` links the SHA-256 of the guest-image
job's `image.json` in with `-X ...imageset.pinnedManifest`; the engine holds `image.json`
deny-write like the other files, hashes the bytes it parses, and refuses a mismatch with
the file, both folders and the fix (extract the zip again). It hands HCS the final path of
each held file, so a junction on the way cannot be retargeted after the check.
Development builds have no pin, keep the manifest self-check and log that once per start;
`hyperbutler version --json` reports `imagePinned` and `imageManifestSha256`. Review:
docs/release.md, "Review of the image pin and the bundled clients".

**Update path.**

- An update replaces the four files and writes `image.json` last (`build.ps1` does).
- While hyperbutler runs, the files cannot be overwritten, renamed or deleted. An updater must
  stop hyperbutler first, and it gets a sharing violation instead of a torn image if it does
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
| A2 medium | The memory script omitted `vmwp.exe` (the VM worker), and a bug left the "new vmmem" filter empty, so attribution rested on creation time alone. | **Fixed.** The script snapshots `vmmem` before the start, takes `vmwp` as its parent, sums engine + vmmem + vmwp, and checks that the `vmmem` is gone after `hyperbutler stop`. Before and after re-measured with it. |
| A3 medium | Start-time numbers had no saved log; REQ-START-001's first start after a reboot is not reported. | **Fixed:** the runs are logged and the doc records the run set, the phases and the dial-timeout cause (raw logs stay out of git); **cold start rejected for now:** it needs a host reboot, which this session must not do. Listed under Open. |
| A4 medium | `apk add erofs-utils=1.9.1-r0` is not pinned by hash, and Alpine drops old revisions. | **Accepted as a known gap.** It fails closed (the build stops when the revision disappears; it cannot silently change), apk checks the signature, and two clean builds were byte-identical. Follow-up 8: a builder image pinned by digest with erofs-utils in it, or the `.apk`s in `sources.lock`. |
| A5 medium | An older `hyperbutler.exe` with a new image directory would boot stage 1 without `hyperbutler.system=` and only time out. | **Fixed.** Stage 1 is now `initramfs-stage1.gz`, a name older engines do not look for, so they fail at once with "not found". Test: `TestGuestArtifacts`. |
| A6 low | No committed fuzz corpus; no test for `guestArtifacts`; LUN resolution and reparse rejection untested. | **Fixed** for the corpus (`guest/image/testdata/fuzz`) and `guestArtifacts`. **Rejected** for the shell LUN logic and reparse points: the shell is exercised on every boot (and the probes show the right disk at `/` and `/var/lib/docker` even when the kernel swapped `sda` and `sdb`), and creating a symlink needs Developer Mode or admin here. |
| A7 low | The manifest is not a trust anchor; VM-account ACEs pile up on `system.vhd` after crashes. | **Accepted.** Both are stated above (the same ACE issue exists on `data.vhdx`). Open item for the ACEs; the hash in `hyperbutler.exe` was done 2026-11-01. |
| A8 low | The research doc still had placeholders. | **Fixed.** |

The reviewer also checked and found fine: manifest and footer parsing (no traversal,
geometry re-derived), `openDenyWrite` sharing and reparse handling, the `ReadOnly`
attachment and LUN mapping, that the blank-disk check can never target the system disk,
the `hyperbutler.*` argument validation, the read-only root's writable paths, `swiotlb=noforce`
on non-isolated Hyper-V, THP `madvise`, the 400 ms dial timeout, and the dm-verity
rejection.

**Old images still work.** A directory without `image.json` is an initramfs-only image:
the engine boots it as before, with `hyperbutler.data=sda`. The QEMU harness (`run-qemu.ps1`)
now attaches `system.vhd` as `vda`. This was not run: QEMU was not part of this work.

## Docker Engine from Alpine (2026-10-31)

Until now `dockerd`, `containerd`, the shim, `runc`, `docker-proxy` and `docker-init` came
from Docker's static tarball (`download.docker.com/linux/static/stable/x86_64/docker-29.9.2.tgz`).
Docker publishes no checksum, signature or provenance for those tarballs (the directory
listing has only `.tgz` files), so the pin was trust on first use over HTTPS, and strict
release mode refused it: no release was possible by design. Everything else already came
from signed Alpine packages.

### Options

**(a) Alpine's packages, from the v3.24 release the image already pins.** `docker-engine`
(dockerd, docker-proxy, and `docker-init` as a symlink to tini-static), `containerd`
(containerd and the runc shim), `runc`, `tini-static`, and `libseccomp` (main) for runc.

- *Verification:* the same chain as every other input. Each `.apk` is signed with the
  Alpine key that the digest-pinned `alpine:3.24.3` image carries in `/etc/apk/keys`, and
  its signed control segment hashes the data segment. `fetch-verify.sh` checks it and
  that the signed `.PKGINFO` names the pinned file.
- *Source:* the signed `.PKGINFO` names the aports commit, so `fetch-sources.sh` bundles
  the APKBUILD and the upstream tarballs (moby and cli for `docker`, containerd, runc,
  tini, libseccomp) checked against the APKBUILD's sha512sums, with no new code. The
  LICENSE and NOTICE files of Moby, containerd and runc come out of those tarballs into
  `THIRD-PARTY/guest/`, which the static tarball could never provide.
- *Versions* (checked with `go version -m` on the binaries):

  | Component | Static tarball (before) | Alpine v3.24 (after) |
  |---|---|---|
  | dockerd, docker-proxy | 29.9.2 (moby `0367cbe`), Go 1.26.10 | 29.9.2-r0, the same moby commit `0367cbeb49b9`, Go 1.26.9 |
  | containerd, shim | v2.4.2 | 2.3.7-r0 |
  | runc | v1.5.2 | 1.4.3-r2 |
  | tini (docker-init) | 0.19.0, static glibc | 0.19.0-r3, static musl |

  The engine is the same release from the same commit. containerd and runc are one minor
  behind: Alpine keeps a stable branch on a supported upstream series and backports
  security releases (the APKBUILDs' `secfixes` lists them); Docker's tarball bundles the
  newest. containerd 2.3.7 is the newest 2.3 patch release and runc 1.4.3 the newest 1.4
  one (checked against the upstream release lists on 2026-10-31), and Alpine's
  `secfixes` list the CVEs fixed in them (runc 1.4.3: CVE-2026-41579). What the newer
  minors add and these lack: runc 1.5.2's workaround for a kernel bug that crashes runc
  at random under cgroup v2 (a bug fix, no CVE). Docker supports containerd 2.x under
  29.x, and the e2e run below exercises the pair. A stable branch also never jumps a
  minor under us, which is what a pinned image wants.
- *Linkage:* musl, dynamic, PIE (`-buildmode=pie`, `CGO_ENABLED=1`). The `so:` dependencies
  in the signed metadata are `libc.musl-x86_64.so.1` for all of them and
  `libseccomp.so.2` for runc. musl's loader is already in the image for e2fsprogs. So the
  engine's only new library is libseccomp (157 KiB, LGPL-2.1, dynamically linked, so it
  stays replaceable, as the licensing table asks). Of the packages' other dependencies,
  `ca-certificates` and `/bin/sh` are already in the lock (the CA bundle, BusyBox) and
  `iptables` is added (below). The image takes the files it names in `system.spec`,
  never a dependency closure.
- *Size:* the six binaries are 167.0 MB unpacked against 168.1 MB (dockerd 99.7 vs
  102.9 MB, containerd 43.3 vs 39.5, shim 9.6 vs 8.1, runc 11.4 vs 14.1, docker-proxy 2.8
  vs 2.7, tini 0.06 vs 0.74), plus libseccomp and the iptables files (about 0.5 MB).
  The system disk after lz4hc: see below.
- *Costs:* the PIE binaries relocate their read-only data at load, which dirties a few
  MiB of private pages per process; and the engine follows Alpine's update cadence, not
  Docker's. Both are measured or bounded below.

**(b) Build moby, containerd and runc from source in CI, from pinned commits.**
Provenance would be ours end to end (commit hashes, `go.sum`, a pinned Go), and the Go
parts can be built reproducibly with `-trimpath`. But runc needs cgo and libseccomp, and
moby's supported build is its own Dockerfile and buildx bake; doing it in the Alpine
container means writing and maintaining what aports already does (tags, patches,
`secfixes`), for three projects, on every security release. It adds roughly 10 to 15
minutes of CI per image build, and a CVE fix would wait for us instead of for Alpine.
The GPL bundle would still need Alpine's sources for libseccomp. Kept as the fallback if
Alpine fails the revisit rule below.

**(c) Keep the static tarball and verify it some other way.** There is nothing to verify
it against: no `.sha256`, `.asc` or attestation beside the tarballs, and the GitHub
releases of moby carry no binaries. Docker's apt and yum repositories are signed (the
`InRelease` file with Docker's release key, then package hashes), so `docker-ce` and
`containerd.io` `.deb`s are verifiable. But they are glibc builds: the image would gain
glibc and its loader (and owe glibc's LGPL source), a second signature scheme in CI
(`gpgv` and Debian metadata), and their source is not offered per package the way aports
is. That is more work than (a) for a weaker source story. Rejected.

### Decision

**(a).** It removes the only unsigned input, so strict mode now accepts the lock and runs
on every CI build, not just on a tag. It needs no new trust root, no new verification
code and no new source-bundle code, and it brings the Apache NOTICE files the release
owes. Revisit, and move the engine to (b), if Alpine's `docker`, `containerd` or `runc`
package lags an upstream security release by more than 7 days (the same rule as the
kernel, Decision 2), or if Alpine drops a component.

The price is about 15 MiB of idle memory (PIE relocations, see Measurements). That is a
regression against the A2b numbers, accepted here for the release blocker it removes;
building dockerd and containerd non-PIE from the same pinned Alpine sources is the way
to take it back (Follow-up 10).

### iptables, for name resolution on user networks

Testing the change showed that containers on a user network could not resolve each other
by name. This is not new (the static-tarball image had no iptables either): dockerd runs with `--iptables=false`, but its embedded DNS
server (127.0.0.11) still needs a DNAT and an SNAT rule for port 53 in each container's
network namespace, which it installs with `iptables`, and the image had none ("Resolver
Start failed ... iptables not found"). That is why `docker-engine` depends on `iptables`.
The image now has Alpine's `iptables-1.8.13-r0` (nft backend) with `libxtables`,
`libnftnl` and `libmnl`, signed and pinned like the rest: `xtables-nft-multi`, its
command-name links, and only the four extensions those rules load (`standard`, `tcp`,
`udp`, `NAT` with its `DNAT`/`SNAT` aliases). The package's full extension set cannot be
unpacked on NTFS anyway: nine pairs such as `libxt_MARK.so` and `libxt_mark.so` differ
only in case. `init` loads `nf_tables`, `nft_compat`, `nft_chain_nat`, `nft_nat`, `xt_nat`
and `xt_tcpudp` (added to `modules.txt`); the image has no `modules.alias` for the kernel
to autoload them. The guest's own tables stay empty: Docker's firewalling is still off
(Follow-up 4). iptables and libnftnl are GPL-2.0-or-later and libmnl LGPL-2.1-or-later;
their sources go into the bundle like every other origin.

### Implementation

- `sources.lock` pins `docker-engine-29.9.2-r0`, `containerd-2.3.7-r0`, `runc-1.4.3-r2`,
  `tini-static-0.19.0-r3`, `libseccomp-2.6.1-r0`, `iptables-1.8.13-r0`,
  `libxtables-1.8.13-r0`, `libnftnl-1.3.1-r0` and `libmnl-1.0.5-r2`; the `docker-static`
  entry is gone. `system.spec` installs the engine files under their old paths
  (`/usr/bin/docker-init` is tini-static itself) and the libraries under their sonames.
  `init` gains only the netfilter `modprobe` above.
- `linux-virt` moved from 6.18.58-r0 to 6.18.59-r0: Alpine had dropped 6.18.58-r0 from
  its mirrors, so a clean build could not fetch the old pin (the case the release's
  input mirror exists for). On 2026-11-03 it moved again, to 6.18.60-r0 (upstream
  2026-10-30, in v3.24), before v0.1.0: stable point releases carry security fixes, and
  the old pin would be dropped next. On 2026-11-10, after v0.2.0, Alpine dropped
  6.18.60-r0 and the CI guest build failed with a 404, so it moved to 6.18.62-r0. Released
  images still rebuild from the inputs their release publishes.
- `build.ps1` now checks signatures too, on every build: after the SHA-256 pins it streams
  the lock, the pinned files and `fetch-verify.sh` into the digest-pinned Alpine container
  (`--network=none`), which runs the CI check (`apk verify`, `.PKGINFO` name) and returns
  `guest-inputs.tsv`. `-Strict` refuses a non-`.apk` input there, as CI does. Each file is
  hashed again just before it is unpacked.
- `fetch-verify.sh` accepts a package only from the verifying container's Alpine branch
  and architecture (`/alpine/v3.24/{main,community}/x86_64/`, and `.PKGINFO arch`): the
  same key signs every branch, so a signature alone would also accept an edge package.
- The CI build (`guest-image.yml`) builds the A2b layout now: the old
  `build-prototype.sh` still produced the A2a initramfs and had not matched
  `initramfs.spec` since the A2b change. `build-system.sh` is the Linux port of
  `build.ps1`, and both run the same `mkfs-erofs.sh`; `ci-entry.sh` installs
  `erofs-utils=1.9.1-r0`, the version `build.ps1` pins. Strict mode is the workflow's
  default and `ci.yml` sets it.
- `dist/package.ps1` packs the A2b files and fails on a `guest-inputs.tsv` line it cannot
  parse (it used to skip it, which skipped the signature check for that line).

### Measurements

Same machine and method as "A2b implementation" (HCS, non-elevated, default `hyperbutler
start`). Another agent was running VMs on this machine all along; the bench tools excluded
runs that overlapped one. "Before" is the image built from `main` (static tarball,
linux-virt 6.18.58), "after" this change (Alpine engine and iptables, linux-virt 6.18.59).

| | Before | After |
|---|---|---|
| `system.vhd` | 92,275,200 B (88 MiB) | 91,226,624 B (87 MiB) |
| `initramfs-stage1.gz` | 899,917 B | 900,005 B |
| Start, `bench/start` (n = 20, 1 warm-up) | median 1.44 s, p95 1.46 s (2 overlapped runs excluded) | median **1.14 s**, p95 1.25 s |
| Engine-reported ready | 1220–1236 ms in 17 of 20 runs | 911–1079 ms |
| Idle total, `bench/mem-idle` (min 5–10 median) | 315 MiB (earlier valid run); six runs today all overlapped another VM: 301–325 MiB | 323.5 MiB, one valid run (three later runs overlapped: 325–349 MiB) |
| Release zip (`package.ps1 -Snapshot`) | | 84.9 MB (89,026,173 B), 113.1 MB unpacked |

The start gain is mostly the engine's ready time; the guest marks (`dockerd-exec` →
`docker-sock`) moved by less than 0.05 s, so its cause is not isolated and should not be
counted on.

**Memory goes up by about 15 MiB, and the cause is known.** The host numbers vary by
±12 MiB between boots, so they were checked inside the guest instead: two boots of each
image, 90 s after start, no containers.

| Guest, after 90 s idle | Before (two boots) | After (two boots) |
|---|---|---|
| `AnonPages` | 43.8, 43.6 MiB | 59.5, 58.3 MiB |
| dockerd `Private_Dirty` | 17.5, 17.2 MiB | 26.8, 26.3 MiB |
| containerd `Private_Dirty` | 11.4, 10.7 MiB | 14.7, 14.4 MiB |
| netfilter modules | 0 | 0.8 MiB |
| `MemFree` | 3766, 3770 MiB | 3756, 3749 MiB |

Alpine builds Go programs as PIE. A PIE carries `.data.rel.ro` (Go's type data, itabs and
pointers in read-only data) that the loader relocates at start, which turns those pages
into private dirty anonymous memory: 11.0 MiB in dockerd and 4.7 MiB in containerd
(`debug/elf`), against none in the static non-PIE builds. That matches the measured
+9.2 and +3.5 MiB. Each container's shim adds its own, smaller share. It is not
reclaimable without swap.

So option (a) costs about 15 MiB of idle memory (about 5% of the total; still within
REQ-MEM-001's 400 MB for 0.1, and it widens the gap to 1.0's 250 MB). Ways to win it back,
not done here:

- Build dockerd and containerd as non-PIE from the same Alpine sources (the aports
  commit and the distfiles the source bundle already holds), in CI. This is option (b)
  for two binaries only, with the source and its sha512 pins taken from the signed
  `.PKGINFO`'s commit.
- Ask Alpine for a non-PIE Go build (unlikely: PIE is the distribution's policy).

### Verification (2026-10-31)

- **Reproducible:** two `build.ps1 -Strict` runs from empty caches (fresh downloads, the
  builder image removed in between) gave byte-identical `vmlinuz-virt`,
  `initramfs-stage1.gz`, `system.vhd`, `image.json` and `guest-inputs.tsv`. The CI path
  (`ci-entry.sh` with `build-system.sh`, strict, in `alpine:3.24.3` on hyperbutler's own
  engine, Go 1.27.0 linux) produced the same five files byte for byte, so the Windows and
  the release build agree. A strict CI run with the source bundle also passed: 19
  inputs, all signed; licence texts for every origin, including LICENSE and NOTICE of
  moby, cli, containerd and runc; distfiles for moby, cli, containerd, runc, tini,
  libseccomp, iptables, libnftnl and libmnl. A non-strict local build gives the same image.
- **The checks fail closed:** fetch-verify.sh, in the pinned image, refused a
  signature-stripped `.apk`, a package re-signed with another key, a SHA-256 mismatch, a
  signed package under another package's file name, a non-apk input under strict, an
  http URL, and packages from `edge` and from `testing`. `package.ps1` refused a
  malformed `guest-inputs.tsv` line, with `-Snapshot` too.
- **End to end** on the new image (hyperbutler start, docker CLI over the pipe): `docker
  version` reports 29.9.2 with containerd v2.3.7, runc 1.4.3, docker-init 0.19.0;
  hello-world; a `-v C:\...` bind mount read and written from both sides; `-p` nginx
  answering 200; `docker logs`; `docker exec`; a two-stage `docker build`; two containers
  on a user network resolving each other by name (after the iptables addition); seccomp
  mode 2 in containers; `--init` runs docker-init. The data disk written by the old
  engine was reused without trouble.

### Review of this change

The adversarial reviewer got the diff and the requirement, downloaded the nine new
packages, checked every pin, signature key, file name, origin and commit, and the paths
`system.spec` takes from them. It found no way for an unverified byte to reach the image
or a release. Disposition:

| # | Finding | Disposition |
|---|---|---|
| R1 medium | This section still said libseccomp was the only new library and `init` was unchanged after iptables was added. | **Fixed.** |
| R2 medium | NOTICE and the release checklist omitted iptables, libnftnl (GPL) and libmnl (LGPL). The generated SOURCE-OFFER and THIRD-PARTY were already right. | **Fixed.** |
| R3 low | fetch-verify.sh accepted any Alpine branch and arch, and one key signs them all. | **Fixed:** the URL must be the verifying image's `/alpine/v3.24/{main,community}/x86_64/`, and `.PKGINFO arch` that arch or `noarch`. Tested with edge and testing URLs. |
| R4 low | No test for a foreign-signed package or a data-segment change. | **Partly fixed:** a package re-signed with another key is refused (run by hand). A data-segment change with the pin updated was not built: its gzip CRC or the signed datahash fails, and the pin makes it moot. CI does not run these yet (Follow-up 11). |
| R5 low | No automated test of package.ps1's and build.ps1's manifest parsing. | **Accepted.** A malformed line was checked by hand (refused, with `-Snapshot` too); Pester is not set up in this repository. |
| R6 low | build.ps1 does not check the Go version, so "same bytes as CI" holds only by luck. | **Fixed:** it prints the version and warns when it is not the go.mod one. Pinning `toolchain` stays Follow-up 5. |

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
| 25 | Default cache path names the AI tooling directory | **Fixed.** The default is now `%TEMP%\hyperbutler-guest-build`. |

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
- The initramfs (and later a per-boot config cpio) can be built by `hyperbutler.exe` itself on
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
   `mkfs.erofs` in a container on hyperbutler's own Docker Engine, in about 12 s.
4. Turn Docker's firewalling back on (iptables-nft userspace is in the image since
   2026-10-31, used only for the embedded DNS rules). Measure the start-time delta.
5. Pin the Go toolchain (`toolchain` in `go.mod`) so initramfs bytes are reproducible
   across machines. Then check this by building from two different commits, and on CI.
6. Add GPL source entries to the lock (aports commit, kernel.org and BusyBox tarballs with
   SHA-256), and make the build fail when a GPL binary has no source entry.
7. (disk track) Measure the discard path on both backends: ext4 `discard` vs periodic
   `fstrim`, hv_storvsc UNMAP on a dynamic VHDX attached without admin, and virtio-blk
   discard on QEMU-WHPX. QEMU may share the missing-MADVISE problem for raw files.
8. Pin the erofs builder completely: an image by digest with erofs-utils installed, or the
   erofs-utils `.apk`s (with SHA-256) in `sources.lock`. Today it is a version pin.
9. Embed the image manifest's SHA-256 in release `hyperbutler.exe` builds, so `image.json` stops
   being self-certifying.
10. **Required before 1.0.** Win back the ~15 MiB that Alpine's PIE dockerd and
    containerd cost at idle: build those two non-PIE in CI from the aports commit and
    distfiles of the signed packages. The maintainer accepted the regression for 0.1 only
    (2026-10-31).
11. Run `scripts/guest-image` negative tests (unsigned, foreign-signed, wrong branch,
    renamed, pin mismatch) in CI; today they were run by hand (see Verification).
**What would make us revisit**

- HCS can't attach the read-only disk from a non-elevated or broker setup.
- erofs page-cache behavior under HCS dynamic memory turns out worse than tmpfs.
- Alpine stops shipping `linux-virt` with the features above.
