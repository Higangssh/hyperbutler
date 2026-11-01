# Guest networking on Windows Home without elevation

Question for [ADR 0006](../adr/0006-guest-networking.md): how does the HCS guest get
outbound network access, for example for `docker pull`, on Windows 11 Home without running
anything elevated?

Requirements: REQ-NET-002 (VPN and split DNS with zero configuration), REQ-PLAT-003 (no
elevation in daily use), REQ-PLAT-004 (coexist with WSL, Docker Desktop and VPN clients).
Pain points: N1 and N2 in [wsl2-docker-pain-points.md](wsl2-docker-pain-points.md).

## Setup

- Windows 11 Home build 26200, Wi-Fi uplink, `hns` and `vmcompute` running.
- The account is in **Hyper-V Administrators** and the process runs at **medium
  integrity** (not elevated). This is the developer mode of ADR 0002, not the end-user
  path of ADR 0004.
- A real VPN was connected for every run: **Tailscale** (WireGuard, a TUN adapter with
  per-peer `/32` routes in 100.64.0.0/10, and split DNS through NRPT rules that send
  `*.ts.net` to 100.100.100.100).
- WSL and Docker Desktop are **not installed** on this machine, so their coexistence was not
  tested.
- Guest: Alpine `linux-virt` 6.18.58 (`hv_netvsc` built in; `vsock` and `hv_sock` loaded
  as modules), booted with LinuxKernelDirect, 512 MB, 2 vCPUs, with ADR 0002's memory
  settings.
- Code: `internal/hcn` (a `computenetwork.dll` binding) and `spike/net` (the driver, a
  guest probe, and a CONNECT relay). The guest probe is a static Go binary in an extra
  initramfs cpio. It does what `docker pull` does:
  1. DNS lookup of `registry-1.docker.io`.
  2. TCP and TLS 1.3 to it.
  3. `GET /v2/`, which should return 401.
  4. An anonymous token from `auth.docker.io`.
  5. The `ubuntu:24.04` linux/amd64 manifest.
  6. A download of its largest layer, 29.8 MB, which redirects to a CDN host.

Before anything was created, `HcnEnumerateNetworks` and `HcnEnumerateEndpoints` returned
**no networks and no endpoints**. Home with only Virtual Machine Platform enabled has no
Default Switch.

## Path 1: an HNS NAT network and endpoint

### Creating it, non-elevated

`HcnCreateNetwork` and then `HcnCreateEndpoint`, with schema 2.0 documents (see
`internal/hcn/schema.go`).

- **Network:** `Type: NAT`, one static IPAM subnet `10.239.77.0/24` with a default route via
  `.1`, and `Flags: 8` (`EnableNonPersistent`).
- **Endpoint:** a static `10.239.77.2/24`.

| Call | Result | Time |
|---|---|---|
| `HcnCreateNetwork` (NAT) | **S_OK** | 882 ms on the first call, then 292–367 ms (median 329 ms, n=7) |
| `HcnCreateEndpoint` | **S_OK** | 15–55 ms (median 35 ms, n=8) |
| `HcnDeleteEndpoint`, `HcnDeleteNetwork` | S_OK | not timed |
| `HcnCreateNetwork` with `Type: "Bogus"` | `0x803B001B` (HCN_E_INVALID_JSON), with the error record `JSON document string is invalid. {{Type,UnknownEnumValue}}`, localized | 4 ms |

So **a Hyper-V Administrators member can create and delete NAT networks and endpoints
without elevation.** No access error appeared. It is not known whether a user outside
that group can do it, because that needs a second account. ADR 0004's broker would make
these calls anyway.

What HNS did on the host, read from the network document:
- It created a hidden host vNIC (`NlmHidden: true`, "Host Vnic ...") with `10.239.77.1`.
- It created a WinNAT instance for the prefix (`"Tag": "NAT"`, `NATState: 2`).
- It allocated a MAC pool `00-15-5D-E1-70-00`–`7F-FF`.
- It wrote the host's own DNS servers into the endpoint's `Dns.ServerList`. Nobody asked
  for that, and the guest only sees it if hyperbutler passes it on.

### Attaching it to the VM

The endpoint goes into the HCS document at create time:

```json
"Devices": { "NetworkAdapters": { "eth0": { "EndpointId": "<endpoint GUID>", "MacAddress": "<endpoint MAC>" } } }
```

`HcsCreateComputeSystem` succeeded without elevation and without any `HcsGrantVmAccess`.
The guest saw an `hv_netvsc` `eth0` with the endpoint's MAC at **0.18 s** of uptime. There
was no namespace and no `HcnModifyEndpoint` step.

### In the guest

| Check | NAT | ICS type, created the same way |
|---|---|---|
| DHCP (`udhcpc`, 5 discovers) | **no lease** | **no lease** |
| Static IP + default route via `ip` | ready at 0.19 s uptime | same |
| ping gateway `.1` | no reply | no reply |
| ping 1.1.1.1 | 3.5–8.9 ms | 25–103 ms |
| DNS to gateway `.1` | timed out | timed out |
| DNS to the host's upstream resolver | works (3.6–7.5 ms) | works |

- hyperbutler has to assign the address itself (kernel command line or guest agent). It also
  has to supply DNS. The NAT gateway offers neither DHCP nor a DNS proxy.
- The ICS type (what the Default Switch and WSL use) behaved the same with these settings.
  WSL gets DHCP and DNS from extra HNS settings and its own DNS tunnelling. Those were not
  explored. They matter only if HNS NAT becomes the fallback (see Open items).

### The registry probe through NAT

| Run | DNS | TCP | TLS | `/v2/` | token | manifest | layer (29.8 MB) | total |
|---|---|---|---|---|---|---|---|---|
| host (baseline) | 19.0 ms | 186.9 | 210.1 | 572.7 | 240.6 | 484.6 | 844.7 ms = **282 Mbit/s** | 2559 ms |
| NAT, static | 3.6 | 197.6 | 192.8 | 613.3 | 254.6 | 518.7 | 808.9 ms = **294 Mbit/s** | 2600 ms |
| NAT + VPN checks | 4.3 | 191.3 | 189.3 | 609.5 | 239.5 | 550.9 | **297 Mbit/s** | 2609 ms |
| NAT + bulk | – | – | – | – | – | – | **296**, **321**, **302 Mbit/s** | 2601, 2505 ms |
| ICS, static | 7.5 | 195.9 | 193.6 | 580.3 | 250.2 | 524.6 | **306 Mbit/s** | 2540 ms |

Each row is one run (n=1 per cell, except where several values are listed), on Wi-Fi.
The download rates through NAT (294–321 Mbit/s, n=6) are close to the host's single run
(282 Mbit/s). This suggests the internet link, not NAT, is the bottleneck, but the sample
is too small to rank the paths.

**Local bulk, guest to host** (256 MiB from a guest TCP server, read by the host over the
NAT vNIC; the host connects out, so no inbound firewall rule is needed): **7.96, 7.82,
9.59, 8.00, 9.56, 5.52, 8.77, 9.11 Gbit/s** (n=8, from three VM boots). One more run was cut
short when the guest powered off, which was a bug in the test script, since fixed.

### The VPN through NAT

| Check | Result |
|---|---|
| TCP to a tailnet peer by IP (a VPN-only `/32` route) | **works**, 39 ms. WinNAT's outbound traffic followed the host routing table into the Tailscale adapter. |
| The same peer by its MagicDNS name, with the host's upstream resolver in `resolv.conf` | **fails**: NXDOMAIN |
| `nslookup` of the MagicDNS name against 100.100.100.100 | works |
| `nslookup registry-1.docker.io` against 100.100.100.100 | **SERVFAIL** |

- Split DNS cannot work with a resolver list in the guest. Windows decides per name
  (NRPT) which server to use, and the guest has a single `resolv.conf`.
- hyperbutler would need a host-side DNS forwarder that asks the Windows resolver, as WSL's
  `dnsTunneling` does. That forwarder is the same component the user-mode path needs
  anyway.
- **Not tested:** full-tunnel corporate VPNs (AnyConnect, GlobalProtect, Zscaler) that
  filter or refuse forwarded traffic. This is the WSL N2 failure class. NATed guest
  traffic is forwarded traffic, not traffic from a host process, so per-app VPN policies
  and "local LAN access" settings treat it differently.

## Path 2: a user-mode path over hvsock

The VM has **no network adapter**. The guest loads `vsock` and `hv_sock` and connects to
AF_VSOCK CID 2 (the host). `spike/net/relay` on the host accepts the connection through
`winio.ListenHvsock` on the VM's runtime ID. It reads `CONNECT host:port`, resolves the
name with the **Windows resolver**, dials with ordinary Windows sockets, and then copies
bytes.

This is a TCP-only stand-in for a full user-mode stack such as gvisor-tap-vsock. A full
stack would add an Ethernet/IP layer (gVisor netstack) on the host and a TAP device in
the guest. The transport and the exit through host sockets are the same.

### hvsock without the registry

Hyper-V sockets normally need the service GUID registered under
`HKLM\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Virtualization\GuestCommunicationServices`.
That is an admin write, and Podman's Hyper-V provider needs elevation for it. Instead, the
spike puts the service in the **VM's own HCS document**:

```json
"Devices": { "HvSocket": { "HvSocketConfig": { "ServiceTable": {
  "000013eb-facb-11e6-bd58-64006a7986d3": {
    "BindSecurityDescriptor": "D:P(A;;FA;;;<user SID>)",
    "ConnectSecurityDescriptor": "D:P(A;;FA;;;<user SID>)",
    "AllowWildcardBinds": false } } } } }
```

- `ListenHvsock` took 1 ms, and the guest's connections were accepted. **No registry write
  and no elevation were needed** beyond the Hyper-V Administrators membership that HCS
  itself requires.
- The descriptor is meant to admit only the current user, and the listener is bound to
  this VM's runtime ID rather than a wildcard. **Unverified:** no second account tried to
  bind or connect, so it is not shown that the descriptor is enforced on Home. ADR 0004
  lists this as open, and `bench/noadmin` must test it.
- The host also connected *into* the guest over the same mechanism (the bulk test below).

### Results

| Run | TCP (includes host DNS) | TLS | `/v2/` | token | manifest | layer | total |
|---|---|---|---|---|---|---|---|
| relay + VPN checks | 191.5 ms | 188.3 | 577.0 | 240.7 | 518.8 | 986.6 ms = **241 Mbit/s** | 2716 ms |
| relay + bulk | 218.0 | 204.0 | 564.3 | 254.6 | 497.5 | 804.1 ms = **296 Mbit/s** | 2547 ms |
| relay + bulk, after review fixes | – | – | – | – | – | 805.0 ms = **296 Mbit/s** | 2529 ms |

The 241 Mbit/s run is 15% below the host's single run, and the other two equal NAT. With
n=3 that cannot separate a relay cost from Wi-Fi variance.

- **Local bulk, guest to host over hvsock:** **10.10, 8.85, 10.18, 5.32, 6.79, 6.73
  Gbit/s** (n=6, from two VM boots). That is raw stream transport, the ceiling for any
  stack on top of it. The spread between the two boots is as wide as the gap to NAT, so the
  two paths look comparable but cannot be ranked.
- **First connection:** at 0.6 s after `HcsStartComputeSystem` by the host clock. There
  are no HNS objects to create, and the guest needs no address, route or resolver. The NAT
  numbers above (0.19 s) are guest uptime, a different clock, so the two are not directly
  comparable.

| VPN check | Result |
|---|---|
| tailnet peer by IP | **works**, 14.9 ms |
| tailnet peer by MagicDNS name | **works**, 35.4 ms. The host resolver applied the NRPT rule. |
| public names (`registry-1.docker.io`, `auth.docker.io`, the CDN) in the same run | work |

**Split DNS works with zero configuration *in this relay*,** because the guest sends names
and the host resolves them where Windows would. A full user-mode stack such as
gvisor-tap-vsock receives the guest's DNS packets instead. Split DNS then holds only if its
DNS forwarder asks the Windows resolver rather than fixed upstream servers. This was not
tested. The traffic also leaves as hyperbutler's own sockets, so VPN clients, firewalls and
proxies see an ordinary host process.

## The user-mode stack: gVisor in hyperbutler-dev

This is the implementation ADR 0006 asked for (`internal/netstack`, `guest/cmd/netfwd`).
The machine and VPN are the same as above. The guest is the ADR 0005 prototype initramfs
with dockerd 29.9.2, 1024 MB and 2 vCPUs. The design is in the ADR's "Implementation"
section.

### Boot and Docker

| Step | Result |
|---|---|
| Guest link up (netfwd connected over hvsock) | 0.89–1.17 s after `HcsCreateComputeSystem` |
| dockerd `/_ping` | 1.15–1.39 s (unchanged) |
| `docker pull alpine`, fresh boot (3.6 MB compressed) | 2.86, 3.06 s; repeated pulls 2.7–2.9 s |
| `docker pull ubuntu:24.04`, fresh boot | 3.66, 3.49 s |
| `docker pull python:3.13-slim`, fresh boot | 4.02, 3.97 s |
| `docker pull nginx` | 4.7–5.3 s |
| hyperbutler-dev CPU during those pulls | 47–344 ms, 0.02–0.09 cores |
| `docker run --rm alpine wget -qO- https://example.com` | 214–478 ms including container start; prints the page |
| `docker run -d -p 8080:80 nginx`, then curl on the host | `127.0.0.1:8080`, `[::1]:8080` and `localhost:8080` serve nginx 26–99 ms after start; unpublished on `docker rm` |

Pulls are dominated by registry round trips (token, manifest, redirects), not by
bandwidth. For bandwidth, the same 100 MiB HTTPS file
(`proof.ovh.net/files/100Mb.dat`) was downloaded by the host (`curl.exe`) and by a
container (`busybox wget`), alternating, n=3 each: **host 41, 31, 51 Mbit/s; container
32, 65, 52 Mbit/s**, with hyperbutler-dev at 0.05–0.08 cores. Both are limited by the internet
path. The guest cannot be told apart from the host at this n.

### Local throughput and CPU against raw hvsock

`hyperbutler-dev run -bench-bytes N` boots with `hyperbutler.netbench=1`. netfwd then serves a zero
source, a sink and an echo on TCP 5201/5203/5205 (reached through `Publish`, so through
the stack plus one Windows loopback hop) and on vsock 5202/5204/5206 (raw hvsock). Each
run moves 1 GiB. "Cores" is hyperbutler-dev's own CPU time over wall time, so the
hypervisor's (vmwp, vmmem) is not included.

| Configuration | Guest to host, stack | Guest to host, raw | Host to guest, stack | Host to guest, raw |
|---|---|---|---|---|
| First build: MTU 1500, 1 MiB buffers, RACK on | 3.35–3.87 Gbit/s | 8.2–16.7 | **0.18–0.21** (85% of segments retransmitted) | 22–29 |
| MTU 1500, RACK off | 3.16–3.39 | 5.6–9.0 | 2.88–3.38 | 23–33 |
| MTU 9000, 1 MiB buffers, RACK off | 4.69–4.83 (0.31–0.33 cores per Gbit/s) | 7.2–14.2 | 2.62–4.10 | 23–25 |
| MTU 65520, 1 MiB buffers, RACK off | 4.13–4.43 | 9.5–10.6 | 2.88–3.98 | 23–26 |
| **Shipped:** MTU 9000, 256 KiB buffers, boot 1 | 3.68–3.79 (0.22–0.24) | 9.35–10.12 | 4.20–4.88 (0.17–0.24) | 24–27 |
| **Shipped**, boot 2 | 1.76–4.48 (0.28–0.63) | 7.75–13.29 | 4.68–5.43 (0.19–0.32) | 23–26 |

- The host-to-guest collapse in the first build was not loss. The guest reported no drops
  (`rx_dropped` 0, softnet drops 0), but `TCPDSACKOldSent` was 284,812 after a 64 MiB
  transfer: gVisor's RACK and tail loss probe re-sent segments that had arrived. Raising
  the guest's `netdev_max_backlog` alone changed nothing. Turning RACK off fixed it.
- Batching was needed in both directions. netfwd drains the TAP until EAGAIN into one
  vsock write, and the host queues frames for the guest and writes them in one hvsock write.
- The raw hvsock figure varies by 2x between runs of the same boot, so the ratio is noisy.

### Small-request latency (ADR 0003)

64-byte request and 64-byte reply, one at a time, n=20000 per row, timed with
`QueryPerformanceCounter` (Go's monotonic clock on Windows ticks about every 0.5 ms).
"Under load" runs a host-to-guest bulk transfer through the stack at the same time.

| Path | p50 | p99 | p99.9 | max |
|---|---|---|---|---|
| raw hvsock, idle | 53–56 µs | 80–154 µs | 144–267 µs | 0.75–1.3 ms |
| stack, idle | 120–131 µs | 185–271 µs | 0.35–2.0 ms | 1.9–10.6 ms |
| raw hvsock, under load | 50–52 µs | 103–273 µs | 0.5–2.0 ms | 4–30 ms |
| stack, under load | 154–171 µs | 444–710 µs | **11.6–13.5 ms** | 17–40 ms |

The stack adds about 70 µs at the median and about 120 µs at p99 when idle. Under load,
p99.9 sits at 11–13 ms on every run. That is longer than the 1 MiB guest-bound queue
takes to drain at the measured rates (2–3 ms), so a GC pause or scheduler delay is the
likely cause. A `GODEBUG=gctrace=1` run would tell them apart; this was not done.

### VPN (Tailscale), from a container

| Check | Result |
|---|---|
| `nslookup peer-a.<tailnet>.ts.net` (MagicDNS through NRPT) | 100.64.0.10 |
| `nslookup peer-a` (single label; the host applies the tailnet search suffix) | 100.64.0.10 |
| `nslookup registry-1.docker.io` in the same run | 8 public addresses |
| `nslookup nosuch.<tailnet>.ts.net` | NXDOMAIN |
| TCP to tailnet peers by IP (`100.64.0.10:80`, `100.64.0.11:445`) and by MagicDNS name (`:80`, `:22`) | connected |
| TCP to the host's own Tailscale address (`100.64.0.2:445`, `:135`) | refused by the policy |
| TCP to 169.254.169.254:80 | refused |

The host resolver's NRPT rules apply to the stack's DNS forwarder, so split DNS works
with zero configuration. Only Tailscale was tested; full-tunnel VPNs were not.

### Memory

hyperbutler-dev idle with the stack: 21 MB working set, 56 MB private. After the pulls and
downloads: 42 MB and 77 MB. The stack's TCP buffers are bounded at 256 MiB in total
(512 connections x 2 x 256 KiB). No test stressed this bound.

### Reproducing

```powershell
powershell -ExecutionPolicy Bypass -File guest\prototype\build.ps1 -Out $env:TEMP\hyperbutler-out
go build -o $env:TEMP\hyperbutler-dev.exe ./cmd/hyperbutler-dev
& $env:TEMP\hyperbutler-dev.exe run -kernel $env:TEMP\hyperbutler-out\vmlinuz-virt -initrd $env:TEMP\hyperbutler-out\initramfs-hyperbutler.gz
docker -H npipe:////./pipe/docker_engine_hyperbutler run --rm alpine wget -qO- https://example.com
& $env:TEMP\hyperbutler-dev.exe run ... -bench-bytes 1073741824 [-net-mtu 1500]   # throughput and latency
```

## Throughput work (2026-10-31)

The shipped stack missed the ADR 0006 gate (guest-to-host 36-38% of raw hvsock). This
section finds out why and fixes it. Same machine (Windows 11 Home 26200, Core Ultra 7
155H, AC power, non-elevated Hyper-V Administrators account), Wi-Fi uplink, Tailscale
connected. `hyperbutler-dev run -image` with the erofs image, 2048 MB and 2 vCPU,
`-memlimit 256MiB` (the product engine's default).

### Benches

- **`bench/net-perf`** (new) runs `bench/net-guest` in containers on the default bridge
  and the same `internal/netbench` code on the host as the baseline:
  - `dl`/`ul`: a container downloads from, or uploads to, a server on the host's
    127.0.0.1, through `-host-forward`. This is the outbound path: a guest TCP
    connection is terminated in gVisor and dialed again as a Windows socket.
  - `pub-*`: the same through `docker run -p` (inbound).
  - `rr`, `rr-load`, `pub-rr`: 64-byte round trips (n=20000 after 2000 warm-up), idle
    and during a download.
  - `cps`: connections per second, 8 connectors.
  - `url`: a 100 MB HTTPS download from the internet.

  Each container run records the engine's CPU, its private working set (CIM), its
  private bytes and Go heap (`internal/diag`), and its GC count. 5 runs of 5 s per test
  and side; medians.
- **`hyperbutler-dev run -bench-bytes`** (the gate bench) compares the stack with raw
  hvsock on the same boot. It now reads with 256 KiB buffers on both sides: `io.Discard`
  read 8 KiB at a time, which had held raw guest-to-host at 4-7 Gbit/s. It reports
  medians of 5 runs of 1 GiB.

### What limited the stack

Profiles (`-debug-pipe`, CPU and execution traces) and gVisor's TCP probe showed both
the engine (1.1-1.7 cores) and the guest (about 30% of 2 vCPUs) mostly idle during bulk
transfers. Four things were waiting:

1. **MSS 1460 for every container.** `docker0` and the containers' veths have MTU 1500,
   so a container's connection through the 9000-byte link used a 1460-byte MSS. A
   container with `--network host` downloaded at 5.2 Gbit/s; the same on the bridge at
   2.6. Fix: **segmentation offload** on the link. `netfwd -vnet` opens the TAP with
   `IFF_VNET_HDR` and `TUN_F_CSUM | TUN_F_TSO4 | TUN_F_TSO_ECN`; every frame carries a
   virtio_net_hdr. The guest kernel hands netfwd TCP segments of up to 64 KiB. gVisor
   sends 64 KiB segments with only the pseudo-header checksum (its "host GSO", as its
   `fdbased` endpoint does for a TAP), and the guest's TAP takes them as one packet. The
   bridge MTU no longer matters for bulk TCP.
2. **Windows sends slowly on an accepted hvsock connection.** With offload, host-to-guest
   still ran at 2.0-2.6 Gbit/s. The host's writes of about 600 KB took 1.0-1.5 ms each,
   while the guest's reader waited for data 95% of the time. Splitting the directions over
   two connections changed nothing. A connection the **host dialed** took the same
   writes in about 50 µs. Fix: in offload mode netfwd listens, and the host dials it
   (`Stack.ServeOffload`). Guest-to-host also doubled on the dialed connection (ul
   3.9 to 7.7 Gbit/s, in the same experiment).
3. **Loopback Windows sockets with autotuned buffers.** The relay goroutine then spent
   67% of its time waiting in `WSARecv` on the socket to the host-local server. Fix: 1 MiB
   `SO_RCVBUF`/`SO_SNDBUF` on loopback sockets (published ports and explicit host
   forwards). Connections to other machines keep autotuning, because a fixed size would
   cap them on long paths. Effect on the same boot: dl 3.3 to 7.5 Gbit/s, ul 7.7 to 13.2.
4. **gVisor's 256 KiB buffers.** With the other fixes, the gVisor TCP probe showed
   SRTT of 1-5 ms under load: the queues in front of the guest, not the wire. At that RTT
   a 256 KiB window caps a connection at 2-4 Gbit/s. Fix: buffers start at 128 KiB and are
   tuned (receive moderation, send autotuning) up to 1 MiB, for at most 16 connections at
   a time. Every other connection is pinned at 256 KiB. Relayed connections are bounded at
   16 x 2 x 1 MiB + 496 x 2 x 256 KiB = 280 MiB (it was 256 MiB). Moving the send
   limit alone from 256 KiB to 1 MiB took dl from 5.6 to 11.1 Gbit/s; moving the receive
   limit took pub-dl from 2.9 to 7.8.

Measured and **not** a cause:
- The GC: 1-3 GCs per 5 s bulk run, pauses below the 0.5 ms clock resolution, 30-36 MB
  of heap in use.
- The frame relay's syscalls: reading the link ahead of the frame handler on both
  sides (`frame.Pump`) changed nothing on its own. It is kept because it drops a copy and
  drains the vsock while the TAP is written.
- The copy buffer size: 32 KiB and 256 KiB measured the same.
- RACK: it stays off.
- Separate hvsock connections per direction: no change.

Found on the way and fixed:
- **Connections per second.** gVisor's 60 s TIME_WAIT held one endpoint per closed
  connection (180 MB at 640 connections/s). Once the guest reused a port, gVisor answered
  the SYN from the TIME_WAIT endpoint, because it hands SYNs over only to a listener, not
  to a forwarder. Every later connection on that port failed: cps fell to 4-7
  connections/s after the first 5 s run. TIME_WAIT is now 1 s; the guest reuses a port
  only after 28k connections.
- **Garbage.** Fresh 32 KiB copy buffers per connection meant 320 MB/s of garbage at
  5000 published connections/s: Go heap 240 MB, private working set 260 MB. The buffers
  are pooled now: heap 30 MB, working set 51 MB.
- **Resets.** Resets are passed on as resets in both directions. A normal close of the
  other side left a Windows socket in TIME_WAIT for minutes (ephemeral ports) or a
  gVisor endpoint for 1 s.
- **The p99.9 of 11.6 ms under load.** It came from the gate bench, which reconnected its
  bulk load every 256 MiB, 20 times per latency measurement. The p99.9 of 20,000 samples
  is the 20th worst. With one load connection the stack's p99.9 under load is 0.49 ms
  (max 2.6 ms). The raw hvsock echo under the same reconnecting load had stayed at
  0.5-0.7 ms, so the stall per new load connection lies in the stack path. Its exact
  place was not found; it is an open item.

### Before and after

`bench/net-perf`, medians of 5 runs (url: 3). The host column is the same client
against the host's own servers (loopback), on the same boot. Before: the shipped code
and image (`6eceb97`). After: this branch.

| Test | Host | Before | After | After / host | Engine cores, after | Engine private WS, after (before) |
|---|---|---|---|---|---|---|
| dl, container from host server, Gbit/s | 8.7-12.7 | 2.71 | **10.2** | 118% | 1.29 (0.13/Gbit/s) | 46 MB (34) |
| ul, container to host server, Gbit/s | 8.9-12.8 | 3.34 | **8.29** | 93% | 1.40 | 48 MB (35) |
| pub-dl, `-p`, host from container, Gbit/s | 8.5-11.8 | 3.19 | **7.54** | 88% | 1.50 | 55 MB (230, after cps) |
| pub-ul, `-p`, host to container, Gbit/s | 9.2-11.3 | 4.45 | **11.6** | 127% | 1.40 | 53 MB (43) |
| cps, container to host, connections/s | 8056 | 640 in run 1, then **4-7** (failing) | **6024**, 0 errors | 75% | 6.5 | 52 MB (180) |
| pub-cps, `-p`, connections/s | 8086 | 4914 | 5475 | 68% | 2.9 | 51 MB (45) |
| rr, p50 / p99 / p99.9 µs | 37 / 93 / 277 | 205 / 540 / 2692 | 187 / 392 / 1244 | | 0.57 | 48 MB |
| rr-load (during dl) | 36 / 128 / 228 | 424 / 1698 / 4198 | 356 / 1418 / 3093 | | 1.53 | 48 MB |
| pub-rr | 36 / 87 / 272 | 271 / 754 / 7063 | 233 / 421 / 1196 | | 0.47 | 53 MB |
| url, internet HTTPS, Gbit/s | 0.08-0.09 | 0.09 | 0.09 | 99% | 0.15 | 51 MB |

- Host loopback itself varied from 8 to 25 Gbit/s between runs, so a ratio above
  100% means the stack was not the limit in that run, not that it is faster than
  loopback.
- The engine's private working set stayed at 46-55 MB in every after-run (Go heap
  30-36 MB, private bytes about 100 MB).
- The internet link was limited to about 90 Mbit/s that day, so `url` shows only that
  the container matches the host.
- The bulk CPU cost is 0.12-0.20 cores per Gbit/s, from 0.26-0.48.

### The gate (stack against raw hvsock, same boot)

`hyperbutler-dev run -bench-bytes 1073741824`, guest root namespace (netfwd's bench
servers), medians of 5 runs per path and direction:

| Build | Raw guest to host | Stack guest to host | Ratio | Raw host to guest | Stack host to guest | Ratio |
|---|---|---|---|---|---|---|
| Before (the ADR's gate run, n=3, 2 boots) | 7.75-13.29 | 3.60-3.79 | 36-38% | 23-27 | 4.2-5.4 | 17-23% |
| After, boot 1 | 12.26 | 9.79 | **80%** | 26.80 | 10.86 | 41% |
| After, boot 2 | 12.43 | 9.19 | **74%** | 24.69 | 11.80 | 48% |
| After review fixes, boot 3 | 21.70 | 10.56 | 49% | 29.15 | 13.06 | 45% |
| boot 4 | 18.74 | 10.41 | 56% | 23.18 | 14.45 | 62% |
| boot 5 | 7.96 | 7.33 | 92% | 28.63 | 9.98 | 35% |
| boot 6 | 10.81 | 6.92 | 64% | 29.49 | 8.29 | 28% |
| After, offload off (`-net-offload=false`) | 13.11 | 4.75 | 36% | 23.05 | 6.20 | 27% |

The offload-off row keeps the buffer, socket and TIME_WAIT fixes. Without offload (and
so with the guest dialing), the container paths fall back to the old numbers: dl 3.0, ul
3.5, pub-dl 3.9, pub-ul 5.3 Gbit/s. Over 6 boots the guest-to-host ratio is 49-92% (median 69%,
5 of 6 at or above 50%). Raw hvsock guest-to-host varies from 8 to 22 Gbit/s between boots,
while the stack holds 6.9-10.6 Gbit/s, so the ratio depends mostly on the raw run.

Latency in the gate bench, after the load fix (one boot): raw idle p50/p99/p99.9
52/77/124 µs; stack idle 158/257/504 µs; raw under load 52/96/259 µs; stack under load
147/244/**491** µs (max 2.6 ms).

### Reproducing

```powershell
powershell -ExecutionPolicy Bypass -File guest\prototype\build.ps1 -Strict -Out OUT -DockerHost npipe:////./pipe/docker_engine_hyperbutler
go build -o hb-dev.exe ./cmd/hyperbutler-dev ; go build -o net-perf.exe ./bench/net-perf
hb-dev.exe run -image OUT\image.json -memlimit 256MiB -mem 2048 -debug-pipe \\.\pipe\hb-netperf-debug `
    -host-forward 192.168.127.254:5301=127.0.0.1:5301 -host-forward 192.168.127.254:5302=127.0.0.1:5302 `
    -host-forward 192.168.127.254:5303=127.0.0.1:5303 -host-forward 192.168.127.254:5304=127.0.0.1:5304
net-perf.exe -docker docker.exe -debug-pipe \\.\pipe\hb-netperf-debug -rounds 5 -secs 5 [-profile DIR [-trace | -snapshot heap]]
hb-dev.exe run -image OUT\image.json -bench-bytes 1073741824 [-net-offload=false]   # the gate
hb-dev.exe run ... -net-stats 2s                                                     # gVisor's counters
```

## Layer digest mismatches on LAN pulls (2026-11-01)

**Symptom.** Pulling a test image from a LAN registry (a Raspberry Pi on 1 Gbit wired,
`lanbench:1`: three random 256 MiB layers, amd64, FROM scratch), then `docker rmi`, back
to back, failed 1 time in 10 with:

```
failed commit on ref "layer-sha256:d6e3...": commit failed: unexpected commit digest
sha256:<random>, expected sha256:d6e3...: failed precondition
```

containerd logged `(*service).Write failed` with the right total (268517545). With a 15 s
sleep between `rmi` and the next pull, 0 of 10 failed. Each `rmi` was followed by a
delete-triggered `fstrim` of 1.2-2.5 GiB and a guest page-cache reclaim, so the data disk
was the first suspect (REQ-DISK-003).

**Cause: the host's queue to the guest sent a buffer it was overwriting.** It was not
the disk. `guestLink` (`internal/netstack/link.go`) queues frames for the guest in
`pending` and writes the whole queue with one hvsock write, while the stack keeps
queueing into a second buffer, `spare`. After a write, the written buffer became the
next spare only if it was not larger than about the queue bound (1 MiB plus a frame). A
queue that grew past that under backpressure was dropped, and `spare` was left as it
was: pointing at the array that had just become `pending`. The next write then sent
`pending` while `enqueue` appended into the same array from offset 0. Whatever the
Windows socket had not read yet was replaced by later frames.

The guest cannot notice. In offload mode the host sends TCP segments with only the
pseudo-header checksum (`NEEDS_CSUM`) and the guest kernel trusts them, as it trusts a
virtio NIC. So a frame whose headers belonged to one segment and whose payload, or part
of it, to another was accepted. The layer arrived complete, with the right size and the
wrong digest, and containerd's commit check (it hashes the stream as it is written) was
the first thing to see it.

The queue only grows past the bound when the guest stops reading the link for a few
milliseconds while a fast download is running, which is why the failures followed
`rmi`: removing 1.5 GiB of snapshots and blobs, the trim and the cache reclaim keep the
guest's vCPUs busy, and `netfwd` gets descheduled. That is consistent with the 15 s sleep
avoiding it; the scheduling of `netfwd` itself was not measured. The bug came with the
throughput work (d8ddbbd kept spares up to 4 MiB; 2bbd7fa lowered the bound to about
1 MiB, which made a dropped spare, and so the alias, far more frequent).

**Experiments** (same machine as above; product engine `hyperbutler start`, 4 GiB, 8
vCPUs, containerd 2.3.7, docker 29.9.2, test home under `%TEMP%\hb-dev\integrity\`):

| Experiment | Engine | Result |
|---|---|---|
| `pull`/`rmi -f` back to back, no sleep | before the fix | 0 of 70 failed; a trim after almost every `rmi` |
| Same, with `netfwd` stopped (`SIGSTOP`) for 2-41 ms, then run for 1-30 ms, during the pull, on an engine that had already pulled | before | 0 of 18 |
| Same with stalls, a fresh engine (counters below) | before, instrumented | 2 failed of 28 pulls (both `unexpected commit digest`, on layers `9ce4...` and `23d5...`) |
| Counters per fresh engine, with stalls: aliased spares (`spare` and `pending` sharing an array at the swap) | before, instrumented | 0-42 per engine, in 9 of 10 engines; 36 and 42 in the two engines whose pull failed |
| Same: queue writes whose buffer changed while the write was in flight (CRC before and after `Write`) | before, instrumented | 2, 22, 11 and 1 in 4 of 4 engines; the failure was in the one with 22 |
| Same | fixed, instrumented | 0 aliased spares and 0 changed buffers in 5 engines (large queues still happen: 3-12 per engine); 0 of 10 pulls failed |
| `pull`/`rmi -f` back to back | fixed | 0 of 40 failed |
| Stall pulls, fresh engine per round | fixed | 0 of 20 (10 fresh engines, 2 pulls each) |
| Three parallel `wget` of the layers in the guest, with stalls, `sha256sum` | before, instrumented | 27 of 27 matched; the queue never grew past the bound (busybox `wget` does not drive the link that hard), which is also why the earlier `wget` check passed |

The aliased writes come mostly in the first pull after a start, while the queue's
buffers are still growing; a long-running engine hits them more rarely, when the last
frame of a full queue pushes it past its capacity.

**What it was not:**
- *Storage.* containerd hashes a fresh ingest as the bytes come in, so a disk that
  stored them wrongly cannot change the commit digest; only a resumed ingest is re-read
  from disk. After the failed commits here no ingest was left to resume. A storage stress
  run on the data disk found no mismatch either: 100 random 256 MiB files written through `tee` into `sha256sum`, then re-read after `sync` and `drop_caches` and again with `dd iflag=direct`, all 300 hashes equal, while a second loop wrote and deleted 512 MiB random files (99 times), with an explicit `fstrim` after each delete in the first 60 rounds and only hyperbutlerd's own delete-triggered and periodic trims in the other 40.
- *containerd or dockerd.* The failures follow the link's state, not the content store,
  and stop with the link fixed. No upstream report matched this error after `rmi`.
- *The API proxy.* It does not touch pulls.

**Fix.** `writeLoop` takes the spare when it swaps buffers (`l.pending, l.spare =
l.spare[:0], nil`), so the spare is only ever a buffer that is no longer being written,
or nil. `TestGuestLinkSpareNotLive` replays the exact sequence (frame 18 arrived with
frame 19's payload before the fix) and `TestGuestLinkBackpressureStress` runs free
producers against a stalling reader (211-490 of 3000 frames corrupted before the fix,
0 after).

Still open: the earlier failing pull that took 4.2 s, faster than the line, was not
explained; here a failed commit left no ingest behind to resume from.

**Follow-up: a CRC-32C on every frame (link version 2).** So that the next such bug
drops frames instead of corrupting data, every offload-mode frame now carries a CRC-32C
of its virtio_net_hdr and Ethernet frame, in both directions, computed where the frame
is built and checked before it is delivered (ADR 0006, "Integrity", has the format, the
counters and the modes). The receive path (guest to host, `frame.Pump`) is covered the
same way. What it does against this bug, with 036c062 reverted in a local build:

| Test (fix reverted) | Version 1, no CRC | Version 2 |
|---|---|---|
| `TestGuestLinkBackpressureStress`, per run of 3000 frames | (211-490 frames with another frame's payload, measured before the CRC) | 110-357 whole frames in another's place (a real segment; TCP takes it for a retransmission), 22-80 frames mixed from several, all failing the CRC, 0 passing it |
| `TestGuestLinkSpareNotLive` | frame 18 arrives with frame 19's payload | frame 18 arrives as a whole, intact copy of frame 19 |
| `TestLinkIntegrityUnderStalls`, 20 runs | 2 delivered a segment with other bytes; 12 ended out of sync | 0 delivered one; 8 dropped 1-4 frames for their CRC; 15 ended out of sync |

"Out of sync" is a length read from the middle of a payload: the overwrite starts
mid-frame, and a random length is out of range almost always. The CRC cannot repair the
stream; the length check ends the link, which netfwd reconnects, and nothing corrupt is
delivered. With the fix, all three pass with 0 CRC drops.

On the real VM (this image and engine, `hyperbutler start`, 4 GiB, 4 vCPUs, test home
under `%TEMP%\hb-dev\crc\`): 20 back-to-back `docker pull`/`docker rmi` cycles of the LAN
registry's `lanbench:1` (768 MiB) failed 0 times at 726-865 Mbit/s, and neither the engine
log nor the guest console reported a CRC drop. An engine without version 2 against this
image gets no network; netfwd logs, every time the engine dials, that the host speaks
version 1 and the guest expects 2.

Cost, from `bench/net-perf` against `hyperbutler-dev run -net-crc on|off` on the same
image, 4 boots alternating on, off, on, off, medians of 5 rounds of 5 s:

| Test | On (boot 1, 3) | Off (boot 2, 4) | Engine cores per Gbit/s, on / off |
|---|---|---|---|
| dl | 8.38, 8.05 | 8.76, 8.26 | 0.16, 0.17 / 0.16, 0.16 |
| ul | 7.81, 7.76 | 8.74, 8.08 | 0.18, 0.19 / 0.17, 0.19 |
| pub-dl | 8.04, 7.92 | 8.71, 8.59 | 0.20, 0.20 / 0.19, 0.20 |
| pub-ul | 9.33, 8.88 | 9.75, 9.93 | 0.16, 0.18 / 0.16, 0.16 |

All 8 pairs are lower with the CRC, by 2.5-11% (6.5% on average), more than the 5% the
change was allowed. A 15 s CPU profile of the engine during dl and ul puts
`castagnoliSSE42Triple` and its callers at 1.9% and 1.1% of the engine's CPU (syscalls
are 74-80%), and the CRC itself runs at 33 GB/s (`BenchmarkCRC`), about 3% of a core per
side at 9 Gbit/s. So the engine's own pass does not explain the drop. The guest's pass
in netfwd was not profiled, and this laptop is noisy: the host-only medians of the same
runs, which do not touch the engine, moved between 21 and 41 Gbit/s. 40 bulk runs, 0
corrupt, 0 CRC drops. `-net-crc off` is there to repeat this on a quieter machine.

## Comparison

| | HNS NAT (or ICS) | User-mode stack over hvsock |
|---|---|---|
| Needs elevation? | No, but needs Hyper-V Administrators or the ADR 0004 broker for the HCN calls. A non-member was not tested. | Nothing beyond what HCS already needs |
| Host-wide state | Creates a host vNIC, a WinNAT instance and a /24 that must not collide with LAN or VPN routes. Leaks until reboot if hyperbutler crashes (non-persistent flag). | None: a socket in the hyperbutler process |
| Guest address / DNS | hyperbutler must assign the IP; no DHCP and no DNS at the gateway | Not needed (the stack's virtual DHCP, or the relay) |
| VPN routes | Worked with Tailscale; full-tunnel VPNs untested and a known WSL failure class | Worked; traffic is host-process traffic |
| Split DNS | **Broken** without a host-side DNS forwarder (which would fix it) | Works in the relay; depends on the stack's DNS forwarder |
| Throughput, internet | 294–321 Mbit/s (n=6) | 241–296 Mbit/s (n=3) |
| Throughput, local | 5.5–9.6 Gbit/s over the vNIC (n=8) | 5.3–10.2 Gbit/s raw hvsock (n=6); a real netstack will cost more and is unmeasured |
| Host memory | In HNS and vmwp, outside hyperbutler | The stack's buffers count against hyperbutler's REQ-MEM-001 budget; unmeasured |
| Host services | Guest traffic crosses the vNIC, so inbound firewall rules apply | A host process dials for the guest, so it bypasses inbound firewall rules for the host's own addresses unless the stack refuses them (the relay does) |
| Inbound / LAN (REQ-NET-003) | Natural (NAT port mappings or the vNIC IP) | Host-side listeners forward over hvsock, the same machinery REQ-NET-001 needs |
| Coexistence (REQ-PLAT-004) | Shares HNS and WinNAT with WSL, Docker Desktop and Windows Sandbox; prefix collisions are possible; not tested here | Shares nothing but the hypervisor |
| Protocols | Everything IP | Whatever the stack implements: gvisor-tap-vsock does TCP, UDP and DNS; ICMP is limited |
| Code | ~300 lines of HCN binding | A dependency (gvisor-tap-vsock, Apache-2.0) or our own stack |

## Open items

- The stack now meets the 50% gate on 5 of 6 boots (49-92%, median 69% of raw hvsock
  guest to host; see "Throughput work"). The LAN registry comparison against HNS NAT has
  still not been run (no second machine); ADR 0006 defers it to the release checklist.
  The HNS NAT local numbers above were read with 8 KiB reads and are probably low.
- Host-to-guest through the stack is 28-62% of raw hvsock (raw is 23-29 Gbit/s), and the
  cause of a 10-13 ms stall of other connections when a bulk connection is set up was
  not found.
- Many short connections from a guest to one host destination leave the engine's
  Windows sockets in TIME_WAIT whenever the guest closes first (a reset is passed on and
  leaves none). That is what any relay does; Windows' ephemeral range then bounds the
  sustained rate to one destination.
- Whether a non-member of Hyper-V Administrators can call HCN.
- Full-tunnel VPN clients, and coexistence with WSL and Docker Desktop installed
  (`bench/net-vpn`, `bench/coexist`).
- IPv6 was not tested on either path.
- Sleep and resume: whether hvsock listeners and HNS endpoints survive host sleep.
- The ICS settings WSL uses to get DHCP and a gateway DNS proxy. They matter if HNS NAT
  becomes the fallback.

## Spike code caveats

`spike/net` is throwaway code. Do not copy these parts into the product:
- It opens the serial console by a predictable pipe name, which another local user could
  pre-create. ADR 0004 forbids that.
- Relay tunnels have no idle timeout. Long-lived idle connections are legitimate (stalled
  pulls, `docker logs -f`), so the product stack needs a deliberate policy.
- The relay refuses loopback, link-local, multicast and the host's own interface
  addresses. Its tunnel count is capped.

## Reproducing

```
go build -o %TEMP%\hb-dev\hyperbutler-spike\net\net-spike.exe ./spike/net
set GOOS=linux& set CGO_ENABLED=0& go build -o %TEMP%\hb-dev\hyperbutler-spike\net\guestprobe ./spike/net/guestprobe
net-spike list                     # read-only
net-spike baseline                 # host probe; saves the TLS roots the guest needs
net-spike hns [-ip dhcp] [-type ICS] [-bulk 5201] [-reach host:port,...] [-dnstry ip]
net-spike relay [-bulk 5201] [-reach host:port,...] [-log]
net-spike cleanup                  # deletes only IDs this tool recorded
```

`hns` generates each object's GUID and records it in a state file **before** asking HNS
to create the object. A crash at any point therefore leaves nothing that `cleanup` does not
know about. It deletes the objects at exit or on Ctrl+C, then lists HNS again to verify.
Objects are also created non-persistent, so a reboot removes any leftovers. Deletion refuses any object whose name lacks
the `hyperbutler-` prefix.
