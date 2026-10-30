# Docker API over hvsock: stock docker CLI to dockerd in an HCS VM

- Date: 2026-10-30
- Requirements: REQ-API-001, REQ-START-001 (indicative only, see "Timings")
- Machine: Windows 11 Home 10.0.26200, user in Hyper-V Administrators, **non-elevated**
- Code: `guest/cmd/agent`, `internal/vm`, `cmd/winbox-dev` (dev-only harness),
  `internal/transport` (existing)

## Path

```
docker.exe --npipe--> \\.\pipe\docker_engine_winbox   (transport.ListenPipe, current user only)
           --Proxy--> AF_HYPERV <VM runtime id>:<vsock 2375 service GUID>   (transport.HvsockDialer)
           --hv_sock--> guest agent, AF_VSOCK port 2375, host CID only
           --relay--> /run/docker.sock (dockerd 29.9.2)
```

No guest network is involved. The HCS document (`internal/vm.Config.Document`) has an
`HvSocket.HvSocketConfig` whose default bind and connect descriptors are `D:P` (empty
DACL, nobody), and whose `ServiceTable` has one entry, the vsock 2375 service GUID, with
`ConnectSecurityDescriptor = D:P(A;;FA;;;<current user SID>)` and a deny-all bind
descriptor.

## How to reproduce

```powershell
powershell -ExecutionPolicy Bypass -File guest\prototype\build.ps1 -Out $env:TEMP\winbox-out
go build -o $env:TEMP\winbox-dev.exe ./cmd/winbox-dev
& $env:TEMP\winbox-dev.exe run -kernel $env:TEMP\winbox-out\vmlinuz-virt -initrd $env:TEMP\winbox-out\initramfs-winbox.gz
# second shell:
docker -H npipe:////./pipe/docker_engine_winbox version
& $env:TEMP\winbox-dev.exe mkimage -busybox <busybox.static from the build cache> -o busybox.tar
docker -H npipe:////./pipe/docker_engine_winbox load -i busybox.tar
docker -H npipe:////./pipe/docker_engine_winbox run --rm --network none winbox-test/busybox
```

The docker CLI was the official static build `docker-29.9.2.zip` from
`https://download.docker.com/win/static/stable/x86_64/`, SHA-256
`b2be959bfed1281ddaf665c0972cba9a62ce30c542b6bcce82f055bf8886bc7d` (49,730,404 bytes;
`docker.exe` is not Authenticode-signed). It is not in git.

## Results

All from non-elevated PowerShell, 1024 MB, 2 vCPUs, `console=ttyS0 quiet`.

| Step | Result |
|---|---|
| HcsCreateComputeSystem | 43–107 ms |
| HcsStartComputeSystem | +60 ms |
| first guest console line (init) | ~0.83–0.98 s after create |
| agent listening, docker.sock present | ~0.87–1.03 s |
| **dockerd answers `/_ping` over hvsock** | **1.17–1.21 s** (4 boots) |
| `docker version` | works; median 95 ms (5 runs, warm) |
| `docker info` | works; median 102 ms. Storage driver overlayfs (containerd snapshotter), cgroup v2 |
| `docker load` of the 1 MB busybox scratch image | 133 ms |
| `docker run --rm --network none winbox-test/busybox` | prints `hello from winbox`; median 191 ms |
| `docker run -i ... cat` with piped stdin | echoes stdin, exits: half-close works end to end |
| `docker run ... sh -c 'exit 7'` | exit code 7 reaches the CLI |
| 100 MiB container stdout to a file (`--log-driver none`) | 0.78 s including container start |
| 100 MiB file to container stdin (`wc -c`) | 0.34 s, byte count correct |

The first `docker.exe` invocation after download took 2.0 s (antivirus scanning a new
45 MB binary); later ones are below 100 ms.

`go test -tags integration -run Integration ./internal/vm/` boots the same guest and
checks that the listed service answers `/_ping`, that a service missing from the table
is refused with `WSAEACCES` (the deny-all default is enforced, not just "nothing
listening"), that a pending connect honors its context, and that Terminate is
idempotent. It passed on this machine in 1.8 s.

### Findings on the way

- **go-winio ignores the context while ConnectEx is pending.** A connect to a guest port
  that nobody listens on yet (the guest is still booting) stays pending for about 30 s,
  whatever the context says. An earlier readiness probe here therefore reported 2.1 s
  for what is really 1.2 s, and `Proxy.DialTimeout` and Ctrl+C could not cut a dial
  short. `transport.HvsockDialer` now returns when its context ends and closes a
  connection the abandoned attempt still produces.
- **An unread COM port stalls the guest.** Writes to `/dev/console` block when nobody
  reads the console pipe, and the prototype's dockerd logs there. winbox-dev always
  drains it.
- **json-file logs on tmpfs can exhaust guest RAM.** A 100 MiB stdout run with the default
  log driver stopped at ~80 MB and wedged dockerd: json-file stores the stream, escaped,
  in `/var/lib/docker`, which is tmpfs in the prototype. Not a transport problem; the
  production image puts `/var/lib/docker` on a disk (ADR 0005).

## Adversarial review

The adversarial reviewer reviewed the diff. Every finding and what was done:

| # | Sev | Finding | Resolution |
|---|---|---|---|
| 1 | Med | Terminate tolerated only the `0xC037…` (vmcompute) codes for "not found / already stopped", computecore returns `0x8037…` | Fixed: `alreadyGone` accepts both families; integration test terminates twice |
| 2 | Med | `cmd/winbox-dev` had no tests; image layout untested | Fixed: `TestBuildImage` checks every descriptor against its blob, diff_id, legacy manifest, layer entries, reproducibility; `TestFullName` |
| 3 | Med | Connect descriptor enforcement asserted, not verified | Partly fixed: integration test shows an unlisted service is refused with WSAEACCES. Not verified: that a **different user** is refused by the listed entry (needs a second account or a restricted token). Note: the descriptor excludes SYSTEM and Administrators, so an ADR 0004 broker running as SYSTEM cannot use this entry as is |
| 4 | Low | Console scanner stopped on a >64 KiB line; guest text printed to the terminal unescaped | Fixed: `bufio.Reader.ReadSlice` loop that never stops on long lines, reported lines escaped with `strconv.QuoteToGraphic`; `TestStreamConsole` |
| 5 | Low | Console pipe opened without SQOS flags | Fixed: `SECURITY_SQOS_PRESENT \| SECURITY_IDENTIFICATION` |
| 6 | Low | Agent hid dockerd dial failures | Fixed: logged, at most once per 5 s |
| 7 | Low | Ctrl+C during boot reported as "not ready" / CreateFile error | Fixed: returns `context.Canceled`; `TestWaitPingCancelled` |
| 8 | Low | Comment claimed AllowOvercommit returns idle memory | Fixed comment |
| 9 | Low | Host proxy allows 1024 connections, agent 256 | Fixed: winbox-dev sets `Proxy.MaxConns` to the agent's 256 |
| 10 | Low | Init does not restart the agent | Fixed: restart loop in `guest/prototype/init` |
| 11 | Low | No fuzz corpus committed | Fixed: `guest/cmd/agent/testdata/fuzz/FuzzRelay`, `internal/vm/testdata/fuzz/*` |

## Tests and limits

- Unit tests: relay half-close both orders, reverse direction after half-close, reset,
  CloseWrite failure; HCS document and validation; SID/SDDL; RuntimeId parsing; image
  layout; console streaming.
- Fuzz: `FuzzRelay` (byte streams and half-close ordering through the relay),
  `FuzzUserOnlySDDL`, `FuzzParseRuntimeID`.
- **`-race` was not run**: the race detector needs cgo, and this host has no C toolchain.
  The relay tests are platform-independent and should be run with `-race` in Linux CI.
- The agent's AF_VSOCK code (`main_linux.go`) is exercised only by the boots above, not
  by unit tests.

## Open issues

- Other-user denial (finding 3) is unverified.
- The pipe is only as private as `transport.ListenPipe`'s DACL; the hvsock entry adds the
  same restriction one hop further. Neither authenticates the guest to the host.
- winbox-dev has no reconnect on guest restart and no host sleep/resume handling.
- No guest network; images come only from `docker load`. (Since superseded: the user-mode network of ADR 0006 is on by default, so `--network none` is no longer needed; see guest-networking.md.)
