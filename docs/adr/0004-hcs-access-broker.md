# 0004. HCS access for non-admin users

- Status: proposed
- Date: 2026-10-30
- Requirements: REQ-PLAT-001, REQ-PLAT-002, REQ-PLAT-003, REQ-DISK-007, REQ-UX-001,
  REQ-UX-002

## Context

[ADR 0002](0002-primary-vm-backend.md) showed that HCS boots a Linux VM on Windows 11 Home in
under a second, but only for callers that are elevated or in the **Hyper-V Administrators**
group (`S-1-5-32-578`). A normal, non-elevated winbox process gets `HCS_E_ACCESS_DENIED`
(`0x8037011B`).

winbox must run day to day without elevation. Enabling Virtual Machine Platform already needs
admin once, so one more admin step at that same moment costs the user little. Every admin step
after that is what REQ-PLAT-003 forbids.

## Options

### A. Add the user to Hyper-V Administrators

The installer adds the user to the group once. After that, winbox calls HCS directly from the
user's process.

- This is the simplest option: no service and no extra protocol.
- Microsoft documents the group as having complete, unrestricted access to all Hyper-V
  features. Its members control VM configuration, which includes which host files a VM may
  open. Every program the user runs inherits that power, not just winbox.
- Group membership tends to outlive the tool that asked for it.

### B. A SYSTEM broker service with a narrow API

A small Windows service is the only winbox component that calls HCS. The user's winbox process
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

The broker runs as a virtual service account (`NT SERVICE\winbox-broker`) that is a member of
Hyper-V Administrators, instead of as SYSTEM. A broker bug then yields Hyper-V Administrators
rights rather than SYSTEM. That is still serious, but smaller and easier to audit.

- It is not yet verified that HCS accepts a virtual service account through group membership.
  This needs an elevated spike, the same one as open question (b) in ADR 0002.

### D. Elevate once per boot and keep the VM alive

A UAC prompt appears on the first `winbox start` after each reboot, and a long-lived elevated
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
   - If the name is already taken, the broker fails loudly and `winbox doctor` reports it
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
   - On start, the broker lists compute systems that carry the winbox owner tag and
     terminates any it does not track. That releases orphaned `vmmem` memory (REQ-MEM-001).
10. **Surface budget instead of a line count.**
    - No network listeners.
    - Dependencies are limited to the Go standard library and `golang.org/x/sys`.
    - Only the verbs above are accepted.
    - Every decoder is fuzzed.
    - Any change to the broker requires an adversarial review.

### Upgrades (REQ-PLAT-003)

The broker is versioned separately from the CLI, and it is kept deliberately small and stable.
- **CLI-only releases:** most releases change only `winbox.exe` and the guest images, and these
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
- winbox now has a privileged component, and its bugs are local privilege escalation bugs.
  The surface budget and review rule exist because of that.
- REQ-UX-002 allows three resident processes. The broker service counts as the "optional
  helper", so no other resident helper may be added.
- Whether a no-service, no-admin path exists (for example through QEMU-WHPX) depends on
  ADR 0002. This ADR does not assume one.
- Option A remains documented as an unsupported manual mode for developers.

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
