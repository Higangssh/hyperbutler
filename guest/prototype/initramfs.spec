# Prototype initramfs for ADR 0005. Built by build.ps1 with guest/cmd/mkinitramfs.
# ${BUSYBOX}, ${DOCKER}, ${MODULES}, ${GUESTBIN}, ${CACERTS} and ${PROTO} are passed with -D.

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

# Only the modules listed in modules.txt and their dependencies, decompressed.
tree lib/modules ${MODULES} 0755 0644
