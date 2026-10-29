# 0001. Windows Home is a first-class target

- Status: accepted
- Date: 2026-10-29
- Requirements: (to be linked from docs/requirements.md)

## Context

Most Docker-compatible runtimes on Windows assume Hyper-V, which ships only with
Pro/Enterprise, or they fall back to WSL2. Windows Home users have the fewest options, and the
maintainer's own development machine runs Windows 11 Home.

On that machine (Windows 11 Home, build 26200) we observed the following before enabling any
optional features:

- The hypervisor is running (likely because of VBS/Memory Integrity).
- `WinHvPlatform.dll` and `computecore.dll` exist on disk.
- The optional features `VirtualMachinePlatform`, `HypervisorPlatform`, and
  `Microsoft-Windows-Subsystem-Linux` are all **disabled**.
- `vmcompute.exe` and the `vmcompute` service are absent. They are expected to arrive with
  Virtual Machine Platform, which hasn't been verified yet.

## Options

1. **Pro/Enterprise only**, relying on the full Hyper-V stack. This excludes Home users.
2. **Home first-class.** Every feature must work on Home. Pro-only fast paths are allowed
   only if they're optional.

## Decision

Option 2. Which VM backend is primary on Home is decided separately, in
[ADR 0002](0002-primary-vm-backend.md).

## Adversarial review

- *"Home support doubles the backend work."* Accepted. The backend interface exists so that
  only one backend is required for 1.0.
- *"The Home observations reflect disabled features, not edition limits."* Agreed. They are
  recorded as a baseline only. ADR 0002 re-checks with the features enabled.

## Consequences

CI and integration tests need a Home edition runner, or documented manual runs on Home.
Edition-specific code paths must be detected at runtime and reported clearly to users.
