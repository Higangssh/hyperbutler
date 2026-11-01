# 0008. Settings, an on-demand GUI, and an MCP server

- Status: accepted. Toolkit decided on 2026-11-01: WebView2 with software compositing,
  under the budgets in "Toolkit spike results"; native Win32 stays the fallback if they
  are not met
- Date: 2026-10-31
- Requirements: REQ-UX-002, REQ-UX-004 to REQ-UX-012

## Context

hyperbutler started with "no GUI and no tray" as a non-goal (REQ-UX-002), because the
weight of Docker Desktop is one of the main complaints about it. Two things changed that:

- Users need to change settings. The guest's `daemon.json` lives on the read-only system
  disk, so today a LAN or corporate registry (`insecure-registries`), a mirror, or a log
  limit can't be set at all. The LAN registry bench on 2026-10-31 hit this directly.
- The research in [settings-and-gui.md](../research/settings-and-gui.md) and section 8 of
  [wsl2-docker-pain-points.md](../research/wsl2-docker-pain-points.md) shows what people
  dislike about the Docker Desktop app itself: updates that break setups (A2), start hangs
  (A1), settings scattered over seven places and sometimes overwritten (A6), sign-in
  pressure (A4), telemetry (A12), heavy background processes (A5), and resets as the fix
  (A11). It also shows what the GUI is mostly used for: status, the container list with
  start/stop, logs, opening a port, cleanup and disk use (A17).

The maintainer also wants hyperbutler to work well with AI coding tools.

## Options

1. **Keep the CLI only.** Lightest, but leaves settings unsolved for people who don't live
   in a terminal, and gives no reason to switch for most Docker Desktop users.
2. **Copy Docker Desktop: a resident GUI and tray.** Familiar, but brings back the weight
   and the process count that are the point of hyperbutler.
3. **Settings CLI first, then an on-demand GUI and an MCP server over the same internals.**
   The GUI and the MCP server are clients of the engine, started when needed and gone when
   closed.

## Decision

Option 3.

- **0.1:** `hyperbutler config` over the existing `settings.json`, `daemon.json`
  passthrough, Windows CA import and a manual proxy, `--json` output, bounded start
  failures, a local `diagnose` zip, and a clean `uninstall` (REQ-UX-004 to REQ-UX-010).
- **0.2–0.3:** an on-demand GUI (REQ-UX-011) and `hyperbutler mcp` (REQ-UX-012). Both use
  the same internal calls that back `status --json`, `config` and `diagnose`, so there is one
  implementation of each operation.
- **Rules:** no account, no telemetry, no forced or silent updates, no plan-gated
  features, no extensions marketplace, no built-in AI assistant. AI tools connect through
  MCP instead.

### GUI shape

- Visual direction (2026-11-01): the friendly, mascot-forward look ("B" of three mockups):
  - a light blue ground with soft, round white cards and pill tabs;
  - big numbers and plain, warm copy;
  - the butler mascot greeting on the home screen.
  The flat stacked-layers icon stays the app and taskbar icon. The mockups also cover
  containers and logs, storage, settings with `daemon.json`, troubleshooting, first run and
  a start failure.
- Not Electron. The candidates are WebView2 (already part of Windows 11) driven from Go,
  or a native toolkit. A spike measures memory, process count, start time and binary size
  for both before 0.2; the result is added here.
- It exits when its window closes and does not poll while hidden. `bench/footprint`
  checks that no GUI process is left 2 s after closing.
- A tray icon, if offered, is off by default. When on, it either runs inside the engine
  process or is counted as the one optional helper of REQ-UX-002; the broker (ADR 0004)
  may need that slot, which is decided when the tray is built.

#### Toolkit spike results (2026-11-01)

Two spikes draw the same home screen of direction "B" (light blue ground, round white
cards, pill tabs, big numbers, the mascot, Korean text) from pure Go, since cgo is not
available: `spike/gui/webview2` (WebView2 hosted in a Win32 window through
`github.com/jchv/go-webview2/pkg/edge`, page and mascot served from memory on
`https://hb.localhost/`, no network port) and `spike/gui/native` (raw Win32 plus the GDI+
flat API through `golang.org/x/sys/windows`, no third-party module). Both read
`hyperbutler status --json` once at start (no polling) and have a Start button that runs
`hyperbutler start` with `-real-start` and is stubbed otherwise. `spike/gui/measure`
launches a spike, waits for its "painted" line, samples the GUI and all its descendants
with NtQuerySystemInformation 10 s later, closes the window and polls until every process
is gone.

Setup: Windows 11 Home 26200, Core Ultra 7 155H, 32 GB, display at 200% (192 DPI),
window 960 x 620 DIP, Korean UI, WebView2 Runtime 156.0.4314.50, Go 1.27,
`go build -trimpath -ldflags "-H windowsgui -s -w"`. Medians of 5 runs, warm / cold.
"Cold" is the first start of a newly copied executable and, for WebView2, a new user data
folder; the Windows file cache was not emptied (that needs administrator tools).

| | Native (Win32 + GDI+) | WebView2, software compositing | WebView2, runtime defaults (GPU) |
|---|---|---|---|
| Processes while open | 1 | 7 (GUI + 6 `msedgewebview2.exe`) | 7 |
| Private working set, all processes | 5.4 / 5.7 MiB | 77.2 / 82.5 MiB | 171.4 / 177.4 MiB |
| Commit (private bytes), all processes | 17.8 / 18.0 MiB | 125.0 / 130.2 MiB | 241.5 / 245.6 MiB |
| Launch to first paint, warm | 109 ms | 1445 ms | 1509 ms |
| Launch to first paint, cold | 630 ms | 2364 ms | 2376 ms |
| Cold, counted from process creation | 107 ms | 1569 ms | 1603 ms |
| Binary (stripped) | 3.05 MiB | 3.40 MiB | 3.40 MiB |
| Runtime dependency | none (user32, gdi32, gdiplus) | Evergreen WebView2 Runtime, part of Windows 11 (871 MB on disk, shared) | same |
| Window close to last process gone | 31 ms | 267 ms | 327 ms |
| Any process left 2 s after close | 0 of 31 closes | 0 of 31 closes | 5 of 72 closes |

- The gap between "cold" and "cold from process creation" (about 500 to 750 ms) is spent
  before the program's first instruction, on a file that has never run: the antivirus
  scan. It is the first start after an install or update, for either toolkit.
- First paint for WebView2 is 1.4 s even warm: the runtime starts its browser, GPU,
  network, storage and renderer processes before the page can draw. The native spike
  spends most of its 100 ms decoding the 1.1 MB mascot PNG.
- With the GPU on, the GPU process alone holds about 95 MiB for this page, and 5 of 72
  closes left 4 to 6 `msedgewebview2.exe` processes for 5 to 10 s. They had 1 to 8
  threads left, so they were stuck exiting rather than still working. This happened
  without and with the fixes below, so it breaks REQ-UX-011 by itself.
- Fixes in the WebView2 spike: it calls `ICoreWebView2Controller::Close` on `WM_CLOSE`
  (go-webview2 does not), and puts itself in a kill-on-close job object, waiting up to
  1.5 s for the browser processes before it exits. The CLI is started with
  `CREATE_BREAKAWAY_FROM_JOB`, so an engine started from the GUI outlives it. With
  software compositing (`--disable-gpu` through `WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS`,
  set in the GUI's own environment) none of 31 closes left a process.
- Without the WebView2 Runtime the spike shows a message box that names the Evergreen
  runtime and its download link, and exits with code 3; it installs nothing. The CLI is
  unaffected.

**Revisit rule:** with runtime defaults WebView2 is over the 100 MB line (171 MiB private
working set, 242 MiB commit). With software compositing it is under it by private
working set (77 MiB, the REQ-MEM-001 metric) and over it by commit (125 MiB).

**Recommendation for 0.2: WebView2, with software compositing.** The native numbers are
15 to 30 times better, but the GUI REQ-UX-011 describes (a container list grouped by
Compose project, logs, images, volumes, settings, a `daemon.json` editor,
troubleshooting) in the look of direction "B", translated and accessible, would mean
writing a toolkit: layout, text input with IME, scrolling lists, a JSON editor, and a UI
Automation provider for every painted element, all over COM from pure Go. WebView2 gets
those from the browser. The GUI is on demand, so its cost is paid while the window is
open and is zero once it closes. Conditions:

- Software compositing by default, and the controller close plus job object above.
- Budgets checked by `bench/footprint`: at most 100 MiB private working set and 8
  processes for the GUI while open, first paint within 2 s warm, and no process left 2 s
  after close over at least 50 closes. If the budgets cannot be met, the native
  toolkit is the fallback.
- Ship `WebView2Loader.dll` (Microsoft-signed, BSD-3-Clause, 137 KB) next to the GUI
  executable. go-webview2 tries the normal `LoadLibrary` first and otherwise maps an
  embedded copy into memory with its own PE loader (go-winloader), which security
  products tend to flag.
- Before 0.2, replace go-webview2 with a small binding of the dozen interfaces we use, or
  vendor and fix it. It has one maintainer (last commit 2026-02-05) and calls
  `log.Fatal` inside COM callbacks. It also never releases some COM objects (every
  `WebResourceResponse`) and hands Go pointers to native code without pinning them.
- The status schema (`StatusJSON`) lives in `package main`. Move it to an internal
  package the GUI can call directly, instead of starting `hyperbutler status --json`.

#### The 0.2 GUI against the budgets (2026-11-01)

The GUI is `hyperbutler gui` in `hyperbutler.exe`, not a second executable: the window
calls the CLI's own code in process (one implementation of each operation), a
double-click on `hyperbutler.exe` opens it from the same process (REQ-UX-016), the engine
never loads WebView2 (only `gui` loads `WebView2Loader.dll`), and the stripped binary grows
by 713 KB instead of repeating 17 MB. The cost is the console window a double-click shows
for a moment; a `windowsgui` stub can remove it for the MSI's shortcut. `go-webview2` is
replaced by `internal/gui/webview2` (the conditions above: no `log.Fatal` in callbacks,
every COM object released, `runtime.Pinner`, `Controller::Close`, the loader DLL from disk
only), and `StatusJSON` moved to `internal/schema`. Details, the bridge and its review:
[docs/gui.md](../gui.md).

bench/footprint, 50 runs of `hyperbutler gui --wait` with the engine stopped (same machine
and display as above, WebView2 Runtime 156.0.4314.58, window 1120 x 780 DIP):

| | Measured | Budget |
|---|---|---|
| Private working set, all processes | median 91.0 MiB, max 94.4 MiB | 100 MiB |
| Processes | 7 | 8 |
| First paint, warm | median 429 ms, max 453 ms | 2 s |
| Processes left 2 s after close | 0 of 50 | 0 |
| Network sockets held | 0 of 50 runs | none |

All within the budgets, so WebView2 stays; native Win32 remains the fallback. The working
set is 14 MiB above the spike's and reached 98.4 MiB after the layout check had shown every
screen: the margin is small, and bench/footprint is the gate as screens grow. The socket
check found the WebView2 Runtime connecting to its experimentation service on every launch
after the first with a profile; no browser argument stops that, and the window removes the
profile's `Local State` before each start, after which no run held a socket (docs/gui.md).

Toolkit notes:

- **DPI:** both spikes are per-monitor DPI aware (v2). WebView2 scales the page by
  itself. The native spike multiplies every coordinate and font size by the DPI and
  handles `WM_DPICHANGED`; moving between monitors was not tested.
- **Dark mode:** WebView2 gets `prefers-color-scheme` and needs only the title bar set
  through `DwmSetWindowAttribute`. Native means a second hand-drawn palette, and dark
  standard controls rely on undocumented uxtheme exports.
- **Korean:** both render Hangul in the screenshots. GDI+ does not fall back to another
  font for missing glyphs, so the native spike picks one face per language (Malgun
  Gothic for Korean): a Korean UI showing a Chinese container name would draw empty
  boxes. Native would need DirectWrite (COM) for font fallback. The home screen has no
  text field; in WebView2 Chromium handles the IME, natively an `EDIT` or RichEdit control
  would, though neither looks like direction "B" without subclassing.
- **Not used:** `github.com/lxn/walk`, last commit 2021-01-12.

Implications for REQ-UX-013:

- The spikes take every string from a keyed catalog (`spike/gui/internal/spikeui`), in
  English and Korean, with a completeness test. The real catalog adds plurals and
  placeholders.
- 30% room: in WebView2, flex and grid layout absorb longer text by themselves (the grid
  widened the column for "Not set up"). In the native spike the same text was clipped
  until each big number got its own measure-and-shrink code; every painted element needs
  that.
- The pseudo-locale layout check is a page rendered with a pseudo-locale catalog and a
  screenshot (`PrintWindow` with `PW_RENDERFULLCONTENT` works for WebView2).
- The font stack for Latin, Korean, Japanese and Chinese is one CSS `font-family` list.

Implications for accessibility:

- WebView2 exposes the DOM to UI Automation (Narrator, keyboard focus, `aria-live` for
  status changes, high contrast through `forced-colors`), so the work is semantic HTML
  and ARIA, reviewed with Narrator.
- Natively, only real Win32 controls are in the UI Automation tree. In the spike that is
  the Start button, an owner-drawn `BUTTON` that takes focus and has a name. Every
  painted card, tab and number is invisible to a screen reader until we implement
  `IRawElementProviderSimple`/`Fragment` for it.
- Neither spike was checked with Narrator; do that on the first real screen.

Third-party code added by the spike (`hyperbutler.exe` links none of it; the 0.2 GUI ships
only `WebView2Loader.dll`, listed in `NOTICE`):

- `github.com/jchv/go-webview2` v0.0.0-20260205173254-56598839c808: MIT.
- `github.com/jchv/go-winloader` v0.0.0-20250406163304-c1995be93bd1: ISC, a permissive
  license equivalent to MIT.
- The `WebView2Loader.dll` embedded in go-webview2, from Microsoft's WebView2 SDK:
  BSD-3-Clause.

### Languages (2026-11-01)

- **Translated:** the GUI and its error guidance.
  - English (the source text) and Korean from the first GUI release.
  - Japanese and Simplified Chinese around 1.0.
  - Other languages as community translations, through the same message files.
- **English only, always:**
  - engine and guest logs, `hyperbutler logs` and the `diagnose` bundle, so they can be
    searched and shared in issues;
  - CLI output, for now, because scripts parse it (`--json` does not depend on language);
  - MCP responses.
- **Errors bridge the two.** A GUI error shows the translated explanation next to a stable
  error code (for example `HB-VM-001`) and a button that copies the English original.
- **Rules from the start:**
  - Every user-facing string comes from a message catalog with keys.
  - Messages are whole sentences with placeholders and plural rules, never concatenated
    fragments.
  - The language follows Windows by default and can be changed in settings.
  - Layouts leave room for about 30% longer text.
  - The type stack covers Korean, Japanese and Chinese.
  - The mascot's voice is written per language, not translated literally.
- **Docs:** English, with a Korean README (`README.ko.md`).

### MCP shape

- `hyperbutler mcp` speaks MCP over stdio. The AI client starts it and it exits with the
  client, so nothing stays resident.
- Read-only by default: engine status, containers, logs, images, volumes, disk and memory
  use, settings, diagnose.
- `--allow-write` enables start, stop, run, pull and `config set`. Deleting containers,
  images or volumes and prune need a separate flag.
- Output is redacted the same way as `diagnose` (tokens, proxy credentials, registry auth).
- The engine's npipe ACL already limits who can reach the engine, and the MCP server only
  talks to that pipe; it opens no network port.

## Adversarial review

Run on the 0.1 settings implementation (REQ-UX-006 to 008) on 2026-10-31: nine findings,
two high (the guest's dockerd-restart watch), all fixed or documented in
[settings.md](../research/settings.md#adversarial-review). On the MCP server: see
[mcp.md](../mcp.md). On the 0.2 GUI (2026-11-01): ten findings, none high, all fixed (the
last by host-enforced one-time confirmation tokens), in
[gui.md](../gui.md#adversarial-review-2026-11-01).

## Consequences

- REQ-UX-002 changes from "no GUI" to "no GUI process with the window closed".
- The settings code becomes a stable internal API, because three front ends use it.
- An MCP server that can change state is a new way to cause damage. The read-only default
  and the separate delete flag are the mitigation; prompt-injected instructions in logs or
  container output are data the AI client sees, so the review must look at what the tools
  return as well as what they accept.
- Revisit the GUI decision if the spike shows WebView2 costs more than about 100 MB while
  open, or if users ask for a tray more than for anything else.
