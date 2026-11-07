# 0009. A sandbox for AI coding agents

- Status: proposed
- Date: 2026-11-05
- Requirements: REQ-SBX-001 to REQ-SBX-009 (proposed below), REQ-PLAT-003, REQ-UX-004,
  REQ-UX-005

## Context

AI coding agents (Codex, Gemini CLI, Aider, editor agents) run shell commands, install
packages and edit files on the user's behalf. On Windows they run those commands as the user,
with the user's whole disk, credentials and network in reach:

- As of 2026-11, most agents sandbox on macOS (Seatbelt) and Linux (bubblewrap, Landlock)
  and on native Windows either run unsandboxed or tell the user to install WSL 2. One uses
  a restricted token, which limits what the process may open but is not a kernel boundary.
- The commercial VM sandbox on Windows is proprietary. Microsoft's isolation work for agents
  is experimental and, as of 2026-11, isolates packaged Windows apps, not Linux toolchains.
- The threat that matters most is not an agent breaking `C:\` by mistake. It is prompt
  injection: a README, issue, web page or package the agent reads tells it to send the
  user's code or keys somewhere. Isolating the file system does not stop that; only control
  of outbound traffic does.

hyperbutler already has most of a VM sandbox: a broker that boots only a hash-pinned image
from a fixed template (ADR 0004), a read-only system disk (ADR 0005), a user-mode network
stack that decides every outbound connection (ADR 0006) and a file server (ADR 0007). What
it lacks is more than one VM per user, a way to bring a project in and changes out, and an
egress policy finer than "routable or not".

Scope: the sandbox runs agents and the commands they start. It is not a general Linux
environment that replaces WSL (a non-goal in [requirements](../requirements.md)), and
hyperbutler still ships no AI assistant of its own.

## Options

### Isolation boundary

1. **A container in the existing Docker VM.** Fastest to build: an agent image, a bind
   mount, `docker run`. But the agent shares a kernel with dockerd and every other
   container, a container escape reaches all of them, and an agent that needs Docker has
   to be given the Docker socket, which is the host's whole engine.
2. **One VM per sandbox session, started by the broker.** A kernel boundary per session.
   Costs one more VM's memory while a session runs, and broker work (below).
3. **Windows Sandbox or another Windows-guest isolation.** A Windows guest, not the Linux
   toolchain agents expect, and Windows Sandbox is not on Windows 11 Home (ADR 0001).

### Where the project lives

1. **Copy in, review the changes, apply on approval.** The engine copies the project into
   the session's disk; the agent works at the guest disk's own speed; at the end the user
   sees a diff and the engine writes the accepted changes back.
2. **Mount `C:\` through the file server.** What `docker run -v` does today. Familiar, and
   edits show in the editor at once, but the agent writes the user's files directly and
   every `npm install` goes through the file server.

### Egress control

1. **None** (today's Docker VM).
2. **An HTTP(S) proxy only**, through `HTTP_PROXY`. Gives the host name for every
   request, but tools that ignore the variable either fail or, if direct traffic is
   allowed, bypass it.
3. **The network stack enforces it**: DNS answers only for allowed names, TCP only to
   addresses those answers produced and only with a matching TLS server name, and an
   explicit proxy for the tools that use one, deciding through the same tables.

## Decision

A VM per session (boundary 2), the project copied in and changes applied on approval
(project 1, decided by the maintainer on 2026-11-05), and egress enforced by the network
stack (egress 3) with three modes, `dev` the default.

### Sessions and disks

- A session is one VM, booted by the broker from a **sandbox image**: its own hash-pinned,
  read-only system disk with the runtimes agents need (git, a shell, Node.js, Python, a C
  toolchain, ripgrep). hyperbutler does not redistribute agent CLIs; the user installs the
  agent inside the session (`npm i -g …`), and it stays on the session disk.
- Each session has its own **new sparse data disk**, formatted by the guest on first boot,
  as the Docker VM's is. There is no differencing disk: a child VHDX names its parent in
  metadata the user can rewrite, which the broker would have to verify on every start,
  and the parent would only have held an empty file system anyway.
- A session is **kept** (stop and resume later, everything on its disk stays) or
  **ephemeral** (`--rm`: deleted when it ends, see REQ-SBX-008). Kept is the default for a
  named session, ephemeral for an unnamed one.
- Running sessions are capped by memory, not only by count: the memory assigned to the
  Docker VM and all sessions together stays under a share of physical memory (a setting),
  a session's default is smaller than the Docker VM's, and free page reporting is on in the
  session kernel as in the Docker VM's. A session idle for a set time is paused.
- The engine runs a **session manager**: it holds each session's broker lease, its
  network stack and its review cache; `hyperbutler sandbox` commands attach to it. Closing
  the terminal detaches; it does not end a kept session. Ending a session asks the guest
  to shut down before the lease is dropped.

### Bringing the project in

- `hyperbutler sandbox run <dir> -- <agent>` copies the working tree of `<dir>` into
  `/workspace`. In a git repository that is the tracked files and the untracked files git
  does not ignore (`.gitignore`, `.git/info/exclude` and the global excludes file). Reparse
  points are never followed: a symbolic link is copied as a link, a junction is skipped
  and listed.
- History comes in as a bundle of the branches and tags only: no reflog, stash, dangling
  objects or `.git/logs`. `.git/config` is not copied; the session gets a minimal config
  with the remotes' URLs stripped of user information. A key removed from history only by
  a later commit is still in the bundle; the docs say so.
- Secrets are kept out by name and by content: files named like secrets (`.env*`, `*.pem`,
  `*.key`, `*.pfx`, `*.p12`, `id_*`, `.npmrc`, `.pypirc`, `.netrc`, `.git-credentials`,
  `*.tfstate`, `*.tfvars`, kubeconfigs, service-account JSON) and files containing known
  token formats or private-key blocks are skipped and listed. `--include` brings a listed
  file in. The scan is a net, not a guarantee.
- The total size is computed first and shown; above a default limit (per session and per
  file) the copy stops and asks.

### Bringing changes out

- Changes come out through a new guest-agent protocol (list, stat, read): host-side
  parsing of guest-controlled input, so it gets the length limits and fuzz tests of
  ADR 0004 rule 10. The host never mounts or parses the session's ext4 itself.
- `hyperbutler sandbox diff` reads the changed files once into the review cache, keyed by
  hash, and shows them. `sandbox apply` writes exactly the bytes the user reviewed from
  that cache and never reads the guest again.
- Everything the review shows (file names, diff text, commit messages) is guest-controlled:
  control characters, bidirectional overrides and other invisible characters are shown
  escaped, and long names are cut at the end with their full length shown.
- Paths are checked against an allowed grammar, not a list of bad patterns: relative,
  `/`-separated segments of 1 to 255 characters from a set of allowed characters, no
  segment `.` or `..`, no trailing dot or space, no device name, no `\`, `:` or drive
  prefix, a total length limit, and no segment that matches an existing 8.3 short name
  in the target. Name collisions are decided by the target directory's own case
  sensitivity and NTFS's upcase rules, not by a Go case fold.
- Every write opens relative to a handle on `<dir>` with reparse points not followed, as
  the broker's data-location check does. A conflict is checked on the open handle (the
  file's hash now against its hash at copy time) and the new content goes to a temporary
  file that replaces the target; a file changed on the host is a conflict, never
  overwritten. Deletions and renames are applied as such; a case-only rename follows the
  target directory's case sensitivity.
- The review lists, ahead of ordinary edits, changes that run code or point tools at a
  server later: `.git/hooks`, `.gitattributes` filters, `.gitmodules`, `.lfsconfig`, CI
  workflows, editor settings, tasks and launch files, package manifests' scripts and
  registry settings, `.envrc`, tool-version files, `Makefile`, test-runner configs,
  `.husky/`, shortcut files and new executables, and new files that look like secrets.
- Changes under `.git` are never applied. The agent's commits come out as a bundle the
  engine verifies (size limit, ref names) and fetches into a branch of `<dir>` itself,
  with LFS and smudge filters off.

### Egress modes

| Mode | Allowed | Is it an exfiltration defence? |
|---|---|---|
| `open` | everything the Docker VM may reach today, LAN and VPN included | no |
| `dev` (default) | a preset of exact host names (package registries, git hosts, container registries, model providers' API hosts) plus what the user allowed | it narrows it: several preset hosts accept uploads from anyone |
| `locked` | the exact API host(s) of the user's model provider, or nothing | the real one |

- **DNS**: a name outside the allowlist is answered NXDOMAIN by the engine and **never
  reaches the host's resolver**, so a query cannot carry data to an outside name server.
  In `dev` and `locked` only A, AAAA and CNAME are answered; other types are refused.
- **Addresses**: a TCP connection is allowed only to an address an allowed answer produced,
  pinned per name and address for a sliding window renewed while connections are open.
  Answers in private, CGNAT and ULA ranges are dropped unless the user allowed that LAN
  entry, so an allowed name cannot be pointed at the router or a VPN host.
- **TLS and HTTP**: on 443 the engine reads the ClientHello and allows the connection only
  if its server name is the allowed name the address was pinned for; no server name, an
  Encrypted ClientHello, or non-TLS traffic on 443 is blocked in `dev` and `locked`. On 80
  the Host header must match; `locked` blocks 80. Other ports need an entry that names them
  (`github.com:22`) and are checked by address only, which the docs say.
- **Proxy**: the explicit proxy for `HTTP_PROXY` users checks the name against the
  allowlist, resolves it, routes each address through the network stack's policy and
  records it in the same pin table; IP-literal requests are refused in `dev` and `locked`.
- **Host and other VMs**: the host gateway (`host.docker.internal`) is off in sessions and
  cannot be turned on in `dev` or `locked`. A session reaches neither another session nor
  the Docker VM except through ports published on the host, and not those in `dev` or
  `locked`. UDP other than DNS and ICMP are not forwarded; QUIC falls back to TCP.
- **The preset** lists exact names only. No wildcard under a zone where anyone can get a
  name (`github.io`, cloud storage, CDN zones); a wildcard the user adds is capped in how
  many distinct names it may resolve per session.

### Asking about a new domain

- A lookup of a name outside the allowlist is held for up to about 25 seconds while the
  user is asked: *allow once* (this session), *allow always* (saved to this user's `dev`
  preset; not offered in `locked`, which only widens per session) or *block*. No answer
  means NXDOMAIN.
- The question shows the mode, the session's name, the name in its ASCII form (punycode,
  never the Unicode look-alike) and how many times it was asked for. Repeated lookups of a
  pending name join the same question.
- There is no "allow all". Past a rate, further names are not asked one by one: they are
  blocked and counted in one line ("N more names blocked, show list"), from which each can
  still be allowed.

### Secrets

- First version: the user names secrets (from Windows Credential Manager) to pass into a
  session as environment variables, sent over the guest control channel, never on the
  kernel command line. The agent can read them, and a kept session's disk may keep them in
  the agent's history; the docs say both.
- Later, behind its own review: the egress proxy adds the credential to requests for the
  host it belongs to, so the secret never enters the guest. That needs the proxy to
  terminate TLS for those hosts with a per-session CA the guest trusts, and is decided
  separately.

### Broker changes

The broker stays the only HCS caller and keeps every ADR 0004 rule. The VM verbs (start,
stop, status, resize, pause) take an optional slot: none is the Docker VM, as every
older client sends, and 1 to 4 a sandbox session. Each session VM has its own VM secret,
lease, size and disk (`session-N.vhdx`, opened by handle below the user's data location
with the same checks as `data.vhdx`); one connection may hold several leases, and closing
it terminates all of them. A session boots only the image in `sandbox-image` next to the
broker, checked against its own compiled-in pin, so a broker built without that pin boots
no session. The decoder and the backend both refuse the file server, dockerd's
configuration and the host gateway for a session. A start or resize that would take one
user's running VMs past a total memory cap is refused. Like every broker change, it gets
fuzz tests on the new messages and an adversarial review before it merges.

The slot change was reviewed on 2026-11-05 (no high findings). Answered: the service
sets the total memory cap to 75% of physical memory (between the Docker VM's 2 GiB
default and the 16 GiB per-VM maximum), and the server refuses a cap below a default VM;
a sandbox image whose system disk is the Docker image's own file is refused, since each
image's access bookkeeping would revoke the other's VM; the slot is checked before any
backend state is made, and the server bounds it again before indexing. Open:

- Release builds carry no sandbox pin until the sandbox image exists, so a release broker
  boots no session (fail closed); `dist/package.ps1` gains the pin with the image.
- A data disk is checked by its metadata only (owner, ACL, links, reparse), not its
  contents, so a differencing VHDX whose parent the user cannot read but VMs can (a file
  readable by the Virtual Machines group, or a share reached as the computer account)
  might be read by the guest. This predates the sessions, which add disks; it is fixed on
  its own, by refusing a data disk whose VHDX metadata says it has a parent.
- A forced stop that fails leaves the slot without a lease while its VM runs, until the
  sweep or a broker restart (as before); and retiring idle endpoints asks the backend
  about five slots under the endpoints' lock. Both are accepted for now.

### Phases

1. Spike: a second VM per user; measure boot time, memory per session and the agent CLIs'
   behaviour on the sandbox image (musl or glibc is decided here).
2. Sessions, copy-in, `diff`, `apply`, `open` and `locked` modes.
3. `dev` mode with the preset and the questions; the GUI's sandbox page.
4. Host-side secret injection, snapshots, Docker inside a session.

### Spike results (2026-11-05)

Phase 1, first part: two VMs of one user at once, booted straight through HCS by
`hyperbutler-dev run` (the maintainer's account is in Hyper-V Administrators; the broker
was not involved), with the v0.1.0 guest image, on the reference laptop.

| Run | dockerd answers `/_ping` |
|---|---|
| one VM | 798 ms, again 777 ms |
| two VMs started together | 803 ms and 742 ms |

| Two idle VMs, user-mode network, 2 GiB, own 16 GiB data disk | 30 s | 90 s | 150 s |
|---|---|---|---|
| `vmmem`, first VM | 213 MiB | 220 MiB | 226 MiB |
| `vmmem`, second VM | 228 MiB | 228 MiB | 224 MiB |
| dev engine process, each | 15–17 MiB | 16–17 MiB | 18 MiB |

- A second VM per user needs nothing new from HCS: separate VM IDs, hvsock services and
  data disks are enough, and starting two at once does not slow either.
- An idle session costs about 240 MiB with dockerd running in it; the sandbox image has no
  dockerd, so this is an upper bound until that image exists. Four sessions and the Docker
  VM would idle near 1.2 GiB.
- Killing the dev process ended its VM (the HCS handle is closed on exit and the VM is
  created to terminate on it): no `vmmem` remained.

Second part: agent CLIs on musl and glibc. In one dev VM, `npm i -g` and then `--version`
and `--help` of each CLI in `node:22-alpine` (musl) and `node:22-bookworm-slim` (glibc):

| CLI | Alpine (musl) | Debian (glibc) |
|---|---|---|
| `@openai/codex` 0.166.0 | installs and runs | installs and runs |
| `@google/gemini-cli` 0.67.0 | installs and runs | installs and runs |

- The agents themselves do not decide the base: both run on either.
- What they install does. Python wheels on PyPI are built for glibc first (`manylinux`;
  `musllinux` wheels are fewer), and many npm packages download glibc-only prebuilt
  binaries (browsers for test runners, native modules). An agent that hits a missing wheel
  compiles from source or fails, which costs the user time and tokens.
- So the sandbox image is to be **glibc-based (Debian slim)**, unlike the Docker VM's
  Alpine image. That adds a second distribution to pin and to cover in the source offer
  (ADR 0005), which is accepted for compatibility. Only `--version` and `--help` were run;
  a real task in each agent comes with the sandbox image.

### Phase 2 progress (2026-11-05)

The parts that need no session VM are in, each with unit tests and, where it parses
guest input, a fuzz target:

| Package | What it does |
|---|---|
| `internal/sandbox/review` | the path grammar (`CheckPath`), escaping for display (`Escape`, `Shorten`), the list of changes shown first (`Classify`), secret file names |
| `internal/sandbox/copyin` | the file list (git's, or a walk), secrets kept out by name and by token format, the tar stream with the hash of each file sent |
| `internal/sandbox/wire` | the list and read protocol on vsock 2390 (copy on 2391): bounded, canonical decoding, a client that never trusts the guest's sizes |
| `internal/sandbox/workspace` | the guest side: unpack through an `os.Root`, the baseline, the change list; portable, so tested on Windows |
| `internal/sandbox/session` | the engine side: copy in, read each change once into the review, refuse what cannot be applied |
| `internal/sandbox/apply` | writes reviewed bytes relative to the project's handle, one name at a time, no links, the normalized name checked (8.3 and case), host edits kept as conflicts, temporary file renamed over the target |
| `guest/cmd/sbxd` | the session's guest agent: host CID and VM secret first, copy once, then serve the change list |

A test runs the whole cycle without a VM: copy in, an agent's edits, list and read
through the wire client, apply; a file the user edited meanwhile stays as a conflict,
and `.git` and `.env` never cross. Still to come in this phase: the engine's session
manager and `hyperbutler sandbox` commands, a terminal for the agent, the diff view, the
git bundle for commits, and the `open` and `locked` egress modes.

### The sandbox image (2026-11-06)

`guest/sandbox/build.ps1` builds it on Windows without WSL, as the Docker image is built:

- The system disk is Debian 13 (trixie) from `mmdebstrap`, in the digest-pinned
  `debian:trixie-slim` container that `guest/sandbox/debian.lock` names, on any Docker
  Engine. The lock pins a snapshot.debian.org timestamp: apt reads the indexes from the
  snapshot only and checks their signatures against the container's keyring, so the
  lock decides every version and hash. The packages themselves come from Debian's own
  mirrors (fast) and from the snapshot only when the mirrors no longer carry a file
  (the snapshot is slow and answers 503 under load); apt checks each against the
  snapshot's indexes either way. `sandbox-packages.tsv` lists what went in: package,
  version, architecture, source package and source version, tab-separated and sorted.
- The kernel and the stage-1 initramfs are the Docker image's: the build checks that the
  kernel's version names the kernel modules it copies in, and writes its own
  `image.json`, so the sandbox image has its own pin (`imageset.SandboxPin`).
- `/sbin/init` runs tini as PID 1 (it reaps what an agent's processes leave behind) and
  `guest/sandbox/init`, which mounts the session disk (formatted only when blank, by the
  Docker VM's rule) for `/workspace`, `/home`, `/usr/local` and `/var/lib/hyperbutler`,
  and starts `sbxd` as the agent user (uid 1000, no capabilities, no new privileges).
  `npm i -g` writes to `/usr/local` on the session disk; the root stays read-only.
- A session VM's hvsock table holds sbxd's two ports and the network link only
  (`vmtemplate.Spec.Sandbox`, which the broker sets for slots 1 to 4): not the Docker API
  or the control channel, which the first broker change still offered to a session.
- `hyperbutler-dev sandbox -image … -project DIR` boots the image in its own process,
  copies the project in and prints the review, until the session manager exists.

First image (snapshot 20261105T000000Z): 565 packages, an 814 MiB tree in a 368 MiB
system disk (lz4hc), built in about 4.5 minutes on the reference laptop. Booted through
HCS with no data disk, the init reaches `sbxd` 0.17 s after stage 1 starts, and this
repository (1,049 files, 7.8 MB, `.env` files kept out) is copied in 1.24 s after the VM
starts. Fixed on the way: a session's dial waited out hvsock's 30 s connect timeout when
it came before sbxd listened (each attempt now gets one second); trixie's essential set
has no `mount`; Debian's kmod needs `depmod`'s binary indexes. The build now checks that
every command the init runs is in the tree. Changes made inside the session wait for
the agent's terminal.

**CI build and source bundle (2026-11-07).** Release images come only from CI (ADR 0005,
Decision 4): `.github/workflows/sandbox-image.yml` checks the guest-image artifact of the
same run against the digest its caller passes, and `scripts/guest-image/build-sandbox.sh`
does build.ps1's steps on the Ubuntu runner, with the same output names. The kernel's
modules come from the guest image's `kernel-modules.tar`, which build.ps1 instead reads from
its cache. The one difference in the container's options is `apparmor=unconfined`:
Docker's default AppArmor profile denies mount even with `SYS_ADMIN`, and the runner
has AppArmor. `ci.yml` and `release.yml` call the workflow; nothing consumes its artifact
yet, so no release carries the image until how it ships is decided. `build-rootfs.sh` now
fixes the modules' modes and times, which came from wherever they were staged: from the
same inputs and Go version, build.ps1 and build-sandbox.sh made the same `system.vhd`, byte
for byte (run on the same machine and engine, 2026-11-07).

With `with-sources`, `scripts/guest-image/sandbox-sources.sh` runs in the same pinned
container, with deb-src lines from the same snapshot and mirror priorities, so apt checks
every file against the snapshot's signed Sources indexes. It downloads every source package
in `sandbox-packages.tsv` at its exact version; a missing version fails the build. It
also holds each `.dsc` to the list (apt's own comparison calls `1.0` and `1.0-0` equal) and
each file to its `.dsc`. The artifact gets `sandbox-sources.tar`, its manifest
`sandbox-sources.tsv` and the release text `SANDBOX-SOURCES.txt`. A tar of 2 GiB or more, the
limit of a GitHub release asset, becomes numbered parts of 2000 MiB
(`sandbox-sources.tar.000`, `.001`, ...; join them with `cat`).

First bundle (snapshot 20261105T000000Z, 2026-11-07): 448 source packages for 566 binary
packages, 2,096 files, 941,281,280 bytes (898 MiB; about 2.4 times the system disk), so
one asset and no parts. It took 8 min 13 s through the reference laptop's engine (939 MB in
7 min 40 s at 2 MB/s), every file from the Debian mirrors and none from the snapshot. The
largest are `linux` 152 MB (`linux-libc-dev`), `node-corepack` 126 MB, `gcc-14` 94 MB and
`npm` 68 MB.

Reviewed on 2026-11-05. Fixed: a `.git` at any depth (a planted `sub/.git` would run
code at the next `git status`) is never applied and never copied; a work tree with no
git installed is refused instead of walked (the walk took ignored files); invisible
letters (U+3164) and Latin mixed with Cyrillic, Greek and similar scripts in one part of
a name are refused; the risky lists match at any depth and cover `desktop.ini`, `.scf`,
script and disk-image extensions, container build files and coding agents' instruction
and settings files; a failed change leaves no directories behind; two spellings of one
directory in a set are a collision; cloud file placeholders are copied, only links
skipped; names from git's index are checked to stay below the project. Open: a
temporary file survives a crash before its rename; volumes without POSIX rename (FAT,
older SMB) fail with a raw status; `Apply` itself is not yet fuzzed against a directory
(REQ-SBX-003 asks for it).

### Egress as built (2026-11-07)

`internal/sandbox/egress` is a `netstack.Egress` hook on the session's stack; `open` has no
hook at all, and the Docker VM's network does not change. The hook narrows and never widens:
an address it allows still goes through the stack's Policy.

Enforced in `dev` and `locked`:

- **DNS**: only A, AAAA and CNAME of a name on the list reach the host's resolver, in lower
  case. Anything else causes no lookup of any kind: NXDOMAIN for those three types, REFUSED
  for the others. The guest subnet's reverse names stay local.
- **Answers**: private, loopback, link-local, CGNAT, ULA and benchmark addresses, and the IPv6
  forms that embed an IPv4 address, are dropped unless the entry says `lan:`. The rest are
  pinned to the name for 5 minutes, renewed by each answer and each accepted connection; 4096
  pins at most, the oldest goes first.
- **Connections**: an unpinned address is refused on every port, which is what stops IP
  literals, DNS over TCP, DoT and DoH to outside servers. Nothing is dialed before the
  decision. On 443 and 80 the handshake with the guest completes first and at most 16 KiB and
  5 seconds of its first bytes are read: the ClientHello's one server name, or `dev`'s one
  Host header, must be a name that address is pinned for, and the bytes then go out unchanged.
  Other ports need an entry that names them, by address only. `host:port` allows that port
  only, so `github.com` and `github.com:22` are two entries.
- **Refused hellos and heads**: not TLS, SSLv2, no server name, several, ECH (`0xfe0d`, its
  drafts and ESNI), any length that does not add up, a second hello right behind the first;
  no Host, several, folded headers, bare LF, absolute-form targets, CONNECT.
- **A second hello**: a server can answer a ClientHello with a HelloRetryRequest, and the
  second hello is in plaintext with a server name of its own. So on 443 nothing else from the
  guest goes out until the server's first bytes are seen, within the dial timeout (10 s by
  default; past it the connection closes, it is not relayed unseen). After a
  HelloRetryRequest the guest's next bytes, a ChangeCipherSpec at most and a hello, pass the
  same checks before they go on. Whatever else the server sends, the client deals with. Port
  80 does not wait.
- **UDP** is never forwarded, so QUIC falls back to TCP.
- **Wildcards** (`*.host`, any depth, not `host` itself, never a zone on the public suffix
  list, ICANN's or the private part, such as `*.co.uk` or `*.github.io`) get 64 distinct names
  per session. The name is counted at the question, because the question is the data channel;
  past the cap it is blocked and counted.
- **Blocked log**: 256 entries by name and kind with a count of the rest, one line per address
  however many ports, and a share of the table for each kind, so a scan does not keep a later
  name out of it. Guest-chosen text is printable ASCII with `\xHH` for everything else, and cut
  to a bounded length.

Known limits:

- Anything inside TLS and anything after the first request on an HTTP connection go unseen: a
  client can send an allowed server name and another Host inside (domain fronting), and DNS
  over HTTPS to an allowed name looks like any HTTPS.
- A connection on a `host:port` port is checked by address only, so it reaches whatever else
  shares that address.
- 64 distinct names of a wildcard reach the host's resolver per session; repeated queries among
  them are not bounded, so the cap limits the names a query can be made of, not how much a
  guest can signal by which of them it asks for and when.
- One allowed name the agent can influence opens 443 and 80 to any public address that name
  answers with, for example through a DNS record of a host it controls, and a server with a
  default virtual host does not care for the server name. Allow only names whose answers you
  trust.
- A ClientHello must not offer ECH. Browsers that send a placeholder ECH extension when they
  have no real key, as Chromium does, are refused on 443 until a placeholder can be told apart
  from the real thing. Clients on Go's `crypto/tls` send none unless configured for ECH.
- Allowed names can upload: `dev` narrows exfiltration, only `locked` defends against it.

The explicit `HTTP_PROXY` proxy of "Egress modes" is not built, because direct connections are
enforced in the stack and a second policy path adds nothing to them. Tools that require a proxy
are not served yet. The question about a new domain (REQ-SBX-006) and `bench/sandbox-egress`
are still to come; the unit tests with a mock resolver cover REQ-SBX-005 up to the host's
network.

## Adversarial review

The first draft was reviewed on 2026-11-05. Each finding and its answer:

- **DNS tunnelling** (high): the engine's DNS forwarder sends every query to the host's
  resolver, so NXDOMAIN to the guest would not stop a query reaching an outside name
  server. Fixed: names outside the allowlist never reach the host resolver; only A, AAAA
  and CNAME are answered.
- **Shared CDN addresses** (high): pinning by address allows any site behind the same CDN
  address as an allowed name. Fixed: the TLS server name (and HTTP Host) must match; no
  server name and ECH are blocked.
- **Wildcards and multi-tenant hosts** (high): a wildcard turns the query name into a data
  channel, and storage and pages hosts accept anyone's uploads. Fixed: exact names only in
  the preset, user wildcards capped; the mode table says `dev` narrows exfiltration and
  only `locked` defends against it.
- **Rebinding to the LAN** (high): an allowed name answering with a private address would
  reach the router or VPN hosts. Fixed: private answers dropped without a LAN entry; pin
  lifetime defined.
- **Review and apply reading the guest twice** (high): a compromised guest could serve
  different bytes to `apply` than to `diff`. Fixed: `apply` writes the reviewed bytes from
  the host cache; the new guest protocol is fuzzed.
- **Deceptive text in the review and questions** (high): control characters, bidi
  overrides and look-alike domains. Fixed: escaped display, punycode names.
- **Windows path forms** (high): 8.3 names, trailing dots, per-directory case sensitivity,
  Unicode case folding, `\` in Linux names, drive-relative paths. Fixed: an allowed path
  grammar and handle-relative opens without reparse.
- **History carries old secrets** (high): reflog, stash, dangling objects and `.git/config`
  (credential helpers, includes, user information in URLs). Fixed: a bundle of branches and
  tags, a generated config, a content scan; the limit that a removed key stays in history
  is documented, and REQ-SBX-004 claims only what the scan does.
- **Differencing disk parents** (high): the parent's location is in a file the user can
  rewrite. Fixed by dropping differencing disks for a new sparse disk per session.
- **Host gateway** (high): a session would reach services on the host's loopback. Fixed:
  off in sessions, impossible in `dev` and `locked`.
- **The proxy as a second policy path, question timing, the code-running file list,
  links and size in copy-in, leases and memory with several VMs, what `--rm` deletes,
  apply races, secrets on the kernel command line** (medium): each answered in the
  sections above.
- Low: ICMP is not forwarded; IPv6 literals and AAAA are in the egress test though the
  stack is IPv4 today; repeated lookups join one question; dated claims carry their date.
  Accepted as a risk: an ARM64 sandbox image multiplies the image work by architecture,
  not by two.

## Consequences

- hyperbutler gains a reason to install it that Docker Desktop and `wslc` do not offer for
  free: a local, open-source VM sandbox for agents on Windows, without WSL.
- The broker grows from one VM per user to several, which widens the code an attacker
  reaches as SYSTEM. That is the main cost, and the reason for the review gate above.
- The engine gains a TLS ClientHello parser, a guest file protocol and a hostile-path
  writer, all parsing untrusted input on the host; each is fuzzed.
- A second guest image adds to the pinning, source-offer and update work of ADR 0005, per
  architecture.
- Revisit if a Windows-native agent sandbox with a kernel boundary ships in the box, or if
  the spike shows a session costs too much memory to run several at once.

## Proposed requirements

| ID | Requirement | Acceptance criterion (how measured) | Pri |
|---|---|---|---|
| REQ-SBX-001 | A session is its own VM. | Two sessions and the Docker VM run at once; a process in one cannot reach another's files or processes, nor reach another session, the Docker VM or the host's services through the host's loopback or LAN addresses (`bench/sandbox`). | P1 |
| REQ-SBX-002 | The project never changes without approval. | After any session, `<dir>` is byte-identical to before until `sandbox apply`; `apply` writes only accepted files, with the bytes shown in the review (unit and VM tests). | P1 |
| REQ-SBX-003 | Apply refuses hostile paths. | Fuzzed change sets with `..`, absolute and drive-relative paths, ADS, device names, trailing dots and spaces, 8.3 names, `\`, Unicode case and normalisation collisions, overlong names and reparse points never write outside `<dir>`, through a link, or over another file (fuzz test). | P1 |
| REQ-SBX-004 | Known secret files stay out by default. | Ignored files, files with the built-in secret names and files matching the built-in token formats never reach the session without `--include`; `.git/config` and the reflog never do (unit test over fixtures). | P1 |
| REQ-SBX-005 | Egress follows the mode. | In `locked` and `dev`, connections and lookups outside the allowlist fail, including by IPv4 and IPv6 literal, DNS over TCP, record types other than A/AAAA/CNAME, QUIC, a different TLS server name to a pinned address, no server name, and an allowed name answering with a private address; a blocked name causes no query at the host's resolver (verified today by unit tests with a mock resolver; `bench/sandbox-egress` is still to come). | P1 |
| REQ-SBX-006 | Unknown domains are asked about or counted, never silently allowed. | A blocked name produces one question or, past the rate, one count in the blocked list; no answer means blocked; there is no allow-all (unit test, GUI test). | P1 |
| REQ-SBX-007 | Sessions are cheap enough to keep several. | Memory per idle session and boot to a shell are measured and published (`bench/sandbox`). *Targets open until the spike.* | P1 |
| REQ-SBX-008 | Ephemeral means gone. | After an `--rm` session ends, its disk, saved state, flow log, review cache and question answers are deleted; only "allow always" entries remain (`hyperbutler uninstall --dry-run` lists none of the rest). | P1 |
| REQ-SBX-009 | The review cannot be faked by text. | File names, diffs, commit messages and domain names with control, bidi or look-alike characters are shown escaped or in punycode (unit test). | P1 |
