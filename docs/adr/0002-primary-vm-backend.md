# 0002. Primary VM backend

- Status: proposed
- Date: 2026-10-29
- Requirements: (to be linked from docs/requirements.md)

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
- In this test, HCS boots **2.4× faster than QEMU-WHPX** (0.78 s vs 1.89 s).

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

Pending the spike.

- If HCS works on Home, HCS is primary.
- If it doesn't, WHP is primary and HCS becomes an optional fast path.

## Adversarial review

To be done once the spike results exist.

## Consequences

To be written with the decision.
