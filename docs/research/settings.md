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
| `engine.memoryLimit` | GOMEMLIMIT syntax or `off` | 256MiB | VM restart |
| `hostGateway.enabled` | bool | true | at once (the running engine, no restart) |
| `hostGateway.ports` | port list (`5432,8000-8100`) or `all` | all | at once (the running engine, no restart) |
| `ca.import` | bool | true | dockerd restart (`config daemon apply`, or `--apply`) |
| `ca.extraFile` | a PEM file on a local drive, or empty | empty | dockerd restart |
| `proxy.http`, `proxy.https` | `http://`, `https://` or `socks5://` URL, credentials allowed | none | dockerd restart |
| `proxy.noProxy` | comma-separated hosts, `.domains`, IPs, CIDRs | empty | dockerd restart |

- **Reserved names** (refused with "reserved for a later release"): `proxy.mode` (the
  Windows system proxy, 0.2), `updates.check`, `autostart`, `tray`, `ports.lan`,
  `buildkit.gcKeepStorage`.
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
  `CurrentUser\Disallowed` or `LocalMachine\Disallowed`; duplicates are dropped by the
  SHA-256 of the DER. `ca.extraFile` certificates come first and need only parse.
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
  proxy is set.
- **A proxy on this host's loopback** (a local NTLM or Kerberos relay such as px or cntlm,
  which is how such proxies are used without Docker Desktop's Business-only support) is
  rewritten to the host gateway address, `192.168.127.254`, which forwards to Windows'
  `127.0.0.1` (REQ-NET-004). `config set proxy.*` and `hostGateway.*` refuse a combination
  where the host gateway is off or does not forward the proxy's port.
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
  first dockerd (generation 1) had `SSL_CERT_FILE` and the proxy.
- **Restarts only when asked.** A changed environment on a running dockerd restarts it
  (never reported as in effect without), so only `config daemon apply` and `config set
  ... --apply` build a new one. On a control reconnect (sleep and resume, a hyperbutlerd
  restart) the engine sends what is in effect, not a new import: the stores change on
  their own (AuthRoot fills on demand, Group Policy refreshes), and a restart stops every
  container. An apply restarts dockerd once: for the environment if it changed, otherwise
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
with a root CA made for the run. It never touches a certificate store: the CA reaches
hyperbutler through `ca.extraFile`, which takes the same path as the store's
certificates from the CertSet on. The proxy dials Docker Hub itself. 21 of 21 checks
passed on the guest image built from this tree:

| Check | Result |
|---|---|
| `proxy.http`/`https` = `http://corpuser:PASSWORD@127.0.0.1:PORT`, apply | dockerd restarted once (1.8 s) |
| `hostGateway.enabled false` with that proxy | refused: on loopback, needs the host gateway |
| `docker pull busybox:1.37` without the CA | fails, `x509: certificate signed by unknown authority`; the proxy saw `CONNECT registry-1.docker.io:443` and a refused handshake |
| `ca.extraFile` = the proxy's CA, `--apply` | dockerd restarted (1.3 s); the pull works in 2.9 s through the proxy: 4 CONNECTs, 4 inspected TLS sessions, 15 requests, no 407 |
| guest | `dockerd.env` 0600 in a 0700 directory; `HTTP(S)_PROXY` at `192.168.127.254`, `NO_PROXY`, `SSL_CERT_FILE` |
| containers | no proxy variables; `/proc/cmdline` without the proxy |
| the password | in none of: `config list/get/set` output, `status [--json]`, state.json, engine.log, console.log, `docker info`; `--show-secrets` shows it |
| unset and apply | the pull works directly, no CONNECT |

Also on the VM: the store import itself (29 roots sent, 18 added beyond the stock bundle);
the boot path above; and a forced reconnect (hyperbutlerd killed in the guest, the init
restarted it) after the CA file changed on disk: the engine re-sent the environment in
effect, dockerd kept generation 5 and its PID, the bundle's hash did not change.

## --json (REQ-UX-008)

`status`, `version`, `config list`, `config get`, `config path`, `diagnose` and `uninstall`
take `--json`. Every document is one object with `"schema": 1`. Within a schema version
fields are only added; removing or renaming one means a new version.
`cmd/hyperbutler/schema_test.go` pins every field name, so a change fails the tests.

- `version`: `schema`, `version`, `goVersion`, `os`, `arch`, `imagePinned`, and `revision`,
  `modified` when the build has them. `imagePinned` is true for a release build, which
  boots only the guest image it was released with; `imageManifestSha256` is then the
  SHA-256 of that image's `image.json` (ADR 0005, Decision 4).
- `status`: `schema`, `running`, `setUp`, `home`, `dockerContextSelected`, `daemonJson`,
  `daemonJsonExists`, and when known `state` (the engine's state.json, with `daemonConfig`,
  `imageDir` and `hostGateway`: `address`, `enabled`, `ports`, `reloads`, `error`), `dataDir`, `disk`, `diskHostBytes`, `diskSizeBytes`, `hostFreeBytes`,
  `memoryMB`, `cpus`, `dockerContext`, `dockerCli` (the docker.exe a plain `docker` runs:
  the first on PATH) and `dockerCliBundled` (the one next to `hyperbutler.exe`). The exit
  code is 3 when hyperbutler is not running.
- `config list`: `schema`, `settingsFile`, `keys` (a list of the items below).
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
  `docker-config.json` and `files.json` (size, SHA-256, BOM and CRLF of each original
  JSON file). Every file goes through `internal/redact`: values of secret keys (`auth`,
  `password`, `token`, `identitytoken`, ...), URL user information, Authorization,
  Proxy-Authorization, Cookie and X-Registry-Auth headers, Bearer tokens, JWTs, PEM
  private keys. Paths stay. The zip is created new (never over a file) with an ACL for
  the user, SYSTEM and Administrators.
- `uninstall`: `schema`, `home`, `dryRun`, `keepData`, `items` (`kind`, `path`, `detail`;
  `[]` when no trace is left), and when there are any `kept`, `failed` and `notes`. Kinds:
  `engine`, `vm`, `acl`, `docker-context`, `file`, `dir`.

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
  FuzzDecode, FuzzClient with the new messages. All passed.

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
| 4 low | `ca.extraFile` on a local drive can still be a link to a network path; it is read at each boot and apply. | **Open**; UNC and device paths are refused. |
| 5 low | dockerd's environment is readable through `/proc` by a `--privileged --pid=host` container, which is root on the guest anyway. | **Accepted.** |

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
