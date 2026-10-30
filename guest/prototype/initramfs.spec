# Stage-1 initramfs (ADR 0005 A2b). Built by build.ps1 with guest/cmd/mkinitramfs.
# It only reaches the system disk: BusyBox, initramfs-init and the boot modules
# (modules-boot.txt). ${BUSYBOX}, ${BOOTMODULES} and ${PROTO} are passed with -D.

dir  bin 0755
dir  proc 0555
dir  sys 0555
dir  dev 0755
dir  newroot 0755

nod  dev/console 0600 c 5 1
nod  dev/null 0666 c 1 3

file init ${PROTO}/initramfs-init 0755
file bin/busybox ${BUSYBOX}/bin/busybox.static 0755

tree lib/modules ${BOOTMODULES} 0755 0644
