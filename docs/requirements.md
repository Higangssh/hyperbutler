# Requirements

> Draft. These will be expanded from the WSL2/Docker pain-point research in
> `docs/research/`. IDs are stable once published. Don't renumber them.

ID format: `REQ-<AREA>-NNN`. Areas: `START`, `MEM`, `DISK`, `FS`, `NET`, `API`, `PLAT`, `UX`.
Priority: **P0** = required for 0.1, **P1** = required for 1.0, **P2** = nice to have.

| ID | Requirement | Acceptance criterion (how measured) | Priority |
|---|---|---|---|
| REQ-DISK-001 | Disk space freed in the guest is returned to the host automatically. | After writing and then deleting 10 GB of images, host disk usage returns to within 1 GB of the baseline in ≤ 5 min, with no manual compaction (`bench/disk`). | P0 |
| REQ-START-001 | Fast cold start. | `winbox start` until `docker info` succeeds takes ≤ 3 s, median of 10 runs (`bench/start`). | P0 |
| REQ-MEM-001 | Small idle footprint. | Host memory attributed to the VM is ≤ 150 MB after 5 min idle with no containers (`bench/mem`). | P0 |
| REQ-FS-001 | Fast bind mounts from Windows paths. | Bind-mount I/O is within 2× native for the `bench/fs` suite. | P1 |
| REQ-API-001 | Docker CLI compatibility. | The stock `docker` and `docker compose` CLIs pass the `bench/compat` suite through the `winbox` context. | P0 |
| REQ-PLAT-001 | Works on Windows 11 Home. | All P0 criteria pass on Home ([ADR 0001](adr/0001-windows-home-first-class.md)). | P0 |

## Non-goals

- Windows containers.
- A GUI or tray app.
