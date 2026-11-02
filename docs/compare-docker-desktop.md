# hyperbutler and Docker Desktop, measured on the same machine

Both engines were installed on one laptop and measured with the same script,
[`bench/compare`](../bench/compare/README.md), one engine at a time, each with its
out-of-the-box settings. The raw CSVs and logs are in
[`data/compare-2026-11-02/`](data/compare-2026-11-02/).

- **Machine:** Windows 11 Home 26200, Intel Core Ultra 7 155H (22 logical processors),
  32 GB RAM, NVMe SSD, a non-elevated account in Hyper-V Administrators.
- **hyperbutler:** a snapshot of commit `ae71701` (the release candidate code, with the
  guest image pinned), defaults: a 4 GiB / 8 vCPU VM.
- **Docker Desktop 4.97.0** (Docker Engine 29.9.2) on WSL 3.0.3 (kernel 6.18), WSL 2
  backend, defaults: WSL gives its VM half the RAM (16 GB) and all 22 processors; Resource
  Saver on.
- **Date:** 2026-11-02. One machine and one build of each: not a general result.

## Results

| | hyperbutler | Docker Desktop 4.97 |
|---|---|---|
| Needs WSL | no | yes (WSL 2) |
| Download | **116 MB** MSI, 109 MB zip | 592 MB installer, plus 351 MB for WSL if it is missing |
| Installed size | **179 MB** | 3,979 MB (Program Files\Docker) + 1,167 MB (WSL) + 1,583 MB data folder after first start |
| Start until a container runs (median of 10, VM stopped before each) | **2.51 s** (2.48–2.59) | 5.64 s (5.54–6.29) |
| Start until `docker info` answers (median of 10) | **2.09 s** | 4.98 s |
| Memory, idle, VM running (private working set of all its processes) | **322 MiB** (median, minutes 5–10) | 1,690–2,660 MiB (minutes 0–6) |
| Memory, idle after Resource Saver stops Docker Desktop's VM (from about minute 7) | n/a (the VM keeps running) | 404 MiB, VM stopped; the next command waits for it to start again |
| Resident processes, idle | **3** (engine, `vmwp`, `vmmem`) | 20 with the VM running, 10 after Resource Saver |
| Disk on the host, fresh install | **0.05 GiB** | 1.54 GiB |
| Disk after pulling 4 images (~5 GB) | 6.35 GiB | 6.79 GiB |
| Disk after `docker rmi` + `system prune -af`, then 5 min | **1.48 GiB**, given back automatically | 6.82 GiB, nothing given back (also after stopping) |
| VM memory after a container read 2 GiB and exited, 2 min later | **320 MiB** (was 335 before) | 1,295 MiB (was 2,178 before) |
| Bind mount from `C:\`: `stat` 2000 files | **39 ms** | 2,349 ms |
| Bind mount: read 2000 × 4 KiB | **419 ms** | 3,420 ms |
| Bind mount: read 512 MiB | **1,050 ms** | 2,699 ms |
| Bind mount: write 2000 small files | **1,750 ms** | 4,870 ms |
| Bind mount: write 512 MiB + fsync | **1,870 ms** | 2,450 ms |
| Price | Apache-2.0, free | free for personal use and small businesses; a paid subscription for larger companies (under 250 employees and under $10 million revenue; [Docker's pricing](https://www.docker.com/pricing/)) |

## What the numbers mean

- **Start.** Both stop their VM before each run (Docker Desktop: `docker desktop stop` and
  `wsl --shutdown`), so each start boots a VM. "Until a container runs" is the fair
  measure: Docker Desktop's backend answers `docker info` even while its VM is stopped, so
  `docker info` alone can report ready before anything can run.
- **Idle memory.** Docker Desktop's VM is sized at half the machine's RAM and its memory use
  depends on what the guest has touched; on this machine it held 1.3–2.2 GB while idle with
  the VM running. Its Resource Saver then stopped the VM after about 7 minutes without
  containers; from then on its host processes alone used 404 MiB, more than hyperbutler with
  its VM running. hyperbutler keeps its VM running and small instead.
- **Disk.** Docker Desktop keeps its images in `docker_data.vhdx`, which grew to 6.8 GiB and
  stayed there after every image was deleted; getting the space back takes a manual compact
  (or opting into a sparse VHD). hyperbutler's data disk is sparse and the guest returns
  freed blocks, so the space came back within 5 minutes without any step.
- **Memory after a workload.** A container wrote and read 2 GiB inside the VM. Two minutes
  after it exited hyperbutler's VM was back at its idle size; Docker Desktop's had given
  back about 3 GB of the 4.3 GB it held but stayed at 1.3 GB.
- **Bind mounts.** A Windows folder mounted into an Alpine container (`-v C:\...:/w`):
  hyperbutler was faster in all five tests, by 1.3× (large write) to 60× (`stat`).

## Not measured

- The time Docker Desktop's first command takes after Resource Saver has stopped its VM.
- Docker Desktop with a smaller VM (`.wslconfig`), or hyperbutler with a larger one.
- Network throughput, `docker build`, Kubernetes (hyperbutler has none), Windows
  containers (hyperbutler has none), GPUs.
- Other machines, Windows 11 Pro, other builds of either.

## Where Docker Desktop is ahead

Docker Desktop is mature and broad: Kubernetes, Windows containers, extensions, a full GUI,
Docker Scout and build cloud integration, support contracts. hyperbutler is a preview that
runs Linux containers with the Docker CLI and Compose, and its own gaps are listed in the
README's status.
