// Package hcn is a minimal binding to the Host Compute Network API (computenetwork.dll),
// the API behind the Windows Host Network Service (HNS).
//
// It covers what the guest-networking spike needs: enumerate, create, query and delete
// networks and endpoints. It is research code for docs/adr/0006-guest-networking.md, not
// a supported hyperbutler interface yet.
//
// Safety rules enforced here rather than left to callers:
//
//   - Create and Delete only accept objects whose name starts with NamePrefix, so code
//     built on this package cannot delete a network that belongs to WSL, Docker Desktop,
//     a VPN client or the Default Switch.
//   - New networks are created with FlagEnableNonPersistent, so HNS drops them at the
//     next reboot even if hyperbutler crashes before cleaning up.
//
// Everything HNS returns (error records, property documents, ID lists) is parsed as
// untrusted input with size limits; see the fuzz tests.
package hcn
