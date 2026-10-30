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

- The gVisor stack now runs in hyperbutler-dev; see "The user-mode stack" above. It misses the
  50% gate under the 256 KiB buffer bound (medians 36–38% of raw hvsock), and the LAN registry
  comparison against HNS NAT has not been run.
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
