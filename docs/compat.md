# Docker compatibility notes

Known differences from Docker Desktop and Docker Engine on Linux (REQ-API-001). Only the
networking, docker CLI and proxy notes exist so far.

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
  `start` prints a note when they are set.

**Windows on ARM64.** The release is x64 only (`hyperbutler-<tag>-windows-amd64.zip`).
The x64 `hyperbutler.exe` and docker CLI would run under emulation on ARM64, but the guest
image is x86_64 and cannot boot in an ARM64 Hyper-V VM, so ARM64 is not supported yet.
Docker publishes no Windows arm64 static CLI; an arm64 release would build `docker.exe`
from the same pinned source with `GOARCH=arm64` (compose also publishes a
`windows-aarch64` build).
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
