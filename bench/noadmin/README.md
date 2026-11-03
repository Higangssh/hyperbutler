# bench/noadmin: cross-user isolation and a standard user's hyperbutler

The release gate's `bench/noadmin` ([docs/release.md](../../docs/release.md): "`bench/net-vpn`,
`bench/coexist` and `bench/noadmin` have run"). It covers, from
[ADR 0006](../../docs/adr/0006-guest-networking.md) "What stays unverified" and
[ADR 0004](../../docs/adr/0004-hcs-access-broker.md) rules 5 and 6 and its testing
obligations:

- per-user enforcement of the HvSocket connect descriptor: another local user's
  AF_HYPERV connects to a VM are refused;
- no other local user can reach another user's VM, engine or Docker API pipe;
- hyperbutler works for a standard user outside Hyper-V Administrators, through the
  broker service the MSI installs.

`run.ps1` drives it; `noadmin.exe` (this folder, Go) makes the probes PowerShell cannot
(AF_HYPERV, named events, `whoami` and token checks). Each check prints PASS or FAIL;
each part writes `owner-results.json` or `intruder-results.json` next to `owner.json`
and exits 1 if any check failed.

## What it checks

**Owner** (`-Role owner`, the main user, whose hyperbutler runs through the broker
service; read through the hyperbutler CLI only, `status --json` and `logs`):

1. hyperbutler runs and its engine booted through the HCS access broker;
2. the owner has a per-user broker endpoint; `owner.json` is written to `-Dir` (default
   `C:\Users\Public\hb-noadmin`, which both users can read): the owner's SID, the Docker
   API pipe, the engine's control events, the VM's runtime id and the owner's broker
   endpoints. Names and ids only; the VM secret never leaves the engine and the broker.
   The broker endpoint's random suffix is not a secret either: it stops a squatter from
   creating the name first, and any user can list `\\.\pipe\`;
3. positive control: the owner opens its Docker API pipe, its broker endpoint and its
   engine's control events, and connects to its VM's Docker relay (2375). So the
   refusals of the second part are about the user, not a wrong name or id.

It also copies `run.ps1` and builds `noadmin.exe` into `-Dir` (with `go`, from this
repository; or pass `-Helper`), so the second account needs neither the repository nor Go.

**Intruder** (`-Role intruder`, a second, standard account, through `runas`):

- (a) a standard user: `whoami /groups` and the token have neither Administrators
  (S-1-5-32-544, not even deny-only) nor Hyper-V Administrators (S-1-5-32-578), the
  token is not elevated, the label is medium. Only SIDs are read, so a localized
  Windows is fine;
- (b) opening the owner's Docker API pipe, broker endpoint(s) and engine control events
  (`stop`, `daemon`, `settings`, opened with the right `hyperbutler stop` needs and never
  set) is refused with **access denied**. A name that does not exist is a FAIL, not a
  pass: it means owner.json is stale (the owner's engine restarted) and proves nothing.
  Creating a server instance of the owner's pipes fails too (squatting);
- (c) `docker -H npipe:////./pipe/<owner's pipe> version` fails with access denied;
- (d) AF_HYPERV connects to the owner's VM on 2375 (Docker relay), 2380 (guest control),
  1024 (network link) and 2385 (share) are refused with WSAEACCES; "no such VM"
  (WSAEADDRNOTAVAIL) or a refused connection is a FAIL. Nothing is sent: a connect
  that succeeds is closed at once;
- (e) its own hyperbutler: `HYPERBUTLER_HOME` and `DOCKER_CONFIG` under its own
  `%TEMP%\hb-noadmin-<id>`, its own Docker API pipe (the owner holds the default),
  `hyperbutler start --backend broker` (the service only, never HCS in its own process),
  the engine log's broker line, `status --json` on its own pipe and a VM id that is not
  the owner's, `docker run --rm hello-world` (or `-ImageTar`, below), `hyperbutler
  stop`, and `hyperbutler uninstall --yes` of the test installation;
- (f) its `hyperbutler doctor --json`: `hcs` info, `hyperv-admins` info, `broker` ok;
- (g) (b) and (d) again after its own VM ran: still refused, and still denied rather
  than missing, so the owner's engine and VM were there throughout.

Nothing binds a network address. Neither user's `%LOCALAPPDATA%\hyperbutler` or
`~\.docker` is written; the owner's is read only through the CLI.

## Running it

Needs the MSI installed (the broker service running), and a second local account that
is a standard user and not in Hyper-V Administrators: the `hbtest` account of the
two-account run ([docs/install.md](../../docs/install.md), M4 step 4). Check with
`net localgroup "Hyper-V Administrators"` and `net localgroup Administrators` as an
administrator.

1. As the owner, with hyperbutler started (`hyperbutler start`, through the service),
   from the repository root:

   ```powershell
   powershell -NoProfile -ExecutionPolicy Bypass -File bench\noadmin\run.ps1 -Role owner
   ```

   If the engine was started with `--debug-pipe`, pass `-DebugPipe <name>` to probe that
   pipe too.

2. From the same desktop, as the second account (`runas` asks for its password, which a
   person types):

   ```powershell
   runas /user:hbtest "powershell -NoProfile -ExecutionPolicy Bypass -NoExit -File C:\Users\Public\hb-noadmin\run.ps1 -Role intruder"
   ```

   For a machine without network, have the owner save an image to the shared folder
   first (`docker save busybox:latest -o C:\Users\Public\hb-noadmin\busybox.tar`) and
   add `-ImageTar C:\Users\Public\hb-noadmin\busybox.tar` (`-Image` defaults to
   `busybox:latest`; the container runs `echo` with `--network none`).
   `-Memory` (default `2GiB`) keeps both VMs within the host's memory.

3. Afterwards, as the owner: `hyperbutler status` still running, `docker run --rm
   hello-world` works. Delete `C:\Users\Public\hb-noadmin` when done.

Do not restart the owner's engine between the two parts: the engine's events carry its
PID, so a new engine makes (b) report "not found".

## Out of scope

- **HCN for users outside Hyper-V Administrators** (ADR 0006's "whether HCN works for
  users outside Hyper-V Administrators"): the shipped netstack does not use HCN
  (ADR 0006: the guest network is the user-mode stack over hvsock; HNS NAT through
  `internal/hcn` is only a possible opt-in backend that no release ships), so there is
  nothing to run. It matters again only if that backend ships, which needs the broker
  to gain HCN verbs.
- The engine has no control pipe: `hyperbutler stop`, `config daemon apply` and live
  settings reach it through the named events above, which (b) covers, and the Docker
  API pipe is its only pipe (plus the optional debug pipe).
- The wildcard-VM listener (`HV_GUID_WILDCARD` on 1024 and 2385), squatting before the
  broker starts, holding the shared endpoint, the data directory and the broker verbs
  on the shared endpoint: covered by `spike/xuser` and the runs below, not repeated here.
- A sign-in that is not interactive (S4U, ssh): M4 step 8 in docs/install.md.

## Earlier evidence

- **Two-account run, 2026-11-02** (ADR 0004, "Two-account run (2026-11-02)"): with
  `spike/xuser` as `hbtest`, 26 of 28 attempts refused: the owner's per-user pipe
  (access denied), connects to the owner's VM on 2375, 2380, 1024 and 2385 (WSAEACCES at
  connect, so Home enforces the connect descriptors), the data directory and disk. The
  two not refused were listens on the wildcard VM id.
- **Wildcard-listener re-run, 2026-11-03** (same section): after the fix (the host
  listener sends the VM secret first and the guest checks it before writing), the MSI's
  broker service (v0.1.0-preview.3), `xuser -catch 4m` as `hbtest` while the owner
  started the installed hyperbutler five times: every start answered in 1.3 s, the bind
  mount and container network worked, and the wildcard listener got no connection. HCS
  routed every guest-initiated connection to the owner's VM-specific listeners, which
  the engine binds before the VM starts; the secret is the second line. Not run: the
  plain-mode network.
- **M4 step 4, 2026-11-03** ([docs/install.md](../../docs/install.md), "Run of
  2026-11-03"): as `hbtest` through `runas`, medium integrity, not in Hyper-V
  Administrators: `hyperbutler start` answered after 1.5 s, `hello-world` ran, doctor
  `hcs` and `hyperv-admins` info, `broker` ok, stop 1.6 s.

This bench repeats those as one scripted run from the installed MSI, adds the engine's
control events and the docker CLI on the owner's pipe, and checks the owner's side as a
positive control.
