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
  not a supported way for end users to run hyperbutler. Until the broker ships, the
  installer uses it with the user's consent; see Installer.

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
out the membership it recorded.

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
- Which HvSocket security descriptor fields are honoured on Home, and does guest-initiated
  connect need its own descriptor? Every service now carries the owner-only descriptor in
  the field its direction uses (see "Per-user endpoints and the VM secret (M3)"); whether
  Home enforces them is untested, and the per-VM secret is the barrier if it does not.

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
(`-fake-backend`) or, since M1, the HCS backend (`-hcs-backend`; see "HCS backend (M1)"). It
never installs, registers or starts a service.

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
channel. `--backend=hcs`, the default, is the in-process developer mode of
Implementation decision 5.

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
- The guest-initiated connections (share, plain network link) are protected by their
  bind descriptors only; the guest does not check that the listener knows the secret. A
  later change could have the host listener send it first, for the guest to check.
- Host and guest image must match: an M3 host sends 32 bytes an older guest would pass
  to dockerd, and an M3 guest refuses an older host. They ship together in the MSI.
- Whether Home enforces the HvSocket descriptors is the open question above; the
  two-account plan below tests it.

### Two-account test plan (M3, not run)

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
   B's listener (the share must not mount from B, the network must not come up through B).
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
