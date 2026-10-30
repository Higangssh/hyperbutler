#!/bin/sh
# Entry point of the canonical guest image build (ADR 0005, Decision 4). The guest-image
# workflow runs it as root in the digest-pinned Alpine container:
#
#   /src  the repository, read-only
#   /out  the output directory (bind mount from the runner)
#   /usr/local/go  the Go toolchain from actions/setup-go, read-only
#
# Parameters come from the environment, never from the command line, so workflow inputs
# are not interpolated into shell code:
#   HYPERBUTLER_LOCK          sources.lock path, relative to the repository root
#   HYPERBUTLER_BUILD_SCRIPT  build script path, relative to the repository root (contract in
#                        build-prototype.sh)
#   HYPERBUTLER_OUTPUTS       space-separated file names the build script must produce
#   HYPERBUTLER_STRICT        1: only signed Alpine packages are accepted (release)
#   HYPERBUTLER_WITH_SOURCES  1: also produce the GPL source bundle and license texts
#   HYPERBUTLER_HOST_UID      owner to give /out at the end
#
# /out receives the outputs, guest-inputs.tsv (what went in), BUILDINFO.txt (the tools
# that built it), optionally guest-sources.tar and licenses/, and SHA256SUMS over all of
# them.
set -eu

fail() { echo "ci-entry: $*" >&2; exit 1; }

relpath() { # value name: a path inside /src, no "..", no leading "/"
    case "$1" in
        '' | /* | *..* | *[!A-Za-z0-9._/-]*) fail "$2 must be a plain relative path inside the repository: '$1'" ;;
    esac
    [ -f "/src/$1" ] || fail "$2 not found: $1"
}

relpath "${HYPERBUTLER_LOCK:-}" HYPERBUTLER_LOCK
relpath "${HYPERBUTLER_BUILD_SCRIPT:-}" HYPERBUTLER_BUILD_SCRIPT
for f in ${HYPERBUTLER_OUTPUTS:?}; do
    case "$f" in
        *[!A-Za-z0-9._-]* | .*) fail "output names are plain file names: '$f'" ;;
    esac
done

# Tools from the same pinned Alpine release. apk verifies every package it installs
# against the image's keys. Versions are recorded in BUILDINFO.txt.
tools="curl tar gzip xz"
[ "${HYPERBUTLER_WITH_SOURCES:-0}" = 1 ] && tools="$tools git abuild"
# shellcheck disable=SC2086
apk add --no-cache --quiet $tools

export PATH="/usr/local/go/bin:$PATH"
export GOTOOLCHAIN=local GOFLAGS=-mod=readonly GOCACHE=/tmp/go-build GOMODCACHE=/tmp/go-mod GOPATH=/tmp/go
go version >/dev/null || fail "Go is not mounted at /usr/local/go"

work=/tmp/hyperbutler-guest
cache="$work/cache"
mkdir -p "$cache" "$work/build" /out
rm -rf "${work:?}/build/"* /out/*

echo "::group::fetch and verify inputs"
sh /src/scripts/guest-image/fetch-verify.sh "/src/$HYPERBUTLER_LOCK" "$cache" /out/guest-inputs.tsv
echo "::endgroup::"

echo "::group::build ($HYPERBUTLER_BUILD_SCRIPT)"
HYPERBUTLER_SRC=/src HYPERBUTLER_CACHE="$cache" HYPERBUTLER_WORK="$work/build" HYPERBUTLER_OUT=/out \
    sh "/src/$HYPERBUTLER_BUILD_SCRIPT"
echo "::endgroup::"

for f in $HYPERBUTLER_OUTPUTS; do
    [ -s "/out/$f" ] || fail "build script did not produce $f"
done

if [ "${HYPERBUTLER_WITH_SOURCES:-0}" = 1 ]; then
    echo "::group::corresponding source"
    mkdir -p "$work/src"
    HYPERBUTLER_SRC=/src sh /src/scripts/guest-image/fetch-sources.sh \
        /out/guest-inputs.tsv "$work/src" /out/guest-sources.tar /out/licenses
    # Alpine keeps only the newest build of a package in its index, so a pinned .apk can
    # disappear. The release publishes the verified inputs as its own mirror (ADR 0005).
    tar --sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner --format=gnu \
        -C "$cache" -cf /out/guest-inputs.tar dl
    echo "::endgroup::"
fi

{
    echo "image: ${HYPERBUTLER_IMAGE:-unknown}"
    echo "alpine: $(cat /etc/alpine-release)"
    echo "go: $(go version)"
    echo "lock: $HYPERBUTLER_LOCK"
    echo "build-script: $HYPERBUTLER_BUILD_SCRIPT"
    echo "outputs: $HYPERBUTLER_OUTPUTS"
    echo "strict: ${HYPERBUTLER_STRICT:-0}"
    echo "tools:"
    # shellcheck disable=SC2086
    apk info -v $tools apk-tools busybox 2>/dev/null | sort | sed 's/^/  /'
} >/out/BUILDINFO.txt

# SHA256SUMS over every regular file except itself, sorted, paths relative to /out.
(cd /out && find . -type f | sed 's#^\./##' | LC_ALL=C sort |
    while read -r f; do sha256sum "$f"; done >/tmp/SHA256SUMS)
mv /tmp/SHA256SUMS /out/SHA256SUMS
cat /out/SHA256SUMS | grep -v ' licenses/' || true

[ -n "${HYPERBUTLER_HOST_UID:-}" ] && chown -R "$HYPERBUTLER_HOST_UID" /out
exit 0
