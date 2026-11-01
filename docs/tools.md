# Other tools, and moving from Docker Desktop

The docker CLI follows the `hyperbutler` docker context that `hyperbutler start` selects.
Many other tools do not: they read `DOCKER_HOST`, or a setting of their own, and
otherwise use `\\.\pipe\docker_engine`, which is Docker Desktop's pipe, not hyperbutler's
(REQ-API-003). The second half of this page moves images and volumes from Docker Desktop
to hyperbutler (REQ-UX-023, REQ-UX-021).

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

## Moving from Docker Desktop (REQ-UX-023, REQ-UX-021)

The two engines keep separate data disks; nothing is shared or migrated automatically.
With both running, one command copies images and volumes; the manual steps below it are
the fallback. Containers are not moved: create them again in hyperbutler from their
compose files (`docker compose up`) or `docker run` lines.

### `hyperbutler import --from-desktop`

Start Docker Desktop and hyperbutler, then:

```powershell
hyperbutler import --from-desktop --dry-run                    # what there is, what is here already
hyperbutler import --from-desktop                              # pick from a numbered list
hyperbutler import --from-desktop --image myapp:1,postgres:16 --volume pgdata
hyperbutler import --from-desktop --all-images --all-volumes
```

(In PowerShell, quote a list with a comma, `"--volume=pgdata,cache"`, or PowerShell
splits it into two arguments.)

- **It is a copy, never a move.** Docker Desktop's images, volumes and containers are
  unchanged and keep working. Before the import, the command records Docker Desktop's
  images and tags, volume names, the content hash of every volume it will read, and its
  containers with their states; afterwards it records them again and prints "Docker
  Desktop unchanged: N images, M volumes, K containers", or what differs (then it exits
  1). Anything Docker Desktop's own containers change during the import shows there too.
- **Where Docker Desktop is.** The endpoint of the `desktop-linux` docker context (its
  `meta.json` under `%DOCKER_CONFIG%` or `~/.docker`, read only, and only a local
  `npipe://` endpoint is used), else `\\.\pipe\dockerDesktopLinuxEngine`. Nothing else of
  Docker Desktop's is read. When nothing answers there: "Docker Desktop is not running:
  start Docker Desktop first", exit 1. hyperbutler must be running too (`hyperbutler
  start`).
- **What is already here.** An image whose id hyperbutler has is "already imported", under
  any tags; if only some of its tags are missing they are added (the tag API, nothing is
  copied). A volume of the same name whose content hash equals Docker Desktop's is
  "already imported". `--dry-run` and the numbered list mark each item new, already
  imported, partly present (tags), conflict, or cannot import (an untagged image; a
  volume that is not a plain local one, such as a bind or NFS volume). A second run right
  after a full one copies 0 bytes: "Nothing to import: 5 images and 2 volumes already
  here.", exit 0. There is no record file: what counts is what hyperbutler has.
- **What is refused** (exit 1, nothing changed): a tag that names another image in
  hyperbutler (remove or retag it there first); a volume of the same name with other
  content (`--overwrite-volumes` deletes hyperbutler's and copies Docker Desktop's); a
  volume a running Docker Desktop container mounts, with the container names: stop them
  in Docker Desktop first, so the copy is consistent. `--allow-live-volumes` copies
  anyway and warns that the copy may be inconsistent. The command never stops anything
  itself.
- **Free space.** Before copying, the planned bytes (the sizes of the new images and of
  the volumes to copy) plus a 10% margin must fit in the free space the VM's data disk
  last reported (the `guestFs` report `status` prints under `data:`) and on the host
  volume of `data.vhdx`, above the 5 GiB the free-space guard keeps. Otherwise the
  import is refused before anything is copied, saying which one is short; `--force-space`
  starts it anyway. A volume Docker Desktop gives no size for is not counted (a note
  says so), and a place whose free space is not known is not checked.
- **One at a time.** Each import removes the helpers it finds left over, so a second one
  in the same installation while one runs (a dry run too) refuses at once: "another
  import is running" (`import.lock` in the state directory).
- **Verification.** After loading an image every tag must name the same image id in
  hyperbutler. A volume is hashed as it streams (entry names, types, permission bits,
  owners, sizes, link targets, contents); the hash must equal the one recorded before the
  copy (else the volume changed while copying), and a fresh export of the copy in
  hyperbutler must hash the same. A mismatch removes the copy from hyperbutler, and only
  there, and exits 1.
- **Progress** goes to stderr (bytes and rate). Ctrl+C cancels: the item in progress is
  removed from hyperbutler, the helpers are removed, and the check of Docker Desktop
  still runs; a second Ctrl+C quits at once. If the command is killed, the next one
  removes the helpers and the half-filled volume it left.
- `--json` prints a schema 1 result: every item with its size, status, outcome, bytes
  copied and reason, the skipped ones, and the check of Docker Desktop
  ([settings.md](research/settings.md#--json-req-ux-008)).

#### How it reads Docker Desktop

Only through Docker Desktop's Docker Engine API, by a client (`internal/desktopimport/source`)
that can send a GET to a fixed list of read-only endpoints (`/_ping`, `/info`, `/version`,
`/images/json`, `/images/{name}/json`, `/images/get`, `/volumes`, `/volumes/{name}`,
`/containers/json`, `/containers/{id}/json`, `/containers/{id}/archive`, `/system/df`) and
nothing else, with one exception, decided on purpose: copying a volume needs the archive
API, and that works on a container. So for each volume it reads, the command creates a
**helper container** in Docker Desktop that

- mounts the volume **read-only** (and never copies image content into it),
- is created from an image already there (the smallest tagged one), so nothing is
  pulled,
- has no network and an entrypoint that does not exist, and is **never started**,
- is labelled `dev.hyperbutler.import=helper`,
- is removed right after the read, also on an error or Ctrl+C (a killed command's helper
  is removed by the next run, after checking it is one: the label, the entrypoint, state
  "created", never started).

That is a short-lived entry in Docker Desktop's container list and a small snapshot on its
disk while it exists; the volume itself is never written. Without any image in Docker
Desktop, volumes are skipped with a message (pull a small image there, or use the manual
steps). A read-only way without a container does not exist in the Docker API: `system df`
gives sizes only, and the volume API has no content endpoint. The client cannot tag,
delete, prune, stop, start, exec or attach; its tests fail if it gains a method or
sends anything else. In hyperbutler the command creates helpers the same way (writable),
volumes with Docker Desktop's labels (so `docker compose` takes them as its own) plus
`dev.hyperbutler.import.source`, and loads images; when hyperbutler has no image for its
helper, Docker Desktop's helper image is copied for the time of the import and removed
afterwards.

Images stream from `GET /images/get` straight into hyperbutler's `POST /images/load`, and
volumes from the helper's archive straight into `PUT /containers/{id}/archive` of
hyperbutler's helper: nothing is buffered whole in memory or written to a file on
Windows, and nothing is extracted on Windows (dockerd extracts the archive inside the VM).

With the containerd image store (Docker Desktop's default for new installs, and
hyperbutler's), an image's id is its index or manifest digest. `images/get` of a
multi-platform image exports the whole index with only the platforms that were pulled
(seen with `alpine:3.22`: the index digest, its manifest for linux/amd64); hyperbutler's
dockerd loads it and keeps the same id. When one engine uses the containerd store and the
other does not, the ids differ for the same image, and an image counts as the same when
its layers, creation time and platform match.

Checked on 2026-11-01 against a second hyperbutler installation serving
`\\.\pipe\dockerDesktopLinuxEngine` (dockerd 29.9 with the containerd image store), not
against Docker Desktop itself: busybox (two tags) and alpine imported and the ids equal;
a volume with nested files, owners, modes 750 and 600, a symbolic and a hard link, and a
300 MiB volume, imported with equal md5s of their file lists and contents; a volume a
running container mounts refused; an import killed during a 400 MiB copy left a helper on
each side and a half-filled volume, which the next run removed before copying it again;
a second run copied 0 bytes and exited 0; the source's images, volumes, volume md5s and
`docker ps -a` were identical before and after, and alpine still ran there.

#### Adversarial review (2026-11-01)

- **Writes to Docker Desktop.** Only the helper container: create and remove. A mount of
  a volume that does not exist would create it, so the helper is created only after the
  volume is looked up; if it is deleted in that instant, Docker Desktop gets a new empty
  volume, and the check after the run reports "volume X is new". Removal is by id, only of
  helpers this run created or ones verified as helpers, never forced.
- **Leftover helpers.** Removed on every path (deferred, on a context that outlives a
  cancel, 30 s each); a failure to remove is printed with the `docker rm` line; a kill is
  cleaned up by the next run, and the check after the run lists any helper still there.
  Two imports at the same time could remove each other's helpers, so the second one of
  an installation refuses while the first runs (`import.lock`).
- **Partial imports.** A failed or canceled load removes the tags it added and the image
  if it was not there; a failed, canceled or mismatching volume copy removes the volume
  it created. A kill between creating a volume in hyperbutler and creating its helper (one
  API call) leaves an empty labelled volume that the next run reports as a conflict
  (open). `--overwrite-volumes` deletes hyperbutler's volume before copying, so a failed
  overwrite leaves none.
- **Name injection.** Names from the command line are checked (the MCP server's checks,
  now `internal/dockerapi`) before anything is sent; names and ids from either engine are
  checked before they go into a path; query values are URL-encoded.
- **Huge streams.** Images and volumes are streamed, never held; JSON answers are bounded
  (8 MiB), error bodies (64 KiB) and the load answer (4 MiB) too. There is no limit on an
  image's or volume's size or on the time a copy takes (Ctrl+C stops it), and no check of
  hyperbutler's free space first (its free-space guard and dockerd's errors apply).
- **A malicious archive from the source.** The checker refuses entries outside the volume
  (`..`, absolute names, backslashes, names not under `v/`), hard links pointing out of
  it, unexpected entry types, a missing or repeated root and short contents, before they
  reach hyperbutler, and re-encodes the archive; dockerd extracts it inside the VM with its
  own protections. hyperbutler never extracts an archive on Windows. Symbolic links are
  passed as they are (they resolve inside a container, as with `docker cp`); setuid bits
  and device nodes are kept as `docker cp` keeps them. An image archive is passed to
  dockerd's load unread, as `docker load` would. Fuzzed: the checker, the load answer and
  the endpoint parser.
- **Verification gaps.** The volume hash leaves out times and extended attributes (file
  capabilities, ACLs), so a copy that lost those still verifies. The hash relies on both
  dockerds walking the volume in the same (lexical) order; a different order fails safe
  (the copy is removed). An image's RepoDigests are not carried over. With different image
  stores on the two sides, image identity falls back to layers, creation time and
  platform.

### By hand (the fallback)

With both running, stream images and volumes from one context to the other: Docker
Desktop's context is `desktop-linux`, hyperbutler's is `hyperbutler`.

#### Images

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

#### Named volumes

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
