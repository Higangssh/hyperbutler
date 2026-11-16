#!/bin/sh
# Negative tests of fetch-verify.sh's engine-nonpie entry (ADR 0005, "The engine without
# PIE"). Runs as root in the digest-pinned Alpine container after build-nonpie.sh:
#
#   test-engine-nonpie.sh LOCK TAR
#
# TAR is a good engine-nonpie tar for the lock's docker-engine and containerd. Every case
# changes one thing and must make fetch-verify.sh fail; the good tar must pass.
set -eu

lock=$1
good=$2
src=${HYPERBUTLER_SRC:-/src}
work=/tmp/hyperbutler-nonpie-test
mirror=https://github.com/Higangssh/hyperbutler/releases/download/guest-inputs

fail() { echo "test-engine-nonpie: $*" >&2; exit 1; }

apk add --no-cache --quiet curl tar
rm -rf "$work"
mkdir -p "$work/cache"
grep -E '^(docker-engine|containerd)[[:space:]]' "$lock" >"$work/packages.lock"
[ "$(wc -l <"$work/packages.lock")" -eq 2 ] || fail "the lock lacks docker-engine or containerd"
# No fallback: every case below places its tar in the cache, so nothing is downloaded but
# the two packages.
export HYPERBUTLER_INPUTS_MIRROR=

# case NAME WANT DIR [LOCK-ORDER]: DIR holds the tree to tar (BUILDINFO.txt, usr/bin/...).
# WANT is pass or fail. LOCK-ORDER "first" lists engine-nonpie before the packages.
run_case() {
    name=$1 want=$2 tree=$3 order=${4:-last} url=${5:-}
    t="$work/$name.tar"
    # shellcheck disable=SC2046 # the top-level names, without "./"
    tar --sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner --format=gnu -C "$tree" -cf "$t" $(cd "$tree" && ls)
    sum=$(sha256sum "$t" | cut -d' ' -f1)
    file=${6:-$(printf '%s' "${good##*/}" | sed -E 's/-[0-9a-f]{12}\.tar$//')-$(printf '%s' "$sum" | cut -c1-12).tar}
    rm -rf "$work/cache/dl/engine-nonpie"
    mkdir -p "$work/cache/dl/engine-nonpie"
    cp "$t" "$work/cache/dl/engine-nonpie/$file"
    entry="engine-nonpie $sum ${url:-$mirror}/$file"
    if [ "$order" = first ]; then
        { echo "$entry"; cat "$work/packages.lock"; } >"$work/case.lock"
    else
        { cat "$work/packages.lock"; echo "$entry"; } >"$work/case.lock"
    fi
    if HYPERBUTLER_STRICT=1 sh "$src/scripts/guest-image/fetch-verify.sh" "$work/case.lock" "$work/cache" "$work/case.tsv" >"$work/case.log" 2>&1; then
        got=pass
    else
        got=fail
    fi
    if [ "$got" != "$want" ]; then
        cat "$work/case.log" >&2
        fail "case '$name': fetch-verify.sh did $got, want $want"
    fi
    echo "ok     case '$name': $got ($(tail -n 1 "$work/case.log"))"
}

base="$work/base"
mkdir -p "$base"
tar -xf "$good" -C "$base"
fresh() { # a copy of the good tree
    rm -rf "$work/tree"
    cp -a "$base" "$work/tree"
    printf '%s' "$work/tree"
}

run_case good pass "$(fresh)"
# A rebuild must name the pinned packages' commits; this one names another.
tree=$(fresh)
sed -i -E 's/^(docker-engine: aports )[0-9a-f]{40}/\10000000000000000000000000000000000000000/' "$tree/BUILDINFO.txt"
run_case "stale docker-engine commit" fail "$tree"
tree=$(fresh)
sed -i '/^containerd: aports /d' "$tree/BUILDINFO.txt"
run_case "no containerd line" fail "$tree"
tree=$(fresh)
rm "$tree/usr/bin/containerd-shim-runc-v2"
run_case "a binary missing" fail "$tree"
tree=$(fresh)
rm "$tree/BUILDINFO.txt"
run_case "no BUILDINFO.txt" fail "$tree"
run_case "listed before the packages" fail "$(fresh)" first
run_case "another host" fail "$(fresh)" last https://example.com/guest-inputs
run_case "another release" fail "$(fresh)" last https://github.com/Higangssh/hyperbutler/releases/download/v0.2.0
run_case "other versions in the name" fail "$(fresh)" last "" engine-nonpie-docker-0.0.1-r0-containerd-0.0.1-r0-000000000000.tar
run_case "a hash suffix that is not the pin's" fail "$(fresh)" last "" "$(printf '%s' "${good##*/}" | sed -E 's/-[0-9a-f]{12}\.tar$//')-000000000000.tar"
echo "all engine-nonpie cases passed"
