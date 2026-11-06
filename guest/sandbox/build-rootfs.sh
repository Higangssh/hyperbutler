#!/bin/sh
# Builds the sandbox image's system disk (ADR 0009) inside the builder container that
# debian.lock pins. guest/sandbox/build.ps1 runs it on a Docker Engine (hyperbutler's own
# works: no WSL).
#
#   build-rootfs.sh DIR
#
# DIR holds debian.lock, init, sbxd, netfwd and modules/<kver>/ (the kernel modules of the
# Docker image this disk boots with); build.ps1 sends them, with this script, as one tar on
# stdin. stdout: a tar with system.erofs and sandbox-packages.tsv (package, version, arch).
# Needs CAP_SYS_ADMIN: mmdebstrap mounts /proc and /sys in the tree while maintainer
# scripts run.
set -eu

[ "$#" -eq 1 ] || { echo "usage: build-rootfs.sh DIR" >&2; exit 2; }
in=$1
field() { awk -v k="$1" '$1 == k { print $2 }' "$in/debian.lock"; }
ts=$(field snapshot)
suite=$(field suite)
pkgs=$(awk '$1 == "packages" { printf "%s%s", sep, $2; sep = "," }' "$in/debian.lock")
case "$ts" in [0-9]*T[0-9]*Z) ;; *) echo "debian.lock: bad snapshot $ts" >&2; exit 2 ;; esac

# The indexes come from the snapshot alone, so the lock decides every version and hash.
# The packages are then fetched from Debian's own mirrors, which are fast, and from the
# snapshot (slow, often 503 under load) only for a file the mirrors no longer carry; the
# priorities say so, since without them apt's mirror method spreads the requests over
# every mirror in a list. Either way apt checks each file against the snapshot's signed
# indexes. A snapshot's Release files are past their Valid-Until by design.
key=/usr/share/keyrings/debian-archive-keyring.gpg
snap=http://snapshot.debian.org/archive
mirrors=/etc/apt/mirrors
mkdir -p "$mirrors"
echo "$snap/debian/$ts" >"$mirrors/debian.list"
echo "$snap/debian-security/$ts" >"$mirrors/security.list"
src1="deb [signed-by=$key] mirror+file:$mirrors/debian.list $suite main"
src2="deb [signed-by=$key] mirror+file:$mirrors/debian.list $suite-updates main"
src3="deb [signed-by=$key] mirror+file:$mirrors/security.list $suite-security main"
aptopt="-o Acquire::Check-Valid-Until=false -o Acquire::Retries=5"
rm -f /etc/apt/sources.list.d/*
printf '%s\n' "$src1" "$src2" "$src3" >/etc/apt/sources.list
# shellcheck disable=SC2086
apt-get $aptopt -qq update >&2
printf 'http://deb.debian.org/debian\tpriority:1\n%s/debian/%s\tpriority:9\n' "$snap" "$ts" >"$mirrors/debian.list"
printf 'http://security.debian.org/debian-security\tpriority:1\n%s/debian-security/%s\tpriority:9\n' "$snap" "$ts" >"$mirrors/security.list"
# shellcheck disable=SC2086
apt-get $aptopt -qq install -y --no-install-recommends mmdebstrap erofs-utils >&2

# The snapshot's own time makes the tree's timestamps a function of the lock.
SOURCE_DATE_EPOCH=$(date -u -d "$(echo "$ts" | sed 's/\(....\)\(..\)\(..\)T\(..\)\(..\)\(..\)Z/\1-\2-\3 \4:\5:\6/')" +%s)
export SOURCE_DATE_EPOCH
root=/rootfs
# --skip=update: the tree uses the snapshot's indexes fetched above, copied in by the
# setup hook (with the mirror lists, wherever apt resolves their path).
mmdebstrap --mode=root --variant=essential --skip=check/empty --skip=update \
	--aptopt='Acquire::Check-Valid-Until "false"' --aptopt='Acquire::Retries "5"' \
	--setup-hook='mkdir -p "$1/var/lib/apt/lists" "$1/etc/apt/mirrors" && cp /var/lib/apt/lists/_etc_apt_mirrors_* "$1/var/lib/apt/lists/" && cp /etc/apt/mirrors/* "$1/etc/apt/mirrors/"' \
	--dpkgopt='path-exclude=/usr/share/man/*' --dpkgopt='path-exclude=/usr/share/info/*' \
	--dpkgopt='path-exclude=/usr/share/locale/*' --dpkgopt='path-include=/usr/share/locale/locale.alias' \
	--dpkgopt='path-exclude=/usr/share/doc/*' --dpkgopt='path-include=/usr/share/doc/*/copyright' \
	--include="$pkgs" \
	"$suite" "$root" "$src1" "$src2" "$src3" >&2

chroot "$root" dpkg-query -W -f '${Package}\t${Version}\t${Architecture}\n' | LC_ALL=C sort >"$in/sandbox-packages.tsv"

# What a read-only root needs: writable state lives on mounts the init makes.
rm -rf "$root/var/cache/apt" "$root/var/lib/apt/lists" "$root/var/log"/* "$root/etc/apt/sources.list" "$root/etc/apt/mirrors"
: >"$root/etc/machine-id"
rm -f "$root/etc/resolv.conf" "$root/etc/hostname"
ln -s ../run/resolv.conf "$root/etc/resolv.conf"
echo sandbox >"$root/etc/hostname"
printf '127.0.0.1\tlocalhost\n127.0.1.1\tsandbox\n::1\tlocalhost ip6-localhost ip6-loopback\n' >"$root/etc/hosts"
mkdir -p "$root/workspace" "$root/data" "$root/var/lib/hyperbutler"

# The agent user. The image has no shadow tools (no passwd package); the lines are written
# as useradd would, with no password (login only through the session's own terminal).
echo 'agent:x:1000:1000:agent:/home/agent:/bin/bash' >>"$root/etc/passwd"
echo 'agent:x:1000:' >>"$root/etc/group"
echo 'agent:!:20000::::::' >>"$root/etc/shadow"

# The session's processes and their entry point.
install -d -m 0755 "$root/usr/lib/hyperbutler"
install -m 0755 "$in/init" "$root/usr/lib/hyperbutler/sandbox-init"
install -m 0755 "$in/sbxd" "$root/usr/bin/sbxd"
install -m 0755 "$in/netfwd" "$root/usr/bin/netfwd"
printf '#!/bin/sh\nexec /usr/bin/tini -s -- /usr/lib/hyperbutler/sandbox-init\n' >"$root/usr/sbin/init"
chmod 0755 "$root/usr/sbin/init"
# The kernel runs /sbin/poweroff when the host asks the VM to shut down; busybox's applet
# signals PID 1 (tini), which passes it to the init.
[ -e "$root/usr/sbin/poweroff" ] || ln -s ../bin/busybox "$root/usr/sbin/poweroff"

# Every command the session init runs must be in the tree: a missing one would only show
# as a failed boot.
for c in tini busybox mount umount modprobe ip blkid mke2fs e2fsck setpriv pkill env cp chown dd tr wc sleep cat; do
	chroot "$root" sh -c "command -v $c" >/dev/null || { echo "the tree has no $c" >&2; exit 1; }
done

# Kernel modules: the decompressed set of the Docker image's build.
rm -rf "$root/usr/lib/modules"
mkdir -p "$root/usr/lib/modules"
cp -a "$in/modules/." "$root/usr/lib/modules/"
# Debian's kmod reads the binary indexes, which the staged set (a BusyBox modules.dep)
# does not have.
for k in "$root"/usr/lib/modules/*; do
	chroot "$root" depmod "${k##*/}"
done

# Every file's time is the snapshot's; --all-root and a UUID from the inputs make the
# image a function of the lock and the files put in.
find "$root" -newermt "@$SOURCE_DATE_EPOCH" -exec touch -h -d "@$SOURCE_DATE_EPOCH" {} +
uuid=$(cat "$in/debian.lock" "$in/sandbox-packages.tsv" "$in/init" "$in/sbxd" "$in/netfwd" | sha256sum |
	sed 's/^\(........\)\(....\).\(...\).\(...\)\(............\).*/\1-\2-4\3-8\4-\5/')
mkfs.erofs --quiet -zlz4hc,12 -x-1 "-T$SOURCE_DATE_EPOCH" -U "$uuid" --all-root -L hyperbutler-sbx \
	"$in/system.erofs" "$root" >&2
du -sh "$root" "$in/system.erofs" >&2
tar -C "$in" -cf - system.erofs sandbox-packages.tsv
