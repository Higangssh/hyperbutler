# Usage scenarios: what happens, and what should

Research snapshot, 2026-11-01. The maintainer asked: "With Docker Desktop, typing `docker
compose up -d` in a terminal works when Desktop is running. Is ours the same? What happens
if Docker Desktop and ours are both running?" This note answers that, and goes through the
other everyday situations a Windows user meets: the engine not running, sign-out and sleep,
Docker Desktop next to hyperbutler, other tools, other users, settings changes, a full disk,
networks, upgrades, bind mounts and crashes.

For each scenario:

- **What the user does:** the commands and the Windows situation.
- **Today:** what the code does now, with file:line where it decides. Each is marked
  *verified on VM* (run on the reference machine on 2026-11-01, see
  [How it was checked](#how-it-was-checked)), *from code*, or *unknown*.
- **Docker Desktop:** what it does, marked *docs* (from Docker's documentation),
  *well-known* (common, widely reported behaviour) or *unknown*. Docker Desktop is not
  installed on the test machine, so nothing in this column was run.
- **Intended:** what hyperbutler should do. It is chosen to be at least as good as Docker
  Desktop, within the project's rules: no resident GUI, no silent restarts, no account, no
  telemetry, and never change the user's setup silently.
- **Gap:** *none*, *small* or *needs work*, with the requirement that covers it. New IDs
  are proposals only, collected in [Proposed requirements](#proposed-requirements);
  [requirements.md](../requirements.md) is unchanged until the maintainer accepts them.

## The short answer

- **`docker compose up -d` in a new terminal works the same way as with Docker Desktop**,
  once `hyperbutler start` has run. The selected docker context is stored in the docker
  CLI's `config.json`, not in the environment. So every terminal sees it, including ones
  opened before the start: PowerShell, cmd, Git Bash, VS Code's terminal. The bundled
  compose plugin is registered unless another one is installed. Compose itself has not
  been tested against hyperbutler yet (REQ-API-001).
- **When the engine is stopped**, the docker CLI prints the same kind of error as with
  Docker Desktop not running. Neither product can print "start the engine" there,
  because nothing listens on the pipe. Docker Desktop starts at sign-in by default and
  hyperbutler does not, so a stopped engine is more common with hyperbutler. The
  proposals: say so when `stop` runs, put the hint in the context description and the
  docs, and offer an opt-in autostart (S1.1, S2.3).
- **With both running**, a plain `docker` talks to whichever context is selected. The
  last of the two to select its own context wins. `DOCKER_HOST` or `DOCKER_CONTEXT`
  overrides the selection in one shell, and `docker --context NAME` in one command. The
  two engines keep separate images, containers and volumes. They do not share pipes. They
  do share the host's ports, and today a port collision is **not reported to the user**
  and can split traffic between the two engines (S3.4). That is the most important fix
  in this note.

## Summary

| # | Scenario | Gap | Requirement |
|---|---|---|---|
| S0.1 | Double-clicking `hyperbutler.exe` in Explorer | needs work | REQ-UX-016 (new) |
| S0.2 | A Start-menu or desktop shortcut | small | REQ-UX-016 (new) |
| S0.3 | Opening the GUI (0.2): does it start or stop the engine? | small (design) | REQ-UX-011, REQ-UX-016 (new) |
| S1.1 | `docker ps` / `run` / `compose up` with the engine stopped | small | REQ-UX-014 (new) |
| S1.2 | Starting on demand when a docker command arrives | needs work (later) | REQ-START-002 (new), REQ-MEM-004 |
| S2.1 | Closing the terminal that ran `hyperbutler start` | none | REQ-UX-001 |
| S2.2 | Containers with `--restart` after `stop` and `start` | none | REQ-API-001 |
| S2.3 | Windows sign-in: start automatically? | needs work | REQ-UX-015 (new) |
| S2.4 | Sign-out, shutdown or reboot with containers running | small | REQ-PWR-003 (new) |
| S2.5 | Fast startup | small | REQ-PWR-003 (new) |
| S2.6 | Sleep, hibernate and resume | unknown, needs a bench | REQ-PWR-001/002, REQ-PWR-003 (new) |
| S2.7 | `hyperbutler stop` with containers running | small | REQ-UX-017 (new) |
| S3.1 | Docker Desktop installed, one or both stopped | small | REQ-UX-014 (new) |
| S3.2 | Both running: which engine does `docker` reach? | small | REQ-UX-018 (new), REQ-PLAT-004 |
| S3.3 | Docker Desktop selects `desktop-linux` again when it starts | small | REQ-UX-018 (new) |
| S3.4 | Both publish the same port | **needs work** | REQ-NET-005 (new), REQ-NET-001 |
| S3.5 | `host.docker.internal` differences | small (documented) | REQ-NET-004 |
| S3.6 | Compose plugin and credential helper (`credsStore: desktop`) | small | REQ-UX-019 (new) |
| S3.7 | Moving images and volumes from Docker Desktop | small (docs) | REQ-UX-021 (new) |
| S3.8 | WSL 2–based Docker Desktop and hyperbutler's VM at the same time | unknown, needs a bench | REQ-PLAT-004 |
| S4.1 | PowerShell, cmd, Git Bash | small | REQ-API-001 |
| S4.2 | VS Code Docker extension and Dev Containers | unknown, likely none | REQ-API-002 |
| S4.3 | Testcontainers, Docker SDKs | needs work (docs) | REQ-API-002, REQ-API-003 (new) |
| S4.4 | JetBrains IDEs | unknown | REQ-API-002 |
| S4.5 | Tools that only know `\\.\pipe\docker_engine` | small | REQ-API-003 (new) |
| S5.1 | A second Windows user on the same PC | **needs work** | REQ-UX-020 (new) |
| S5.2 | An elevated terminal of the same user | small (unverified) | REQ-UX-020 (new) |
| S5.3 | An elevated terminal of a different (admin) account | small | REQ-UX-020 (new) |
| S6.1 | `config set ... --apply` that restarts dockerd | small | REQ-UX-017 (new), REQ-UX-006 |
| S6.2 | `config set memory ... --apply` (VM restart) | small | REQ-UX-017 (new) |
| S6.3 | Live settings (`hostGateway.*`) | none | REQ-NET-004 |
| S7.1 | The data disk (guest file system) is full | needs work | REQ-DISK-009 (new), REQ-DISK-005 |
| S7.2 | The host volume is nearly full | small | REQ-DISK-004, REQ-DISK-010 (new) |
| S7.3 | A container uses more memory than the VM has | none (bench open) | REQ-MEM-003 |
| S8.1 | VPN connect or disconnect while running | none (one client tested) | REQ-NET-002 |
| S8.2 | Wi-Fi change, airplane mode | none (from code) | REQ-NET-002 |
| S8.3 | Corporate proxy | small | REQ-UX-009 |
| S9.1 | Unzipping a new version over the running one | small | REQ-PLAT-008 (new), REQ-PLAT-005 |
| S9.2 | `hyperbutler start` from a new folder while the old engine runs | small | REQ-PLAT-008 (new) |
| S10.1 | Paths with spaces or Korean characters | none | REQ-FS-004 |
| S10.2 | OneDrive folders | unknown | REQ-FS-004 |
| S10.3 | Network drives, UNC and `\\wsl$` paths | small | REQ-FS-006 (new) |
| S10.4 | File watching | none | REQ-FS-003 |
| S11.1 | The VM or dockerd crashes | none | REQ-UX-003 |
| S11.2 | The engine process is killed | none | REQ-DISK-003 |
| S11.3 | Something is wrong and the user wants help | small | REQ-UX-003 |

---

## 0. Launching the app

### S0.1 Double-clicking `hyperbutler.exe`

- **What the user does:** unzips the release and double-clicks `hyperbutler.exe` in
  Explorer, as with most Windows programs.
- **Today** *(from code)*: with no arguments, `run` prints the usage to stderr and returns
  2 (`cmd/hyperbutler/main_windows.go:56-58`; the exit code was *verified on VM*). The
  console window Windows opened for it closes at once, so the user sees a flash and
  nothing else. Nothing starts.
- **Docker Desktop** *(well-known)*: the app opens its window and starts the engine.
- **Intended (0.1, CLI only):** when `hyperbutler.exe` has no arguments **and owns its
  console**, it behaves as a small interactive launcher instead of exiting. "Owns its
  console" means `GetConsoleProcessList` returns 1: the console was created for this
  process, which is what Explorer, the Run box and a shortcut do. A shell would make it
  2 or more. In that case it:
  1. prints one line about what hyperbutler is, and the status (the `hyperbutler status`
     text);
  2. if the engine is stopped: `Press Enter to start the engine, or close this window.`
     Enter runs `start`; closing the window does nothing. It asks because a VM boot uses
     memory, and an accidental double-click should not start one;
  3. after a start, or when the engine already runs: what to type in a terminal
     (`docker run hello-world`, `hyperbutler stop`), and that **the engine keeps running
     when this window closes**, until `hyperbutler stop` or sign-out;
  4. waits for Enter, then exits.

  Started from a shell with no arguments, it keeps printing the usage. The exit code
  should become 0 for that case: asking for usage is not an error.
- **Intended (0.2, with the GUI):** the same detection opens the GUI window instead
  (`hyperbutler gui`), with the behaviour of S0.3. The console launcher stays for
  machines where the GUI cannot run.
- **Gap:** needs work. REQ-UX-016 (new).

### S0.2 A Start-menu or desktop shortcut

- **Today** *(from code)*: the zip has no shortcut and nothing creates one. That fits
  "hyperbutler never changes your setup".
- **Docker Desktop** *(well-known)*: the installer adds a Start-menu entry and a desktop
  icon, which open the app.
- **Intended:** 0.1 ships no shortcut. A user who makes one by hand gets S0.1's launcher.
  From 0.2 the MSI (ADR 0008, REQ-UX-010) adds one Start-menu entry, "hyperbutler", which
  opens the GUI. `uninstall` lists and removes it. There is no desktop icon unless the
  installer's checkbox for it is ticked (off by default).
- **Gap:** small. REQ-UX-016 (new).

### S0.3 The 0.2 GUI: start, stop and closing

- **What the user does:** opens the GUI from the Start menu; closes it later.
- **Today:** there is no GUI. The ADR 0008 mockups show a first-run screen with a big
  **시작할게요** ("Start") button.
- **Docker Desktop** *(well-known)*: opening the app starts the engine. Closing the window
  keeps the engine and the tray running; only "Quit Docker Desktop" stops it.
- **Intended:**
  - **Opening never starts the engine by itself.** The window shows the engine's phase at
    once. When it is stopped, a primary **Start** button is the first thing on the
    screen (the first-run screen is the same button with the mascot's greeting). This
    costs one click compared with Docker Desktop. It also never boots a VM the user did
    not ask for, for example when they open the GUI only to read settings or logs. A
    setting, "Start the engine when hyperbutler opens" (off by default), gives Docker
    Desktop's behaviour to those who want it.
  - **Closing the GUI never stops the engine.** The engine is a separate process with its
    own lifetime: `hyperbutler stop`, the GUI's **Stop** button, sign-out or shutdown end
    it. The GUI process itself exits within 2 s of its window closing (REQ-UX-011), so
    REQ-UX-002's "no GUI process with the window closed" holds.
  - When the GUI's **Stop** is pressed with containers running, it shows the S2.7 list
    first.
  - The GUI says, once, the first time its window closes while the engine runs: "The
    engine keeps running in the background. Stop it here or with `hyperbutler stop`."
- **Gap:** small (a design decision for the GUI spike). REQ-UX-011, REQ-UX-016 (new).

## 1. The engine is not running

### S1.1 `docker ps`, `docker run`, `docker compose up -d` with the engine stopped

- **What the user does:** `hyperbutler stop` earlier (or a reboot), the `hyperbutler`
  context still selected, then `docker ps` in any terminal.
- **Today** *(verified on VM, docker CLI 29.9.2)*: `hyperbutler stop` leaves the context
  selected (`cmd/hyperbutler/commands_windows.go:551-585` prints only "hyperbutler
  stopped: shut down in N s."). Then every docker command fails with exit code 1:

  ```
  failed to connect to the docker API at npipe:////./pipe/docker_engine_hyperbutler; check if the path is correct and if the daemon is running: open //./pipe/docker_engine_hyperbutler: The system cannot find the file specified.
  ```

  `docker run` and `docker compose` print the same. The error names the pipe, not the
  context, and the docker CLI shows a context's description only in `docker context ls`
  ("hyperbutler (managed by hyperbutler.exe)", `commands_windows.go:514`).
  `hyperbutler status` exits 3 and says "stopped".
- **Can hyperbutler print "run `hyperbutler start`" there?** Not without a resident
  process. With nothing listening on the pipe, the error comes from the CLI's
  `CreateFile` and never reaches hyperbutler. The options and their costs:
  - *A pipe listener that stays when the engine stops* and answers every request with an
    error saying "run `hyperbutler start`". It is a resident process (a few MB). It is
    the same process as S1.2's standby, so it only makes sense as part of that.
  - *The context description.* It is free, but it is only shown in `docker context ls`.
    It should still say what to do: `hyperbutler (run "hyperbutler start" if docker
    cannot connect)`.
  - *docker CLI plugin hooks* (docker CLI 26 and later run registered plugin hooks after
    some commands; Docker Desktop uses them for its "What's next" hints). Whether a hook
    runs after a failed connection is *unknown*. Using one would mean registering a
    plugin hook in the user's `config.json`, so it would have to be opt-in. Investigate
    before deciding.
  - *Say it where the user is when the engine stops*: `hyperbutler stop` prints
    `docker commands fail until "hyperbutler start". To use another engine: docker
    context use NAME` (with `desktop-linux` named when that context exists).
- **Docker Desktop** *(well-known)*: the same CLI error, with
  `npipe:////./pipe/dockerDesktopLinuxEngine` and, with the `default` context,
  `npipe:////./pipe/docker_engine`. Docker Desktop prints no hint either. In practice the
  user meets it less often, because Docker Desktop starts at sign-in by default.
- **Intended:** for 0.1, the `stop` message, the context description, and a
  troubleshooting entry in the README that quotes the error above. For 0.2, the opt-in
  autostart (S2.3). Later, the standby engine (S1.2), whose pipe can answer with a real
  hint.
- **Gap:** small. REQ-UX-014 (new).

### S1.2 Starting on demand

- **What the user wants:** no `hyperbutler start`; the first docker command starts the
  engine.
- **What Windows offers:** nothing for named pipes. There is no socket activation (systemd
  style) and no on-demand service start for a pipe. Something has to own the pipe before
  the client connects. A Windows service started on demand by a trigger only covers
  device, network, group-policy and ETW events, not a pipe connect. So "on demand" means a
  small process that stays.
- **Docker Desktop** *(well-known)*: no on-demand start. Its Resource Saver stops the VM
  when idle and starts it at the next API call, with the app process staying resident.
- **Intended (later, opt-in):** a *standby* engine. The autostart task (S2.3) starts
  `hyperbutler engine` in a mode that serves the pipe with the VM off, at about the
  engine's idle memory (18 MiB measured, REQ-MEM-001). The first API connection boots
  the VM (about 1.5 s, REQ-START-001) and holds the request until dockerd answers. It is
  the same mechanism as REQ-MEM-004's idle suspend, which already budgets "the next
  `docker` command resumes it in ≤ 3 s", so the two should be built together. It stays
  opt-in: the default remains "nothing runs until you start it".
- **Gap:** needs work, later. REQ-START-002 (new), REQ-MEM-004.

## 2. Start/stop lifecycle

### S2.1 Closing the terminal that ran `hyperbutler start`

- **Today** *(verified on VM)*: the engine runs as a detached process in its own process
  group and breaks away from the caller's job object where the job allows it
  (`commands_windows.go:359-403`). It ran `hyperbutler start` in a new PowerShell
  window, then `taskkill /T /F` on that PowerShell (which also killed its console host).
  The engine kept running and `docker ps` still worked. A job that forbids breakaway
  (some CI runners) still takes the engine with it. That is by design: those jobs exist
  to clean up.
- **Docker Desktop:** not applicable (it is a GUI app).
- **Intended:** as today. `start` could say once, on the first start, "The engine keeps
  running after this terminal closes; `hyperbutler stop` stops it." (part of
  REQ-UX-014).
- **Gap:** none.

### S2.2 Containers with a restart policy after `stop` and `start`

- **What the user does:** `docker run -d --restart always ...` and
  `--restart unless-stopped`, then `hyperbutler stop` and `hyperbutler start`.
- **Today** *(verified on VM)*: the data disk keeps the containers. After the start:
  `always` and `unless-stopped` containers were up again; a container without a policy
  stayed exited (137, killed at dockerd's 10 s `--shutdown-timeout`,
  `guest/prototype/init:358`); an `unless-stopped` container stopped by hand before stayed
  stopped. That is dockerd's normal behaviour, as on Linux. The same held after the
  engine process was killed (S11.2).
- **Docker Desktop** *(docs, well-known)*: the same, because it is the same dockerd
  behaviour when Desktop starts its engine.
- **Gap:** none.

### S2.3 Windows sign-in: start automatically?

- **Today** *(from code)*: nothing starts at sign-in. `autostart` is a reserved setting
  name (`internal/config/config.go:121`). `uninstall` says hyperbutler creates no
  scheduled tasks.
- **Docker Desktop** *(docs)*: "Start Docker Desktop when you sign in" is a setting, on
  after a default install (*well-known*; check against the current release).
- **Intended (0.2):** opt-in, off by default: `hyperbutler config set autostart true`.
  - It creates one per-user Task Scheduler logon task that runs `hyperbutler start` for
    this user. Per-user tasks need no admin. A Run key would work too, but a task can be
    delayed (start 30 s after sign-in, so it does not compete with sign-in) and is easier
    to list.
  - `config set` says exactly what it created (the task's name and path), and
    `config set autostart false` removes it. `uninstall --dry-run` lists it and
    `uninstall` removes it (REQ-UX-010 already names tasks).
  - **No console flash.** `hyperbutler.exe` is a console program, so a task that runs it
    opens a window for a moment. The task should run a windowless entry point instead
    (a GUI-subsystem build of the same code, or `hyperbutler.exe` started hidden by a
    small GUI-subsystem stub). This is decided when autostart is built.
  - **Why not on by default:** it boots a VM with 4 GiB of guest memory (default) at
    every sign-in, which the user may not need that day. Docker Desktop's default is one
    of the complaints in A5. `hyperbutler start` mentions the setting once, on the first
    start, so it can be found.
  - A failed autostart writes to `engine.log` and `state.json` as any start does;
    `hyperbutler status` shows it. No popup.
- **Gap:** needs work. REQ-UX-015 (new).

### S2.4 Sign-out, shutdown or reboot with containers running

- **Today** *(from code)*: the engine has no console, so it never gets console logoff or
  shutdown events. It creates a hidden top-level window instead
  (`internal/engine/sessionend_windows.go:96-125`, wired in
  `internal/engine/serve_windows.go:128-144`). On `WM_QUERYENDSESSION` it shows the
  shutdown block reason "Stopping the hyperbutler VM" and starts the normal stop. On
  `WM_ENDSESSION` it waits for the stop, for up to 20 s (`endSessionWait`, line 50). It
  also asks to be told before ordinary applications (`SetProcessShutdownParameters
  0x3FF`). The guest's stop stops dockerd, which gives containers 10 s after SIGTERM,
  then unmounts the data disk. Containers that ignore SIGTERM (a shell's `sleep`, many
  PID 1s without `--init`) make the stop take about 12 s (*verified on VM*: `stop` took
  11.8 s with such containers against a median of 1.25 s without). That fits within the
  20 s. Whether Windows waits that long, or the user clicks "Shut down anyway" on the
  blocking screen, is *unknown*. A forced end kills the VM uncleanly; the data disk's
  ext4 journal recovers it (REQ-DISK-003). The `--restart` containers come back at the
  next `hyperbutler start` (S2.2), not at sign-in (S2.3).
- **Docker Desktop** *(well-known)*: WSL shuts down with Windows; the app crashing at
  shutdown was a long-standing report (A16, for-win#13697).
- **Intended:** as today, plus a manual bench that signs out and reboots with
  containers running, 10 times each, and checks for a clean guest shutdown in the
  console log (REQ-PWR-003, new).
- **Gap:** small (unmeasured).

### S2.5 Fast startup

- **Today** *(from code)*: with fast startup on (the Windows default), "Shut down" signs
  the user out and hibernates only the kernel session. For hyperbutler that is a
  sign-out: the engine stops the VM as in S2.4. Nothing of the VM is in the hibernation
  file, and the next boot starts with the engine stopped. "Restart" is a real reboot, as
  in S2.4.
- **Docker Desktop:** the same for WSL (*well-known*).
- **Gap:** small (part of the S2.4 bench).

### S2.6 Sleep, hibernate and resume

- **Today** *(from code)*: the engine does nothing special. It does not handle
  `WM_POWERBROADCAST`, and there is no `PBT_APMSUSPEND` handler. During sleep the VM is
  frozen with the host. The guest clock comes from Hyper-V time sync (`hv_utils`,
  requirements.md, REQ-UX-005 status). The free-space guard counts its own ticks rather
  than wall-clock time, so a sleep does not trip it
  ([disk-reclaim.md](disk-reclaim.md), review finding 5). Whether HCS keeps a running VM
  across *hibernate* and whether hvsock connections (the Docker API relay, the guest
  network, the control channel) survive a long sleep is *unknown*;
  [guest-networking.md](guest-networking.md) lists it as open.
- **Docker Desktop** *(well-known)*: clock drift after sleep (S1 in the pain points),
  hangs and CPU spikes after resume (S2), Resource Saver problems (A16).
- **Intended:** after resume, `docker ps` answers within 3 s, the guest clock is within
  1 s, and running containers keep running (REQ-PWR-001/002). Hibernate behaves like
  sleep, or, if HCS cannot keep the VM, the engine stops it in order on
  `PBT_APMSUSPEND` and `status` says why it stopped. It never restarts it silently on
  resume. The `bench/resume` manual run decides which.
- **Gap:** unknown until `bench/resume` runs. REQ-PWR-001, REQ-PWR-002, REQ-PWR-003 (new).

### S2.7 `hyperbutler stop` with containers running

- **Today** *(verified on VM)*: `stop` stops the VM without a word about the containers.
  Those with `always` or `unless-stopped` come back at the next start; the rest stay
  exited.
- **Docker Desktop** *(well-known)*: quitting the app stops containers the same way,
  without a list.
- **Intended:** `stop` (and `restart`, and every `--apply` that restarts dockerd or the
  VM) prints the running containers it is about to stop, and which of them will come
  back: "Stopping 3 running containers (2 come back at the next start: web, db; 1 does
  not: job-17)". It does not ask, because `stop` is already the explicit request. The
  GUI's Stop shows the same list and asks.
- **Gap:** small. REQ-UX-017 (new).

## 3. Docker Desktop installed alongside

Facts from the code that the scenarios below rely on
([compat.md](../compat.md#the-docker-cli-and-docker-desktop)):

- The two engines never share a pipe. hyperbutler serves `\\.\pipe\docker_engine_hyperbutler`
  (`internal/transport/pipename.go:11`). Docker Desktop serves `\\.\pipe\docker_engine`
  (context `default`) and `\\.\pipe\dockerDesktopLinuxEngine` (context `desktop-linux`).
- `hyperbutler start` creates or refreshes only its own context, selects it, and prints the
  one it replaced (`commands_windows.go:505-547`): `Docker context "hyperbutler" selected
  (was "desktop-linux").` It notes `DOCKER_HOST` or `DOCKER_CONTEXT` when they are set in
  the shell that runs it (lines 541-545).
- hyperbutler's guest network is a user-mode stack over hvsock (ADR 0006). It creates no
  Hyper-V switch, HNS network, NAT or firewall rule, so it does not touch what WSL and
  Docker Desktop configure.

### S3.1 Docker Desktop installed, one or both stopped

| Desktop | hyperbutler | Selected context | `docker ps` |
|---|---|---|---|
| stopped | stopped | either | the S1.1 error for that context's pipe |
| stopped | running | `hyperbutler` (after `start`) | hyperbutler's containers |
| running | stopped | `hyperbutler` (left by `stop`) | **the S1.1 error**, although Desktop runs |
| running | stopped | `desktop-linux` | Desktop's containers |

- The third row is the surprising one: the user quits nothing, starts Docker Desktop, and
  docker fails because hyperbutler's context is still selected.
- **Docker Desktop** *(unknown)*: whether it selects `desktop-linux` again at its start
  (S3.3) decides whether the third row happens.
- **Intended:** `hyperbutler stop` names the way back (S1.1's message). `uninstall`
  already selects the context `start` replaced (compat.md). Changing the selection on
  `stop` would be a silent change of the user's setup, and the next `start` would have
  to switch again, so `stop` does not do it.
- **Gap:** small. REQ-UX-014 (new).

### S3.2 Both running: which engine does `docker` reach?

- **Today** *(from code)*: the docker CLI chooses the engine, in this order: `--context`
  or `-H` on the command line, then `DOCKER_HOST`, then `DOCKER_CONTEXT`, then
  `currentContext` in `config.json`. hyperbutler writes only the last, at `start`.
  `hyperbutler status` shows whether its context is selected ("context: hyperbutler (not
  selected)") but not which one is.
- **How the user switches:**
  - for good: `docker context use desktop-linux` or `docker context use hyperbutler` (or
    `hyperbutler start` again);
  - for one shell: `$env:DOCKER_CONTEXT = "desktop-linux"` (PowerShell),
    `set DOCKER_CONTEXT=desktop-linux` (cmd), `export DOCKER_CONTEXT=desktop-linux`
    (Git Bash);
  - for one command: `docker --context desktop-linux ps`.
- **Docker Desktop** *(docs)*: the same docker CLI rules.
- **Intended:** `hyperbutler start` also says when another Docker engine is running, by
  looking for the pipes `docker_engine` and `dockerDesktopLinuxEngine` (it only checks
  that the pipe exists, never connects): "Docker Desktop is running too; plain `docker`
  now uses hyperbutler. `docker context use desktop-linux` switches back." `status`
  names the selected context when it is not `hyperbutler`. Neither changes anything.
- **Gap:** small. REQ-UX-018 (new), REQ-PLAT-004.

### S3.3 Docker Desktop selects `desktop-linux` again when it starts

- **Today:** *unknown* whether current Docker Desktop releases switch the context at
  start. Reports describe Desktop resetting it to `desktop-linux` at launch in some
  versions; not checked here.
- **What happens if it does** *(from code)*: plain `docker` reaches Docker Desktop.
  hyperbutler keeps running and serving its pipe. `hyperbutler status` shows "context:
  hyperbutler (not selected)". The user sees Desktop's containers and may think theirs are
  gone.
- **Intended:** hyperbutler does not fight over the selection: it never re-selects in the
  background, only at `start`. `status` makes it visible (S3.2), and the docs say how to
  pin a shell with `DOCKER_CONTEXT`. `bench/coexist` (REQ-PLAT-004) records what each
  Desktop release does.
- **Gap:** small. REQ-UX-018 (new).

### S3.4 Both publish the same port

- **What the user does:** `docker run -p 8080:80` on Docker Desktop and on hyperbutler, or
  a host program already listening on 8080.
- **Today** *(verified on VM, with a host program in place of Docker Desktop)*: before
  mirroring a port, the engine probes it by binding `0.0.0.0:P`, `[::]:P` and the
  dual-stack `:P` (`internal/engine/net_windows.go:473-489`). If one fails, it logs "refusing
  a port a host program already uses" and skips the port (lines 427-430). Otherwise it
  binds `127.0.0.1:P` and `[::1]:P` and logs any failure (lines 433-438). In every case
  `docker run` has already succeeded in the guest. The test:
  - a host program listening on `127.0.0.1:18080` only, then
    `docker run -d -p 18080:80 busybox httpd`;
  - the probe **passed**: Windows lets another socket bind the wildcard address while a
    specific address holds the port;
  - `127.0.0.1:18080` failed to bind (engine log only), and `[::1]:18080` succeeded;
  - `docker run` exited 0, and `docker ps` showed `0.0.0.0:18080->80/tcp`;
  - `curl http://127.0.0.1:18080/` reached **the host program**, while
    `curl http://localhost:18080/` and `http://[::1]:18080/` reached **the container**.

  So the user gets no error and two different services on one port, depending on how
  the client resolves `localhost`.

  With Docker Desktop *(from code; Desktop not run)*: Docker Desktop publishes on
  `0.0.0.0:P` by default.
  - **Desktop first:** hyperbutler's probe fails and the port is skipped. hyperbutler's
    container runs, but every host client reaches Desktop's container. Only
    `engine.log` says why.
  - **hyperbutler first:** hyperbutler holds `127.0.0.1:P` and `[::1]:P`. Desktop's
    `0.0.0.0:P` bind succeeds for the reason the probe passed above. Loopback clients
    reach hyperbutler's container, and clients on the LAN reach Desktop's.
- **Docker Desktop** *(well-known)*: `docker run` fails before the container starts, with
  `Ports are not available: exposing port TCP 0.0.0.0:8080 -> 0.0.0.0:0: listen tcp
  0.0.0.0:8080: bind: Only one usage of each socket address ... is normally permitted.`
- **Intended:**
  1. **All or nothing per port.** A port is published on both `127.0.0.1` and `[::1]` or
     on neither. If IPv6 loopback is disabled on the host, IPv4 alone is fine. The probe
     also tries `127.0.0.1:P` and `[::1]:P`, so a specific-address listener counts as
     taken.
  2. **The user sees it.** When a port cannot be published, `docker run` fails, as on
     Docker Desktop. The API proxy already rewrites container creates
     (`internal/transport/rewrite.go`), so it can also check a
     `POST /containers/{id}/start`. It reads the container's port bindings, probes
     them, and answers `500` with Docker's own wording ("Ports are not available:
     exposing port TCP 127.0.0.1:8080: ... in use by another program") before dockerd
     starts the container. A race between that check and the bind is still possible.
     In that case, and for containers started by a restart policy, `hyperbutler status`
     lists the ports that could not be published, with the reason.
  3. The docs say that two engines cannot both publish one port, and how to run both
     (different host ports).
- **Gap:** **needs work**. It is a correctness problem today, not only a message.
  REQ-NET-005 (new), REQ-NET-001.

### S3.5 `host.docker.internal`

- **Today** *(from code; REQ-NET-004 met)*: it reaches the Windows host's `127.0.0.1` (or
  `[::1]`) on the same port and nothing else
  ([compat.md](../compat.md#hostdockerinternal)). hyperbutler never edits the Windows
  `hosts` file.
- **Docker Desktop** *(docs, well-known)*: it reaches the host. Desktop also writes
  `host.docker.internal` and `gateway.docker.internal` into the Windows `hosts` file with
  the host's LAN address, so host programs can resolve the name too.
- **Differences a user can hit:**
  - a host service bound only to the LAN address (not loopback, not `0.0.0.0`) is
    reachable from Desktop's containers but not from hyperbutler's;
  - a configuration shared between host and container that uses `host.docker.internal`
    resolves on the host only while Desktop's `hosts` entry exists. After Desktop is
    uninstalled, it no longer resolves on the host;
  - with both installed, Desktop's `hosts` entry does not affect hyperbutler's containers
    (the name is answered by hyperbutler's DNS).
- **Intended:** as today; editing `hosts` would change the machine's setup and needs
  admin. compat.md gains the second point.
- **Gap:** small (documentation).

### S3.6 Compose plugin and credential helper

- **Compose** *(from code)*: Docker Desktop's compose in `%ProgramFiles%\Docker\cli-plugins`
  wins, and the bundled one is registered in `cliPluginsExtraDirs` only while no other
  is found (`internal/engine/dockerctx/plugins.go:103`). This is re-checked at every
  `hyperbutler start`. After Docker Desktop is uninstalled, `docker compose` is an
  unknown command until the next `start` registers the bundled one. *Gap: small*
  (`start` is the natural next step anyway).
- **Credential helper** *(from code)*: hyperbutler never reads or writes `credsStore` or
  `credHelpers` (`internal/engine/dockerctx` keeps every key but `currentContext` and
  `cliPluginsExtraDirs`). Docker Desktop writes `"credsStore": "desktop"`.
  - **Desktop installed but not running** *(unknown)*: `docker-credential-desktop.exe` is
    on PATH from Desktop's install and stores credentials in the Windows Credential
    Manager. It is believed to work without the app running, but this was not checked.
  - **Desktop uninstalled** *(well-known, A9)*: the entry stays and the helper is gone,
    so `docker login` and `docker pull` fail with
    `error getting credentials - err: exec: "docker-credential-desktop": executable file not found in %PATH%`.
    This happens even for public images, because the CLI asks the helper for every
    registry.
- **Intended:** hyperbutler never edits `credsStore` (settings-and-gui.md, A9).
  `hyperbutler start` and `doctor` check whether the helpers that `credsStore` and
  `credHelpers` name are on PATH. If one is missing, they say: "config.json's credsStore
  is "desktop", but docker-credential-desktop.exe is not on PATH, so docker login and
  pull will fail. Set it to "wincred" (docker-credential-wincred, shipped with the docker
  CLI releases) or remove the line." The user makes the change. Whether to ship
  `docker-credential-wincred.exe` in `clients\` is a separate decision.
- **Gap:** small. REQ-UX-019 (new).

### S3.7 Moving images and volumes from Docker Desktop

- **Today:** the two engines' data are separate disks. Nothing is shared or migrated.
- **Docker Desktop:** has no import from other engines (*well-known*).
- **Intended (documented path, no command for 0.1):** with both running, stream between
  contexts. In **PowerShell 5.1 the `|` pipe re-encodes binary output as text and
  corrupts the stream**, so use `cmd /c` or a file:

  ```powershell
  # images
  docker --context desktop-linux save -o "$env:TEMP\img.tar" myapp:1 postgres:16
  docker --context hyperbutler load -i "$env:TEMP\img.tar"

  # a named volume (cmd.exe passes the bytes unchanged)
  cmd /c "docker --context desktop-linux run --rm -v pgdata:/v busybox tar -C /v -cf - . | docker --context hyperbutler run --rm -i -v pgdata:/v busybox tar -C /v -xf -"
  ```

  Containers are recreated from their compose files, not moved. A later
  `hyperbutler import --from desktop-linux [--images] [--volumes NAME...]` could do the
  same with progress; it would talk only to the two pipes.
- **Gap:** small (documentation). REQ-UX-021 (new).

### S3.8 WSL 2–based Docker Desktop and hyperbutler's VM together

- **Today** *(from code)*: both are Hyper-V partitions under the same hypervisor, created
  through HCS. hyperbutler's VM name has its own prefix, and orphan cleanup only touches
  that prefix (`serve_windows.go:299-328`). It uses no Hyper-V switch or HNS network, so
  it cannot conflict with WSL's NAT or mirrored networking. Memory is the real contention:
  WSL's VM (up to 50% of host RAM by default) and hyperbutler's (4 GiB by default) add
  up. Not run: there is no WSL on the test machine.
- **Docker Desktop** *(well-known)*: coexists with other Hyper-V VMs; the VirtualBox and
  VMware conflicts (P5) are about the hypervisor itself, which both products need.
- **Intended:** REQ-PLAT-004's `bench/coexist` covers it: both running, each engine's
  `bench/compat` passing, and a `wsl --shutdown` not affecting hyperbutler.
- **Gap:** unknown until the bench runs. REQ-PLAT-004.

## 4. Terminals and tools

What each kind of client needs to reach hyperbutler:

| Client | Reads the docker context? | Reads `DOCKER_HOST`? | Default without either | Works today |
|---|---|---|---|---|
| docker CLI (any shell) | yes | yes (wins) | `npipe:////./pipe/docker_engine` | yes *(verified)* |
| docker compose plugin | yes (through the CLI) | yes | as the CLI | from code; compose untested |
| VS Code Docker / Container Tools extension | yes: it runs the docker CLI *(docs)* | yes | as the CLI | unknown, likely |
| VS Code Dev Containers | yes: it runs the docker CLI *(docs)* | yes | as the CLI | unknown, likely |
| Testcontainers for Go | yes, the current context, in recent versions *(docs)* | yes | `docker_engine` | unknown |
| Testcontainers for Java, .NET, Node | not known to *(docs, unknown)* | yes; also `docker.host` in `~/.testcontainers.properties` | `docker_engine` | needs `DOCKER_HOST` |
| Docker SDK for Python (`docker.from_env()`) | no *(well-known)* | yes | `npipe:////./pipe/docker_engine` | needs `DOCKER_HOST` |
| Docker SDK for Go (`client.FromEnv`) | no *(well-known)* | yes | `npipe:////./pipe/docker_engine` | needs `DOCKER_HOST` |
| JetBrains IDEs (Docker plugin) | unknown | unknown | "Docker for Windows" = `docker_engine` | unknown; a custom engine URL can be entered |

`DOCKER_HOST` for hyperbutler is `npipe:////./pipe/docker_engine_hyperbutler` (four slashes,
then forward slashes). `hyperbutler status` prints it on the "docker:" line.

### S4.1 PowerShell, cmd, Git Bash

- **Today** *(verified for PowerShell; from code for the others)*: the context is in
  `config.json`, so all three work without any variable. Git Bash quirks that are not
  hyperbutler's (*well-known*): MSYS rewrites arguments that look like paths, so
  `-v /c/src:/app` or `-v "$(pwd)":/app` can arrive mangled. Use `MSYS_NO_PATHCONV=1`,
  or a leading `//`, or a Windows path (`-v "C:\src:/app"`). Interactive `docker run -it`
  needs a ConPTY-based mintty (Git for Windows 2.38 and later) or `winpty`.
  `hyperbutler uninstall` and `delete` cannot read a confirmation there and ask for
  `--yes` (README).
- **Intended:** a "Shells" note in the docs with the Git Bash points.
- **Gap:** small (documentation). REQ-API-001.

### S4.2 VS Code Docker extension and Dev Containers

- **Today:** *unknown*, not run. Both run the docker CLI, so they should follow the
  selected context. Dev Containers bind-mounts the workspace from a Windows path, which
  goes through hyperbutler's share (ADR 0007). Its "Clone in volume" mode avoids that.
- **Intended:** REQ-API-002's `bench/compat-tools` runs a Dev Containers sample.
- **Gap:** unknown, likely none. REQ-API-002.

### S4.3 Testcontainers and the Docker SDKs

- **Today** *(from docs)*: SDKs and most Testcontainers ports ignore the context and fall
  back to `\\.\pipe\docker_engine`. With Docker Desktop running, **they silently use
  Docker Desktop**. Without it, they fail with "file not found" on that pipe.
  Testcontainers also starts its Ryuk reaper container, which mounts the Docker socket
  (inside the guest, dockerd's own `/var/run/docker.sock`; *unknown* whether it works)
  and connects to a published port on `localhost` (works per REQ-NET-001).
- **Intended:**
  - The docs give per-tool settings: `$env:DOCKER_HOST =
    "npipe:////./pipe/docker_engine_hyperbutler"` (or set it as a user variable for
    IDEs), and `docker.host=npipe:////./pipe/docker_engine_hyperbutler` in
    `~/.testcontainers.properties`.
  - hyperbutler never sets `DOCKER_HOST` itself: it is user setup, and a global
    `DOCKER_HOST` would break Docker Desktop's context switching.
  - `bench/compat-tools` covers Testcontainers Java, Go and Node, as REQ-API-002 says.
- **Gap:** needs work (documentation and the bench). REQ-API-002, REQ-API-003 (new).

### S4.4 JetBrains IDEs

- **Today:** *unknown*. The Docker plugin's "Docker for Windows" option means
  `\\.\pipe\docker_engine`. Whether it offers the docker contexts, or accepts an
  `npipe://` URL in its TCP field, was not checked.
- **Intended:** checked in `bench/compat-tools`; the docs then give the steps.
- **Gap:** unknown. REQ-API-002.

### S4.5 Tools that only know `\\.\pipe\docker_engine`

- **Should hyperbutler serve on `docker_engine`?**
  - **Today** *(from code)*: possible but not documented. `hyperbutler config set pipe
    \\.\pipe\docker_engine` is accepted (`ValidatePipeName` allows any local pipe name)
    and applies at the next start. If Docker Desktop holds the name, `start` fails with
    `pipe-in-use`. The docker CLI's `default` context then reaches hyperbutler too.
  - **Decision:** keep the distinct default name. It is what makes side-by-side
    installation safe. Document the setting as the opt-in for tools that hard-code the
    default pipe, for machines without Docker Desktop. `config set pipe
    \\.\pipe\docker_engine` warns when Docker Desktop is installed (its
    `Docker Desktop.exe` under `%ProgramFiles%\Docker\Docker`), because whichever starts
    second then fails. It is never switched automatically, not even when Desktop is
    absent: a later Desktop install would then fail in a confusing way.
- **Gap:** small. REQ-API-003 (new).

## 5. Users and elevation

### S5.1 A second Windows user on the same PC

- **Today:**
  - *(from code)* each user has their own installation: `%LOCALAPPDATA%\hyperbutler`,
    data disk, settings and `~/.docker` context.
  - Named pipe names are **machine-wide**, and every installation defaults to
    `\\.\pipe\docker_engine_hyperbutler`. While user A's engine runs, user B's start
    fails.
  - The pipe's DACL grants only its owner (`internal/transport/pipe_windows.go:26-39`).
    So user B's docker CLI gets "Access is denied" on A's pipe, and never reaches A's
    engine. That part is right.
  - *(verified on VM, with a second installation of the same user, which fails the same
    way)* the second `start` failed with `transport: listen on \\.\pipe\...: Access is
    denied.` The explanation printed under it names the default pipe even though a
    different one was configured: `Describe(CodePipeInUse)` hard-codes it
    (`internal/engine/serve_windows.go:348-349`). That is a small bug.
  - Published ports are on the machine-wide loopback: user B's processes can reach user
    A's published ports, as with any loopback server, and the two users' engines collide
    on ports as in S3.4.
- **Docker Desktop** *(docs, well-known)*: one engine per machine, shared by members of
  the `docker-users` group; the user who is not running the app reaches the other's
  engine.
- **Intended:**
  - the pipe-in-use explanation names the configured pipe, says that another user's or
    another installation's engine may hold it, and gives the fix:
    `hyperbutler config set pipe \\.\pipe\docker_engine_hyperbutler_<user>`;
  - the README states that each Windows user has their own engine and data, and that
    published ports are shared;
  - later, consider making the default pipe name per user. It costs nothing, because
    clients use the context, which is per user too.
- **Gap:** **needs work** (a bug and a missing message). REQ-UX-020 (new).

### S5.2 An elevated terminal of the same user

- **Today** *(from code; not run, because elevating needs UAC)*: the elevated token has
  the same user SID. It finds the same `%LOCALAPPDATA%\hyperbutler` and `~/.docker`, and
  the same engine lock, so `status`, `stop` and `docker` work there. The pipe's DACL
  grants that SID and owns the pipe as the user even when created elevated
  (`pipe_windows.go:36-38`). A pipe created by a non-elevated engine carries no mandatory
  label, so it counts as Medium, which a High-integrity client may open. The reverse, a
  normal shell reaching an engine *started* from an elevated shell, depends on the label
  Windows gives a pipe created at High integrity. That is *unknown*. An elevated engine
  refuses `--debug-pipe` (`commands_windows.go:980-982`).
- **Docker Desktop:** its pipes allow the `docker-users` group; elevation does not matter
  (*docs*).
- **Intended:** both directions work. Check the second case once, and if a High label
  blocks normal shells, `start` from an elevated shell says so (or lowers the label).
- **Gap:** small (unverified).

### S5.3 An elevated terminal of a different (admin) account

- **Today** *(from code)*: on Home with a standard user, UAC "Run as administrator"
  often runs as another account (the administrator's). That shell has a different
  profile: no `hyperbutler` context, a different `%LOCALAPPDATA%`, and an `Access is
  denied` error on the user's pipe. `hyperbutler status` there shows "not set up yet".
- **Intended:** the docs say: use hyperbutler from your own, non-elevated terminal;
  nothing in it needs elevation (REQ-PLAT-003, A10). `status` could add "(this shell runs
  as ADMIN-ACCOUNT, not as the user whose engine holds the pipe)" when the pipe exists but
  is not this user's.
- **Gap:** small. REQ-UX-020 (new).

## 6. Settings changes while containers run

### S6.1 `config set ... --apply` that restarts dockerd

- **What the user does:** `hyperbutler config set proxy.https http://proxy:3128 --apply`,
  or `hyperbutler config daemon apply`, with containers running.
- **Today** *(verified on VM)*: `config set` without `--apply` restarts nothing and says
  so (`cmd/hyperbutler/config_windows.go:263-270`). With `--apply` it prints
  "Restarting dockerd (--apply; the VM keeps running)." and restarts dockerd. `live-restore`
  is a key hyperbutler owns and refuses (`internal/daemoncfg/daemoncfg.go:60`), so dockerd
  stops every container. In the run, containers with a restart policy were up again after
  the 12.3 s restart. One without a policy was left exited (137).
  **Nothing said beforehand that running containers would stop.**
- **Docker Desktop** *(well-known)*: "Apply & restart" restarts the whole engine, with the
  same effect and no list.
- **Intended:** before restarting, the S2.7 list ("dockerd restarts: 2 running containers
  stop; 1 comes back"). `--apply` stays the consent; there is no extra prompt. The docs
  explain why `live-restore` is owned (the guest must unmount the data disk cleanly).
- **Gap:** small. REQ-UX-017 (new), REQ-UX-006.

### S6.2 `config set memory ... --apply` (VM restart)

- **Today** *(from code)*: without `--apply`: "The VM must restart to apply it: run
  `hyperbutler restart`, or repeat this with --apply. Nothing was restarted."
  (`config_windows.go:280-282`). With it: "Restarting the VM (--apply)." and a stop and
  start (S2.2 applies to the containers).
- **Intended:** the same list as S6.1, with "the VM restarts".
- **Gap:** small. REQ-UX-017 (new).

### S6.3 Live settings

- **Today** *(from code; REQ-NET-004 met)*: `hostGateway.enabled` and `hostGateway.ports`
  apply to the running engine without a restart (`config_windows.go:254-258`).
- **Gap:** none.

## 7. Disk and memory

### S7.1 The data disk is full

- **What the user does:** pulls and builds until the guest file system (64 GiB by default,
  sparse) is full.
- **Today** *(from code)*: dockerd and containers get `no space left on device`.
  `hyperbutler status` shows the disk's allocated size on the host, its maximum and the
  host volume's free space (`commands_windows.go:713-714`), but not how full the guest
  file system is. `disk.size` applies only at the first start; there is no grow command.
  The way out is `docker system prune` (space comes back to the host, REQ-DISK-001) or
  `hyperbutler delete`, which loses everything.
- **Docker Desktop** *(docs)*: a "Virtual disk limit" slider that can grow the disk, and a
  disk-usage view.
- **Intended:** `status` shows guest file system use ("data: 61.2 of 64 GiB used in the
  VM"); `start` warns above 90%. Then a `hyperbutler config set disk.size` that can grow
  (never shrink) the disk with the engine stopped, without data loss.
- **Gap:** needs work. REQ-DISK-009 (new), REQ-DISK-005.

### S7.2 The host volume is nearly full

- **Today** *(from code; spike-verified for REQ-DISK-004)*: the free-space guard gives the
  guest a write budget from the host's free space. The guest gets `ENOSPC` before the
  host fills. Below a 24 MiB floor the host pauses the VM until space is freed
  (`internal/engine/guard_windows.go`). While paused, docker commands hang; nothing
  records the pause in `state.json`, so `status` does not say why.
- **Docker Desktop** *(well-known)*: D5: no protection; a full host disk can corrupt the
  VHDX.
- **Intended:** as today, plus `state.json` and `status` show "paused: the host volume C:
  has 20 MiB free; free space to resume", and the engine log says the same once.
- **Gap:** small. REQ-DISK-004, REQ-DISK-010 (new).

### S7.3 A container uses more memory than the VM has

- **Today** *(from code)*: the guest's OOM killer ends the process inside the VM; the VM
  and other containers keep running. `memory` is a real limit (VM restart to change,
  S6.2).
- **Docker Desktop** *(well-known)*: the same inside the WSL VM.
- **Gap:** none in behaviour; REQ-MEM-003's bench is not run.

## 8. Networking

### S8.1 VPN connect or disconnect while running

- **Today** *(from code; REQ-NET-002 met for Tailscale)*: containers' traffic leaves
  through the engine's Windows sockets, so it follows the host's routes at that moment.
  DNS questions become typed lookups through the Windows resolver (NRPT, split DNS, hosts
  file), with a 10 s TTL on answers (`internal/netstack/dns.go:14-31`). The list of host
  addresses that guest traffic may not reach is refreshed every second
  (`internal/netstack/policy.go:38-40`). Connections open at the moment of the change break
  as they do for host programs. Nothing needs a restart.
- **Docker Desktop** *(well-known)*: N2: VPNs and corporate DNS often break WSL
  networking.
- **Intended:** as today; more VPN clients in `bench/net-vpn`.
- **Gap:** none (one client tested).

### S8.2 Wi-Fi change, airplane mode

- **Today** *(from code)*: same mechanism as S8.1. In airplane mode, pulls and container
  traffic fail with the host's errors and DNS fails. They work again when the network
  returns, with no restart. Published ports and `host.docker.internal` are loopback and
  keep working offline.
- **Gap:** none (not run).

### S8.3 Corporate proxy

- **Today** *(REQ-UX-009 met on one machine)*: Windows root CAs are imported for dockerd;
  `proxy.http`/`https`/`noProxy` are manual settings (dockerd restart, S6.1). The Windows
  system proxy and PAC (`proxy.mode`) come in 0.2. Containers and builds get neither,
  as with Docker Desktop.
- **Gap:** small (system proxy in 0.2). REQ-UX-009.

## 9. Upgrades

### S9.1 Unzipping a new version over the running one

- **Today** *(from code and Windows behaviour; not run)*: the running engine is
  `hyperbutler.exe` from that folder, and the VM holds the guest image files open.
  Copying over them fails for the locked files ("being used by another process"), and
  the others are replaced. The result is a mixed folder. At the next start, a release
  build refuses guest image files that do not match the `image.json` hash it was built
  with, and says to extract the zip again (README). The README's upgrade steps start with
  `hyperbutler stop`.
- **Docker Desktop:** the app updates itself (*well-known*, A2).
- **Intended:** as today. `hyperbutler version` shows the folder and version. A later
  `hyperbutler upgrade --from ZIP` (or the MSI in 0.2) does stop, replace and start in
  order.
- **Gap:** small. REQ-PLAT-008 (new), REQ-PLAT-005.

### S9.2 `hyperbutler start` from a new folder while the old engine runs

- **Today** *(from code)*: `start` finds the running engine and prints "hyperbutler is
  already running (engine pid N, running)." (`commands_windows.go:203-219`). Nothing says
  that this engine is the old version from another folder. `hyperbutler restart` from the
  new folder stops the old engine and starts the new one. With a release build, it uses
  the image next to the new `hyperbutler.exe` and says so if it differs from the old
  engine's (`config_windows.go:556-570`).
- **Intended:** when the running engine's executable or image folder differs from this
  one, `start` says: "hyperbutler 0.1.0 from C:\old is running; this is 0.1.1 in C:\new.
  Run `hyperbutler restart` from here to switch." `state.json` already has `ImageDir`;
  it needs the engine's executable path and version as well.
- **Gap:** small. REQ-PLAT-008 (new).

## 10. Bind mounts

### S10.1 Paths with spaces or Korean characters

- **Today** *(verified on VM)*: `docker run -v "...\한글 폴더:/m" busybox` read a UTF-8
  file and wrote one back that Windows saw. Spaces were verified earlier
  ([bind-mounts.md](bind-mounts.md)), and the name mapping is covered by its table.
- **Gap:** none. REQ-FS-004.

### S10.2 OneDrive folders

- **Today:** *unknown*, not run. The share opens files through normal Win32 calls, so a
  OneDrive Files On-Demand placeholder is downloaded when a container reads it. That can
  be slow and fails offline. File watching sees OneDrive's own changes too.
- **Intended:** the docs advise keeping project trees outside OneDrive, as for any build
  tree. `bench/fs-compat` adds a placeholder file case.
- **Gap:** unknown. REQ-FS-004.

### S10.3 Network drives, UNC and `\\wsl$` paths

- **Today:**
  - *(verified on VM)* a source that is not a drive-letter path (`\\server\share\x`,
    `\\wsl$\...`) is not rewritten (`internal/share/bind.go:21-25`). It reaches dockerd,
    which refuses it with: `create \\server\share\x: "\\\\server\\share\\x" includes
    invalid characters for a local volume name, only "[a-zA-Z0-9][a-zA-Z0-9_.-]" are
    allowed. If you intended to pass a host directory, use absolute path`.
  - *(from code)* a mapped network drive (`Z:\...`) is a drive-letter path. It is shared
    if the engine's logon session sees the mapping. Elevated and normal sessions have
    separate drive mappings.
- **Docker Desktop** *(well-known)*: `\\wsl$` paths work there, because the files are in
  WSL. UNC paths are generally not supported.
- **Intended:** the API proxy refuses UNC and device sources with a message of its own:
  "bind source \\server\share\x is a network path; hyperbutler shares local drive paths
  only (map the share to a drive letter, or copy the files)". The docs say network drives
  are untested and slow.
- **Gap:** small. REQ-FS-006 (new).

### S10.4 File watching

- **Today** *(REQ-FS-003 met)*: a Windows edit reaches inotify in the container in p95
  3.4 ms; pinned Vite and webpack rebuild.
- **Docker Desktop** *(well-known)*: F2: often no events on WSL 2's 9P mounts.
- **Gap:** none.

## 11. Errors and recovery

### S11.1 The VM or dockerd crashes

- **Today** *(from code)*: if dockerd exits on its own, the guest init powers the VM off
  (`guest/prototype/init:410`). The engine then ends with `vm-exited`, "The VM stopped on
  its own. `hyperbutler logs --console` shows why." (`serve_windows.go:352-353`). There is
  no automatic restart (no silent restarts). `status` shows "stopped (last run failed:
  ...)", and the docker CLI shows the S1.1 error.
- **Docker Desktop** *(well-known)*: A1: "Docker Desktop is starting…" hangs, and
  restarts or resets are the usual fix.
- **Intended:** as today. Once a standby engine exists (S1.2), it could offer
  "the VM stopped unexpectedly; `hyperbutler start` starts it again" in the pipe's error.
- **Gap:** none.

### S11.2 The engine process is killed

- **Today** *(verified on VM)*: the engine was killed with `Stop-Process -Force` while
  containers ran.
  - The VM went with it: it is created with `ShouldTerminateOnLastHandleClosed`
    (`internal/vm/config.go:193`).
  - `status` said "stopped (the engine, pid N, exited without shutting down; see
    `hyperbutler logs`)" and exited 3.
  - The next `start` printed "the previous engine (pid N) exited without shutting down"
    (`commands_windows.go:339-357`), cleaned up, and booted in 1.5 s. The `--restart`
    containers came back, and the data disk was intact.
- **Gap:** none. REQ-DISK-003 covers the integrity side.

### S11.3 Something is wrong and the user wants help

- **Today** *(from code)*: `hyperbutler diagnose` writes a local zip with secrets redacted
  (REQ-UX-008, A11). `hyperbutler doctor` (REQ-UX-003) does not exist yet.
- **Intended:** `doctor` also checks the conditions found in this note:
  - the selected context, `DOCKER_HOST` and `DOCKER_CONTEXT`;
  - other Docker pipes present (S3.2);
  - a missing credential helper (S3.6);
  - the pipe held by another user (S5.1);
  - ports that could not be published (S3.4);
  - a full guest file system (S7.1);
  - a guard pause (S7.2).
- **Gap:** small. REQ-UX-003.

---

## Proposed requirements

None of these are in [requirements.md](../requirements.md). The IDs continue the existing
numbering, so the maintainer can accept, renumber or drop each one. "0.1" means before
the first preview, "0.2" the release with the GUI, "later" after that.

| Suggested ID | Requirement | Priority | Scenarios |
|---|---|---|---|
| REQ-NET-005 | A published port is mirrored on `127.0.0.1` and `[::1]` together or not at all. A port held by any host socket, wildcard or specific, is reported to the user: `docker run` fails with Docker's "Ports are not available" wording, and `status` lists ports a restart policy could not publish. | 0.1 | S3.4 |
| REQ-UX-020 | When the Docker API pipe is taken, the error names the configured pipe, says whether another user's or installation's engine may hold it, and gives the `config set pipe` fix. The README says each Windows user has their own engine and that published ports are shared. | 0.1 | S5.1, S5.2, S5.3 |
| REQ-UX-014 | A stopped engine is explained where the user is. `stop` says docker commands fail until `start` and how to switch context. The context description says to run `hyperbutler start`. The README quotes the CLI's error with the fix. | 0.1 | S1.1, S3.1 |
| REQ-UX-016 | Launched without a console of its own (Explorer, a shortcut), `hyperbutler.exe` shows the status, offers to start a stopped engine on Enter, says the engine outlives the window, and waits before closing. From 0.2 it opens the GUI. The GUI never starts the engine unless asked (a Start button, or an opt-in setting), and closing it never stops the engine. Usage without arguments exits 0. | 0.1 (console), 0.2 (GUI) | S0.1, S0.2, S0.3 |
| REQ-UX-017 | Before any stop or restart of the VM or dockerd (`stop`, `restart`, `--apply`, `config daemon apply`), hyperbutler lists the running containers that will stop and which will come back. | 0.1 | S2.7, S6.1, S6.2 |
| REQ-DISK-010 | A VM paused by the free-space guard is shown in `state.json` and `status`, with the volume and the free space needed to resume. | 0.1 | S7.2 |
| REQ-PLAT-008 | `start` from a different folder or version than the running engine says so and points to `restart`. `state.json` records the engine's executable path and version. | 0.1 | S9.1, S9.2 |
| REQ-UX-015 | An opt-in autostart (`config set autostart true`) creates one per-user logon task that starts the engine without a window and needs no admin. `config set` names the task; it is off by default; `uninstall` lists and removes it. | 0.2 | S2.3 |
| REQ-UX-018 | `start` says when another Docker engine's pipe (`docker_engine`, `dockerDesktopLinuxEngine`) exists and which engine plain `docker` now reaches. `status` names the selected context when it is not `hyperbutler`. It never changes the selection outside `start`. | 0.2 | S3.2, S3.3 |
| REQ-UX-019 | `start` and `doctor` warn when `config.json`'s `credsStore` or `credHelpers` names a helper that is not on PATH, with the fix. hyperbutler never edits those keys. | 0.2 | S3.6 |
| REQ-API-003 | Tools that ignore docker contexts are documented: `DOCKER_HOST`, `~/.testcontainers.properties`, IDE settings. `config set pipe \\.\pipe\docker_engine` is the documented opt-in for tools that hard-code the default pipe, and warns when Docker Desktop is installed. | 0.2 | S4.3, S4.5 |
| REQ-UX-021 | Moving images and volumes from Docker Desktop is documented: `save`/`load` and a volume `tar` between contexts, with the PowerShell 5.1 pipe caveat. | 0.2 | S3.7 |
| REQ-PWR-003 | Sign-out, reboot, fast-startup shutdown and hibernate with containers running end in an orderly guest shutdown, or, for hibernate, keep the VM. Either way `status` says what happened. A manual bench repeats each 10 times. | 0.2 | S2.4, S2.5, S2.6 |
| REQ-FS-006 | Bind sources hyperbutler cannot share (UNC, `\\wsl$`, device paths) are refused with a message that says why and what to do. | 0.2 | S10.3 |
| REQ-DISK-009 | `status` shows how full the guest file system is, `start` warns above 90%, and the data disk can be grown without losing data. | later (status: 0.2) | S7.1 |
| REQ-START-002 | Opt-in standby: the engine serves the pipe with the VM off and boots it on the first API connection within REQ-MEM-004's 3 s. Built with REQ-MEM-004. | later | S1.2 |

Two smaller findings to fix without a requirement:

- `Describe(CodePipeInUse)` names `\\.\pipe\docker_engine_hyperbutler` even when another
  pipe is configured (`internal/engine/serve_windows.go:348-349`, S5.1).
- The port probe can bind `0.0.0.0:P` and `[::]:P` for a moment
  (`net_windows.go:473-489`). Whether that ever raises a Windows Firewall prompt for
  `hyperbutler.exe` was not checked. None appeared in this run.

## How it was checked

On 2026-11-01, on the reference machine (Windows 11 Home 26200), with this tree built as
`hyperbutler.exe`, a development guest image, and docker CLI 29.9.2. The installation was
a test one: `HYPERBUTLER_HOME` and `DOCKER_CONFIG` under `%TEMP%\hb-dev\scen\`, a pipe of
its own (`config set pipe \\.\pipe\docker_engine_hbscen`), a 2 GiB guest and an 8 GiB
data disk. The user's own installation and `~/.docker` were not touched. All test
directories were deleted afterwards. The runs:

- `hyperbutler.exe` without arguments: exit 2, usage on stderr (S0.1).
- `docker ps` and `docker run` after `hyperbutler stop`: the error quoted in S1.1.
- Four `busybox` containers (`--restart always`, `unless-stopped`, none, and
  `unless-stopped` stopped by hand) across `stop`/`start`, across a dockerd restart from
  `config set proxy.noProxy ... --apply`, and across a killed engine (S2.2, S6.1, S11.2).
- `hyperbutler start` in a separate PowerShell window, then `taskkill /T /F` on it (S2.1).
- A Go program listening on `127.0.0.1:18080` only, then `docker run -p 18080:80`, with
  `curl` to `127.0.0.1`, `localhost` and `[::1]` (S3.4). Nothing was bound to a
  non-loopback address by the test itself.
- A second test installation with the same pipe name (S5.1).
- A bind mount from a folder named `한글 폴더`, and a UNC source (S10.1, S10.3).

Not run: anything with Docker Desktop, WSL, a second Windows account, elevation, sleep or
sign-out, Compose, and the third-party tools in section 4.
