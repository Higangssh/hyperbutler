# Installing with the MSI

From v0.1.0-preview.1 hyperbutler also ships as `hyperbutler-vX.Y.Z-windows-amd64.msi`, next to the zip.
The zip stays: it needs no installer and no admin rights for itself, but leaves the
prerequisites to you (README, Quick start). The MSI does them in the one UAC prompt of the
install.

## What the installer does

| Step | When | Undone on uninstall |
|---|---|---|
| Copies the release folder to `C:\Program Files\hyperbutler` (the guest image in `image\`) | always | yes |
| Appends that folder and its `clients\` folder to the **end** of the system PATH, so a docker CLI you already have (Docker Desktop's, say) keeps being the one `docker` runs | always | yes |
| Turns on Virtual Machine Platform with DISM and asks for a restart | the box is ticked (default) and the Host Compute Service is missing | no: other programs (WSL, Hyper-V) may use it |
| Adds your account to Hyper-V Administrators ([ADR 0004](adr/0004-hcs-access-broker.md#installer-amendment-2026-11-02)) | the box is ticked (default in the window); applies after you sign out and back in | yes, if the installer added it |
| Leaves the daily update check on, or records that it is off (`HKLM\SOFTWARE\hyperbutler`, `UpdateCheck` = 0), the default for every user who has not set `updateCheck` | the "Check GitHub for a new version" box (ticked by default); an upgrade keeps an earlier off | yes |
| A Start menu shortcut | always | yes |

The options page names the account and says what the group means: every program you run
can control the Hyper-V VMs on this PC, not just hyperbutler. Without it, `hyperbutler
start` needs an elevated shell. If a standard user installs with an administrator's
password at the UAC prompt, it is the standard user who is added. The install folder is
always `C:\Program Files\hyperbutler`.

The installer never touches your data. Images, containers and volumes stay in
`%LOCALAPPDATA%\hyperbutler` through upgrades and uninstalls; `hyperbutler uninstall`
removes them and everything else hyperbutler created, before you uninstall the MSI.

Before an upgrade or uninstall replaces or removes files, the installer runs `hyperbutler
stop` for you, so a running engine shuts its VM down cleanly. That can take up to about a
minute.

## Silent installs

```
msiexec /i hyperbutler-vX.Y.Z-windows-amd64.msi /qn /norestart
msiexec /i hyperbutler-vX.Y.Z-windows-amd64.msi /qn /norestart ADDHYPERVADMIN=1 ENABLEVMP=0
```

Without the window, no one is added to Hyper-V Administrators unless you pass
`ADDHYPERVADMIN=1`; then it is the account the install runs for. Under a deployment tool
that runs as SYSTEM no one is added either: users join the group another way (`net
localgroup`, Intune, Group Policy). `ENABLEVMP=0` leaves Virtual Machine Platform alone.
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

If a step fails, the install still finishes. `HKLM\SOFTWARE\hyperbutler\Setup` keeps the
outcome of the last step (`LastResult`) and the accounts the installer added
(`HyperVAdminsAdded`); `hyperbutler doctor` shows what is still missing.

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

`build-msi.ps1` builds from the folder `package.ps1` staged (the zip's contents), runs the
Windows Installer consistency checks, and holds the MSI to the same 150 MB limit as the zip
(REQ-UX-002). The WiX 5 tool runs on a .NET 6 or later runtime.

## Known gaps

- English only; the window and the CLI follow your language, the installer does not yet.
- `hyperbutler doctor` reports the group membership and Virtual Machine Platform, but not
  yet the installer's `LastResult`.
- Tested end to end on one machine (below) only with the basic progress window (`/qb`),
  on an account that was already in Hyper-V Administrators and with Virtual Machine
  Platform on. The options page, a first-time group add and the DISM step have not run
  for real yet.

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
