# 0004. HCS access for non-admin users

- Status: proposed
- Date: 2026-10-30
- Requirements: REQ-PLAT-001, REQ-PLAT-002, REQ-PLAT-003, REQ-DISK-007, REQ-UX-001,
  REQ-UX-002

## Context

[ADR 0002](0002-primary-vm-backend.md) showed that HCS boots a Linux VM on Windows 11 Home in
under a second, but only for callers that are elevated or in the **Hyper-V Administrators**
group (`S-1-5-32-578`). A normal, non-elevated hyperbutler process gets `HCS_E_ACCESS_DENIED`
(`0x8037011B`).

hyperbutler must run day to day without elevation. Enabling Virtual Machine Platform already needs
admin once, so one more admin step at that same moment costs the user little. Every admin step
after that is what REQ-PLAT-003 forbids.

## Options

### A. Add the user to Hyper-V Administrators

The installer adds the user to the group once. After that, hyperbutler calls HCS directly from the
user's process.

- This is the simplest option: no service and no extra protocol.
- Microsoft documents the group as having complete, unrestricted access to all Hyper-V
  features. Its members control VM configuration, which includes which host files a VM may
  open. Every program the user runs inherits that power, not just hyperbutler.
- Group membership tends to outlive the tool that asked for it.

### B. A SYSTEM broker service with a narrow API

A small Windows service is the only hyperbutler component that calls HCS. The user's hyperbutler process
talks to it over a local, per-user-authorised channel. The broker accepts only high-level
requests, not raw HCS documents.

- WSL uses this model (`wslservice.exe`, which runs as SYSTEM and hands each user a per-user
  session object). Docker Desktop's `com.docker.service` is similar.
- Docker Desktop's privileged backend has had a series of local privilege escalation CVEs
  (CVE-2020-11492, CVE-2022-25365, CVE-2022-31647, CVE-2022-37326 and CVE-2022-38730). Their
  root causes fall into a few classes: trusting caller-supplied paths, symlink and TOCTOU
  races, and named-pipe squatting or the privileged side connecting to caller-chosen pipes.
  Option B is only acceptable if its design removes those classes of bug.

### C. Like B, but the service runs as a dedicated virtual account

The broker runs as a virtual service account (`NT SERVICE\hyperbutler-broker`) that is a member of
Hyper-V Administrators, instead of as SYSTEM. A broker bug then yields Hyper-V Administrators
rights rather than SYSTEM. That is still serious, but smaller and easier to audit.

- It is not yet verified that HCS accepts a virtual service account through group membership.
  This needs an elevated spike, the same one as open question (b) in ADR 0002.

### D. Elevate once per boot and keep the VM alive

A UAC prompt appears on the first `hyperbutler start` after each reboot, and a long-lived elevated
helper then holds the VM.

- Rejected. It means a recurring UAC prompt, which is the friction this project exists to
  remove. It also leaves an elevated process running with a wider surface than a purpose-built
  service.

### E. Grant access per VM through HCS itself

- Rejected. The `securityDescriptor` parameter of `HcsCreateComputeSystem` is documented as
  reserved, and it must be NULL.
- `HcsGrantVmAccess` exists, but it grants a VM's own account access to a file. It does not
  let an unprivileged caller create VMs.

## Decision

**A broker service (B), run as a virtual account (C) if the spike confirms that works, and as
SYSTEM otherwise.** It follows these design rules.

1. **No caller-controlled host paths.** Requests never carry file paths, kernel command lines,
   share definitions or HCS JSON. The broker builds the HCS document from a fixed template.
2. **Data location by handle, not by path** (REQ-DISK-007).
   - To choose or move the data directory, the caller opens the directory itself and passes
     the handle. The broker duplicates it from the authenticated client process.
   - The broker rejects any target that is a reparse point or on a non-local or non-NTFS
     volume. It also rejects any target not owned by the caller's SID, or whose ACL grants
     anyone other than the caller, SYSTEM and Administrators.
   - Everything below that directory is opened by handle, relative to it, and never by
     re-resolving a path.
3. **Signed guest artifacts.**
   - The kernel and initrd are signed in release CI with an ed25519 key. Its public half is
     compiled into the broker binary.
   - The broker verifies the signature on an open handle and then boots from that verified
     copy. A hash stored next to the file would add nothing.
4. **Identity from the OS, not the message.** The broker identifies the caller from the
   channel by impersonating the client token. It ignores any identity in the request body.
5. **Pipes.**
   - The broker creates its endpoint with `FILE_FLAG_FIRST_PIPE_INSTANCE` at service start.
     The endpoint ACL grants connect only to interactive users.
   - Each user then gets a per-user endpoint whose ACL allows only that user's SID,
     under a name with a random part that the broker picks when it creates the pipe, so
     no one can create the name first (amended 2026-11-02, M3 review).
   - The broker never opens or connects to any pipe or file name it did not create itself.
     That includes VM serial console pipes: these are disabled in release builds, or created
     by the broker with an owner-only ACL.
   - If the name is already taken, the broker fails loudly and `hyperbutler doctor` reports it
     (REQ-UX-003). It does not silently fall back.
6. **Cross-user isolation on hvsock.**
   - Each VM's HvSocket service table sets a connect security descriptor that allows only the
     owning user's SID.
   - As defence in depth, the guest agent and the Docker API proxy require a per-VM secret.
     The broker hands that secret only to the owner.
   - No other local user can reach another user's VM, even if they know its GUID.
7. **Small verb set.** The broker accepts only these requests:
   - start or create the caller's VM
   - stop it
   - query its status
   - resize resources within fixed limits
   - set the data location by handle
   - report the broker version
   - open the caller's own endpoint (rule 5; amended 2026-11-02, M3): served on the
     shared endpoint only, and with version the only verb served there

   Each request is a small, versioned, length-limited message.
8. **Unprivileged data plane.** File sharing, port forwarding and the Docker API proxy run in
   the user's own process over hvsock. They never run in the broker.
9. **VM lifetime.**
   - Each VM is tagged with its owner's SID and created with `ShouldTerminateOnLastHandleClosed`,
     so a broker crash cannot leave a VM running unseen.
   - VMs stop on the owner's logoff, and after a configurable idle time.
   - On start, the broker lists compute systems that carry the hyperbutler owner tag and
     terminates any it does not track. That releases orphaned `vmmem` memory (REQ-MEM-001).
10. **Surface budget instead of a line count.**
    - No network listeners.
    - Dependencies are limited to the Go standard library and `golang.org/x/sys`.
    - Only the verbs above are accepted.
    - Every decoder is fuzzed.
    - Any change to the broker requires an adversarial review.

### Upgrades (REQ-PLAT-003)

The broker is versioned separately from the CLI, and it is kept deliberately small and stable.
- **CLI-only releases:** most releases change only `hyperbutler.exe` and the guest images, and these
  install, upgrade and uninstall as a standard user.
- **Broker releases:** a release that changes the broker needs admin once. The CLI detects the
  version gap and explains it.

So REQ-PLAT-003 must be amended. The one-time setup covers both the Windows features and the
broker service. A broker upgrade is the one documented exception, and it is rare. Uninstalling
the service needs admin. Uninstalling the CLI and its data does not.

### Testing obligations

- The request decoder is fuzzed with Go native fuzzing, with its corpus in `testdata/fuzz/`.
- Integration tests run as a second, non-admin local account and try to reach another user's
  VM, state, data directory and hvsock services. All of these attempts must fail.
- The data-location verb is tested against junctions, symlinks, hard links, network paths and
  ACLs that are too broad.
- Every broker change gets an adversarial review aimed at the CVE classes above.

## Consequences

- Install needs admin once, at the same moment as feature setup. Daily use needs no
  elevation. REQ-PLAT-003 is amended as described in Upgrades.
- hyperbutler now has a privileged component, and its bugs are local privilege escalation bugs.
  The surface budget and review rule exist because of that.
- REQ-UX-002 allows three resident processes. The broker service counts as the "optional
  helper", so no other resident helper may be added.
- Whether a no-service, no-admin path exists (for example through QEMU-WHPX) depends on
  ADR 0002. This ADR does not assume one.
- Option A remains a developer mode. It is documented and used for spikes (ADR 0002), but it is
  not a supported way for end users to run hyperbutler. Until the broker shipped, the
  installer used it with the user's consent; see Installer, and Installer (M4) for the
  switch.

## Installer (amendment, 2026-11-02)

The broker is not built yet, and the clean-account Quick Start run (docs/release.md) showed
what the zip leaves to a new user: an admin shell to enable Virtual Machine Platform, an
admin shell to join Hyper-V Administrators, a sign-out, and editing PATH by hand. From 0.2
the MSI (`dist/msi`, [docs/install.md](../install.md)) does these in the one UAC prompt of
the install, so **option A is the supported interim path**, with these limits:

- The options page explains what the group grants (every program the user runs can control
  Hyper-V VMs), names the account, and has a box for it, ticked by default. An install
  without that page (silent, advertised, install on demand) adds no one unless
  `ADDHYPERVADMIN=1` is given, so no install a standard user can trigger grants it.
- Only the installing user is added: Windows Installer's `UserSID`, and only a user
  account of the machine, an AD domain (`S-1-5-21-…`) or Entra ID (`S-1-12-1-…`), never a
  group or a built-in or service account (`hyperbutler setup`, run as SYSTEM).
- The privileged steps run on a first install or an upgrade only. A repair, which a standard
  user can start on a per-machine product, runs none of them. The install folder is fixed
  to Program Files, because SYSTEM runs hyperbutler.exe from it.
- Every account an install or upgrade added is recorded under
  `HKLM\SOFTWARE\hyperbutler\Setup`; uninstalling takes those out of the group, and a
  rolled-back install only its own. A membership that existed before stays.
- A failed step does not stop the install; `hyperbutler setup` records its outcome there
  (`LastResult`), and `hyperbutler doctor` reports the membership as before.
- The zip keeps working without any of this, as before.

The decision above stands: the broker (B or C) replaces option A, and when it ships the
installer installs the broker instead of adding anyone to the group, and an upgrade takes
out the membership it recorded. Done in M4 (2026-11-03): see "Installer (M4)" under
Implementation notes. The group box is gone from the options page; `ADDHYPERVADMIN=1`
on the command line stays, for developer mode only.

## Implementation decisions (2026-11-02)

Taken by the owner before the HCS backend was written:

1. **Account.** The broker runs as SYSTEM first. If the option C spike shows HCS accepts
   `NT SERVICE\hyperbutler-broker` through Hyper-V Administrators, it moves to that account.
2. **Pause and resume become a 7th verb**, owner-scoped: the free-space guard
   (REQ-DISK-004, REQ-DISK-010) pauses the caller's own VM and resumes it. Rule 7's list is
   amended accordingly.
3. **Image trust.** The broker opens the guest image only from the `image\` folder next to
   its own executable (Program Files) and checks `image.json` against the SHA-256 compiled
   into the broker binary, as `hyperbutler.exe` does today. Images and broker ship in the
   same MSI, so a pin is enough for now; rule 3's ed25519 signature replaces it when images
   start to ship apart from the broker. No per-user images in broker mode.
4. **Network configuration** reaches the broker as typed request fields (IPv4 addresses,
   an MTU in a fixed range, flags), which the broker renders into the kernel command line
   itself; no caller text reaches the command line.
5. **The in-process HCS path stays as a developer mode** (`--backend=hcs`), documented as
   option A, for the zip and for spikes.
6. **Lifetime.** The connection that started the VM is its lease: when it closes, the broker
   terminates the VM, as the engine's exit does today.
7. **Release builds open no serial console pipe.** VM exit is detected through the lease,
   status and the guest control channel; the console stays in developer mode.

## Open questions

- Does HCS accept a virtual service account that is a member of Hyper-V Administrators
  (option C)? This needs an elevated spike.
- Answered 2026-11-02 (two-account run, below): Home enforces the connect descriptors. A
  standard user's connects to another user's VM on 2375, 2380, 1024 and 2385 are refused
  by the OS (WSAEACCES) before the guest sees them. A wildcard-VM listener
  (`HV_GUID_WILDCARD`) on 1024 and 2385 is allowed for any user; whether HCS routes a
  guest-initiated connection to it before the owner's engine binds is untested. Since
  2026-11-03 the guest checks the listener's secret, so such a listener gets no data
  either way. Re-run 2026-11-03 (below): with B listening on the wildcard id, five
  starts of A's VM routed no connection to B.

## Adversarial review

The first review (2026-10-30) raised eight findings. All eight were accepted:

1. **High. Conflict with REQ-PLAT-003 and REQ-DISK-007.** Fixed by the data-location-by-handle
   rule 2 and the Upgrades section. REQ-PLAT-003 must be amended.
2. **High. Cross-user isolation was asserted, not designed.** Fixed by rule 6: an HvSocket
   connect descriptor plus a per-VM secret.
3. **Medium. The pipe rule left a squatting denial of service open, plus connects to
   caller-named pipes.** Fixed by rule 5: `FILE_FLAG_FIRST_PIPE_INSTANCE`, a per-user ACL, and
   no connects to names the broker did not create.
4. **Medium. Missing options.** Added C (virtual account) and D (elevate per boot, rejected).
5. **Medium. VM lifetime was undefined.** Fixed by rule 9.
6. **Medium. The QEMU fallback overclaimed.** Removed. It is now a dependency on ADR 0002.
7. **Low. Unsourced claims.** The unreferenced escalation claim was removed, and the
   line-count target was replaced by a surface budget (rule 10).
8. **Low. "Signature or hash" was underspecified.** Fixed by rule 3: ed25519 with the public
   key embedded in the broker.

## Implementation notes

The first implementation of the broker's protocol and request handling is in
`internal/broker` (server, handle validation, named pipe) and `internal/broker/proto` (wire
format). `cmd/hyperbutler-broker` runs it in the foreground with an in-memory fake backend
(`-fake-backend`) or, since M1, the HCS backend (`-hcs-backend`; see "HCS backend (M1)").
Since M2 it also installs itself as a Windows service and runs as one (see "Service (M2)").

### Wire format

- Each frame is `uint16` big-endian length, version byte (1), type byte, then TLV fields
  (`tag u8`, `size u8`, value). A frame is at most 512 bytes.
- Every type has a fixed schema. Decoding rejects all of the following:
  - unknown versions, types and tags;
  - repeated or out-of-order tags;
  - wrong value sizes;
  - missing required fields;
  - optional fields encoded with their zero value;
  - non-printable strings;
  - a length that disagrees with the input, which covers trailing bytes.
- Decoding is therefore canonical: re-encoding an accepted frame gives the same bytes. The
  fuzzers check this.
- The only verbs are version, start, stop, status, resize, set-data-location, (since
  M1, Implementation decision 2) pause, whose optional `Resume` flag resumes, and (since
  M3, rule 5) open-endpoint. The version
  verb and the frame header must stay stable across protocol versions. The additions of M1
  keep version 1, because no broker was released before them.
- A start carries, all optional: CPUs and memory (zero keeps the caller's values; the
  server checks its limits and remembers them), feature bits (network, share, daemon.json,
  blank data disk), and with the network bit the subnet's IPv4 address (masked, RFC 1918),
  its prefix length (8..29), the MTU (576..65520) and network flags (offload, host
  gateway, CRC off or strict, the CRC flags only with offload and never both). Network
  fields without the network bit are rejected. An optional number encoded as zero is
  rejected for every type. The broker renders these with `internal/vmtemplate`, the
  template the in-process engine uses too, so no caller text reaches the kernel command
  line (Implementation decision 4).
- Status replies can say paused. Errors added in M1: image rejected, disk rejected, lease
  held.
- M3 adds, still in version 1: the open-endpoint verb (no fields; its reply carries the
  pipe name, 1..128 printable ASCII bytes, required), an optional 32-byte secret in
  status replies (never all zero, and only with a running or paused VM and its id), and
  the errors wrong endpoint and endpoint squatted. The fuzzers cover both, with corpus
  entries in `testdata/fuzz/`.

### Identity and state

- The caller is the user SID of the pipe client's token. The broker reads it by
  impersonating after the first frame, because a pipe cannot be impersonated before data
  has been read from it.
- Anonymous, AppContainer, restricted and below-medium-integrity tokens are rejected.
- Clients must connect at impersonation level. go-winio's default dial is anonymous and is
  rejected.
- State is keyed by SID. Each user may have at most 2 concurrent connections
  (`MaxConnsPerUser`).

### Data location

1. The handle is duplicated from the client process. That process is opened while
   impersonating the caller, its token user must be the caller, and the pipe's client PID
   is checked again under the connection's lock.
2. The object type must be `File`.
3. The broker opens the same object again, relative to the handle with an empty name:
   - as the caller;
   - with `FILE_DIRECTORY_FILE`, `FILE_OPEN_REPARSE_POINT` and `OBJ_DONT_REPARSE`;
   - with an anonymous security QoS.

   So a pipe or device handle is never opened, and so never connected to. The broker's
   handle has a private file object, so pending synchronous I/O on the caller's handle
   cannot stall it. Both cases are tested.
4. `ValidateDataDir` then checks rule 2 from the handle:
   - device type disk and not remote;
   - NTFS;
   - a directory and not a reparse point;
   - the owner is the caller;
   - every allow ACE is for the caller, SYSTEM or Administrators. An inherit-only ACE may
     also be for CREATOR OWNER. Deny ACEs are allowed, and any other ACE type is rejected.
5. The check runs again on every start.
6. The whole take-and-check is bounded by a 10-second timeout, because a remote or
   provider-backed file system may never answer.

### HCS backend (M1)

`internal/broker/hcs_windows.go` is the broker's only HCS user. Its HCS calls are
HcsCreateComputeSystem, HcsStartComputeSystem, HcsGetComputeSystemProperties (runtime id
and state), HcsPauseComputeSystem, HcsResumeComputeSystem, HcsShutDownComputeSystem,
HcsTerminateComputeSystem, HcsCloseComputeSystem, HcsEnumerateComputeSystems and
HcsOpenComputeSystem (the startup sweep), and HcsGrantVmAccess on a probe file of its own
(below). For each start:

1. **Image.** Only `image\image.json` next to the broker executable is opened, with the
   kernel, initramfs and system disk it names, and image.json checked against the SHA-256
   pin compiled in (`internal/imageset`, the code `hyperbutler.exe` uses;
   `-X ...imageset.pinnedManifest` pins both binaries, and `dist/package.ps1` builds both).
   It is verified once, at the first start, and held deny-write for the broker's life, so
   no start hashes it again and nothing can change it in between. A build without a pin
   refuses every start unless `-allow-unpinned-image` is given, which only a development
   build (version `dev`, no pin) accepts.
2. **Data disk.** `data.vhdx` is opened relative to the validated data location handle,
   with `OBJ_DONT_REPARSE`, `FILE_OPEN_REPARSE_POINT` and `FILE_NON_DIRECTORY_FILE`, and
   must be a regular file on a local disk, not a reparse point, with one link, owned by
   the caller. The handle shares read and write but not delete, and keeps read access:
   a handle without data access takes no part in share-mode checks, so it would not stop
   a rename. HCS gets the handle's `GetFinalPathNameByHandle` path. The open is bounded
   (10 s) like the data-location check: the caller could stall it with an oplock on its
   own file or a ProjFS directory, and an abandoned open closes its handle when it returns.
3. **Account.** The VM's runtime id comes from `vm.ProbeVMAccount`: HcsGrantVmAccess on
   a new file in the broker's private state directory, which also tells the access mask
   HCS grants. That mask, for `vm.AccountSID(runtimeID)`, is set on the data disk's
   handle (removing entries of VM accounts a crash left: the disk is the user's alone).
   The system disk, shared by every user's VM, gets read access for the account on a
   handle reopened once with `ReOpenFile`, unless the installer lets every VM read it,
   when it is not ACLed at all; only entries of VM accounts the broker does not run are
   cleaned up there, and each release removes only its own. A VM whose runtime id differs
   from the probe's is terminated.
4. **Document.** `vmtemplate.Build` with the request's typed fields, the files above, the
   owner tag `hyperbutler-broker/<SID>` as HCS Owner, every service descriptor for the
   caller's SID only, and no console pipe (Implementation decision 7).

The connection whose start created the VM holds its lease: when it closes, the server
terminates the VM. A start from another connection of the same user gets "lease held"; a
forced stop ends the lease. Pause, resume, status and stop are owner-scoped. Status asks
HCS for the state and releases a VM that stopped on its own. Resizing a running VM is not
supported yet (the new size applies at the next start). Each owner's operations take
that owner's lock only, so a start held up by HCS or a slow file delays no other user and
no lease release; the request's context is checked before the uncancellable HCS calls.
At startup the foreground `-hcs-backend` terminates the compute systems with its own
user's owner tag that it does not track (another user's broker may run theirs); on exit
it terminates its own.

The client (`internal/broker/client_windows.go`) dials at impersonation level only after
checking that the pipe's owner is SYSTEM, a service SID (`S-1-5-80-...`) or the user
itself, since a squatter's pipe could otherwise impersonate the user; since M3 it does so
on the shared endpoint and then on the user's own (see below). In
`hyperbutler start --broker PIPE` (developer mode, not in the usage text; PIPE is the
shared endpoint) the engine
creates the data disk as before, passes the data directory by handle, starts with the
network as the stack runs it, polls the status every 10 s (which also keeps the lease
inside the idle timeout), pauses and resumes through the broker for the free-space guard,
and dials the VM's hvsock services by the returned runtime id. It opens no console pipe:
the VM's exit is learned from the status, a lost connection or the guest control
channel. `--backend=hcs`, the default until M4 (since then `auto`, see "Installer
(M4)"), is the in-process developer mode of Implementation decision 5.

Still to check in the integration run (not run yet, see `hcs_integration_test.go`):

- HCS attaches the data disk while the broker holds it with read access and without
  `FILE_SHARE_DELETE`, and the probe's access mask is what the VM worker needs;
- the system disk needs only read access when the image folder is not VM-readable;
- `GetSecurityInfo` on the client's pipe handle reads the owner of the broker's pipe
  when the broker is SYSTEM;
- a lease closed by killing the engine terminates the VM, and the sweep finds a VM left
  by a killed broker.

### Pipe

- All instances are created up front, the first with `FILE_FLAG_FIRST_PIPE_INSTANCE`.
- The pipe is byte mode, overlapped, and rejects remote clients.
- The DACL is protected. The broker account has full access; the client SID, or IU for a
  shared endpoint, gets `0x12019b`, which is read and write without
  `FILE_CREATE_PIPE_INSTANCE`.
- If the name is taken, the error is `ErrPipeExists`, and the broker does not fall back to
  another name.

### Service (M2)

`internal/broker/svc_windows.go` (service handler, event log, `QueryService`) and
`svcinstall_windows.go` (install, uninstall), driven by `hyperbutler-broker install`,
`uninstall` and `run`.

**Status: verified on 2026-11-02** (Windows 11 Home 10.0.26200, elevated). The service
itself (install, the configuration, the pipe, a restart after a crash, uninstall) passed
`TestServiceLifecycle`. The HCS backend under the service's restricted token (the two
privileges above) passed `TestServiceHCS`: HCS, HcsGrantVmAccess and the data directory
by handle work for it, and it started and stopped a VM. Both left no service behind.

- **Registration.** `install` (elevated) creates the service `hyperbutler-broker`
  ("hyperbutler broker"): own process, automatic start, delayed (the broker is needed only
  once a user starts hyperbutler), LocalSystem (Implementation decision 1), service SID type
  unrestricted (so the token carries `NT SERVICE\hyperbutler-broker`, which option C builds
  on). The command line is `"<exe>" run`, the executable always quoted, and the only other
  tokens are bare flags. The executable path may not hold `%`, since the SCM keeps the
  command line as `REG_EXPAND_SZ`. It then starts the service and waits for it to run.
- **Where the binary lies.** The service runs the file as SYSTEM, so `install` refuses an
  executable that anyone but SYSTEM, Administrators or TrustedInstaller could replace
  (`CheckServiceBinaryPath`):
  - it must be on a local fixed drive;
  - the file, its folder and every folder above must not be reparse points, and must be
    owned by one of those three;
  - no allow ACE that applies to the file or its folder may give anyone else a write
    right: data, append (on a folder: adding a file, such as a DLL the broker would load),
    EA, delete, delete child, DACL, owner, generic write or generic all;
  - on the folders above, no one else may delete, delete children, or change the DACL or
    owner (adding a folder to `C:\` is harmless);
  - deny ACEs are not credited, and ACE types other than allow and deny are refused.

  Program Files passes, a user's folder does not. The MSI installs into Program Files
  (docs/install.md, M4). `InstallService` runs the check itself, so no caller can skip it;
  if a step fails, it undoes the earlier ones and reports any undo that failed too.
- **Privileges.** The token keeps only `SeImpersonatePrivilege` (to identify callers and
  act as them: rule 4 and the data-location reopen) and `SeChangeNotifyPrivilege`
  (traverse checking, which the SCM keeps anyway). The SCM removes the rest of
  LocalSystem's: debug, TCB, backup, restore, take ownership, load driver and the like.
  HCS access comes from the account, not from a privilege; that HCS needs nothing more
  from a service token is still to be checked (below).
- **Recovery.** Restart after 5 s, then after 30 s, then no action; the failure count
  resets after a day. The actions also apply to a stop with a nonzero exit code (a failed
  start, or the server stopping on its own), not only to a crash.
- **Running.** `run` refuses unless the SCM started the process (`svc.IsWindowsService`).
  It reports StartPending, with a 60 s wait hint and a new checkpoint every 2 s, while it
  builds:
  - the HCS backend; its state directory is created in `%SystemRoot%\SystemTemp` with a
    DACL for SYSTEM only;
  - the server, for every caller the pipe admits;
  - `\\.\pipe\hyperbutler-broker` with 16 instances, `FILE_FLAG_FIRST_PIPE_INSTANCE` and
    the IU ACL above;
  - then, only once the pipe is its own, the startup sweep, which covers every user's
    broker owner tag, since the service is the only broker on the machine. A broker that
    cannot take the name touches no VM: the holder may be a broker still running its own.

  Running, it accepts Stop and Shutdown only. The event log opens before anything else,
  so a bad flag on the command line is logged as a failed start (code 1).
- **Stopping.** Stop and Shutdown report StopPending (30 s hint, a checkpoint every 2 s)
  while the listener closes. That ends every connection and with it every lease, so the
  leased VMs are terminated; then the server closes, and the backend terminates the VMs
  left and closes the image. A Stop exits with code 0. A start that fails or a broker
  that stops serving on its own exits with a service-specific code (1 start failed, 2
  pipe name taken, 3 stopped serving), so the recovery actions apply.
- **Hard limits.** A backend that hangs (an HCS call that never returns, a start in
  flight) cannot keep the service pending: a start is given up after 3 minutes (code 1),
  and a stop after 2 minutes (code 3). The service logs it and the process exits; the VMs
  whose handles it held end with it (`ShouldTerminateOnLastHandleClosed`, rule 9). Since
  code 3 is a failure, the recovery actions may then start the service again.
- **Pipe taken.** If the name exists at start, the service logs an error that names the
  pipe and says it does not fall back, and exits with code 2. `hyperbutler doctor`
  reports it: its `broker` check says another program holds the name when the pipe exists
  while the service is not installed or stopped.
- **Logging.** The service writes only to the Windows Event Log, source
  `hyperbutler-broker` in the Application log. `install` registers the source, with
  `EventCreate.exe` as its message file (event IDs 1 info, 2 warning, 3 error), and
  `uninstall` removes it. The service writes no file. Messages pass through
  `FormatEvent`:
  - valid UTF-8;
  - control characters other than tab and newline replaced;
  - `%%` broken up, since the event viewer would expand it as a parameter insertion;
  - at most 4096 characters.

  Info and warning events are capped at 30 a minute, with one note of how many were
  dropped, so a client that makes the server log errors cannot flood the log. The
  service's own lifecycle events (serving, stopped) and errors are not capped.
- **Uninstall.** `uninstall` (elevated) stops the service, waits for it to stop (the stop
  limit and 15 s), deletes it and removes the event log source. A service that is not
  installed is not an error. The SCM removes a deleted service only once its process has
  ended. If it has not ended, `uninstall` says the service is marked for deletion and how
  to finish it (end the process, or restart Windows). `install` says the same instead of
  "already installed" when it finds the service in that state.
- **Doctor.** `hyperbutler doctor` and the window's check list have a `broker` check. It
  says one of: not installed (a note: start runs the VM in its own process), installed but
  stopped, starting or stopping, running but not answering, running with a protocol older
  than the CLI's minimum (`proto.Version`), or running with its version. When the pipe
  exists but the service does not run, it reads the pipe's owner (opened for
  `READ_CONTROL` only, at anonymous level). If the owner is the user, the pipe belongs to
  a foreground `hyperbutler-broker` the user runs (a note). Otherwise another program
  holds the name (a warning). It reads the
  service with `SC_MANAGER_CONNECT` and `SERVICE_QUERY_STATUS`, which every user has, and
  checks the pipe by listing it. It connects only while the SCM reports the service
  running, through `broker.Dial`, which checks the pipe's owner first. Since M4 an older
  protocol is a failure and a missing service a warning for users who cannot use HCS
  themselves (see "Installer (M4)").
- **Engine.** `hyperbutler start --broker \\.\pipe\hyperbutler-broker` boots through the
  service. Since M4 that is the default whenever the service is installed.
- **Development builds.** A development build (version `dev`, no image pin) also accepts
  `-fake-backend` and `-allow-unpinned-image` for `install`, which passes them to `run`,
  and for `run`. They are not in the usage text, and a release build refuses both.

The service integration test (`cmd/hyperbutler-broker/service_integration_test.go`, tag
`integration`, `HYPERBUTLER_TEST_BROKER_SERVICE=1`, elevated) makes no HCS call. It:

1. refuses to run if the service is installed;
2. builds the broker into a new folder under `%ProgramData%` whose DACL admits only
   SYSTEM and Administrators to change it;
3. checks that a copy in the user's temporary folder is refused;
4. installs it with the fake backend, and checks the configuration and the event log
   source;
5. connects with the client (version and status), and checks that the pipe name cannot
   be taken;
6. kills the process, waits for the SCM to restart it, and connects again;
7. uninstalls, and checks that the service, the source and the pipe are gone.

Run it from the repository root in an elevated PowerShell:

```
$env:HYPERBUTLER_TEST_BROKER_SERVICE = "1"
go test -tags integration -run TestServiceLifecycle -count=1 -v ./cmd/hyperbutler-broker/
```

`TestServiceHCS` (`service_hcs_integration_test.go`, same tag) does the same with the
real HCS backend. It boots the image set of `HYPERBUTLER_TEST_IMAGE_DIR`, copied into the
image folder next to the broker and booted with `-allow-unpinned-image` (a development
build). It starts one VM through the pipe and stops it: no network, no console, and a
new 64 GiB sparse, blank data disk in a private directory passed by handle. Then it
uninstalls. The service's startup sweep terminates every broker-tagged VM on the
machine, so run it when no other broker runs VMs:

```
$env:HYPERBUTLER_TEST_BROKER_SERVICE_HCS = "1"
$env:HYPERBUTLER_TEST_IMAGE_DIR = "<folder with image.json>"
go test -tags integration -run TestServiceHCS -count=1 -v ./cmd/hyperbutler-broker/
```

Still to check with the service:

- a second, non-admin interactive user on the shared pipe (the cross-user tests below);
- the service at shutdown and at a user's sign-out with a VM running.

### Testing

- Unit tests cover:
  - junctions, both opened as themselves and followed to a system directory;
  - symlinks (skipped without Developer Mode);
  - hard links;
  - wide and NULL DACLs;
  - an owner that is not the caller;
  - pipe, event and invalid handles;
  - a non-disk device (the NPFS root);
  - pipe squatting;
  - cross-user isolation, using a simulated `Peer`;
  - that the reopen impersonates and validation does not.
- Fuzz targets: `FuzzDecodeRequest`, `FuzzDecodeReply`, `FuzzReadFrame`, `FuzzServeConn`.
- `go test -race` was not run, because the dev machine has no C compiler.

### Still needs an elevated or multi-account test

- Running as SYSTEM or the virtual account against a second, non-admin user:
  - the pipe ACL denies that user, and denies FILE_CREATE_PIPE_INSTANCE to the client;
  - the reopen-as-caller denies directories the caller cannot open;
  - that user's tokens and handles are refused.
- Network shares (SMB) and ProjFS or cloud-filter directories, against the real timeout.
- Option C (the virtual account).

### Per-user endpoints and the VM secret (M3)

Rules 5 and 6, implemented 2026-11-02 in `internal/broker` (`endpoints_windows.go`,
`server.go`, `client_windows.go`), `internal/vmsecret`, `internal/vmtemplate` and the
guest's agent, hyperbutlerd and netfwd.

**Endpoints.**

- The shared endpoint (`\\.\pipe\hyperbutler-broker`, IU may connect) serves two verbs:
  version and open-endpoint. Every other verb gets "wrong endpoint" there, so no VM state
  and no secret ever travels on it; the server also refuses to write a reply with a
  secret on it, as a last check.
- Why a verb and not a longer handshake: the version reply must stay stable across
  protocol versions (rule 7, Wire format), and a verb without fields, answered only on
  the shared endpoint, adds the least: one schema, one reply field, two codes.
- open-endpoint creates, on first use, `\\.\pipe\hyperbutler-broker-<hash>-<random>`,
  where `<hash>` is the hex of the first 16 bytes of SHA-256 over the caller's SID in its
  canonical string form and `<random>` 16 hex digits from `crypto/rand`, new for each
  endpoint the broker creates, with `FILE_FLAG_FIRST_PIPE_INSTANCE` and the protected
  DACL of "Pipe" below (the broker account full access, that SID `0x12019b`, nobody
  else). The reply names it. With a name that depended on the SID only, another user
  could create it before the broker and deny the owner for good; now the name is not
  known before the broker creates it.
- On a per-user endpoint the caller's token SID must be the endpoint's SID (checked after
  the first frame, before any verb runs, the version verb included), every VM verb is
  served, open-endpoint gets "wrong endpoint", and status replies of a running VM carry
  its secret.
- If the new name exists already (a collision, which a squatter can only reach by
  guessing 64 random bits), the request fails with "endpoint squatted" and is logged;
  nothing is created under another name for that request, and the next request draws a
  new one. `hyperbutler doctor` should report the code (M2).
- At most 64 per-user endpoints (`EndpointsConfig.MaxUsers`). At the cap, endpoints with
  no identified connection and no running VM are retired (their users get a new one,
  under a new name, when they ask again); if none is idle, "busy". The per-user
  connection cap (`MaxConnsPerUser`) counts each endpoint separately.
- The shared endpoint must stay reachable while another user tries to hold it:
  - 16 instances;
  - the first frame must arrive within 3 s (`FirstFrameTimeout`, apart from the 30 s
    `IdleTimeout` between requests);
  - before the first frame, connections are capped per user of the client process
    (`MaxConnsPerUser`, by the process token; a counting key only, never an identity);
  - a connection refused before its first frame, or silent past its deadline, gives the
    instance back at once instead of lingering.
  One user can then hold at most `MaxConnsPerUser` instances, for 3 s at a time; several
  users acting together can still keep the instances busy, as on any shared pipe.
- The client connects to the shared endpoint, asks, accepts only a name of its own form
  (`ValidUserPipeName`: its SID's hash and a 16-digit lowercase hex suffix), connects
  there, and checks that both pipes have a trusted owner and the same one. It caches no
  name. A "busy" reply or a dropped connection on the shared endpoint is retried until
  the caller's context ends. The engine's broker provider uses it unchanged.
- `cmd/hyperbutler-broker` must serve through `broker.ListenEndpoints(srv,
  broker.EndpointsConfig{Name: pipe})` and `Endpoints.Serve` / `Close` instead of
  `ListenPipe` and `Server.Serve`; a client of M3 refuses an M1 broker's single pipe.

**hvsock descriptors.** Every service in the VM's table allows the owner's SID only
(`vm.UserOnlySDDL`), in the field its direction uses, and the defaults deny everyone:

| Service | Who connects | Connect | Bind |
|---|---|---|---|
| Docker API (2375) | host to guest | owner | deny |
| guest control (2380) | host to guest | owner | deny |
| network link (1024), offload | host to guest | owner | owner |
| network link (1024), plain | guest to host | deny | owner |
| share (2385) | guest to host | deny | owner |

Where the guest connects to a host listener, the bind descriptor is what keeps another
user from listening there in the owner's place. A test checks every shape of VM.

**Secret.**

- 32 bytes from `crypto/rand` per start: the broker's HCS backend makes one in each
  `Start`, the in-process developer mode in each boot, so both paths are the same.
- vmtemplate renders it from a typed `[32]byte` as `hyperbutler.secret=<64 hex digits>`,
  right after the hash table sizes; a zero secret, or a developer command line that names
  the key, is refused. The guest refuses a command line with the key twice.
- The secret returns only in the owner's own endpoint's start, status, resize and pause
  replies (tag 19). `vmsecret.Secret` prints as `[redacted]` under every fmt verb, so it
  cannot reach a log through a struct.
- The host sends it as the first 32 bytes of every hvsock connection it makes to the
  guest (`transport.HvsockTarget.Secret`, which every dial of the Docker API, the control
  channel and the offload link carries).
- The guest agent (Docker API), hyperbutlerd (control channel) and netfwd -vnet (offload
  link) read exactly 32 bytes within 1 s (the host writes them in the dial call itself),
  compare in constant time, and close a connection that fails without writing a byte.
  Connections are checked concurrently through a bounded `vmsecret.Gate` (agent 16,
  hyperbutlerd 4, netfwd 4 at a time; one over the bound is closed at once), so a peer
  that connects and stalls delays neither the host nor the accept loop. hyperbutlerd
  admits a connection before it may replace the current one, so a peer without the
  secret cannot end the host's session. A guest whose command line has no usable secret
  refuses every connection.
- The guest-initiated services run the other way round (2026-11-03): the host's
  listener writes the secret right after it accepts (`transport.SendSecret`, from
  `shareService.serve` for the share and `guestNet.servePlain` for the plain network
  link), before anything else; the share's HELLO and netfwd's frames come from the guest
  after it, so neither framing changes past those 32 bytes. hyperbutlerd's share relay
  and netfwd's plain mode read exactly 32 bytes within 1 s (`vmsecret.Verify`, the
  client-side `Admit`), compare in constant time, and close a listener that fails
  without writing a byte to it. Then they dial again after their backoff (share relay
  100 ms doubling to 5 s, netfwd 50 ms doubling to 2 s), so a listener that is not the
  host cannot make the guest spin. The share mounts only when every queue's connection
  passed. A guest without a usable secret mounts nothing and brings up no plain link;
  a host without one (it never has none: both providers refuse a zero secret) closes
  each connection having written nothing. The host's accept loops back off at most
  250 ms after a failed accept (`maxAcceptBackoff`, tested to stay well below the
  guest's 1 s), so a sleeping listener still sends the secret in time. A connection the
  guest closes without a byte (it refused the secret: a mismatched image, or no
  `hyperbutler.secret`) is logged once as a warning with that hint, then at debug.
- `vmsecret.Secret` hides itself from fmt only where fmt calls its methods: in exported
  fields. The two holders that keep one in an unexported field (`hcsVM`,
  `hostSessions`) implement `fmt.Formatter` themselves.
- The guest init sets `kernel.dmesg_restrict=1`, so only CAP_SYSLOG reads the kernel log
  (which echoes the command line); docker containers lack it by default. A console log
  that carries the line is redacted in diagnostics bundles (`internal/redact`, tested).

**Limits.**

- `/proc/cmdline` is readable inside the guest, containers included. The secret guards
  against other host users, not against the owner's own containers, which could leak it;
  a leaked secret matters only where the descriptors are not enforced.
- The guest-initiated connections (share, plain network link) were protected by their
  bind descriptors only, which do not cover a wildcard VM id. Fixed 2026-11-03: the host
  listener sends the secret first and the guest checks it (Secret above). Another user's
  wildcard listener can still accept the guest's connection in the window before the
  owner's engine listens (the guest then waits up to 1 s per attempt and backs off),
  which delays the share and the plain link but serves nothing through it.
- Host and guest image must match: an M3 host sends 32 bytes an older guest would pass
  to dockerd, and an M3 guest refuses an older host. The same holds for the listener's
  secret: a host from 2026-11-03 on sends 32 bytes that an older guest reads as a share
  reply or a network frame (the share fails to mount, the plain link drops and redials),
  and a newer guest waits 1 s for an older host's secret and refuses it. Each way fails
  closed. They ship together in the MSI; the guest image must be rebuilt with the change
  (CI builds it), and a dev host needs the image of the same commit.
- Home enforces the HvSocket connect descriptors (two-account run below). It does not
  stop another user from listening on a wildcard VM id for the guest-initiated services;
  the secret check above is what keeps such a listener from being served.

### Two-account run (2026-11-02)

Windows 11 Home 10.0.26200. The broker installed as the service (a dev build, with
`-allow-unpinned-image`), A (`user`, in Hyper-V Administrators) running
`hyperbutler start --broker \\.\pipe\hyperbutler-broker` with a container, B (`hbtest`,
a standard user, removed from Hyper-V Administrators for the run) running
`spike/xuser` through `runas`. 26 of 28 attempts were refused; A's container and a new
`docker run` worked throughout and after.

- Refused: B's status and stop reach only B's own (empty) endpoint; opening A's per-user
  pipe and adding an instance to it (access denied); connects to A's VM on 2375, 2380,
  1024 and 2385 with nothing, a zero secret, 31 bytes and a Docker request (WSAEACCES at
  connect, so the guest's secret check was never reached); listening on A's VM id for
  1024 and 2385 (address in use: A's engine holds them; whether the bind descriptor would
  refuse B with them free is not shown); listing A's data directory and opening its disk.
- **Not refused (fixed 2026-11-03, re-run the same day):** B can listen on the wildcard VM
  id for 1024 and 2385. While A's engine holds its own VM-specific listeners, those win;
  in the window before they exist (A's engine starting or restarting while the VM runs),
  A's guest might connect to B's listener, which could then serve A's share or sit in
  A's network path. Fixed: the host's listener sends the VM secret first and the guest
  checks it before it writes (Secret, above), so B's listener may get a connection but
  never a byte, and the share never mounts from it. The listen itself stays allowed.
  Re-run, with a guest image built from the fix:
  1. As B, `xuser -catch 3m -vmid <A's runtime id>`: it listens on the wildcard id for
     1024 and 2385, answers each connection alternately with nothing and with 32 random
     bytes, and prints one line per connection with what the guest wrote.
  2. As A, during those 3 minutes, start `hyperbutler start --broker ...` several times
     (the window is from the VM's start to the engine's listeners, about a second of
     guest boot), with the default offload network and once with a plain-mode network
     (`hyperbutler-dev -net-offload=false`, which makes netfwd dial the host on 1024).
  3. Pass: every connection line is PASS (the guest wrote 0 bytes), and after each start
     A's bind mount (`docker run -v C:\...`) and network (`docker run alpine wget`) work.
     If no connection arrives, HCS did not route to the wildcard listener in the window;
     record that, as it answers the open question above.
  Re-run 2026-11-03, the MSI's broker service (v0.1.0-preview.3), B = `hbtest` through
  `runas`, `xuser -catch 4m`; A started the installed `hyperbutler` five times in the
  window (offload network, the default): each start answered in 1.3 s, the bind mount
  read back a new token and `wget` from a container worked; B's listener got no
  connection ("no connection arrived", 0 not refused). So HCS routed every
  guest-initiated connection to A's VM-specific listeners, which A's engine binds before
  the VM starts; the secret is the second line. Not run: the plain-mode network (no
  `hyperbutler` flag selects it; `hyperbutler-dev` has no broker mode).
- Not run: steps 1.5 and 1.6 (squatting before the broker starts, holding the shared
  endpoint) and step 3's VM restart.

### Two-account test plan (M3)

Machine with the broker installed (SYSTEM service, or `hyperbutler-broker -hcs-backend`
run elevated), two local accounts: A (the owner, may be admin) and B (standard user, not
in Hyper-V Administrators, not an administrator). A runs `hyperbutler start --broker
\\.\pipe\hyperbutler-broker`; record A's VM runtime id (`hyperbutler status` or the
broker log) and A's per-user pipe name. Every step below runs as B, unelevated, and must
fail; record the error for each.

1. Pipes:
   1. Open A's per-user pipe for read/write (`CreateFile`, impersonation QoS): access
      denied.
   2. Create a server instance of A's per-user pipe (`CreateNamedPipe` on the name):
      access denied; and of the shared pipe: access denied.
   3. On the shared endpoint, send status, start and stop: each gets "wrong endpoint"
      and no reply carries a VM id or a secret.
   4. Open-endpoint as B: B gets B's own name, never A's.
   5. Before the broker starts (reboot or service restart), create as B every name of
      A's form B can think of (A's hash with suffixes of A's earlier runs, all zeros),
      then let A connect: A gets a fresh name and connects. Then, as B, answer
      open-endpoint from a pipe of B's own (B cannot create the shared pipe): A's client
      must refuse it (owner check).
   6. As B, open 16 connections to the shared pipe and send nothing, reopening each as
      soon as it is dropped: A's `hyperbutler start` still connects within a few
      seconds, and the broker log shows B over its connection limit.
2. hvsock connects, with A's VM id and each service id: `vsock:2375`, `vsock:2380`,
   `vsock:1024` (offload mode), plus a random service id. Each connect must be refused
   by HCS (access denied). On Home record whether it is refused at connect time (the
   descriptor is honoured) or accepted.
3. hvsock binds, with A's VM id: listen on `vsock:1024` (plain mode) and `vsock:2385`,
   and on a wildcard VM id (`HV_GUID_WILDCARD`) for the same services. Each bind must be
   refused; if one succeeds, restart A's VM and check that A's guest never sends data to
   B's listener (the share must not mount from B, the network must not come up through B):
   `xuser -catch` does this for the wildcard id (see the run above).
4. Secret, for each connect of step 2 that HCS accepted (or, to exercise the guest even
   where HCS refuses, as A with a raw dialer that sends no secret):
   1. Send nothing for 2 s: the guest closes the connection, having written nothing.
   2. Send 32 zero bytes, then an HTTP `GET /_ping`: closed, nothing read.
   3. Send the secret of an earlier run of A's VM (A reads it from a status reply with a
      test client on A's own endpoint, then restarts the VM): closed, nothing read.
   4. Send 31 bytes of the right secret and wait: closed after 1 s. Hold 20 such
      connections at once: the ones over the guest's bound are closed at once, and A's
      session is unaffected.
   5. As A, with the right secret: the connection is served (control); this confirms
      the steps above failed for the right reason.
   6. During steps 4.1 to 4.4, A's `hyperbutler` session must stay up: the control
      channel is not replaced and the Docker API keeps answering.
5. State and data: as B, open A's data directory, `data.vhdx` and the broker's state
   directory: access denied for each.
6. Logs: the broker's log, the engine's log and the Event Log contain no secret (search
   for the hex of the secret A's test client read from its status reply).

### Installer (M4)

Implemented 2026-11-03: the MSI installs the service, and `hyperbutler start` uses it by
default. **Status: built and inspected, not run.** The MSI builds with WiX 5 and passes
its consistency checks; its custom action tables were read back and are as below. The
elevated install, upgrade, repair, rollback and uninstall procedure is in
[docs/install.md](../install.md#test-procedure-the-broker-service-from-the-msi-m4-not-run-yet).

**Packaging.** `dist/package.ps1` builds `hyperbutler-broker.exe` next to
`hyperbutler.exe` with the same image pin (`-X ...imageset.pinnedManifest`) and the
release version (`-X main.version`), and checks both with `-version-json`; so a release
broker is not a development build and boots only the pinned image. Both executables
open the one `image\` folder next to them (`ImageDirName`), which the MSI installs once,
with the `ImageFolderAcl` DACL: SYSTEM and Administrators full, Users and every VM
(`S-1-5-83-0`) read; no other writer, so the system disk needs no per-VM ACE. The
install folder inherits Program Files' DACL, which `CheckServiceBinaryPath` accepts
(checked on this machine against MSI-made folders under Program Files, the earlier
hyperbutler snapshot's included).

**Registration: `hyperbutler setup broker install|uninstall`,** a setup verb like the
others (SYSTEM, deferred, `WixQuietExec64`, `Return="ignore"`, outcome in `LastResult`),
rather than running `hyperbutler-broker install` from the MSI or the MSI's own
`ServiceInstall` table:

- it calls the same `broker.InstallService` as `hyperbutler-broker install`, so the
  configuration (delayed automatic start, SID type, the two privileges, the recovery
  actions including non-crash failures, the event source) has one definition, which
  `TestServiceLifecycle` checks; `ServiceInstall` and `MsiServiceConfig` cannot express
  the non-crash recovery flag;
- it takes no path: it registers `hyperbutler-broker.exe` next to the `hyperbutler.exe`
  that runs, and still runs `CheckServiceBinaryPath` on it, also for a service that is
  registered already;
- it is idempotent, which an MSI's install, upgrade and repair all need. A service
  registered as it would register it is left alone if it runs (a repair must not restart
  the broker: that ends every user's VM) or started once if it does not (a service that
  stops again after that has failed; it is not started over and over), and gets its DACL
  again; one registered otherwise (another executable, another account) is removed and
  registered again; one deleted but not yet gone (an open handle, or the upgrade's
  removal of the old product a moment before) is waited for, up to 90 s; one an
  administrator **disabled** is left as it is, DACL included, and `LastResult` says so
  (`ok, ... is disabled; left as an administrator set it`, which doctor does not count as
  a failed step): a repair a standard user starts must not turn it on again. A deleted
  service is registered again: that is what a repair is for;
- `uninstall` deletes the service first and then stops it (`UninstallService`): users may
  start it, and a start between a stop and a delete would leave it running under a
  deleted service; once it is marked for deletion, starts fail. It then waits until the
  SCM has removed it, so its file is free before `RemoveFiles`.

**Sequence** (InstallExecuteSequence, as built):

| Action | Where | Condition |
|---|---|---|
| `RemoveExistingProducts` | after `InstallInitialize` (`MajorUpgrade Schedule="afterInstallInitialize"`, was after `InstallValidate`) | an upgrade |
| `HbBrokerUninstallRollback` (rollback: `setup broker install`), `HbBrokerUninstall` | before `HbRemoveHyperVAdmin`, before `RemoveFiles` | `REMOVE~="ALL"` (uninstall, and the old product's removal in an upgrade) |
| `HbBrokerInstallRollback` (rollback: `setup broker uninstall`) | after `InstallFiles` | `NOT Installed AND NOT REMOVE~="ALL"` |
| `HbBrokerInstallFirst` (`setup broker install`, `Return="check"`) | after it | `NOT Installed AND NOT WIX_UPGRADE_DETECTED`: a first install fails and rolls back without the service |
| `HbBrokerInstall` (the same, `Return="ignore"`) | after it | `NOT REMOVE~="ALL" AND (Installed OR WIX_UPGRADE_DETECTED)`: upgrade and repair go on, the failure in `LastResult` |
| `HbAddHyperVAdmin` and its rollback | after `HbBrokerInstall` | `NOT Installed AND ADDHYPERVADMIN="1"` |
| `HbDropHyperVAdmins` (`setup hyperv-admins remove --all --if-broker`) | before `InstallFinalize` | `NOT Installed AND WIX_UPGRADE_DETECTED AND NOT REMOVE~="ALL" AND NOT ADDHYPERVADMIN="1"` |

`TestMSISequence` (cmd/hyperbutler) reads `hyperbutler.wxs` and holds these conditions,
the schedule, and the rule below that the only SYSTEM step a repair runs is
`HbBrokerInstall`.

- With the removal of the old product inside the transaction, a failed upgrade rolls back
  the new service and files, then the old removal, whose rollback registers the old
  service again from the restored files.
- A failed first install unregisters the service before its files go, and that
  `setup broker uninstall` deletes `HKLM\SOFTWARE\hyperbutler\Setup` (and the empty
  parent) when it names no account, so no `LastResult` stays on a machine without the
  product; the uninstall's does the same. A failed repair leaves the service it found
  (no rollback where the product was installed).
- **Repair runs the broker step.** The installer amendment said a repair (which a standard
  user can start) runs no privileged step. This one only restores what the install put
  there, from the fixed folder, and grants no one anything. It leaves a disabled service
  disabled; the worst a standard user can do with it is have a service an administrator
  deleted or reconfigured (other than disabling it) registered again, which restarts it
  (the one case that ends running VMs). Accepted. Every other SYSTEM step needs `NOT
  Installed` (a first install or an upgrade, which need an administrator) or a removal.
- **Hyper-V Administrators.** The options page lost its box; the page now says what the
  service is. `ADDHYPERVADMIN=1` stays for the command line (developer mode). An upgrade
  takes out the accounts an earlier installer recorded (0.2 ticked the box by default),
  but `--if-broker` makes that a no-op unless the service runs, so a failed broker step
  never also takes away the membership. It runs last because it has no rollback. Its
  condition has `NOT Installed`: `WIX_UPGRADE_DETECTED` is a public property, so without
  it a standard user's repair with `WIX_UPGRADE_DETECTED=1` would have SYSTEM take other
  users out of the group (review, 2026-11-03).
- The uninstall still takes out the recorded accounts (`HbRemoveHyperVAdmin`), after the
  broker step, so the `LastResult` that step writes goes with the key.
- `build-msi.ps1 -FailForTest` adds `util:FailWhenDeferred` (fails with
  `WIXFAILWHENDEFERRED=1`) to a `...-failtest.msi` for the rollback steps of the test
  procedure, named "hyperbutler FAILTEST" and installing only with `FAILTESTACK=1` (a
  launch condition); release builds never have it, and the release workflow refuses it.

**Starting the service as a user.** The delayed automatic start comes a few minutes after
boot, later than a start at sign-in (autostart). So the service's DACL (`serviceSDDL`,
set by `InstallService` and again by `setup broker install` through `SecureService`) is
the SCM's default plus `SERVICE_START` for interactive users (`IU`); stop, pause,
reconfiguration and deletion stay with administrators, and starting a running service
does nothing. `hyperbutler start` starts a stopped service itself
(`broker.RequestServiceStart`) and waits for it, up to 90 s. Unit-tested on the SDDL;
on a live service in the procedure. A token that is neither interactive nor a service
(ssh is a network logon, a batch task) may not even read the service, which only `IU`
and `SU` may query. `hyperbutler start` takes that access denied as "the service does
not serve this sign-in": if the account may call HCS itself (Hyper-V Administrators, or
elevated), the engine runs the VM itself as before the broker, with a warning on the
terminal and in engine.log; otherwise the start fails, telling to start hyperbutler from
a desktop sign-in or to have an administrator run `sc.exe start`. Doctor's `broker`
check says the same (a note with own HCS access, else a warning). A disabled service is
not started at all, and start says nothing about starting it.

**Default backend.** `hyperbutler start --backend auto|broker|hcs`, `auto` by default, and
the window, autostart, restart and the MCP server go through the same start:

- `auto`: the service if it is installed, after starting it when it is stopped or waiting
  while it starts or stops. A service that is installed but cannot be used (disabled, a
  start this user may not make, stopped again, not running within 90 s, an unreadable
  state) fails the start, nothing spawned, with the reason and a pointer to doctor; the
  start records it in state.json as `broker-unavailable`, so the window shows HB-VM-005.
  Only without a service (not installed) does the engine run the VM itself (option A);
  the reason goes to the engine log ("not using the broker service", with why), and to
  the terminal when the user cannot use HCS (not elevated and not in Hyper-V
  Administrators). An `--image-dir` other than the image folder next to
  `hyperbutler.exe` also means the engine process: the broker boots only its own image.
- `broker`: the service only; without it too, an error.
- `hcs`: the engine process (developer mode, Implementation decision 5); excludes
  `--broker`.
- `--broker PIPE` (developer mode) is unchanged.

An installed service that fails is never left for the in-process path (owner's decision,
2026-11-03): an administrator installed it to be used, rule 5 says the client does not
silently fall back, and a user without the group could not use the other path anyway.

**Errors.** The broker provider asks for the version first (on the user's own endpoint)
and codes its failures, which `hyperbutler start` explains (`engine.Describe`) and the
window maps (en and ko):

| Engine code | GUI | When |
|---|---|---|
| `broker-unavailable` | HB-VM-005 | no pipe, a timeout, a lost connection; and, recorded by start before any engine, an installed service it cannot use (disabled, a start refused, stopped again, a sign-in it does not serve). One code with general steps (the English original says why, doctor says what to do) rather than one per cause: the fix differs per cause, and doctor has each |
| `broker-refused` | HB-VM-006 | an error reply: caller, data directory, data disk, image, lease |
| `broker-squatted` | HB-VM-007 | a pipe whose owner is not the broker (`broker.ErrUntrustedPipe`), a name not of the user's form, or "endpoint squatted" |
| `broker-old` | HB-VM-008 | a protocol older than `proto.Version`, or "unsupported version" |

HB-VM-001 (HCS access denied, the in-process path) now says to install with the MSI
first. `CleanupOrphans` no longer warns when HCS refuses the caller: a user without the
group has no VMs of their own to clean, the broker sweeps them.

**Doctor.** The `broker` check: not installed is a warning when the user cannot use HCS
(not elevated, not in Hyper-V Administrators: start would fail), a note otherwise; an
older protocol is a failure (start refuses it), and so is a disabled service; a stopped
service says start starts it (`QueryService` reads the start type with
`SERVICE_QUERY_CONFIG`, which users have).
When the service runs and answers, a missing Hyper-V Administrators membership (or one
pending sign-in) and HCS refusing the user's own enumeration are notes ("not needed"),
not failures.

Still open:

- The procedure in docs/install.md (elevated, with the user present).
- Option C (the virtual account) would change the service account; `setup broker
  install` then re-registers a service whose account differs.
- An upgrade replaces the service by delete and create; an in-place update (stop,
  change the configuration, replace the file, start) would keep the gap shorter.

### Review findings (2026-10-30)

1. **Medium. A non-cancellable reopen on a caller-chosen file system held the per-user lock
   and the pipe instance.** Fixed by bounding the check with a timeout. After a timeout the
   abandoned goroutine closes its handle. The pipe peer is closed before the instance is
   reused, so the abandoned goroutine can never impersonate the next client.
2. **Medium. One user could hold every instance of a shared endpoint.** Fixed with
   `MaxConnsPerUser` (default 2). The cap is enforced after identification. Slow clients
   from many distinct users remain bounded only by the instance count. Per-user endpoints
   (rule 5) are the real fix.
3. **Medium. The impersonation around the reopen was not observable in tests.** Fixed with a
   test that asserts the reopen runs while impersonating, validation runs without it, and a
   failed impersonation opens and validates nothing.
4. **Low. `Serve` could be called twice and share an instance's OVERLAPPED.** Fixed: a
   second call returns `ErrAlreadyServing`.
5. **Low. Rule 2 rejects common directories when the broker is SYSTEM.** Not changed. This
   matches the rule as written. Rejected directories include ones owned by Administrators
   after elevated creation, ones that inherit BU or AU from a drive root, and directories
   on Dev Drive (ReFS). The CLI must create the data directory itself with the user as
   owner and a protected DACL. `hyperbutler doctor` must explain each rejection reason. Whether
   to allow ReFS for Dev Drive is an open question.
6. **Low. Untested branches.** Fixed:
   - the not-local-disk branch is tested through the NPFS root;
   - the IU ACL is tested on a live pipe;
   - a persistently failing instance now backs off up to 5 seconds instead of logging every
     100 ms.

   The "not NTFS" branch still has no test, because it needs a FAT or ReFS volume.
7. **Nit. The `MaxHandle` comment was misleading.** Fixed.

### Review of the whole broker (M6, 2026-11-03)

The adversarial reviewer read every part together (the service, the endpoints and the
handle passing, the secret, the client side, `setup`, the MSI and the guest's checks)
against the requirement that a standard user starts, uses and stops only their own VM.
It found no way to SYSTEM or Hyper-V Administrators rights, to another user's VM or
data, or to group or service changes through the MSI. It checked as sound: the handle
duplication and the reopen as the caller; the data directory against a rename while the
disk is held (reproduced: NTFS refuses renaming the parent and the grandparent);
instance squatting; owner identity per connection; where the secret could leak (none
outside the HCS document and the guest's `/proc/cmdline`, which rule 4 accepts); the
lease; the sweep; the MSI's conditions. Its findings:

1. **Medium-high. A host's sleep ended the VM.** Every request read waited at most
   `IdleTimeout` (30 s), and a relative wait expires at once when Windows wakes, before
   the engine's next status request: the lease's connection ended, and the VM with it.
   **Fixed:** the connection that holds the lease reads without a deadline (the client's
   exit closes the pipe and ends it); other connections keep the limit.
   `TestLeaseOutlivesIdleTimeout`. Not yet tried with a real sleep.
2. **Medium. A data folder made before the broker was refused with no fix.** A folder
   from developer mode, or `--data` on a drive whose root lets every user change it,
   inherits other accounts' access (on this machine even `%TEMP%` inherits a security
   product's group), or is owned by Administrators when an elevated shell made it; the
   service refuses it (rule 2) and start said only that. **Fixed:** on that refusal the
   engine runs the same check itself and says why and how to fix it (an `icacls`
   command), and `hyperbutler doctor` has a `data-dir` check that does the same before
   start. `TestCheckBrokerDataDir` runs the fix it prints.
3. **Low. A repair registered a service set to start on demand again**, which ends every
   running VM. **Fixed:** the start type is an administrator's choice; only a disabled
   service is left alone and only another executable, account or SID type registers it
   again. A repair is an administrator's anyway: Windows Installer refuses a standard
   user's before any custom action (error 1730, docs/install.md). Its rewriting of the
   service's DACL stays.
4. **Low. A service that ended without a stop left its state folder** under
   `%SystemRoot%\SystemTemp`. **Fixed:** the service removes those it owns at start
   (plain folders of that name, not links). `TestRemoveStaleStateDirs`.
5. **Low. The data disk's own DACL was not checked**, only its folder's; traverse
   checking is off for every user, so a disk whose ACL its owner widened could be opened
   by name. **Fixed:** the disk's allow ACEs must be for the caller, SYSTEM,
   Administrators or the VM accounts (`S-1-5-83-...`). `TestCheckDiskInfo`.
6. **Info. Rule 10's dependencies:** the broker uses go-winio's `guid` (also through
   `internal/vm`) and, in the client the engine uses, its pipe dial; rule 10 allows only
   the standard library and `x/sys`. Open: the owner decides whether rule 10 admits
   go-winio or the broker replaces both uses.
7. **Info. The sweep runs once, at start.** Accepted: a VM ends with the broker's handle
   (`ShouldTerminateOnLastHandleClosed`).
8. **Info. The hvsock dialer sends no secret when it is zero.** Kept: the bench paths of
   `hyperbutler-dev` dial ports without one, and the engine refuses a VM whose secret is
   zero before it dials.

Measurements still to record: `bench/noadmin` through the installed service, and
REQ-MEM-001 with the broker process counted.
