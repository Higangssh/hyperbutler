# 0010. When a session end stops the VM

- Status: accepted
- Date: 2026-11-10
- Requirements: REQ-PWR-003

## Context

The engine runs without a console, so it learns of sign-out and shutdown through a hidden
top-level window that receives `WM_QUERYENDSESSION` and `WM_ENDSESSION`
(`internal/engine/sessionend_windows.go`). Up to v0.2.0 it started its normal stop on
`WM_QUERYENDSESSION` and ignored `lParam`.

The query is only a question. Per the Win32 documentation:

- Any application may veto `WM_QUERYENDSESSION`; the system then sends `WM_ENDSESSION`
  with `wParam` FALSE to the applications that had agreed, and the session goes on.
- `lParam` carries flags: 0 for shutdown or restart, `ENDSESSION_LOGOFF` (0x80000000) for
  a sign-out, `ENDSESSION_CRITICAL` (0x40000000) when the end is forced, and
  `ENDSESSION_CLOSEAPP` (0x1) when the Restart Manager closes the application because an
  installer or updater must replace a file it holds or is servicing the system.
- An application should not end its work until `WM_ENDSESSION` arrives with `wParam`
  TRUE. After that handler returns, the process may be ended at any time.
- The Restart Manager sends the same pair to the windows of the processes it is closing.
  If they do not exit, a forced shutdown (`RmForceShutdown`, and package updates that
  close a package's processes) terminates them; an unforced one fails and the installer
  asks for a restart. The `RmShutdown` documentation does not promise a `WM_ENDSESSION`
  with `wParam` FALSE when another application refuses, so a query may be the last
  message the window gets.

On 2026-11-10 at 10:41:15 the v0.2.0 soak engine logged "the Windows session is ending;
stopping" while a packaged desktop app updated itself, with no sign-out or shutdown. The
engine had been started from a shell inside that app. The guest powered off cleanly in
1.4 s and the 24 h soak ended at 12h59m; the rest of the soak's process tree was closed
by the same update. Because `lParam` was not logged, we cannot tell whether the engine got
a close that was then confirmed (the likely case: it ran as a process of the package being
updated, and its neighbours were closed too) or a query that was later canceled. Whether
a package update sends this pair before it ends a package's processes is not documented;
this is inferred from the log.

So this decision does not by itself prevent that incident: if the close was confirmed, the
engine stops the same way it did. What it fixes is the canceled case, where stopping on
the query was wrong, and it makes the choice for `ENDSESSION_CLOSEAPP` on purpose. What
keeps the soak alive is running it outside the app's package (see Decision).

The hyperbutler MSI sets `MSIRESTARTMANAGERCONTROL=Disable`, so our own upgrades do not
use the Restart Manager on the engine.

## Options

For every flag, the query now only sets the shutdown block reason and returns TRUE at
once, and a `WM_ENDSESSION` with `wParam` FALSE stops nothing. That part has no real
alternative: stopping on the query is what broke the soak, and vetoing a sign-out or
shutdown only delays it. The open question is a confirmed `ENDSESSION_CLOSEAPP`
(`WM_ENDSESSION`, `wParam` TRUE, the flag set):

1. **Stop cleanly, like a shutdown.** Start the normal stop and hold `WM_ENDSESSION` until
   the guest is down (up to 20 s). The engine exits, as the Restart Manager asked.
   Pros: the documented contract; when the caller would terminate the process anyway, the
   guest powers off instead of losing its writes. Cons: a third-party updater can stop the
   VM, though only one that targets the engine's process.
2. **Ignore it and keep running.** Pros: no third-party updater stops the VM on purpose.
   Cons: a forced close then terminates the engine with the VM running, which is the
   unclean stop the session watch exists to prevent; an unforced one fails and the
   installer asks the user to restart Windows, blaming hyperbutler.
3. **Veto the query (return FALSE for `ENDSESSION_CLOSEAPP`).** Pros: an unforced Restart
   Manager session gives up before anything is closed. Cons: the same forced termination
   as option 2, and a vetoed query makes the installer fail or ask for a restart.

## Decision

Option 1. The engine stops only on `WM_ENDSESSION` with `wParam` TRUE, whatever the flags,
and logs which end it was (`why`: shutdown or restart, sign-out, Restart Manager, forced).
When something closes the engine, a clean stop is the only outcome that does not risk the
guest's disk; refusing does not keep the VM running against a forced close, it only makes
the stop unclean.

The soak runs the engine from Task Scheduler, outside any app package, so an app update
no longer reaches it.

## Adversarial review

- *Losing the head start.* Stopping on the query gave the guest a head start while other
  applications answered. The guest powers off in about 1.5 s, and in about 12 s with
  containers that ignore SIGTERM ([S2.4](../research/usage-scenarios.md)); `WM_ENDSESSION`
  holds the session for up to 20 s with the block reason shown. Windows shows its
  blocking screen after about 5 s, so the slow case depends on the user not clicking
  "Shut down anyway"; that was already true before this change. Accepted until the
  REQ-PWR-003 bench (sign-out, reboot, fast startup, 10 times each) is run.
- *A closer that never sends `WM_ENDSESSION`.* If something sent the query and then
  terminated the process without the confirming message, v0.2.0 would have stopped
  cleanly in time and this change lets the VM be killed (the data disk's ext4 journal
  recovers it, REQ-DISK-003). The documented senders (session end, the Restart Manager)
  confirm before they close; an undocumented one is speculative. Accepted as a risk; the
  `why` field of the log line shows which message a stop came from.
- *The block reason lingers.* Set on the query, it would stay on the window when no
  `WM_ENDSESSION` follows. It is set only once the end is confirmed, inside the handler
  that waits, and removed when that handler returns; a test asserts no reason is left
  after any message sequence.
- *`ENDSESSION_CRITICAL` may give no time.* Then the stop is best effort, as before; the
  window still starts it on `WM_ENDSESSION`.
- *An updater stops the VM.* Only one that closes the engine's own process, and it would
  end the process anyway. If logs show `why` = Restart Manager for updates that do not
  need the engine closed, revisit option 2 with a check of who asked.

## Consequences

A canceled sign-out or shutdown, and a Restart Manager request that another application
refuses, leave the VM running. A confirmed close still powers the guest off cleanly. The
log line now says which kind of end it was, so the next unexpected stop can be traced.
