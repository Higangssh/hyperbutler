// Package notelemetry is the static half of the REQ-UX-005 check ("No telemetry"):
// with default settings, hyperbutler makes no network requests of its own. Its tests
// run in the normal `go test ./...` job, on Windows and Linux; the runtime half is the
// manual bench/no-telemetry.
//
// The tests fail when production code (cmd/hyperbutler, cmd/hyperbutler-broker,
// internal/..., guest/cmd/... and guest/internal/...; not _test.go files, bench/,
// spike/ or cmd/hyperbutler-dev) gains a way to reach the network that nobody has
// reviewed:
//
//   - ScanGo parses the Go sources and reports every use of an API that opens a
//     connection, sends a datagram or resolves a name: net.Dial*, net.Dialer,
//     net.Resolver, the net.Lookup* and net.Resolve*Addr functions, UDP and IP
//     listeners (net.ListenUDP, ListenPacket, ListenIP: they send too), the net/http
//     client (Client, Transport, Get, ...), httputil's reverse proxy, crypto/tls,
//     net/smtp and net/rpc dialing, miekg/dns's client, raw socket connects outside
//     AF_VSOCK and AF_UNIX, exec of network tools (curl, wget, ...; named as the
//     program or in any literal argument, as in cmd /c curl or powershell -c
//     Invoke-WebRequest) or of a program named at run time (the engine, dockerd, the
//     user's docker.exe: code this scan cannot see), the WinHTTP, WinINet, URLMon,
//     Winsock (ws2_32) and DNS client (dnsapi) DLLs, URL literals naming any host but
//     the local Docker API ("docker"), the debug endpoint ("diag") or loopback, dot
//     imports of these packages and of os/exec (their identifiers carry no package
//     name), and cgo. Calls whose arguments are literally local (net.Dial("unix", ...),
//     a loopback address, unix.Connect to a SockaddrVM) pass on their own.
//   - Every other finding needs a line in allowlist.txt, which names the file, the
//     API, how many uses of it the file has, and why they cannot leave the machine on
//     their own (they dial an hvsock or a pipe, or relay the guest's own traffic). The
//     count is exact, so one reviewed net.Dialer does not cover the next one added to
//     the file, and an entry that no longer matches anything fails too.
//   - The modules linked into the shipped binaries (hyperbutler.exe and the guest's
//     agent, sockfwd, netfwd and hyperbutlerd, from `go list -deps`) must each be on a
//     reviewed list, so a new dependency that phones home cannot arrive unnoticed.
//   - The guest's init scripts may not run network tools (an NTP client, a DHCP
//     client, wget, ...) or turn on dockerd's metrics or OpenTelemetry export.
//
// The package has no non-test API beyond ScanGo and ScanShell; it exists for its tests.
package notelemetry
