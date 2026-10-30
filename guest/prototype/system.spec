# The read-only system disk (ADR 0005 A2b): everything the guest runs. build.ps1 turns
# this spec into an uncompressed cpio with guest/cmd/mkinitramfs, then into an erofs
# image with mkfs.erofs (in a pinned Alpine container) and into system.vhd with
# guest/cmd/mksysdisk. The BusyBox applet links are added in that container
# (busybox --install -s), so they match the pinned BusyBox exactly.
#
# ${BUSYBOX}, ${DOCKERENGINE}, ${CONTAINERD}, ${RUNC}, ${TINI}, ${SECCOMP}, ${MODULES},
# ${GUESTBIN}, ${CACERTS}, ${E2FS}, ${MUSL} and ${PROTO} are passed with -D: each is the
# unpacked package of that name in sources.lock (or a staging directory).

dir  bin 0755
dir  sbin 0755
dir  usr/bin 0755
dir  usr/sbin 0755
dir  proc 0555
dir  sys 0555
dir  dev 0755
dir  run 0755
dir  tmp 1777
dir  root 0700
dir  var 0755
slink var/run ../run
dir  var/lib 0755
dir  var/lib/docker 0710
dir  etc 0755
# The mount point of the bind-mount file system (ADR 0007).
dir  mnt 0755
dir  mnt/hyperbutler 0755
# containerd's "opt" plugin wants /opt/containerd to exist; it stays empty.
dir  opt 0755
dir  opt/containerd 0711

# Mount points and writable state live elsewhere: the root is read-only.
slink etc/mtab ../proc/mounts
slink etc/resolv.conf ../run/resolv.conf
file usr/lib/os-release ${PROTO}/rootfs/usr/lib/os-release 0644
slink etc/os-release ../usr/lib/os-release

file sbin/init ${PROTO}/init 0755
file bin/busybox ${BUSYBOX}/bin/busybox.static 0755
slink bin/sh busybox

file etc/passwd ${PROTO}/rootfs/etc/passwd 0644
file etc/group ${PROTO}/rootfs/etc/group 0644
file etc/hosts ${PROTO}/rootfs/etc/hosts 0644
file etc/ssl/certs/ca-certificates.crt ${CACERTS}/etc/ssl/certs/ca-certificates.crt 0644

# Docker Engine (Moby), containerd, runc and tini from the signed Alpine packages (musl,
# dynamically linked: they use the loader below). The docker CLI and ctr are left out:
# the host talks to the API. docker-engine ships docker-init as a symlink to
# /sbin/tini-static; the file itself is installed here. runc links libseccomp, which is
# installed under its soname like the e2fsprogs libraries.
file usr/bin/dockerd ${DOCKERENGINE}/usr/bin/dockerd 0755
file usr/bin/docker-proxy ${DOCKERENGINE}/usr/bin/docker-proxy 0755
file usr/bin/containerd ${CONTAINERD}/usr/bin/containerd 0755
file usr/bin/containerd-shim-runc-v2 ${CONTAINERD}/usr/bin/containerd-shim-runc-v2 0755
file usr/bin/runc ${RUNC}/usr/bin/runc 0755
file usr/bin/docker-init ${TINI}/sbin/tini-static 0755
file usr/lib/libseccomp.so.2 ${SECCOMP}/usr/lib/libseccomp.so.2.6.1 0755

# Guest binaries cross-compiled from this repo (GOOS=linux).
file usr/bin/agent ${GUESTBIN}/agent 0755
file usr/bin/sockfwd ${GUESTBIN}/sockfwd 0755
file usr/bin/netfwd ${GUESTBIN}/netfwd 0755
file usr/bin/hyperbutlerd ${GUESTBIN}/hyperbutlerd 0755

# Data disk tools: mke2fs formats a blank data disk, e2fsck checks it before the mount.
# They are musl builds (like the engine binaries); the libraries are installed under
# their sonames.
file lib/ld-musl-x86_64.so.1 ${MUSL}/lib/ld-musl-x86_64.so.1 0755
slink lib/libc.musl-x86_64.so.1 ld-musl-x86_64.so.1
file sbin/mke2fs ${E2FS}/sbin/mke2fs 0755
file sbin/e2fsck ${E2FS}/sbin/e2fsck 0755
file etc/mke2fs.conf ${E2FS}/etc/mke2fs.conf 0644
file usr/lib/libext2fs.so.2 ${E2FS}/usr/lib/libext2fs.so.2.4 0755
file usr/lib/libe2p.so.2 ${E2FS}/usr/lib/libe2p.so.2.3 0755
file usr/lib/libcom_err.so.2 ${E2FS}/usr/lib/libcom_err.so.2.1 0755
file usr/lib/libblkid.so.1 ${E2FS}/usr/lib/libblkid.so.1.1.0 0755
file usr/lib/libuuid.so.1 ${E2FS}/usr/lib/libuuid.so.1.3.0 0755
file usr/lib/libeconf.so.0 ${E2FS}/usr/lib/libeconf.so.0.8.3 0755

# The modules in modules.txt and modules-boot.txt plus their dependencies,
# decompressed. Stage 1 loaded the boot ones already; the rest load from here.
tree lib/modules ${MODULES} 0755 0644
