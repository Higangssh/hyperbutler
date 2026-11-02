// Package engine runs the hyperbutler VM: it boots the guest through HCS, attaches the
// persistent data disk, serves the Docker API on a named pipe, runs the user-mode
// network stack, and supervises the guest services (the guestctl channel, the
// free-space guard budget and the hard-floor pause).
//
// There are two layers:
//
//   - Run (Windows only) runs one VM until its context ends, then shuts the guest down
//     and stops the VM. It keeps no files of its own except the data disk. cmd/hyperbutler-dev
//     calls it directly.
//   - Serve (Windows only) is the background engine process of `hyperbutler start`. It takes
//     the single-instance lock, cleans up VMs left by a crashed engine, publishes its
//     State in the state directory, waits for a stop request, and calls Run.
//
// The state directory (Layout) and the files in it (Settings, State, DiskMeta) are
// plain JSON written atomically; their parsers are strict and fuzzed, because a crash
// or a user can leave anything there.
//
// The VM's HCS document (kernel command line, disks, hvsock services) is built by
// internal/vmtemplate from typed inputs, the same template the HCS access broker of
// ADR 0004 uses. HCS is reached directly through internal/vm, which needs membership in
// Hyper-V Administrators (ADR 0004). The calls Run makes go through the Backend
// interface.
package engine
