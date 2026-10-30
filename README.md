# hyperbutler

> A lightweight, Docker-compatible container runtime for Windows that doesn't need WSL.

**Status: early development, pre-0.1.** `hyperbutler start`, `stop`, `status`, `logs` and `delete`
work end to end on Windows 11 Home, and the stock Windows `docker` CLI runs containers
against it. No release has been published yet. Until one is, build from source
([CONTRIBUTING.md](CONTRIBUTING.md)). Idle memory does not yet meet the 0.1 target (see
[Current status](#current-status)).

hyperbutler boots a minimal Linux VM directly through the Windows Host Compute Service (HCS, the
same API WSL2 uses), runs dockerd inside it, and exposes the Docker API on a Windows named
pipe. The stock `docker` CLI talks to that pipe through a `hyperbutler` docker context. Docker
Compose has not been tested yet.

## Why

Docker on Windows today usually means Docker Desktop on WSL2. Out of the box, that setup has
well-known problems:

- Bind mounts from `C:\` are slow (9P), and file watchers often don't fire.
- Disk space isn't reliably returned to the host. The VHDX keeps growing, and reclaiming the
  space usually means opting into sparse VHDs or compacting the disk by hand.
- `vmmem` can hold on to RAM long after your containers stop. `autoMemoryReclaim` helps
  only partly.
- It's heavy: multiple VMs, proxies, and a GUI you don't need.

hyperbutler aims to fix these by default, with no tuning.

## Goals

| Target | 0.1 preview | 1.0 |
|---|---|---|
| Disk space | returned to the host automatically, no manual compaction, no admin | same |
| WSL | not required, and unaffected by WSL updates | same |
| Start until `docker info` works | ≤ 8 s median | ≤ 5 s |
| Idle host RAM | ≤ 400 MB | ≤ 250 MB |
| Bind-mount I/O | n/a | within 2× of in-VM ext4; metadata within 3× |
| `docker` CLI and Compose | core commands | full |
| Windows editions | Home is first-class | Home, Pro, Enterprise |
| Download size | ≤ 150 MB | same |

These are targets. The next section says which ones are met. For comparison, Docker
Desktop is commonly reported at 2.5–3 GB idle RAM and 20–60 s to start. The full list is in
[docs/requirements.md](docs/requirements.md).

## Current status

Measured on one machine: Windows 11 Home 26200, Core Ultra 7 155H, non-elevated account in
Hyper-V Administrators. The `hyperbutler` CLI rows used a 4 GiB / 8 vCPU guest; rows marked
*spike* come from development harnesses with other guest sizes, as noted. One machine and
one Windows build is not a general result. The details and methods are in the linked
documents.

| Area | Result | Target | Met? |
|---|---|---|---|
| Start, `hyperbutler start` → `docker info` ([cli-engine](docs/research/cli-engine.md#measurements)) | median **1.95 s**, p95 1.96 s (n = 20, warm host cache, existing disk). First start with a new data disk: 1.97 s | ≤ 8 s median, p95 ≤ 12 s | Yes. The first start after a host reboot is not measured yet |
| `hyperbutler stop` | median 1.33 s, orderly guest shutdown every time | none | – |
| Image pull over the internet, *spike*: `hyperbutler-dev`, 1 GiB / 2 vCPU guest ([guest-networking](docs/research/guest-networking.md)) | `alpine` 2.7–3.1 s, `ubuntu:24.04` 3.5–3.7 s, `nginx` 4.7–5.3 s, about the same as the host | none | The LAN-registry comparison of ADR 0006 has not been run |
| Disk space returned after `docker rmi` + `prune` (cli-engine) | 3 GiB of images deleted: the data disk shrank back to within 322 and 843 MiB of the baseline in ≤ 11 s (2 runs), with no manual step | within 1 GB in ≤ 5 min (REQ-DISK-001) | Yes for this test. The full 10 GB / 24 h bench has not been run |
| Host disk full, *spike* ([disk-reclaim](docs/research/disk-reclaim.md#free-space-guard-req-disk-004-non-elevated-2026-10-30)) | with the free-space guard: 10/10 writes got `ENOSPC`, no file system errors, no lost files | REQ-DISK-004 | Yes, on a 3 GiB test volume in a spike harness. Not yet repeated with the `hyperbutler` CLI |
| Idle memory, minutes 5–10 (cli-engine) | **580 MiB**: engine 16 MiB, VM 564 MiB | ≤ 400 MB (REQ-MEM-001) | **No.** The guest keeps its whole root file system in RAM; the planned erofs system disk ([ADR 0005](docs/adr/0005-guest-image.md)) is the fix |
| Memory back to the host after a workload, *spike*: `spike/vhdx`, 3 GiB guest ([guest-services](docs/research/guest-services.md)) | 2 GiB of page cache: VM back to 350 MiB (260 MiB at ready) 27 s after the read | REQ-MEM-002 | Partly: the 12 GB / 8 GB bench has not been run |
| Bind mounts from Windows paths, `docker run -v C:\…` and `--mount type=bind` ([ADR 0007](docs/adr/0007-bind-mounts.md#performance-pass-and-benchfs--2026-10-31)) | work through `hyperbutler start`: read/write, `:ro`, single files; shares no container uses are removed. `bench/fs-*`: cached reads 1.12× ext4, sequential write + fsync 259 MiB/s; lstat 2.1×, missing names 0.9×, `git status` 3.1–6.1× ext4; a Windows edit reaches inotify in p95 4.8 ms with none missed, and pinned Vite and webpack rebuild; 10k creates 11.6 s (ext4: 0.12 s) | 1.0: REQ-FS-001/002/005; REQ-FS-003 ≤ 200 ms p95 | REQ-FS-003 **yes**. REQ-FS-002 **no** (`git status`). REQ-FS-001 reads yes; REQ-FS-001 writes and REQ-FS-005 not measured (no WSL 2 for the 9P baseline) |
| Download size | the release zip is about 80 MB | ≤ 150 MB (REQ-UX-002) | Yes (measured on a local build) |
| Soak (24 h, ADR 0003) | a 13-minute run against `hyperbutler-dev` showed no growth faster than about 14 MB/h | 24 h, no positive trend | **Not run.** It blocks the first release |

What is missing for 0.1: the memory target, the 24-hour soak, the ADR 0004 broker (until it
ships you need Hyper-V Administrators membership, see below), `hyperbutler doctor`, and
Compose testing. Bind mounts from Windows paths work but are not yet fast for write-heavy
trees: use a named volume for `node_modules`. [docs/release.md](docs/release.md) lists every
release gate.

## Quick start

Releases will appear on the [Releases](https://github.com/Higangssh/hyperbutler/releases) page.
There is no installer: hyperbutler is a zip you unpack. The steps below need no admin rights,
except the one-time prerequisites.

### Prerequisites (one time)

1. **Windows 11** (Home, Pro or Enterprise), x64, with virtualization enabled in the
   firmware.
2. **Virtual Machine Platform.** In an elevated PowerShell, then reboot:

   ```powershell
   dism.exe /online /enable-feature /featurename:VirtualMachinePlatform /all /norestart
   ```

3. **Hyper-V Administrators membership** for your account, until the HCS access broker
   ([ADR 0004](docs/adr/0004-hcs-access-broker.md)) ships. Find your account name with
   `whoami` in your own (non-elevated) PowerShell. An administrator then runs this once in
   an elevated PowerShell, with that name in place of `<user>` (if the administrator is a
   different account, `$env:USERNAME` there would be the wrong one), and you sign out and
   back in:

   ```powershell
   net localgroup "Hyper-V Administrators" <user> /add
   ```

   **Security note:** this group has complete, unrestricted access to Hyper-V on the
   machine. Every program you run, not only hyperbutler, can then create VMs and give them
   access to host files, so treat membership as close to admin rights. It is a
   developer-preview arrangement, not the supported end state. When you no longer need it,
   remove it the same way with `/delete` instead of `/add`.

4. **The `docker` CLI**, for example `docker.exe` from Docker's static Windows binaries at
   <https://download.docker.com/win/static/stable/x86_64/>. Docker Desktop is not needed.

WSL is not needed and can stay disabled.

### Install, run, stop

```powershell
# 1. Download hyperbutler-<version>-windows-amd64.zip and SHA256SUMS from the release, then check it:
Get-FileHash .\hyperbutler-*-windows-amd64.zip -Algorithm SHA256   # compare with SHA256SUMS
gh attestation verify .\hyperbutler-*-windows-amd64.zip --repo Higangssh/hyperbutler   # optional

# 2. Unpack it into a fixed folder and put that folder on your user PATH (once). Keep the
#    files together: hyperbutler.exe looks for the guest image next to itself.
$dir = "$env:LOCALAPPDATA\Programs\hyperbutler"
Expand-Archive .\hyperbutler-*-windows-amd64.zip -DestinationPath $env:TEMP\hyperbutler-new -Force
New-Item -ItemType Directory -Force $dir | Out-Null
Copy-Item "$env:TEMP\hyperbutler-new\hyperbutler-*-windows-amd64\*" $dir -Recurse -Force
Remove-Item -Recurse $env:TEMP\hyperbutler-new
[Environment]::SetEnvironmentVariable('Path', [Environment]::GetEnvironmentVariable('Path', 'User') + ";$dir", 'User')
$env:Path += ";$dir"

# 3. Start. The first start creates %LOCALAPPDATA%\hyperbutler and a 64 GiB sparse data disk,
#    and creates and selects the `hyperbutler` docker context.
hyperbutler start
docker run --rm hello-world
docker run -d -p 8080:80 nginx; curl.exe http://127.0.0.1:8080/

hyperbutler status
hyperbutler logs
hyperbutler stop      # orderly guest shutdown
```

To upgrade, run `hyperbutler stop`, repeat step 2 without the PATH line, and `hyperbutler start`. The
data disk in `%LOCALAPPDATA%\hyperbutler` is kept. Upgrades across releases are not tested yet
(there has been no release).

`hyperbutler delete` removes the VM state and the data disk, with all images, containers and
volumes. To uninstall, run `hyperbutler delete`, remove the folder and its PATH entry, and
`docker context rm hyperbutler`.

## Architecture

```
docker CLI ──npipe──▶ hyperbutler (Windows) ──hvsock──▶ guest agent ─▶ dockerd/containerd
                         │                              (minimal Linux VM on HCS)
                         └─ HCS broker service (planned, ADR 0004) for non-admin use
```

Design decisions are recorded in [docs/adr](docs/adr). How releases are built and checked
is in [docs/release.md](docs/release.md).

## Why Go, not Rust?

Rewriting infrastructure in Rust is popular, so here is why hyperbutler is written in Go
([ADR 0003](docs/adr/0003-implementation-language.md) has the details):

- **The network stack is Go.** hyperbutler embeds gvisor-tap-vsock as a library, as Podman does.
- **The Windows container stack is Go.** Microsoft's `hcsshim` and `go-winio`, containerd and
  moby are Go. hyperbutler calls the HCS DLLs directly, which Rust could do too through
  `windows-sys`. What Go adds is the reference implementation and the pipe and hvsock helpers
  in the same language.
- **The cost is small, and we measured it.** The Go host process used 3–7 MB of private
  memory while serving Docker, and 16 MB idle as the full engine. The VM uses hundreds of
  MB. Rust would save a few MB.
- **Neither language prevents memory leaks.** Rust prevents memory-safety bugs, and so does
  Go's garbage collector. Leaks in a long-running daemon are design bugs, in either language.
  hyperbutler bounds every queue and connection set, and every package's tests fail on a leaked
  goroutine. A soak test (`bench/soak`) fails on memory, handle or goroutine growth; its
  24-hour run needs Hyper-V, so it runs on self-hosted hardware, and it blocks any release.

If a measurement ever shows that a component misses its budget because of the language, that
component gets rewritten. The broker's protocol is small and specified for exactly that
reason.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Security issues: see [SECURITY.md](SECURITY.md).

## License

hyperbutler is [Apache-2.0](LICENSE). The release zip also contains a Linux guest image with
GPL-2.0 components (the Linux kernel, BusyBox and others); their corresponding source is
published with each release, and `SOURCE-OFFER.txt` and `THIRD-PARTY/` in the zip list them.

Docker is a trademark of Docker, Inc. hyperbutler is not affiliated with or endorsed by Docker,
Inc. or Microsoft.
