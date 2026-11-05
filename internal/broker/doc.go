// Package broker is the HCS access broker of ADR 0004: the only hyperbutler component that
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
//   - Pipes: the broker creates its pipes with FILE_FLAG_FIRST_PIPE_INSTANCE. The shared
//     endpoint admits interactive users and serves only the version and open-endpoint
//     verbs; open-endpoint creates the caller's own pipe (under a fresh random name), whose ACL admits
//     only that user, and every VM verb is served there alone (Endpoints). The broker
//     never opens a pipe or file name it did not create (rule 5).
//   - Cross-user isolation on hvsock: every service of the VM allows the owner's SID only,
//     and each start makes a fresh VM secret that the guest requires at the start of every
//     connection and that only the owner's own endpoint returns (rule 6).
//   - VM lifetime: the connection that started the VM is its lease; when it ends, the
//     VM is terminated (Implementation decision 6). HCSBackend tags VMs with the owner's
//     SID and sweeps untracked ones (rule 9).
//   - Slots (ADR 0009): each user has the Docker VM (slot 0) and up to proto.MaxSlot
//     agent sandbox sessions, each a VM with its own lease, size, secret and disk
//     (SessionDiskName). A session boots only the sandbox image, against its own pin,
//     and never gets the file server, dockerd's configuration or the host gateway. The
//     memory of one user's running VMs together is capped (Config.MaxTotalMemoryMB).
//   - No caller-controlled host paths: HCSBackend boots only the image next to the broker
//     executable, checked against the compiled-in pin, takes the data disk by handle
//     below the data location, and builds the VM document with internal/vmtemplate from
//     the request's typed fields (rule 1).
//
// Server runs in any process, against any Backend: FakeBackend runs without HCS,
// HCSBackend through it. RunService runs it as the Windows service ServiceName, which
// InstallService registers and UninstallService removes (ADR 0004, "Service (M2)").
// Client is the caller's side.
package broker
