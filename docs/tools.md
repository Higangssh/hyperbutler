# Other tools, and moving from Docker Desktop

The docker CLI follows the `hyperbutler` docker context that `hyperbutler start` selects.
Many other tools do not: they read `DOCKER_HOST`, or a setting of their own, and
otherwise use `\\.\pipe\docker_engine`, which is Docker Desktop's pipe, not hyperbutler's
(REQ-API-003). The second half of this page moves images and volumes from Docker Desktop
to hyperbutler (REQ-UX-021).

hyperbutler's Docker API is at:

```
npipe:////./pipe/docker_engine_hyperbutler
```

(`hyperbutler status` prints it on its `docker:` line; with `config set pipe` it is that
pipe.) hyperbutler never sets `DOCKER_HOST` itself: a global `DOCKER_HOST` overrides every
docker context, Docker Desktop's included, so it is your choice where to set it.

## Tools that ignore docker contexts (REQ-API-003)

### `DOCKER_HOST`, for one shell or for your account

```powershell
# this PowerShell only
$env:DOCKER_HOST = "npipe:////./pipe/docker_engine_hyperbutler"

# every program you start from now on (a user variable; sign out, or restart the IDE,
# for programs already running to see it)
[Environment]::SetEnvironmentVariable("DOCKER_HOST", "npipe:////./pipe/docker_engine_hyperbutler", "User")
# and to undo it:
[Environment]::SetEnvironmentVariable("DOCKER_HOST", $null, "User")
```

In cmd: `set DOCKER_HOST=npipe:////./pipe/docker_engine_hyperbutler`. In Git Bash:
`export DOCKER_HOST=npipe:////./pipe/docker_engine_hyperbutler`. While `DOCKER_HOST` is
set, `docker context use` has no effect in that shell (`hyperbutler start` and `status`
say so). The Docker SDKs for Go, Python (`docker.from_env()`), Java (docker-java) and
.NET (Docker.DotNet) all read it.

### Testcontainers

Testcontainers for Java, Go, Node and .NET read `~/.testcontainers.properties`
(`%USERPROFILE%\.testcontainers.properties`) when `DOCKER_HOST` is not set:

```properties
docker.host=npipe:////./pipe/docker_engine_hyperbutler
```

Its Ryuk container mounts the Docker socket; inside the VM that is dockerd's own
`/var/run/docker.sock`, so the default works. Testcontainers' compatibility is tracked in
REQ-API-002 (`bench/compat-tools`), which has not run yet.

### VS Code

The Docker extension (and Dev Containers) use the docker CLI and its selected context,
so nothing is needed while `hyperbutler` is selected. To pin them to hyperbutler
whatever is selected, in your user `settings.json`:

```json
{
  "docker.environment": { "DOCKER_HOST": "npipe:////./pipe/docker_engine_hyperbutler" }
}
```

(`docker.environment` is the Docker extension's setting; newer releases of the Container
Tools extension call it `containers.environment`.) Dev Containers can also be told
explicitly with `"dev.containers.dockerPath"` if your docker CLI is not on PATH; it then
uses that CLI's context.

### JetBrains IDEs (IntelliJ, GoLand, PyCharm, Rider, ...)

Settings, Build, Execution, Deployment, Docker, then add a Docker connection: choose
**TCP socket** and enter `npipe:////./pipe/docker_engine_hyperbutler` as the Engine API
URL. The **Docker for Windows** choice means `\\.\pipe\docker_engine`, which is Docker
Desktop. The IDEs' services and run configurations then use that connection. Whether every
IDE version accepts an `npipe://` URL there is not checked yet (REQ-API-002).

### Tools that only know `\\.\pipe\docker_engine`

A few tools have the pipe name built in. For them, and only on a machine without Docker
Desktop, hyperbutler can serve that pipe itself:

```powershell
hyperbutler config set pipe \\.\pipe\docker_engine
hyperbutler restart
```

The docker CLI's `default` context then reaches hyperbutler too. `config set` prints a
warning when Docker Desktop is installed (its `Docker Desktop.exe` in
`%ProgramFiles%\Docker\Docker`) or another engine serves that pipe right now: two engines
cannot share one pipe, so whichever starts second fails (`hyperbutler start` with
`pipe-in-use`, or Docker Desktop with its own error). It is never switched automatically,
not even without Docker Desktop, because a later Docker Desktop install would then fail
in a confusing way. Back to the default: `hyperbutler config unset pipe`, then
`hyperbutler restart`.

## Moving from Docker Desktop (REQ-UX-021)

The two engines keep separate data disks; nothing is shared or migrated automatically.
With both running, stream images and volumes from one context to the other: Docker
Desktop's context is `desktop-linux`, hyperbutler's is `hyperbutler`. Containers are not
moved: create them again from their compose files or `docker run` lines.

### Images

```powershell
docker --context desktop-linux save -o "$env:TEMP\images.tar" myapp:1 postgres:16
docker --context hyperbutler load -i "$env:TEMP\images.tar"
Remove-Item "$env:TEMP\images.tar"
```

`docker --context desktop-linux image ls` lists what there is. In cmd or Git Bash a pipe works too and needs no file:

```
docker --context desktop-linux save myapp:1 | docker --context hyperbutler load
```

**Not in Windows PowerShell 5.1.** Its `|` turns the first program's output into text
lines and back, which corrupts a binary stream: the `load` then fails with an unexpected
EOF or a checksum error. Use `-o` and `-i` with a file as above, or run the pipe through
cmd: `cmd /c "docker --context desktop-linux save myapp:1 | docker --context hyperbutler load"`.
(PowerShell 7.4 and later pass bytes through a pipe between two programs unchanged.)

### Named volumes

A volume is copied with `tar` in a small container on each side: one reads the volume
in Docker Desktop, the other writes it in hyperbutler. Again with cmd's pipe, which keeps
the bytes as they are:

```powershell
cmd /c "docker --context desktop-linux run --rm -v pgdata:/from busybox:1.37 tar -C /from -cf - . | docker --context hyperbutler run --rm -i -v pgdata:/to busybox:1.37 tar -C /to -xf -"
```

The volume in hyperbutler is created by that `run` if it does not exist. Stop the
containers that write to the volume first (`docker --context desktop-linux stop ...`), so
the copy is consistent. To check, compare the file lists:

```powershell
docker --context desktop-linux run --rm -v pgdata:/v busybox:1.37 sh -c "cd /v && find . | sort | md5sum"
docker --context hyperbutler    run --rm -v pgdata:/v busybox:1.37 sh -c "cd /v && find . | sort | md5sum"
```

Or with a file in between, which works in any shell, PowerShell 5.1 included:

```powershell
docker --context desktop-linux run --rm -v pgdata:/from -v "${env:TEMP}:/out" busybox:1.37 tar -C /from -cf /out/pgdata.tar .
docker --context hyperbutler run --rm -v pgdata:/to -v "${env:TEMP}:/in" busybox:1.37 tar -C /to -xf /in/pgdata.tar
Remove-Item "$env:TEMP\pgdata.tar"
```

(The second line binds `%TEMP%` into hyperbutler's VM, which works for any folder on a
local drive.) `tar` keeps owners, permissions and symbolic links; a database's own dump
tool (`pg_dump`, `mysqldump`) is the safer way for a database that must stay running.

### Afterwards

- `docker context use hyperbutler` (or `hyperbutler start`) selects hyperbutler; `docker
  context use desktop-linux` goes back. `hyperbutler status` says which one is selected
  when it is not `hyperbutler`.
- Docker Desktop writes `"credsStore": "desktop"` into the docker CLI's `config.json`. Once
  Docker Desktop is uninstalled, `docker login` and `docker pull` fail with "error getting
  credentials". `hyperbutler start` and `hyperbutler doctor` warn about it and say what to
  change; hyperbutler never edits that line itself.
- `hyperbutler doctor` lists what else may still point at Docker Desktop.
