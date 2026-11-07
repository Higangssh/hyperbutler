# Using hyperbutler

The README has the short version; this is the long one: prerequisites, installing,
the docker CLI and Compose, ports, settings, the window, AI tools, troubleshooting and
reaching the host.

## Quick start

Downloads are on the [Releases](https://github.com/Higangssh/hyperbutler/releases) page.
**The easy way:** the MSI does the prerequisites and PATH for you in one UAC prompt, and
installs the hyperbutler broker service, which starts the VM for you: no Hyper-V
Administrators membership and no elevation afterwards. Restart if it asks, then run
`hyperbutler start` ([docs/install.md](install.md)). The broker service in the MSI
is a preview: its install, upgrade from v0.1.0-preview.1, repair, rollback and uninstall
ran on one machine, where an account outside Hyper-V Administrators started the VM and
ran containers through it (docs/install.md has the run); a clean machine is next. The steps below are for the zip,
which needs no installer and no admin rights, except the one-time prerequisites.

## Prerequisites (one time)

1. **Windows 11** (Home, Pro or Enterprise), x64, with virtualization enabled in the
   firmware. The release is x64 only: on ARM64 the x64 programs would run under
   emulation, but the x86_64 guest image cannot boot there. Windows 10 is not supported:
   its mainstream support has ended and hyperbutler is not tested there. The APIs it uses
   exist on Windows 10 22H2, so it may work; reports are welcome, fixes are not promised.
2. **Virtual Machine Platform.** In an elevated PowerShell, then reboot:

   ```powershell
   dism.exe /online /enable-feature /featurename:VirtualMachinePlatform /all /norestart
   ```

3. **Hyper-V Administrators membership** for your account: the zip runs the VM in its
   own process (developer mode), since it installs no broker service (the MSI does,
   [ADR 0004](adr/0004-hcs-access-broker.md)). Find your account name with
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
   developer arrangement, not the supported one: the MSI's broker service needs none.
   When you no longer need it, remove it the same way with `/delete` instead of `/add`.

4. Nothing else: the zip comes with the `docker` CLI and the compose plugin (in its
   `clients\` folder, built from Docker's source). A docker CLI you already have, 19.03 or
   later (Docker Desktop's, or any other), works too. Docker Desktop is not needed.

WSL is not needed and can stay disabled.

## Install, run, stop

```powershell
# 1. Download hyperbutler-<version>-windows-amd64.zip and SHA256SUMS from the release, then check it:
Get-FileHash .\hyperbutler-*-windows-amd64.zip -Algorithm SHA256   # compare with SHA256SUMS
gh attestation verify .\hyperbutler-*-windows-amd64.zip --repo Higangssh/hyperbutler   # optional

# 2. Unpack it into a fixed folder and put that folder on your user PATH (once). Keep the
#    folders together: hyperbutler.exe boots the guest image in the image\ folder next to
#    it. Leave image\ alone; Explorer offers to mount system.vhd if you double-click it.
$dir = "$env:LOCALAPPDATA\Programs\hyperbutler"
Expand-Archive .\hyperbutler-*-windows-amd64.zip -DestinationPath $env:TEMP\hyperbutler-new -Force
New-Item -ItemType Directory -Force $dir | Out-Null
Copy-Item "$env:TEMP\hyperbutler-new\hyperbutler-*-windows-amd64\*" $dir -Recurse -Force
Remove-Item -Recurse $env:TEMP\hyperbutler-new
[Environment]::SetEnvironmentVariable('Path', [Environment]::GetEnvironmentVariable('Path', 'User') + ";$dir", 'User')
$env:Path += ";$dir"
#    Only if `docker` is not found yet (Get-Command docker), also add the bundled CLI:
[Environment]::SetEnvironmentVariable('Path', [Environment]::GetEnvironmentVariable('Path', 'User') + ";$dir\clients", 'User')
$env:Path += ";$dir\clients"

# 3. Start. The first start creates %LOCALAPPDATA%\hyperbutler and a 64 GiB sparse data disk,
#    and creates and selects the `hyperbutler` docker context.
hyperbutler start
docker run --rm hello-world
docker run -d -p 8080:80 nginx; curl.exe http://127.0.0.1:8080/

hyperbutler status
hyperbutler logs
hyperbutler stop      # orderly guest shutdown
```

The engine runs in the background until `hyperbutler stop` or sign-out; closing the terminal
does not stop it. `stop`, `restart`, `config daemon apply` and a `config set ... --apply`
that restarts dockerd or the VM first list the running containers that stop, and which
come back afterwards: those with `--restart always` or `unless-stopped`. `on-failure`
brings one back only if it stops with an error (a container killed because it ignored
the stop signal does); one without a policy stays stopped. They do not ask: the command
is the request. After `stop`, docker commands fail until the next `hyperbutler start`
(see [Troubleshooting](#troubleshooting-and-uninstalling)).

Double-clicking `hyperbutler.exe` in Explorer (or a shortcut to it) opens a window with
the status. If hyperbutler is stopped, Enter starts it; the engine keeps running after
the window closes. Nobody answering starts nothing: the start prompt gives up after
5 minutes, and "Press Enter to close." after 1 minute. From a terminal, `hyperbutler`
with no arguments prints the commands. Scripts and tools that start `hyperbutler.exe`
without arguments in a window of its own (`Start-Process -Wait`, `start /wait`, a
scheduled task, `wt.exe hyperbutler`) can set `HYPERBUTLER_NO_LAUNCHER=1` to get the
commands at once instead of the window.

To upgrade, run `hyperbutler stop`, repeat step 2 without the PATH lines (it replaces every
file, `image\` included), and `hyperbutler start`. The data disk in `%LOCALAPPDATA%\hyperbutler` is kept. Upgrades across releases are not tested yet
(this is the first one). `hyperbutler status` shows which `hyperbutler.exe` the running
engine is and its version. `hyperbutler start` while an engine from another folder or
another version runs says so; `hyperbutler restart` from the new folder switches to it.

A release `hyperbutler.exe` boots only the guest image files it was released with, the
ones in `image\` next to it; `hyperbutler version` shows the `image.json` hash it expects.
If it refuses them (a half-copied upgrade, a file from another version), extract the zip
again into an empty folder. Image files directly next to `hyperbutler.exe` are not used:
`hyperbutler start` names them (an archive extracted over an older layout leaves them)
so you can delete them.

## The docker CLI, compose and Docker Desktop

- `hyperbutler start` and `hyperbutler status` show which `docker.exe` a plain `docker`
  runs, marked `(bundled)` when it is the one in `clients\`. hyperbutler never changes your
  PATH and never copies or replaces a docker CLI. A CLI older than 19.03 (API 1.40, the
  oldest the guest's Docker Engine 29 accepts) gets a warning with the fix.
- The docker CLI finds plugins only in `cliPluginsExtraDirs` from its `config.json`, then
  `%USERPROFILE%\.docker\cli-plugins`, then `%ProgramFiles%\Docker\cli-plugins` (first match
  wins), never next to `docker.exe`. So `hyperbutler start` adds `clients\cli-plugins` to
  `cliPluginsExtraDirs`, but only while no other `docker-compose.exe` is in those places,
  and takes it out again when one appears. Docker Desktop's or your own compose always
  wins. With `context.manage false`, nothing is added; run
  `docker compose` from Docker Desktop, or add the folder to `cliPluginsExtraDirs` yourself.
- Docker Desktop can run at the same time: hyperbutler serves `\\.\pipe\docker_engine_hyperbutler`,
  not Desktop's `docker_engine`, and only adds its own `hyperbutler` docker context. `start`
  selects it (and prints the one it replaced); switch back with `docker context use
  desktop-linux`. `start` says when Docker Desktop's engine runs too and which one a plain
  `docker` now reaches; `status` names the selected context when it is not `hyperbutler`.
  See [docs/compat.md](compat.md#the-docker-cli-and-docker-desktop).
- Tools that ignore docker contexts (Testcontainers, the Docker SDKs, IDEs) and moving
  images and volumes over from Docker Desktop: [docs/tools.md](tools.md).
- `hyperbutler import --from-desktop` copies the images and volumes you pick from a running
  Docker Desktop, verifies each copy and leaves Docker Desktop as it was (`--dry-run` lists).
  `--container NAME` and `--all-containers` also create again the containers made with
  `docker run`, with their image and volumes, and do not start them: stop Docker Desktop's
  first if it holds the same ports, then `docker start` the new one. For a Compose project,
  run `docker compose up` in its folder. What a container wrote outside its volumes is not
  copied, and one with host access waits for `--confirm-container NAME`. See
  [docs/tools.md](tools.md#containers-req-ux-024).

## Published ports and other users

- `docker run -p 8080:80` listens on `127.0.0.1:8080` and `[::1]:8080` (so `localhost`
  works), not on the LAN. If another program (Docker Desktop, a dev server, another user's
  hyperbutler) already listens on that port on any address, `docker run` fails with
  Docker's own "Ports are not available" error and names the process. A container started
  by a restart policy keeps running without its ports then, and `hyperbutler status` lists
  them with the reason. See [docs/compat.md](compat.md#published-ports).
- Each Windows user has their own engine, data disk and settings. Published ports are on
  the machine's loopback and are shared by all users of the PC. Pipe names are shared
  too: if `start` says the pipe is taken, give one installation its own with
  `hyperbutler config set pipe \\.\pipe\docker_engine_<name>`.

## Settings

```powershell
hyperbutler config list                      # every setting: value, default, source, when it applies
hyperbutler config set memory 8GiB           # says what restart it needs; --apply does it
hyperbutler config set context.manage false  # do not create or select the docker context
hyperbutler config set autostart true        # start at sign-in (one Task Scheduler task of yours)
hyperbutler config set updateCheck false     # no daily look for a newer release on GitHub
hyperbutler config path                      # settings.json and daemon.json
hyperbutler restart

# Docker Engine settings: put a daemon.json at the path above, then
hyperbutler config daemon apply              # restarts dockerd only, not the VM
```

`daemon.json` reaches the guest's dockerd byte for byte. Keys hyperbutler owns (`hosts`,
`iptables`, `data-root`, `live-restore` and a few more) are refused with the reason, and an
invalid file leaves the previous one in effect. For example, a registry that speaks plain
HTTP:

```json
{ "insecure-registries": ["192.168.1.20:5000"] }
```

Behind a corporate proxy, dockerd trusts the CA certificates of the Windows root stores
(`ca.import`, on by default; a PEM file can be added with `ca.extraFile`, which is copied
when it is set, so set it again after the file changes), and takes a proxy:

```powershell
hyperbutler config set proxy.https http://proxy.corp:3128
hyperbutler config set proxy.http  http://proxy.corp:3128 --apply   # restarts dockerd
```

A proxy on this machine's `127.0.0.1`, such as px or cntlm for NTLM or Kerberos, works as
it is. Passwords in proxy URLs are shown as `***`. This is for dockerd's own pulls and
pushes: as with Docker Desktop, containers and `docker build` do not get the CA or the
proxy, so add the certificate to the image and set the proxy for builds (the docker CLI's
`proxies` in `config.json`).

`status`, `version`, `config list/get/path`, `doctor`, `diagnose` and `uninstall` take `--json`; every
document carries `"schema": 1`. See [docs/research/settings.md](research/settings.md).

## The window

`hyperbutler gui` (or a double-click on `hyperbutler.exe`) opens a small window: the engine's
state with Start and Stop, containers by Compose project with their logs, storage,
settings with `daemon.json`, and troubleshooting, in English or Korean. It uses the
WebView2 Runtime that is part of Windows 11 (`WebView2Loader.dll` ships next to the exe),
makes no network requests, starts nothing until you press Start, and leaves nothing
running when you close it; the engine keeps running. See [docs/gui.md](gui.md).

## AI tools (MCP)

`hyperbutler mcp` is a Model Context Protocol server on stdio for an MCP client such as
your editor's AI assistant: `"command": "hyperbutler.exe", "args": ["mcp"]`. It is
read-only (status, containers, logs, images, volumes, disk and memory use, settings,
diagnose); `--allow-write` adds start, stop, run, pull and `config_set`, and
`--allow-delete` the deletes. Results are redacted and bounded, and it exits with its
client. See [docs/mcp.md](mcp.md).

## Troubleshooting and uninstalling

```powershell
hyperbutler doctor                   # what hyperbutler needs from this PC, read-only, with fixes
hyperbutler diagnose                 # a local zip for a bug report; nothing is uploaded
hyperbutler uninstall --dry-run      # everything hyperbutler created, and what it keeps
hyperbutler uninstall                # removes it (--keep-data keeps images and volumes)
```

**docker cannot connect after `hyperbutler stop` or a reboot.** With the `hyperbutler`
context selected and the engine stopped, every docker command fails with:

```
failed to connect to the docker API at npipe:////./pipe/docker_engine_hyperbutler; check if the path is correct and if the daemon is running: open //./pipe/docker_engine_hyperbutler: The system cannot find the file specified.
```

Nothing listens on the pipe, so the docker CLI cannot say more. Run `hyperbutler start`
(it starts at sign-in only with `config set autostart true`), or use another engine: `docker context use
desktop-linux` for Docker Desktop, `docker context use default` for `DOCKER_HOST` or
`\\.\pipe\docker_engine`. `docker context ls` shows the hint in the context's
description. With another pipe set (`config set pipe`), the error names that pipe.

**docker commands hang.** If the volume that holds the data disk is almost full, the
free-space guard pauses the VM until space is freed. `hyperbutler status` says so:
`paused:  C: has 900.0 MiB free; it resumes at 1.2 GiB free`. Free space on that volume
and the VM goes on where it stopped; nothing is lost.

`diagnose` redacts passwords, tokens, registry auth and proxy credentials, but keeps paths
(with your user name): look through the zip before you attach it. `uninstall` stops
hyperbutler, removes its state, data disk, logs and docker context (selecting the context
you used before), and leaves anything it did not create. It asks for confirmation; where
input is not a Windows console (Git Bash, mintty, VS Code tasks) pass `--yes`. It does not
remove the hyperbutler folder or its PATH entry: delete those yourself.

`hyperbutler delete` removes only the VM state and the data disk, with all images,
containers and volumes, and keeps the settings. Both `delete` and `uninstall` take
`clients\cli-plugins` out of the docker CLI's `config.json`.

## Reaching the host from a container

As in Docker Desktop, `host.docker.internal` (and `--add-host NAME:host-gateway`) reaches
services on this machine's `127.0.0.1` (or `[::1]`) on any port, TCP and UDP. It reaches
nothing else: not the host's other addresses, not the LAN.

**Every container can then reach every service on your loopback**, including ones that
trust loopback clients (database admin ports, dev servers, other tools' local APIs, a
Docker Engine exposed on `tcp://localhost:2375`). The Windows firewall does not filter
loopback. To turn it off or limit it, at once and without a restart:

```powershell
hyperbutler config set hostGateway.enabled false
hyperbutler config set hostGateway.ports 5432,8000-8100   # "all" is the default
```

See [docs/compat.md](compat.md#hostdockerinternal).

