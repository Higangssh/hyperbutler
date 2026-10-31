// Command net-perf measures the guest network (ADR 0006, REQ-NET-001) from containers,
// against the same measurements run on the host itself:
//
//   - dl, ul: bulk download and upload between a container and a server on the host's
//     127.0.0.1, reached through the engine's -host-forward (the outbound path: guest
//     TCP is terminated in the engine's gVisor stack and dialed again as a Windows
//     socket);
//   - rr, rr-load: 64-byte request/response latency, idle and during a download;
//   - cps: connections per second (connect, one byte each way, the server closes);
//   - pub-dl, pub-ul, pub-rr, pub-cps: the same through `docker run -p` (inbound);
//   - url: an internet download (HTTPS), host against container.
//
// During each container run it records the engine's CPU time, its private working set
// (one sample mid-run and one after) and, with -debug-pipe, its private bytes and
// GC count; -profile saves a CPU profile of the first round of each bulk test.
//
//	hyperbutler-dev run -image DIR\image.json -debug-pipe \\.\pipe\hb-netperf-debug ^
//	    -host-forward 192.168.127.254:5301=127.0.0.1:5301 ^
//	    -host-forward 192.168.127.254:5302=127.0.0.1:5302 ^
//	    -host-forward 192.168.127.254:5303=127.0.0.1:5303 ^
//	    -host-forward 192.168.127.254:5304=127.0.0.1:5304
//	net-perf -docker docker.exe -debug-pipe \\.\pipe\hb-netperf-debug [-rounds 5] [-secs 5]
//
// Servers bind 127.0.0.1 only. It needs Hyper-V, so CI runs only its unit tests.
package main
