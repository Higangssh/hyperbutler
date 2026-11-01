# Returning freed guest disk space to the host without admin

Spike for [REQ-DISK-001](../requirements.md) (freed space goes back to the host automatically)
and REQ-DISK-002 (no admin rights, no Hyper-V tools). Code: [`spike/vhdx/`](../../spike/vhdx/).
It is throwaway code.

**Short answer:** yes, but not with any disk format or VMM option that ships today.

- **QEMU-WHPX is the only backend this machine runs from a plain standard-user token.** HCS
  fails with `HCS_E_ACCESS_DENIED` for a standard user. ADR 0002 leaves open whether Hyper-V
  Administrators membership is enough without elevation. A SYSTEM broker, as WSL uses, is still open there. It would fit REQ-PLAT-003 if
  installed once by an admin.
- **QEMU's Windows file driver drops guest discards.**
- **What worked is a sparse raw image served by our own process.** In the spike that is an NBD
  server, which turns each discard into `FSCTL_SET_ZERO_DATA`. With it, host allocation fell
  from 1026 MiB to 1–2 MiB within 0.2 s of `fstrim`, without admin and with the VM running.
- **That is a best case.** One contiguous 1 GiB file on a freshly formatted FAT32 gives
  `fstrim` a few huge, aligned extents to discard. ext4 after real churn has not been measured.

## Setup

- Windows 11 Home 26200, NTFS `C:` with 4 KiB clusters, Go 1.27.
- Every command ran from a non-elevated, medium-integrity token (`BUILTIN\Administrators` is
  deny-only).
- QEMU 11.1.0 with `-accel whpx -m 1024 -smp 2`.
- Alpine `vmlinuz-virt` 6.18.58-0-virt and the stock `initramfs-virt`. The spike appends a
  second cpio with its own `/wbinit` script, so no extra Alpine packages were needed.
- **Allocation is measured with `GetCompressedFileSizeW`.** That counts the clusters actually
  allocated for a sparse file. The table also shows `FILE_ATTRIBUTE_SPARSE_FILE`.
- **The guest filesystem is FAT32.** `initramfs-virt` has `mkfs.vfat` but no `mke2fs`, and no
  ext4 module either: it is in neither `modules.builtin` nor the module list. Every layer being
  measured sits below the filesystem: the block-device discard, the VMM block layer, and the
  host file. Linux has supported `FITRIM` on FAT since 4.19.
- **What FAT32 does not show:**
  - FAT's `FITRIM` trims *all* free space on every call. The log shows the same
    2,143,289,344 bytes each time.
  - The file being deleted is contiguous.
  - So the guest sends 1 discard per `fstrim` (3 trims per run, counting `blkdiscard`, which
    the kernel splits at `discard_max_bytes`).
  - ext4 after churn sends many small, unaligned extents. Q4a shows that anything under 64 KiB,
    or misaligned, frees little or nothing.
  - The ext4 path has to be measured before an ADR.

```powershell
go build -o $env:TEMP\hb-dev\hyperbutler-spike\disk\vhdx-spike.exe ./spike/vhdx
$x = "$env:TEMP\hb-dev\hyperbutler-spike\disk\vhdx-spike.exe"
& $x vhdx                  # Q1, Q3
& $x raw                   # Q4a
& $x qemu -backend raw     # Q4b: QEMU file driver, sparse raw, discard=unmap
& $x qemu -backend qcow2   # Q4b: qcow2, discard=unmap
& $x qemu -backend nbd     # Q4b: sparse raw served by the spike's NBD server
& $x qemu -backend nbd -hostflush=false -fsync   # measurement-method check (see the note)
go test ./spike/vhdx/      # NBD range check; FuzzNBDInRange with -fuzz
```

Smart App Control blocked some fresh builds of the spike ("An Application Control policy has
blocked this file"). Building to a new file name worked. Expect this on Home machines with SAC
on.

## Q1: Can a non-admin create a dynamic VHDX, and can it be sparse?

Yes, and yes. There is one catch: the sparse flag has to be paired with another flag.

| `CreateVirtualDisk` (VHDX, 10 GiB, default 32 MiB blocks) | Result | File size | Allocated | Sparse |
|---|---|---|---|---|
| `CREATE_VIRTUAL_DISK_FLAG_NONE` | ok | 4.0 MiB | 4.0 MiB | no |
| `SPARSE_FILE` (0x80) only | **fails:** "Virtual hard disk files must be uncompressed and unencrypted and must not be sparse." | | | |
| `SPARSE_FILE \| SUPPORT_SPARSE_FILES_ANY_FS` (0x80 \| 0x400) | ok | 4.0 MiB | 4.0 MiB | yes |

- **`FSCTL_SET_SPARSE` works without admin on an existing VHDX.** After that, `OpenVirtualDisk`
  fails with the same "must not be sparse" error, unless it passes
  `OPEN_VIRTUAL_DISK_FLAG_SUPPORT_SPARSE_FILES_ANY_FS` (0x400).
- **WSL does the same thing.** `CreateVhd` in `src/windows/common/WslCoreFilesystem.cpp`
  (`microsoft/WSL` main, read on 2026-10-30) does all of this:
  - sets `BlockSizeInBytes = _1MB`;
  - always adds `CREATE_VIRTUAL_DISK_FLAG_SUPPORT_COMPRESSED_VOLUMES`;
  - for sparse disks, adds `CREATE_VIRTUAL_DISK_FLAG_SPARSE_FILE |
    CREATE_VIRTUAL_DISK_FLAG_SUPPORT_SPARSE_FILES_ANY_FS`;
  - emits `MessageSparseVhdUnsafe`.
- **Creating or opening the file is the easy part.** Using it as a disk is not.
  `AttachVirtualDisk` returns `ERROR_PRIVILEGE_NOT_HELD` ("A required privilege is not held by
  the client") for a standard user. HCS works elevated. Whether Hyper-V Administrators
  membership alone is enough is open (ADR 0002).
  `vhdx-spike hcs`, run non-elevated, got `HCS_E_ACCESS_DENIED` (0x8037011B) from
  `HcsCreateComputeSystem`. Before that, `HcsGrantVmAccess` on the VHDX succeeded. So from a
  standard-user token, no Windows component will run guest I/O against a VHDX. QEMU has its own `vhdx` driver, but it has no
  discard either (see Q4b).

## Q2: Does HCS SCSI pass guest TRIM through, and does the VHDX shrink?

From the documentation and source, not measured here:

- **vhdmp handles UNMAP on SCSI-attached VHDX.** Microsoft's VHDX overview lists "efficiency in
  representing data (also known as 'trim'), which results in smaller file size", for SCSI disks
  ([Hyper-V Virtual Hard Disk Format Overview](https://learn.microsoft.com/en-us/previous-versions/windows/it-pro/windows-server-2012-r2-and-2012/hh831446(v=ws.11))).
- **For a normal, non-sparse VHDX, "smaller" means space inside the file is reusable.** The file
  itself does not shrink. Trimmed blocks get freed in the BAT and reused for new writes. The host
  file only gets smaller after offline compaction (`Optimize-VHD`, or `CompactVirtualDisk`; see
  Q3). This matches the WSL2 and Docker Desktop experience in
  [the pain-points research](wsl2-docker-pain-points.md).
- **A sparse VHDX is how WSL tries to make the host file shrink online.** WSL sets three things
  (GitHub code search in `microsoft/WSL` main, 2026-10-30; the matching lines were read, not
  the surrounding logic):
  - The VHDX is created sparse (Q1).
  - The HCS attachment sets `AlwaysAllowSparseFiles: true` (`WslCoreVm.cpp`,
    `HcsVirtualMachine.cpp`, `hcs.cpp`).
  - ext4 is mounted with `discard,errors=remount-ro,data=ordered` (`WslCoreVm.cpp`).

  The design intent is that vhdmp then frees whole VHDX blocks in the host file. That is not
  verified here.
- **That path has a bad record:**
  - WSL disabled sparse VHDs "due to potential data corruption" ([WSL #13075](https://github.com/microsoft/WSL/issues/13075)).
  - WSL 3.0.2 re-enabled them as experimental, with no fix stated. This is a secondary source
    and not checked against the release notes
    ([summary](https://xenospectrum.com/en/wsl302-vnuma-sparse-vhd-scope/)).
  - WSL #41837 (in [the pain-points research](wsl2-docker-pain-points.md)) shows a sparse VHDX
    corrupted when the host disk filled.
  - Users report that sparse VHDs did not shrink, and `Optimize-VHD` refuses sparse files
    ([Microsoft Q&A](https://learn.microsoft.com/en-us/answers/questions/1526083/in-wsl2-with-sparse-vhd-the-storage-usage-does-not)).
- **hcsshim has none of this.** `AlwaysAllowSparseFiles` appears only in its generated schema
  (`internal/hcs/schema2/attachment.go`). A code search finds no guest `discard` or `fstrim`
  plumbing.

### Elevated test (run on 2026-10-30; results below)

`vhdx-spike hcs` boots the same guest script on HCS, with the VHDX on Hyper-V SCSI (`/dev/sda`).
It measures the host file at each step, and again 5 s later. `hv_storvsc` and `sd_mod` are not
in `initramfs-virt`. Only `hv_vmbus` is built in (`modules.builtin`). So the modules come from
Alpine's `linux-virt` package for the same kernel, and the spike packs them into the initrd.

```powershell
# Elevated PowerShell, from the repo root.
$s = "$env:TEMP\hb-dev\hyperbutler-spike"
go build -o "$s\disk\vhdx-spike-hcs.exe" ./spike/vhdx
# 1. Download linux-virt-6.18.58-r0.apk by hand from the Alpine branch that the netboot files
#    came from (dl-cdn.alpinelinux.org/alpine/<branch>/main/x86_64/). It must match
#    6.18.58-0-virt exactly. tar may warn about the apk signature; that is fine.
New-Item -ItemType Directory -Force "$s\linux-virt", "$s\mods" | Out-Null
tar -xzf "$s\linux-virt-6.18.58-r0.apk" -C "$s\linux-virt"
Get-ChildItem -Recurse "$s\linux-virt" -Include scsi_transport_fc.ko*, hv_storvsc.ko*, sd_mod.ko* |
  Copy-Item -Destination "$s\mods"
# 2. Check the dependency order in modules.dep and fix -modlist to match the file names.
Get-ChildItem -Recurse "$s\linux-virt" -Filter modules.dep | Select-String -Pattern "hv_storvsc|sd_mod"
$mods = "scsi_transport_fc.ko.gz,hv_storvsc.ko.gz,sd_mod.ko.gz"   # the names actually in $s\mods, dependencies first
# 3. Plain VHDX, then sparse VHDX with AlwaysAllowSparseFiles.
& "$s\disk\vhdx-spike-hcs.exe" hcs -modules "$s\mods" -modlist $mods         | Tee-Object "$s\hcs-plain.log"
& "$s\disk\vhdx-spike-hcs.exe" hcs -modules "$s\mods" -modlist $mods -sparse | Tee-Object "$s\hcs-sparse.log"
```

Two things may need adjusting if the run fails. A failure here does not mean "HCS does not
trim":

- **Schema version.** If HCS rejects the document, `AlwaysAllowSparseFiles` may need a newer
  schema than 2.1. Retry with `-schema-minor 5`, or higher.
- **File access.** Only the VHDX gets `HcsGrantVmAccess`. The kernel and initrd do not. The
  earlier `spike/hcs` elevated run booted from the same `%TEMP%` paths without a grant.

The results to record:

- Does the guest see discard support? Look at `discard_max_bytes` and `provisioning_mode`.
- Does `allocated` drop after `fstrimmed`, immediately or after the 5 s settle?
- For the plain VHDX, does the file stay the same size after `fstrimmed`, and stay roughly flat
  after `rewritten` (space reused inside the file)?
- Do `reread_ok` and `blkdiscard` succeed on the sparse VHDX?

#### Results (elevated, 2026-10-30)

These are the same machine and guest kernel as above. The guest is a 1024 MiB VM with a 2 GiB
VHDX on HCS SCSI, formatted FAT32. It writes 1 GiB, deletes it, and runs `fstrim`. Then it
rewrites 1 GiB, re-reads it, and runs `blkdiscard` on the whole device. Host allocation is
measured at each step and again 5 s later; the +5 s values never differed.

The first elevated run found that `kmod insmod` is not a kmod subcommand, so no module loaded
and no disk appeared. The guest script now calls kmod through an `insmod` symlink.

The guest sees discard support: `provisioning_mode=unmap`, `discard_granularity=33554432`
(32 MiB, the VHDX block size), `discard_max_bytes=4294966784`, and
`write_zeroes_max_bytes=0`. Both runs passed `reread_ok`.

| Host allocation (MiB) | written | deleted | fstrimmed (2 GiB reported) | rewritten | blkdiscarded | after VM exit |
|---|---|---|---|---|---|---|
| Plain VHDX | 1060 | 1060 | 1060 | 1060 | 1060 | **4** |
| Sparse VHDX + `AlwaysAllowSparseFiles` | 1032 | 1032 | 1032 | 1060 | **36** | **4** |

- **Plain VHDX:**
  - Nothing shrinks while the VM runs. A rewrite after `fstrim` did not grow the file, so
    trimmed blocks are reused inside it.
  - The file was back to 4 MiB after the VM exited. But that was after a whole-device
    `blkdiscard`, so the run does not show whether `fstrim` alone would shrink it on close.
- **Sparse VHDX:**
  - A whole-device `blkdiscard` punched NTFS holes **while the VM was running**: 1060 MiB
    became 36 MiB at once. So vhdmp can return space online, without admin rights beyond the
    HCS access itself.
  - FAT32 `fstrim` reported 2 GiB trimmed but freed nothing on the host. The likely cause is
    alignment: vhdmp unmaps only whole 32 MiB blocks, and FAT32's data area does not start on
    a 32 MiB boundary. This is not proven. ext4, aligned to 32 MiB and mounted with
    `discard`, or run through periodic `fstrim`, is the next test.
#### Follow-up runs (non-elevated, Hyper-V Administrators member, 2026-10-30)

These were run after the account joined Hyper-V Administrators, without elevation. That also
answers ADR 0002 open question (b): group membership is enough.

There are two new guest modes:
- `-mode align` fills the raw device with `dd conv=fsync`, then discards 256 MiB starting at
  4 MiB (off a 32 MiB boundary) and then 256 MiB starting at 512 MiB (on a boundary).
- `-mode trimonly` is the FAT32 flow with a second `fstrim` in place of `blkdiscard`.

| Host allocation (MiB) | written | unaligned discard | aligned discard | after VM exit |
|---|---|---|---|---|
| Sparse VHDX, `align` | 1028 | **772** | **516** | 516 |
| Plain VHDX, `align` | 1028 | 1028 | 1028 | 1028 |

| Host allocation (MiB) | written | deleted | 1st `fstrim` | rewritten | rm + 2nd `fstrim` | after VM exit |
|---|---|---|---|---|---|---|
| Sparse VHDX, `trimonly` | 1032 | 1032 | 1032 | 1060 | **36** | 36 |
| Plain VHDX, `trimonly` | 1060 | 1060 | 1060 | 1060 | 1060 | **36** |

What this shows:
- **Alignment is not the cause.** On the sparse VHDX, a discard that started off a block
  boundary freed its full 256 MiB as well.
- **`fstrim` does work on HCS.** The second FAT32 `fstrim` freed the space online on the
  sparse VHDX. On the plain VHDX it freed the space when the VM exited.
- **Why the first `fstrim` freed nothing (hypothesis).** Before the first `fstrim`, the data
  had only been `sync`ed, and FAT `sync` sends no device flush. Before the second, the file
  system had been unmounted and remounted, which flushes. The `align` run wrote with
  `conv=fsync`, and its discards worked at once. So vhdmp seems to ignore an unmap of blocks
  whose writes have not yet been flushed. To confirm, flush before the first `fstrim`. ext4
  commits its journal with flushes, so normal use is likely fine, but it needs a test.
- **Plain VHDX gives space back only when the disk is closed.** That makes it fit for
  "reclaim on `hyperbutler stop`", not for REQ-DISK-001's "while running".
- **Sparse VHDX with `AlwaysAllowSparseFiles` gives space back online.** It is the candidate
  for REQ-DISK-001 on HCS. WSL marks this path as experimental, so it is not yet trusted.

#### Flush hypothesis and ext4 (non-elevated, 2026-10-30)

- **The flush hypothesis is wrong.** With `-fsync`, the guest writes with `dd conv=fsync`. The
  first FAT32 `fstrim` still freed nothing, on both VHDX types, and the second still worked.
  So the first-`fstrim` miss is FAT32-specific, and its cause is still unknown.
- **ext4 works on the first `fstrim`.** `-mode ext4` uses `mke2fs` 1.47.4 from Alpine v3.24
  packages, with signatures checked against `alpine-devel@lists.alpinelinux.org-6165ee59`. It
  formats a 4 GiB VHDX with `-E nodiscard,lazy_itable_init=0`, writes 4096 files of 256 KiB,
  deletes them, and runs `fstrim` twice. Then it writes and deletes the files again and runs
  `fstrim` once more.

| Host allocation (MiB) | formatted | written | deleted | `fstrim` | 2nd `fstrim` (0 B) | churn | churn `fstrim` | after exit |
|---|---|---|---|---|---|---|---|---|
| Sparse VHDX | 7.8 | 1034 | 1035 | **11.4** | 11.4 | 1038 | **13.8** | 13.8 |
| Plain VHDX | 356 | 1412 | 1412 | 1412 | 1412 | 1412 | 1412 | 1284 |

- **Sparse VHDX + ext4 + `fstrim` returns freed space to the host online, without
  elevation**, and it keeps doing so through a write/delete churn. This is the HCS candidate
  for REQ-DISK-001. It needs a periodic or event-driven `fstrim` in the guest.
- The plain VHDX did not shrink at all with ext4. On exit it only dropped to 1284 MiB, the
  highest block used. So the 4 MiB and 36 MiB "after exit" results above were truncations of
  unused space at the end of the file, not real reclaim.

#### Block size and fragmented free space (non-elevated, 2026-10-30)

The adversarial review of ADR 0002 pointed out a gap. ext4's FITRIM skips free extents smaller
than the device's discard granularity, which is the VHDX block size: 32 MiB by default. The
full-delete test above leaves one big free run, so it hides this. The new `-group N` option
deletes alternating groups of N files of 256 KiB, which leaves free holes of N/4 MiB between
kept files. The new `-block` option sets the VHDX block size. The first `-block` runs had a
bug: the flag was applied after the VHDX was created. The rows below are from the fixed
build, and the block size was confirmed in the VHDX metadata and in the guest's
`discard_granularity`.

| Sparse VHDX, ext4, 1 GiB written, half deleted | after delete | after `fstrim` |
|---|---|---|
| 32 MiB blocks, 2 MiB holes | 1035 MiB | 1035 MiB (nothing returned) |
| 32 MiB blocks, 16 MiB holes | 1035 | 1035 (nothing) |
| 32 MiB blocks, 64 MiB holes | 1035 | **523** |
| **1 MiB blocks**, 2 MiB holes | 1036 | **524** |
| **1 MiB blocks**, 16 MiB holes | 1035 | **523** |
| 1 MiB blocks, full delete then churn | 1035 → 11 | churn 1038 → **14** |

- With the default 32 MiB blocks, free space in runs shorter than 32 MiB is never returned.
  That is the normal case after deleting some image layers and keeping others.
- **With 1 MiB blocks (what WSL uses), the freed half comes back even when the holes are only
  2 MiB.** The empty formatted disk is also smaller: 36 MiB of file size instead of 356 MiB.
- So hyperbutler creates its data VHDX with `BlockSizeInBytes = 1 MiB`. Holes under 1 MiB are
  still not returned. The cost is bounded by about 1 MiB per fragment.

#### Host disk full (non-elevated, 2026-10-30)

**Setup:**
- A 3 GiB NTFS test volume `T:`. It is a VHDX created with `diskpart` in one approved
  elevated step and stored at `C:\hyperbutler-test\fulltest.vhdx`.
- The guest disk is a 4 GiB sparse VHDX on `T:` with 1 MiB blocks.
- A 400 MiB reserve file left about 2.6 GiB free.
- `-mode full` formats ext4 and writes 256 KiB files until a write fails.
- `-mode check` boots the same VHDX, runs `e2fsck -fn`, mounts read-only, re-reads every file,
  and runs `e2fsck -fn` again after journal replay.

**What happened:**
- **HCS did not pause the VM.** When the host volume filled at 2643 MiB allocated, after
  11,217 files, the guest got **I/O errors (`EIO`), not `ENOSPC`**.
- ext4 logged `failed to convert unwritten extents ... potential data loss`, `This should not
  happen!! Data will be lost` and `Detected aborted journal`, then went read-only. `fstrim`
  failed with an I/O error.
- **Rebooted with the host still full:** mounting failed, because journal replay needs to
  write.
- **Rebooted after freeing 400 MiB on the host:** journal recovery completed. `e2fsck -fn`
  then reported **clean** (exit 0). **10,516 files read back with 0 errors.** About 700 files
  that were being written when space ran out were lost.

**Verdict for REQ-DISK-004:**
- The VHDX container and the file system survived and recovered without reformatting.
  That is unlike WSL issue #41837.
- But the guest saw `EIO` instead of `ENOSPC`, and in-flight data was lost. So REQ-DISK-004
  is **not met by the backend alone**. The same would happen with any backend whose image
  grows on demand, including the NBD design.

**Needed:** a free-space guard in hyperbutler.
- The host watches the free space of the volume that holds the VHDX.
- The guest agent keeps an `fallocate`d reservation file on ext4, so that guest free space
  stays at or below host free space minus a margin. `fallocate` reserves guest blocks
  without writing, so the sparse VHDX does not grow.
- The guest then hits `ENOSPC` before the host fills up.
- As a last resort, hyperbutler pauses the VM before the host fills (`HcsPauseComputeSystem`).

- **Still open:**
  - Build and test the free-space guard against `bench/disk-full`.
  - Repeat the WSL corruption scenario: host disk full while a sparse VHDX grows (WSL issue
    #41837).

## Q3: Can a non-admin call `CompactVirtualDisk`?

Yes, for **file-system-agnostic** compaction of a **detached** VHDX. The handle needs
`VIRTUAL_DISK_ACCESS_METAOPS`, plus `OPEN_VIRTUAL_DISK_FLAG_SUPPORT_SPARSE_FILES_ANY_FS` if the
file is sparse. The test used `qemu-io` to write 512 MiB of data and then overwrite it with
zeros. That is the best a guest can do with a VHDX that vhdmp does not serve.

| VHDX | After 512 MiB data | After zero overwrite | After `CompactVirtualDisk` | Time |
|---|---|---|---|---|
| plain | 516.0 MiB | 516.0 MiB | **4.0 MiB** | 117 ms (124 ms on an earlier run) |
| sparse (opened with 0x400) | 516.0 MiB | 516.0 MiB | **4.0 MiB** | 158 ms |

Allocated equals file size in every cell.

**File-system-aware compaction needs the disk attached read-only.** `AttachVirtualDisk` fails
with `ERROR_PRIVILEGE_NOT_HELD`. It only understands NTFS anyway.

**Compaction is offline.** The docs allow it only on a disk that is detached or attached
read-only
([CompactVirtualDisk](https://learn.microsoft.com/en-us/windows/win32/api/virtdisk/nf-virtdisk-compactvirtualdisk)).
It also only finds blocks that are entirely zero. Under QEMU's `vhdx` driver, `fstrim` doesn't
zero anything, so the guest would have to zero-fill free space. Under vhdmp (HCS), UNMAP marks
BAT entries as unmapped. Offline compaction should then reclaim them without zero-filling, but
that is not tested here. Either way it is a stop-the-VM fallback. It fails REQ-DISK-001 ("no
VM restart") on its own.

## Q4a: Sparse raw image with `FSCTL_SET_SPARSE` / `FSCTL_SET_ZERO_DATA`

Both work without admin on a file the user owns.

| Step (10 GiB sparse file) | Allocated |
|---|---|
| `FSCTL_SET_SPARSE` + extend to 10 GiB | 0 |
| write 512 MiB random | 512.0 MiB |
| `FSCTL_SET_ZERO_DATA` 4 KiB / 32 KiB (aligned) | −0 / −0 |
| `FSCTL_SET_ZERO_DATA` 64 KiB / 128 KiB / 1 MiB (aligned) | −64 KiB / −128 KiB / −1 MiB |
| `FSCTL_SET_ZERO_DATA` 128 KiB at +512 B (unaligned) | −64 KiB |
| `FSCTL_SET_ZERO_DATA` whole 512 MiB | 0 (0 allocated ranges left); reads back as zeros |

**NTFS frees space in 64 KiB units (16 × 4 KiB clusters).** For smaller or unaligned ranges it
zero-fills the edges instead. The guest should therefore be told a discard granularity of
64 KiB, so it doesn't send discards that free nothing.

**HCS cannot attach a raw image directly.** As far as hcsshim and the schema show, attachment
types are `VirtualDisk` (VHD, VHDX or ISO through vhdmp), `PassThru` (a host physical disk,
which needs admin to set up), and `ExtensibleVirtualDisk`. The closest thing is a *fixed VHD*,
which is a raw image plus a 512-byte footer. Whether vhdmp punches holes in a sparse fixed VHD
on UNMAP is untested. It needs the same elevated run as Q2.

## Q4b: QEMU-WHPX, measured end to end without admin

Guest steps:

1. `mkfs.vfat` on a 2 GiB disk.
2. Write 1 GiB of `/dev/urandom` and `sync` (with `-fsync`: `dd conv=fsync`).
3. `rm`, `sync`.
4. `fstrim` (it reports 2,143,289,344 bytes trimmed every time).
5. Write 1 GiB again.
6. Remount and check the SHA-1 of the file.
7. `blkdiscard` the whole device.

In the main table the host flushes the image before each measurement (see the measurement
note). Device: `virtio-blk-pci,discard=on,write-zeroes=on`. Drive:
`discard=unmap,detect-zeroes=unmap`.

| Host allocation | raw file, QEMU file driver | qcow2 | **raw, NBD from our process** |
|---|---|---|---|
| after mkfs | 4.0 MiB | 2.4 MiB | 2.1 MiB |
| after 1 GiB write | 1028.0 MiB | 1026.6 MiB | 1026.2 MiB |
| after rm | 1028.0 MiB | 1026.6 MiB | 1025.0–1026.2 MiB |
| **after fstrim** | **1028.0 MiB** | **1026.6 MiB** | **1.0–2.2 MiB** |
| after second 1 GiB write | 2048.0 MiB | 1036.0–1038.8 MiB | 1026.9–1028.0 MiB |
| after blkdiscard | 2048.0 MiB | 1036.0–1038.8 MiB | 0.1 MiB |
| data re-read after remount | ok | ok | ok |
| runs | 4 | 2 | 4 |

The re-read only checks that the data survives a remount within one boot. It is not a
durability check. Each column was also run at least once with `-fsync` (`dd conv=fsync`). The
`after fstrim` row was unchanged: 1028.0 MiB, 1026.6 MiB and 1.5–1.8 MiB.

What the table shows:

- **QEMU's raw file driver doesn't free anything.** The guest sees discard support
  (`discard_max_bytes=2147483136`), and `fstrim` succeeds, but the host file keeps every byte.
  `block/file-win32.c` has no `bdrv_co_pdiscard` or `bdrv_co_pwrite_zeroes`. This was read from
  QEMU's `master` branch on 2026-10-30, not from the 11.1 tag; the 11.1 binary behaves the same.
  The driver only sets the sparse flag when QEMU itself creates the file. `qemu-io -c "discard 0 256M"` on a sparse raw
  file reproduces this in isolation: allocation stays at 256 MiB.
- **qcow2 makes trimmed space reusable inside the file.** The second 1 GiB write grew the file
  by only about 10 MiB. But the file never shrinks.
- **The spike's NBD server gives space back to the host while the VM runs.** The server is
  about 300 lines of Go with no dependencies. It maps `NBD_CMD_TRIM` and `NBD_CMD_WRITE_ZEROES`
  without `NO_HOLE` to `FSCTL_SET_ZERO_DATA`. Each run did 3 trims covering 4092 MiB. The
  `fstrim` step took ≤ 0.2 s as seen from the host.
- **NBD costs throughput, but by how much is not known.** These numbers come from `dd` from
  `/dev/urandom` in the guest. Without `-fsync`, most of the write is absorbed by the guest and
  host caches. So they show a direction, not a storage benchmark:

  | | first 1 GiB write | second 1 GiB write |
  |---|---|---|
  | QEMU file driver | 215 MB/s (185 MB/s with fsync) | 386–406 MB/s (316 MB/s with fsync) |
  | NBD over loopback TCP | 154–157 MB/s (141–146 MB/s with fsync) | 243–269 MB/s (219–220 MB/s with fsync) |

  The spike serves one request at a time, with simple replies, over TCP. A real figure needs
  `bench/`.

**Measurement note:** NTFS allocates clusters in a sparse file's holes only when cached data
is written back. Linux `sync(2)` on FAT does not send a device flush, so neither QEMU nor the
NBD server ever got a FLUSH, and nothing on the host wrote the data back. Right after the guest
had written and synced 1 GiB, `GetCompressedFileSizeW` showed:

| Run | Raw file driver | NBD |
|---|---|---|
| no flush anywhere | 92 / 111 MiB | 364 MiB |
| guest `dd conv=fsync` (device flush), no host flush | 1028 MiB | 1026 MiB |
| host `FlushFileBuffers` from a second handle | 1028 MiB | 1026 MiB |

QEMU opens the image so that a second handle can open and flush it. No "host flush" error was
printed in any run. The main table uses the host flush. A guest that sends flushes, which ext4
does on `sync` and on journal commits, needs no trick. For `hyperbutler status` (REQ-DISK-005) and
the `bench/disk-reclaim` harness, this means sampling allocation after a guest or host flush.
Otherwise freshly written data is under-counted for a while.

## Risks and open questions

- **Layer digest mismatches after `rmi` (2026-11-01):** pulls failing with `unexpected
  commit digest` right after a delete-triggered `fstrim` were investigated as a
  REQ-DISK-003 problem. The cause was the network link, not the disk: see
  [guest-networking.md](guest-networking.md) "Layer digest mismatches on LAN pulls".
  A storage stress run on the data disk (256 MiB random files hashed on write, re-read
  after `drop_caches` and with `O_DIRECT`, while other files were written, deleted and
  trimmed) found 0 mismatches in 100 files.
- **REQ-DISK-003 (integrity):** nothing here was crash-tested.
  - The spike's NBD server serves one connection at a time, and applies its requests in
    arrival order, so a trim never overtakes an earlier write. That ordering is a property of
    this spike, not something NBD gives for free. A real server that handles requests in
    parallel, or accepts several connections, has to order overlapping requests itself.
  - FUA is honored on WRITE, TRIM and WRITE_ZEROES with `FlushFileBuffers`. Nothing checks yet
    that `FlushFileBuffers` commits an `FSCTL_SET_ZERO_DATA`.
  - Whether `FSCTL_SET_ZERO_DATA` is atomic across a power loss is not known. NTFS journals the
    metadata change but not the ordering against data still in the cache.
  - The kill-test bench has to run against this design before it ships.
- **REQ-DISK-004 (host full):** this design has the same exposure as WSL's sparse VHDX
  (#41837, corrupted when the host disk filled).
  - Writes into holes in a sparse file fail with `ERROR_DISK_FULL` even though the file looks
    big enough. They can fail after the guest filesystem has already committed metadata that
    points at those blocks.
  - `FSCTL_SET_ZERO_DATA` on an unaligned edge has to write zeros, and can fail too.
  - The spike returns `EIO`. Returning `NBD_ENOSPC` instead is necessary, but it does not make a
    half-written extent safe.
  - A real backend probably needs to reserve space ahead of the guest, or refuse writes below a
    host reserve (REQ-DISK-005). Until `bench/disk-full` passes, assume WSL's failure class.
- **Fragmentation:** repeated punch/fill cycles fragment a sparse file. NTFS caps the number of
  extents per file, and fails with `ERROR_FILE_SYSTEM_LIMITATION` once a file is heavily
  fragmented. This was not measured. A 24 h churn test is needed. Punching only in 64 KiB-aligned
  units, or coarser (1 MiB), reduces the extent count.
- **ReFS / Dev Drive:** sparse and zero-data behave differently on ReFS. Only NTFS was tested.
- **ext4:** this was not tested, because there was no `mke2fs` or ext4 module in the initramfs.
  - A 5-minute `fstrim` timer alone can miss REQ-DISK-001's ≤ 5 min window, since the trim
    itself takes time.
  - The guest should run `fstrim` right after large deletes (image removal, prune), plus a
    timer as a backstop. `-o discard` is the alternative.
  - Which one to use needs a measurement of write latency and of discard size distribution on
    ext4 after churn.
- **Transport:** loopback TCP is reachable by any local process. A real server should use an
  AF_UNIX socket or a named pipe restricted to the user's SID. QEMU's Windows NBD client over
  AF_UNIX was not tested.
- **Hardening:** the NBD protocol handler parses data from the VMM.
  - The range check has a unit test and a fuzz target, after an overflow found in review.
  - The option and request parsers still need fuzz tests (CONTRIBUTING.md) before any of this leaves
    `spike/`.
  - The handshake is minimal: it does not parse the `NBD_OPT_GO` payload, and it drops the
    connection on oversized writes instead of draining them.

## Recommendation

For the default, non-admin backend (QEMU-WHPX), keep the data disk as a **sparse raw file on
NTFS, served by a block backend inside hyperbutler**. The spike used NBD over loopback. Map
discard and write-zeroes to `FSCTL_SET_ZERO_DATA` in 64 KiB-aligned units. Have the guest run
ext4 with `fstrim` triggered after large deletes, plus a timer.

This is the only setup measured here that hands space back while the VM runs, without admin:
1026 MiB down to 1–2 MiB after `fstrim`. That number is a best case: FAT32, one contiguous
file. Don't rely on QEMU's file driver, `vhdx` driver, or qcow2. They turn discards into a
no-op, or into reuse inside the file.

Two alternatives should go in the ADR:

- **A small QEMU patch adding `pdiscard` to `file-win32.c`.** It would remove the NBD hop. It is
  GPL, so it has to be upstreamed or shipped as a patched binary, not copied into hyperbutler.
- **For an HCS backend** (elevated, possibly Hyper-V Administrators, or a SYSTEM broker; see
  ADR 0002):
  a sparse VHDX (0x80 | 0x400, creatable without admin) with `AlwaysAllowSparseFiles`, as WSL
  does. That path waits on the elevated run above, and on WSL's open corruption warning.

Offline `CompactVirtualDisk` works without admin but needs the VM stopped, so it is a fallback
only. Before an ADR, run these against the NBD design:

- the REQ-DISK-003 kill test;
- the REQ-DISK-004 disk-full test (it inherits WSL's sparse-file host-full risk);
- an ext4 churn test that measures both the reclaim ratio and fragmentation.

## Free-space guard (REQ-DISK-004, non-elevated, 2026-10-30)

The "Host disk full" run showed that HCS does not pause the VM when the host volume fills,
so the guest gets `EIO`. The guard makes the guest hit `ENOSPC` first.

**Code:**
- `internal/spaceguard` holds the pure policy and the budget wire format.
- `guest/cmd/spaceguard` is the guest daemon.
- `spike/vhdx/guard_windows.go` is the host loop, plus `-mode guard`/`guardcheck` in
  `spike/vhdx`.

### Design

The guest keeps an `fallocate`d reservation file on its ext4 data file system. `fallocate`
creates unwritten extents. ext4 takes the blocks, but nothing is written, so the sparse VHDX
does not grow.

**Invariant.** Take these quantities:
- D: the guest block device size;
- R: the reservation;
- A: the VHDX's host allocation;
- M: the part of A that is VHDX metadata (the allocation of the empty VHDX);
- F_h: host free space.

If the guest wrote every block outside the reservation, the VHDX could grow by at most
`exposure = D ??R ??(A ??M)`. The guard keeps `exposure ??F_h ??margin`.

- **Why not "guest free ??host free ??margin".** That simpler rule failed on the first
  measured run. ext4 counts the journal and inode tables as used, but `mke2fs` writes them as
  zeros, and vhdmp leaves zero-written blocks unallocated. So the guest's first writes to its
  own metadata grew the VHDX by about 46 MiB more than the budget, and host free fell to
  3.2 MiB (only the hard-floor pause stopped it). A file written as zeros and overwritten
  later does the same thing. The exposure rule counts all of these, and dirty data that is
  not yet on disk too, with no guess about which used blocks are backed.
- **No TOCTOU with the guest's own writes.** When the guest writes W new bytes, A grows by W
  and F_h shrinks by W. Both sides of the inequality move together, so a budget measured
  before a write burst is still exact after it, whatever the write rate.
  `TestGuestWriteDoesNotStaleBudget` checks this, and `FuzzPlan` asserts it on random
  inputs. Only *other* host writers consume F_h behind the guard. The margin covers one
  update interval of those, and the hard floor covers the rest.
- **Where it is optimistic:**
  - when the reservation sits on blocks that are still backed (deleted, not yet trimmed).
    The guest daemon runs `FITRIM` before growing the reservation, at most every 10 s, to
    keep this small;
  - when a 1 MiB VHDX block is shared between reservation and data. The margin covers this.

**Split:**
- The host sends `budget <F_h ??margin> <A ??M>` (or `budget unlimited` when
  `VHDXMax ??A ??F_h ??margin`) every 100 ms.
- The guest computes `need = D ??(A ??M) ??budget`, capped at free + reserved.
- **Hysteresis:**
  - when growing, the guard overshoots to need + H/2;
  - it shrinks only above need + H;
  - when nothing is needed, it releases everything.
- Targets are rounded up to 1 MiB.
- `Short > 0` means the exposure cannot be brought under the budget even with every free
  block reserved.

**Last resort.** The host pauses the VM (`HcsPauseComputeSystem`) when F_h < floor. It
resumes it once F_h ??floor + resume gap. The floor must be below the margin, which
`Config.Validate` enforces. If the guest daemon stops reporting for 5 s, the host pauses at
the margin instead of the floor, because the reservation may be gone.

**Fail closed:**
- **Guest:** before the first budget, after the control channel breaks, and after `-stale`
  (30 s) worth of its own 100 ms ticks without a budget, the budget is treated as 0. The
  guard counts ticks, not wall-clock time, so a paused VM or a sleeping host, whose guest
  clock jumps on resume, does not trip it.
- **Host:** a measurement error sends budget 0 and still runs the pause check.

**Transport (spike only).** Text lines on COM2 (`/dev/ttyS1`, a second HCS `ComPorts`
entry). The host opens the pipe for overlapped I/O. With a synchronous handle, Windows
serializes the pending `ReadFile` and the `WriteFile`, so budgets were delayed until the
guest printed a status line (review finding 1). The guest agent over hvsock replaces this.

### Results

**Setup:**
- 2 GiB sparse VHDX, 1 MiB blocks, in `T:\guard\` on the 3 GiB test volume. The volume
  had about 384 MiB free, since older files at the root are not this test's to delete.
- ext4 with `lazy_itable_init=0,lazy_journal_init=0`.
- 256 KiB files from `/dev/urandom` written with `dd` until 10 writes in a row fail. busybox
  `head` prints "I/O error" for any write error, so it cannot tell `ENOSPC` from `EIO`.
- Margin 64 MiB, floor 24 MiB, resume gap 32 MiB, hysteresis 16 MiB.
- After the fill: delete half, `fstrim`, and a manifest (file count and SHA-1 over all files,
  read after `drop_caches`).
- `guardcheck` boots the same VHDX and runs these checks:
  - `e2fsck -fn`;
  - a read-only mount;
  - a re-read and size check of every file;
  - the manifest;
  - `e2fsck -fn` again after mount.

| Run | Failed writes | Min host free | Pauses | dmesg ext4/IO errors | `e2fsck` (before / after mount) | Files written ??kept ??read back | Manifest |
|---|---|---|---|---|---|---|---|
| Baseline, no guard (section above, 4 GiB disk) | `EIO`, journal aborted | 0 | 0 (HCS never pauses) | yes | clean after freeing space | ~700 in-flight files lost | n/a |
| First guard (guest free ??budget) | none: host hit floor first | **3.2 MiB** | 1 (VM stuck paused, nothing could free space) | ??| not checked | ??| ??|
| **Guard (exposure rule)** | **10/10 `ENOSPC`**, 0 `EIO` | 179 MiB | 0 | **0** | **0 / 0** | 805 ??400 ??**400** | match |
| Guard + 150 MiB zero file overwritten after the fill | 10/10 `ENOSPC` | 188 MiB | 0 | 0 | 0 / 0 | 173 (+ zero file) ??86 ??86 | match |
| Guard + host process fills the volume to 12 MiB | 10/10 `ENOSPC` | 12.0 MiB | **1: paused 10.8 s, resumed** | 0 | 0 / 0 | 773 ??384 ??384 | match |

**What the runs show:**
- **REQ-DISK-004 is met on this setup:** `ENOSPC` before the host fills, no ext4 errors,
  clean `e2fsck`, and every file the writer saw succeed is intact. The failed partial file of
  each `ENOSPC` is deleted by the script, because its writer was told. In the zero-file run,
  the zero file is the one `wrong_size` entry: it is 150 MiB, not 256 KiB.
- **The zero-file case grew the VHDX by 150 MiB with no change in guest free space.** The
  guard had already reserved for it, so host free stayed at 188 MiB or more.
- **Pause works and is safe.**
  - `HcsPauseComputeSystem` took 3??5 ms and the state became `Paused`.
  - After the filler was deleted, `HcsResumeComputeSystem` took 2??7 ms.
  - The guest continued and its writes then ended in `ENOSPC`.
  - Pause is not recovery: in the first guard run nothing could free space, so the VM
    stayed paused until it was killed. hyperbutler has to tell the user.
- **The guard costs slack.** With a working guard the guest stopped with about 115 MiB more
  host free space than the margin. That slack is real worst-case exposure, not error:
  - inode tables and journal blocks that ext4 has not written yet (about 100 MiB on 2 GiB);
  - 16 MiB of ext4 `s_resv_clusters`, which `statfs` reports free but `fallocate` cannot
    take.

  Inode tables are about 1.6% of the disk at the default inode ratio. The cost only
  matters when the host is nearly full; `mke2fs -i` (fewer inodes) reduces it.
- **Not tested:**
  - other host writers faster than the margin per 100 ms;
  - several VHDXs on one volume, which each get the full spare today;
  - host sleep;
  - the guest-root attacks listed below.

### Fuzzing

`internal/spaceguard/fuzz_test.go` has three targets, each run for 90 s on the final code:

| Target | Execs |
|---|---|
| `FuzzPlan` | 22.6M |
| `FuzzParseBudget` | 30.9M |
| `FuzzBudgetWireRoundTrip` | 30.5M |

**`FuzzPlan` asserts, with `math/big`:**
- exposure over budget equals `Short` exactly;
- Target ??free + reserved;
- Target ??Need;
- the pause hysteresis;
- no flapping (re-planning after applying the target gives the same target);
- monotonicity (less host free never lowers the need);
- the TOCTOU property above;
- errors exactly when the inputs fail `Validate`.

**Bugs found:**
- `FuzzParseBudget` found one bug: a bare trailing `\r` was accepted. It is fixed, and the
  input is kept in `testdata/fuzz/FuzzParseBudget/`.
- Unit tests caught two overflow bugs before fuzzing: `Used+Free` in `Validate`, and the
  saturated pending sum.

### Adversarial review

| # | Finding | Resolution |
|---|---|---|
| 1 | High: the COM2 pipe was a synchronous handle, so the sender's write waited for the reader's pending read. Budgets reached the guest only when it printed a status line (~1 s lag; seen in run 3's log). | **Fixed:** the host opens the pipe with go-winio (overlapped). |
| 2 | High: used-but-unbacked blocks (zero-written files, preallocated files, unwritten metadata) can grow the VHDX without changing guest free. | **Fixed** by the exposure invariant, and tested with the zero-file run. **Rejected for a hostile guest root** that writes into the reservation file itself (`dd conv=notrunc`): the guard protects against cooperative workloads, and the hard floor is the guarantee against a hostile guest. In hyperbutler the file lives outside every container mount. |
| 3 | Medium: `grow` started at `st_size`, so a file with holes was never filled. | **Fixed:** `grow` allocates from offset 0 in chunks; this is idempotent on allocated ranges. |
| 4 | Medium: `Overhead` counted all metadata forever, causing permanent early `ENOSPC`. The runs never came within the margin. | **Fixed** by the exposure rule: metadata stops counting once it is written. The remaining slack is real exposure (see above). The tight-margin case is covered by the host-filler run, which drove the host to 12 MiB. |
| 5 | Medium: wall-clock staleness after a pause or host sleep would fail closed and give spurious `ENOSPC`. | **Fixed:** staleness counts the daemon's own ticks. |
| 6 | Medium: a control-channel error killed the daemon and froze the reservation. The host did not notice a dead guard. | **Fixed:** the daemon fails closed and reopens the channel. The host pauses at the margin after 5 s without status. |
| 7 | Medium: a VHDX measurement error skipped the pause check. | **Fixed:** the host sends budget 0 and the pause check always runs. |
| 8 | Low: `freeBytes` hid API errors; several VHDXs on one volume each get the full spare. | **Fixed** (the guard logs the error and treats free space as 0). **Deferred** the multi-VHDX split to the real implementation, which must divide the spare across the VHDXs on a volume. |

**Still open:**
- Move the budget to the guest agent over hvsock.
- Divide the spare across several VHDXs.
- Run `bench/disk-full` once it exists.
- Repeat with `docker pull` as the writer.
