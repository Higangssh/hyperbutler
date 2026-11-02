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
   - Each user then gets a per-user endpoint whose ACL allows only that user's SID.
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

## Open questions

- Does HCS accept a virtual service account that is a member of Hyper-V Administrators
  (option C)? This needs an elevated spike.
- Which HvSocket security descriptor fields are honoured on Home, and does guest-initiated
  connect need its own descriptor?

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
format). `cmd/hyperbutler-broker` runs it in the foreground with an in-memory fake backend. It
never installs, registers or starts a service. There is no HCS backend yet.

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
- The only verbs are version, start, stop, status, resize and set-data-location. The
  version verb and the frame header must stay stable across protocol versions.

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
