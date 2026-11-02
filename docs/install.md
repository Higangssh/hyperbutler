# Installing with the MSI

From v0.1.0-preview.1 hyperbutler also ships as `hyperbutler-vX.Y.Z-windows-amd64.msi`, next to the zip.
The zip stays: it needs no installer and no admin rights for itself, but leaves the
prerequisites to you (README, Quick start). The MSI does them in the one UAC prompt of the
install.

## What the installer does

| Step | When | Undone on uninstall |
|---|---|---|
| Copies the release folder to `C:\Program Files\hyperbutler` (the guest image in `image\`, next to `hyperbutler.exe` and `hyperbutler-broker.exe`, which both boot it) | always | yes |
| Installs and starts the **hyperbutler broker service** (`hyperbutler-broker`, [ADR 0004](adr/0004-hcs-access-broker.md#installer-m4)), which starts the VM for every user signed in at the PC: no Hyper-V Administrators membership and no elevation needed | always: a first install fails (and rolls back) without it; an upgrade or a repair records a failure and goes on; a repair restores a deleted service but leaves one an administrator disabled | yes |
| Appends that folder and its `clients\` folder to the **end** of the system PATH, so a docker CLI you already have (Docker Desktop's, say) keeps being the one `docker` runs | always | yes |
| Turns on Virtual Machine Platform with DISM and asks for a restart | the box is ticked (default) and the Host Compute Service is missing | no: other programs (WSL, Hyper-V) may use it |
| Adds your account to Hyper-V Administrators (developer mode, ADR 0004 option A) | only with `ADDHYPERVADMIN=1` on the command line; applies after you sign out and back in | yes, if the installer added it |
| Takes out of Hyper-V Administrators the accounts an earlier installer added (0.2 added the installing user by default) | an upgrade, once the broker service runs, unless `ADDHYPERVADMIN=1` | - |
| Leaves the daily update check on, or records that it is off (`HKLM\SOFTWARE\hyperbutler`, `UpdateCheck` = 0), the default for every user who has not set `updateCheck` | the "Check GitHub for a new version" box (ticked by default); an upgrade keeps an earlier off | yes |
| A Start menu shortcut | always | yes |

The options page says what the broker service is: a small Windows service that starts and
stops the hyperbutler VM for the people signed in at this PC, takes only hyperbutler's own
requests over a local pipe, and lets each person reach only their own VM. It has no box:
it is how hyperbutler runs without administrator rights. The install folder is always
`C:\Program Files\hyperbutler`, which only administrators can change, because the service
runs `hyperbutler-broker.exe` from it as SYSTEM (the service refuses to install from a
folder anyone else could write).

`hyperbutler start` (and the window, and the sign-in task of `config set autostart true`)
boots through the service when it is installed, and starts it first if it is stopped
(interactive sign-ins may start it, nothing more; right after Windows starts, the
service's delayed automatic start may not have come yet). A service that is installed but
cannot be used (disabled, failing, not answering, another program on its pipe, too old)
makes the start fail with the reason and a pointer to `hyperbutler doctor` (the window
shows HB-VM-005 to HB-VM-008); it never quietly runs the VM another way. Only when no
service is installed does start run the VM in its own process, which needs Hyper-V
Administrators or an elevated shell; it says so in the engine log, and in the terminal
when the account has neither. `--backend broker` also fails without a service,
`--backend hcs` (developer mode) never uses it. A sign-in that is not interactive (ssh,
a batch task), which the service does not serve, runs the VM itself with a warning when
the account may use HCS (Hyper-V Administrators, or elevated), and otherwise fails,
saying to start hyperbutler from the desktop.

The installer never touches your data. Images, containers and volumes stay in
`%LOCALAPPDATA%\hyperbutler` through upgrades and uninstalls; `hyperbutler uninstall`
removes them and everything else hyperbutler created, before you uninstall the MSI.

Before an upgrade or uninstall replaces or removes files, the installer runs `hyperbutler
stop` for you, so a running engine shuts its VM down cleanly. That can take up to about a
minute. Then it stops and removes the broker service (an upgrade installs the new one
after the new files), which ends the VMs of other users signed in at the same time
(they see HB-VM-004 and start again).

## Silent installs

```
msiexec /i hyperbutler-vX.Y.Z-windows-amd64.msi /qn /norestart
msiexec /i hyperbutler-vX.Y.Z-windows-amd64.msi /qn /norestart ADDHYPERVADMIN=1 ENABLEVMP=0
```

The broker service is installed either way, so no one needs Hyper-V Administrators, and
a deployment tool that runs as SYSTEM covers every user of the machine. `ADDHYPERVADMIN=1`
(developer mode) also adds the account the install runs for to the group, for running the
engine without the service (`hyperbutler start --backend hcs`); under a deployment tool
that runs as SYSTEM it adds no one. `ENABLEVMP=0` leaves Virtual Machine Platform alone.
`UPDATECHECK=""` turns the daily update check off for every user of the machine (a user
can still turn it on with `hyperbutler config set updateCheck true`).
Without `/norestart`, a silent install that turned Virtual Machine Platform on restarts
Windows by itself; with it, msiexec exits with 3010 (restart needed).

## Privacy

hyperbutler sends no usage data or telemetry. Its one request of its own is the update
check: at most once a day it asks api.github.com for the latest release, anonymously (no
identifier beyond the request itself). The installer's options page has a box for it, and
`hyperbutler config set updateCheck false` turns it off later. Everything else on the
network (image pulls, registries, proxies) happens because you or your containers asked
for it.

If a step fails, the install still finishes, except the broker service on a first
install. `HKLM\SOFTWARE\hyperbutler\Setup` keeps the
outcome of the last step (`LastResult`) and the accounts the installer added
(`HyperVAdminsAdded`); `hyperbutler doctor` shows what is still missing (its `broker`
check shows a service that did not install or does not run). The broker step is
`"C:\Program Files\hyperbutler\hyperbutler.exe" setup broker install`, which an
administrator can run again by hand.

## Unsigned builds

Until releases are code-signed, Windows SmartScreen shows "Windows protected your PC" for a
downloaded MSI: **More info**, then **Run anyway**. Check the file first:

```
Get-FileHash .\hyperbutler-*-windows-amd64.msi -Algorithm SHA256   # compare with the release's SHA256SUMS
```

## Building it

```
dotnet tool install wix --version 5.0.2 --tool-path DIR
pwsh -File dist\package.ps1 -ImageDir IMAGE [-Snapshot]
pwsh -File dist\msi\build-msi.ps1 -Stage dist\out\hyperbutler-VERSION-windows-amd64 -Wix DIR\wix.exe
```

`build-msi.ps1` builds from the folder `package.ps1` staged (the zip's contents, with
`hyperbutler-broker.exe`), runs the Windows Installer consistency checks, and holds the
MSI to the same 150 MB limit as the zip (REQ-UX-002). The WiX 5 tool runs on a .NET 6 or
later runtime. `-FailForTest` builds `...-failtest.msi` ("hyperbutler FAILTEST" in Apps),
which installs only with `FAILTESTACK=1` and fails on purpose with
`WIXFAILWHENDEFERRED=1` just before the end of the install, for testing the rollbacks; it
is never a release.

## Known gaps

- English only; the window and the CLI follow your language, the installer does not yet.
- The broker service install (M4) ran from the MSI once (2026-11-03, below), on one
  machine; its steps with a second account (4, 7, 8) have not run yet.
- Tested end to end on one machine (below) only with the basic progress window (`/qb`),
  on an account that was already in Hyper-V Administrators and with Virtual Machine
  Platform on. The options page and the DISM step have not run for real yet.
- An upgrade removes the old service and installs the new one; between the two (a few
  seconds) no user can start a VM. A future installer could update the service in place.
- The upgrade's removal of the accounts an earlier installer added to Hyper-V
  Administrators has no rollback: it runs last, just before the install commits.

## Test procedure: the broker service from the MSI (M4)

Run in an elevated PowerShell (each `msiexec` asks UAC once), on a machine with an
earlier MSI without the broker installed (v0.1.0-preview.1), from the repository root.
A plain snapshot MSI is version 0.0.0, which that release refuses as a downgrade: build
the stage with `-Snapshot -Version v0.1.0-preview.N`, whose MSI version is 0.1.0 (the
same-version upgrade a release over a preview makes). `$m` is the new MSI, `$f` the `-FailForTest` build of the same stage,
`$log` a folder for the logs. The stage must hold a guest image that matches this
hyperbutler (since M3 the host sends the VM secret, which an older guest refuses), with
both executables pinned to its `image.json` (`package.ps1 -ImageDir` does both). Every
step: record the outcome here.

The MSIs for the first run (2026-11-03) were built from 02da1c3 with the guest image of
main's guest code (`package.ps1 -ImageDir $env:TEMP\hb-dev\wc\image -Snapshot -Version
v0.1.0-preview.2`, image.json SHA-256 `757f5217…c68d`, which both executables report with
`version --json` and `-version-json`):

```
$d = "$env:TEMP\hb-dev\m4msi\pkg2"; $n = "hyperbutler-v0.1.0-preview.2-windows-amd64-snapshot"
$stage = "$d\$n"; $m = "$d\$n.msi"; $f = "$d\$n-failtest.msi"; $log = "$env:TEMP\hb-dev\m4msi\logs"
mkdir $log -Force
```

0. **Before.** `sc.exe query hyperbutler-broker` (1060: not installed);
   `net localgroup "Hyper-V Administrators"` (note the members);
   `reg query HKLM\SOFTWARE\hyperbutler /s`. As the user, `hyperbutler stop`.
1. **Upgrade from the pre-broker MSI**, with the window (`msiexec /i $m /l*v
   $log\upgrade.log`): the options page shows the broker text and no group box. After it:
   - `sc.exe qc hyperbutler-broker`: `BINARY_PATH_NAME "C:\Program Files\hyperbutler\hyperbutler-broker.exe" run`,
     `START_TYPE AUTO_START (DELAYED)`, `SERVICE_START_NAME LocalSystem`;
     `sc.exe query hyperbutler-broker`: RUNNING; `sc.exe sdshow hyperbutler-broker`: IU has `RP`;
   - `reg query HKLM\SOFTWARE\hyperbutler\Setup /v LastResult`: the last step `ok`
     (`hyperv-admins remove`, which an upgrade runs last);
   - in the log, `HbBrokerInstall` ran after `InstallFiles`, `HbDropHyperVAdmins` before
     `InstallFinalize`, and the old product's removal right after `InstallInitialize`;
   - the group: unchanged (the 0.0.0 install recorded no one, the account was a member
     before).
2. **Start through the service, as the user (not elevated):** `hyperbutler start`, then
   `docker run --rm hello-world`. `hyperbutler logs` shows "booting through the HCS
   access broker on \\.\pipe\hyperbutler-broker". `hyperbutler doctor`: `broker` ok,
   `hyperv-admins` ok (still a member). `hyperbutler stop`.
3. **Start a stopped service as the user:** elevated `sc.exe stop hyperbutler-broker`;
   then as the user `hyperbutler start`: it prints "starting the broker service..." and
   boots through it. `hyperbutler stop`.
4. **A standard user without the group** (the `hbtest` account of the two-account run,
   not in Hyper-V Administrators, signed in, or through `runas /user:hbtest`): `hyperbutler
   start`, `docker run --rm hello-world`, `hyperbutler doctor` (`hyperv-admins` and `hcs`
   are info, "not needed"), `hyperbutler stop`. This is the M4 goal.
5. **A disabled service is an error, and a repair leaves it disabled:** elevated
   `sc.exe stop hyperbutler-broker` and `sc.exe config hyperbutler-broker start=
   disabled`. As `hbtest`: `hyperbutler start` fails at once, saying an administrator
   disabled the service (window: HB-VM-005), and does not try HCS itself; `hyperbutler
   doctor`: `broker` FAIL (disabled). Repair as an administrator (`msiexec /fa $m /l*v
   $log\repair-disabled.log`): `sc.exe qc` still says DISABLED, `LastResult` says
   `broker install: ok, hyperbutler-broker is disabled; left as an administrator set
   it`, `sc.exe sdshow` unchanged. Then elevated `sc.exe config hyperbutler-broker
   start= delayed-auto`.
6. **Repair restores a deleted service, and leaves a running one alone:** elevated
   `sc.exe stop hyperbutler-broker`, `sc.exe delete hyperbutler-broker`; repair
   (`msiexec /fa $m /l*v $log\repair.log`): registered again and running. A second repair
   with it running: the log's `WixQuietExec64:` line says "hyperbutler-broker is installed
   and running" (the step leaves it alone). Its PID still changes: `/fa` rewrites every
   file, the running `hyperbutler-broker.exe` too, so Restart Manager stops the service
   for the copy and starts it again ("RESTART MANAGER: Previously shut down applications
   have been restarted"), as any repair that rewrites the file does; running VMs end with
   it (HB-VM-004).
7. **A standard user's repair does not touch the group:** elevated, give the record an
   account to remove: `& "C:\Program Files\hyperbutler\hyperbutler.exe" setup
   hyperv-admins add --sid <hbtest's SID>` (`wmic useraccount where name='hbtest' get
   sid`). Then as `hbtest`, not elevated: `msiexec /fa $m WIX_UPGRADE_DETECTED=1 /l*v
   $env:TEMP\repair-user.log`: `hbtest` is still in `net localgroup "Hyper-V
   Administrators"`, and the log shows `HbDropHyperVAdmins` skipped (condition false).
   Afterwards, elevated: `... setup hyperv-admins remove --sid <hbtest's SID>`.
8. **A sign-in that is not interactive** (optional; a batch task or ssh may not even read
   the service): a scheduled task set to "Run whether user is logged on or not" without
   a stored password (S4U) that runs `cmd /c hyperbutler start > %TEMP%\s4u.txt 2>&1`
   (and `hyperbutler doctor >> ...`):
   - for `hbtest` (not in the group): the start fails, saying to start hyperbutler from
     an interactive sign-in or have an administrator run `sc.exe start
     hyperbutler-broker`; doctor's `broker` warns the same;
   - for the user (in the group): the start prints a warning that the service does not
     serve this sign-in and boots the VM in its own process; engine.log has the same
     warning; doctor's `broker` is a note. Then `hyperbutler stop` from the task.
9. **Reinstall over itself** (`msiexec /i $m /l*v $log\same.log`): the same MSI file
   has the same product code, so this is a maintenance run, not an upgrade: one product
   in Apps, the service running with the same PID. The same-version upgrade (another
   build of the same version, a new product code) is step 1's: v0.1.0-preview.1 and the
   test build are both 0.1.0.
10. **The test MSI refuses a plain install:** `msiexec /i $f`: the message says it is a
    test build and wants `FAILTESTACK=1`; nothing changes.
11. **Rollback of an upgrade:** `msiexec /i $f FAILTESTACK=1 WIXFAILWHENDEFERRED=1 /l*v
    $log\rollback.log`: the install fails at the end; afterwards the product from step 9
    is still installed (named "hyperbutler", not "hyperbutler FAILTEST"), `sc.exe query
    hyperbutler-broker` shows it RUNNING from `C:\Program
    Files\hyperbutler\hyperbutler-broker.exe`, and `hyperbutler start` as the user works.
    (Rolled back: the new product's service and files, and the old product's removal,
    whose rollback installs its service again.)
12. **Uninstall, with users starting the service meanwhile:** as the user, in another
    window, `while ($true) { sc.exe start hyperbutler-broker | Out-Null; sleep -m 100 }`;
    then `msiexec /x $m /l*v $log\uninstall.log`, then stop the loop. `sc.exe query
    hyperbutler-broker` 1060; no `hyperbutler-broker` source under
    `HKLM\SYSTEM\CurrentControlSet\Services\EventLog\Application`; `C:\Program
    Files\hyperbutler` gone (no file left in use, no restart asked); `HKLM\SOFTWARE\hyperbutler`
    gone; the group as in step 0.
13. **Rollback of a first install:** `msiexec /i $f FAILTESTACK=1 WIXFAILWHENDEFERRED=1
    /l*v $log\rollback-fresh.log`: fails; no service, no folder, no product, and no
    `HKLM\SOFTWARE\hyperbutler` (no `Setup` key with a `LastResult` left behind).
14. **A first install fails without the service:** as the user, hold the service's pipe
    name with a foreground broker from the stage folder: `& $stage\hyperbutler-broker.exe
    -fake-backend -pipe \\.\pipe\hyperbutler-broker` (leave it running). Elevated:
    `msiexec /i $m /l*v $log\fresh-fail.log`: the install ends with an error;
    `HbBrokerInstallFirst` failed in the log (the service could not take its pipe name);
    no service, no `C:\Program Files\hyperbutler`, no product, no
    `HKLM\SOFTWARE\hyperbutler`. Stop the foreground broker.
15. **First install, silent:** `msiexec /i $m /qn /norestart /l*v $log\fresh.log`; then
    steps 2 and 4 again. Uninstall at the end if the machine should go back to how it was
    (then reinstall the old snapshot if it is wanted).

Look in each log for `WixQuietExec64:` lines: they carry `hyperbutler setup`'s output
("installed and started hyperbutler-broker", "is installed and running", "uninstalled").

### Run of 2026-11-03

Windows 11 Home 26200, the MSIs above, `/passive` or `/qn` (every step's result is
from the commands and the log the step names), the user in Hyper-V Administrators, the
CLI with its own `HYPERBUTLER_HOME`:

| Step | Result |
| --- | --- |
| 1 upgrade | Passed: the service as above (RUNNING, `IU` has `RP`), one product (0.1.0), the group unchanged, the old product removed after `InstallInitialize`, `HbBrokerInstall` after `InstallFiles`, `HbDropHyperVAdmins` last ("the record names no such account"). Two findings, below. |
| 2 start | Passed: answered after 1.6 s, `hello-world` ran, `logs` shows the broker line, doctor `broker` ok (v0.1.0-preview.2, protocol 1), stop 1.6 s. |
| 3 stopped service | Passed: "starting the broker service...", then booted through it. |
| 5 disabled | Passed (as the user, not `hbtest`): the start failed in 48 ms with the message above, no HCS of its own; doctor `broker` FAIL (disabled); the repair left it DISABLED with that `LastResult` and the same SDDL. |
| 6 repair | Passed: the deleted service came back, running. The repair with it running restarted it (Restart Manager, above), the step itself left it alone. |
| 9 same MSI | A maintenance run (above); the same-version upgrade is step 1. |
| 10 test MSI | Passed: refused with the test-build message, exit 1603. |
| 11 upgrade rollback | Passed: "hyperbutler" (the same product code) and its service RUNNING from Program Files afterwards, and `hyperbutler start` works. |
| 12 uninstall | Passed, with `sc.exe start` every 100 ms meanwhile: no service, no event log source, no folder, no `HKLM\SOFTWARE\hyperbutler`, no product, no restart asked, the group as before. |
| 13 first-install rollback | Passed: the service was installed and started, then uninstalled by the rollback; nothing left. |
| 14 squatted pipe | Passed: the service stopped at once (could not take its pipe), `HbBrokerInstallFirst` failed the install, nothing left. |
| 15 silent first install | Passed: `LastResult` "broker install: ok", then step 2 again. |
| 4, 7, 8 | Not run yet (they need the `hbtest` account's password). |

Findings, fixed after the run:

- An upgrade left no `LastResult`: its last step, `hyperv-admins remove --all
  --if-broker`, deleted the record as the uninstall's `--all` does. Now only the
  uninstall's does; the upgrade's records its outcome.
- The machine had a development snapshot from before the first tag installed, and the
  preview.1 install over it ended without `hyperbutler.exe` (so its `hyperbutler stop`
  later failed, `0x80070002`): the snapshot's version was a bare commit (`26a483c`), which
  go-winres read as file version 26.0.0.0, newer than preview.1's 0.1.0.0, so Windows
  Installer skipped preview.1's copy ("Disallowing installation of component ... since
  the same component with higher versioned keyfile exists") and then the removal of the
  old product deleted the old one. `package.ps1` now stamps X.Y.Z.0 of a vX.Y.Z version
  and 0.0.0.0 of anything else. Every build since the first tag had 0.1.0.0; a machine
  with such an early snapshot uninstalls it first.

## Test on a real machine (2026-11-02)

Windows 11 Home 26200, snapshot MSI (115.5 MB), `msiexec /i … /qb ADDHYPERVADMIN=1
/norestart`, one UAC prompt each:

- Install: files under `C:\Program Files\hyperbutler`, the image folder readable by
  `NT VIRTUAL MACHINE\Virtual Machines`, two PATH entries at the end, the Start menu
  shortcut. `setup` took the account from `UserSID`, found it already in the group and
  recorded nothing; the DISM step was skipped (the Host Compute Service exists).
- From a new shell: `hyperbutler` and `docker` resolve to the installed copy;
  `hyperbutler start` answered in 1.5 s, `docker run hello-world` and `docker compose
  version` work.
- Upgrade over an older build of the same version: one product left in Apps, `hyperbutler
  stop` ran first, the membership untouched.
- Uninstall: files, PATH entries, shortcut, Apps entry and `HKLM\SOFTWARE\hyperbutler`
  gone; the machine PATH is byte for byte what it was before; the membership, which the
  installer had not made, stays.

The first rounds found three bugs, fixed before the run above: the exe installed under
the name `!(bindpath.stage)hyperbutler.exe`; `hyperbutler start` from Program Files failed
with access denied, because `HcsGrantVmAccess` needs to change the image's DACL (now the
installer lets every VM read the image folder, and the engine goes on when the group can
read it); and uninstalling left `HKLM\SOFTWARE\hyperbutler\Setup` behind.
- Not signed (above).
- The engine of another user signed in at the same time is not stopped before an upgrade;
  Windows then asks to close it or to restart.
- Uninstalling does not remove the sign-in task of `config set autostart true` (run
  `hyperbutler config set autostart false` or `hyperbutler uninstall` first); a leftover
  task points at a missing file and does nothing.
