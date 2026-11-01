// Command no-telemetry checks REQ-UX-005 (docs/requirements.md) at run time: with
// default settings, hyperbutler makes no network requests of its own except the update
// check (REQ-UX-022), which REQ-UX-005 allows because it can be turned off. Image pulls
// and container traffic are the user's.
//
// The run turns the update check off first (`hyperbutler config set updateCheck false`
// in -home, said in its output), so any connection to api.github.com, like any other,
// fails it. The check itself, one HTTPS GET with a User-Agent and nothing else, is
// covered by internal/updatecheck's tests and listed in internal/notelemetry's
// allowlist.
//
//	no-telemetry -hyperbutler hyperbutler.exe -home %TEMP%\hb-dev\notelemetry\home
//	    -image-dir DIR -docker docker.exe [-docker-config DIR] [-idle 5m]
//	    [-start-args "--pipe \\.\pipe\docker_engine_hb_notel"] [-out report.json]
//	    [-allow-no-vmwp]
//
// It starts the installation in -home with its debug endpoint on, leaves it idle for
// -idle with one container (`-image`, default busybox:1.37, running `sleep 5`) run in
// the middle, and stops it. It needs no administrator rights and installs nothing (no
// packet capture driver), so it watches the only two ways out that hyperbutler has:
//
//   - The engine process: every -interval (250 ms) it reads the system's TCP and UDP
//     tables with their owning PIDs (GetExtendedTcpTable, GetExtendedUdpTable) and
//     records every socket of the engine, its child processes and this VM's vmwp. A
//     TCP connection to a non-loopback address, or a UDP socket not bound to loopback,
//     is a request. A run that cannot identify this VM's vmwp fails, since the VM
//     worker's sockets would go unwatched; -allow-no-vmwp runs without them.
//   - The guest: its only network is the engine's user-mode stack (ADR 0006), which
//     logs every connection, UDP flow and DNS query the guest makes. The bench reads
//     that log from the debug endpoint (/debug/vars/net) at each phase boundary.
//     Anything the guest asks for is a request, whether the stack allowed it or not.
//
// The run is split into phases (boot, idle, container, idle). Everything counts except
// a pull the bench makes itself when -image is missing, which is the user's traffic
// and is shown but not counted. It prints every socket and guest flow it saw, and
// PASS or FAIL; it exits 1 on FAIL.
//
// What it cannot see, and what covers it:
//
//   - A connection shorter than one poll, or one already in TIME_WAIT (whose rows
//     have no PID). The guest's connections are all in the stack's log, which misses
//     nothing; the engine's own code is held by internal/notelemetry, which fails the
//     build on any unreviewed network API.
//   - Which phase an engine socket belongs to, when it relays a guest flow: the engine
//     dials the host side after the guest's SYN, so a pull's last connection can first
//     show up in the poll after the pull ended. An engine socket to an address the guest
//     connected to is judged by that guest flow's phase instead. An engine connection of
//     its own to the same address would hide behind it; internal/notelemetry covers that.
//   - DNS: the engine resolves the guest's names with the Windows resolver, so the DNS
//     Client service (svchost) sends those packets, plus LLMNR, mDNS and NetBIOS for
//     single-label names. They happen only for names the guest asked for, and those
//     are in the stack's log. The engine makes no lookups of its own.
//   - The docker CLI, which is the user's program, is not watched. The bench runs it
//     with a private DOCKER_CONFIG and DOCKER_CLI_HOOKS=0, so plugin hooks (Docker
//     Desktop's scout after a pull) stay out of the run.
//
// It needs Hyper-V, so CI runs only its unit tests.
package main
