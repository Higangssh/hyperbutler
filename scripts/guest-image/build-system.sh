#!/bin/sh
# Linux port of guest/prototype/build.ps1: the ADR 0005 A2b image (kernel, stage-1
# initramfs, read-only erofs system disk, manifest). It is the default `build-script` of
# .github/workflows/guest-image.yml.
#
# Build-script contract (what guest-image.yml provides, and what any replacement must
# accept):
#   HYPERBUTLER_SRC    repository root (read-only)
#   HYPERBUTLER_CACHE  inputs from sources.lock, already fetched and verified by
#                      fetch-verify.sh, at $HYPERBUTLER_CACHE/dl/<name>/<file>
#   HYPERBUTLER_WORK   empty scratch directory
#   HYPERBUTLER_OUT    directory that must receive the files named in the `outputs` input
#   Go (go.mod version) on PATH, GOTOOLCHAIN=local, GOFLAGS=-mod=readonly.
#   erofs-utils installed at the version ci-entry.sh pins (the one build.ps1 uses).
#   Runs as root (mkfs-erofs.sh chroots into the tree).
# It must not download anything that is not in the lock (Go modules excepted: go.sum
# verifies them).
#
# Keep the steps in sync with build.ps1: the same specs, module lists, guest binaries, Go
# flags and mkfs-erofs.sh, so both produce the same bytes from the same inputs and Go
# version.
set -eu

: "${HYPERBUTLER_SRC:?}" "${HYPERBUTLER_CACHE:?}" "${HYPERBUTLER_WORK:?}" "${HYPERBUTLER_OUT:?}"
proto="$HYPERBUTLER_SRC/guest/prototype"
dl="$HYPERBUTLER_CACHE/dl"
x="$HYPERBUTLER_WORK/x"
stage="$HYPERBUTLER_WORK/stage"
name=initramfs-stage1.gz
compress=lz4hc,level=12

fail() { echo "build-system: $*" >&2; exit 1; }

# The one file in $dl/<name>/.
input() {
    set -- "$dl/$1"/*
    [ "$#" -eq 1 ] && [ -f "$1" ] || fail "expected exactly one verified input in $dl/$1"
    printf '%s' "$1"
}

expand() { # archive dest
    rm -rf "$2"
    mkdir -p "$2"
    # .apk files are concatenated gzip members that read as one tar stream. Alpine adds
    # PAX keywords GNU tar does not know; they carry checksums apk already verified.
    tar --warning=no-unknown-keyword -xzf "$1" -C "$2"
}

command -v mkfs.erofs >/dev/null || fail "mkfs.erofs not found (ci-entry.sh installs erofs-utils)"

# --- unpack ---------------------------------------------------------------------------
expand "$(input linux-virt)" "$x/linux-virt"
expand "$(input busybox-static)" "$x/busybox-static"
expand "$(input ca-certificates-bundle)" "$x/ca-certificates-bundle"
for n in docker-engine containerd runc libseccomp tini-static; do
    expand "$(input "$n")" "$x/$n"
done
e2fs="$x/e2fsprogs"
expand "$(input e2fsprogs)" "$e2fs"
for n in e2fsprogs-libs libblkid libcom_err libeconf libuuid; do
    expand "$(input "$n")" "$x/$n"
    cp -a "$x/$n/usr" "$e2fs/"
done
expand "$(input musl)" "$x/musl"

# --- kernel modules -------------------------------------------------------------------
set -- "$x/linux-virt/lib/modules"/*/
[ "$#" -eq 1 ] || fail "expected one kernel version in linux-virt, found $#"
kmod=${1%/}
kver=${kmod##*/}

# stage_modules DEST LIST...: the modules named in the lists plus their modules.dep
# closure, decompressed into DEST/<kver> with a matching modules.dep. Module names treat
# - and _ alike. Paths are sorted bytewise (LC_ALL=C), like build.ps1's ordinal SortedSet.
stage_modules() {
    dest=$1
    shift
    sdir="$dest/$kver"
    rm -rf "$dest"
    mkdir -p "$sdir"
    lists=
    for l in "$@"; do lists="$lists $proto/$l"; done
    # shellcheck disable=SC2086 # $lists is a list of paths without spaces
    LC_ALL=C awk '
        function norm(p,   n) { n = p; sub(/^.*\//, "", n); sub(/\.ko(\.gz|\.xz|\.zst)?$/, "", n); gsub(/-/, "_", n); return n }
        FNR == NR {
            split($0, a, ":"); path = a[1]; deps[path] = a[2]; byname[norm(path)] = path; next
        }
        {
            m = $0; gsub(/^[ \t]+|[ \t\r]+$/, "", m)
            if (m == "" || m ~ /^#/) next
            k = m; gsub(/-/, "_", k)
            if (!(k in byname)) { print "module " m " (" FILENAME ") is not in modules.dep (built in, or misspelled?)" > "/dev/stderr"; bad = 1; next }
            q[++qt] = byname[k]
        }
        END {
            if (bad) exit 1
            for (qh = 1; qh <= qt; qh++) {
                p = q[qh]
                if (p in seen) continue
                seen[p] = 1
                nd = split(deps[p], d, " ")
                for (i = 1; i <= nd; i++) if (d[i] != "") q[++qt] = d[i]
            }
            for (p in seen) print p
        }
    ' "$kmod/modules.dep" $lists | LC_ALL=C sort >"$dest.want"

    : >"$sdir/modules.dep"
    count=0
    while read -r p; do
        plain=${p%.gz}
        mkdir -p "$sdir/$(dirname "$plain")"
        gzip -dc "$kmod/$p" >"$sdir/$plain"
        deps=$(LC_ALL=C awk -v p="$p" '
            BEGIN { FS = ":" }
            $1 == p {
                n = split($2, d, " "); s = ""
                for (i = 1; i <= n; i++) { t = d[i]; sub(/\.gz$/, "", t); s = s (s == "" ? "" : " ") t }
                print s; exit
            }' "$kmod/modules.dep")
        if [ -n "$deps" ]; then
            printf '%s: %s\n' "$plain" "$deps" >>"$sdir/modules.dep"
        else
            printf '%s:\n' "$plain" >>"$sdir/modules.dep"
        fi
        count=$((count + 1))
    done <"$dest.want"
    cp "$kmod/modules.builtin" "$kmod/modules.order" "$sdir/"
    echo "modules $kver ($*): $count files"
}
stage_modules "$stage/modules-boot" modules-boot.txt
stage_modules "$stage/modules" modules-boot.txt modules.txt

# --- tools and guest binaries ---------------------------------------------------------
tools="$HYPERBUTLER_WORK/tools"
guestbin="$HYPERBUTLER_WORK/guest-bin"
mkdir -p "$tools" "$guestbin"
cd "$HYPERBUTLER_SRC"
for t in mkinitramfs mksysdisk; do
    CGO_ENABLED=0 go build -trimpath -buildvcs=false -o "$tools/$t" "./guest/cmd/$t"
done
for cmd in agent sockfwd netfwd hyperbutlerd; do
    GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
        go build -trimpath -buildvcs=false -ldflags '-s -w -buildid=' -o "$guestbin/$cmd" "./guest/cmd/$cmd"
done

# --- the system disk ------------------------------------------------------------------
cpio="$stage/system.cpio"
"$tools/mkinitramfs" -spec "$proto/system.spec" -mtime 0 -level 0 -o "$cpio" \
    -D "PROTO=$proto" -D "BUSYBOX=$x/busybox-static" -D "MODULES=$stage/modules" -D "GUESTBIN=$guestbin" \
    -D "DOCKERENGINE=$x/docker-engine" -D "CONTAINERD=$x/containerd" -D "RUNC=$x/runc" \
    -D "TINI=$x/tini-static" -D "SECCOMP=$x/libseccomp" \
    -D "CACERTS=$x/ca-certificates-bundle" -D "E2FS=$e2fs" -D "MUSL=$x/musl"
# The erofs UUID comes from the cpio's SHA-256, formatted like build.ps1 does.
h=$(sha256sum "$cpio" | cut -c1-64)
uuid=$(printf '%s' "$h" | sed -E 's/^(.{8})(.{4}).(.{3}).(.{3})(.{12}).*$/\1-\2-4\3-8\4-\5/')
erofs="$stage/system.erofs"
TMPDIR="$HYPERBUTLER_WORK" sh "$proto/mkfs-erofs.sh" "$uuid" "$compress" "$erofs" <"$cpio"
"$tools/mksysdisk" -erofs "$erofs" -o "$HYPERBUTLER_OUT/system.vhd" -mtime 0

# --- stage 1 and the kernel -----------------------------------------------------------
"$tools/mkinitramfs" -spec "$proto/initramfs.spec" -mtime 0 -level 9 -o "$HYPERBUTLER_OUT/$name" \
    -D "PROTO=$proto" -D "BUSYBOX=$x/busybox-static" -D "BOOTMODULES=$stage/modules-boot"
cp "$x/linux-virt/boot/vmlinuz-virt" "$HYPERBUTLER_OUT/vmlinuz-virt"

# --- the manifest, last ---------------------------------------------------------------
rm -f "$HYPERBUTLER_OUT/image.json"
"$tools/mksysdisk" -manifest "$HYPERBUTLER_OUT/image.json" -kernel "$HYPERBUTLER_OUT/vmlinuz-virt" \
    -initrd "$HYPERBUTLER_OUT/$name" -system "$HYPERBUTLER_OUT/system.vhd"
