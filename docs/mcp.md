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
  and makes no request of its own. `pull` asks dockerd to pull from the image's registry,
  as `docker pull` does.
- It is read-only unless you pass `--allow-write`. Deleting needs `--allow-delete` as
  well. A tool that is not allowed is not listed and cannot be called.
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
| `status` | | the `hyperbutler status --json` document (schema 1). Works with the engine stopped |
| `containers` | | running and stopped containers: name, short id, image, state, ports, restart policy, whether it comes back with the engine, Compose project and service. `running: true` lists only running ones |
| `logs` | | the last `tail` lines (default 100, at most 1000) of a container's stdout and stderr, in `untrustedText` |
| `images` | | tags, short id, size, creation time, containers using it |
| `volumes` | | name, driver, creation time |
| `disk_usage` | | `docker system df` (images, containers, volumes, build cache: count, active, size, reclaimable), the data disk on the host and the host volume's free space, and the guest file system's free space from the guest's last report, if any |
| `memory` | | the VM's configured memory, the guest's last `/proc/meminfo` report (from the engine log, once a minute), dockerd's total, and the use of up to 20 running containers (as `docker stats`) |
| `settings` | | `hyperbutler config list --json`. Proxy passwords are always redacted; there is no way to show them over MCP |
| `diagnose` | | writes the `hyperbutler diagnose` zip to `%TEMP%` and returns its path and file list, not the contents. It takes no path |
| `engine_logs` | | the last lines of the engine log (`source: engine`) or the guest console (`source: console`), in `untrustedText`. Works with the engine stopped |
| `start`, `stop` | `--allow-write` | `hyperbutler start` (with the guest image the engine last ran) and `hyperbutler stop`, with their output. `stop` lists the containers that stop and which come back |
| `container_start`, `container_stop`, `container_restart` | `--allow-write` | the container's state afterwards and whether anything changed. `timeoutSeconds` 0 to 300 (default 10) |
| `run` | `--allow-write` | creates and starts a detached container from an image that is already there: `image`, `name`, `command`, `env`, `ports` (`HOST:CONTAINER[/udp]`), `restart`. Ports go to 127.0.0.1 only; there are no mounts, volumes, privileges, capabilities or host networking. The container is labelled `dev.hyperbutler.mcp=run` |
| `pull` | `--allow-write` | pulls an image without credentials (public images); a reference without a tag pulls `:latest`. Returns the last progress messages |
| `config_set` | `--allow-write` | `hyperbutler config set KEY VALUE` with the CLI's validation and its message about what restart is needed. It never restarts anything and refuses `proxy.http`, `proxy.https`, `ca.extraFile` and `data.dir` (see below) |
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
- **Untrusted text.** Text that containers, dockerd or the guest wrote (container logs, the
  engine log, the guest console) is in a field named `untrustedText`, next to an
  `untrustedNote` that says it is data. The tool descriptions and the server's
  instructions say the same. The server never acts on what that text says: no tool reads
  another tool's output, and nothing in a result starts an action.
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
| `start`, `stop`, `config_set` output and error messages | 16 KiB |
| one result | 256 KiB; a larger one becomes an error that says so |
| one request from the client | 1 MiB per line |
| Docker API answer decoded whole | 8 MiB |

Read tools time out after 20 s; container actions, `run`, `config_set`, `diagnose` and the
single deletes after 2 minutes; `start` and `stop` after 4; `pull` and `prune` after 15.

## Why config_set refuses some keys

`proxy.http` and `proxy.https` can hold a password: setting them over MCP would put the
password into the AI client's context and logs. A proxy chosen by text the model read
(a prompt injected into a log) could also see every pull. `ca.extraFile` adds a root CA
that dockerd trusts, which would let the same kind of injection intercept TLS, and
`data.dir` makes hyperbutler create a directory and a data disk wherever the client says.
These stay CLI-only. Every other key goes through the CLI's own `config set` code, with
`--` before the key and value, so a value such as `--apply` is a value and is refused by
the key's validation, never taken as a flag.

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

## Adversarial review (2026-11-01)

| # | Attack or failure | Finding | Fix or status |
|---|---|---|---|
| 1 | Prompt injection through container logs and the engine log | Log text reaches the client verbatim, so an instruction in it reaches the model | Fixed by design: log text is only in `untrustedText` with `untrustedNote`; the tool descriptions and server instructions say it is data; no tool reads another's output or acts on a result. Escape sequences and bidi overrides are replaced. Tested with an injected line (unit test and VM) |
| 2 | Injection outside `untrustedText` | Compose labels are free text anyone can set with `docker run --label`, and were returned as `composeProject` | Fixed: only a label that looks like a Compose name is returned; others are left out (test). Names and image references are restricted by dockerd; statuses come from dockerd. A registry's pull error is returned in the error, marked as untrusted text |
| 3 | Injection steering the model to a write tool | A model can be talked into calling `container_rm` | Mitigated: write and delete tools exist only with the flags, and are annotated destructive so clients ask; the server's instructions say to confirm with the user. The model's choices are the client's to gate |
| 4 | Path traversal in a container or volume name (`../images/json`, `..`, `a/b`, `%2e%2e`) | dockerd's router cleans paths, so `..` in a name could reach another endpoint | Fixed: names must match `[a-zA-Z0-9][a-zA-Z0-9_.-]*` (one plain path segment), checked before any request; tested with 12 bad names on 4 tools, with no request reaching the fake engine |
| 5 | Path traversal in an image reference (`a/../b`, `a//b`, `x?force=1`) | Same, for `/images/{name}` | Fixed: references are limited to `[a-zA-Z0-9._/:@-]`, with no empty, `.` or `..` segment (test) |
| 6 | Path arguments elsewhere | `diagnose --out`, `--image-dir`, `ca.extraFile`, `data.dir`, bind mounts would let a client name host paths | Fixed: `diagnose` takes no path (it writes to `%TEMP%`), `start` takes no image directory (it uses the last engine's, as `restart`), `config_set` refuses path keys, `run` has no mounts. `engine_logs` takes `source` from a fixed set |
| 7 | Unbounded output: many containers, huge log lines, a long pull | | Fixed: the bounds above, `truncated: true`, and a final 256 KiB check per result (tests with 250 containers, a 900 KB log, an over-size result). Pull progress is streamed and only the last 10 messages kept, with 1 MiB per line |
| 8 | Unbounded input | A client could send one huge line | Fixed: 1 MiB per request line; the session ends on a longer one (test) |
| 9 | A write tool reachable without its flag | | Not reachable: the tools are not registered, so `tools/call` fails with "unknown tool" and nothing reaches the host or engine (test over all 12 write and delete tools, and on the VM). `--allow-delete` without `--allow-write` is refused |
| 10 | A read tool with a side effect | `diagnose` writes a zip | Accepted: ADR 0008 lists diagnose as read-only; it writes one new file to `%TEMP%` and is not annotated read-only |
| 11 | Secrets in results: proxy passwords in settings, credentials in state.json errors, tokens in logs | | Fixed: every result through `redact.JSON`, every error through `redact.String`; `settings` never shows secrets (no option); tests plant a proxy URL, a bearer token, a password and a GitHub token |
| 12 | Secrets in arguments | `config_set proxy.http` would carry a password through the model | Fixed: secret and path keys are refused (see above); a refused key never reaches settings.json (test) |
| 13 | Over-redaction | The refusal's advice "credentials: run ..." was itself redacted (the `key: value` rule), seen on the VM | Fixed: the message no longer puts a colon after the word (test) |
| 14 | A flag smuggled in a value (`config_set cpus --apply`) | `config set` parses flags among its arguments | Fixed: the host passes `--` first; the value is validated as a value (test) |
| 15 | Hang when the client closes stdin | The SDK waits for running tool calls before its session ends, and the CLI's `start` ignores cancellation, so a closed client could leave the process up to 4 minutes | Fixed: the server watches stdin itself; when it ends, answers get 2 s to be written, calls are canceled, and it returns 2 s later at most (test with a call that ignores its context; VM: exit in 4 to 309 ms) |
| 16 | Answers lost when input ends | The SDK drops unwritten answers when its input ends, so piped requests got none | Fixed: the end of stdin is held back until the server has written nothing for 300 ms (at most 2 s) |
| 17 | Protocol corruption by stray output on stdout | A command writing to `os.Stdout` would break the JSON-RPC stream | Fixed: every CLI command run for a tool writes to a buffer, and `os.Stdout` points to stderr while the server runs. The engine `start` spawns writes to engine.log and does not inherit the pipes |
| 18 | A byte order mark before the first request | Windows PowerShell 5.1 adds one; the SDK refused the line and the session ended | Fixed: a leading UTF-8 BOM is dropped (VM) |
| 19 | Network use (REQ-UX-005) | The SDK links HTTP transports and an OAuth client | Reviewed: only the stdio transport is constructed; no SDK package dials at init. `internal/notelemetry` lists the Docker API client (a pipe) and the new modules |
| 20 | `MCPGODEBUG` | The SDK panics at init if this variable is malformed (no `=`), which stops every hyperbutler command, not only `mcp` | Open: the panic names the variable; unset it. Not reported upstream yet |

## Open items

- `bench/mcp`, named in REQ-UX-012, does not exist yet: the VM check above was by hand.
- `pull` has no registry credentials. Private images need `docker login` and `docker pull`.
- `status` runs the CLI's `status`; once the update check (REQ-UX-022) is part of
  `status`, decide whether an MCP `status` call may make it.
- The `MCPGODEBUG` panic (review item 20).
- No resources or prompts are served, only tools.
