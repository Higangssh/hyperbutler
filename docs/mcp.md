# hyperbutler mcp

`hyperbutler mcp` serves the [Model Context Protocol](https://modelcontextprotocol.io) on
stdin and stdout, so an AI client (an MCP client such as your editor's AI assistant) can
read the engine's state and, if you allow it, start and stop things (REQ-UX-012,
[ADR 0008](adr/0008-settings-gui-mcp.md) "MCP shape").

- The client starts `hyperbutler.exe mcp` and ends it by closing its stdin. Nothing stays
  resident: the server exits within a few seconds of its client, even in the middle of a
  tool call.
- It talks only to the engine's Docker API pipe (`\\.\pipe\docker_engine_hyperbutler`, or
  the one `config set pipe` chose) and the installation's files. It opens no network port
  and makes no request of its own: the `status` tool does not make the daily update check
  that `hyperbutler status` makes (REQ-UX-022), and neither does `diagnose`. `pull` asks
  dockerd to pull from Docker Hub (or a registry you allow), as `docker pull` does.
- It is read-only unless you pass `--allow-write`. Deleting needs `--allow-delete` as
  well. A tool that is not allowed is not listed and cannot be called.
- With `--allow-write`, two more flags widen what the write tools may do:
  `--allow-persistent` lets `run` use the restart policies `always` and `unless-stopped`,
  and `--allow-registry=HOST[,HOST]` lets `pull` use those registries besides Docker Hub.
  Both need `--allow-write`.
- Everything it returns goes through the same redaction as `hyperbutler diagnose`
  (`internal/redact`) and is bounded in size.

It is built on the official Go SDK, `github.com/modelcontextprotocol/go-sdk` v1.8.0
(Apache-2.0, with MIT for older contributions), and speaks MCP 2026-07-28; older protocol
versions a client asks for (back to 2024-11-05) are negotiated by the SDK.

## Client configuration

Most clients read a JSON file with an `mcpServers` object. Add:

```json
{
  "mcpServers": {
    "hyperbutler": {
      "command": "hyperbutler.exe",
      "args": ["mcp"]
    }
  }
}
```

`hyperbutler.exe` must be on the PATH the client sees; otherwise give the full path, such
as `"command": "C:\\Tools\\hyperbutler\\hyperbutler.exe"` (JSON needs the backslashes
doubled). Some clients call the object `servers` and want `"type": "stdio"` in the entry;
the command and arguments are the same.

To let the client change state, add the flags:

```json
"args": ["mcp", "--allow-write"]
"args": ["mcp", "--allow-write", "--allow-delete"]
"args": ["mcp", "--allow-write", "--allow-registry=ghcr.io", "--allow-persistent"]
```

A second installation (`HYPERBUTLER_HOME`) is chosen the same way as for the CLI:

```json
"env": { "HYPERBUTLER_HOME": "D:\\hyperbutler-test" }
```

Clients usually ask before each tool call; keep that on for the write and delete tools.

To see what a client would see, pipe requests in by hand (PowerShell):

```powershell
@(
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"me","version":"1"}}}'
  '{"jsonrpc":"2.0","method":"notifications/initialized"}'
  '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"containers","arguments":{}}}'
) | hyperbutler mcp
```

Answers that are ready within about 2 seconds of the end of the input are written; slower
calls are canceled when the input ends, as when a client goes away.

## Tools

| Tool | Needs | Returns |
|---|---|---|
| `status` | | the `hyperbutler status --json` document (schema 1), with its free-text fields moved into `untrustedFields` (see below). It makes no update check: `update` is the last check's record. Works with the engine stopped |
| `containers` | | running and stopped containers: name, short id, image, state, ports, restart policy, whether it comes back with the engine, Compose project and service. `running: true` lists only running ones. The restart policy needs one inspect per container: the first 50 get one (4 at a time, within 10 s), the others say `unknown` with `truncated: true` and a `note` |
| `logs` | | the last `tail` lines (default 100, at most 1000) of a container's stdout and stderr, in `untrustedText` |
| `images` | | tags, short id, size, creation time, containers using it |
| `volumes` | | name, driver, creation time |
| `disk_usage` | | `docker system df` (images, containers, volumes, build cache: count, active, size, reclaimable), the data disk on the host and the host volume's free space, and the guest file system's free space from the guest's last report, if any |
| `memory` | | the VM's configured memory, the guest's last `/proc/meminfo` report (from the engine log, once a minute), dockerd's total, and the use of up to 20 running containers (as `docker stats`) |
| `settings` | | `hyperbutler config list --json`. Proxy passwords are always redacted; there is no way to show them over MCP |
| `diagnose` | | writes the `hyperbutler diagnose` zip to `%TEMP%` and returns its path and file list, not the contents. It takes no path |
| `engine_logs` | | the last lines of the engine log (`source: engine`) or the guest console (`source: console`), in `untrustedText`. Works with the engine stopped |
| `start`, `stop` | `--allow-write` | `hyperbutler start` (with the guest image the engine last ran) and `hyperbutler stop`, with their output in `untrustedText` (it can quote dockerd and the guest). `stop` lists the containers that stop and which come back |
| `container_start`, `container_stop`, `container_restart` | `--allow-write` | the container's state afterwards and whether anything changed. `timeoutSeconds` 0 to 300 (default 10) |
| `run` | `--allow-write` | creates and starts a detached container from an image that is already there: `image`, `name`, `command`, `env`, `ports` (`HOST:CONTAINER[/udp]`), `restart`, `memoryMiB`, `cpus`. A TCP port is reachable from Windows on 127.0.0.1 and [::1] only: the engine mirrors it there and on no other host address. There are no mounts, volumes, privileges, capabilities or host networking, and the container's memory, CPUs and processes are capped (see "What run creates"). The container is labelled `dev.hyperbutler.mcp=run`; dockerd's warnings are in `untrustedText` |
| `pull` | `--allow-write` | pulls a Docker Hub image (or one from a registry `--allow-registry` lists) without credentials; a reference without a tag pulls `:latest`. At most 2 at once. Returns the last progress messages in `untrustedText` (see "Which registries pull reaches") |
| `config_set` | `--allow-write` | `hyperbutler config set KEY VALUE` with the CLI's validation and its message about what restart is needed. It never restarts anything, and only `memory`, `cpus`, `disk.size`, `engine.memoryLimit` and `context.manage` can be set (see below) |
| `container_rm`, `image_rm`, `volume_rm` | `--allow-delete` | `docker rm` (`force` for a running one), `docker rmi` (`force`), `docker volume rm` |
| `prune` | `--allow-delete` | one `kind`: `containers` (stopped), `images` (dangling), `volumes` (unused anonymous), `networks` or `build-cache`; how many were removed and the space reclaimed |

With the engine stopped, the tools that need it fail with "the hyperbutler engine is not
running" and say how to start it: the `start` tool when the server has `--allow-write`,
otherwise `hyperbutler start` or a restart of the server with `--allow-write`. While the
free-space guard has the VM paused, they say that instead of waiting for the VM.

## What it returns

Every result is one JSON object, both as text content and as structured content.

- **Redaction.** The object goes through `redact.JSON` (secret keys, URL credentials,
  bearer tokens, JWTs, known token prefixes, `password=...` and the rest), and every error
  message through `redact.String`, as in `diagnose`.
- **Untrusted text.** Free text that containers, dockerd, the guest, a registry or Windows
  wrote is only ever in a field named `untrustedText`, next to an `untrustedNote` that
  says it is data. That covers container logs, the engine log and the guest console;
  `start` and `stop` output; `run`'s warnings from dockerd; `pull`'s progress messages;
  and in `status`, the fields `state.error`, `state.daemonConfig.error`,
  `state.daemonConfig.env.error`, `state.hostGateway.error` and every
  `state.unpublishedPorts[].reason`, which are removed from their place and listed in
  `untrustedFields` as `{"field": "state.error", "untrustedText": "..."}`. An error result
  whose cause is such text (dockerd's message for an API error, a registry's pull
  failure, a failed Docker API connection, the error of a CLI command) is a JSON object:
  `error` is the server's own message, and the other side's text is in `untrustedText`.
  The guest-reported numbers `disk_usage` and `memory` read from the engine log are
  returned only when they are numbers, sizes and times; anything else is left out. The
  tool descriptions and the server's instructions say the same. The server never acts
  on what that text says: no tool reads another tool's output, and nothing in a result
  starts an action.
- **Clean text.** Control characters (escape sequences included), C1 controls and the
  Unicode bidirectional overrides become U+FFFD; invalid UTF-8 too.
- **Bounds.** When anything is cut, the result says `truncated: true`.

| What | Bound |
|---|---|
| list entries (containers, images, volumes, removed images) | 200 |
| log lines | `tail`, at most 1000 |
| log text after redaction | 64 KiB (the newest lines) |
| raw log read from the API | 4 MiB |
| one name, status or other short string | 512 bytes |
| image tags per image | 16 |
| containers asked for memory use | 20 |
| containers inspected for their restart policy | 50, 4 at a time, within 10 s |
| `start`, `stop`, `config_set` output and error messages | 16 KiB |
| untrusted text in an error or a `status` field | 4 KiB |
| pulls at once | 2 |
| one result | 256 KiB; a larger one becomes an error that says so |
| one request from the client | 1 MiB per line |
| Docker API answer decoded whole | 8 MiB |

Read tools time out after 20 s; container actions, `run`, `config_set`, `diagnose` and the
single deletes after 2 minutes; `start` and `stop` after 4; `pull` after 10 (enough for a
large image on a slow line; it also bounds how long a pull holds one of the 2 slots);
`prune` after 15.

## Which settings config_set may change

`config_set` takes a key only if it is on an allowlist (`internal/mcpserver/configpolicy.go`).
A key added to `internal/config` later is refused until someone reviews it: a test fails
until every key is classified. A key is allowed when its value is bounded by its
validator, setting it back undoes the change, and it neither widens what containers or
the network can reach nor changes what is trusted or what runs at sign-in.

| Key | MCP | Why |
|---|---|---|
| `memory` | allowed | bounded by the validator (the VM's memory range); applies at the next VM start |
| `cpus` | allowed | at most the host's logical CPUs; applies at the next VM start |
| `disk.size` | allowed | bounded, sparse, and only before the data disk exists; fixed afterwards |
| `engine.memoryLimit` | allowed | the engine process's Go soft memory limit; applies at the next VM start |
| `context.manage` | allowed | whether `start` creates and selects the `hyperbutler` docker context; changes nothing running |
| `proxy.http`, `proxy.https` | refused | can hold a password, which would pass through the AI client's context and logs; a proxy chosen by text the model read (a prompt injected into a log) could see every pull |
| `proxy.noProxy` | refused | decides which hosts dockerd reaches around the proxy: an egress policy, the user's choice |
| `ca.extraFile` | refused | names a file on this machine and adds a root CA dockerd trusts, which would let an injection intercept TLS |
| `ca.import` | refused | decides whether dockerd trusts the CAs of the Windows root stores: a TLS trust decision |
| `data.dir` | refused | makes hyperbutler create a directory and a data disk where the client says |
| `pipe` | refused | chooses a name in the pipe namespace every program shares; it could take Docker Desktop's `docker_engine` and receive its clients' requests |
| `hostGateway.enabled`, `hostGateway.ports` | refused | open services on this machine's 127.0.0.1 to containers, and apply at once (the engine reloads them live) |
| `autostart` | refused | creates (or removes) a Task Scheduler task that runs at every sign-in |
| `updateCheck` | refused | not dangerous, but whether hyperbutler makes its one network request (REQ-UX-005) is the user's network-policy choice, in either direction: a client must neither turn it on behind the user's back nor turn off the notices the user chose |
| `network.frameCheck` | refused | turning it off trades data integrity for a few percent of throughput, a choice that is the user's |

An allowed key goes through the CLI's own `config set` code, with `--` before the key and
value, so a value such as `--apply` is a value and is refused by the key's validation,
never taken as a flag.

## Which registries pull reaches

dockerd connects to whatever host a reference names, and its error says whether
something answered there. A `pull` of `HOST:PORT/x` was therefore a port scan of the
guest network, of this machine (`host.docker.internal`) or of the LAN, with the answer in
the error. Now:

- A reference without a registry host (`busybox`, `owner/name:tag`) is a Docker Hub image;
  dockerd pulls it from Docker Hub, or from the registry mirrors in `daemon.json`. So is
  one that names `docker.io`, `index.docker.io` or `registry-1.docker.io`. These are
  allowed.
- A reference with another host (the part before the first `/` holds a `.` or `:`, is
  `localhost`, or has an upper-case letter, as in Docker's own parser) is refused unless
  `--allow-registry` lists that host, with the same port. Entries are lower-case host
  names with an optional port.
- An IP address, a number (which some resolvers take for an address, such as
  `2130706433` or `0x7f000001`), `localhost` and `*.localhost`, and `host.docker.internal`,
  `gateway.docker.internal` or any other `*.docker.internal` name are always refused, in
  a reference and in `--allow-registry`.
- A refused pull never reaches dockerd. At most 2 pulls run at once; a third is refused
  until one ends.

## What run creates

`run` creates a container with:

- Ports bound to every guest address (`HostIp` empty, as `docker run -p 8080:80`). The
  engine mirrors such a TCP port on Windows' 127.0.0.1 and [::1] and on no other host
  address (`internal/engine/publish.go`), so it is reachable from this machine only. A
  binding to 127.0.0.1 would be the guest's own loopback, which the engine cannot reach
  and does not mirror (until review item H1 it was, and such ports were unreachable). UDP
  ports are published in the guest only; the engine mirrors TCP.
- A memory limit: 1 GiB, or half the VM's memory (dockerd's `MemTotal`) when that is less.
  `memoryMiB` can choose 6 MiB up to half the VM's memory, never more. No swap on top.
- `NanoCpus` of the VM's CPU count, or the `cpus` argument (0.01 up to the VM's CPUs).
- `PidsLimit` 512, so a fork bomb stays in its container.
- Restart policy `no` (default) or `on-failure` with at most 5 retries. `always` and
  `unless-stopped` bring the container back at every engine start, so they need the
  server's `--allow-persistent`.
- The label `dev.hyperbutler.mcp=run`, and no mounts, volumes, privileges, added
  capabilities or host networking.

## When the client goes away during start

`start` runs `hyperbutler start`, which spawns the engine as a detached process and waits
for its Docker API. If the client closes stdin in the middle, the server returns within
about 4 s and the process exits; the wait ends with it, but the engine it spawned is
detached and keeps going, as after a start from a terminal that was closed: it comes up
(the next `status` says running) or fails and records why in `state.json`. What does not
happen is the CLI's own step on a start that times out, stopping the engine it waited
for; `stop` does that. The ops lock `start` held is a file lock and is released when the
process exits. A `start` that had not spawned the engine yet spawns nothing.

## VM check (2026-11-01)

Windows 11 Home 26200, a development build in `%TEMP%\hb-dev\mcp\bin`, a test home with
its own pipe (`config set pipe \\.\pipe\hb-dev-mcp`), `context.manage false`, a test
`DOCKER_CONFIG`, 2 GiB guest, and a busybox container with a published port that printed a
password, a GitHub-style token and an injected instruction. A small Go client using the
SDK's command transport:

- `initialize`: protocol 2026-07-28 in 28 ms; `tools/list`: the 10 read tools, 18 with
  `--allow-write`.
- `status`: the `status --json` document (running, the test pipe, the VM id).
- `containers`: `mcpbox`, `127.0.0.1:18080->80/tcp`, `unless-stopped`, `comesBack: true`.
- `logs`: 3 lines, `db password=[REDACTED] token=[REDACTED]`, the injected line as data in
  `untrustedText`.
- `memory`: guest 1975 MiB total, 80 MiB used (guest-reported), the container 1.8 MiB.
  `disk_usage`: one image of 6.8 MB, the 27 MB data disk.
- `container_stop` on the read-only server: "unknown tool". With `--allow-write`:
  `changed: true`, `exited`, exit code 137, in 2.2 s; `containers` then showed it exited.
- `stop` (1.4 s) and `start` (1.2 s) as tools; `containers` between them said the engine
  is not running and to call the start tool. `config_set proxy.http` was refused;
  `config_set cpus 2` wrote the file and said a VM restart is needed. `diagnose` wrote the
  zip to `%TEMP%` and listed its 12 files.
- After the client closed stdin the server exited in 4 to 309 ms with status 0. The same
  requests piped from PowerShell 5.1 (which adds a byte order mark) were answered.

## VM check of the second review (2026-11-01)

The same machine, a development build of 66232ce in `%TEMP%\hb-dev\mcpfix\bin`, a test
home with its own pipe (`config set pipe \\.\pipe\hb-dev-mcpfix`), `context.manage false`,
`--no-context`, a test `DOCKER_CONFIG` and a 2 GiB guest. A small Go client spoke
JSON-RPC to `hyperbutler mcp --allow-write` on its stdin and stdout:

- `pull nginx:alpine`: done in 4.2 s, the progress in `untrustedText`.
- `run nginx:alpine` with `ports ["18080:80"]`: the create request had
  `{"80/tcp":[{"HostIp":"","HostPort":"18080"}]}`, a memory limit of 987 MiB (half of
  dockerd's `MemTotal`, which is below 1 GiB on a 2 GiB guest), `NanoCpus` 8e9 (the VM's
  8 CPUs), `PidsLimit` 512, restart `no` and the label. dockerd's warning (no swap limit
  in the guest kernel) came back in `untrustedText`. **From Windows, `curl
  http://127.0.0.1:18080` returned the nginx welcome page, and `http://[::1]:18080`
  answered 200**; the only listeners on port 18080 were the engine's, on 127.0.0.1 and
  ::1. This is the H1 proof: with the old `HostIp: 127.0.0.1` the port was not mirrored.
- `run` with `restart: always`: refused, naming `--allow-persistent`.
- `pull 192.168.0.4:5000/lanbench:1`: refused ("is an IP address") without reaching
  dockerd; `host.docker.internal:5000/x` and `ghcr.io/o/n` were refused too.
- `config_set` refused `hostGateway.enabled`, `hostGateway.ports`, `pipe` (with
  `\\.\pipe\docker_engine`), `ca.import`, `proxy.noProxy`, `autostart`, `updateCheck`,
  `proxy.http`, `ca.extraFile` and `data.dir`; `cpus 2` was set.
- `status` listed `state.daemonConfig.env.error` (the test guest image takes no proxy
  settings) under `untrustedFields`, not in its place.
- Afterwards the container was removed, the engine stopped, and the test directories
  deleted.

## Adversarial review (2026-11-01)

| # | Attack or failure | Finding | Fix or status |
|---|---|---|---|
| 1 | Prompt injection through container logs and the engine log | Log text reaches the client verbatim, so an instruction in it reaches the model | Fixed by design: log text is only in `untrustedText` with `untrustedNote`; the tool descriptions and server instructions say it is data; no tool reads another's output or acts on a result. Escape sequences and bidi overrides are replaced. Tested with an injected line (unit test and VM) |
| 2 | Injection outside `untrustedText` | Compose labels are free text anyone can set with `docker run --label`, and were returned as `composeProject` | Fixed: only a label that looks like a Compose name is returned; others are left out (test). Names and image references are restricted by dockerd; statuses come from dockerd. Corrected by the second review (M1): this was partial. `status` passed `state.error`, the daemon.json and environment errors and the unpublished ports' reasons as they were, `run` returned dockerd's warnings unlabelled, and Docker API errors and pull failures carried dockerd's or the registry's text in the plain error message. All of these are now in `untrustedText` |
| 3 | Injection steering the model to a write tool | A model can be talked into calling `container_rm` | Mitigated: write and delete tools exist only with the flags, and are annotated destructive so clients ask; the server's instructions say to confirm with the user. The model's choices are the client's to gate |
| 4 | Path traversal in a container or volume name (`../images/json`, `..`, `a/b`, `%2e%2e`) | dockerd's router cleans paths, so `..` in a name could reach another endpoint | Fixed: names must match `[a-zA-Z0-9][a-zA-Z0-9_.-]*` (one plain path segment), checked before any request; tested with 12 bad names on 4 tools, with no request reaching the fake engine |
| 5 | Path traversal in an image reference (`a/../b`, `a//b`, `x?force=1`) | Same, for `/images/{name}` | Fixed: references are limited to `[a-zA-Z0-9._/:@-]`, with no empty, `.` or `..` segment (test) |
| 6 | Path arguments elsewhere | `diagnose --out`, `--image-dir`, `ca.extraFile`, `data.dir`, bind mounts would let a client name host paths | Fixed: `diagnose` takes no path (it writes to `%TEMP%`), `start` takes no image directory (it uses the last engine's, as `restart`), `config_set` refuses path keys (now through its allowlist, H2), `run` has no mounts. `engine_logs` takes `source` from a fixed set |
| 7 | Unbounded output: many containers, huge log lines, a long pull | | Fixed: the bounds above, `truncated: true`, and a final 256 KiB check per result (tests with 250 containers, a 900 KB log, an over-size result). Pull progress is streamed and only the last 10 messages kept, with 1 MiB per line. Partial, corrected by L5: `containers` made one inspect per listed container (up to 200), which could run past the 20 s limit; it now makes at most 50 |
| 8 | Unbounded input | A client could send one huge line | Fixed: 1 MiB per request line; the session ends on a longer one (test) |
| 9 | A write tool reachable without its flag | | Not reachable: the tools are not registered, so `tools/call` fails with "unknown tool" and nothing reaches the host or engine (test over all 12 write and delete tools, and on the VM). `--allow-delete`, `--allow-persistent` and `--allow-registry` without `--allow-write` are refused |
| 10 | A read tool with a side effect | `diagnose` writes a zip | Accepted: ADR 0008 lists diagnose as read-only; it writes one new file to `%TEMP%` and is not annotated read-only |
| 11 | Secrets in results: proxy passwords in settings, credentials in state.json errors, tokens in logs | | Fixed: every result through `redact.JSON`, every error through `redact.String`; `settings` never shows secrets (no option); tests plant a proxy URL, a bearer token, a password and a GitHub token |
| 12 | Secrets in arguments | `config_set proxy.http` would carry a password through the model | Fixed: secret and path keys are refused; a refused key never reaches settings.json (test). Partial, corrected by H2/H3/M4: other keys that widen reach or trust (`hostGateway.*`, `pipe`, `ca.import`, `proxy.noProxy`, `autostart`) were settable; `config_set` now takes only the keys on its allowlist |
| 13 | Over-redaction | The refusal's advice "credentials: run ..." was itself redacted (the `key: value` rule), seen on the VM | Fixed: the message no longer puts a colon after the word (test) |
| 14 | A flag smuggled in a value (`config_set cpus --apply`) | `config set` parses flags among its arguments | Fixed: the host passes `--` first; the value is validated as a value (test) |
| 15 | Hang when the client closes stdin | The SDK waits for running tool calls before its session ends, and the CLI's `start` ignores cancellation, so a closed client could leave the process up to 4 minutes | Fixed: the server watches stdin itself; when it ends, answers get 2 s to be written, calls are canceled, and it returns 2 s later at most (test with a call that ignores its context; VM: exit in 4 to 309 ms) |
| 16 | Answers lost when input ends | The SDK drops unwritten answers when its input ends, so piped requests got none | Fixed: the end of stdin is held back until the server has written nothing for 300 ms (at most 2 s) |
| 17 | Protocol corruption by stray output on stdout | A command writing to `os.Stdout` would break the JSON-RPC stream | Fixed: every CLI command run for a tool writes to a buffer, and `os.Stdout` points to stderr while the server runs. The engine `start` spawns writes to engine.log and does not inherit the pipes |
| 18 | A byte order mark before the first request | Windows PowerShell 5.1 adds one; the SDK refused the line and the session ended | Fixed: a leading UTF-8 BOM is dropped (VM) |
| 19 | Network use (REQ-UX-005) | The SDK links HTTP transports and an OAuth client | Reviewed: only the stdio transport is constructed; no SDK package dials at init. `internal/notelemetry` lists the Docker API client (a pipe) and the new modules. Wrong, corrected by the second review: the `status` tool ran the CLI's `status`, which makes the daily update check (REQ-UX-022), and `diagnose` did as well. Neither does now (test) |
| 20 | `MCPGODEBUG` | The SDK panics at init if this variable is malformed (no `=`), which stops every hyperbutler command, not only `mcp` | Fixed in 71d7d20 (M5): `internal/mcpenv` removes a malformed value before the SDK's init reads it, and `mcp` says so on stderr. Not reported upstream yet. A subprocess test guards it on every platform (L1) |

## Second adversarial review (2026-11-01)

| ID | Attack or failure | Finding | Fix |
|---|---|---|---|
| H1 | `run`'s published ports unreachable | `run` bound ports to `HostIp: 127.0.0.1`, the guest's own loopback: the engine mirrors only bindings to every guest address or the guest's address, logged that the port is unreachable from the host, and the tool's promise "127.0.0.1 only" meant the wrong machine | Fixed: `HostIp` is empty, as `docker run -p`; the engine puts every mirrored port on Windows' 127.0.0.1 and [::1] only, which is what the docs now say. A unit test runs the bindings `run` makes through the engine's own rules (`engine.MirroredOnStart`, `engine.MirroredBinding`); VM: `curl` from Windows got the page |
| H2 | `config_set hostGateway.enabled` / `hostGateway.ports` | They open this machine's loopback services to containers and apply at once | Fixed: refused over MCP |
| H3 | `config_set pipe` | The pipe namespace is shared; a client could take Docker Desktop's `docker_engine` | Fixed: refused over MCP |
| M4 | `config_set ca.import`, `proxy.noProxy`, `autostart`, `updateCheck` | TLS trust, egress policy, a logon task, and the user's choice about hyperbutler's one network request | Fixed: refused. `config_set` is an allowlist (`memory`, `cpus`, `disk.size`, `engine.memoryLimit`, `context.manage`; see "Which settings config_set may change"); a test fails until every key of `internal/config` is classified |
| M1 | Untrusted text outside `untrustedText` | `status` (`state.error`, `daemonConfig.error`, `daemonConfig.env.error`, `unpublishedPorts[].reason`), the guest-reported values of `disk_usage` and `memory`, `run`'s warnings, Docker API error messages and pull failures | Fixed: see "What it returns". A fake engine and host put an injected instruction in each place; the test walks every result and error and finds it only under `untrustedText`. `state.hostGateway.error`, `start`/`stop` output and pull progress are labelled too |
| M2 | `pull` as a port scanner | Any `host[:port]/name` was pulled, and the error said whether something answered there | Fixed: Docker Hub only by default; `--allow-registry=HOST[,HOST]`; IP literals, numbers, `localhost` and `*.docker.internal` always refused; at most 2 pulls at once, 10 minutes each (tests; VM: `192.168.0.4:5000/lanbench:1` refused) |
| M3 | `run` without limits | No memory, CPU or process limit, and `always` / `unless-stopped` bring a container back at every engine start | Fixed: see "What run creates" (1 GiB or half the VM, up to half the VM; CPUs at most the VM's; 512 processes); `always` and `unless-stopped` need `--allow-persistent` (tests) |
| M5 | Stale review row | Item 20 said the `MCPGODEBUG` panic was open | Fixed: 71d7d20 fixed it; item 20 says so |
| L1 | The `MCPGODEBUG` guarantee rests on the module path | `internal/mcpenv` initializes first only because its import path sorts before the SDK's | Documented in `internal/mcpenv` and CONTRIBUTING.md; a copy of the subprocess test without a build tag in `internal/mcpserver` runs on Linux CI too |
| L3 | Goroutine leaks; a client gone during `start` | | `internal/mcpserver` has a leaktest `TestMain`. "When the client goes away during start" says what happens |
| L5 | `containers` was N+1 | One inspect per container, up to 200, in sequence | Fixed: at most 50 inspects, 4 at a time, none after 10 s; the rest say `unknown` with `truncated: true` and a note (tests) |
| New | `status` made a network request | The MCP `status` tool (and `diagnose`) ran the CLI's `status`, which makes the update check (REQ-UX-022) | Fixed: they run `status` without the check and show the last check's record; the CLI's `status` still checks (test with a fake release API: no request from `status`, `diagnose`, `disk_usage` or `memory`) |

## Open items

- `bench/mcp`, named in REQ-UX-012, does not exist yet: the VM checks were by hand.
- `pull` has no registry credentials. Private images need `docker login` and `docker pull`.
- `pull` cannot tell a Docker Hub image from one a registry mirror serves, so a mirror in
  `daemon.json` is trusted as the user configured it.
- No resources or prompts are served, only tools.
