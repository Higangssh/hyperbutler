# Settings, GUI, and the app layer

Research snapshot, October 2026. This note turns the Docker Desktop app pain points (A1–A17 in
[wsl2-docker-pain-points.md](wsl2-docker-pain-points.md#8-docker-desktop-app)) into proposals
for hyperbutler. It covers user-facing settings, a `hyperbutler config` CLI, `daemon.json`
passthrough, an on-demand GUI, an optional tray icon, updates, diagnostics, and uninstall.

The proposals are not decided. Several of them conflict with today's
[requirements.md](../requirements.md): the "GUI or tray app" non-goal and REQ-UX-002. The last
section lists the requirement changes the maintainer would need to accept.

## Principles

1. **Few resident processes.** REQ-UX-002 allows at most `hyperbutler.exe`, the VMM (`vmmem`),
   and one optional helper while running. Nothing in this note may add a process that keeps
   running after the user closes a window. The ADR 0004 broker is the likely candidate for the
   "optional helper" slot, and that matters for the tray (see "Tray" below).
2. **The CLI and one settings file are the source of truth.** The GUI is a client of the same
   files and the same Docker API. Anything the GUI can do, the CLI can do, and scripts can
   do (A6, [roadmap#172](https://github.com/docker/roadmap/issues/172)).
3. **Never rewrite a file the user owns.** hyperbutler doesn't silently "fix" `daemon.json`,
   `~/.docker/config.json`, or its own settings file. An invalid file is an error with a line
   and column, not a reset to defaults (A6, A7, A9).
4. **No account, no telemetry, no forced updates.** These are product rules, not settings
   (A3, A4, A12).
5. **Recovery is granular and says what it destroys.** Never "factory reset" as the first
   answer (A11, U1).
6. **No network calls the user didn't ask for.** The only ones are image pulls and an update
   check that can be turned off. This also helps behind strict proxies (A8, A12).

## What hyperbutler should do, per pain point

"Threat" says whether the proposal puts the lightweight principle at risk.

| Pain | What hyperbutler does | When | Threat to "lightweight" |
|---|---|---|---|
| A1 Start/stop hangs | `hyperbutler start` prints phases (VM boot, guest agent, dockerd, API ready) with timings from `state.json`, and fails with an error code and a hint after `--timeout` instead of spinning. `stop` is already bounded (45 s, then kill). The GUI shows the same phase text. Invalid settings fail **at once** with the file, the line, and the key. | 0.1 | None |
| A2 Updates break things | No auto-install, ever. Updates come through the installer or WinGet/Scoop, when the user chooses. Keep the previous version's guest image next to the new one, and support `hyperbutler rollback` (or document reinstalling the old MSI) without touching the data disk. Publish a support window. "Latest only" is what users dislike. | 0.2–0.3 | None |
| A3 Nags and notifications | At most one line on `hyperbutler start`, at most once a day: "0.x.y is available". Turned off with `config set updates.check false`. No badges, no popups, no surveys, no "what's new" window. Nothing updates silently, including bundled `docker`/`compose` binaries if we ship them. | 0.2 | None |
| A4 Sign-in pressure | No account and no sign-in, for any feature. `docker login` for registries stays the standard Docker CLI flow. | 0.1 (rule) | None |
| A5 Heavy GUI, many processes | GUI is a separate, on-demand program that exits when its window closes. No Electron. It doesn't poll while minimized; it listens to `/events`. No tray by default. `bench/footprint` measures resident processes with the GUI closed and open. | 0.2–0.3 | **Yes, while the window is open** (see "GUI technology") |
| A6 Scattered settings, restarts, overwrites | One file, `%LOCALAPPDATA%\hyperbutler\settings.json` (it exists today). `hyperbutler config` reads and writes it. Every key says when it applies: live, engine restart, VM restart, or first start only. `config set` says what will restart and asks first. Memory, CPUs, and disk size are real limits, because hyperbutler owns the VM, unlike WSL2 sliders. A BOM is accepted. Invalid files are never overwritten. | 0.1 (core keys) | None |
| A7 `daemon.json` editing | A separate passthrough file, `%LOCALAPPDATA%\hyperbutler\daemon.json`, copied verbatim into the guest's `/etc/docker/daemon.json`. hyperbutler owns only the keys it must, such as `hosts`, and refuses a file that sets them, with a clear message. It never merges or rewrites the user's file. `config daemon validate` runs `dockerd --validate` in the guest. Keys that dockerd [reloads on SIGHUP](https://docs.docker.com/reference/cli/dockerd/#configuration-reload-behavior) apply without a restart. Others restart dockerd only, not the VM. | 0.1 | None |
| A8 Proxy and CA | **CA:** at start, import trusted root and intermediate certificates from the Windows stores (user and machine) into the guest trust store, so dockerd pulls work behind TLS inspection. On by default, with `ca.import false` to opt out. **Proxy:** `proxy.mode` = `system`, `manual`, or `none`. `system` reads the WinINet/WinHTTP settings and PAC. Applies to dockerd (pulls). Passing proxy and CA into *containers* and builds is opt-in later. Note: the egress already goes through Windows sockets (ADR 0006), which helps with VPNs but not with TLS inspection. | CA + manual proxy 0.1; system/PAC 0.2 | None. Kerberos/NTLM auth later may need code that should stay in-process. |
| A9 Credential helper | Don't ship a credential helper, and never touch `credsStore`. The docker context points at our pipe, and credentials stay with whatever the user's Docker CLI already uses (for example `wincred`). `doctor` warns if `credsStore` names a helper that isn't on PATH, but doesn't fix it. | 0.1 (rule), doctor check 0.2 | None |
| A10 Elevation prompts | No elevation after the one-time setup (ADR 0004, P2). `start` never triggers UAC. If something needs admin, `start` says what and why, once. | 0.1 | None |
| A11 Diagnostics and resets | `hyperbutler diagnose` writes a **local** zip: logs, `state.json`, settings, `doctor` output, version, Windows build. Secrets are redacted (proxy passwords, tokens, `auth` fields). The user reviews it and attaches it to an issue. Nothing is uploaded. Recovery is a ladder: `restart engine` → `restart` (VM) → `reset --settings` → `reset --vm` (keeps the data disk) → `delete` (data). Each step lists what it destroys and needs `--yes` when it deletes data. `doctor` is REQ-UX-003. | diagnose 0.1; ladder 0.2; doctor per REQ-UX-003 | None |
| A12 Telemetry | None. No usage statistics, no crash upload, no "anonymous" pings. The update check (A3) sends only a plain GET for a version file, with no ID. Say so in the README. | 0.1 (rule) | None |
| A13 Bloat | No extensions marketplace, no AI assistant, no bundled Kubernetes (already a non-goal), no onboarding survey, no learning center. Tools like lazydocker, Portainer, and kind work through the docker context. | Never | Adding any of these would |
| A14 Licensing | Apache-2.0. No paid tier, no features gated by seat or plan. If enterprise controls come later (a machine-wide policy file), they're free and need no account. | 0.1 (rule) | None |
| A15 Uninstall leftovers | Everything lives in `%LOCALAPPDATA%\hyperbutler` (and the data dir if moved), plus the `hyperbutler` docker context and the broker if installed. `hyperbutler uninstall --dry-run` lists every path, registry value, scheduled task, and context it will remove, and `--keep-data` keeps the disk. The MSI uninstaller calls the same code. No hosts-file edits, no services we don't remove, no files under `ProgramData` unless the broker needs them. | 0.1 (list + delete), MSI 0.2 | None |
| A16 Sleep and idle | Covered by REQ-PWR-001/002 and the memory work. App side: the GUI and tray must survive the engine pausing or restarting and show "reconnecting", not a blank window. No "resource saver" that freezes other software. hyperbutler owns its own VM, so pausing it can't freeze WSL. | 0.2–0.3 (GUI) | None |
| A17 What the GUI is for | See "What the GUI should show". | 0.2–0.3 | — |
| Autostart (A6) | Off by default. `config set autostart true` adds a per-user Run key or a logon task that runs `hyperbutler start`, with no window. It's removed on uninstall. | 0.2 | None. It starts the same processes the user would start by hand. |

## Settings model

- **File:** keep the existing `settings.json` (strict parser, atomic writes, fuzzed; see
  [cli-engine.md](cli-engine.md)). Extend it with the keys below. Don't add a second user
  file. `daemon.json` is the only other file, and it is passed through to dockerd untouched.
- **Commands:**
  - `hyperbutler config list` shows each key with its current value, its default, where the
    value came from (default, file, or flag), and when it applies.
  - `hyperbutler config get KEY`
  - `hyperbutler config set KEY VALUE` validates the value, writes atomically, then says
    "applies now", "restarts dockerd", or "restarts the VM; run `hyperbutler restart`". It
    never restarts silently, and `--apply` does the restart.
  - `hyperbutler config unset KEY`, `config reset` (all keys, asks first), and `config path`.
  - `hyperbutler config edit` opens the file in `%EDITOR%` or Notepad and validates it after
    save. If the file is invalid, the old one stays in effect.
  - `hyperbutler config daemon edit` and `config daemon validate` do the same for
    `daemon.json`.
- **Starter keys:**
  - `memory`, `cpus` (VM restart).
  - `disk.size` (first start; growing later is a separate command).
  - `data.dir` (first start; moving is `hyperbutler move`, REQ-DISK-007).
  - `pipe` (engine restart).
  - `context.manage` (whether `start` creates and selects the docker context).
  - `proxy.mode`, `proxy.http`, `proxy.https`, `proxy.noProxy` (engine restart).
  - `ca.import` (engine restart).
  - `updates.check` (live).
  - `autostart` (live).
  - `ports.lan` (opt-in LAN exposure, REQ-NET; live if the forwarder allows it).
  - `buildkit.gcKeepStorage` (REQ-DISK-006; engine restart).
- **Never stored here:** credentials or registry tokens. Proxy passwords are not stored
  either. Use the system proxy, or a URL whose credentials the user accepts in plain text, and
  warn when they do.
- **Admin policy (later):** an optional machine-wide file (for example under `ProgramData`, or
  HKLM policy keys) that can *lock* keys. Locked keys show as locked in `config list` and the
  GUI. Unlocked keys keep the user's values. This is the Settings Management behavior without
  the 4.27 "reset what isn't listed" surprise. It needs no account.

## GUI

### Shape
- **One program, launched on demand:** `hyperbutler gui`, or a Start-menu shortcut. It exits
  when its window closes, and leaves no background process behind.
- **How it reads state:** it talks to the Docker API over the named pipe, and to hyperbutler
  through the same files and commands the CLI uses (`state.json`, `settings.json`, and the
  engine's control channel).
- **No network access** beyond the local pipe. No sign-in screen, no web content, no remote
  feeds.
- **No polling while hidden.** It subscribes to Docker `/events` and to engine state changes.
  It refreshes stats only while a view that shows them is visible. Polling must never wake an
  idle VM ([desktop-feedback#406](https://github.com/docker/desktop-feedback/issues/406)).
- **Fast and honest when things fail:** the window appears at once with the engine phase. If
  the engine is stopped, it offers **Start**. If the engine is stuck, it shows the last error
  and a **Diagnose** button.

### GUI technology (the main risk to "lightweight")
- **Electron:** ruled out. It means a large install, a Chromium runtime of its own, and a
  multi-process tree (A5).
- **WebView2:** preinstalled on Windows 11, so the binary stays small. But WebView2 runs its
  own multi-process tree, with browser, renderer, GPU, and utility processes
  ([process model](https://learn.microsoft.com/en-us/microsoft-edge/webview2/concepts/process-model)).
  That is acceptable *only* because the GUI is on-demand: the processes exist while the window
  is open and must all exit when it closes. `bench/footprint` should check that.
- **Native Win32 (or a small native toolkit):** the fewest processes and the fastest start,
  but more UI work.
- **Recommendation:** decide with a spike. Measure cold open time, RAM with the window open,
  and process count, for WebView2 and native. Pick native if WebView2 opens in more than ~1 s
  or holds more than ~150 MB. These numbers are suggestions, not measured.
- **Stopgap for 0.1:** document lazydocker and Portainer against the `hyperbutler` context.
  This costs nothing and covers most of A17 (lazydocker has 53k GitHub stars).

### What the GUI should show
This follows A17: what people use, and nothing from A13.

1. **Engine status:** the phase (starting, running, stopped, error) with the last error and
   time, plus **Start**, **Stop**, and **Restart** buttons, and versions.
2. **Containers**, grouped by Compose project:
   - name, image, state, uptime, and ports;
   - a port opens `http://127.0.0.1:PORT` in the default browser
     ([for-win#9537](https://github.com/docker/for-win/issues/9537));
   - start, stop, restart, and remove, also per project.
3. **Logs** for one container or one project: follow, search with highlighting, timestamps,
   copy, and a bounded buffer.
4. **Shell:** "Open shell" launches Windows Terminal (or the console) with
   `docker exec -it … sh`. No embedded terminal, because that adds weight and bugs (see
   4.97's terminal fixes in the release notes).
5. **Images:** size, in use or unused, and dangling. Remove the selected images, or prune.
6. **Volumes:** size and the containers using them. Remove or prune.
7. **Disk and memory, host side:** the bytes the data disk really takes on the host, and the
   memory the VM really holds. These are the numbers Docker Desktop users can't trust (D1, D2,
   M1). Add **Prune build cache** and a "space returned" readout once REQ-DISK-001 works.
8. **Settings:** a form over `settings.json`, with each key marking when it applies; a
   `daemon.json` editor with validation that never rewrites the file; and proxy and CA
   status (how many CAs were imported, the effective proxy).
9. **Troubleshoot:** `doctor` results with their fixes, **Create diagnostics zip** (local),
   and the recovery ladder from A11, with the data-loss warnings.

It should **not** show: a sign-in screen, Docker Hub browsing, extensions, an AI chat,
Kubernetes, vulnerability scanning, a learning center, surveys, or ads.

### Tray
- **Off by default.** `config set tray true` turns it on.
- **What it shows:** status (running, stopped, error) and a menu: Start, Stop, Restart, Open
  GUI, Diagnose, Quit tray.
- **How it's built:** plain `Shell_NotifyIcon`, with no web view. It reads engine state
  through events and doesn't poll the Docker API.
- **Threat:** a tray is a resident process. REQ-UX-002 allows one optional helper, and the
  ADR 0004 broker may need that slot. The options are:
  - (a) run the tray icon inside the `hyperbutler.exe` engine process. It is detached from any
    console but can still own a hidden window in the user's session. This needs a check that
    it works with `DETACHED_PROCESS` and a job object.
  - (b) raise the budget to 4 *only when the user turns on the tray*.
  - Prefer (a) if the spike shows it's clean.

## Prioritised feature list

### 0.1 (must-have)
Small and mostly CLI. The 0.1 thesis is the engine (own VM, disk space comes back), so the
app layer must not delay it.

- Product rules written into the README: no account, no telemetry, no auto-update, no paid
  tier (A4, A12, A14, A2).
- `hyperbutler config list/get/set/unset/path` over `settings.json`, with apply-time per key
  and strict validation that never overwrites (A6).
- `daemon.json` passthrough, with validation and an engine-only restart (A7).
- Windows CA import into the guest, plus a manual proxy for dockerd (A8).
- `start` phase output and bounded failure with an error code (A1).
- `hyperbutler diagnose`: a local zip with redaction (A11).
- `uninstall --dry-run` and a full cleanup, including the docker context (A15).
- Documentation for using lazydocker and Portainer with the `hyperbutler` context (A17).

### 0.2–0.3
- The on-demand GUI with the nine views above, after the WebView2 vs native spike (A5, A17).
- The optional tray, off by default (A5).
- Opt-in autostart (A6).
- An update-available line that can be turned off, a WinGet/Scoop manifest, and
  rollback (A2, A3).
- System proxy and PAC (A8).
- The first `doctor` checks, toward REQ-UX-003. Add the `credsStore` check (A9).
- The recovery ladder, `reset --vm` and `reset --settings` (A11).
- `config edit`, and `config daemon edit` (A6, A7).
- A GUI and tray that survive engine restarts and resume (A16).

### Later
- Admin policy file with locked keys (A6, A14).
- Kerberos/NTLM proxy authentication, in-process (A8).
- Proxy and CA injection into containers and builds, opt-in (A8).
- A signed MSI for Intune/SCCM, with silent install options (A15).
- Opt-in Windows notifications for container exit or health changes.
- GUI localization.

### Never
- Account sign-in, or any feature that needs one (A4).
- Telemetry, analytics, or automatic crash upload (A12).
- Forced or silent updates of the app, the guest, or bundled CLIs (A2, A3).
- An extensions marketplace, an AI assistant, bundled Kubernetes, onboarding surveys, or a
  learning center (A13).
- An Electron GUI, or any GUI process that stays resident after its window closes (A5).
- Features gated by plan or seat (A14).
- Silently rewriting `daemon.json`, `~/.docker/config.json` (`credsStore`), or `settings.json`
  (A6, A7, A9).
- A one-button "factory reset" offered as the first fix (A11).

## Proposed requirement changes (for the maintainer)

These need the maintainer's decision. This note doesn't edit [requirements.md](../requirements.md).

- **Non-goals:** replace "A GUI or tray app" with something like: *"A resident GUI. A GUI may
  exist only as an on-demand program that exits when its window closes. A tray icon, if any,
  is off by default."*
- **REQ-UX-002:**
  - Reword "There is no GUI and no tray process" to: *"With default settings and no GUI window
    open, no GUI or tray process is running."*
  - Add that the process budget is measured with the GUI closed. With the GUI open, it's
    measured separately and reported.
  - Decide whether an enabled tray counts against the "one optional helper".
- **New candidates:**
  - **UX: no account.** No feature requires a Docker or hyperbutler account.
  - **UX: no telemetry.** With default settings, hyperbutler makes no network requests except
    image pulls and the update check, and the check can be turned off. Verified with a packet
    capture in `bench/`.
  - **UX: settings CLI.** Every user setting can be read and changed with `hyperbutler config`.
    An invalid file is never overwritten.
  - **UX: daemon.json passthrough.** The file reaches dockerd byte for byte. Keys hyperbutler
    owns are refused with a message.
  - **NET or UX: corporate CA.** Behind a TLS-inspecting proxy whose root CA is in the
    Windows store, `docker pull` works with no extra steps.
  - **UX: clean uninstall.** After uninstall, `uninstall --dry-run` lists nothing, and no
    hyperbutler file, registry value, context, or task remains.
