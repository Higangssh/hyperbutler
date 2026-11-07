#!/bin/sh
# Linux port of guest/sandbox/build.ps1: the agent sandbox image (ADR 0009). Runs on the
# Ubuntu runner of .github/workflows/sandbox-image.yml, the only place release images come
# from (ADR 0005, Decision 4).
#
#   build-sandbox.sh GUEST_IMAGE_DIR OUT_DIR
#
# GUEST_IMAGE_DIR is the guest-image artifact, already checked against its SHA256SUMS: the
# kernel and stage 1 this image boots with, and kernel-modules.tar. OUT_DIR must be empty or
# absent. It receives the same names as build.ps1 (vmlinuz-virt, initramfs-stage1.gz,
# system.vhd, sandbox-packages.tsv, image.json, so imageset.Open takes it), then
# BUILDINFO.txt and SHA256SUMS over everything.
#
# Environment:
#   HYPERBUTLER_WITH_SOURCES  1: also build the source bundle (sandbox-sources.sh):
#                             sandbox-sources.tar, or sandbox-sources.tar.000, .001, ... when
#                             it does not fit one GitHub release asset (join them with cat);
#                             sandbox-sources.tsv (its manifest) and SANDBOX-SOURCES.txt.
#   HYPERBUTLER_RELEASE_URL   where the release's assets are; SANDBOX-SOURCES.txt names it
#
# Needs: Go (go.mod version), the docker CLI and an engine with network access
# (snapshot.debian.org, deb.debian.org, security.debian.org), GNU tar and split.
#
# Keep the steps in sync with build.ps1: the same Go flags, input tar and build-rootfs.sh.
set -eu

fail() { echo "build-sandbox: $*" >&2; exit 1; }

[ "$#" -eq 2 ] || { echo "usage: build-sandbox.sh GUEST_IMAGE_DIR OUT_DIR" >&2; exit 2; }
mkdir -p "$2"
[ -z "$(ls -A "$2")" ] || fail "$2 is not empty"
# Absolute: the Go builds run from the repository root.
image=$(CDPATH='' cd -- "$1" && pwd)
out=$(CDPATH='' cd -- "$2" && pwd)
here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
repo=$(CDPATH='' cd -- "$here/../.." && pwd)
sandbox="$repo/guest/sandbox"
lock="$sandbox/debian.lock"

stage=$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/hyperbutler-sandbox.XXXXXX")
trap 'rm -rf "$stage"' EXIT
in="$stage/in"
mkdir -p "$in"

# --- 1. the Docker image and its modules ------------------------------------------------
for f in image.json vmlinuz-virt initramfs-stage1.gz kernel-modules.tar; do
    [ -f "$image/$f" ] || fail "$image has no $f; it must be a guest-image artifact"
done
kernel="$image/vmlinuz-virt"
modules="$image/kernel-modules.tar"
# The bzImage setup header's magic is at 0x202, and its kernel_version field (0x20E, little
# endian) points, less 0x200, at the version string, whose first word names the modules
# directory.
[ "$(dd if="$kernel" bs=1 skip=514 count=4 2>/dev/null)" = HdrS ] || fail "$kernel is not a bzImage"
vp=$(od -An -tu1 -j 526 -N 2 "$kernel" | awk '{ print $1 + 256 * $2 + 512 }')
kver=$(dd if="$kernel" bs=1 skip="$vp" count=256 2>/dev/null | tr '\000' '\n' | LC_ALL=C sed -n '1{s/ .*//p;q;}')
case "$kver" in
    '' | *[!A-Za-z0-9._+-]*) fail "$kernel: bad version string at offset $vp: '$kver'" ;;
esac
tar -tf "$modules" >"$stage/modules.list"
if grep -Eq '^/|(^|/)\.\.(/|$)' "$stage/modules.list" || grep -Evq '^modules/' "$stage/modules.list"; then
    fail "$modules has an entry outside modules/"
fi
kdirs=$(awk -F/ '$2 != "" { print $2 }' "$stage/modules.list" | LC_ALL=C sort -u)
[ "$kdirs" = "$kver" ] || fail "$modules does not hold the modules of kernel $kver alone: $kdirs"
echo "kernel $kver, $(sha256sum "$kernel" | cut -d' ' -f1)"

# --- 2. binaries -------------------------------------------------------------------------
echo "go     $(go env GOVERSION)"
cd "$repo"
CGO_ENABLED=0 go build -trimpath -buildvcs=false -o "$stage/mksysdisk" ./guest/cmd/mksysdisk
mksysdisk="$stage/mksysdisk"
for cmd in sbxd netfwd; do
    GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
        go build -trimpath -buildvcs=false -ldflags '-s -w -buildid=' -o "$in/$cmd" "./guest/cmd/$cmd"
done

# --- 3. the system tree, in the pinned container ----------------------------------------
builder=$(awk '$1 == "builder" { print $2 }' "$lock")
printf '%s\n' "$builder" | grep -Eq '^[^[:space:]]+@sha256:[0-9a-f]{64}$' ||
    fail "debian.lock: the builder is not pinned by digest: $builder"
tar -xf "$modules" -C "$in"
cp "$lock" "$sandbox/init" "$sandbox/build-rootfs.sh" "$in/"
tar --format=ustar --sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner -cf "$stage/in.tar" \
    -C "$in" build-rootfs.sh debian.lock init sbxd netfwd modules
# Unconfined: mmdebstrap mounts /proc and /sys in the tree, and on an AppArmor host (the
# Ubuntu runner) Docker's default profile denies mount even with CAP_SYS_ADMIN.
docker run --rm -i --pull=missing --cap-add SYS_ADMIN --security-opt apparmor=unconfined "$builder" \
    sh -c 'mkdir /in && tar -xf - -C /in && exec sh /in/build-rootfs.sh /in' \
    <"$stage/in.tar" >"$stage/out.tar.part"
mv "$stage/out.tar.part" "$stage/out.tar"
res="$stage/out"
mkdir -p "$res"
tar -xf "$stage/out.tar" -C "$res"
for f in system.erofs sandbox-packages.tsv; do
    [ -s "$res/$f" ] || fail "build-rootfs.sh did not produce $f"
done

# --- 4. the disk, the boot files and the manifest, last ----------------------------------
"$mksysdisk" -erofs "$res/system.erofs" -o "$out/system.vhd" -mtime 0
cp "$res/sandbox-packages.tsv" "$kernel" "$image/initramfs-stage1.gz" "$out/"
"$mksysdisk" -manifest "$out/image.json" -kernel "$out/vmlinuz-virt" -initrd "$out/initramfs-stage1.gz" \
    -system "$out/system.vhd"
# Nothing below reads the tree; the source step may need the disk it took.
rm -rf "$in" "$res" "$stage/in.tar" "$stage/out.tar"

# --- 5. the source bundle ---------------------------------------------------------------
commit=${GITHUB_SHA:-$(git -C "$repo" rev-parse HEAD 2>/dev/null || echo unknown)}
if [ "${HYPERBUTLER_WITH_SOURCES:-0}" = 1 ]; then
    srcin="$stage/srcin"
    mkdir -p "$srcin"
    cp "$lock" "$here/sandbox-sources.sh" "$out/sandbox-packages.tsv" "$srcin/"
    tar --format=ustar --sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner -cf "$stage/srcin.tar" \
        -C "$srcin" debian.lock sandbox-packages.tsv sandbox-sources.sh
    bundle="$out/sandbox-sources.tar"
    docker run --rm -i --pull=missing "$builder" \
        sh -c 'mkdir /in && tar -xf - -C /in && exec sh /in/sandbox-sources.sh /in' \
        <"$stage/srcin.tar" >"$bundle.part"
    mv "$bundle.part" "$bundle"
    tar -xOf "$bundle" hyperbutler-sandbox-sources/MANIFEST.tsv >"$out/sandbox-sources.tsv"
    [ -s "$out/sandbox-sources.tsv" ] || fail "the source bundle has no manifest"
    bytes=$(wc -c <"$bundle")
    sum=$(sha256sum "$bundle" | cut -d' ' -f1)
    # A GitHub release asset must be under 2 GiB. Numbered parts, joined with cat.
    if [ "$bytes" -ge 2147483648 ]; then
        split -d -a 3 -b 2000M "$bundle" "$bundle."
        rm -f "$bundle"
    fi
    srcpkgs=$(cut -f1,2 "$out/sandbox-sources.tsv" | LC_ALL=C sort -u | wc -l)
    url=${HYPERBUTLER_RELEASE_URL:+${HYPERBUTLER_RELEASE_URL%/}/}
    {
        cat <<EOF
Corresponding source of the hyperbutler agent sandbox image

The sandbox image's system disk is built from Debian packages, many under the GPL and
LGPL. Their complete corresponding source, the .dsc, orig and debian tarballs of all
$((srcpkgs + 0)) source packages at the exact versions in the image, is published next to this
file, in the same GitHub release, as:

EOF
        if [ -f "$bundle" ]; then
            echo "  ${url}sandbox-sources.tar"
        else
            echo "  sandbox-sources.tar, in numbered parts, each under GitHub's 2 GiB limit on an asset:"
            (cd "$out" && for p in sandbox-sources.tar.*; do
                echo "    $url$p  sha256 $(sha256sum "$p" | cut -d' ' -f1)"
            done)
            echo
            echo "  Join them, in order, with: cat sandbox-sources.tar.* >sandbox-sources.tar"
        fi
        echo "  $((bytes + 0)) bytes, sha256 $sum"
        cat <<EOF

It stays available for as long as this release is offered.

sandbox-sources.tsv lists every file in it (source package, version, file, bytes, SHA-256),
and README.txt inside it says how to unpack and rebuild a package. Every file was checked
against the signed indexes of the snapshot.debian.org timestamp in debian.lock.

The image's kernel, its modules and its stage-1 initramfs are the guest image's: their
source is the guest source bundle of the same release.

The hyperbutler code in the image is Apache-2.0; its source is
https://github.com/Higangssh/hyperbutler at commit $commit.
EOF
    } >"$out/SANDBOX-SOURCES.txt"
fi

# --- 6. what built it, and the checksums, last ------------------------------------------
{
    echo "image: $builder"
    echo "snapshot: $(awk '$1 == "snapshot" { print $2 }' "$lock")"
    echo "suite: $(awk '$1 == "suite" { print $2 }' "$lock")"
    echo "kernel: $kver"
    echo "guest-image SHA256SUMS: $(sha256sum "$image/SHA256SUMS" 2>/dev/null | cut -d' ' -f1)"
    echo "go: $(go version)"
    echo "docker: $(docker version --format '{{.Server.Version}}')"
    echo "commit: $commit"
    echo "lock: guest/sandbox/debian.lock"
    echo "build-script: scripts/guest-image/build-sandbox.sh"
    echo "with-sources: ${HYPERBUTLER_WITH_SOURCES:-0}"
} >"$out/BUILDINFO.txt"

# SHA256SUMS over every regular file except itself, sorted, paths relative to OUT_DIR.
(cd "$out" && find . -type f | sed 's#^\./##' | LC_ALL=C sort |
    while read -r f; do sha256sum "$f"; done >"$stage/SHA256SUMS")
mv "$stage/SHA256SUMS" "$out/SHA256SUMS"
cat "$out/SHA256SUMS"
