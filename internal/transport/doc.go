// Package transport carries the Docker API from the Windows host to dockerd in the guest
// (REQ-API-001).
//
// The host side listens on a named pipe (DefaultPipeName by default) that only the current
// user can open. Every accepted connection is forwarded byte for byte to an upstream
// connection opened by a Dialer: HvsockDialer (AF_HYPERV, VM ID + service GUID) in
// production, a TCP dialer in tests.
//
// Forwarding is protocol-agnostic and preserves half-close: when one side finishes
// sending, the proxy calls CloseWrite on the other side and keeps the reverse direction
// open. Docker attach, exec, and any hijacked or upgraded HTTP connection depend on this,
// because the client closes stdin while it is still reading output.
//
// The one exception is Proxy.RewriteCreate (rewrite.go, ADR 0007): with it set, the
// client-to-guest direction follows HTTP request boundaries so that container create
// bodies can be rewritten; every other byte, and the whole guest-to-client direction,
// still passes unchanged.
//
// The named-pipe listener and the hvsock dialer are Windows-only. Proxy, Dialer and the
// address parsers build on every platform.
package transport
