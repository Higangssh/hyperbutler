# 0006. Guest networking

- Status: proposed
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

  If the gate fails, the default becomes HNS NAT plus a host DNS forwarder. That needs the
  ADR 0004 broker to gain HCN verbs, and its VPN and coexistence cost has to be recorded.
- **Dependencies:** gvisor-tap-vsock and gVisor's netstack (Apache-2.0) would be recorded
  in `NOTICE`. It is unverified whether upstream `gvproxy` on Windows can listen on a
  per-VM hvsock address without a registry entry. The spike shows `winio.ListenHvsock`
  can, so at worst a small adapter is needed.
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
  - whether HCN works for users outside Hyper-V Administrators.

  These run in `bench/net-vpn`, `bench/coexist` and `bench/noadmin` before 0.1.
- **Revisit if:** the netstack fails the gate, or Microsoft ships a supported per-process
  NAT or DNS-tunnelling API for third-party HCS VMs.