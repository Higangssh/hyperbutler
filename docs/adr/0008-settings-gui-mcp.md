# 0008. Settings, an on-demand GUI, and an MCP server

- Status: accepted (direction); the GUI toolkit is decided by a spike before 0.2
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
[settings.md](../research/settings.md#adversarial-review). It runs again on the GUI spike
and the MCP server.

## Consequences

- REQ-UX-002 changes from "no GUI" to "no GUI process with the window closed".
- The settings code becomes a stable internal API, because three front ends use it.
- An MCP server that can change state is a new way to cause damage. The read-only default
  and the separate delete flag are the mitigation; prompt-injected instructions in logs or
  container output are data the AI client sees, so the review must look at what the tools
  return as well as what they accept.
- Revisit the GUI decision if the spike shows WebView2 costs more than about 100 MB while
  open, or if users ask for a tray more than for anything else.
