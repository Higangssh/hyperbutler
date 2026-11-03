<p align="center">
  <img alt="hyperbutler" src="assets/brand/logo-banner.webp" width="640">
</p>

<h3 align="center">Run Docker on Windows with less RAM, less disk, and no WSL.</h3>

<p align="center">
  <a href="https://github.com/Higangssh/hyperbutler/actions/workflows/ci.yml"><img alt="CI" src="https://github.com/Higangssh/hyperbutler/actions/workflows/ci.yml/badge.svg"></a>
  <a href="https://github.com/Higangssh/hyperbutler/releases"><img alt="Release" src="https://img.shields.io/github/v/release/Higangssh/hyperbutler?include_prereleases&label=release"></a>
  <img alt="Windows 11" src="https://img.shields.io/badge/Windows-11-0078D4">
  <a href="LICENSE"><img alt="License: Apache-2.0" src="https://img.shields.io/badge/license-Apache--2.0-blue"></a>
</p>

<p align="center">
  <a href="https://hyperbutler.dev">Website</a> ·
  <a href="https://github.com/Higangssh/hyperbutler/releases/latest">Download</a> ·
  <a href="docs/usage.md">Docs</a> ·
  <a href="docs/status.md">Status</a>
</p>

hyperbutler is an open-source, Docker-compatible engine for Windows. It boots a small Linux VM
directly through the Windows Host Compute Service (the API WSL 2 uses), runs dockerd in it,
and lets the stock `docker` CLI and Docker Compose talk to it. No WSL, no Docker Desktop.

> [!NOTE]
> **Preview.** v0.1.0-preview.1 works end to end on Windows 11 Home. v0.1.0 waits on a
> 24-hour soak test and an install test on a clean machine ([status](docs/status.md)).
> Not for production yet.

## Highlights

- **No WSL.** The VM runs on Hyper-V's Host Compute Service. WSL can stay off, and its updates
  never touch hyperbutler.
- **Light.** 322 MiB of memory at idle, a 116 MB download, 179 MB installed.
- **Disk space comes back by itself.** Delete images and the data disk shrinks. No
  compaction, no admin step.
- **Fast bind mounts from `C:\`.** Its own file server reads 2000 small files in 0.4 s
  (Docker Desktop: 3.4 s), and file watchers fire.
- **Your tools, unchanged.** The stock `docker` CLI, Compose, SDKs and VS Code, through a
  `hyperbutler` docker context. Both clients are bundled.
- **No admin rights for daily use.** A small Windows service is the only privileged part, and
  it only starts and stops your VM ([how](#how-it-works)).
- **Safe for AI agents.** A built-in MCP server lets an AI assistant read and, if you allow it,
  manage your containers, with limits it cannot lift.
- **Free for every company size.** Apache-2.0, no telemetry.

## Measured against Docker Desktop

Same laptop (Windows 11 Home, Core Ultra 7 155H, 32 GB), out-of-the-box settings, the same
script, one engine at a time ([method and raw data](docs/compare-docker-desktop.md)).

| | hyperbutler | Docker Desktop 4.97 (WSL 2) |
|---|---|---|
| Needs WSL | **no** | yes |
| Download | **116 MB** | 592 MB (+351 MB WSL) |
| Installed size | **179 MB** | about 6.7 GB |
| Start until a container runs | **2.5 s** | 5.6 s |
| Idle memory, VM running | **322 MiB** | 1.7–2.7 GB |
| Disk 5 min after deleting ~5 GB of images | **1.5 GiB**, given back automatically | 6.8 GiB, kept |
| VM memory 2 min after a 2 GiB workload | **320 MiB** | 1.3 GB |
| Bind mount from `C:\`: `stat` / read / write 2000 small files | **0.04 / 0.4 / 1.8 s** | 2.3 / 3.4 / 4.9 s |

Docker Desktop is ahead on breadth: Kubernetes, Windows containers, extensions and a mature
GUI. One machine is not a general result.

## Install

You need Windows 11 (Home, Pro or Enterprise) on x64 with virtualization enabled in the
firmware.

1. Download the MSI from the [latest release](https://github.com/Higangssh/hyperbutler/releases/latest)
   and run it. One UAC prompt; it sets up the PATH and turns on Virtual Machine Platform if
   it is off.
2. Restart if it asks.

Builds are not code-signed yet: SmartScreen says "Windows protected your PC", so choose
**More info** → **Run anyway**, after checking the file against `SHA256SUMS` from the release.
Prefer no installer? The zip works too ([install guide](docs/install.md)).

## Quick start

```powershell
hyperbutler start                 # boots the VM; the Docker API answers in about 1.5 s
docker run --rm hello-world
docker compose up -d              # in a folder with a compose.yaml
hyperbutler status                # is it running, and which docker it serves
hyperbutler stop
```

Everything else, from published ports to troubleshooting, is in the [docs](docs/usage.md).

## Features

**Docker CLI and Compose.** The bundled `docker` CLI and compose plugin are built from pinned
source; your own docker CLI works too. Builds, healthchecks, service names, named volumes,
bind mounts and published ports are tested end to end (`bench/compose`).
[More](docs/usage.md#the-docker-cli-compose-and-docker-desktop)

**Bind mounts and file watching.** `-v C:\src:/src` goes through hyperbutler's own file server;
a change on Windows reaches inotify in a few milliseconds, so Vite and webpack rebuild.
[ADR 0007](docs/adr/0007-bind-mounts.md)

**Networking.** A user-mode network stack: published ports on `localhost`,
`host.docker.internal`, DNS and NAT, with no virtual switch and no admin rights.
[More](docs/usage.md#published-ports-and-other-users)

**Settings and `daemon.json`.** `hyperbutler config` sets memory, CPUs and disk size; a
`daemon.json` reaches dockerd (registry mirrors, insecure registries…) and applies without
restarting the VM. [More](docs/usage.md#settings)

**The window.** `hyperbutler gui` shows the engine, containers by Compose project with their
logs, storage, settings and troubleshooting, in English or Korean. It makes no network
requests. [docs/gui.md](docs/gui.md)

**AI tools (MCP).** `hyperbutler mcp` lets an MCP client (your editor's AI assistant) read
status, containers, logs and disk use. Starting, stopping and deleting need flags you add,
and what it may run is fenced in. [docs/mcp.md](docs/mcp.md)

**Moving from Docker Desktop.** `hyperbutler import --from-desktop` copies images and volumes
over. [docs/tools.md](docs/tools.md)

**When something is wrong.** `hyperbutler doctor` checks every prerequisite and says how to
fix it (`--fix` repairs what it may); `hyperbutler diagnose` packs a redacted report.
[More](docs/usage.md#troubleshooting-and-uninstalling)

## How it works

```mermaid
flowchart TB
  subgraph WIN["🪟 Windows · your account, no admin rights"]
    direction LR
    tools["<b>Your tools</b><br/>docker CLI<br/>Compose · SDKs"]
    engine["<b>hyperbutler engine</b><br/>Docker API relay<br/>user-mode network: NAT, DNS, ports<br/>file server for bind mounts<br/>disk-space guard · status · window"]
    tools -- "named pipe" --> engine
  end
  subgraph SVC["🔒 Windows service · SYSTEM"]
    broker["<b>hyperbutler broker</b><br/>the only part that calls HCS<br/>7 typed requests<br/>fixed VM template"]
  end
  subgraph VM["🐧 Linux VM on Hyper-V · no WSL"]
    direction TB
    agent["<b>hyperbutlerd</b> guest agent<br/>admits only the VM's secret"]
    dockerd["<b>dockerd + containerd</b><br/>read-only system disk, hash-pinned<br/>sparse data disk that gives space back"]
    agent --> dockerd
  end
  engine <== "Hyper-V sockets · API, network, files" ==> agent
  engine -- "start · stop · status" --> broker
  broker -- "HCS" --> VM

  classDef eng fill:#0b2a4a,stroke:#3d9bff,color:#e8f1ff
  classDef brk fill:#3a2a06,stroke:#f5b83d,color:#fff4dc
  classDef box fill:#141b26,stroke:#5b6b82,color:#e8edf5
  class engine eng
  class broker brk
  class tools,agent,dockerd box
```

- **Engine** (`hyperbutler.exe`), runs as you: relays the Docker API, runs the VM's network in
  user mode, serves bind mounts, and keeps the host disk from filling.
- **Broker service** (`hyperbutler-broker.exe`), the only privileged part: it starts and stops
  your VM through HCS, from a fixed template, on seven short typed requests over a pipe only
  you can open. That is why you need neither admin rights nor the Hyper-V Administrators
  group. It links nothing but the Go standard library and `x/sys`, and its design and reviews
  are in [ADR 0004](docs/adr/0004-hcs-access-broker.md).
- **Guest**, a small Alpine VM: dockerd and containerd on a read-only, hash-pinned system disk,
  and a sparse data disk that gives freed space back to Windows.

Design decisions are in [docs/adr](docs/adr); how releases are built and checked is in
[docs/release.md](docs/release.md).

## FAQ

**Is this Docker?** It runs Docker's own engine (dockerd, containerd) in a Linux VM and speaks
the Docker API, so Docker's tools work with it. It is not made by Docker, Inc.

**How is it different from Docker Desktop?** Lighter (see the table), no WSL, gives disk and
memory back without tuning, no account, free at any company size. Docker Desktop has
Kubernetes, Windows containers and extensions; hyperbutler does not.

**Windows containers? Windows 10?** No to both: Linux containers on Windows 11 only.

**Do I need to be an administrator?** Once, for the MSI. After that, no: from v0.1.0 the broker
service starts your VM for you (v0.1.0-preview.1 still adds you to Hyper-V Administrators). The zip, without the service, needs Hyper-V Administrators
membership ([install guide](docs/install.md)).

**Why Go?** The container stack (containerd, moby, gVisor's network stack) is Go, and the cost
is measured: the engine idles at about 18 MB. Leaks are design bugs in any language, so every
package's tests fail on a leaked goroutine and a 24-hour soak gates releases.
[ADR 0003](docs/adr/0003-implementation-language.md)

## Contributing

Issues and pull requests are welcome: see [CONTRIBUTING.md](CONTRIBUTING.md). Security issues:
[SECURITY.md](SECURITY.md).

## License

hyperbutler is [Apache-2.0](LICENSE). The release also contains a Linux guest image with
GPL-2.0 components (the Linux kernel, BusyBox and others); their corresponding source is
published with each release, and `SOURCE-OFFER.txt` and `THIRD-PARTY/` list them.

Docker is a trademark of Docker, Inc. hyperbutler is not affiliated with or endorsed by Docker,
Inc. or Microsoft.
