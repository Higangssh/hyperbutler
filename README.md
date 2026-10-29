# winbox

> A lightweight, Docker-compatible container runtime for Windows that doesn't need WSL.

**Status: early development. Nothing works yet.**

winbox boots a minimal Linux VM directly through the Windows hypervisor (HCS or the
Windows Hypervisor Platform), runs containerd/dockerd inside it, and exposes the Docker API
on a Windows named pipe. Your existing `docker` CLI and Compose files keep working.

## Why

Docker on Windows today usually means Docker Desktop on WSL2. Out of the box, that setup has
well-known problems:

- Bind mounts from `C:\` are slow (9P), and file watchers often don't fire.
- Disk space isn't reliably returned to the host. The VHDX keeps growing, and reclaiming the
  space usually means opting into sparse VHDs or compacting the disk by hand.
- `vmmem` can hold on to RAM long after your containers stop. `autoMemoryReclaim` helps
  only partly.
- It's heavy: multiple VMs, proxies, and a GUI you don't need.

winbox aims to fix these by default, with no tuning.

## Goals

| Target | Goal |
|---|---|
| Cold start | ≤ 3 s |
| Idle RAM | ≤ 150 MB |
| Bind-mount I/O | within 2× native |
| Disk space | returned to the host automatically, no manual compaction |
| `docker` CLI | fully compatible |
| Windows editions | Home is first-class, as well as Pro/Enterprise |

These are targets, not measured results. The full list is in
[docs/requirements.md](docs/requirements.md).

## Planned usage

```powershell
winbox start      # boots the VM and registers a `winbox` docker context
docker run --rm hello-world
winbox stop
```

## Architecture (planned)

```
docker CLI ──npipe──▶ winbox (Windows) ──hvsock──▶ guest agent ─▶ containerd/dockerd
                         │                              (minimal Linux VM)
                         └─ VM backend: HCS | WHP | QEMU-WHPX (prototype)
```

Design decisions are recorded in [docs/adr](docs/adr).

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Security issues: see [SECURITY.md](SECURITY.md).

## License

[Apache-2.0](LICENSE)

Docker is a trademark of Docker, Inc. winbox is not affiliated with or endorsed by Docker,
Inc. or Microsoft.
