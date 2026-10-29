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
