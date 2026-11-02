# bench/compare: hyperbutler and Docker Desktop on the same machine

Runs the same tests against hyperbutler and Docker Desktop (WSL 2 backend), each with its
out-of-the-box settings, on one machine, one engine at a time. The results and the table
in the README are in [docs/compare-docker-desktop.md](../../docs/compare-docker-desktop.md).

| Test | What it measures |
|---|---|
| `start` | One warm-up, then `-Runs` starts (10): stop the engine and wait until its VM process is gone (Docker Desktop: `docker desktop stop` and `wsl --shutdown`), start it (`hyperbutler start`; `docker desktop start --detach`), and time until `docker info` answers. Median, min, max |
| `idle` | Private working set (Task Manager's Memory column) of the engine's processes, every 10 s for `-IdleSeconds` (600) after a start, no containers. Median of minutes 5 to 10, and the VM process alone |
| `disk` | The data disk's size on the host (file length and bytes allocated) before and after pulling `-Images` (about 4 GB), after `docker rmi` and `docker system prune -af`, 5 minutes later, and after stopping |
| `reclaim` | The VM process's private working set before a container fills 2 GiB of page cache, right after it exits, and 2 minutes later |
| `bind` | A Windows folder mounted into an Alpine container: `stat` and `cat` of 2000 4 KiB files, a 512 MiB read, 2000 small writes, a 512 MiB write with fsync. Median of 3 runs |

Which processes count:

- hyperbutler: the engine (`hyperbutler.exe`) and the `vmmem` and `vmwp` of its VM, the
  ones that appeared with the start.
- Docker Desktop: every `Docker Desktop.exe`, `com.docker.*`, `docker-agent.exe`, and the
  WSL 2 processes it runs on (`vmmemWSL`, `wsl.exe`, `wslhost.exe`, `wslrelay.exe`,
  `wslservice.exe`).
- Neither counts `vmcompute.exe`, the Host Compute Service both use.

Both read the same performance counters (`Win32_PerfRawData_PerfProc_Process`). The
sampling runs a CIM query every 10 s for both engines alike.

```powershell
powershell -File bench\compare\run.ps1 -Engine hyperbutler -Out OUT -Hyperbutler PATH\hyperbutler.exe -HbHome DIR
powershell -File bench\compare\run.ps1 -Engine desktop -Out OUT
```

It needs Hyper-V and Docker Desktop installed, so CI does not run it. It pulls about 4 GB
per engine.
