# 0006. Guest networking

- Status: proposed (implemented in `internal/netstack`; the acceptance gate is not yet
  met, see "Gate results")
- Date: 2026-10-30
- Requirements: REQ-NET-001, REQ-NET-002, REQ-NET-003, REQ-PLAT-003, REQ-PLAT-004

## Context

The HCS guest from [ADR 0002](0002-primary-vm-backend.md) needs outbound network access,
for example for `docker pull`. This must work on Windows 11 Home, without elevation in
daily use (REQ-PLAT-003).

It must keep working behind VPNs, including split DNS, with zero configuration
(REQ-NET-002). It must also coexist with WSL, Docker Desktop and VPN clients
(REQ-PLAT-004). VPN breakage and the subnet clashes of WSL's NAT network are among the most
reported WSL2 problems (pain point N2).

The measurements are in [guest-networking.md](../research/guest-networking.md). They come
from one Home machine, a non-elevated Hyper-V Administrators account, and Tailscale as the
VPN.

## Options

1. **HNS NAT network and endpoint** (`computenetwork.dll`), attached as a Hyper-V NIC.
   - **Works without elevation:** creating the network took 329 ms (median), the endpoint
     35 ms. The guest had a link at 0.18 s of uptime. Docker Hub downloads were limited by
     the internet link (294–321 Mbit/s, n=6, against one host run at 282). Local throughput was
     5.5–9.6 Gbit/s (n=8).
   - **Problems:**
     - There is no DHCP and no DNS at the NAT gateway, so winbox must configure the guest.
     - Split DNS failed: a VPN-only name gave NXDOMAIN. Fixing it needs a host DNS
       forwarder.
     - It creates host-wide objects (a vNIC, WinNAT, a /24 that must not collide with LAN
       or VPN routes), shared with WSL and Docker Desktop.
     - Full-tunnel VPNs that filter forwarded traffic are a known failure class (untested).
2. **The ICS type, as used by the Default Switch and WSL.** Home has no Default Switch.
   Creating an ICS network ourselves behaved exactly like NAT with these settings: no DHCP
   and no DNS at the gateway. It has the same host-wide-state problems.
3. **A user-mode network stack on the host, reached over hvsock**, as gvisor-tap-vsock
   does for Podman, Rancher Desktop and wsl-vpnkit, and as vpnkit did for Docker Desktop's
   Hyper-V backend.
   - **Prototyped:** a TCP-only CONNECT relay over hvsock, with no NIC in the VM.
     - The hvsock service is declared in the VM's own HCS document (`HvSocketConfig`), so
       there is no admin registry write. The security descriptor is meant to admit only the
       current user; that is not yet verified with a second account.
     - Docker Hub: 241–296 Mbit/s (n=3). Raw hvsock: 5.3–10.2 Gbit/s (n=6).
     - The VPN-routed IP **and** the split-DNS name both worked, because the host resolved
       the names. A real netstack's DNS path is untested.
     - It needs no host-wide state.
   - **Problems:**
     - A real stack (UDP, DNS, ICMP, DHCP for the guest) is a dependency or a lot of code.
     - Its CPU cost and throughput are unmeasured. The relay only bounds the transport.
4. **Mirrored or bridged (External/Transparent switch).** Rejected without testing. It
   attaches the guest to the physical NIC, which changes host networking. On Wi-Fi it is
   unreliable. It also needs the guest to get a LAN address, which public and corporate
   networks often refuse.

## Decision

**Default: a user-mode network stack on the host, reached over hvsock (option 3).** The
first candidate is gvisor-tap-vsock (Apache-2.0).

1. The guest has one virtual NIC backed by the stack. The host side runs inside the winbox
   process (or the ADR 0004 broker's per-user worker), so every connection is a Windows
   socket of that process.
2. DNS from the guest goes to a resolver in the stack, which answers through the Windows
   resolver (`DnsQueryEx`/`GetAddrInfoExW`). NRPT, VPN split DNS and hosts files then apply
   with no configuration.
3. The hvsock services are declared per VM in the HCS document with a per-user security
   descriptor. Nothing is written to the registry.
4. Published ports (REQ-NET-001, REQ-NET-003) are host listeners that forward over the same
   hvsock connection, on `127.0.0.1`, `::1`, and the LAN only when opted in.

**HNS NAT stays an opt-in backend for throughput** (option 1). Possible uses: a future
`--network=hns` flag, or as the fallback if the user-mode stack turns out too slow.
`internal/hcn` keeps the binding. When it is used:
- Networks are created non-persistent and named `winbox-*`.
- The guest gets a static address from the kernel command line or the agent.
- DNS still goes through the same host forwarder, so split DNS keeps working.

### Why

Split DNS does **not** decide between options 1 and 3. Through NAT it failed, and through
the relay it worked. But a host DNS forwarder would fix NAT, and a real netstack's DNS
forwarder could break it for option 3. Both paths need the same forwarder that asks the
Windows resolver. Throughput does not decide it either: both were link-limited on the
internet path, and locally the two overlapped (NAT 5.5–9.6, raw hvsock 5.3–10.2 Gbit/s).

The decision rests on three structural arguments. They are **not yet tested** against
WSL, Docker Desktop or a full-tunnel VPN.

1. **No host-wide state.** Option 3 creates no vNIC, no WinNAT instance and no subnet. So
   it cannot collide with WSL, Docker Desktop, Windows Sandbox or a VPN's routes
   (REQ-PLAT-004, pain point N2). Option 1 shares HNS and WinNAT with all of them.
2. **Traffic is host-process traffic.** VPN clients that refuse or filter forwarded (NATed)
   traffic still see an ordinary socket of the winbox process. Option 1's packets are
   forwarded, which is the WSL failure class. The flip side: the guest inherits winbox's
   per-app firewall and VPN policy, and the stack must refuse the host's own addresses,
   which a host process would otherwise reach past inbound firewall rules.
3. **A smaller privileged surface.** Under ADR 0004, option 1 adds HCN create and delete
   verbs to the privileged broker. Option 3 adds nothing there: the stack runs in the
   user's own process, and its only privileged dependency is the HvSocket service table in
   the VM document, which the broker already builds.

The open risk is the cost of a real netstack on top of hvsock, and whether its DNS path
keeps split DNS. Both are part of the acceptance gate below.

## Adversarial review

The adversarial review was run on this ADR, the research note and the
code on 2026-10-30. It found 13 issues. All are fixed except one, which is accepted for
the spike:

1. **High. The "Why" did not follow from the data.** Split DNS was shown with a
   CONNECT-by-name relay, not a netstack, and the ADR admitted that NAT plus a forwarder
   gets the same result. **Fixed:** "Why" now names the real, untested basis. The gate
   re-runs the VPN checks against the real stack.
2. **High. The throughput gate could not fail,** since both paths are link-limited at
   ~300 Mbit/s. **Fixed:** the gate is now on local throughput relative to raw hvsock, CPU
   per Gbit/s, and a pull from a LAN registry on a wired link.
3. **Medium. The relay let the guest reach host services on the host's own non-loopback
   addresses, bypassing inbound firewall rules.** **Fixed:**
   - `relay.Server` refuses every address assigned to a host interface, read at each dial.
   - `Allowed` also refuses 0.0.0.0/8 and IPv4-compatible IPv6.
   - New test: `TestServerRefusesHostAddress`. The consequence is recorded in "Why" and in
     the research comparison table.
4. **Medium. hvsock per-user isolation was stated but not tested.** **Fixed:** it is marked
   unverified in both documents and assigned to `bench/noadmin`.
5. **Medium. A crash between `HcnCreateNetwork` and recording the ID leaked the object.**
   **Fixed:**
   - `hcn.CreateNetwork` and `CreateEndpoint` take a caller-chosen ID (`hcn.NewID`). The
     spike records the ID before creating and forgets it if the create fails.
   - Ctrl+C now runs cleanup.
   - Non-persistent networks still vanish at reboot.
6. **Low. The VM handle leaked when the asynchronous create failed.** **Fixed:** the handle
   is closed.
7. **Low. The serial console is opened by a predictable pipe name.** **Accepted for the
   spike** and recorded under "Spike code caveats" in the research note, so it is not
   copied. ADR 0004 already forbids it in the product.
8. **Low. `drive()` raced on its results on timeout and ignored scanner errors.** **Fixed:**
   - On timeout it terminates the VM and waits for the reader, instead of closing a pipe
     with a read pending, which can hang.
   - `sc.Err()` is reported.
9. **Low. The relay had no tunnel cap and no write deadline on error replies.** **Fixed:**
   - `MaxTunnels` (default 256), with test `TestServeMaxTunnels`.
   - 5 s write deadlines on the 400 and 502 replies.
   - Idle-tunnel reaping is left to the product design, because idle long-lived tunnels
     are legitimate. Recorded as a caveat.
10. **Low. The `FuzzParseNetwork` invariant was tautological.** **Fixed:** the target now
    checks that any deletable parsed name is the value of a top-level `Name` key in the raw
    document, matched case-insensitively and counting duplicates. That is exactly what the
    delete guard relies on.
11. **Low. Numbers were presented with more confidence than n supports.** **Fixed:**
    - The research note gives n for each figure and drops "as fast as the host".
    - It notes the relay's 15% low run and the different clocks behind 0.19 s and 0.6 s.
    - Two more runs were added after the fixes.
12. **Low. Omissions.** **Fixed:**
    - Host memory of the stack (REQ-MEM-001) is in the gate.
    - The ICS DHCP/DNS settings are an open item for the fallback, no longer "moot".
    - The broker-surface argument is in "Why".
    - Sleep and resume are in the unverified list.
13. **Low. An unused `VsockServiceID(0)` entry was added when `-bulk` was 0.** **Fixed.**

## Implementation

`internal/netstack` embeds gVisor's TCP/IP stack with gvisor-tap-vsock's
`tap.LinkEndpoint` in `winbox-dev run` (default `-net user`). gvisor-tap-vsock's
`VirtualNetwork` is not used as a whole, because it has no hook to filter destinations,
its switch indexes the Ethernet header of a frame before checking its length, and its DNS
answers every lookup error with NXDOMAIN and falls back to `/etc/resolv.conf`, which does
not exist on Windows. Instead:

- **Link.** The VM document declares the hvsock service of vsock port 1024 with a bind
  descriptor for the current user only and a deny-all connect descriptor
  (`vm.Service.BindSDDL`). winbox listens on that service for this VM's runtime ID. In the
  guest, `guest/cmd/netfwd` creates a persistent TAP `eth0` and relays its frames,
  length-prefixed (`internal/netstack/frame`), over AF_VSOCK to CID 2. Both sides reject
  frames shorter than an Ethernet header or longer than the MTU. Both batch: one read on
  either side carries every frame queued.
- **Addresses.** Static, from the kernel command line: guest 192.168.127.2/24, gateway
  and DNS 192.168.127.1, MTU 9000. There is no DHCP server. Containers on `docker0` are
  routed, not NATed: the guest forwards, and the stack's default route points back at the
  guest. dockerd keeps `--iptables=false`; `--network none` is no longer needed.
- **Policy.** Before every dial, and again in the dialer's `Control` hook, the
  destination is refused if it is unspecified, loopback, link-local (including
  169.254.169.254), multicast, broadcast, 0/8, 240/4, an IPv6 form that embeds IPv4
  (`::/96`, 6to4, Teredo, NAT64), the guest subnet, or any address of a host interface
  (re-read at most once a second, failing closed). After the dial, a connection whose
  local and remote addresses are equal (a connection to this host) is closed. Only
  `-host-forward VIRTUAL:PORT=HOST:PORT` entries reach host services.
- **DNS.** UDP and TCP on the gateway. Each query becomes one typed lookup through the
  Windows resolver (`GetAddrInfoW`, `DnsQuery`): A, CNAME, MX, NS, SRV, TXT, PTR. AAAA is
  NODATA (the stack is IPv4 only); other types are NOTIMP. Not found is NXDOMAIN; timeouts
  and other errors are SERVFAIL. 64 queries in flight, 5 s timeout, names restricted to
  LDH, `_` and `.`, answers capped at 64 records and 1232 bytes over UDP.
- **Limits.** 512 relayed TCP connections, 512 TCP handshakes in flight, a 10 s dial
  timeout, 1024 UDP flows with a 60 s idle timeout, and 256 KiB per TCP buffer. Total
  stack buffers are therefore at most 512 x 2 x 256 KiB = 256 MiB.
- **TCP tuning.** gVisor's RACK loss detection is turned off. With RTTs of tens of
  microseconds and a queue in front of the guest, it declared in-flight segments lost.
  The guest then received each one about six times (`TCPDSACKOldSent`), and host-to-guest
  TCP was capped at 0.18 Gbit/s. With RACK off: 0 retransmits, 4.2–5.4 Gbit/s.
- **Published ports.** `-publish [IP:]HOST:GUEST` (loopback unless `-publish-lan`), and
  auto-publish. Auto-publish follows dockerd's `/events` and mirrors every TCP binding on
  the guest's wildcard address to `127.0.0.1:P` and `[::1]:P`, but refuses a port that a
  host socket already listens on.

## Gate results (2026-10-30)

Windows 11 Home 10.0.26200, non-elevated Hyper-V Administrators account, Wi-Fi uplink,
Tailscale connected, 1024 MB and 2 vCPU guest. `winbox-dev run -bench-bytes 1073741824`
moves 1 GiB per run, three runs per path and direction. The bench servers are in
`netfwd`. "Stack" paths include one Windows loopback hop (the publish listener).
Details are in [guest-networking.md](../research/guest-networking.md#the-user-mode-stack-gvisor-in-winbox-dev).

| Gate | Result | Pass? |
|---|---|---|
| Guest-to-host TCP at least 50% of raw hvsock, same boot | MTU 9000, 256 KiB buffers (shipped): 3.60–3.79 Gbit/s against raw 7.75–13.29 (medians 36–38% on 2 boots). With 1 MiB buffers: 4.69–4.83 against 7.19–14.22 (60%). | **No** with the shipped memory bound |
| Host CPU at most 1 core per Gbit/s | 0.17–0.63 cores per Gbit/s through the stack, both directions | Yes |
| LAN registry pull within 20% of HNS NAT | Not run: no second machine with `registry:2` on a wired link | **Not run** |
| Idle memory within REQ-MEM-001 | winbox-dev with the stack idle: 21 MB working set, 56 MB private; 42 / 77 MB after pulls | Yes for idle; the worst case is bounded at 256 MiB of stack buffers |
| VPN split DNS, public DNS and a VPN-only address in one run | `peer-a.<tailnet>.ts.net` and the bare `peer-a` resolved to 100.64.0.10, `registry-1.docker.io` resolved, a missing tailnet name gave NXDOMAIN, and TCP to tailnet peers by IP and by MagicDNS name connected. The host's own Tailscale address was refused. | Yes (Tailscale only) |
| p99 small-request latency (ADR 0003) | 64-byte round trips, n=20000. Idle: stack p99 239–271 µs (raw hvsock 89–154 µs). Under bulk load: stack p99 444–688 µs, p99.9 11.6 ms (raw 124–273 µs, p99.9 0.7–2.0 ms). | Recorded. The p99.9 tail under load (about 11 ms on every run) is open: GC or the 1 MiB guest-bound queue. |

**ADR 0006 stays proposed.** Two gates are not met: the 50% throughput gate, under the
256 KiB buffer bound the review asked for, and the LAN registry gate, which was not run.
Internet pulls are not affected: container downloads matched the host (below).

## Implementation review

The adversarial reviewer reviewed the implementation on 2026-10-30. It found 10
issues. Each one is fixed or rejected below.

| # | Sev | Finding | Resolution |
|---|---|---|---|
| 1 | High | Auto-publish binds `127.0.0.1:P` for ports the guest picks. Windows lets that specific bind succeed beside a host service on `0.0.0.0:P`, and it takes over the service's loopback clients. | **Fixed:** a port is refused if an IPv4, IPv6-only or dual-stack wildcard probe bind fails. The test covers all three listener kinds; the e2e run confirmed a refused port. **Accepted residual:** a guest can still claim a *free* loopback port before a host program does. That is inherent to Docker-style publishing, and the fix (an allow-list from the user's own `POST /containers/create`) belongs to `internal/transport`/`winboxd`. |
| 2 | Med | Host addresses are cached for 1 s, so a new host address (VPN connect) is reachable in that window. | **Fixed:** after each TCP and UDP dial, a connection to this host (local address equals remote address) is closed. Test: `TestStaleHostAddrsStillDenied`. |
| 3 | Med | The memory bound was about 8 GiB (4096 connections x 2 x 1 MiB). | **Fixed:** 512 connections x 2 x 256 KiB = 256 MiB. This costs throughput (see the gate). |
| 4 | Med | The `/events` decoder could buffer one value of up to 1 TiB. | **Fixed:** events are read line by line with a 64 KiB cap. Test: `TestReadEventLineBounded`. |
| 5 | Med | `[::1]:P` was not published, and `-p 127.0.0.1:P:C` silently published nothing. | **Fixed:** ports are published on `127.0.0.1` and `[::1]` (both verified with curl). Guest-loopback bindings are logged as unreachable. |
| 6 | Low | Any guest process with AF_VSOCK can take over the link. | **Rejected:** that needs guest root or a container without the default seccomp profile, which already controls all guest traffic. The host is unaffected, and netfwd restarts need the replacement. |
| 7 | Low | `WaitGroup.Add` could race `Close`; auto-publish was not joined. | **Fixed:** goroutines register through `track()` under a closing lock; `guestNet.close` cancels and waits for auto-publish. |
| 8 | Low | Auto-publish did not reconcile after the event stream dropped. | **Fixed:** each resync removes containers that are no longer running. |
| 9 | Low | 128 in-flight dials of 15 s stall new connections. | **Fixed:** 512 in flight, 10 s dial timeout. |
| 10 | Low | Single-label names trigger LLMNR/NBNS on the LAN, and PTR reveals host names. | **Rejected:** this is the host resolver's behavior, and REQ-NET-002 wants it (the bare tailnet name resolved through the VPN's search suffix). Recorded in "What stays unverified". |

## Consequences

- **Acceptance gate.** Run gvisor-tap-vsock's `gvproxy` on hvsock against the spike guest,
  on a wired link. Record the machine and Windows edition. ADR 0006 is accepted only if
  all of these hold:
  - Guest-to-host TCP throughput through the stack is at least 50% of raw hvsock on the
    same boot.
  - Host CPU is at most one core per Gbit/s.
  - A pull of a large image from a LAN registry (`registry:2` on another machine) is
    within 20% of HNS NAT.
  - Idle host memory of the stack stays within the REQ-MEM-001 budget.
  - With the VPN connected, a container resolves a split-DNS name and a public name in the
    same run, and reaches a VPN-only address.
  - p99 round-trip latency of small requests through the Go stack is recorded, idle and
    under bulk load, next to raw hvsock, so GC pauses show (required by ADR 0003, adversarial review
    finding 4).

  If the gate fails, the default becomes HNS NAT plus a host DNS forwarder. That needs the
  ADR 0004 broker to gain HCN verbs, and its VPN and coexistence cost has to be recorded.
- **Dependencies:** gvisor-tap-vsock, gVisor (Apache-2.0), miekg/dns (BSD-3) and their
  transitive modules are recorded in `NOTICE`. `gvproxy` itself is not used.
  `winio.ListenHvsock` on the VM's runtime ID, declared in the HCS document, needs no
  registry entry and no elevation.
- **Protocol gaps:** ICMP from containers (`ping`) is limited in user-mode stacks. UDP
  needs per-flow NAT in the stack. Document both in `docs/compat.md`.
- **Security:** the stack dials on the guest's behalf as the user. It must refuse loopback,
  link-local and the host's own addresses, except through an explicit host-gateway
  feature (`host.docker.internal`) that is designed and reviewed on its own.
- **What we avoid:** HNS objects, subnet clash handling, WinNAT conflicts with WSL and
  Docker Desktop, and the class of WSL bugs where the VPN breaks the guest. All of this
  is argued, not yet measured.
- **What stays unverified:**
  - full-tunnel corporate VPNs;
  - coexistence with WSL and Docker Desktop installed;
  - IPv6;
  - sleep and resume;
  - per-user enforcement of the HvSocket descriptor;
  - whether HCN works for users outside Hyper-V Administrators;
  - the stack's ICMP: `ping` from the guest is answered by gVisor itself for any address,
    so it shows nothing about reachability; UDP port publishing; IPv6 inside the guest;
  - single-label names and PTR queries go to the host resolver, which may use LLMNR or
    NetBIOS on the LAN;
  - a guest can claim free host loopback ports through auto-publish (finding 1).

  These run in `bench/net-vpn`, `bench/coexist` and `bench/noadmin` before 0.1.
- **Revisit if:** the netstack fails the gate, or Microsoft ships a supported per-process
  NAT or DNS-tunnelling API for third-party HCS VMs.