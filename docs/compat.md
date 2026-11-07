# Docker compatibility notes

Known differences from Docker Desktop and Docker Engine on Linux (REQ-API-001). Only the
networking, docker CLI, client tools and proxy notes exist so far. Everyday situations (the engine
stopped, sign-out and sleep, Docker Desktop running at the same time, other tools, other
users, upgrades) are walked through in
[research/usage-scenarios.md](research/usage-scenarios.md).

## host.docker.internal

REQ-NET-004, [ADR 0006, "Host gateway"](adr/0006-guest-networking.md#host-gateway-req-net-004).

| | Docker Desktop | hyperbutler |
|---|---|---|
| `host.docker.internal` resolves in every container and on the guest | yes | yes, to `192.168.127.254`; answered by the engine's DNS, never by the host's resolver or hosts file |
| `gateway.docker.internal` | yes | yes, the same address (an alias) |
| `--add-host NAME:host-gateway` | yes | yes (dockerd runs with `--host-gateway-ip=192.168.127.254`) |
| TCP to it on port P reaches | the host | `127.0.0.1:P` on Windows, or `[::1]:P` if `127.0.0.1:P` refuses |
| UDP to it on port P reaches | the host | `127.0.0.1:P` only (no `[::1]` fallback: UDP cannot tell that nothing listens) |
| The host's LAN, VPN and other addresses through it | not checked | no: only loopback, on the same port |
| AAAA for the name | not checked | none (the guest network is IPv4 only) |
| Turn it off, limit ports | not that we know of | `hostGateway.enabled`, `hostGateway.ports`; live, no restart |
| Concurrent connections through it | not checked | 256 TCP, 256 UDP flows; more are refused (reset) |

**Security.** With the default (on, every port), **every container can reach every
service listening on the Windows host's loopback**, as on Docker Desktop. Programs often
trust loopback clients: database and cache servers without passwords, development
servers, admin and debug ports of other tools, a Docker Engine exposed on
`tcp://localhost:2375`. A compromised or malicious image, or a page that a browser in a
container loads, can use them. The Windows firewall does not filter loopback. Limit it
with `hyperbutler config set hostGateway.ports 5432,8000-8100`, or turn it off with
`hyperbutler config set hostGateway.enabled false`.

hyperbutler's own endpoints are not on TCP: the Docker API, the debug endpoint and the
control channel are a named pipe and hvsock. Ports published with `docker run -p` are on
`127.0.0.1` and are reachable through `host.docker.internal` too, as on Docker Desktop.

When it is off, connections to `192.168.127.254` are refused, the names give NXDOMAIN
(not the host's hosts file, where Docker Desktop writes the host's LAN address), and
`--add-host NAME:host-gateway` still maps NAME to `192.168.127.254`, which then refuses.

**Not covered:** a `docker network create --subnet` that overlaps `192.168.127.0/24` cuts
the guest off from the host gateway (daemon.json's `bip`, `fixed-cidr` and
`default-address-pools` are checked; `docker network create` is not).

## Published ports

REQ-NET-001; the collision handling is the proposed REQ-NET-005
([usage-scenarios.md, S3.4](research/usage-scenarios.md#s34-both-publish-the-same-port)).
Code: `internal/engine/publish.go`, `internal/engine/net_windows.go` (auto-publish).

| | Docker Desktop | hyperbutler |
|---|---|---|
| `-p 8080:80` listens on | `0.0.0.0:8080` (all host addresses) | `127.0.0.1:8080` and `[::1]:8080`; the LAN needs REQ-NET-003 (not built) |
| A port a host program already listens on (any address: wildcard, loopback, LAN IP) | `docker run` fails: `Ports are not available: exposing port TCP 0.0.0.0:8080 -> 0.0.0.0:0: listen tcp 0.0.0.0:8080: bind: Only one usage of each socket address ...` | `docker run` and `docker start` fail the same way, before dockerd starts the container (exit 125, the container stays `Created`): `Ports are not available: exposing port TCP 127.0.0.1:8080 -> 0.0.0.0:0: listen tcp 127.0.0.1:8080: bind: Only one usage of each socket address ... (another program, process N, listens on ADDR)` |
| One of the two loopbacks taken | n/a | neither is published (no split between `127.0.0.1` and `localhost`/`[::1]`) |
| `-p 127.0.0.1:8080:80` (or `::1`, common in Compose files) | `127.0.0.1:8080` on Windows | `127.0.0.1:8080` and `[::1]:8080` on Windows, as `-p 8080:80` (since v0.1.2; before, not published at all). The engine binds it to the guest's own address in the VM, where a loopback binding would be out of its reach, so `docker port` and `docker ps` show `192.168.127.2:8080`, and other containers in the VM can reach it there |
| A port in a Windows excluded port range (Hyper-V and WinNAT reserve ranges; `netsh int ipv4 show excludedportrange protocol=tcp` lists them) | `docker run` fails: `Ports are not available: exposing port TCP 0.0.0.0:P -> 0.0.0.0:0: listen tcp 0.0.0.0:P: bind: An attempt was made to access a socket in a way forbidden by its access permissions.` | `docker run` and `docker start` fail before the start with the same bind error for `127.0.0.1:P` (or `[::1]:P`), then `(Windows reserves this port: it is in an excluded port range ...)` with the `netsh` command that lists the ranges |
| Two containers publishing one host port on different guest addresses (`-p 8080:80` and `-p GUESTIP:8080:80`) | the second fails at the host bind | the second fails before the start, ending `(another container of this engine publishes host port 8080)` |
| A container started by a restart policy, `docker restart`, or after a host program took the port between the check and the bind | the start fails | the container runs, **none** of its ports is published, `engine.log` says why, and `hyperbutler status` (and `status --json`, `state.unpublishedPorts`) lists each port with the reason until the container stops |
| Random ports (`-P`, `-p 80`) and host-port ranges | checked at bind | not checked before the start (dockerd picks the port then); published all-or-nothing after it, as above |
| UDP (`-p 53:53/udp`) | published | not mirrored on the host yet |
| IPv6 loopback disabled on the host | n/a | `127.0.0.1` alone counts as published |

How a taken port is found: the engine reads the system's TCP listener tables
(`GetExtendedTcpTable`) and opens no socket for it. Windows lets a wildcard listener
(`0.0.0.0:P`) and a specific one (`127.0.0.1:P`) coexist, so a successful bind would not
prove the port free, and a probe bind of the wildcard address can raise the Windows
Firewall "allow access" prompt. hyperbutler never listens on a wildcard address. When
the tables say the port is free, it also binds `127.0.0.1:P` and `[::1]:P` and closes
them at once: a port in an excluded range is in no table, but refuses that bind (with
`WSAEACCES`). If the tables cannot be read, that bind is the whole check, which catches
a loopback listener but not one on the wildcard or the LAN address.

The start check runs in the engine's Docker API proxy, so it covers every client that
uses the pipe (the docker CLI, compose, SDKs). A program that takes the port in the
moment between the check and the bind still wins; then the container runs unpublished
and `status` says so.

## The docker CLI and Docker Desktop

REQ-UX-001, REQ-API-001. Code: `cmd/hyperbutler/dockercli_windows.go`,
`internal/engine/dockerctx`.

**Which docker CLI.** The release zip carries `clients\docker.exe` and
`clients\cli-plugins\docker-compose.exe`, built by `dist/package.ps1` from the docker/cli
and docker/compose source modules pinned in `dist/clients.lock`. They sit in a subfolder
so that the folder on PATH (the one with `hyperbutler.exe`) holds no `docker.exe`: the
README adds `clients\` to PATH only for someone without a docker CLI. hyperbutler never
edits PATH (system or user), never copies a CLI anywhere, and runs no docker CLI except
`docker --version` at `start`. `start` and `status` (`dockerCli`, `dockerCliBundled` in
`--json`) report the `docker.exe` a plain `docker` resolves to (the first on PATH, as
`exec.LookPath` finds it) and mark it `(bundled)` when it is the one in `clients\`.

**Old CLIs.** Docker Engine 29.9.2 refuses API versions below 1.40 unless configured
otherwise (`daemon/config/config.go` at tag `docker-v29.9.2`: `defaultMinAPIVersion =
"1.40"`; the running guest reports `MinAPIVersion 1.40`). API 1.40 is docker 19.03.
`start` reads the CLI's version from `docker --version` and warns below 19.03, naming the
bundled CLI and the `$env:Path` line for this shell. Newer CLIs negotiate down, so there
is no upper limit.

**Compose.** The docker CLI (v29, `cli-plugins/manager`) searches for plugins in
`config.json`'s `cliPluginsExtraDirs`, then `<config dir>\cli-plugins` (`%DOCKER_CONFIG%`
or `%USERPROFILE%\.docker`), then `%ProgramFiles%\Docker\cli-plugins`. The first match
wins, and there is no search next to `docker.exe`, so without help no CLI, bundled or
not, finds the bundled compose. With the context management on (`context.manage`, the
default), `start` lists `clients\cli-plugins` in `cliPluginsExtraDirs` while no other
`docker-compose.exe` is in that search, and takes it out again when one appears (Docker
Desktop installs into `%ProgramFiles%\Docker\cli-plugins`). An extra directory comes
first, so listing it next to another copy would shadow that copy; this rule never does.
Nothing is copied into `~/.docker/cli-plugins`. `hyperbutler delete` and `hyperbutler
uninstall` take the entry out.

**Docker Desktop at the same time.**

| | Docker Desktop | hyperbutler |
|---|---|---|
| Docker API pipe | `\\.\pipe\docker_engine` (context `default`), `\\.\pipe\dockerDesktopLinuxEngine` (context `desktop-linux`) | `\\.\pipe\docker_engine_hyperbutler` (setting `pipe`), context `hyperbutler`; `start` fails if the pipe is taken |
| docker contexts | `desktop-linux` and others | only its own `hyperbutler`, marked `HyperbutlerManaged`; a foreign context of that name is left alone |
| `currentContext` in `config.json` | set by Desktop | `start` selects `hyperbutler` and prints the context it replaced; every other key is kept |
| compose plugin | `%ProgramFiles%\Docker\cli-plugins` | used only when there is no other (above) |

**Not covered** (no Docker Desktop on the test machine; from reading the code):

- `hyperbutler stop` leaves `hyperbutler` selected, and `delete` resets the selection to
  docker's `default`. `start` records the context it replaced, and `hyperbutler uninstall`
  selects that one again if it still exists (`desktop-linux`, say). Between a `stop` and
  an uninstall, a Desktop user switches back with `docker context use desktop-linux`.
- Whether Docker Desktop re-selects `desktop-linux` when it starts, or rewrites
  `cliPluginsExtraDirs`, was not checked.
- `DOCKER_HOST` or `DOCKER_CONTEXT` in the environment overrides the selected context;
  `start` and `status` print a note when they are set.
- Another engine (REQ-UX-018): `start` looks for `\\.\pipe\dockerDesktopLinuxEngine` and
  `\\.\pipe\docker_engine` by listing the pipe file system (`FindFirstFile`, which opens
  no pipe and uses no instance of it), names the engine, and says which one a plain
  `docker` now reaches and how to switch. `status` names the selected context when it is
  not `hyperbutler` ("context: hyperbutler (not selected; docker uses
  "desktop-linux")", `dockerContextCurrent` in `--json`). Neither changes the selection:
  only `start` selects, as before. Tested with a pipe of its own, not with Docker Desktop.
- Credential helpers (REQ-UX-019): `start` and `doctor` warn when `config.json`'s
  `credsStore` or a `credHelpers` entry names a `docker-credential-<name>` that is not on
  PATH (Docker Desktop's `desktop` after it is uninstalled), with the line to change;
  hyperbutler never edits those keys.
- Tools that ignore docker contexts, the `docker_engine` opt-in, and moving images and
  volumes from Docker Desktop: [tools.md](tools.md).
- Both engines share the host's ports. A `docker run -p` on hyperbutler for a port Docker
  Desktop already publishes fails with "Ports are not available", as on Desktop
  ([Published ports](#published-ports)). The reverse (hyperbutler first) depends on
  Desktop, which binds `0.0.0.0:P` and may succeed next to hyperbutler's loopback
  listeners; run the two on different host ports. See
  [usage-scenarios.md, S3.4](research/usage-scenarios.md#s34-both-publish-the-same-port).

**Windows on ARM64.** The release is x64 only (`hyperbutler-<tag>-windows-amd64.zip`).
The x64 `hyperbutler.exe` and docker CLI would run under emulation on ARM64, but the guest
image is x86_64 and cannot boot in an ARM64 Hyper-V VM, so ARM64 is not supported yet.
Docker publishes no Windows arm64 static CLI; an arm64 release would build `docker.exe`
from the same pinned source with `GOARCH=arm64` (compose also publishes a
`windows-aarch64` build).

## Client tools

REQ-API-002, REQ-API-003. Repeat with [`bench/compat-tools`](../bench/compat-tools/README.md).
Only what ran is here. Each tool ran its own basic sample against an isolated engine on
`\\.\pipe\hb_compat`, on 2026-11-07: Windows 11 Home 25H2, build 26200.9711; the engine
built from commit 877731b; the guest image of v0.1.1; dockerd 29.9.2 (API 1.56); the docker
CLI 29.9.2 of the v0.1.1 zip; Go 1.27.0, Python 3.13.15, Node 26.13.0. Docker Desktop is
not on this machine, so nothing here compares with it by running it.

| Tool | Version | Result | What it needed |
|---|---|---|---|
| Docker SDK for Go (ping, pull, run, logs, remove) | `github.com/moby/moby/client` v0.6.2 | pass | `DOCKER_HOST` |
| Docker SDK for Python (the same) | `docker` 7.2.0 | pass | `DOCKER_HOST`, or a selected docker context |
| Testcontainers for Go (nginx, wait strategy, mapped port, Ryuk) | `testcontainers-go` v0.45.0 | pass, but **Ryuk fails by default** | `DOCKER_HOST` and `TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE=/var/run/docker.sock`, or `TESTCONTAINERS_RYUK_DISABLED=true` |
| Testcontainers for Python (the same) | `testcontainers` 4.15.0 | pass, Ryuk included | `DOCKER_HOST`, or a selected docker context |
| Testcontainers for Node (the same) | `testcontainers` 12.2.1 | pass, Ryuk included, but **not with the usual `DOCKER_HOST`** | `DOCKER_HOST=npipe:////.\pipe\<name>` with that backslash |
| Dev Containers CLI, `up` and `exec` of an image, a Windows folder as the workspace | `@devcontainers/cli` 0.90.0 | pass | `DOCKER_HOST` or a selected docker context; `--docker-path` |
| Dev Containers CLI, a Dockerfile with a feature (`ghcr.io/devcontainers/features/node:1`) | the same | pass | the same; see the legacy builder below |
| Testcontainers for Java | | not run: no `java` on this machine | |
| Testcontainers for .NET | | not run: runtimes 8.0.16 and 10.0.10, no SDK | |
| JetBrains IDEs | | not automated (a GUI) | a Docker connection, [tools.md](tools.md#jetbrains-ides-intellij-goland-pycharm-rider-) |
| VS Code Docker, Container Tools and Dev Containers extensions | | not automated (a GUI); VS Code 1.143.1 is here, the extensions did not run | [tools.md](tools.md#vs-code) |

In the Dev Containers samples, a file that `postCreateCommand` wrote into the workspace
reached the Windows folder, and a file from Windows was read through `devcontainer exec`.
Ryuk "included" means that a container the sample left running was gone, with Ryuk,
10 to 11 s after the sample's process ended. A Python program that exits cleanly stops
Ryuk first (the library's own `atexit` hook), so its container stays; the sample ends with
`os._exit` to see Ryuk at work.

**Testcontainers for Go: the Ryuk mount.** With any pipe, by default:

```
run nginx: generic container: create container: reaper: new reaper: run container: container start: Error response from daemon: error while creating mount source path '/pipe/hb_compat': mkdir /pipe: read-only file system: could not start container
```

The library bind-mounts the path of `DOCKER_HOST` into Ryuk, here `//./pipe/hb_compat`,
which dockerd in the VM takes for a Linux path. It has an exception only for an engine
whose `OperatingSystem` is `Docker Desktop` (`extractDockerSocketFromClient`,
`internal/core/docker_host.go`); hyperbutler says `hyperbutler guest (prototype)`. The
same error comes on `\\.\pipe\docker_engine` (`/pipe/docker_engine`).
`TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE=/var/run/docker.sock` names the socket in the VM,
and Ryuk then works.

**Testcontainers for Node: the pipe's name.** With `npipe:////./pipe/hb_compat`, as an
environment variable or in `~/.testcontainers.properties`:

```
Container runtime strategy "ConfigurationStrategy" does not work: "Error: connect UNKNOWN //pipe/hb_compat"
Error: Could not find a working container runtime strategy
```

The library reads the value with `new URL()`, which removes the `.` from the path, so no
named pipe can be given in that spelling. It is not about this engine (from the code, not
run on another). `npipe:////.\pipe\hb_compat` passes through the parser and Windows reads
it as the same pipe. Other tools do not take that spelling, so set it for the Node tests
only. In `~/.testcontainers.properties` the backslash is doubled:
`docker.host=npipe:////.\\pipe\\hb_compat`. With nothing set the library tries
`\\.\pipe\docker_engine`, and works there.

**The legacy builder.** The docker CLI in the zip has no buildx plugin, so for a feature
the Dev Containers CLI runs a plain `docker build`, and it works with a warning: `DEPRECATED:
The legacy builder is deprecated and will be removed in a future release.`

**Without `DOCKER_HOST`.** Whether a setting finds the engine (Ryuk off, so that only this
is tested). `no` means the tool went to `\\.\pipe\docker_engine` and failed there, as it
would reach Docker Desktop if that were running.

| Tool | A selected docker context | `docker.host` in `~/.testcontainers.properties` |
|---|---|---|
| Docker SDK for Go, `client.FromEnv` | no | |
| Docker SDK for Python, `docker.from_env()` | **yes** | |
| Testcontainers for Go | no | yes |
| Testcontainers for Python | yes | no |
| Testcontainers for Node | no | only with the backslash spelling above |
| Dev Containers CLI | yes | |

**The opt-in**, `hyperbutler config set pipe \\.\pipe\docker_engine` and a new start, with
nothing set in the tool: the docker CLI without a context, both SDKs and Testcontainers for
Python and Node all reached the engine, the last two with Ryuk on; Testcontainers for Go
reached it with `TESTCONTAINERS_RYUK_DISABLED=true`, and with Ryuk on failed as above. With no Docker Desktop
installed and the pipe free, `config set` gave no warning, as it should. The run used
`stop` and `start`, not `restart`, to stay on its own guest image and backend.

## Bind sources hyperbutler does not share

REQ-FS-006, [ADR 0007](adr/0007-bind-mounts.md). Code: `internal/share/bind.go`,
`internal/share/netdrive_windows.go`.

Bind mounts share folders on local drives (`C:\src`, also as `\\?\C:\src`). The API proxy
refuses the create of any other Windows source, for `-v` and `--mount type=bind` alike,
before dockerd sees it, so the container is not created:

| Source | Docker Desktop | hyperbutler |
|---|---|---|
| UNC, `\\server\share\x`, `\\?\UNC\server\share\x` | not supported (*well-known*) | `Error response from daemon: hyperbutler: bind source \\server\share\x is a network path; hyperbutler shares folders on local drives only. Copy the files to a folder on a local drive (such as C:\src) and bind that, or use a named volume.` |
| `\\wsl$\Distro\...`, `\\wsl.localhost\Distro\...` | works (the files are in WSL) | refused: `... is inside a WSL distribution ...`, with the advice to copy the files or use the engine in WSL for them |
| a device path, `\\.\...`, `\\?\Volume{...}\...` | not checked | refused: `... is a device path ...; use a path with a drive letter` |
| a folder on a mapped network drive (`Z:\x`, `GetDriveType` = remote in the engine's logon session) | not checked | refused: `... is on Z:, a network drive ...` |

Before 0.2 such a source went to dockerd unchanged, which refused it as an invalid local
volume name. Git Bash's `//var/run/docker.sock` (a guest path written with two slashes
so MSYS leaves it alone) and named volumes are not affected.

## Corporate CA and proxy

REQ-UX-009, [settings.md](research/settings.md#corporate-ca-and-proxy-req-ux-009).

| | Docker Desktop | hyperbutler |
|---|---|---|
| dockerd trusts the Windows store's CAs | yes | yes, trusted roots only (`ca.import`, default on); intermediates are not anchors |
| An extra PEM file of CAs | no | `ca.extraFile` |
| Containers and `docker build` trust them | no: add them to the image | no: the same |
| Proxy for pulls | the "Containers proxy" setting | `proxy.http`, `proxy.https`, `proxy.noProxy` (dockerd's environment) |
| `proxies` in daemon.json | allowed (it overrode the app's setting in 4.35) | refused: use `config set proxy.*` |
| The Windows system proxy, PAC | yes | not yet (`proxy.mode`, 0.2) |
| SOCKS5 | Business plan | yes (`socks5://`) |
| NTLM, Kerberos | Business plan | through a local relay (px, cntlm) on `127.0.0.1`, reached via the host gateway |
| Proxy passwords shown | redacted | redacted (`--show-secrets` shows them) |
