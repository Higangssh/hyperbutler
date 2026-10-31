// Command host-gateway checks REQ-NET-004 end to end: containers reach services on the
// Windows host's loopback as host.docker.internal, as in Docker Desktop, and nothing
// else through it.
//
//	host-gateway -hyperbutler hyperbutler.exe -docker docker.exe [-image busybox:1.37]
//	    [-wildcard]
//
// hyperbutler must be running; the bench uses the engine, settings and docker endpoint
// of the current HYPERBUTLER_HOME and DOCKER_HOST (point both at a test install). It
// starts its own servers on 127.0.0.1 and [::1] only (never the wildcard address,
// unless -wildcard: that binds 0.0.0.0, which can make Windows Firewall ask), then
// checks from containers:
//
//   - host.docker.internal:P and gateway.docker.internal:P reach 127.0.0.1:P, on the
//     default bridge and on a user-defined network (dockerd's embedded DNS);
//   - --add-host NAME:host-gateway maps NAME to the same address;
//   - a server on [::1] only is reached when 127.0.0.1 refuses;
//   - UDP to host.docker.internal reaches a 127.0.0.1 UDP server;
//   - other guest-subnet addresses and the host's own LAN address are refused (with
//     -wildcard, against a server that listens on every address, so the refusal is
//     the policy's and not an empty port);
//   - 400 connections held open at once from one container are capped at the engine's
//     limit (256), and the gateway works again afterwards;
//   - `hyperbutler config set hostGateway.enabled false` refuses connections and makes
//     the name NXDOMAIN without a restart (same VM), and turning it on restores both;
//   - `hyperbutler config set hostGateway.ports P` forwards P and refuses other ports.
//
// It prints PASS/FAIL per check and exits 1 if any failed. It restores both settings
// and removes its containers and network at the end.
package main
