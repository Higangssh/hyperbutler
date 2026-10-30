# Security Policy

hyperbutler runs a hypervisor-backed VM and exposes a Docker API endpoint on the host, so security
bugs matter. Examples: named-pipe access by other users, guest-to-host escapes through the
agent or file sharing, and path traversal in mounts.

## Reporting a vulnerability

**Please do not open a public issue.** Report privately through GitHub's
[private vulnerability reporting](https://github.com/Higangssh/hyperbutler/security/advisories/new).

Include the affected version or commit, your Windows edition and build, reproduction steps,
and impact. We aim to acknowledge reports within 7 days.

## Supported versions

Until 1.0, only the latest release gets security fixes.
