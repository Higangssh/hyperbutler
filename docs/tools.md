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
With both running, one command copies images and volumes and creates the containers made
with `docker run` again (REQ-UX-024); the manual steps below it are the fallback. A Compose
project's containers are not moved: `docker compose up` in its folder creates them.

### `hyperbutler import --from-desktop`

Start Docker Desktop and hyperbutler, then:

```powershell
hyperbutler import --from-desktop --dry-run                    # what there is, what is here already
hyperbutler import --from-desktop                              # pick from a numbered list
hyperbutler import --from-desktop --image myapp:1,postgres:16 --volume pgdata
hyperbutler import --from-desktop --all-images --all-volumes
hyperbutler import --from-desktop --container web              # its image and volumes come along
hyperbutler import --from-desktop --all-containers
hyperbutler import --from-desktop --confirm-container vpn      # one that needs a yes
```

(In PowerShell, quote a list with a comma, `"--volume=pgdata,cache"`, or PowerShell
splits it into two arguments.)

- **It is a copy, never a move.** Docker Desktop's images, volumes and containers are
  unchanged and keep working. Before the import, the command records Docker Desktop's
  images and tags, volume names, the content hash of every volume it will read, and its
  containers with their states; afterwards it records them again and prints "Docker
  Desktop unchanged: N images, M volumes, K containers", or what differs (then it exits
  1). A difference is something the import could have caused: an image, a tag, a
  volume, the content of a volume it read, one of its helper containers still there. A
  container that changed state, or was created or removed, during the import is a
  warning only (the import never starts, stops, creates or removes a container of the
  user's: it was the user or Docker Desktop), and the exit is 0 when every item is OK.
- **Where Docker Desktop is.** The endpoint of the `desktop-linux` docker context (its
  `meta.json` under `%DOCKER_CONFIG%` or `~/.docker`, read only, and only a local
  `npipe://` endpoint is used), else `\\.\pipe\dockerDesktopLinuxEngine`. Nothing else of
  Docker Desktop's is read. When nothing answers there: "Docker Desktop is not running:
  start Docker Desktop first", exit 1. hyperbutler must be running too (`hyperbutler
  start`).
- **What is already here.** An image whose id hyperbutler has is "already imported", under
  any tags; if only some of its tags are missing they are added (the tag API, nothing is
  copied). A volume of the same name whose content hash equals Docker Desktop's, with the
  same labels (those docker compose relies on; the import's own `dev.hyperbutler.import*`
  labels left out) and no driver options, is "already imported". `--dry-run` and the
  numbered list mark each item new, already imported, partly present (tags), conflict,
  same content but different labels, or cannot import (an untagged image; a volume that
  is not a plain local one, such as a bind or NFS volume). A second run right after a
  full one copies 0 bytes: "Nothing to import: 5 images and 2 volumes already here.",
  exit 0. What counts is what hyperbutler has; `imports-complete.json` in the state
  directory only records which volumes an import verified, so that the recovery of a
  killed import never removes one.
- **What is refused** (exit 1, nothing changed): a tag that names another image in
  hyperbutler (remove or retag it there first); a volume of the same name with other
  content, or with the same content and other labels (`--overwrite-volumes` replaces it,
  below); a volume a running Docker Desktop container mounts, with the container names:
  stop them in Docker Desktop first, so the copy is consistent. `--allow-live-volumes`
  copies anyway and warns that the copy may be inconsistent. The command never stops
  anything itself. One exception is not refused: an empty hyperbutler volume without
  docker compose labels that no hyperbutler container uses, where Docker Desktop's is
  empty too, is created again with Docker Desktop's labels (nothing is lost).
- **`--overwrite-volumes`** never leaves hyperbutler without either the old content or
  a verified copy of the new one. Docker Desktop's volume is copied into a temporary
  volume (`hyperbutler-import-<run>-<hash>`) and verified; only then is hyperbutler's
  volume deleted, created again with Docker Desktop's labels, filled from the temporary
  one inside hyperbutler (through the archive API of two helpers there) and verified
  again, and the temporary one removed. A failure, Ctrl+C or kill before the delete
  leaves the old volume as it was; after it, the temporary volume holds Docker Desktop's
  verified content and the message (and the next import) names it. A volume a
  hyperbutler container uses is refused first. It needs twice the volume's size while
  it runs.
- **Free space.** Before copying, the planned bytes (the sizes of the new images and of
  the volumes to copy, twice for one `--overwrite-volumes` replaces) plus a 10% margin
  must fit in the free space the VM's data disk last reported (the `guestFs` report
  `status` prints under `data:`) and on the host volume of `data.vhdx`, above the 5 GiB
  the free-space guard keeps. Otherwise the import is refused before anything is copied,
  saying which one is short; `--force-space` starts it anyway. A volume Docker Desktop
  gives no size for is not counted (a note says so), and a place whose free space is
  not known is not checked. The import itself checks it (`internal/desktopimport`'s
  Run), so an import from a GUI is checked too.
- **One at a time.** An import holds the installation's `import.lock` (state directory)
  while it runs; a second one in the same installation, from the CLI or a GUI, refuses at
  once: "another import is running". A dry run takes no lock: it removes nothing. Each
  import run has a random id, held alive by a named mutex
  (`Global\hyperbutler-import-<id>`, else `Local\...`) that goes when its process ends,
  and labels its helpers and new volumes with it (`dev.hyperbutler.import.run`). An
  import removes only helpers and volumes of runs that are not alive, so two imports from
  two installations reading the same Docker Desktop leave each other's alone.
- **Verification.** After loading an image every tag must name the same image id in
  hyperbutler. A volume is hashed as it streams (entry names, types, permission bits,
  owners, sizes, link targets, contents); the hash must equal the one recorded before the
  copy (else the volume changed while copying), and a fresh export of the copy in
  hyperbutler must hash the same, read through a new read-only helper after the helper
  that filled it is removed. A mismatch removes the copy from hyperbutler, and only
  there, and exits 1. A verified volume is recorded in `imports-complete.json` (removed
  by `uninstall`, kept with `--keep-data`).
- **Progress** goes to stderr (bytes and rate). Ctrl+C cancels: the item in progress is
  removed from hyperbutler, the helpers are removed, and the check of Docker Desktop
  still runs; a second Ctrl+C quits at once. If the command is killed, the next import
  (not a dry run, which only lists it) removes what it left: helpers whose run is gone,
  after the checks below, in both engines; a hyperbutler volume only when a left-over
  helper of that run was filling it, the run created it, and it is not recorded as
  verified; a temporary `--overwrite-volumes` volume when it was not verified or its
  volume is there again (a verified one whose volume is gone is kept and named).
- An aborted image load can leave layers no image refers to in hyperbutler's image
  store until dockerd's garbage collection removes them; `docker image prune` does not
  see them, and they take space until then.
- `--json` prints a schema 1 result: every item with its size, status, outcome, bytes
  copied and reason, the skipped ones, and the check of Docker Desktop
  ([settings.md](research/settings.md#--json-req-ux-008)).

#### Containers (REQ-UX-024)

`--dry-run` and the numbered list show Docker Desktop's containers next to its images and
volumes. `--container` and `--all-containers` create them again in hyperbutler from
`docker inspect`, read through the same read-only client, and **do not start them**:
Docker Desktop's may still hold their ports. Stop that one, then `docker start` the new
one. A restart policy does not start it either: dockerd restarts only containers that have
run (checked on 2026-11-07 by restarting the VM).

- **What carries over.** Image, command and entrypoint, environment, labels, working
  directory, user, hostname if it was set, stop signal and timeout, health check, published
  ports, restart policy, auto-remove, named volumes, bind mounts, tmpfs mounts, user-defined
  networks with aliases and fixed addresses, DNS and extra hosts, capabilities, read-only
  root, init, sysctls, and limits (memory, swap, CPU, cpuset, PIDs, block I/O weight,
  ulimits, OOM settings, shm size). The new container also gets
  `dev.hyperbutler.import.source`, `dev.hyperbutler.import.container` (the id of the
  container it was made from), which is how a second run knows it: "already imported", and
  `dev.hyperbutler.import.run`.
- **What does not**, and the plan says so for that container: the writable layer, with
  its size (what the container wrote outside its volumes; the plan says nothing for a
  container whose layer is empty, and "may hold files" when the engine gives no size); an
  anonymous volume's content (the new container gets an empty one); a log driver other than
  `json-file` or `local`, a runtime other than runc, a volume driver, a cgroup parent, a
  `--mount`'s bind options (propagation, recursion, creating the mountpoint) and volume
  driver settings, and a network's IPAM driver other than the default. A
  MAC address, device-specific block I/O limits, and any setting not listed above are left
  out without a word: the create is built field by field, nothing is passed through.
- **Its image and volumes come along.** Picking a container adds its image and named
  volumes to the import, with the usual checks and the free-space count. Run does the
  containers last and refuses one whose image or volume did not make it, with the reason:
  a volume a running Docker Desktop container mounts is refused, so stop that one first
  (the plan warns) or use `--allow-live-volumes`. A create would otherwise make an empty
  volume of that name, which would then stand in the way of the real one. An image that
  cannot come along is flagged in the plan: it has no tag any more (the tag moved to a
  newer image), it is not in Docker Desktop's list, or its tag names another image in
  hyperbutler. An untagged image hyperbutler already has by id is used by id.
- **Networks.** A user-defined network hyperbutler lacks is created first; one of that name
  is used as it is. Only a bridge network can be created (overlay, macvlan and others list
  the container as "cannot import"), with its labels, internal and IPv6 flags and MTU; other
  driver options mean something on Docker Desktop's VM only. Its subnet goes along only when
  a container has a fixed address on it, because inspect also shows a subnet the engine
  picked, and that one may be taken in hyperbutler. The plan and a run's output say
  "network X will be created", with that subnet. A network the run made for a container
  that then failed is removed, unless that container may remain.
- **Needs a confirmation.** Listed with the reason and skipped by `--all-containers`
  (refused, exit 1, when picked with `--container`), until `--confirm-container NAME`, or
  a yes in the numbered list, says otherwise: a bind source hyperbutler refuses (REQ-FS-006,
  `share.CheckBindSource`, the proxy's own check: UNC, WSL, device paths, a network drive);
  a bind source that is a folder inside Docker Desktop's VM (hyperbutler's VM has its own,
  and the content is not copied); a source with a `..` on either separator; host
  networking; the host's PID, IPC, UTS, user or cgroup namespace; each device (by its host
  path), device cgroup rule and `--gpus` request; `--privileged`; the capabilities `ALL`,
  `SYS_ADMIN`, `SYS_MODULE`, `SYS_RAWIO`, `SYS_PTRACE`, `DAC_READ_SEARCH`, `BPF` and `PERFMON`,
  in any spelling; security options other than `no-new-privileges`. Every one is listed
  whole: a security option by its key and short value, or the size of a long one (an inline
  seccomp profile). A confirmed container keeps these, with one exception: hyperbutler's
  engine refuses that bind source in any create, so the container is created **without
  that mount**, as the plan says.
- **Shown before the copy.** Added capabilities, bind mounts and networks to create are in
  the plan's row and in `--json` (`capAdd`, `binds`, `notes`), and a run prints them, and
  what was confirmed, on stderr
  before it copies anything, also with names on the command line, where there is no plan to
  read. A container's environment and command are never printed: they may hold secrets.
- **Bind paths.** A source as the user wrote it (`C:\src`) stays. `/run/desktop/mnt/host/c/src`,
  `/host_mnt/c/src` and `/mnt/host/c/src`, Docker Desktop's views of a Windows folder, become
  `C:\src`, unless it has a `..`; `/run/desktop/mnt/host/wsl/...` and
  `/mnt/wsl/docker-desktop-bind-mounts/...` are read as a WSL path and refused. These forms are from memory of what Docker Desktop
  reports, not checked against it; any other path counts as a folder in its VM, which
  needs a confirmation, so a form this list misses is shown, not silently emptied.
- **Compose.** A container with a non-empty `com.docker.compose.project` label is never
  recreated,
  confirmed or not. The plan names the project and says to run `docker compose up` in its
  folder (`com.docker.compose.project.working_dir`) instead of offering it.
- **Cannot import**, confirmed or not: `--link`, `--volumes-from`, another container's
  network, PID or IPC namespace (they name a container of Docker Desktop's); a volume that
  is not a plain local one; a container Kubernetes, swarm or a Docker Desktop extension
  manages; a value that cannot be read.
- **Name clashes.** A container of that name in hyperbutler, other than one an import made
  from this container, is reported and never overwritten; also when it appears between the
  plan and the run (the create's 409). Two containers of one name in what the source lists
  are both "cannot import".
- **The inspect answer is untrusted.** The source engine could be anything: answers are
  bounded (8 MiB, as all JSON here), at most 4096 containers are read, a create over 4 MiB
  is refused as hyperbutler's proxy would, an unreadable answer lists that one container as
  "cannot import", and every path, port, name and mode is checked before use
  (`mapContainer`, `FuzzMapContainer`). A hostile engine could still name a real Windows
  folder as a bind source: it is shown before the copy, and the container is not started.
- **Failures.** A create that fails, is canceled by Ctrl+C, or is not found afterwards in
  state `created` with its label: the name is looked up on a context that outlives the
  cancel, a container with this run's label is removed, and so is any network made for it.
  One that cannot be removed, or does not carry the label, stays with its network, and the
  result says it may remain and how to remove it, instead of that nothing is left.

Tested against the two fake engines only (`container_test.go`): not against Docker
Desktop, and not on the VM.

#### How it reads Docker Desktop

Only through Docker Desktop's Docker Engine API, by a client (`internal/desktopimport/source`)
that can send a GET to a fixed list of read-only endpoints (`/_ping`, `/info`, `/version`,
`/images/json`, `/images/{name}/json`, `/images/get`, `/volumes`, `/volumes/{name}`,
`/containers/json`, `/containers/{id}/json`, `/containers/{id}/archive`, `/networks/{name}`,
`/system/df`) and nothing else, with one exception, decided on purpose: copying a volume needs the archive
API, and that works on a container. So for each volume it reads, the command creates a
**helper container** in Docker Desktop that

- mounts the volume **read-only** (and never copies image content into it),
- is created from an image already there, so nothing is pulled: the smallest tagged one
  that declares no `VOLUME` and is for the engine's platform (`/info`). A container
  created from an image with `VOLUME` (redis, postgres, mysql and many more) gets an
  anonymous volume for each, which would be a change to Docker Desktop,
- has no network and an entrypoint that does not exist, and is **never started**,
- is labeled `dev.hyperbutler.import=helper` and `dev.hyperbutler.import.run=<id>`,
- is removed right after the read with any anonymous volume (`v=1`, in case; a named
  volume is never removed by it), also on an error or Ctrl+C (a killed command's helper
  is removed by the next run, after checking it is one: the label, exactly the
  entrypoint, state "created", never started, and its run no longer alive).

That is a short-lived entry in Docker Desktop's container list and a small snapshot on its
disk while it exists; the volume itself is never written. Without such an image in Docker
Desktop, volumes are skipped with a message that says why (pull a small image without
`VOLUME` there, such as busybox, or use the manual steps). A read-only way without a
container does not exist in the Docker API: `system df` gives sizes only, and the volume
API has no content endpoint. The client cannot tag, delete, prune, stop, start, exec or
attach; its tests fail if it gains a method or sends anything else. In hyperbutler the
command creates helpers the same way, from an image picked by the same rules: read-only
to hash a volume, writable only to fill a new one; volumes with Docker Desktop's labels
(so `docker compose` takes them as its own) plus `dev.hyperbutler.import.source` and the
run id; and loads images. When hyperbutler has no such image, Docker Desktop's helper
image is copied for the time of the import and removed afterwards.

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
its layers, creation time, platform and config (command, entrypoint, environment,
labels, working directory, user, exposed ports, volumes) match.

Checked on 2026-11-01 against a second hyperbutler installation serving
`\\.\pipe\dockerDesktopLinuxEngine` (dockerd 29.9 with the containerd image store), not
against Docker Desktop itself: busybox (two tags) and alpine imported and the ids equal;
a volume with nested files, owners, modes 750 and 600, a symbolic and a hard link, and a
300 MiB volume, imported with equal md5s of their file lists and contents; a volume a
running container mounts refused; an import killed during a 400 MiB copy left a helper on
each side and a half-filled volume, which the next run removed before copying it again;
a second run copied 0 bytes and exited 0; the source's images, volumes, volume md5s and
`docker ps -a` were identical before and after, and alpine still ran there.

Checked again on 2026-11-01 after the second review, the same way (two test
installations, the source on `\\.\pipe\dockerDesktopLinuxEngine`, a `desktop-linux`
context in the target's test `DOCKER_CONFIG`). redis:alpine 8.10.3 no longer declares
`VOLUME`, so the source had a 9 kB image made with `docker image import --change
'VOLUME /data'` as its smallest tagged image, and redis:alpine: a volume import used
redis:alpine for the helpers, and `docker volume ls` on the source was the same before
and after. An import with a test hook that leaves the filling helper behind
(`HYPERBUTLER_TEST_IMPORT_HOOK=keep-copying-helper`) succeeded; the next one removed the
helper, kept the volume ("was verified by the import that made it") and the md5s on both
sides were equal. `--overwrite-volumes` of a 400 MiB volume killed while it filled the
temporary volume left hyperbutler's old content intact (same md5); killed after the
temporary copy was verified (the old volume deleted), the temporary volume held the
verified content and the next import named it, removed the half-filled volume and
copied it again; the import after that removed the temporary volume. Every run said
"Docker Desktop unchanged".

#### Adversarial review (2026-11-01)

- **Writes to Docker Desktop.** Only the helper container: create and remove. A mount of
  a volume that does not exist would create it, so the helper is created only after the
  volume is looked up; if it is deleted in that instant, Docker Desktop gets a new empty
  volume, and the check after the run reports "volume X is new". Removal is by id, only of
  helpers this run created or ones verified as unused helpers of a run that is gone,
  never forced, with their anonymous volumes. (Corrected, H1: helpers used to be created
  from the smallest image even when it declared `VOLUME`, which left an anonymous volume
  in Docker Desktop on every read and made every run report a change.)
- **Leftover helpers.** Removed on every path (deferred, on a context that outlives a
  cancel, 30 s each); a failure to remove is printed with the `docker rm` line; a kill is
  cleaned up by the next run, and the check after the run lists any helper of its own
  still there. (Corrected, M1: `import.lock` was taken by the CLI only and per
  installation, so neither two installations nor a GUI import next to a CLI one were
  stopped from removing each other's helpers; now the import itself takes the lock, every
  helper carries its run's id, and only helpers of runs that are no longer alive are
  removed.)
- **Partial imports.** A failed or canceled load removes the tags it added and the image
  if it was not there (layers it unpacked stay until dockerd's garbage collection, L7); a
  failed, canceled or mismatching volume copy removes the volume it created. A kill
  between creating a volume in hyperbutler and creating its helper (one API call), or
  between removing a failed copy's helper and its volume, leaves a volume the next run
  reports (a conflict, or "already" when the content is equal) instead of removing it
  (open: it cannot tell it from a user's volume without the helper). (Corrected, M5:
  `--overwrite-volumes` used to delete hyperbutler's volume before copying; now the old
  one stays until a verified copy is in hyperbutler.)
- **Name injection.** Names from the command line are checked (the MCP server's checks,
  now `internal/dockerapi`) before anything is sent; names and ids from either engine are
  checked before they go into a path; query values are URL-encoded.
- **Huge streams.** Images and volumes are streamed, never held; JSON answers are bounded
  (8 MiB), error bodies (64 KiB) and the load answer (4 MiB) too. There is no limit on an
  image's or volume's size or on the time a copy takes (Ctrl+C stops it). (Corrected:
  hyperbutler's free space is checked first, by the import itself, see Free space.)
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
  stores on the two sides, image identity falls back to layers, creation time, platform
  and config (corrected, M4: the config was not compared). A volume's labels and driver
  options are compared too (corrected, M3: "already imported" looked at the content
  only).

Second review (2026-11-01), each fixed with a regression test against the two fake
engines (`internal/desktopimport`, `review_test.go` unless named):

| ID | Finding | Outcome |
|----|---------|---------|
| H1 | Helpers created from an image with `VOLUME` (redis, postgres, mysql) made anonymous volumes in Docker Desktop that were never removed (`v=0`); every run then reported a change. | Fixed. Helper images on both sides must have no `VOLUME` and match the engine's OS and architecture; without one in Docker Desktop, volumes are skipped with a note. Helpers are removed with `v=1`. The fake makes anonymous volumes; `TestHelperImageWithoutVolumes`, `TestNoHelperImageSkipsVolumes`. Checked on the VM. |
| H2 | Recovery could delete a complete, or later changed, hyperbutler volume: the filling helper's label stayed until the end and its removal could fail silently. | Fixed. The filling helper is removed right after the copy (tried three times; a failure fails the item), and the copy is verified through a new read-only helper. Volumes and helpers carry the run id; recovery deletes a volume only for an orphaned filling helper of the run that created it, and never one recorded in `imports-complete.json` after its verification. `TestCopyingHelperRemovalFailsOnce`, `TestCopyingHelperRemovalFails`, `TestStuckCopyingHelperKeepsVerifiedVolume`, `TestRecoveryRules`. Checked on the VM with a test hook. |
| M1 | The per-installation lock did not cover two installations or a GUI import next to a CLI one, so they could remove each other's helpers. | Fixed. A named mutex per run tells live runs from dead ones; recovery skips helpers and volumes of live runs. The lock moved into the package (a GUI import takes it too). `TestParallelImports`. |
| M2 | One volume that could not be hashed before the import stopped the whole run. | Fixed. That volume fails; images and the other volumes go on. `TestHashFailureIsPerVolume`. |
| M3 | "Already imported" ignored a volume's labels and options. | Fixed. New status "same content, different labels"; an empty, unused volume without compose labels is created again with Docker Desktop's labels; otherwise `--overwrite-volumes` is needed. `TestVolumeLabelsCompared`. |
| M4 | With different image stores, images with the same layers, time and platform but another config counted as the same. | Fixed. Cmd, Entrypoint, Env, Labels, WorkingDir, User, ExposedPorts and Volumes are compared. `TestMixedStoreComparesConfig`. |
| M5 | `--overwrite-volumes` deleted hyperbutler's volume before copying. | Fixed. Copy to a temporary volume and verify, then replace; the space check counts the volume twice. `TestOverwriteKeepsOldUntilVerified`. Checked on the VM by killing it in both phases. |
| L1 | Container state changes (the user's) made the run fail. | Fixed. They are warnings; differences are what the import could cause. `TestCompareSnapshots`, `TestSourceChangeIsReported`. |
| L2 | `--dry-run` (Plan) removed left-over helpers and volumes. | Fixed. Plan lists them (`Plan.Recovery`); only Run removes. `TestPlanRemovesNothing`. |
| L3 | A dry run with names matched them differently from the import. | Fixed. `Plan.Select` uses the import's resolution and refuses unknown names. `TestPlanSelect`. |
| L4 | hyperbutler's volumes were read through writable helpers. | Fixed. Read-only, except the one that fills a new volume. `TestTargetReadsAreReadOnly`. |
| L5 | Stale helpers in hyperbutler were removed on the label alone. | Fixed. The same checks as in Docker Desktop (label, exact entrypoint, created, never started) plus the run. `TestRecoveryRules`. |
| L6 | The helper image's platform was not checked. | Fixed with H1. `TestHelperImageWithoutVolumes`. |
| L7 | An aborted load can leave unreferenced layers. | Documented above (until dockerd's garbage collection). |
| L8 | A volume live at plan time but not at run time was not hashed, so it failed. | Fixed. Run decides from the containers running then. `TestVolumeNoLongerLive`. |
| L9 | REQ-UX-023's text said nothing is ever changed in Docker Desktop, without the helper. | Fixed in docs/requirements.md. |
| L10 | PowerShell splits an unquoted `--volume a,b`; the error did not say why. | Fixed. The error names the flag and how to quote. `TestImportUsage` (cmd/hyperbutler). |

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
