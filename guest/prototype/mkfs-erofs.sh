#!/bin/sh
# Turns the system tree (an uncompressed newc cpio on stdin, from system.spec) into the
# erofs image of the system disk. Shared by both image builds, so they make the same
# bytes from the same cpio: build.ps1 runs it in the digest-pinned Alpine container on a
# Docker Engine, scripts/guest-image/build-system.sh in the CI container. Both install
# the same pinned erofs-utils first. Runs as root (chroot).
#
#   mkfs-erofs.sh UUID COMPRESS OUT <system.cpio
#
# The BusyBox applet links are added here (busybox --install -s, run from the tree), so
# they match the pinned BusyBox exactly. -T0, --all-root, -x-1 and the UUID (derived
# from the cpio by the caller) make the image a function of the cpio.
set -eu

[ "$#" -eq 3 ] || { echo "usage: mkfs-erofs.sh UUID COMPRESS OUT <cpio" >&2; exit 2; }
uuid=$1
compress=$2
out=$3

root=$(mktemp -d)
chmod 0755 "$root" # the root inode's mode comes from this directory (mktemp makes 0700)
(cd "$root" && cpio -i -d -m 2>/dev/null)
chroot "$root" /bin/busybox --install -s
mkfs.erofs --quiet "-z$compress" -x-1 -T0 -U "$uuid" --all-root -L hyperbutler-sys "$out" "$root" >&2
rm -rf "$root"
