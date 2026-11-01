# 0006. Guest networking

- Status: accepted (2026-10-31), with the LAN-registry gate deferred to the release
  checklist; see "Gate results (2026-10-31)" and "Gate amendment"
- Date: 2026-10-30; accepted 2026-10-31
- Requirements: REQ-NET-001, REQ-NET-002, REQ-NET-003, REQ-NET-004, REQ-PLAT-003, REQ-PLAT-004

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
     - There is no DHCP and no DNS at the NAT gateway, so hyperbutler must configure the guest.
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

1. The guest has one virtual NIC backed by the stack. The host side runs inside the hyperbutler
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
- Networks are created non-persistent and named `hyperbutler-*`.
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
   traffic still see an ordinary socket of the hyperbutler process. Option 1's packets are
   forwarded, which is the WSL failure class. The flip side: the guest inherits hyperbutler's
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
`tap.LinkEndpoint` in `hyperbutler-dev run` (default `-net user`). gvisor-tap-vsock's
`VirtualNetwork` is not used as a whole, because it has no hook to filter destinations,
its switch indexes the Ethernet header of a frame before checking its length, and its DNS
answers every lookup error with NXDOMAIN and falls back to `/etc/resolv.conf`, which does
not exist on Windows. Instead:

- **Link.** The VM document declares the hvsock service of vsock port 1024. Its bind
  and connect descriptors admit the current user only (`vm.Service`). In the guest,
  `guest/cmd/netfwd` creates a persistent TAP `eth0` and relays its frames,
  length-prefixed (`internal/netstack/frame`), over one AF_VSOCK stream. Both sides
  batch (one write carries every frame queued) and read the link ahead of the frame
  handler (`frame.Pump`).
  - **Offload mode** (default since 2026-10-31). The engine adds `hyperbutler.netvnet=1`
    to the kernel command line. `netfwd -vnet` then opens the TAP with `IFF_VNET_HDR` and
    checksum and IPv4 TSO offload, and listens on the port (CID 2 only). The engine
    dials it and sends `frame.Hello`, whose last byte is the link version (2, see
    "Integrity"). Every frame then starts with a CRC-32C and a virtio_net_hdr.
    TCP segments of up to 64 KiB cross the link in both directions with a partial
    checksum: the guest's TSO on one side, gVisor's host GSO on the other. The host
    dials because Windows sends slowly on an accepted hvsock connection (see "Gate
    results (2026-10-31)"). The host accepts headers of the kinds the guest's TAP
    produces (no GSO or TCPv4 GSO, `NEEDS_CSUM`, `DATA_VALID`, ECN) and rejects the rest
    (`frame.ParseVnet`, fuzzed). A frame without GSO must fit the MTU; a GSO frame must
    fit an IPv4 packet. gVisor does not verify checksums on this link
    (`CapabilityRXChecksumOffload`, as before): it terminates every packet and forwards
    none.
  - **Integrity.** Neither end verifies a TCP checksum: the guest kernel takes
    `NEEDS_CSUM` segments as it would from a virtio NIC, and gVisor does not verify
    what the guest sends. hvsock delivers the bytes it is given, so a buffer reused
    while it is still being written corrupted payloads silently (the layer digest
    mismatches of 2026-11-01, `docs/research/guest-networking.md`). Since link version
    2 (2026-11-01) every frame, both ways, carries a CRC-32C (Castagnoli, `hash/crc32`
    with SSE4.2 or the ARMv8 CRC instructions) of its virtio_net_hdr and Ethernet
    frame: `length | CRC-32C | virtio_net_hdr | Ethernet frame`, both fields 4 bytes
    big-endian (`frame/crc.go`). The engine computes it in `guestLink.enqueue` from the
    bytes it is about to queue, netfwd right after each TAP read; the receiver checks it
    before delivering (the engine in `frame.ChunkReader` on `Pump`'s reader goroutine,
    netfwd before the TAP write). A frame that fails is dropped, which TCP recovers by
    retransmitting, and counted: `Stats.FramesCRCDropped` (from the guest) and
    `GuestFramesCRCDropped` (netfwd's count, sent in a link report frame at most once a
    second when it changed); both sides log the first five with size and direction,
    then one line a minute. The CRC cannot help when a corrupted length desynchronizes
    the stream: that fails the length check and ends the link, loudly, and netfwd
    reconnects.
    - *Versions.* The Hello's last byte is the version; the engine offers one and
      netfwd refuses any other than the one it was started for, with a message naming
      both. There is no fallback to version 1: the engine ships its guest image, so a
      mismatch is a broken install, and running on without the CRC would hide it.
    - *Modes.* `NetConfig.CRC`, in `hyperbutler-dev run -net-crc on|off|strict`, which
      passes `hyperbutler.netcrc=0` or `=strict` to the guest. `off` is version 1, kept
      only to measure the cost; `strict` ends the link at a mismatch on both sides, so
      tests and benches fail loudly. `hyperbutler start` always runs `on`.
    - *Cost.* The CRC runs at 33 GB/s on the test laptop (`BenchmarkCRC`). `bench/net-perf`,
      4 engine boots alternating on, off, on, off, medians of 5 rounds of 5 s (container
      Gbit/s, on / off): dl 8.38, 8.05 / 8.76, 8.26; ul 7.81, 7.76 / 8.74, 8.08; pub-dl
      8.04, 7.92 / 8.71, 8.59; pub-ul 9.33, 8.88 / 9.75, 9.93. Every pair is lower with
      the CRC, 2.5-11%, about 6.5% on average, while the engine's cores per Gbit/s stayed
      0.16-0.20 either way. A CPU profile puts the engine's own CRC at 1.9% (dl) and 1.1%
      (ul) of its CPU; most of the engine's time is in syscalls. Where the rest goes (the
      guest's pass in netfwd, or noise: the host-only baseline moved 21-41 Gbit/s
      between the same runs) was not measured.
    - *Evidence.* With 036c062 reverted, `TestGuestLinkBackpressureStress` saw 22-80 of
      3000 frames per run mixed from several frames, every one failing its CRC, and
      `TestLinkIntegrityUnderStalls` delivered no segment with other bytes in 20 runs
      (with version 1, 2 of 20 did). On the real VM, 20 back-to-back pulls of the LAN
      registry's 768 MiB image failed 0 times with 0 CRC drops, and the 40 bench runs
      had 0 drops and 0 corrupt streams.
    - The buffer-ownership tests stay: `TestLinkIntegrityUnderStalls` (now also
      requiring 0 CRC drops), `linkCheck`, `TestPump`, netfwd's relay tests, and
      `bench/net-perf`'s verified streams (`corrupt` must be 0).
  - **Plain mode** (`-net-offload=false`, or a guest image whose netfwd dials). The
    guest dials the host's listener and sends bare Ethernet frames of at most the MTU.
  - A stack keeps the mode of its first link. TCP endpoints created on an offload link
    keep sending segments larger than the MTU.
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
  timeout, and 1024 UDP flows with a 60 s idle timeout.
  - TCP buffers start at 128 KiB. gVisor tunes them (receive moderation, send
    autotuning) up to 1 MiB, but only for 16 connections at a time; every other
    connection is pinned at 256 KiB.
  - A connection holds its token from start to close, idle or not. Once 16
    long-lived connections exist (keep-alive pools, dockerd's registry connections),
    later bulk transfers run with 256 KiB, which costs throughput under load (3-6
    Gbit/s instead of 8-11). The gate numbers were taken on a fresh stack. Handing
    tokens to active connections only is a follow-up.
  - Relayed connections' buffers are therefore at most 16 x 2 x 1 MiB + 496 x 2 x
    256 KiB = 280 MiB (it was 256 MiB with 256 KiB each). Published connections come
    on top: 2 x 256 KiB each, up to 256 per published port and address family. The
    memory is used only for data that is queued.
  - TIME_WAIT is 1 s, not gVisor's 60 s; see "Gate results (2026-10-31)".
- **TCP tuning.** gVisor's RACK loss detection is turned off. With RTTs of tens of
  microseconds and a queue in front of the guest, it declared in-flight segments lost.
  The guest then received each one about six times (`TCPDSACKOldSent`), and host-to-guest
  TCP was capped at 0.18 Gbit/s. With RACK off: 0 retransmits, 4.2–5.4 Gbit/s.
- **Relay.** Copy buffers are pooled. A reset or abort that the relay sees on either
  side (reading or writing) is passed on as a reset, so that neither Windows nor gVisor
  keeps the connection in TIME_WAIT. Windows sockets whose peer is on loopback
  (published ports, `-host-forward` to `127.0.0.1`) get 1 MiB send and receive buffers;
  every other connection keeps Windows' autotuning.
- **Published ports.** `-publish [IP:]HOST:GUEST` (loopback unless `-publish-lan`), and
  auto-publish. Auto-publish follows dockerd's `/events` and mirrors every TCP binding on
  the guest's wildcard address to `127.0.0.1:P` and `[::1]:P`, but refuses a port that a
  host socket already listens on.

## Gate results (2026-10-30)

Windows 11 Home 10.0.26200, non-elevated Hyper-V Administrators account, Wi-Fi uplink,
Tailscale connected, 1024 MB and 2 vCPU guest. `hyperbutler-dev run -bench-bytes 1073741824`
moves 1 GiB per run, three runs per path and direction. The bench servers are in
`netfwd`. "Stack" paths include one Windows loopback hop (the publish listener).
Details are in [guest-networking.md](../research/guest-networking.md#the-user-mode-stack-gvisor-in-hyperbutler-dev).

| Gate | Result | Pass? |
|---|---|---|
| Guest-to-host TCP at least 50% of raw hvsock, same boot | MTU 9000, 256 KiB buffers (shipped): 3.60–3.79 Gbit/s against raw 7.75–13.29 (medians 36–38% on 2 boots). With 1 MiB buffers: 4.69–4.83 against 7.19–14.22 (60%). | **No** with the shipped memory bound |
| Host CPU at most 1 core per Gbit/s | 0.17–0.63 cores per Gbit/s through the stack, both directions | Yes |
| LAN registry pull within 20% of HNS NAT | Not run: no second machine with `registry:2` on a wired link | **Not run** |
| Idle memory within REQ-MEM-001 | hyperbutler-dev with the stack idle: 21 MB working set, 56 MB private; 42 / 77 MB after pulls | Yes for idle; the worst case is bounded at 256 MiB of stack buffers |
| VPN split DNS, public DNS and a VPN-only address in one run | `peer-a.<tailnet>.ts.net` and the bare `peer-a` resolved to 100.64.0.10, `registry-1.docker.io` resolved, a missing tailnet name gave NXDOMAIN, and TCP to tailnet peers by IP and by MagicDNS name connected. The host's own Tailscale address was refused. | Yes (Tailscale only) |
| p99 small-request latency (ADR 0003) | 64-byte round trips, n=20000. Idle: stack p99 239–271 µs (raw hvsock 89–154 µs). Under bulk load: stack p99 444–688 µs, p99.9 11.6 ms (raw 124–273 µs, p99.9 0.7–2.0 ms). | Recorded. The p99.9 tail under load (about 11 ms on every run) is open: GC or the 1 MiB guest-bound queue. |

At that point ADR 0006 stayed proposed. Two gates were not met: the 50% throughput gate,
under the 256 KiB buffer bound the review asked for, and the LAN registry gate, which was
not run.

## Gate results (2026-10-31)

Same machine and account; 2048 MB and 2 vCPU guest, engine with the product's 256 MiB Go
memory limit. The work is in [guest-networking.md](../research/guest-networking.md#throughput-work-2026-10-31).

**Why the gate failed.** Profiles showed the engine and the guest mostly idle during
bulk transfers; the stack was waiting, not computing. Four causes, each measured on its
own:
1. Containers' 1500-byte MTU, which put every container connection at a 1460-byte MSS
   over the 9000-byte link. Fixed by segmentation offload: 64 KiB segments both ways.
2. Slow host writes on the hvsock connection Windows had accepted: about 1 ms per
   600 KB, against 50 µs on a connection the host dialed. Fixed by having the host dial.
3. Autotuned loopback Windows sockets, on which the relay waited 67% of the time. Fixed
   with 1 MiB buffers on sockets to this host.
4. 256 KiB TCP windows against an RTT of 1-5 ms under load (queueing in front of the
   guest). Fixed by letting 16 connections at a time tune up to 1 MiB.

**Not a cause:** the GC (1-3 per 5 s bulk run, pauses below the 0.5 ms clock
resolution, 30-36 MB of heap), the relay's syscalls per frame, copy buffer sizes, and
splitting the link into one connection per direction.

| Gate | Result | Pass? |
|---|---|---|
| Guest-to-host TCP at least 50% of raw hvsock, same boot | 6 boots, each the median of 5 x 1 GiB: **80, 74, 49, 56, 92, 64%** (median 69%; before: 36-38% on 2 boots). The stack ran at 6.9-10.6 Gbit/s; raw hvsock varied from 8.0 to 21.7 Gbit/s between boots, and the one boot under 50% was the one with the fastest raw run (stack 10.56 against raw 21.70). Without offload: 36%. Host to guest (not gated): 28-62% of a raw 23-29 Gbit/s. | **Yes**, on 5 of 6 boots and on the median |
| Host CPU at most 1 core per Gbit/s | The engine: 0.11-0.20 cores per Gbit/s for every bulk path. The guest's share (vmmem) cannot be read by a non-elevated user; the guest was at about 30% of its 2 vCPUs during bulk transfers, which adds about 0.06 cores per Gbit/s at 10 Gbit/s. | Yes (engine measured, guest estimated) |
| LAN registry pull within 20% of HNS NAT | Deferred to the release checklist, then run 2026-11-01: stack 929-931 Mbit/s against the host's own 916-941 (99%), docker pull 0 failures in 30; see "Gate amendment". | **Yes** (against the host's rate) |
| Idle memory within REQ-MEM-001 | `bench/mem-idle` on this branch (`hyperbutler start`, 4 GiB / 8 vCPU): median 316.5 MiB (332 MB), engine 19.1 MiB (was 315 / 17.7 MiB). Under load the engine's private working set was 46-55 MB in every bulk, latency and connection-rate run (Go heap 30-36 MB). Before, it was 180-260 MB after a connection-rate run. | Yes |
| VPN split DNS, public DNS and a VPN-only address in one run | Rechecked through `hyperbutler start` with the new image: `peer-a` resolved to 100.64.0.10 and `registry-1.docker.io` resolved, in the same run; HTTPS from a container; `-p` on `127.0.0.1` and `[::1]`; `docker pull`. | Yes (Tailscale only) |
| p99 small-request latency (ADR 0003) | Gate bench, n=20000: stack idle p50/p99/p99.9 158/257/504 µs (raw 52/77/124); under bulk load 147/244/**491** µs (raw 52/96/259), max 2.6 ms. Containers (`bench/net-perf`): rr 187/392/1244 µs, during a download 356/1418/3093, `-p` 233/421/1196. | Recorded |

The p99.9 of 11.6 ms under load came from the bench, which reconnected its bulk load
every 256 MiB. The p99.9 of 20,000 samples is the 20th worst, and there were about 20 new
load connections per measurement. With one load connection the tail is 0.49 ms. Raw
hvsock under the old reconnecting load stayed at 0.5-0.7 ms, so a new bulk connection
does stall others for 10-13 ms somewhere in the stack path. The place was not found;
it is an open item.

Container paths, `bench/net-perf` (medians of 5; the baseline is the host's own
loopback, which varies from 8 to 25 Gbit/s):

| Path | Before | After |
|---|---|---|
| Container downloads from a host server | 2.71 Gbit/s | **10.2** |
| Container uploads to a host server | 3.34 | **8.29** |
| Host downloads from a container (`-p`) | 3.19 | **7.54** |
| Host uploads to a container (`-p`) | 4.45 | **11.6** |
| Container connections/s to a host server | 640, then 4-7 (failing; see below) | **6024**, no errors |
| `-p` connections/s | 4914 | 5475 |
| Internet HTTPS download (link-limited) | = host | = host |

Connections per second had failed outright. gVisor kept each closed connection in
TIME_WAIT for 60 s, and answered a SYN that reused its port from that endpoint, because
it hands SYNs over only to a listener, not to a forwarder. TIME_WAIT is now 1 s. Linux
picks a guest's ports with a stride of 2 per destination, so it reuses a port to the
same destination after about 14k connections, which at 1 s takes more than 14k
connections per second. The gVisor behaviour itself is unchanged: the failure is
bounded, not removed.

### Gate amendment

The LAN-registry gate needs a second machine on a wired link, which the project does not
have; it has not run in two attempts. It was there to show that the user-mode stack does
not make LAN pulls slower than HNS NAT. It is **deferred, not met**, and it stays
release-blocking (docs/release.md, ADR 0006 section).

What the local numbers say, and why they do not replace it:
- HNS NAT's local throughput was measured on this machine on 2026-10-30 in one direction
  only, guest to host: 5.5-9.6 Gbit/s (n=8, median 8.4).
- The stack in that direction now runs at 6.9-10.6 Gbit/s (6 boots). Into a container,
  the direction of a pull, it runs at 10.2 Gbit/s from a host-local server.
- The NAT harness (`spike/net`) read with 8 KiB reads, which this work found to hold raw
  hvsock at 4-7 Gbit/s. The NAT figure is therefore probably low, and the ranges overlap.
- A LAN of 1-2.5 Gbit/s would limit either path. That is a reason to expect the gate to
  pass, not a measurement.

**ADR 0006 is accepted, with the LAN-registry gate deferred to the release checklist.**
The other five gates are met. The user-mode stack stays the default and HNS NAT stays an
opt-in backend. If the deferred check fails, the fallback in "Consequences" applies.

**LAN gate met (2026-11-01), against the host's own rate.** The run had a Raspberry Pi 5
with `registry:2` on 1 Gbit wired and the host on a 1 Gbit USB adapter. Three 256 MiB
layers were fetched in parallel. The host's `curl.exe` reached 916-941 Mbit/s and a
container through the stack 929-931 Mbit/s (99%). `docker pull` through the stack ran at
735-861 Mbit/s with 0 failures in 30 back-to-back cycles. HNS NAT was not run, because
it needs an HNS network created on the host. NAT is also a VM path, so it cannot beat
the host's own rate. The maintainer accepted the host's rate as the baseline: the stack
within 20% of it is within 20% of NAT. The same run found and fixed a silent
host-to-guest corruption bug (036c062; guest-networking.md, "Layer digest mismatches on
LAN pulls").

## Implementation review

The adversarial reviewer reviewed the implementation on 2026-10-30. It found 10
issues. Each one is fixed or rejected below.

| # | Sev | Finding | Resolution |
|---|---|---|---|
| 1 | High | Auto-publish binds `127.0.0.1:P` for ports the guest picks. Windows lets that specific bind succeed beside a host service on `0.0.0.0:P`, and it takes over the service's loopback clients. | **Fixed:** a port is refused if an IPv4, IPv6-only or dual-stack wildcard probe bind fails. The test covers all three listener kinds; the e2e run confirmed a refused port. (Since replaced: the probe reads the TCP listener tables, which also catch a specific-address listener and open no socket, and a refused port fails the container start; see [compat.md](../compat.md#published-ports).) **Accepted residual:** a guest can still claim a *free* loopback port before a host program does. That is inherent to Docker-style publishing, and the fix (an allow-list from the user's own `POST /containers/create`) belongs to `internal/transport`/`hyperbutlerd`. |
| 2 | Med | Host addresses are cached for 1 s, so a new host address (VPN connect) is reachable in that window. | **Fixed:** after each TCP and UDP dial, a connection to this host (local address equals remote address) is closed. Test: `TestStaleHostAddrsStillDenied`. |
| 3 | Med | The memory bound was about 8 GiB (4096 connections x 2 x 1 MiB). | **Fixed:** 512 connections x 2 x 256 KiB = 256 MiB. This costs throughput (see the gate). |
| 4 | Med | The `/events` decoder could buffer one value of up to 1 TiB. | **Fixed:** events are read line by line with a 64 KiB cap. Test: `TestReadEventLineBounded`. |
| 5 | Med | `[::1]:P` was not published, and `-p 127.0.0.1:P:C` silently published nothing. | **Fixed:** ports are published on `127.0.0.1` and `[::1]` (both verified with curl). Guest-loopback bindings are logged as unreachable. |
| 6 | Low | Any guest process with AF_VSOCK can take over the link. | **Rejected:** that needs guest root or a container without the default seccomp profile, which already controls all guest traffic. The host is unaffected, and netfwd restarts need the replacement. |
| 7 | Low | `WaitGroup.Add` could race `Close`; auto-publish was not joined. | **Fixed:** goroutines register through `track()` under a closing lock; `guestNet.close` cancels and waits for auto-publish. |
| 8 | Low | Auto-publish did not reconcile after the event stream dropped. | **Fixed:** each resync removes containers that are no longer running. |
| 9 | Low | 128 in-flight dials of 15 s stall new connections. | **Fixed:** 512 in flight, 10 s dial timeout. |
| 10 | Low | Single-label names trigger LLMNR/NBNS on the LAN, and PTR reveals host names. | **Rejected:** this is the host resolver's behavior, and REQ-NET-002 wants it (the bare tailnet name resolved through the VPN's search suffix). Recorded in "What stays unverified". |

## Throughput review (2026-10-31)

The adversarial reviewer reviewed the throughput work and this decision. It
found 12 issues and no crash, hang or unbounded growth reachable from the guest.

| # | Sev | Finding | Resolution |
|---|---|---|---|
| 1 | High | The amended LAN gate was "met" against an HNS NAT baseline measured with 8 KiB reads, which this work found to be reader-limited, and in the other direction than a pull. | **Fixed:** the gate is deferred and release-blocking, not met; the bias is stated in "Gate amendment". |
| 2 | Med | "Host CPU" was the engine's CPU only. | **Fixed (documented):** the row says so and estimates the guest's share; a non-elevated user cannot read vmmem. |
| 3 | Med | The idle-memory row was asserted, and the change adds resident link buffers. | **Fixed:** the host reads the link in 4 x 256 KiB chunks (was 4 x 1 MiB) and keeps a spare queue buffer of at most about 1 MiB (was 4 MiB); `bench/mem-idle` was re-run (row above). |
| 4 | Med | A guest process holding vsock port 1024 made netfwd exit every second for good; a guest that dials the host after an offload link logged a warning per attempt. | **Fixed:** netfwd retries the listen with backoff instead of exiting; plain links after an offload link are refused at debug level. The squatting itself is in "What stays unverified". |
| 5 | Med | The 16 large-buffer tokens are held for a connection's life, so the result depends on connection order. | **Documented** under "Limits" as a follow-up. |
| 6 | Med | 1 MiB fixed socket buffers also applied to `-publish-lan` clients and host forwards to other machines. | **Fixed:** only when the peer is on loopback. |
| 7 | Low | The TIME_WAIT safety argument (28k) was imprecise, and the gVisor behaviour remains. | **Fixed (documented):** about 14k, bounded not removed. |
| 8 | Low | Resets seen on writes, and aborts, were not passed on. | **Fixed:** both directions' errors, `WSAECONNABORTED` included. |
| 9 | Low | No test proved the host's partial checksums. | **Fixed:** the fake guest completes each one as the kernel would and checks the TCP checksum (`TestOffloadLink`). |
| 10 | Low | `ChunkReader` spun on a reader returning (0, nil). | **Fixed:** `io.ErrNoProgress` after 100 such reads; test added. |
| 11 | Low | The Hello write had no deadline; "link up" was logged for a refused plain link. | **Fixed:** 5 s write deadline; refused plain links are no longer announced. |
| 12 | Low | The 280 MiB bound covers relayed connections only. | **Fixed (documented):** published connections are stated on top. |

## Host gateway (REQ-NET-004)

Added 2026-10-31. The maintainer's decision: behave like Docker Desktop by default. This
is the explicit host-gateway feature the "Security" consequence below asks for.

**Design.**
- **Address.** The last host address of the guest subnet, `192.168.127.254`
  (`Config.HostGatewayAddr`). The stack answers ARP for it, as for `-host-forward`
  addresses. A `-host-forward` on the same address and port takes precedence.
- **Forward.** A guest TCP connection to it on port P is dialed to `127.0.0.1:P`; if that
  is refused (`WSAECONNREFUSED`, and only then), to `[::1]:P`, because Windows dev servers
  that listen on "localhost" often bind `::1` only. A refused loopback connect takes
  about 0.5 ms on this machine (Windows 11 26200), so the fallback costs nothing
  measurable, and a service that accepts on `127.0.0.1` never sees a second connection.
  UDP goes to `127.0.0.1:P` only: a UDP connect cannot tell that nothing listens.
- **No proxy.** The target is built from the port alone; the guest's destination address
  is only compared with the gateway address, never dialed. The dialer's `Control` hook
  refuses any address other than `127.0.0.1` and `::1` as a second line. Every other
  destination goes through `Policy.Check` as before: loopback, link-local, the subnet and
  the host's own addresses stay refused.
- **DNS.** The stack's resolver answers `host.docker.internal` and
  `gateway.docker.internal` (an alias, as Docker Desktop has both) itself: A is the
  address, other types NODATA. The host resolver is never asked, so neither the LAN's DNS
  nor the hosts file (where Docker Desktop writes the host's LAN address) can answer.
  Off, the names are NXDOMAIN. Containers on the default bridge use the guest's resolver
  and those on user networks dockerd's embedded DNS, which forwards to it, so the name
  resolves in every container and on the guest.
- **`--add-host NAME:host-gateway`.** The engine passes `hyperbutler.hostgw=ADDR` on the
  kernel command line and the guest init starts dockerd with `--host-gateway-ip=ADDR`,
  on or off, so a change needs no dockerd restart. `host-gateway-ip` and
  `host-gateway-ips` are owned daemon.json keys; `bip`, `fixed-cidr` and
  `default-address-pools` that overlap the guest subnet (and so the address) were already
  refused, now with tests on the address itself.
- **Settings.** `hostGateway.enabled` (default true) and `hostGateway.ports` (a port list
  such as `5432,8000-8100`; default `all`; `netstack.ParsePortList`, fuzzed) apply
  **live**: `config set` writes settings.json and sets the engine's settings event; the
  engine re-reads the file, swaps the setting atomically in the stack, and publishes it
  in state.json (`hostGateway`), which the CLI waits for. Connections already relayed
  stay up. An invalid settings.json changes nothing and the state says why. The same
  port list applies to TCP and UDP.

**Security decisions.**
1. **SSRF to host loopback is accepted** (Docker Desktop parity, maintainer decision).
   Every container reaches every service on the host's loopback, which the Windows
   firewall does not filter and which programs often trust. It is documented in the
   README and [docs/compat.md](../compat.md#hostdockerinternal), with the two settings
   to limit or turn it off.
2. **Nothing but loopback.** Other host addresses, the LAN, VPN and link-local addresses
   are not reachable through the gateway (by construction, tested by
   `TestHostGatewayIsNotAProxy` and `FuzzPolicyRouteHostGateway`), and stay refused as
   direct destinations.
3. **DNS rebinding.** An outside name cannot point a container at the gateway: A records
   from the host resolver inside the guest subnet are dropped, CNAME answers that name
   `host.docker.internal` are dropped, and reverse names of the subnet are answered
   locally (which also keeps PTR queries for `192.168.127.x` off the LAN).
4. **Ports.** Port 0 is refused; the port list accepts decimal ports 1-65535 and
   ascending ranges, at most 64 items and 512 bytes, merged into a canonical form.
5. **Floods.** At most 256 host gateway TCP connections (within the stack's 512) and 256
   UDP flows (within 1024) at once; more are reset or dropped. `bench/host-gateway` held
   400 connections from one container and saw a peak of 256 at the host, and the
   gateway worked right after. Each connection to loopback keeps the relay's 1 MiB
   Windows socket buffers (see "Relay"); they are limits, used only by queued data.
6. **hyperbutler's own endpoints.** The Docker API, the debug endpoint and the guest
   control channel are named pipes and hvsock, not TCP; the engine had no TCP or UDP
   listener during the e2e run. Ports auto-published on `127.0.0.1` are reachable
   through the gateway (a hairpin back into the guest, as on Docker Desktop); every hop
   is bounded and ends at docker-proxy, so there is no loop.

**Evidence.** Unit tests in `internal/netstack/hostgw_test.go` and `policy_test.go`;
fuzz targets `FuzzParsePortList`, `FuzzPolicyRouteHostGateway`, and `FuzzDNSAnswer` with
a host gateway and a rebinding name. `bench/host-gateway` (Windows 11 Home 10.0.26200,
non-elevated, `hyperbutler start` with a 3 GiB / 2 vCPU guest on its own pipe, guest
image rebuilt with the init change) passed all 25 checks (log of the last of six runs; the first two failed only on the timing check described under "Open", which then moved to the address); servers bound `127.0.0.1` and
`[::1]` only, so the wildcard leg (`-wildcard`, the host's LAN address against a
`0.0.0.0` server) was not run; that case is covered by `TestStaleHostAddrsStillDenied`
(gated) and the policy tests. `config set` applied in about 0.2 s.

**Review.** The adversarial reviewer reviewed the change on 2026-10-31 and could
not turn the gateway into a proxy. It found 7 issues:

| # | Sev | Finding | Resolution |
|---|---|---|---|
| 1 | Med | The design, this section and the SSRF acceptance were not documented. | **Fixed:** this section, README, docs/compat.md, requirements status. |
| 2 | Med | Host gateway UDP flows had no bound of their own and could take the whole flow table. | **Fixed:** 256 of the 1024 flows. Test: `TestHostGatewayUDPFlowLimit`. |
| 3 | Med | The bench's LAN check cannot tell a policy refusal from an empty port by default. | **Documented:** the real check (`-wildcard`) binds `0.0.0.0`, which the project keeps opt-in because of Windows Firewall prompts; the policy is covered by unit tests. Added: a check that a refused port is a prompt reset, not a timeout. |
| 4 | Low | 1 MiB socket buffers for up to 256 gateway connections. | **Rejected:** socket buffer sizes are limits, filled only by queued data, the same as for published ports; recorded in decision 5. |
| 5 | Low | Three event-polling goroutines wake every 200 ms. | **Accepted:** the existing `StopEvent.Wait` pattern; a single `WaitForMultipleObjects` is a follow-up. |
| 6 | Low | `config set hostGateway.*` against an engine without the settings event failed after saving. | **Fixed:** it says the change applies at the next start. |
| 7 | Low | Leading zeros in port lists were accepted untested. | **Fixed:** accepted and tested (`080` is 80). |

**Open.**
- In 3 of 6 bench runs, the fetch by name of an unlisted port, in a fresh container
  right after a fetch of the listed port, took 5.4 s (the busybox resolver's 5 s retry)
  instead of 0.4 s. The same refusal by address was always prompt (0.4 s, every run),
  and 30 refusals by name in one container never were slow. A lost DNS answer is
  suspected, not found; the bench now times the refusal by address and prints the time
  by name.
- IPv6 inside the guest (AAAA for the name) waits for IPv6 in the stack.
- `docker network create --subnet` overlapping the guest subnet is not checked.
## Consequences

- **Acceptance gate.** Run gvisor-tap-vsock's `gvproxy` on hvsock against the spike guest,
  on a wired link. Record the machine and Windows edition. ADR 0006 is accepted only if
  all of these hold:
  - Guest-to-host TCP throughput through the stack is at least 50% of raw hvsock on the
    same boot.
  - Host CPU is at most one core per Gbit/s.
  - A pull of a large image from a LAN registry (`registry:2` on another machine) is
    within 20% of HNS NAT. *Deferred 2026-10-31* (see "Gate amendment"): a manual,
    release-blocking check in docs/release.md.
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
  feature (`host.docker.internal`) that is designed and reviewed on its own: see "Host
  gateway (REQ-NET-004)", which opens the host's loopback (and only that) by default.
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
  - a guest can claim free host loopback ports through auto-publish (finding 1);
  - in offload mode a guest process with AF_VSOCK that listens on port 1024 before
    netfwd (or between two netfwd restarts) receives the engine's link instead. This is
    the same exposure as implementation finding 6, from the other side;
  - a new bulk connection stalls other connections through the stack for 10-13 ms
    (see "Gate results (2026-10-31)"); the cause was not found;
  - host-to-guest TCP through the stack is 28-62% of raw hvsock (raw is 23-29 Gbit/s);
  - many short connections from the guest to one host destination, closed first by the
    guest, leave the engine's Windows sockets in TIME_WAIT, which bounds the sustained
    rate to that destination by Windows' ephemeral port range (as for any relay; resets
    leave none).

  These run in `bench/net-vpn`, `bench/coexist` and `bench/noadmin` before 0.1.
- **Revisit if:** the netstack fails the gate, or Microsoft ships a supported per-process
  NAT or DNS-tunnelling API for third-party HCS VMs.