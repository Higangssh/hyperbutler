// Package broker is the HCS access broker of ADR 0004: the only winbox component that
// will call HCS, serving a small, fixed set of requests (package proto) to non-admin
// users over a per-user named pipe.
//
// The design rules it implements:
//
//   - Identity from the OS: the caller is the user SID of the pipe client's token, read
//     by impersonation (rule 4). Messages carry no identity, and per-user state is keyed
//     by that SID.
//   - Data location by handle: the caller passes a directory handle value; the broker
//     duplicates it from the client process, reopens it as the caller, and checks it with
//     ValidateDataDir (rule 2). No request carries a path (rule 1).
//   - Pipes: the broker creates its pipe with FILE_FLAG_FIRST_PIPE_INSTANCE and an ACL
//     that admits only the client user (or interactive users for a shared endpoint), and
//     it never opens a pipe or file name it did not create (rule 5).
//
// This package does not install or register a Windows service. Server runs in any
// process, against any Backend; FakeBackend lets it run without HCS.
package broker
