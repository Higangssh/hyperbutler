# Prototype initramfs for ADR 0005. Built by build.ps1 with guest/cmd/mkinitramfs.
# ${BUSYBOX}, ${DOCKER}, ${MODULES}, ${GUESTBIN}, ${CACERTS}, ${E2FS} (the unpacked
# e2fsprogs packages), ${MUSL} and ${PROTO} are passed with -D.

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
dir  etc 0755

nod  dev/console 0600 c 5 1
nod  dev/null 0666 c 1 3

file init ${PROTO}/init 0755
file bin/busybox ${BUSYBOX}/bin/busybox.static 0755
slink bin/sh busybox

file etc/passwd ${PROTO}/rootfs/etc/passwd 0644
file etc/group ${PROTO}/rootfs/etc/group 0644
file etc/hosts ${PROTO}/rootfs/etc/hosts 0644
file etc/ssl/certs/ca-certificates.crt ${CACERTS}/etc/ssl/certs/ca-certificates.crt 0644

# Docker static binaries. The docker CLI and ctr are left out: the host talks to the API.
file usr/bin/dockerd ${DOCKER}/dockerd 0755
file usr/bin/containerd ${DOCKER}/containerd 0755
file usr/bin/containerd-shim-runc-v2 ${DOCKER}/containerd-shim-runc-v2 0755
file usr/bin/runc ${DOCKER}/runc 0755
file usr/bin/docker-init ${DOCKER}/docker-init 0755
file usr/bin/docker-proxy ${DOCKER}/docker-proxy 0755

# Guest binaries cross-compiled from this repo (GOOS=linux).
file usr/bin/agent ${GUESTBIN}/agent 0755
file usr/bin/sockfwd ${GUESTBIN}/sockfwd 0755
file usr/bin/netfwd ${GUESTBIN}/netfwd 0755
file usr/bin/winboxd ${GUESTBIN}/winboxd 0755

# Data disk tools: mke2fs formats a blank data disk, e2fsck checks it before the mount.
# They are musl builds; the libraries are installed under their sonames.
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

# Only the modules listed in modules.txt and their dependencies, decompressed.
tree lib/modules ${MODULES} 0755 0644
