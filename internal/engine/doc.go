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
// ADR 0004 uses. Run boots the VM through a VMProvider: by default the in-process one,
// which reaches HCS directly through internal/vm and the Backend interface and needs
// membership in Hyper-V Administrators (developer mode, ADR 0004 option A); with
// Config.Broker the HCS access broker's, which sends only sizes, feature bits and the
// typed network, and returns a Lease that the engine pauses, polls and closes.
package engine
