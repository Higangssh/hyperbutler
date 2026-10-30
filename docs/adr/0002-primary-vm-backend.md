# 0002. Primary VM backend

- Status: proposed. The decision was drafted and reviewed on 2026-10-30. It becomes accepted
  once the host-disk-full test in finding 3 passes.
- Date: 2026-10-29
- Requirements: REQ-START-001, REQ-MEM-001, REQ-MEM-002, REQ-DISK-001, REQ-DISK-002,
  REQ-DISK-004, REQ-API-001, REQ-PLAT-001, REQ-PLAT-003

## Context

winbox needs a Linux VM. The backend must work on Windows Home ([ADR 0001](0001-windows-home-first-class.md)).

## Options

1. **HCS** via `computecore.dll` (hcsshim). This is the thinnest path and the API WSL2 itself
   uses. It's unknown whether a third-party process can create a Linux utility VM through it
   on Home once Virtual Machine Platform is enabled.
2. **Windows Hypervisor Platform (WHP)**, with QEMU-WHPX as the prototype and a dedicated
   VMM (for example crosvm's WHPX backend) later. It's documented as available on Home, but
   the VMM work is much heavier.

## Observations

2026-10-30, Windows 11 Home build 26200, after enabling `VirtualMachinePlatform` and
`HypervisorPlatform` and rebooting (WSL still disabled):

- The `vmcompute` (Host Compute Service) and `hns` (Host Network Service) services exist and are
  running, both with start type Manual.
- `vmcompute.exe`, `vmwp.exe`, `computecore.dll`, `computestorage.dll`, `computenetwork.dll`,
  and `WinHvPlatform.dll` are present.

So the HCS stack itself ships on Home with Virtual Machine Platform, and WSL isn't required for
it.

Spike step 2, first run (`spike/hcs`, which calls `computecore.dll` directly):

- A non-elevated process created a LinuxKernelDirect VM. The account is in Administrators,
  but the token is filtered, and the account is **not** in Hyper-V Administrators.
- `HcsCreateComputeSystem` failed with `0x8037011B` (`HCS_E_ACCESS_DENIED`). The system
  message says that only administrators or members of the **Hyper-V Administrators** group may
  access VMs or containers (see https://aka.ms/hcsadmin).
- So access to HCS on Home is gated by group membership, not by edition. The group exists on
  Home (`S-1-5-32-578`).

Still open:

- (a) Does the VM config boot when run elevated?
- (b) Does Hyper-V Administrators membership alone, without elevation, allow it?
- (c) The security cost. Hyper-V Administrators is close to admin-equivalent, because its
  members can attach any host disk to a VM. The alternative is a SYSTEM broker service, as
  WSL does with `wslservice.exe`, protected by a per-user named-pipe ACL.

Spike step 2, elevated run. This answers (a): **yes, it boots.** Same Alpine kernel and initrd,
512 MB, 2 vCPUs, serial console on a COM port named pipe.

| Step | Time since start |
|---|---|
| `HcsCreateComputeSystem` done | 132 ms |
| `HcsStartComputeSystem` done | 156 ms |
| First serial output | 307 ms |
| `Alpine Init` seen | **784 ms** |

- The kernel booted to the Alpine init and then to the initramfs shell. That is expected, since
  there is no boot media.
- `vmmem` peaked at a 186 MB working set.
- HCS reported a reserved memory size of 131072 pages (512 MB) with `BalancingEnabled: false`.
  Dynamic memory and balloon settings remain to be explored.
- In this test, HCS boots **2.4× faster than QEMU-WHPX** (0.78 s vs 1.89 s). Treat this ratio as
  rough: HCS was measured once and QEMU five times, and ADR 0005 notes a `Start-Process`
  artefact of up to 0.7 s that may inflate the QEMU numbers. Both are far inside the 8 s
  target.

Elevated run with the ADR 0005 prototype guest (initramfs with dockerd 29.9.2), 1024 MB,
2 vCPUs, no network device:

- The dockerd Unix socket was ready **912 ms** after process start. The guest reached
  `init-start` at 0.62 s of uptime and the socket at 0.67 s. QEMU-WHPX took 2.2 s for the same
  image (ADR 0005).
- dockerd stayed up for the 45 s hold.
- `vmmem` peaked at a 408 MB working set. About 174 MB of that is the unpacked initramfs in
  guest RAM (`Shmem`), which ADR 0005's separate system disk is meant to remove.
  `BalancingEnabled` was again false, so memory reclaim is still untested.
- With 512 MB the same image failed. The kernel reported `Initramfs unpacking failed: write
  error`, and dockerd later could not create its bridge.

Disk reclaim on HCS SCSI is recorded in `docs/research/disk-reclaim.md`:
- **Sparse VHDX** with `AlwaysAllowSparseFiles` and ext4 in the guest: `fstrim` returns the
  space of 1 GiB of deleted files to the host while the VM runs (1035 MiB → 11 MiB).
- **Plain VHDX:** space is not returned.

Memory reclaim on HCS (non-elevated, `spike/vhdx -mode mem`):
- **Setup:** a 1024 MB guest with `hv_balloon` loaded fills 600 MiB of tmpfs, then frees it.
- **Measured:** `vmmem` working set and private bytes, read through `Get-Process`.

| `VirtualMachineMemory` flags | booted (WS / private) | 600 MiB used | freed +15 s | freed +30 s |
|---|---|---|---|---|
| `AllowOvercommit` only | 210 / 1026 MiB | 812 / 1026 | 812 / 1026 | 812 / 1026 |
| + `EnableHotHint` | 206 / 1026 | 804 / 1026 | 804 / 1026 | 804 / 1026 |
| + `EnableDeferredCommit`, `EnableHotHint`, `EnableColdDiscardHint` | **204 / 204** | 790 / 791 | **240** / 791 | **240 / 240** |

- **With all three flags, HCS commits memory only as the guest touches it, and gives it back
  within 15–30 s after the guest frees it.** It needs no balloon target and no elevation. This
  meets the reclaim part of REQ-MEM-001. QEMU-WHPX cannot do this: ADR 0005 found that
  free-page reporting fails there.
- `EnableColdDiscardHint` without `EnableDeferredCommit` is rejected:
  `HcsCreateComputeSystem` fails with `0x80070057`, "failed to create partition".
- `BalancingEnabled` stays false. Reclaim comes from guest free page reporting through
  `hv_balloon`, not from dynamic memory balancing. A control run confirms it:
  - With `hv_balloon` loaded, guest `dmesg` shows `Free page reporting enabled` and
    `hv_balloon: Cold memory discard hint enabled with order 9`. Private working set went from
    794 MiB back to 212 MiB within 15 s.
  - Without `hv_balloon`, using the same flags, it stayed at 798 MiB.
- **Page cache is not returned on its own.** Reading 600 MiB from the disk raised the private
  working set to 818 MiB. It came back to 224 MiB only after `drop_caches`. REQ-MEM-002
  (4 GB of page cache) therefore needs a guest-side cache policy, such as periodic or
  pressure-driven cache trimming. This is a design item, not a backend limit.
- From this run on, memory is reported as the private working set
  (`Win32_PerfFormattedData_PerfProc_Process.WorkingSetPrivate`), the REQ-MEM-001 metric. A
  10-minute idle bench as specified has not been run yet.

This answers open question (b): **yes.** After the account joined Hyper-V Administrators and
signed in again, a non-elevated process booted the same Alpine VM through HCS. `Alpine Init`
was reached in 744 ms, the same as elevated.

QEMU-WHPX baseline, same day. Run non-elevated, so no group membership was needed:

- **Setup:** QEMU 11.1.0, `-accel whpx -m 512 -smp 2`, Alpine `latest-stable` netboot
  `vmlinuz-virt` + `initramfs-virt` (SHA256 `17FBD3EF…` / `41D55A3F…`), `console=ttyS0`.
- **Boot:** From process start until the `Alpine Init` serial line, 5 runs: 1915, 1893, 1886,
  1853, and 1877 ms. **Median 1.89 s.** For comparison, TCG (no acceleration) took 2.6 s.
- **Memory:** After 20 s idle at the initramfs shell, with no containers and no dockerd, the
  QEMU process showed 570 MB private and 202 MB working set. QEMU commits the whole guest RAM, so
  private memory tracks `-m`. That makes balloon and free page reporting a must for
  REQ-MEM-001.
- **Gotcha:** With `-machine q35 -cpu max -nodefaults`, the guest printed nothing. The default
  machine works.

## Spike (decides this ADR)

On Windows 11 Home with `VirtualMachinePlatform` and `HypervisorPlatform` enabled:

1. ~~Record whether `vmcompute` is now present and running.~~ It is. See Observations.
2. From a non-admin Go program, create, start, and stop a minimal Linux utility VM via HCS.
3. Boot the same kernel/initrd with QEMU-WHPX. Measure cold start and idle RAM for both.

## Decision

**HCS is the primary and, for 1.0, the only supported backend.**

QEMU-WHPX stays a development and diagnostic tool. It is not a shipped backend.

| Requirement | HCS (measured) | QEMU-WHPX (measured) |
|---|---|---|
| REQ-START-001: start ≤ 8 s | dockerd socket in 0.91 s | 2.2 s |
| REQ-MEM-001: idle RAM, reclaim | commits on touch; freed memory returned in ≤ 30 s | commits all of `-m`; free page reporting fails |
| REQ-DISK-001: online reclaim | sparse VHDX + ext4 `fstrim`: 1035 → 11 MiB | only with a winbox-served block device (NBD prototype) |
| REQ-API-001 transport | hvsock is part of HCS; not yet tested end to end with the guest | no vsock or hvsock in the Windows build |
| No elevation in daily use | needs Hyper-V Administrators or a broker (ADR 0004) | none needed |

### Settings this decision fixes

- **VM document:** schema 2.x, `LinuxKernelDirect` boot, `StopOnReset`,
  `ShouldTerminateOnLastHandleClosed`.
- **Memory:** `AllowOvercommit`, `EnableDeferredCommit`, `EnableHotHint` and
  `EnableColdDiscardHint`. HCS rejects the last one without deferred commit.
- **Data disk:** a dynamic VHDX with **1 MiB blocks** that is an NTFS sparse file, attached on
  SCSI with `AlwaysAllowSparseFiles`. With the default 32 MiB blocks, free runs shorter than
  32 MiB are never returned (`docs/research/disk-reclaim.md`).
- **Guest:** ext4 on the data disk, with a periodic and an event-driven `fstrim`. The guest
  kernel needs `CONFIG_PAGE_REPORTING` and `hv_balloon` loaded.
- **Access:** non-admin access goes through the broker in ADR 0004. Hyper-V Administrators
  membership is a developer mode only, not supported for end users. ADR 0004's
  virtual-account variant is still unverified, so the broker may have to run as SYSTEM.

### Why not a two-backend design

QEMU-WHPX would need its own block backend for REQ-DISK-001 and its own transport for
REQ-API-001. Even then it could not meet the reclaim part of REQ-MEM-001 without patches to
QEMU. ADR 0001 already allows one backend for 1.0. The backend interface stays, so a WHP
backend can be added later if HCS access ever becomes impossible on some edition.

## Adversarial review

The adversarial review of the drafted decision on 2026-10-30 raised nine findings.

1. **High. ext4 reclaim was only shown for one contiguous free run.** **Fixed and tested.**
   With 32 MiB blocks, holes under 32 MiB were never returned. With 1 MiB blocks, half of
   1 GiB came back from 2 MiB holes. The decision now fixes 1 MiB blocks.
2. **High. The memory reclaim mechanism was asserted, not shown.** **Tested.**
   - Guest `dmesg` shows that free page reporting and the cold discard hint are enabled by
     `hv_balloon`. A control run without `hv_balloon` returned nothing.
   - The REQ-MEM-001 vs REQ-MEM-002 mix-up is fixed: reclaim is REQ-MEM-002.
   - The page-cache gap is recorded.
   - Still open: a multi-GB guest and the REQ-MEM-002 bench.
3. **High. The sparse-VHDX fallback is unverified.** **Accepted as a gate.**
   - The plain-VHDX fallback in the first draft was wrong: plain VHDX returned nothing with
     ext4.
   - Before 0.1, a host-disk-full test on sparse VHDX (REQ-DISK-004) must pass. Until then
     this ADR stays proposed.
   - If the test fails, the fallback is a winbox-served block device, the NBD design proven
     under QEMU, re-evaluated for HCS.
4. **Medium. Wrong memory counter.** **Fixed:** the private working set is reported from now
   on. The idle bench is still to be run.
5. **Medium. Requirement and ADR conflicts.** **Fixed.** REQ-DISK-002 and REQ-PLAT-003 are
   amended, and ADR 0004's wording is aligned.
6. **Medium. Deferred commit under host memory pressure.** **Added** as a consequence, with a
   test required before 0.1.
7. **Medium. hvsock is untested.** **Fixed:** the table no longer claims it. It is the next
   integration test.
8. **Low. The boot ratio was overstated.** **Fixed:** sample sizes and the QEMU artefact are
   noted.
9. **Low. ADR 0004 option C is unverified.** **Noted** in Settings; the broker may need to run
   as SYSTEM.

## Consequences

- **Elevation:** winbox needs the ADR 0004 broker, or the user's own Hyper-V Administrators
  membership, before it can start a VM. There is no zero-setup, no-admin path in 1.0.
- **Sparse VHDX risk:** WSL marks sparse VHDX as experimental and has an open corruption report
  when the host disk fills up (WSL issue #41837). Before 0.1, winbox must pass a host-full
  test on sparse VHDX (REQ-DISK-004). If it fails, the fallback is a winbox-served block
  device: the NBD design that worked under QEMU, re-evaluated for HCS. A plain VHDX is not a
  fallback, because it returned nothing with ext4.
- **Undocumented settings:** HCS schema fields such as `AlwaysAllowSparseFiles` and the memory
  hint flags are not documented by Microsoft for third-party use. They were found in hcsshim's
  generated schema. A Windows update could change them, so CI on real Windows hardware must
  cover them. Hosted runners have no nested Hyper-V, so this is a manual or self-hosted gate.
- **Small Windows API surface:** `computecore.dll`, `virtdisk.dll` and hvsock. There is no
  QEMU, so no GPL binary ships in the host package.
- **Microsoft dependency:** Microsoft controls HCS behaviour, and WSL is its main consumer. A
  WSL-driven change can break winbox.
- **Deferred commit under host pressure:** with `AllowOvercommit` and `EnableDeferredCommit`,
  guest RAM is not reserved up front. If the host runs out of commit, HCS may pause or stop
  the VM, possibly in the middle of a write (REQ-DISK-003). This needs a host-memory-pressure
  test before 0.1. Mitigations to evaluate: a commit check before start, and a smaller
  default maximum.
- **Measurement gaps:** most numbers so far come from one run each, on one machine, with
  synthetic workloads, and memory was read through `Get-Process`. The REQ-START-001,
  REQ-MEM-001 and REQ-MEM-002 benches, run the way `docs/requirements.md` specifies, have
  not been run yet.
