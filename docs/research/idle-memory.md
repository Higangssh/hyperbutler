# Idle memory (REQ-MEM-001)

- Date: 2026-10-30
- Requirements: REQ-MEM-001 (idle host memory, median ≤ 400 MB for 0.1, ≤ 250 MB for 1.0),
  REQ-START-001 (must not regress), REQ-MEM-002 (the page-cache policy).
- Machine: Intel Core Ultra 7 155H, 32 GB, Windows 11 Home 10.0.26200, NTFS `C:`. The user
  is in Hyper-V Administrators; every command ran from a **non-elevated** PowerShell. Other
  sessions ran their own VMs on the same machine at the same time.
- Guest: Alpine `linux-virt` 6.18.58, Docker 29.9.2 (static), 8 vCPUs, 4096 MiB unless
  noted, the persistent data disk, user-mode networking.
- Code: `guest/prototype` (stage-1 initramfs, system disk, `init`), `guest/image` (VHD
  footer and image manifest), `guest/cmd/mksysdisk`, `internal/engine` (image
  verification, system disk attach, kernel command line), `cmd/hyperbutler` (finds
  `image.json`). Decisions are in [ADR 0005](../adr/0005-guest-image.md).

## Result

| | Before (initramfs root) | After (erofs system disk + tuning) |
|---|---|---|
| Engine (`hyperbutler.exe`) private WS, median | 14.8 MiB | 14.9 MiB |
| This VM's `vmmem` private WS, median | 514 MiB | 284 MiB |
| This VM's `vmwp.exe` (worker) private WS, median | 5.5 MiB | 5.5 MiB |
| **Total, median (max), 27–29 samples** | **534.3 MiB (534.5)** | **304.5 MiB (304.7)** |
| Guest `MemTotal` | 3924 MiB | 3989 MiB |
| Guest used (`MemTotal − MemFree − Buffers − Cached`) | 197 MiB | 122 MiB |
| Guest `Cached` (includes `Shmem`) | 195 MiB | 93 MiB |
| Guest `Shmem` | 178 MiB | 0.1 MiB |
| Guest `AnonPages` (of which `AnonHugePages`) | 96 MiB (86) | 41 MiB (0) |
| Guest `Slab` | 24 MiB | 26 MiB |
| Reserved at boot, outside `MemTotal` (4096 − `MemTotal`) | 172 MiB | 107 MiB |

REQ-MEM-001 for 0.1 (≤ 400 MB, which is ≤ 381 MiB) is **met**, with 77 MiB to spare. The
1.0 target (≤ 250 MB = 238 MiB) is not; see "What is left".

"Before" is `main` at 326f048 with its prototype image, re-measured with the same script.
`cli-engine.md` had 580 MiB, on a VM that had run containers earlier and without `vmwp`.

Guest process RSS after (anonymous / file): dockerd 16.5 / 19.8 MiB, containerd 11.3 /
28.3 MiB, hyperbutlerd 5.8 / 2.2 MiB, agent 1.2 / 2.2 MiB, netfwd 0.7 / 1.0 MiB. Before: dockerd
34.1 anonymous, containerd 30.1, hyperbutlerd 10.4.

## Method

As REQ-MEM-001 says, with nothing else added. The script is not in `bench/` yet; it should
become `bench/mem-idle`.

1. `hyperbutler start` (its own `HYPERBUTLER_HOME` and `--pipe`, `--no-context`), one `docker info`
   right after the API answers, then nothing.
2. From minute 5 to minute 10 after the API answered, every 10 s: the private working set
   (`WorkingSetPrivate` of `Win32_PerfRawData_PerfProc_Process`) of the engine, of this
   VM's `vmmem` and of its `vmwp.exe`. The result is the median of the per-sample sums.
3. Only after the window: a throwaway privileged container (`--pid=host`) reads the guest's
   `/proc/meminfo`, process RSS, THP mode and `buddyinfo`. The engine also logs the guest's
   own memory report (hyperbutlerd's status) once a minute, which covers the window itself.

Which processes are ours: a non-elevated user cannot read a `vmmem` owner (it runs as the
VM's virtual account). The script snapshots the `vmmem` processes before `hyperbutler start`
and takes the new one. If another session started a VM at the same moment, it takes the
one whose creation time is closest to the engine's start. `vmwp.exe` is that `vmmem`'s
parent. After `hyperbutler stop` the script checks that the `vmmem` is gone (it was, for the
final runs).

Caveats:

- **One run per configuration.** Run-to-run spread was not measured. Within the final run
  the total varied by 0.2 MiB.
- Runs M1–M5 below summed engine + `vmmem` only, and the first attribution code had a bug
  (an empty "before" list): a review finding. Their `vmmem` numbers match the engine logs
  and the guest reports, but treat them as the trail that led to the change, not as
  results. The two result columns above use the fixed script.
- The host side of the system disk (its pages in the Windows standby cache) is not in any
  private working set, as REQ-MEM-001 counts. Windows can reclaim it.

## Where the memory went, and what changed

One change at a time where it mattered (4096 MiB guest unless noted; engine + vmmem):

| Run | Change | Median (max) | What moved in the guest |
|---|---|---|---|
| Before | initramfs root (A2a) | 528.9 (529.3); 534.3 with vmwp | `Shmem` 178 MiB: dockerd, containerd and the rest unpacked in RAM |
| M1 | erofs system disk, `switch_root` (A2b) | 421.5 (445.3) | `Shmem` 178 → 0; the binaries become file cache (78–113 MiB, reclaimed and refaulted) |
| M2 | + `swiotlb=noforce`, THP `madvise`, cache threshold 16 MiB | 336.9 (365.0) | `MemTotal` +65 MiB (no bounce buffers); `AnonPages` 94 → 41 MiB, `AnonHugePages` 84 → 0 |
| M3 | M2 at `--memory 2GiB` | 275.2 (317.2) | smaller `mem_map` and boot-time hash tables |
| M4 | M2 + 16 KiB readahead on the system disk from boot | 313.2 (313.4) | file cache steady at 39 MiB instead of swinging 33–109 MiB |
| M5 | M2 + readahead lowered 10 s after boot | 332.1 (367) | no effect: see 4 |
| **M6** | **M2 + binaries pre-read, then 16 KiB before dockerd starts (shipped)** | **298.9**, 304.5 with vmwp | file cache steady at 42 MiB |

### 1. The root file system (A2b), −107 MiB

The prototype's root was the initramfs: 175 MB of `rootfs` that can never be reclaimed
(`Shmem` 178 MiB). Now the initramfs holds BusyBox, a stage-1 `/init` and five modules
(0.9 MB gzip). It mounts the erofs system disk read-only and `switch_root`s into it, which
frees the initramfs. dockerd, containerd, runc, the guest services, e2fsprogs and the
modules are file cache on a read-only disk: clean, reclaimable, and read only where
touched.

### 2. Bounce buffers, −64 MiB

`dmesg` showed `software IO TLB: mapped [mem 0xf4000000-0xf8000000] (64MB)`. A 4 GiB HCS VM
has RAM above 4 GiB of physical address space (the MMIO gap sits below 4 GiB), so x86 Linux
reserves a 64 MiB SWIOTLB pool. `memblock` zeroes it, so it is committed on the host at
boot. Nothing in this VM needs bounce buffering: the vmbus devices are not DMA-limited, and
this is not an isolated VM (where Linux forces SWIOTLB on its own). `swiotlb=noforce` is now
in the engine's default command line. `MemTotal` rose by 65 MiB.

### 3. Transparent huge pages, −50 MiB

linux-virt defaults to THP `always`. 86 of dockerd's and containerd's 96 MiB of anonymous
memory were huge pages, held at idle because a 2 MiB page is only split under pressure.
`init` sets `madvise`: programs that ask (`MADV_HUGEPAGE`) still get them, and
`hyperbutler.thp=always` on the kernel command line restores the default. dockerd's anonymous
RSS fell from 34 to 17 MiB, containerd's from 30 to 11 MiB.

### 4. Page cache and readahead, about −40 MiB and a flat line

With the binaries on disk, their pages are file cache, and hyperbutlerd's cache policy
(guest-services.md) reclaims file cache above keep + threshold after 10 s idle. With the
kernel's 128 KiB readahead, the idle daemons faulted about 70 MiB back within a minute of
every reclaim, because each random code fault pulls 128 KiB around it. That is the swing
between 33 and 113 MiB of file cache in M1 and M2. With 16 KiB the file cache stays at about
40 MiB (M4). But 16 KiB from boot slowed dockerd's start by about 0.3 s, since it reads
most of its binary then.

M5 tried 128 KiB at boot and 16 KiB 10 s later. It changed nothing: Linux copies a file's
readahead window when the file is opened, so the daemons kept 128 KiB (a review finding,
confirmed by M5's swing).

What ships (M6): `init` reads dockerd, containerd, the shim and runc once, sequentially,
with 128 KiB (about 0.2 s), sets 16 KiB, and only then starts dockerd. The start reads from
cache; idle refaults are small; hyperbutlerd trims what stays unused. `hyperbutler.sysra=<KiB>` sets
the small value. The cache threshold is now 16 MiB (it was 64 MiB; keep stays at 32 MiB;
`hyperbutler.cache=<MiB>`).

### 5. Guest memory size: considered, not changed

The memory hints (`EnableDeferredCommit`, hot and cold hints) already commit guest RAM only
when it is touched. So the configured size costs only what the kernel touches per GiB at
boot: `mem_map` (64 bytes per 4 KiB page, 16 MiB per GiB) and boot-time hash tables. 2048
instead of 4096 MiB saved 62 MiB (M3).

The default stays 4096 MiB. Halving what containers can use to save 62 MiB is the wrong
trade at 0.1, and the 0.1 target is met without it. `--memory 2GiB` is there for users who
want it. Booting small and hot-adding memory through hv_balloon (`CONFIG_MEMORY_HOTPLUG=y`)
would get both; that is an open item.

### 6. zswap and zram: not used

They only help when memory is under pressure or something reclaims anonymous memory. At
idle there are 41 MiB of anonymous pages in all, and nothing pushes them out. Adding
proactive anonymous reclaim to hyperbutlerd would trade idle CPU (REQ-PWR-002) for at most
about 25 MiB. linux-virt has `ZSWAP=y` (off by default) and `ZRAM=m`. Revisit them for
REQ-MEM-002 or the 1.0 target.

### What the rest is

Of the 304.5 MiB:

- the engine: 15 MiB;
- `vmwp`: 5.5 MiB;
- 107 MiB reserved before `MemTotal` is counted: the kernel image, `mem_map` (64 MiB) and
  hash tables;
- guest-used memory: 122 MiB. That is the daemons' 41 MiB of anonymous memory, 26 MiB of
  slab, kernel stacks and page tables, plus about 40 MiB of file cache that `Mapped`
  counts while `Cached` is low.

Free pages go back through hv_balloon's free page reporting in 2 MiB blocks
(`page_reporting_order` 9; since 2026-10-31 single pages, see mem-reclaim.md). `buddyinfo` shows almost all free memory in order-9 and
order-10 blocks.

## Start time (REQ-START-001)

`hyperbutler start` → `docker info` OK → `hyperbutler stop`, 21 runs, the first one discarded, warm
host cache, existing data disk, 4096 MiB, 8 vCPUs:

| Image | Median | p95 | min / max |
|---|---|---|---|
| Before (`cli-engine.md`) | 1.95 s | 1.96 s | 1.94 / 1.97 |
| erofs, 16 KiB readahead from boot (M4 image) | 1.94 s | 2.05 s | 1.83 / 2.06 |
| erofs, readahead lowered after boot (M5 image) | 1.25 s | 1.45 s | 1.23 / 1.94 |
| **Shipped (M6 image)** | **1.44 s** | **1.48 s** | 1.13 / 2.41 |

Boot phases, in guest uptime, from one boot of the shipped image:

- first console output: 0.08 s (it was 0.65 s, with a 66 MB initramfs to unpack);
- system disk mounted: 0.12 s;
- data disk mounted: 0.30 s;
- hyperbutlerd ready: about 0.38 s;
- binaries pre-read: 0.45 s;
- dockerd socket: 0.47 s.

The host saw `/_ping` at 1.23 s. Part of the gain is the engine's guestctl dial timeout,
cut from 1 s to 400 ms. hyperbutlerd now listens about 0.3 s into the boot, but a connect to a
port nobody listens on yet waits for the whole timeout. With 1 s, the first successful dial
came at about 1 s, and dockerd waits for that dial because it waits for hyperbutlerd's first
budget.

Smoke checks on the shipped image all passed:

- `docker run --rm hello-world` printed "Hello from Docker!".
- `docker run -d -p 8081:80 nginx:alpine` answered 200 on `127.0.0.1:8081`.
- After `hyperbutler stop` and `start`, the images and the stopped container were still there,
  and the container started again and answered.
- dockerd runs without `DOCKER_RAMDISK`, so runc uses `pivot_root`: the root is a real
  (erofs) mount.

## What is left

- **The 1.0 target (238 MiB).** Candidates, roughly by size:
  - booting small and hot-adding memory (M3 shows −62 MiB for 4 → 2 GiB);
  - smaller boot-time hash tables (`dhash_entries`, `ihash_entries`, `thash_entries`);
  - a smaller guest kernel (a B2 kernel without what this VM never uses);
  - dockerd's and containerd's idle heaps (`GOGC` or `GOMEMLIMIT` for the daemons);
  - zswap with proactive reclaim.
- **Spread across runs** was not measured (one run per configuration).
- **REQ-MEM-002** was run on 2026-10-31 ([mem-reclaim.md](mem-reclaim.md)). It changed the
  kernel command line: free pages are now reported in single pages, and the dentry and inode
  hash tables are fixed at their 4 GiB sizes.
- **The first start after a host reboot** was not measured. That start reads `system.vhd`
  and hashes 105 MB with a cold host cache, and it needs a reboot.
- **`bench/mem-idle` and `bench/start`** do not exist yet. The scripts used here should
  become them.
