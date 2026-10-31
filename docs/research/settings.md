# Settings, daemon.json and --json output

- Date: 2026-10-31
- Requirements: REQ-UX-006, REQ-UX-007, REQ-UX-008 ([ADR 0008](../adr/0008-settings-gui-mcp.md))
- Machine: Windows 11 Home 10.0.26200, non-elevated account in Hyper-V Administrators,
  a 3 GiB / 8 vCPU guest on its own pipe (`\\.\pipe\docker_engine_hb_settings`) with
  `HYPERBUTLER_HOME` and `DOCKER_CONFIG` in a temp directory.
- Code: `internal/config` (the keys), `internal/daemoncfg` (daemon.json checks),
  `internal/engine/daemonjson.go` (delivery), `internal/guestctl` (the transfer),
  `guest/cmd/hyperbutlerd/daemoncfg.go` and `guest/prototype/init` (the guest side),
  `cmd/hyperbutler/config_windows.go` and `schema.go` (the CLI).

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

- **Reserved names** (refused with "reserved for a later release"): `proxy.mode`,
  `proxy.http`, `proxy.https`, `proxy.noProxy`, `ca.import`, `updates.check`, `autostart`,
  `tray`, `ports.lan`, `buildkit.gcKeepStorage`.
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

`proxies` is not owned yet; REQ-UX-009's proxy settings may take it over.

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

## --json (REQ-UX-008)

`status`, `version`, `config list`, `config get` and `config path` take `--json`. `diagnose`
does not exist yet. Every document is one object with `"schema": 1`. Within a schema version
fields are only added; removing or renaming one means a new version.
`cmd/hyperbutler/schema_test.go` pins every field name, so a change fails the tests.

- `version`: `schema`, `version`, `goVersion`, `os`, `arch`, and `revision`, `modified` when
  the build has them.
- `status`: `schema`, `running`, `setUp`, `home`, `dockerContextSelected`, `daemonJson`,
  `daemonJsonExists`, and when known `state` (the engine's state.json, with `daemonConfig`,
  `imageDir` and `hostGateway`: `address`, `enabled`, `ports`, `reloads`, `error`), `dataDir`, `disk`, `diskHostBytes`, `diskSizeBytes`, `hostFreeBytes`,
  `memoryMB`, `cpus`, `dockerContext`. The exit code is 3 when hyperbutler is not running.
- `config list`: `schema`, `settingsFile`, `keys` (a list of the items below).
- `config get KEY`: `schema` and one item: `key`, `value`, `default`, `source`, `type`,
  `apply` (`live`, `restart-dockerd`, `restart-vm`, `first-start`), `description`.
- `config path`: `schema`, `home`, `settingsFile`, `daemonJson`, `dataDir`.

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

## Adversarial review

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

- `diagnose --json` (with `diagnose`).
- `config edit`, `config daemon edit`, `config reset` (0.2 per the settings plan).
- A LAN registry on a second machine (`-Registry`).
- Admin policy and locked keys (later).
- Restarting dockerd stops the containers (no live-restore); `--restart` policies bring them back.
