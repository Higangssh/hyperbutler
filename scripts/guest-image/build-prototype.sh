#!/bin/sh
# Linux port of guest/prototype/build.ps1 (the A2a prototype layout: kernel + one
# initramfs). It is the default `build-script` of .github/workflows/guest-image.yml until
# the guest track ships its own Linux build for the ADR 0005 erofs layout; then the
# workflow input points at that script and this file is deleted.
#
# Build-script contract (what guest-image.yml provides, and what any replacement must
# accept):
#   WINBOX_SRC    repository root (read-only)
#   WINBOX_CACHE  inputs from sources.lock, already fetched and verified by
#                 fetch-verify.sh, at $WINBOX_CACHE/dl/<name>/<file>
#   WINBOX_WORK   empty scratch directory
#   WINBOX_OUT    directory that must receive the files named in the `outputs` input
#   Go (go.mod version) on PATH, GOTOOLCHAIN=local, GOFLAGS=-mod=readonly.
# It must not download anything that is not in the lock (Go modules excepted: go.sum
# verifies them).
#
# Keep the steps in sync with build.ps1: the same spec, modules.txt, guest binaries and
# Go flags, so both produce the same initramfs bytes from the same inputs and Go version.
set -eu

: "${WINBOX_SRC:?}" "${WINBOX_CACHE:?}" "${WINBOX_WORK:?}" "${WINBOX_OUT:?}"
proto="$WINBOX_SRC/guest/prototype"
dl="$WINBOX_CACHE/dl"
x="$WINBOX_WORK/x"
name=initramfs-winbox.gz

fail() { echo "build-prototype: $*" >&2; exit 1; }

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

# --- unpack ---------------------------------------------------------------------------
expand "$(input linux-virt)" "$x/linux-virt"
expand "$(input busybox-static)" "$x/busybox-static"
expand "$(input docker-static)" "$x/docker-static"
expand "$(input ca-certificates-bundle)" "$x/ca-certificates-bundle"
e2fs="$x/e2fsprogs"
expand "$(input e2fsprogs)" "$e2fs"
for n in e2fsprogs-libs libblkid libcom_err libeconf libuuid; do
    expand "$(input "$n")" "$x/$n"
    cp -a "$x/$n/usr" "$e2fs/"
done
expand "$(input musl)" "$x/musl"

# --- kernel modules: modules.txt plus the modules.dep closure, decompressed ------------
set -- "$x/linux-virt/lib/modules"/*/
[ "$#" -eq 1 ] || fail "expected one kernel version in linux-virt, found $#"
kmod=${1%/}
kver=${kmod##*/}

stage="$WINBOX_WORK/stage/modules"
sdir="$stage/$kver"
mkdir -p "$sdir"

# Resolve the closure with awk. Module names treat - and _ alike. Output: one path per
# line, sorted bytewise (LC_ALL=C), like build.ps1's ordinal SortedSet.
LC_ALL=C awk -v want="$proto/modules.txt" '
    function norm(p,   n) { n = p; sub(/^.*\//, "", n); sub(/\.ko(\.gz|\.xz|\.zst)?$/, "", n); gsub(/-/, "_", n); return n }
    FNR == NR {
        split($0, a, ":"); path = a[1]; deps[path] = a[2]; byname[norm(path)] = path; next
    }
    {
        m = $0; gsub(/^[ \t]+|[ \t\r]+$/, "", m)
        if (m == "" || m ~ /^#/) next
        k = m; gsub(/-/, "_", k)
        if (!(k in byname)) { print "module " m " is not in modules.dep (built in, or misspelled?)" > "/dev/stderr"; bad = 1; next }
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
' "$kmod/modules.dep" "$proto/modules.txt" | LC_ALL=C sort >"$WINBOX_WORK/modules.want"

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
done <"$WINBOX_WORK/modules.want"
cp "$kmod/modules.builtin" "$kmod/modules.order" "$sdir/"
echo "modules $kver : $count files"

# --- build ----------------------------------------------------------------------------
tool="$WINBOX_WORK/mkinitramfs"
guestbin="$WINBOX_WORK/guest-bin"
mkdir -p "$guestbin"
cd "$WINBOX_SRC"
CGO_ENABLED=0 go build -trimpath -buildvcs=false -o "$tool" ./guest/cmd/mkinitramfs
for cmd in agent sockfwd netfwd winboxd; do
    GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
        go build -trimpath -buildvcs=false -ldflags '-s -w -buildid=' -o "$guestbin/$cmd" "./guest/cmd/$cmd"
done

"$tool" -spec "$proto/initramfs.spec" -mtime 0 -level 9 -o "$WINBOX_OUT/$name" \
    -D "PROTO=$proto" -D "BUSYBOX=$x/busybox-static" -D "DOCKER=$x/docker-static/docker" \
    -D "MODULES=$stage" -D "GUESTBIN=$guestbin" -D "CACERTS=$x/ca-certificates-bundle" \
    -D "E2FS=$e2fs" -D "MUSL=$x/musl"
cp "$x/linux-virt/boot/vmlinuz-virt" "$WINBOX_OUT/vmlinuz-virt"
