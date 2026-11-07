# Settings, daemon.json and --json output

- Date: 2026-10-31
- Requirements: REQ-UX-006, REQ-UX-007, REQ-UX-008, REQ-UX-009 ([ADR 0008](../adr/0008-settings-gui-mcp.md))
- Machine: Windows 11 Home 10.0.26200, non-elevated account in Hyper-V Administrators,
  a 3 GiB / 8 vCPU guest on its own pipe (`\\.\pipe\docker_engine_hb_settings`) with
  `HYPERBUTLER_HOME` and `DOCKER_CONFIG` in a temp directory.
- Code: `internal/config` (the keys), `internal/daemoncfg` (daemon.json checks),
  `internal/engine/daemonjson.go` (delivery), `internal/guestctl` (the transfer),
  `guest/cmd/hyperbutlerd/daemoncfg.go` and `guest/prototype/init` (the guest side),
  `cmd/hyperbutler/config_windows.go` and `schema.go` (the CLI). REQ-UX-009:
  `internal/dockerenv` (proxy checks, the CA set, the document), `internal/wincert` (the
  Windows stores), `internal/engine/dockerdenv.go`, `guest/cmd/hyperbutlerd/dockerdenv.go`,
  `bench/corp-proxy`.

## Settings (REQ-UX-006)

```powershell
hyperbutler config list [--json]
hyperbutler config get KEY [--json]
hyperbutler config set KEY VALUE [--apply]
hyperbutler config unset KEY [--apply]
hyperbutler config path [--json]
hyperbutler restart
```

| Key | Type | Default | Applies |
|---|---|---|---|
| `memory` | size (`4GiB`, or a bare number of MiB) | 4GiB | VM restart |
| `cpus` | 1 to the host's logical CPUs | half the host's, 2 to 8 | VM restart |
| `disk.size` | size, 4GiB to 16TiB, whole MiB | 64GiB | first start (fixed once the disk exists) |
| `data.dir` | a local `X:\` path | the state directory | first start (fixed once the disk exists) |
| `pipe` | `\\.\pipe\NAME` | `\\.\pipe\docker_engine_hyperbutler` | VM restart |
| `context.manage` | bool | true | at once (the next `start`) |
| `network.frameCheck` | bool | true | VM restart (a CRC-32C on every engine-guest frame; off gains a few percent on multi-Gbit/s local paths, ADR 0006 "Integrity") |
| `engine.memoryLimit` | GOMEMLIMIT syntax or `off` | 256MiB | VM restart |
| `hostGateway.enabled` | bool | true | at once (the running engine, no restart) |
| `hostGateway.ports` | port list (`5432,8000-8100`) or `all` | all | at once (the running engine, no restart) |
| `ca.import` | bool | true | dockerd restart (`config daemon apply`, or `--apply`) |
| `ca.extraFile` | a PEM file on a local drive, or empty | empty | dockerd restart |
| `proxy.http`, `proxy.https` | `http://`, `https://` or `socks5://` URL, credentials allowed | none | dockerd restart |
| `proxy.noProxy` | comma-separated hosts, `.domains`, IPs, CIDRs | empty | dockerd restart |
| `autostart` | bool | false | at once: `set` creates or removes the sign-in task |
| `updateCheck` | bool | true | at once (the next `start` or `status`) |
| `gui.startOnOpen` | bool | false | the next time the GUI opens (REQ-UX-016) |
| `gui.language` | `auto`, `en` or `ko` | unset: the Windows display language, and the GUI asks once at its first open | at once in an open GUI (REQ-UX-013) |

- **Reserved names** (refused with "reserved for a later release"): `proxy.mode` (the
  Windows system proxy, 0.2), `tray`, `ports.lan`, `buildkit.gcKeepStorage`.
  (`updates.check` and `autostart` were reserved in 0.1; the update check's key is
  `updateCheck`, as REQ-UX-022 names it.)
- **autostart** (REQ-UX-015, 0.2): `config set autostart true` creates one Task Scheduler
  task of this user, `\hyperbutler autostart (<account>)` in the library's root folder,
  which a standard user may write: a logon trigger for this user only, 30 s after
  sign-in, `LeastPrivilege` with the interactive token, no run time limit, also on
  battery. It runs `%SystemRoot%\System32\conhost.exe --headless "<hyperbutler.exe>"
  start`: `hyperbutler.exe` is a console program, and conhost's headless mode gives it a
  console that is never shown, so nothing flashes at sign-in. The task is made with
  `schtasks.exe /Create /XML` (a UTF-16 task XML written to %TEMP% and removed again),
  not the Task Scheduler COM API, which Go has no binding for without a new dependency;
  `schtasks /Query /XML` reads it back, `/Delete` removes it. The task is made before
  settings.json records the change, so a task that could not be created leaves the
  setting as it was. `config list` shows whether the task exists and whether it starts
  this `hyperbutler.exe` (after a move, `set autostart true` again points it here).
  `false` and `uninstall` remove it. It starts the installation in
  `%LOCALAPPDATA%\hyperbutler` only: with `HYPERBUTLER_HOME` set to another directory,
  `set autostart true` is refused, and `false`, `config list`, `doctor` and `uninstall` leave
  the task alone. The first start of an installation mentions the
  setting once.
- **updateCheck** (REQ-UX-022, 0.2): on by default, as REQ-UX-005 allows (the one request
  hyperbutler makes of its own, with an off switch). At most once a day `start` and
  `status` send one HTTPS GET to
  `https://api.github.com/repos/Higangssh/hyperbutler/releases/latest` with the header
  `User-Agent: hyperbutler/<version>` and nothing else (no cookie, no Accept-Encoding,
  no identifier), a 3 s timeout, `proxy.https` or else this shell's `HTTPS_PROXY` and
  `NO_PROXY`, and the Windows certificate store. A newer release prints one line with its
  page; a development build (no tag, a pseudo-version or `+dirty`) never does. Nothing
  is downloaded. The result is kept in `update-check.json` in the state directory; a
  failure (offline, 404 for no release, a proxy refusing) is silent and waits for the
  next day too, and shows only with `HYPERBUTLER_DEBUG=1`.
- **Source** is `default`, `file` (settings.json) or `env`. `engine.memoryLimit` shows `env`
  when this shell has `GOMEMLIMIT`; the running engine has the environment of the shell
  that started it, which may differ. `data.dir` and `disk.size` always show `file` once
  settings.json exists: they are recorded when it is created, so the data disk stays
  where it is even if a default changes. `start --memory` and the other start flags go
  through the same validators and are saved, so their source is `file` too (there is no
  separate `flag` source).
- **set and unset**: the key's validator runs on a copy, the copy is validated as a whole,
  and settings.json is replaced atomically (temporary file, flush, rename) under the ops
  lock, after reading the file again under that lock. If settings.json is invalid or
  unreadable every command that would write it stops with the parse error; `config path`
  still works, so the file can be found and fixed. The output says what restart the
  change needs; nothing restarts unless `--apply` is given. `--apply` on a VM-restart key
  runs `hyperbutler restart`, which starts the same guest image the engine ran
  (`state.json` records its directory).
- **Strict values**: `data.dir` must resolve to a drive path; path elements that start or end
  with a space or end with a dot are refused, because Windows drops those characters and
  the directory would not be the one named (two fuzz crashers, kept in
  `internal/config/testdata/fuzz/FuzzSet`). `cpus` above the host's logical CPUs is refused,
  because Hyper-V refuses it at start. Reserved device names (`CON`, `NUL`) in `data.dir` are
  not checked yet.
- memoryMB and cpus are now optional in settings.json (0 = default); a new install writes only
  `dataDir` and `diskSizeBytes` unless flags are given. Older files, which name every field,
  stay valid.

## daemon.json (REQ-UX-007)

The file is `%LOCALAPPDATA%\hyperbutler\daemon.json` (under `HYPERBUTLER_HOME` if set;
`config path` shows it). hyperbutler never writes it.

```powershell
hyperbutler config daemon validate [--file FILE]   # the host checks only
hyperbutler config daemon apply                    # restarts dockerd, not the VM
```

### Delivery: the control channel

The system disk stays read-only and verified (ADR 0005), so the file goes to the guest's
`/run` tmpfs. Three ways to get it there were considered:

| Option | For | Against |
|---|---|---|
| The data disk | persistent | the host cannot write ext4 in the VHDX while the VM runs; a change would need a VM restart |
| A kernel argument | simple | size limited (a few KiB), and a change needs a VM restart, which REQ-UX-007 rules out |
| **The guestctl hvsock channel** | already authenticated (host CID only, user-only hvsock ACL), supervised and reconnecting; works while the VM runs | the protocol has fixed 256-byte frames |

The guestctl channel is used. `TDaemonChunk` frames carry up to 232 bytes at an offset,
`TDaemonCommit` the size, SHA-256 and a restart flag, and `RDaemon` answers with a code
(ok, rejected by `dockerd --validate`, bad transfer, rolled back, failed), flags and up to
192 bytes of guest text, which the host treats as untrusted and escapes. 64 KiB is the limit.

- **Boot**: with `hyperbutler.daemoncfg=1` on the kernel command line the init waits (up to
  30 s) after hyperbutlerd is ready until the first delivery has been installed, then
  starts dockerd with `--config-file=/run/hyperbutler/daemon.json` if there is a file. The
  engine sends it right after each control connection; measured, the wait added nothing
  (`daemon-config-ready` at the same 0.38 s as `hyperbutlerd-ready`, ready after 1.1 s).
  A file that arrives after the init gave up is applied with a dockerd restart.
- **Byte for byte**: the bench reads the installed file back through a container
  (`sha256sum` in the guest) and compares it with the host file. CRLF line ends and all.
- **Restart**: hyperbutlerd sends SIGHUP to the init (PID 1), which stops dockerd (its
  containers stop) and starts it again; the VM keeps running. Each dockerd writes
  `N PID` to `/run/hyperbutler/dockerd.gen`. While hyperbutlerd holds
  `/run/hyperbutler/dockerd.hold` a dockerd that exits does not power the VM off. If the
  new dockerd does not answer `/_ping` within 90 s, or exits, hyperbutlerd puts the
  previous file back and restarts dockerd with it (`DaemonRolledBack`). The same watch
  runs at boot: a file that passes `--validate` but keeps dockerd from starting (for
  example `"storage-driver": "zfs"`) is removed, dockerd starts without it, and the
  engine then applies its previous copy with a restart. `start` reports ready only for
  the dockerd that stays. The watch does not end when the control connection does: a
  reconnect during a restart is not a reason to roll back.
- **Previous file**: the engine keeps `daemon.applied.json`, the last file the guest
  accepted. While the user's file is invalid, refused, or rolled back, that copy is in
  effect, across VM restarts too. With no user file the copy is removed and dockerd runs
  with its defaults.
- **State**: `state.json` (and `status --json`) report the delivery: `source` (`none`,
  `file`, `previous`), `sha256`, `bytes`, `restarted`, `error`, `generation` and `applies`.
  `config daemon apply` sets a per-engine named event (user-only DACL, like the stop event)
  and waits for `applies` to change.
- **Old guest images** do not advertise `FeatureDaemonConfig`; the engine says so in the
  state and `start` output, and dockerd runs with its defaults.

### What is checked

On the host (`internal/daemoncfg`, also before `apply` sends anything): one JSON object,
UTF-8 without BOM, at most 64 KiB, no duplicate keys, no owned key, and no `bip`,
`fixed-cidr` or `default-address-pools` base that overlaps the guest network
(192.168.127.0/24). Keys are compared without regard to case, with Unicode simple folding
(K and the Kelvin sign, s and ſ), because dockerd decodes into a Go struct, which matches
keys that way. In the guest, `dockerd --validate` checks everything else before the file is
installed.

Owned keys, and why:

| Key | Why |
|---|---|
| `hosts` | dockerd listens on `/run/docker.sock`, which the guest agent relays to the Windows pipe |
| `iptables`, `ip6tables` | the network is routed through the user-mode stack (ADR 0006); dockerd runs with both off |
| `userland-proxy`, `userland-proxy-path` | with iptables off, `-p` works only through docker-proxy |
| `data-root` | `/var/lib/docker` is the data disk |
| `exec-root`, `pidfile` | runtime state is on the `/run` tmpfs; the root is read-only |
| `containerd` | dockerd starts its own containerd |
| `tls`, `tlsverify`, `tlscacert`, `tlscert`, `tlskey` | the pipe's ACL is the access control; TLS on the guest socket breaks the relay |
| `shutdown-timeout`, `log-level` | passed as flags (dockerd refuses a key that is also a flag); use `"debug": true` |
| `live-restore` | containers left running keep the data disk busy at shutdown, so the VM would power off with a dirty file system (review finding 3) |

| `proxies` | dockerd's proxies are the `proxy.*` settings (REQ-UX-009); a daemon.json value would override them without a word (Docker Desktop 4.35 had that bug, A7 in the pain points) |

### End to end (`bench/daemon-json/run.ps1`, 2026-10-31)

registry:2 runs in a container with `-p 5000:5000 --restart=always`. dockerd reaches it at the
guest's address 192.168.127.2:5000, which is not loopback, so plain HTTP needs
`insecure-registries`. The host sees it at 127.0.0.1:5000 through auto-publish. 39 of 39
checks passed:

| Check | Result |
|---|---|
| push without daemon.json | refused: dockerd wants HTTPS |
| `config daemon apply` with `insecure-registries` | **1.9 s** (2.1–2.3 s in earlier runs); same VM id, same guest `boot_id`; the registry container came back by itself |
| guest file vs host file | same SHA-256 |
| push, `rmi`, pull over HTTP; another container lists `hb/busybox` in `/v2/_catalog` | all worked |
| `hosts`, `Data-Root`, `live-restore` | refused on the host with the reason; nothing sent |
| invalid JSON | refused with the line and column |
| `"no-such-option": true` | refused by `dockerd --validate`; dockerd not restarted |
| `"storage-driver": "zfs"` | dockerd exited, rolled back to the previous file in about 3 s; same VM |
| all of the above | the user's file unchanged, the previous file still in effect, same guest boot |
| `hyperbutler restart` with an invalid file | the previous file in effect after the VM restart (`source: previous`) |
| boot with the zfs file | rolled back at boot, previous file applied with a restart; `start` took 4.4 s instead of 1.1 s and printed why |
| removing daemon.json and applying | dockerd back to defaults |

Not tested: a registry on another machine of the LAN (there is no second machine; `-Registry`
takes one), and a reconnect during an apply in a real guest (unit-tested).

## Corporate CA and proxy (REQ-UX-009)

```powershell
hyperbutler config set proxy.https http://user:password@proxy.corp:3128 --apply
hyperbutler config set proxy.http  http://proxy.corp:3128
hyperbutler config set proxy.noProxy .corp.example,10.0.0.0/8
hyperbutler config set ca.import false          # default on
hyperbutler config set ca.extraFile D:\certs\corp-root.pem
hyperbutler config daemon apply                  # or --apply on the last set
```

### What dockerd gets

- **CA certificates.** With `ca.import` the engine reads the trusted roots of
  `CurrentUser\Root` (the logical store, which includes the machine's), `LocalMachine\Root`
  and its Group Policy and Enterprise (Active Directory) stores, read-only through crypt32
  (`CertOpenStore`, `CertEnumCertificatesInStore`). It never changes a store. It keeps a
  certificate only if it parses, is a CA (basic constraints CA:TRUE, or a self-issued
  version 1 root without basic constraints), is within its validity period, and is not in
  `CurrentUser\Disallowed` or `LocalMachine\Disallowed`. Both matches are by the
  certificate's hash (thumbprint), as Windows matches them: the whole certificate, not its
  key or subject, so a reissued certificate with the same key is not caught; duplicates
  are dropped the same way (SHA-256 of the DER). `ca.extraFile` certificates come first and
  need only parse, but the Disallowed stores apply to them too.
- **`ca.extraFile` is copied when it is set.** `config set ca.extraFile PATH` follows
  every link and junction in the path without opening their targets, refuses a UNC or
  device path, a mapped network drive and a link to any of them, checks the PEM, and
  copies it to `ca-extra.pem` in the state directory (atomically, with a DACL for the
  current user only). settings.json keeps the path as given, for display, and the copy's
  SHA-256. Boot and `config daemon apply` read only the copy, so the original is never read
  then (it can be gone, or on a share that hangs). After the file changes, set it again.
  If settings.json was edited by hand to name another file, the copy is still used and
  `state.daemonConfig.env.error` says it is not the one recorded; without a copy, nothing is
  read and the error says to set it again. `unset` removes the copy, and so does
  `uninstall`.
  Intermediate stores (`CA`) are not read: an entry there is a cache, not a trust
  decision, and Go would treat it as a trust anchor. On this machine: 70 certificates in
  the stores, 29 kept (29 duplicates, 12 expired), 7 ms; 18 of the 29 are not in Alpine's
  bundle (Microsoft's own roots and other Windows-only CAs).
- **Bounds.** At most 20000 entries are read per store, 16 KiB per certificate, 2000
  certificates and 960 KiB of PEM in total; what is over the limit is left out and counted
  (`caStats.overLimit`, with a message in `env.error`), never a reason to fail the start.
- **The guest** writes `/run/hyperbutler/ca-certificates.crt`: the stock bundle as it is,
  then the certificates it lacks. The init starts dockerd with
  `SSL_CERT_FILE=/run/hyperbutler/ca-certificates.crt`. dockerd, its containerd and
  BuildKit (which dockerd embeds) use Go's crypto/x509, which reads that file instead of the
  stock one; per-registry certificates in `/etc/docker/certs.d` are not used (the system
  disk is read-only).
- **Proxies** become `HTTP_PROXY`, `HTTPS_PROXY` and `NO_PROXY` in dockerd's environment.
  Go's `http.ProxyFromEnvironment`, which dockerd's registry client uses, supports http,
  https and socks5 proxies, so those are the schemes accepted. The URL must be
  `scheme://[user[:password]@]host[:port]`, nothing else; it is checked when it is set
  (unit and fuzz tests).
- **NO_PROXY** is the user's list plus what a proxy elsewhere can never reach:
  `localhost,127.0.0.1,::1,192.168.127.0/24,host.docker.internal,gateway.docker.internal,192.168.127.254`
  and the hosts of daemon.json's `insecure-registries` and `registry-mirrors`. Only when a
  proxy is set. The user's list is at most 4096 bytes; the list dockerd gets may be 1 KiB
  longer, so the defaults always fit with it, and registries that would not fit are left
  out, counted in `env.error`.
- **A proxy on this host's loopback** (a local NTLM or Kerberos relay such as px or cntlm,
  which is how such proxies are used without Docker Desktop's Business-only support) is
  rewritten to the host gateway address, `192.168.127.254`, which forwards to Windows'
  `127.0.0.1` (REQ-NET-004). `config set proxy.*` and `hostGateway.*` refuse a combination
  where the host gateway is off or does not forward the proxy's port. Only `localhost` and
  `127.0.0.1` are rewritten: the gateway connects to `127.0.0.1` and falls back to `[::1]`
  only when that refuses, so a proxy given as `[::1]` or another `127.x` address would get
  whatever listens on `127.0.0.1` with that port, credentials included. `config set`
  refuses those ("bind the relay to 127.0.0.1"); a relay that listens on `[::1]` only
  still works as `localhost`.
- **Containers and builds** are not changed, as in Docker Desktop: its docs say to add a
  corporate CA to an image and run `update-ca-certificates`, and its proxy settings are for
  pulls, not for what runs in a container. To give containers and `docker build` a proxy,
  the docker CLI's own `proxies` in `~/.docker/config.json` still works; certificates still
  go into the image. `proxy.mode` (the WinINET/WinHTTP system proxy, and PAC files) is
  reserved for 0.2: reading it is small, but a PAC file needs a JavaScript evaluator, and
  following the system proxy live means restarting dockerd when it changes.

### Why the environment and not daemon.json's `proxies`

dockerd has a `proxies` key since 23.0. Using it would mean hyperbutler editing the
user's daemon.json, which REQ-UX-007 delivers byte for byte, or merging two files with
rules about which wins. The environment is dockerd's oldest proxy interface and needs no
merge; to keep one source of truth, `proxies` is now an owned daemon.json key (refused
with a pointer to `config set proxy.*`). The CA bundle has no daemon.json key at all.

### Delivery

The proxies and the certificates travel as one document (`dockerenv.Doc`, JSON, at most
1 MiB) over the control channel with the framing of daemon.json: `TEnvChunk` frames (232
bytes at an offset, their own buffer in the guest, so the two transfers can interleave),
an `TEnvCommit` with size and SHA-256, and an `REnv` reply with the daemon.json codes.
Guest images advertise `FeatureDockerdEnv`; for an older image the state says to update
it and dockerd runs without. hyperbutlerd decodes the document strictly (known fields,
valid values, canonical bytes), writes `dockerd.env` (KEY=VALUE lines) and the bundle,
0600 in a 0700 `/run/hyperbutler`, and the init exports only `HTTP_PROXY`, `HTTPS_PROXY`,
`NO_PROXY` and `SSL_CERT_FILE` from it, without evaluating the file as shell code.

- **Boot:** the engine builds the document before the first control connection is used
  and sends it before daemon.json, so the init's one readiness gate (`daemon.ready`,
  which the daemon.json commit creates) covers both, and `start` waits for both. On the
  VM: `dockerd environment: installed` at 0.38 s, `daemon-config-ready` at 0.40 s, and the
  first dockerd (generation 1) had `SSL_CERT_FILE` and the proxy. Right after that gate
  (or its 30 s timeout) the init creates `/run/hyperbutler/dockerd.starting`; from then on
  hyperbutlerd treats a missing `dockerd.gen` as a dockerd being started, not one yet to
  start: it waits for its generation and restarts it for a changed daemon.json or
  environment, which a dockerd that had read the files already would otherwise miss.
- **Restarts only when asked.** A changed environment on a running dockerd restarts it
  (never reported as in effect without), so only `config daemon apply` and `config set
  ... --apply` build a new one. On a control reconnect (sleep and resume, a hyperbutlerd
  restart) the engine sends what is in effect, not a new import: the stores change on
  their own (AuthRoot fills on demand, Group Policy refreshes), and a restart stops every
  container. What the build reported about that environment (a certificate file or store
  that could not be read) is reported again; only a delivery's own error is cleared.
  An apply restarts dockerd once: for the environment if it changed, otherwise
  for daemon.json; twice only when both changed.
- **Rollback:** the environment shares daemon.json's lock, hold file and 90 s watch in the
  guest; a dockerd that does not come back is restarted with the previous environment
  (`DaemonRolledBack`). At boot there is no previous environment to go back to; see the
  review below.

### Secrets

Proxy URLs can carry a password. It is stored in settings.json (in the user-only state
directory, like the rest) and sent to the guest; everywhere else it is
`user:***@host` (a user name alone, often a token, is `***@host`): `config list/get`
(unless `--show-secrets`), `config set` output, state.json and `status --json`
(`state.daemonConfig.env`), `status`, and the engine log. Validation errors never quote a
proxy URL. hyperbutlerd logs only which variables are set. Not on the kernel command line.
`docker info` shows dockerd's own masked form (`xxxxx:xxxxx@`).

### State

`state.daemonConfig.env` (additive within schema 1): `httpProxy`, `httpsProxy` (redacted,
as the guest sees them), `noProxy`, `caImport`, `caCerts`, `caSkipped`, `caStats`
(`added`, `duplicate`, `notCA`, `expired`, `notYetValid`, `invalid`, `disallowed`,
`overLimit`), `sha256`, `restarted`, `error`. `status` prints a `proxy:` line.

### End to end (`bench/corp-proxy`, 2026-11-01)

The bench runs its own TLS-inspecting proxy on `127.0.0.1` (Basic credentials required),
with a root CA made for the run. It never changes a certificate store: the proxy's CA
reaches hyperbutler through `ca.extraFile`. The store's own path is checked apart, with
`ca.import` on: the bench reads `LocalMachine\Root` and the guest's stock Alpine bundle,
picks the roots that CertSet would import and the stock bundle lacks, and looks for them
(by SHA-256 of the DER) in the guest's `/run/hyperbutler/ca-certificates.crt`. The proxy
dials Docker Hub itself. 21 of 21 checks passed on the guest image built from this tree;
after the review fixes below, 25 of 25 (2026-11-01, a guest image built from that tree):

| Check | Result |
|---|---|
| `proxy.http`/`https` = `http://corpuser:PASSWORD@127.0.0.1:PORT`, apply | dockerd restarted once (1.8 s) |
| `hostGateway.enabled false` with that proxy | refused: on loopback, needs the host gateway |
| `docker pull busybox:1.37` without the CA | fails, `x509: certificate signed by unknown authority`; the proxy saw `CONNECT registry-1.docker.io:443` and a refused handshake |
| `ca.extraFile` = the proxy's CA, `--apply` | dockerd restarted (1.3 s); the pull works in 2.9 s through the proxy: 4 CONNECTs, 4 inspected TLS sessions, 15 requests, no 407 |
| `ca.import` on: `LocalMachine\Root` against the guest | 35 roots, 18 importable and not among the stock bundle's 118; the one picked (`2399561127a57125...`) and the other 17 are in the combined bundle; `caStats.added` > 0 |
| `proxy.https` = `http://...@[::1]:PORT` | refused: bind the relay to 127.0.0.1 |
| guest | `dockerd.env` 0600 in a 0700 directory; `HTTP(S)_PROXY` at `192.168.127.254`, `NO_PROXY`, `SSL_CERT_FILE` |
| containers | no proxy variables; `/proc/cmdline` without the proxy |
| the password | in none of: `config list/get/set` output, `status [--json]`, state.json, engine.log, console.log, `docker info`; `--show-secrets` shows it |
| unset and apply | the pull works directly, no CONNECT |

Also on the VM: the store import itself (29 roots sent, 18 added beyond the stock bundle);
the boot path above; and a forced reconnect (hyperbutlerd killed in the guest, the init
restarted it) after the CA file changed on disk: the engine re-sent the environment in
effect, dockerd kept generation 5 and its PID, the bundle's hash did not change. After
the fixes: `ca.extraFile` set, the original deleted, `hyperbutler restart`: the copy was
used (1 certificate, no error), and `ca-extra.pem` has one ACE, the user's; `uninstall
--dry-run` lists it, `config unset` removes it.

## --json (REQ-UX-008)

`status`, `version`, `config list`, `config get`, `config path`, `diagnose`, `uninstall`,
`doctor` and `import` take `--json`. Every document is one object with `"schema": 1`. Within a schema version
fields are only added; removing or renaming one means a new version.
`cmd/hyperbutler/schema_test.go` pins every field name, so a change fails the tests.

- `version`: `schema`, `version`, `goVersion`, `os`, `arch`, `imagePinned`, and `revision`,
  `modified` when the build has them. `imagePinned` is true for a release build, which
  boots only the guest image it was released with; `imageManifestSha256` is then the
  SHA-256 of that image's `image.json` (ADR 0005, Decision 4).
  Added 2026-11-07, within schema 1: `sandboxImagePinned`, and in a build that has them
  `sandboxImageManifestSha256` (the agent sandbox image's `image.json`), `sandboxMsi`,
  `sandboxMsiSha256`, `sandboxMsiBytes` and `sandboxMsiProductCode` (its installer, as `sandbox
  install` fetches it; the size is a string of digits, as it is linked in) (ADR 0009, "Shipping the
  image").
- `status`: `schema`, `running`, `setUp`, `home`, `dockerContextSelected`, `daemonJson`,
  `daemonJsonExists`, and when known `state` (the engine's state.json, with `daemonConfig`,
  `imageDir`, `pastRuntimeIds` (this installation's last 16 VM runtime ids), `hostGateway`: `address`, `enabled`, `ports`, `reloads`, `error`, and `unpublishedPorts`: the published ports of running containers that could not be mirrored on the host, each with `container`, `port`, `reason`, `since`), `dataDir`, `disk`, `diskHostBytes`, `diskSizeBytes`, `hostFreeBytes`,
  `memoryMB`, `cpus`, `dockerContext`, `dockerCli` (the docker.exe a plain `docker` runs:
  the first on PATH) and `dockerCliBundled` (the one next to `hyperbutler.exe`). The exit
  code is 3 when hyperbutler is not running.
  Added 2026-11-01, within schema 1: `state.executable` and `state.executableVersion`, the
  running engine's `hyperbutler.exe` and its version (REQ-PLAT-008: the release tag, or
  for a source build Go's version of the module, such as a pseudo-version, or `(devel)`
  with the revision). `state.guardPause` (`volume`, `freeBytes`, `resumeAtBytes`, `since`)
  while the free-space guard has the VM paused (REQ-DISK-010), and `guardPause` with the
  same fields, whose `freeBytes` is the free space `status` measured itself (only while
  the engine runs and is paused). Both are absent when they do not apply.
  Added for 0.2, within schema 1: `dockerContextCurrent`, the context config.json
  selects (`default` for docker's own; REQ-UX-018); `state.guestFs` (`sizeBytes`,
  `freeBytes`, `updated`) as the guest last reported its data file system, and
  `guestDisk` (`sizeBytes`, `usedBytes`, `freeBytes`, `usedPercent`, `updated`) while the
  engine runs (REQ-DISK-009; guest-reported numbers, for display only); `update`
  (`checked`, and `latest` and `url` when the last check that got an answer found a
  release) while `updateCheck` is on (REQ-UX-022).
  The container lists of REQ-UX-017 are text only: `stop`, `restart`, `config set
  --apply` and `config daemon apply` have no `--json`.
- `config list`: `schema`, `settingsFile`, `keys` (a list of the items below), and
  `autostart` (0.2): the task's `task` name, `exists`, `command` (what it runs),
  `thisExe` (it starts this `hyperbutler.exe`) and `error` when Task Scheduler could not
  be asked.
- `config get KEY`: `schema` and one item: `key`, `value`, `default`, `source`, `type`,
  `apply` (`live`, `restart-dockerd`, `restart-vm`, `first-start`), `description`.
- `config path`: `schema`, `home`, `settingsFile`, `daemonJson`, `dataDir`.
- `diagnose`: `schema`, `file` (the zip, absolute), `files` (its entries, in order) and
  `missing` (`name`, `reason`) for what could not be included. The zip holds
  `summary.txt`, `version.json`, `status.json`, `settings.json`, `daemon.json`,
  `daemon.applied.json`, `state.json`, `disk.json`, `logs/engine.log(.1)`,
  `logs/console.log(.1)` (the last 4 MiB of each), `last-start-failure.json`, `host.json`
  (Windows build and edition, Hyper-V Administrators, computecore.dll, HCS services,
  hypervisor launch type, CPU virtualization features, compute systems, free space; no
  shell, DISM or elevation), `docker-contexts.json` (meta.json only, never TLS material),
  `docker-config.json` and `files.json` (size, BOM and CRLF of each original JSON file,
  and the SHA-256 of its redacted copy: a hash of the original would let anyone with a
  guess at a password confirm it offline). daemon.json and the docker CLI's config.json
  are read up to 1 MiB, the state files up to 64 KiB; a larger file is listed in
  `missing` and summary.txt with its size. A log's window starts at a whole line. Every
  file goes through `internal/redact`: values of secret keys (`auth`,
  `password`, `token`, `identitytoken`, ...), URL user information, Authorization,
  Proxy-Authorization, Cookie and X-Registry-Auth headers, Bearer tokens, JWTs, PEM
  private keys. Paths stay. The zip is created new (never over a file) with an ACL for
  the user, SYSTEM and Administrators; on FAT32 or exFAT, which keep no ACLs, diagnose
  says the zip is not private.
- `uninstall`: `schema`, `home`, `dryRun`, `keepData`, `items` (`kind`, `path`, `detail`;
  `[]` when no trace is left), and when there are any `kept`, `failed` and `notes`. Kinds:
  `engine`, `vm`, `acl`, `docker-context`, `docker-plugins`, `scheduled-task`, `file`, `dir`.
  `scheduled-task` (added 2026-11-01, within schema 1) is the autostart task, by its Task
  Scheduler path; uninstall lists it only for the installation in
  `%LOCALAPPDATA%\hyperbutler`, the one the task starts.
- `doctor` (added 2026-11-01, REQ-UX-019): `schema`, `home`, `ok` (false when a check
  failed; the exit code is then 1) and `checks`, each with `id`, `status` (`ok`, `info`,
  `warn`, `fail`), `summary` and, when there is one, `fix`. The ids: `vm-platform`,
  `hcs-service`, `hypervisor` (only when it is off), `hcs`, `hyperv-admins`, `pipe`,
  `docker-cli`, `docker-context`, `credential-helper`, `other-engines`, `host-disk`,
  `guest-disk`, `autostart`, and `sandbox-image` (added 2026-11-07: in a build with the pin,
  whether the agent sandbox image is there and matches; no line in a zip without one). doctor reads only: the registry, `computecore.dll`, HCS's
  list of compute systems, the pipe file system's listing (no pipe is opened), PATH,
  `docker --version`, the docker CLI's config.json and Task Scheduler (`schtasks /Query`).
- `import --from-desktop` (added for 0.2, REQ-UX-023; [docs/tools.md](../tools.md)):
  `schema`, `dryRun`, `source` (`pipe`, `via`: `context` for the `desktop-linux`
  context's endpoint, `default` for `\\.\pipe\dockerDesktopLinuxEngine`, and `version`),
  `target` (hyperbutler's pipe), `items`, `skipped`, `bytesCopied`, `canceled`, `ok`
  (false when an item failed or was refused, the import was canceled, or Docker Desktop
  changed; the exit code is then 1), `notes`, and after a run `sourceCheck`:
  `unchanged`, `images`, `volumes`, `containers` (Docker Desktop's counts after the run),
  `hashedVolumes` (volumes whose content hash was compared before and after),
  `differences` and `warnings`. Each item (and each `skipped` one, left out by
  `--all-images`, `--all-volumes` or `--all-containers`) has `kind` (`image`, `volume`,
  `container`), `name`, `id`,
  `tags`, `sizeBytes` (-1: unknown), `status` (before the run: `new`, `already`,
  `partial`, `conflict`, `exists`, `unsupported`, and for a container `compose` or
  `needs-confirm`), `outcome` (`imported`, `tagged`,
  `already`, `refused`, `failed`, `canceled`; empty in a dry run), `bytesCopied`,
  `reason`, `missingTags`, `addedTags`, `usedBy` (running Docker Desktop containers that
  mount the volume) and `hash` (a volume's verified content hash, `sha256:...`). A
  container (REQ-UX-024, added without a schema change) also has `image`, `state`,
  `project`, `projectDir`, `requires`, `capAdd`, `binds`, `ports`, `volumes`, `networks`,
  `writableBytes` (not copied; -1: unknown) and `notes`; never its environment or command.

## Tests and fuzzing

- Unit tests: every key's set, unset, refusals and sources; a refused `set` and an invalid
  settings.json leave the file unchanged byte for byte (`TestConfigNeverOverwritesInvalid`);
  the daemon.json checks; the transfer (chunking, size and hash check, removal, gaps); the
  guest's install, rollback, boot rollback, connection loss during a restart, and a
  changed file on a running dockerd; the engine's candidate order (user file, previous copy,
  none), apply and rollback; the schema fields.
- Fuzz, 45 s each on this machine: `daemoncfg` FuzzCheck 0.96M execs; `config` FuzzSet 6.0M
  (two crashers before, see above); `guestctl` FuzzDecode 10.2M, FuzzServeConn 1.8M,
  FuzzClient 0.28M, FuzzReadFrame 10.5M; `engine` FuzzParseSettings 1.4M, FuzzParseState 1.5M.
  All passed after the fixes.
- REQ-UX-009: unit tests for the proxy checks and redaction, the CA set (limits, order,
  expiry, untrusted), the bundle, the document, the transfer (1 MiB, interleaved with a
  daemon.json), the guest's install, restart and rollback, the shared lock, the engine's
  order, reconnect and apply, the secrets, and a read of this machine's real stores
  (`internal/wincert`). `go test -race` for the guest packages, `guestctl`, `dockerenv`,
  `daemoncfg` and the engine's delivery tests ran in a golang:1.27 container on the
  guest. New fuzz targets, 20-45 s each: `dockerenv` FuzzProxyURL 0.15M (one false alarm
  in the property, fixed), FuzzDecode 0.32M, FuzzNoProxy 3.6M; `config` FuzzSet 1.9M
  with the new keys and a no-password-shown property; `guestctl` FuzzServeConn 0.66M,
  FuzzDecode, FuzzClient with the new messages. All passed. After the second review:
  `engine` FuzzBuildDockerdEnvNoProxy (any accepted `proxy.noProxy` with any daemon.json
  registries gives a valid document with the proxy and the whole user list), 30 s, 76k
  execs, passed; its seeds fail without the larger document limit. `go test -race` for the guest
  packages, `dockerenv` and `guestctl` in a golang:1.27 container on the guest, and the
  init's reader test under dash and BusyBox (golang:1.27-alpine).

## Adversarial review (REQ-UX-009)

| # | Finding | Resolution |
|---|---|---|
| H3 | Re-reading the stores on a control reconnect would restart dockerd, and so every container, whenever AuthRoot or Group Policy changed them. | **Fixed:** built at boot and on an explicit apply only; a reconnect re-sends what is in effect. Test `TestDockerdEnvReconnectKeepsInEffect`; on the VM, a forced reconnect after the CA file changed kept dockerd's PID. |
| H4 | If the environment arrived after daemon.json, dockerd would start without the CA and be restarted on every boot. | **Fixed:** the engine sends it first in the same connection, so the one gate (`daemon.ready`) covers both, and `start` waits for both. Tests `TestDockerdEnvBootOrder`, `TestDockerdEnvAfterBoot` (a late one is applied with a restart, never reported as in effect without); on the VM, generation 1 had the environment. |
| H5 | Proxy credentials could reach the kernel command line, the logs, state.json or `status --json`. | **Fixed/verified:** redacted everywhere but settings.json and the guest's 0600 `dockerd.env` (0700 directory); `--show-secrets` for `config list/get`. Tests `TestDockerdEnvNoSecrets`, `TestConfigProxySecret`, `TestProxyKeys`, the FuzzSet and FuzzProxyURL properties; checked on the VM by the bench. |
| H6 | A proxy for the guest's own addresses; a loopback proxy the host gateway does not forward. | **Fixed:** the default NO_PROXY entries and daemon.json's registries are added; `config set` refuses the gateway/port combination (`TestCheckProxyReach`, `TestProxyKeys`). |
| M1 | `proxies` in daemon.json would silently override `proxy.*`. | **Fixed:** owned key. |
| L4 | Certificates dropped over the limits or not yet valid were invisible. | **Fixed:** `caStats` in state.json, `status --json` and the engine log; `notYetValid` counted apart from `expired`. |
| L5 | Builds and containers do not get the CA or the proxy; NTLM needs a relay. | **Documented** above and in the README. |
| 1 low | A store with absurdly many or large entries. | Bounded per store, per certificate and in total; duplicates are dropped before parsing. |
| 2 low | A root that Windows restricts with properties (enhanced key usage set on the store entry, or a disabled flag) is still imported; only the `Disallowed` stores and the certificate's own fields are honored. | **Open**, as in Docker Desktop as far as we know. |
| 3 low | At boot, a new environment that keeps dockerd from starting is not watched like a daemon.json: with no previous one, the VM would power off. The proxy and certificate values are checked on the host and again in the guest, and neither a proxy variable nor a CA bundle stops dockerd from starting (an unreadable bundle gives an empty pool, not an exit). | **Accepted** for 0.1; a running dockerd is watched and rolled back. |
| 4 low | `ca.extraFile` on a local drive can still be a link to a network path; it is read at each boot and apply. | **Fixed** by M-C below. |
| 5 low | dockerd's environment is readable through `/proc` by a `--privileged --pid=host` container, which is root on the guest anyway. | **Accepted.** |

A second adversarial review of the fixed feature:

| ID | Finding | Outcome | Test |
|---|---|---|---|
| M-A | A `proxy.noProxy` near its 4096-byte limit, with the defaults and the registries added, failed `Doc.Validate`; the "cannot happen" branch then dropped the whole environment, certificates included, while state.json still showed the proxy and `caStats.added`. | **Fixed:** the document's NO_PROXY may be 1 KiB longer than the user's list, so the defaults always fit; registries that do not are left out and counted. The impossible branch now keeps the certificates, clears the proxy in the state and says why. | `TestBuildDockerdEnv` (a 4090-byte list), `TestSettleDoc`, `FuzzBuildDockerdEnvNoProxy` |
| M-B | `bench/corp-proxy` measured `ca.extraFile`, not the requirement's path (a root of the Windows store, `ca.import`). | **Fixed:** the bench picks a `LocalMachine\Root` root that the guest's stock bundle lacks, checks the combined bundle holds it (and every other such root), and that `caStats.added` > 0. Read-only. | `bench/corp-proxy` (25 of 25 on the VM) |
| M-C | The `ca.extraFile` path check was bypassable (a link or junction to a share, a mapped drive), and the file was read at every boot and apply with no timeout. | **Fixed:** copy at `config set` (links resolved without opening their targets; UNC, device paths and network drives refused), only the copy is read at boot; its hash is in settings.json. | `TestResolveLocal` (injected resolver: junction and link to UNC, `\??\UNC\`, volumes, network drive, loops), `TestResolveLocalReal` (a real junction; a real symbolic link to UNC where the account may create one), `TestCAExtraFileCopy`, `TestBuildDockerdEnvCACopy`, `TestSaveCAExtra` |
| L-A | `ca.extraFile` certificates bypassed the Disallowed stores, which were read only with `ca.import`. | **Fixed:** `wincert.LoadDisallowed` runs first, whatever `ca.import` says; matching is by certificate hash, as in Windows (doc wording fixed). | `TestBuildDockerdEnvDisallowedExtraFile` |
| L-B | A reconnect erased a build-time error (a certificate file that could not be read) from the state while that environment stayed in effect. | **Fixed:** the build's error is kept with the environment in effect and reported again on a reconnect. | `TestDockerdEnvReconnectKeepsBuildError` |
| L-C | When the init's 30 s gate timed out, a daemon.json or environment arriving while dockerd was being started was installed as at boot, though dockerd might have read the files already. | **Fixed:** the init creates `dockerd.starting` after the gate; then `apply` and `applyEnv` wait for dockerd's generation and restart it for a change. | `TestDockerdStartingIsNotBoot` |
| L-D | At boot, daemon.json's watch could blame and roll back a good daemon.json for a dockerd that the environment kept from starting. | **Documented** in a comment at the watch: neither a proxy variable nor a CA bundle stops dockerd, and both are checked on both sides. | (comment) |
| L-E | A `[::1]` (or other `127.x`) loopback proxy was rewritten to the host gateway, which connects to `127.0.0.1`: the wrong service, with the credentials. | **Fixed:** refused by `config set` ("bind the relay to 127.0.0.1"), and not rewritten; `localhost` stays supported (the gateway falls back to `[::1]` on a refusal). | `TestCheckProxyReachOtherLoopback`, `TestRewriteLoopback`, `TestProxyKeys`; the bench |
| L-F | Missing tests: NO_PROXY merge keeping `Validate` true, the init's reading of `dockerd.env`, wincert's per-store limit and error paths. | **Fixed:** the M-A fuzz target; the init's own reader run in sh (dash and BusyBox) on the Go writer's output, `sh -n` of the init, and a format contract test on every OS; wincert tests on an in-memory (`CERT_STORE_PROV_MEMORY`) store. The per-store limit was off by one (a store with exactly the limit was reported as over), fixed. | `FuzzBuildDockerdEnvNoProxy`, `TestInitReadsDockerdEnv`, `TestDockerdEnvContract`, `TestEnumStoreLimit`, `TestEnumStoreOversized`, `TestEnumStoreErrors` |

## Adversarial review (REQ-UX-006, REQ-UX-007)

| # | Finding | Resolution |
|---|---|---|
| 1 high | The guest's restart watch used the connection's context: a reconnect during an apply rolled a healthy dockerd back and restarted it again. | **Fixed:** the watch runs without the connection's cancel, bounded by its own 90 s. Test `TestDaemonConfigConnectionLostDuringRestart`. |
| 2 high | A changed file sent without the restart flag to a running dockerd was reported as installed, so the host believed it in effect (init boot wait timed out, a re-send after a rollback). | **Fixed:** a changed file on a running dockerd is always applied with a restart. The init's boot wait is 30 s, not 10. Test `TestDaemonConfigChangedWhileRunning`. |
| 3 medium | `live-restore` keeps containers running through shutdown, so the data disk cannot be unmounted. | **Fixed:** owned. |
| 4 medium | A signal while the init starts dockerd killed the stale PID and left the new dockerd unobserved. | **Fixed:** while starting, the handlers only set flags; `start_dockerd` acts on them once the new PID is known. |
| 5 low | The engine held its lock across guest round trips, so `State` waited for an apply. | **Fixed:** `State` reads a separately locked copy. |
| 6 low | Sources that can mislead: `env` is this shell's, `data.dir`/`disk.size` always `file`. | **Documented** above. |
| 7 low | `cpus` above the host's count failed only at start; device names in `data.dir`. | **Fixed** for cpus (and the default never exceeds the host). Device names: open. |
| 8 low | Bidi and zero-width characters from the guest reached the terminal. | **Fixed:** escaped in guest text and in `printable`. |
| 9 low | The bench compared the host's hash, not the guest's file. | **Fixed:** the bench reads the guest file back; the rollback and `live-restore` cases were added. |

## Open

- `proxy.mode`: the Windows system proxy and PAC files (0.2).
- Kerberos and NTLM proxy authentication without a local relay.
- Windows store properties (EKU restrictions, disabled roots), see review 2.

- `config edit`, `config daemon edit`, `config reset` (0.2 per the settings plan).
- A LAN registry on a second machine (`-Registry`).
- Admin policy and locked keys (later).
- Restarting dockerd stops the containers (no live-restore); `--restart` policies bring them back.
