# Docker compatibility notes

Known differences from Docker Desktop and Docker Engine on Linux (REQ-API-001). Only the
networking notes exist so far.

## host.docker.internal

REQ-NET-004, [ADR 0006, "Host gateway"](adr/0006-guest-networking.md#host-gateway-req-net-004).

| | Docker Desktop | hyperbutler |
|---|---|---|
| `host.docker.internal` resolves in every container and on the guest | yes | yes, to `192.168.127.254`; answered by the engine's DNS, never by the host's resolver or hosts file |
| `gateway.docker.internal` | yes | yes, the same address (an alias) |
| `--add-host NAME:host-gateway` | yes | yes (dockerd runs with `--host-gateway-ip=192.168.127.254`) |
| TCP to it on port P reaches | the host | `127.0.0.1:P` on Windows, or `[::1]:P` if `127.0.0.1:P` refuses |
| UDP to it on port P reaches | the host | `127.0.0.1:P` only (no `[::1]` fallback: UDP cannot tell that nothing listens) |
| The host's LAN, VPN and other addresses through it | not checked | no: only loopback, on the same port |
| AAAA for the name | not checked | none (the guest network is IPv4 only) |
| Turn it off, limit ports | not that we know of | `hostGateway.enabled`, `hostGateway.ports`; live, no restart |
| Concurrent connections through it | not checked | 256 TCP, 256 UDP flows; more are refused (reset) |

**Security.** With the default (on, every port), **every container can reach every
service listening on the Windows host's loopback**, as on Docker Desktop. Programs often
trust loopback clients: database and cache servers without passwords, development
servers, admin and debug ports of other tools, a Docker Engine exposed on
`tcp://localhost:2375`. A compromised or malicious image, or a page that a browser in a
container loads, can use them. The Windows firewall does not filter loopback. Limit it
with `hyperbutler config set hostGateway.ports 5432,8000-8100`, or turn it off with
`hyperbutler config set hostGateway.enabled false`.

hyperbutler's own endpoints are not on TCP: the Docker API, the debug endpoint and the
control channel are a named pipe and hvsock. Ports published with `docker run -p` are on
`127.0.0.1` and are reachable through `host.docker.internal` too, as on Docker Desktop.

When it is off, connections to `192.168.127.254` are refused, the names give NXDOMAIN
(not the host's hosts file, where Docker Desktop writes the host's LAN address), and
`--add-host NAME:host-gateway` still maps NAME to `192.168.127.254`, which then refuses.

**Not covered:** a `docker network create --subnet` that overlaps `192.168.127.0/24` cuts
the guest off from the host gateway (daemon.json's `bip`, `fixed-cidr` and
`default-address-pools` are checked; `docker network create` is not).
