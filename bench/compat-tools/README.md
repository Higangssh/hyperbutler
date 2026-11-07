# bench/compat-tools: REQ-API-002, REQ-API-003

Points real client tools at a running hyperbutler's named pipe and runs each tool's own
basic sample. The results go in [docs/compat.md](../../docs/compat.md).

```powershell
powershell -File bench\compat-tools\run.ps1 -ImageDir DIR
```

`DIR` is a guest image no older than the checkout's last change to the guest protocol
(v0.1.1 or later today). An older image boots and runs containers, but Windows bind mounts
fail, and the Dev Containers samples with them; the run prints the bind mount's state
first.

It builds hyperbutler from this checkout (or `-Hyperbutler PATH`), starts one engine on
`\\.\pipe\hb_compat` with `--no-context --backend hcs`, runs the samples, and stops the
engine. It prints a table and writes `results.md`, `results.json` and one log per program
to `bench/results/compat-tools/<time>/` (git-ignored; `-Out` for another folder). The exit
code is 1 if a tool failed; `-Only ID,ID`, `-SkipSettings` and `-SkipOptIn` shorten a run.

## What it runs

| Id | Tool | Sample |
|---|---|---|
| `go-sdk` | Docker SDK for Go (moby client) | ping, pull, run, logs, remove |
| `go-tc` | Testcontainers for Go | nginx, HTTP wait strategy, GET through the mapped port |
| `py-sdk` | Docker SDK for Python | ping, pull, run, logs, remove |
| `py-tc` | Testcontainers for Python | as `go-tc` |
| `node-tc` | Testcontainers for Node | as `go-tc` |
| `java-tc` | Testcontainers for Java (Maven) | as `go-tc` |
| `dotnet-tc` | Testcontainers for .NET (needs an SDK, not only runtimes) | as `go-tc` |
| `devcontainer-image` | `@devcontainers/cli` through npx | `up` of an image, a folder from a temp dir as the workspace, `exec` |
| `devcontainer-features` | the same | `up` of a Dockerfile with a feature |

A runtime that is not on the machine gives `skipped` with the reason. Java needs `java` and
`mvn` on PATH and .NET an SDK; a JDK zip, a Maven zip and `dotnet-install.ps1 -InstallDir`
put first on PATH are enough, with nothing installed. JetBrains IDEs and the VS Code extensions are GUI
programs and listed as not automated, with their settings in
[docs/tools.md](../../docs/tools.md).

**Ryuk.** A Testcontainers sample leaves one more container running when `COMPAT_LEAK=1`,
and the run waits for Ryuk to remove it and itself once the process has gone. If that
fails, it tries again with `TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE=/var/run/docker.sock`,
then with `TESTCONTAINERS_RYUK_DISABLED=true`, and the row names the first setting that
passed and the errors before it. The Python sample ends with `os._exit` there: on a clean
exit the library stops Ryuk itself first, so Ryuk would have nothing to do.

**Without `DOCKER_HOST`.** Each tool also runs with a selected docker context, and each
Testcontainers with `docker.host` in `~/.testcontainers.properties`, with Ryuk off, to
see whether that setting finds the engine. A `no` here is a fact about the tool and does
not fail the run.

**The opt-in.** `hyperbutler config set pipe \\.\pipe\docker_engine`, a new start, and each
tool with nothing set. It changes only the isolated `settings.json`; for as long as it
runs, the engine holds that pipe name for the machine. So it is left out when another
engine serves that pipe or Docker Desktop is installed.

## Isolation

Everything is under `-Root` (default `%TEMP%\hb-dev\compat`): the engine's home, the
docker configs, and for the tools a home folder (`USERPROFILE`, `HOME`, `APPDATA`,
`LOCALAPPDATA`, Java's `user.home`), a temp folder, a venv and the Go, pip, npm, NuGet and
Maven caches. `DOCKER_HOST`, `DOCKER_CONTEXT` and `TESTCONTAINERS_*` are cleared first, so
a real `~/.testcontainers.properties` or `~/.docker` cannot decide a result. It removes
every container of its own engine between samples and touches no other. Delete `-Root`
to start again.

The Go samples are a module of their own (`go/go.mod`), which `go build ./...` at the
repository root does not enter, so the main module gains no dependency.

It needs Hyper-V and the network, so CI does not run it.
