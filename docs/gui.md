# The window (`hyperbutler gui`)

The on-demand GUI of REQ-UX-011, in the shape ADR 0008 decided: WebView2 with software
compositing, visual direction "B", started when needed and gone when closed. This page
says how it is built, what it may do, how it was checked, and what is left.

## Structure: one executable

The window is `hyperbutler gui`, a command of `hyperbutler.exe`, not a second
`hyperbutler-gui.exe`. The choice:

- **One implementation of each operation.** The window calls the same code as the CLI
  (status, `config list/set`, `doctor`, `diagnose`, the daemon.json check) in process,
  and runs the CLI itself for engine operations. A second executable would have needed
  all of that moved out of `package main` or reached through `hyperbutler.exe` anyway.
- **REQ-UX-016.** A double-click on `hyperbutler.exe` opens the window: the launcher
  that already detects "a console of its own started by Explorer" frees that console
  and runs the window in the same process. No second file has to be found, and the
  window and the CLI never come from different releases.
- **The engine never loads WebView2.** `WebView2Loader.dll` is loaded by full path only
  by `gui` (and the no-argument launcher that becomes it); the engine (`hyperbutler
  engine`) has no WebView2 or shlwapi module (checked on the VM with the engine's module
  list).
- **Size.** The stripped `hyperbutler.exe` grows by 713 KB (16.88 to 17.59 MB): the
  page, the 480 px mascot (258 KB) and the binding. A second executable would have
  repeated most of the 17 MB.
- **Cost:** `hyperbutler.exe` is a console program, so a double-click shows its console
  window for a moment before the window replaces it, and so would a Start-menu shortcut
  to `hyperbutler.exe gui`. If the MSI's shortcut (S0.2) should open with no flash, a
  tiny `-H windowsgui` stub that starts `hyperbutler.exe gui` is the fix; it is not part
  of this round.

From a shell, `hyperbutler gui` starts `hyperbutler gui --wait` detached and returns, so
the prompt comes back; `--wait` runs the window in the calling process (bench/footprint
uses it). `--lang en|ko|pseudo` overrides the language for one run; `--image-dir` is the
guest image for a start from the window.

Packages:

| Package | Role |
|---|---|
| `internal/gui/webview2` | a binding of the WebView2 interfaces the window uses, in pure Go (no cgo); replaces `github.com/jchv/go-webview2` |
| `internal/gui/app` | the Win32 window, the kill-on-close job, serving the page from memory, the clipboard and Explorer helpers |
| `internal/gui/bridge` | the page's requests: decoding, checks, the dispatcher, the views; the `Host` interface |
| `internal/gui/web` | `index.html`, `app.css`, `app.js`, the icons, the CSP |
| `internal/gui/i18n` | the message catalog (`messages/en.json`, `ko.json`), language choice, the pseudo-locale |
| `internal/schema` | the `--json` documents the CLI prints and the window reads (moved from `package main`) |
| `internal/dockerapi` | the one Docker API client: the CLI, `hyperbutler mcp`, `hyperbutler import` and the window share it, with its name checks and log helpers |
| `cmd/hyperbutler/gui_windows.go` | `hyperbutler gui` and the `Host` implementation |
| `assets` | the embedded mascot (`assets/brand/mascot-480.png`) and icon |

Engine control stays in `package main` behind `bridge.Host`: start, stop and restart
share the installation, lock, spawn and docker context code with ten other commands,
and the window runs them as the CLI (below), so there was nothing to gain from moving
them. The status document and the Docker API client moved because the window reads them
in process.

## The WebView2 host

- WebView2 runs with software compositing (`--disable-gpu`), passed through an
  `ICoreWebView2EnvironmentOptions` object of our own, so it reaches the browser only and
  not, through the environment, every program the window starts. The compositor and the
  network service run in the browser process (`--in-process-gpu`,
  `NetworkServiceInProcess2`, as Android's WebView runs them): 5 processes instead of 7
  and about 10 MiB less. Other arguments turn off background networking, component
  updates, domain reliability and pings, make every name lookup of the browser's network
  stack fail (`--host-resolver-rules`), and set WebRTC's IP handling policy to
  `disable_non_proxied_udp`.
- WebRTC is the one network API the CSP does not govern, and the policy alone does not
  keep it off the network (measured, see "No network requests"). A guard script
  (`internal/gui/web/guard.js`), added to every document before the page's own scripts
  with `AddScriptToExecuteOnDocumentCreated`, makes `RTCPeerConnection` and the other RTC
  constructors undefined, not writable and not configurable, and refuses to create frames
  (`iframe`, `object`, `embed`, ...), which would bring them back in a fresh realm.
- The options object also answers `ICoreWebView2EnvironmentOptions3` with
  `IsCustomCrashReportingEnabled` true: the runtime keeps crash dumps in the profile
  (`gui\webview2\EBWebView\Crashpad`) instead of sending them to Microsoft. The loader
  reads it (`--verbose` logs it); a real crash was not provoked to see a dump land there.
- Minimized, the window sets the memory target to low (`ICoreWebView2_19`) and suspends
  the page (`ICoreWebView2_3::TrySuspend`) unless a request or an import is pending, and
  resumes it when restored.
- The page is served from memory on `https://hb.localhost/` through
  `WebResourceRequested` with the filter `*`: every request the page makes, to any URL,
  is answered in process (a known file, else 404; another origin, 403). There is no
  port.
- The CSP is `default-src 'none'` with `'self'` scripts, styles and images only, no
  inline script or style, `connect-src 'none'`, no frames, forms, workers or fonts, and
  Trusted Types with no policy, so every HTML sink (`innerHTML` and the rest) throws.
- Navigation of the page or a frame to any other origin is cancelled; `window.open`
  and `target=_blank` open nothing; downloads are cancelled; permission requests are
  denied. Dev tools, context menus, the status bar, zoom, browser accelerator keys
  (find, print, reload, F12), autofill, saved passwords, pinch zoom, swipe navigation and
  SmartScreen URL checks are off.
- The binding never panics or calls `log.Fatal` in a callback, releases every interface
  it obtains (each request, response and stream too), pins the handler objects it hands
  to native code with `runtime.Pinner` and keeps them reachable after `Close`, and calls
  `ICoreWebView2Controller::Close` before releasing anything.
- `WebView2Loader.dll` (Microsoft-signed, BSD-3-Clause) ships next to `hyperbutler.exe`
  and is loaded from there by full path; there is no in-memory PE loader.
  `dist/package.ps1` downloads the `Microsoft.Web.WebView2` NuGet package (1.0.4258.31)
  from nuget.org, checks the package's SHA-256, takes the loader of the build's
  architecture (amd64 and arm64 are pinned, any other is refused), checks its own SHA-256
  and Microsoft signature, and puts the package's license (also pinned) in
  `THIRD-PARTY/webview2`. A pinned download was chosen over a vendored DLL: the
  repository keeps no binaries, and the pin names the Microsoft artifact exactly, as
  `dist/clients.lock` does for the docker CLI. The loader formerly came from the 2021 SDK
  copy in `github.com/jchv/go-webview2` (1.0.992.28), which predates
  `ICoreWebView2EnvironmentOptions3`.
- The window puts its process in a kill-on-close job (breakaway allowed) and waits up to
  1.5 s for the browser processes after the window is gone; the kernel ends whatever is
  left with the process. If the window already runs in a job that forbids breakaway, it
  does not make its own job (an engine it started could not leave it), and says so with
  `--verbose`.
- One window per browser profile (a named mutex keyed on the resolved data directory,
  `gui\webview2` in the state directory unless `--data` says otherwise; the window class
  carries the same key): a second start brings the first to the front, or, when no
  window of that instance appears within 3 s, says so in a message box. Two windows on
  one profile would share browser processes.
- Closing the window during an import asks in a native message box whether to stop it.
  Yes cancels the import and keeps the window and the process (the title and the page
  say "Stopping the import…") until the import's `Run` has returned, which undoes the
  item being copied and checks Docker Desktop, at most 40 s; No keeps the window open.
  After the window is gone, however it went, `app.Run` cancels an import that still runs
  and waits for the requests being answered and the background work
  (`Dispatcher.Wait`) within the same 40 s. The importer is never closed or replaced
  under a running `Run`.
- Without the WebView2 Runtime (or the loader DLL) the window shows the runtime's
  download link in a message box; the no-argument launcher then runs the console
  launcher of 0.1.

### No network requests (REQ-UX-005)

bench/footprint reads the socket table of the window's processes at every run. It found
one thing: from the second launch with a given profile on, the WebView2 Runtime's
browser process kept two TLS connections to Microsoft (52.97.x, 52.98.x, 40.99.x,
40.104.x: `config.edge.skype.com`, the Edge experimentation and configuration service,
whose host name is in `msedge.dll`). They come from the browser process itself, not its
network service: `--host-resolver-rules`, a dead `--proxy-server` and
`--disable-variations-seed-fetch` did not stop them. The only switch is the
`ExperimentationAndConfigurationServiceControl` policy, and hyperbutler does not change
policies. The first launch with a new profile made no connection (watched for 40 s),
and the difference is the profile's `Local State` file; the window removes it before each
start. With that, 50 of 50 runs held no socket. The page keeps nothing in `Local State`.

This is the runtime's behaviour, not the page's, and it may change with a runtime
update: bench/footprint fails a run that holds any socket, so it would show.

WebRTC (2026-11-02, runtime 156.0.4314.58). `bench/footprint -check-webrtc` starts the
window with `--probe-webrtc`, which adds `probe.js` to the page: it creates an
`RTCPeerConnection` with a STUN server on `127.0.0.1:3478`, makes an offer and sets it
(ICE gathering starts), in the page and in a new frame, while footprint reads the socket
table of the window's processes every 50 ms. Measured without the guard:

| Build | `RTCPeerConnection` | Candidates | Sockets of the window's processes |
|---|---|---|---|
| old arguments (`--force-webrtc-ip-handling-policy` and the policy) | reachable, page and frame | 0 | 8 UDP (mDNS on 5353, and pairs of ephemeral ports), 6 still held at the sample |
| no WebRTC arguments | reachable | 1 | the same 8 |
| CSP with `webrtc 'block'` | reachable | 0 | the same 8 |

So the policy switch works on candidates but not on sockets, `--force-webrtc-ip-handling-policy`
is not in the runtime (no such string in `msedge.dll`; it was dropped), and the CSP has no
working directive for it. With the guard: "RTCPeerConnection unavailable" in the page,
the frame refused (`NotSupportedError`), no socket in 3 of 3 runs.

## The bridge

The page talks to the host only through `window.chrome.webview.postMessage` with
`{"id", "op", "args"}`, answered with `{"id", "ok", "data"}` or `{"id", "ok": false,
"error": {"code", "detail", "ref"}}`. The operations:

| Operation | Arguments | What it does |
|---|---|---|
| `init`, `ready`, `log`, `layout` | none, or bounded text | language and catalog; first paint; a script error for `--verbose`; the layout check's result |
| `status`, `containers` | `auto` | the status view; containers by Compose project. `auto` (a timer) is refused while the window is hidden |
| `container.action` | 64-hex id, `start`/`stop`/`restart`, a token for stop and restart | the Docker API call; stop and restart wait 10 s |
| `container.logs` | 64-hex id, tail 1..1000 | the last lines, redacted, cleaned, at most 256 KiB |
| `confirm` | an action (and a 64-hex id for a container, or a key and value for `config.set`) | a one-time token for a destructive action or a confirmed setting; for the engine and dockerd it comes with REQ-UX-017's list of running containers and what happens to each |
| `engine.start`, `engine.stop`, `engine.restart`, `engine.restartDockerd` | a token, except for start | `hyperbutler start`, `stop`, `restart`, `config daemon apply`, as child processes |
| `config.list`, `config.set` | key, value (printable, at most 1 KiB), a token for a confirmed key | `config list --json`; `config set -- KEY VALUE` for the keys `bridge.Editable` lets the window change |
| `daemon.get`, `daemon.validate`, `daemon.save` | text, at most 64 KiB; a token for save | read, check (the engine's own check), write the user's daemon.json |
| `storage`, `doctor`, `diagnose`, `diagnose.reveal` | none | read-only storage; doctor's checks; a diagnose zip in `%TEMP%`; Explorer on that zip |
| `error.copy` | a ref | puts an earlier error's English original on the clipboard |
| `import.plan`, `import.run`, `import.cancel` | item ids and a token for run | the Docker Desktop import (below) |

Rules that hold for every message:

- `Decode` accepts one JSON object with a known operation and exactly its arguments:
  unknown fields, trailing data, ids outside 1..2^53, invalid UTF-8 and messages over
  512 KiB are refused (HB-GUI-001). `FuzzDecode` covers it.
- No operation takes a path, a command or a URL. A container is named only by the
  64-digit id the list handed out; a setting by a name `config.Lookup` knows, classified
  three ways in `bridge.Editable`: changed freely (memory, cpus, gui.*); changed after a
  confirmation (hostGateway.enabled, hostGateway.ports, autostart, updateCheck,
  context.manage: they open this PC's ports to the containers, add a sign-in task, reach
  GitHub, or change the user's docker CLI), through `confirm` with the setting and its
  new value, the page's dialog saying what the setting does; or CLI-only (paths, the
  pipe, proxies, CA trust, the engine memory limit, network.frameCheck).
  `TestEditableCoversEveryKey` fails until a new key is classified.
- `config.set` and a daemon.json save wait at most 2 s for the operations lock (a start,
  stop or `config daemon apply` holds it) and then answer busy (HB-ENG-006), rather than
  hang for the CLI's minute. A save reads daemon.json again under that lock, so a file
  that gained credentials after the page loaded it is not overwritten.
- `config set` gets `--` before the key and value, so a value is never a flag; the
  window never passes `--apply`. A restart is its own operation, after the page showed
  the REQ-UX-017 list.
- Destructive operations need a confirmation token (`internal/gui/bridge/confirm.go`):
  engine stop and restart, the dockerd restart, a daemon.json save, container stop and
  restart, starting an import, and changing a confirmed setting. The page asks `confirm`
  for the action; the host computes what it stops first (for the engine and dockerd, the
  REQ-UX-017 list), returns it with a random 128-bit token, and refuses the operation
  unless the request echoes a token for that action (and that container, or that setting
  and value) that is unused and at most ten minutes old. The change lock (or, for an
  import, the busy check) is taken before the token is spent, so a request refused as
  busy keeps it. Ten minutes for every token: the REQ-UX-017 list can take a while to
  read, and a shorter life protects nothing, since a token is single-use and bound to its
  action, and a page that could misuse one could ask for another. At most 32 tokens are
  outstanding. The page shows its dialog between the two, so
  nothing is confirmed twice. This guarantees that the host computed the list before
  anything stops and that a confirmation cannot be skipped by calling the operation
  directly or reused; it cannot prove that a person clicked (a page running injected
  script could ask for a token and echo it), which only a native dialog could, and which
  the absence of any HTML sink and the Trusted Types rule are there to prevent.
- No operation the page can ask for deletes anything of the user's: no container,
  image, volume or file. The host itself removes only what it made for the window: the
  browser profile's `Local State` before each start (see "No network requests") and the
  previous diagnose zip in `%TEMP%` when it writes a new one; an import undoes in
  hyperbutler the item it was copying when it is canceled. A daemon.json save replaces
  the file, after a confirmation. `diagnose.reveal` takes no argument and opens only the
  zip this window wrote (the host checks again). `error.copy` copies only text the host
  kept.
- At most 8 requests run at once; operations that change state run one at a time and a
  second is refused as busy (HB-GUI-002).
- Every string the page gets that someone else wrote (container names, images, status,
  logs, engine errors, doctor text, settings) is redacted, stripped of control and
  bidirectional characters and bounded. daemon.json that holds credentials is shown
  redacted and read-only, and `daemon.save` refuses to overwrite it.
- Engine operations run `hyperbutler.exe` as a child with `CREATE_NO_WINDOW` and, inside
  the window's job, `CREATE_BREAKAWAY_FROM_JOB`: a start or stop the user asked for
  finishes, and the engine it starts keeps running, if the window closes meanwhile.

The page shows logs and names with `textContent` only; `web_test.go` fails on any HTML
sink, `eval`, network API, inline script or style, or outside URL in the page.

## Behaviour

- Opening never starts the engine, unless `gui.startOnOpen` is true (default false):
  then the first `init` starts a stopped, set-up engine. Closing never stops it; the first
  close while it runs says once that it keeps running (S0.3), remembered by
  `gui\close-note-shown` in the state directory.
- Nothing polls while the window is hidden or minimized: minimizing hides the WebView2
  control (the page's `document.hidden` turns true and it clears its timer), and the
  host refuses a timer's refresh then. While visible the page refreshes the status at
  most every 5 s, and on focus or a user action; logs refresh only by hand.
- Destructive actions ask first in the page: stopping or restarting a container, and
  stopping or restarting the engine or dockerd, which first lists the running containers
  that will stop and which come back (REQ-UX-017). Changing a setting that needs a VM or
  dockerd restart shows a banner with "Restart now", which goes through the same list.
- Screens: Home (state, memory, CPUs, disk, containers; the error state with steps by
  code), Containers (by Compose project, actions, logs), Storage (read-only), Settings
  (keys with their apply time, daemon.json editor), Help (doctor, diagnose, the recovery
  ladder), and the first-run screen with doctor's key checks.

### Import from Docker Desktop

Storage has an "Import from Docker Desktop" card. Its dialog lists what Docker Desktop
has, as `internal/desktopimport` plans it (with volume contents compared): each image
and volume with a checkbox, its kind, size and status. New, partly imported (missing
tags) and same-name-not-compared items can be chosen; already imported ones are greyed
out and cannot, nor can conflicts, unsupported items, or a volume a running Docker
Desktop container mounts ("in use: stop it in Docker Desktop first", with the
containers' names), nor a volume of the same content with other labels ("same content,
different labels": overwriting it is `import --from-desktop --overwrite-volumes` in a
terminal), nor any state the window does not know yet ("needs attention"). Each status
and detail is a sentence of the window's catalog, built from the plan's fields (the
missing or conflicting tags, the volume driver, the containers that mount a volume),
never the import's English reason, which stays with the host behind "Copy the English
original", as do the plan's recovery notes and the differences Docker Desktop's check
found. `TestImportStatus` runs every status through the adapter and checks it gives only
statuses and details the catalog has. Starting asks for a confirmation (and its token), progress is pushed
by the host as `{"event": "import"}` from the import's callback, Cancel cancels its
context, and the end shows what was imported, already there, refused or failed, and the
import's own check that Docker Desktop is unchanged.

Like `import --from-desktop`, the copy holds the installation's import lock, which the
import package takes itself (a second import is refused, HB-IMP-005; a plan removes
nothing and takes none), and the copy is refused when the free-space check
says it may not fit in the VM's data disk or on the host volume (HB-IMP-004); the dialog
then shows why and offers "Start anyway", which is `--force-space`.

The window's side is a thin adapter (`cmd/hyperbutler/guiimport_windows.go`): `Open`
with hyperbutler's pipe as the target, `Plan`, and `Run` with the plan the user saw;
`import.run` takes only ids of that plan whose state can be chosen. Docker Desktop not
running is HB-IMP-003. `--import-from-pipe` (hidden) points it at another engine for
tests, as `hyperbutler import --from-pipe` does.
## Languages (REQ-UX-013)

Every string is in `internal/gui/i18n/messages/<lang>.json`, keyed, whole sentences with
`{name}` placeholders and CLDR plural forms (`key#one`, `key#other`). English is the
source, Korean ships too. The language follows the Windows display languages
(`GetUserPreferredUILanguages`) unless `gui.language` (auto, en, ko) says otherwise.
Errors show the translated explanation, the stable code (`HB-VM-001`, ...) and "Copy the
English original". Doctor's summaries and container status texts (`Up 10 seconds`) are
dockerd's or the CLI's English and are shown as such under translated titles.

Tests: `TestCatalogComplete` (every key in every language, same placeholders, the plural
forms each language uses, no markup), `TestPageKeys` (every key the page and the host
name exists), `TestPseudoLonger` (the pseudo-locale is at least 30% longer). The layout
check is `bench/footprint -lang pseudo -check-layout`: the page shows every screen in the
pseudo-locale and reports any element that goes past the window edge, clips its content
or shows a missing key; on 2026-11-01 it reported none at the default window size.

## Accessibility

Semantic HTML (a `tablist` with arrow, Home and End keys, buttons, labels, `role=switch`,
`<dialog>` with focus on Cancel, `aria-live` for status and results), visible focus rings,
`forced-colors` support. UI Automation exposes the page: the drive scripts found and
pressed every button by name (through the `Chrome_RenderWidgetHostHWND` element), in
both languages. Not yet checked with Narrator.

## Checks (2026-11-01)

Windows 11 Home 26200, Core Ultra 7 155H, display at 200%, WebView2 Runtime 156.0.4314.58,
window 1120 x 780 DIP, a test state directory with its own pipe and `context.manage false`.

Driven through UI Automation against a running test engine, in Korean and English:
started the engine from the first-run and home screens, listed a running busybox
container (and a Compose-labelled one, grouped under its project), viewed its logs
(markup in the output shown as text), stopped it after the confirmation, changed `cpus`
from 2 to 3, saw the "applies when the VM restarts" banner and the restart warning with
the container that would stop, restarted (`status` then said 3 CPUs), stopped the engine
after the REQ-UX-017 list, and got the error screen for a start whose pipe another
program held (HB-PIPE-001, with its steps). Screenshots were taken with `PrintWindow`
for every screen; they are not in the repository.

bench/footprint, 50 runs, engine stopped:

| | Measured | Budget |
|---|---|---|
| Private working set, all processes | median 91.0 MiB, max 94.4 MiB | 100 MiB |
| Private bytes, all processes | median 173.2 MiB, max 176.6 MiB | (not a budget) |
| Processes | 7 (hyperbutler.exe and 6 msedgewebview2.exe) | 8 |
| First paint, warm | median 429 ms, max 453 ms | 2 s |
| Window close to last process gone | median 222 ms, max 249 ms | |
| Processes left 2 s after close | 0 of 50 | 0 |
| Network sockets held | 0 in every run | 0 |

After the confirmation tokens and the import were added (2026-11-01, 20 runs): median
90.1 MiB, max 92.5 MiB, 7 processes, first paint 412 ms warm (max 432 ms), nothing left
after 20 closes, no socket.

The import dialog was checked against a second test installation serving Docker
Desktop's pipe (`\\.\pipe\dockerDesktopLinuxEngine`), with busybox in both, alpine and a
volume only in the second: busybox was greyed out as already imported, the other two were
chosen, confirmed and copied with progress, and the result said 2 imported and Docker
Desktop unchanged.

The working set is 14 MiB above the spike's (77 MiB): a page with five screens and their
script. After the layout check has shown every screen it was 98.4 to 98.6 MiB, close to the
budget; that is the number to watch as screens grow. The first paint here (about 430 ms
from launch to the page's own "painted" message, after its first status call) is faster
than the spike's 1445 ms; the spike decoded the 1.1 MB mascot, this page a 258 KB one,
and the runtime was warm. Cold starts (a new profile) measured 0.9 to 1.6 s.

## Checks (2026-11-02)

Same machine and runtime; loader 1.0.4258.31; a test state directory with its own pipe.
bench/footprint now runs one CLI command before the first launch: the very first start of
a new executable holds about 30 MiB more private working set in hyperbutler.exe (Windows
loading the new image; the Go heap is the same 2 to 6 MB), which made run 1 of every
fresh build an outlier. Before is the build of `d601dc6`, after is the review's fixes.

| | Before | After | Budget |
|---|---|---|---|
| Private working set, 20 runs | median 90.4, max 93.0 MiB | median 82.0, max 85.1 MiB | 100 MiB |
| Private bytes, 20 runs | median 173.0, max 176.3 MiB | median 154.0, max 158.1 MiB | |
| After every screen (`-check-layout`) | 98.5 to 101.6 MiB (3 runs; one over the budget) | median 90.6, max 93.2 MiB (5 runs) | 100 MiB |
| The same, minimized 15 s | 96.8 MiB | 79.3 MiB | |
| Processes | 7 | 5 (hyperbutler.exe, browser, crashpad, a utility, the renderer) | 8 |
| First paint, warm | median 414 ms | median 491 ms | 2 s |
| Close to last process gone | median 221 ms | median 201 ms | |
| Left 2 s after close; sockets | 0; 0 | 0; 0 | 0; 0 |

What moved it: the compositor and network service in the browser process (about 10
MiB after every screen, and 2 processes), the minimized memory target and suspend. The
immutable cache header for images and clearing the container log are in, but made no
difference large enough to see in these runs; `--renderer-process-limit=1
--disable-extensions` changed nothing and are not used, nor is a low memory target while
visible (it trimmed the working set by paging out, not by freeing: private bytes stayed).
By process after every screen, the renderer grows from about 20 to 27 MiB (Settings +3,
Help +4, Storage +1.6); that is the number to watch as screens grow. The pseudo-locale
layout check still reports no overflow. The aim of 90 MiB after every screen is met by
the median of 5 runs within 1 MiB, not with margin.

Close during an import, against a second test installation serving Docker Desktop's
pipe, with a 300 MiB volume of random data and alpine only there (Korean window): the
copy of the volume started 1.7 s after the confirmation; the window was closed at once,
the message box asked, Yes was pressed, the import stopped 0.76 s later and the process
was gone 1.7 s after Yes. Afterwards Docker Desktop's volume had the same SHA-256
(`a775cce2...`), no helper container and no other volume; hyperbutler had alpine (done
before the volume) and no volume at all, so nothing half-copied. In an earlier run the
close came after the volume's copy had already been verified: hyperbutler kept the
complete volume (same SHA-256) and recorded it in `imports-complete.json`, and the S0.3
note came after the import had stopped.

## Adversarial review (2026-11-01)

A review of the bridge, the page, the binding and the host found no critical or high
issue. Findings and what was done:

| # | Finding | Severity | Status |
|---|---|---|---|
| 1 | Two windows shared one browser profile; closing the first ended the second's browser processes with its job | medium | Fixed: one window per state directory, a second start brings it to the front. Partial: keyed on the state directory, not the profile; corrected in L1 below |
| 2 | The window accepted an empty daemon.json that the engine refuses | medium | Fixed: the check is exactly the engine's |
| 3 | Confirmations and the REQ-UX-017 list are in the page; a page made to run attacker script could call `engine.stop`, `daemon.save` and `engine.restartDockerd` without them | medium (design) | Fixed as the maintainer decided: the host enforces one-time confirmation tokens, issued only with the list it computed (see "The bridge"); what remains is that a token cannot prove a click |
| 4 | WebRTC is not governed by the CSP or the resource filter | low/medium | Claimed fixed for UDP by the policy switch; unverified, and wrong: measured, the runtime still opened UDP sockets. Fixed in M3 below |
| 5 | A log tail over 4 MiB started mid-frame and showed nothing | low | Fixed: resynced to the first whole frame (`FuzzDemux`) |
| 6 | Concurrent "is the engine running" probes take the lock file and can read each other as a running engine | low | Fixed: the window's status and pipe lookups run one at a time |
| 7 | After a double-click freed the console, some failures were silent | low | Fixed: every failure shows a message box |
| 8 | `--verbose` logged the start of every message, which can be typed text | low | Fixed: the operation and size only |
| 9 | Diagnose zips accumulate in `%TEMP%` | low | Fixed: a window keeps one |
| 10 | The REQ-UX-017 preview inspected containers one by one | low | Fixed: 8 at a time within its deadline |

Also checked and found sound: the 64-hex id check (a name cannot be smuggled into the API
path; dockerd's prefix resolution can only pick a container, which the user's buttons
can too), `--` before `config set` values, the reveal and copy operations, the binding's
COM lifetimes (borrowed environment and controller pointers referenced, closed-before-
ready controllers closed and released, handlers retired, not freed), and the redaction of
every view.

## Second adversarial review (2026-11-02)

| ID | Finding | Severity | Outcome |
|---|---|---|---|
| H1 | Closing the window during an import killed it before its cleanup: `Run` ran on the window's context, the process exited 1.5 s later, and the importer was closed under it | high | Fixed: the import has a context of its own; a close asks (native message box), Yes cancels and waits for `Run` (at most 40 s, "Stopping the import…" shown), No keeps the window; `Dispatcher.Wait` drains requests and background work after the window is gone; the importer is never closed or replaced under `Run`. `TestImportOutlivesWindow`; checked on the machine (see Checks) |
| M1 | `config.set` changed `hostGateway.*`, `autostart`, `updateCheck` and `context.manage` with no confirmation | medium | Fixed: three-way classification (free, confirmed with a token for the key and value, CLI-only), a dialog that says what the setting does. `TestEditableCoversEveryKey` (three-way), `TestConfirmedSettings`; Korean screenshot taken |
| M2 | Tokens were spent before the exclusive lock, so a busy refusal burned them; two minutes was short for the REQ-UX-017 list | medium | Fixed: lock (or the import's busy check) first, then the token; every token lives ten minutes (reason in "The bridge"). `TestBusyKeepsToken` |
| M3 | The WebRTC mitigation was unverified; `--force-webrtc-ip-handling-policy` is not in the runtime | medium | Fixed and measured: `bench/footprint -check-webrtc`; the policy stopped candidates but not 8 UDP sockets, the CSP cannot; a guard script removes `RTCPeerConnection` and frames before the page runs: no socket. The unknown switch is gone |
| M4 | `ImportProgress.Current` bypassed `field()` | medium | Fixed: cleaned in the bridge. Tested with U+202E |
| L1 | Single instance keyed on the state directory, not the resolved profile; silent exit when the mutex is held but no window is found | low | Fixed: keyed on the resolved data directory, window class per instance, a message after 3 s without a window. `TestInstanceKey`, `TestSingleInstance` |
| L2 | `Host.ConfigSet` could block 60 s on the operations lock | low | Fixed: 2 s, then HB-ENG-006, which the page shows. `TestBusyLock`, `TestGUIHostLock` |
| L3 | `daemonSave` neither took the operations lock nor read the file again under it | low | Fixed: `DaemonWrite` takes the lock and hands what is there now to the bridge's check. `TestDaemonJSON`, `TestGUIHostLock` |
| L4 | `package.ps1` took the loader from go-webview2, which production no longer imports; no arm64 answer | low | Fixed: pinned `Microsoft.Web.WebView2` download (package, loader and license pins, signature check); amd64 and arm64 pinned, other architectures refused. Checked with a snapshot package |
| L5 | `home.cpus.sub` and `import.done` lacked plural forms; the page glued sentence pieces | low | Fixed: plural keys, catalog sentences with placeholders, `Intl.ListFormat`; `web_test.go` refuses the old patterns; the pseudo-locale check passes |
| L6 | Memory close to the budget after every screen | low | Fixed after measuring: median 99.2 to 90.6 MiB after every screen, 90.4 to 82.0 idle, 97 to 79 minimized (see Checks); the aim of 90 is met only roughly |
| L7 | Crash reports could go to Microsoft; the docs said "nothing deletes" | low | Fixed: `ICoreWebView2EnvironmentOptions3::IsCustomCrashReportingEnabled` true (the loader reads it; no real crash was provoked to see a dump); the deletion wording now says what the host does remove. `TestOptions3` |
| New | The import dialog showed the import's English details in the Korean window | medium | Fixed: every status and detail is a catalog sentence (en, ko) built from the plan's fields; the English reason, the recovery notes and the check's differences are behind "Copy the English original". `TestImportStatus`, `TestImport`, `TestPageKeys`; Korean screenshot taken |

Claims of the first review corrected here: #1 was partial (L1), #4 was unverified and did
not hold (M3); "Nothing deletes" was too broad (L7). Also corrected: the window ran 7
processes and now runs 5, and the loader is no longer the 2021 one.

## Open items

- Narrator review; a dark palette (`prefers-color-scheme`); moving between monitors with
  different DPI.
- Container status, doctor summaries and the update note are English (dockerd's or the
  CLI's text).
- Storage is read-only (cleanup is the CLI's `docker system prune` for now); images and
  volumes have no actions yet.
- The Start-menu shortcut without a console flash (a windowsgui stub), with the MSI.
- The recovery ladder's steps 3 and 4 (reset a setting, replace the system disk) are
  instructions, not buttons.
- After every screen the window is at 90 to 93 MiB, within the budget but not with the
  margin asked for; the renderer's growth per screen is the next thing to cut.
- Crash dumps stay in the profile and nothing prunes them; whether one lands there was
  not checked with a real crash.
- A volume with the same content but other labels is not offered for import in the
  window, even when it is empty and the import could create it again.
- The experimentation-service connection is avoided by resetting `Local State`; if a
  runtime update starts it anyway, bench/footprint fails, and the fallback is the policy
  (documented for administrators) or the native toolkit.
