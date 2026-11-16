#!/bin/sh
# Builds dockerd, docker-proxy, containerd and containerd-shim-runc-v2 without PIE, from
# the same aports commit and upstream sources as the signed Alpine packages pinned in the
# lock (ADR 0005, Follow-up 10). Runs as root in the digest-pinned Alpine container:
#
#   build-nonpie.sh LOCK OUT
#
# /src is the repository (read-only). OUT receives the four binaries, BUILDINFO.txt (the
# commits, toolchain and patches) and SHA256SUMS.
#
# Alpine builds Go programs as PIE: moby's hack/make/.binary adds -buildmode=pie to every
# dynamic build, and the containerd APKBUILD passes BUILDMODE=pie. The loader relocates
# a PIE's .data.rel.ro at start, which costs about 11 MiB of private memory in dockerd
# and 4.7 MiB in containerd at idle. The only change here is the build mode.
set -eu

lock=$1
out=$2
src=${HYPERBUTLER_SRC:-/src}
work=/tmp/hyperbutler-nonpie

fail() { echo "build-nonpie: $*" >&2; exit 1; }

rm -rf "$work"
mkdir -p "$work/cache" "$work/distfiles" "$work/aports" "$out"

apk add --no-cache --quiet git abuild binutils curl tar gzip

echo "::group::fetch and verify the pinned packages"
sh "$src/scripts/guest-image/fetch-verify.sh" "$lock" "$work/cache" "$work/inputs.tsv"
echo "::endgroup::"

branch="v$(cut -d. -f1,2 /etc/alpine-release)"
DISTFILES_MIRROR="${HYPERBUTLER_DISTFILES_MIRROR:-https://distfiles.alpinelinux.org/distfiles/$branch}"
export DISTFILES_MIRROR SRCDEST="$work/distfiles"
export GOTOOLCHAIN=local

git_dir="$work/aports.git"
git init -q --bare "$git_dir"
git --git-dir="$git_dir" remote add github https://github.com/alpinelinux/aports.git
git --git-dir="$git_dir" remote add cgit https://git.alpinelinux.org/aports
git --git-dir="$git_dir" remote add alpine https://gitlab.alpinelinux.org/alpine/aports.git
fetch_commit() { # commit
    for round in 1 2 3; do
        for remote in github cgit alpine; do
            if git --git-dir="$git_dir" fetch -q --depth 1 "$remote" "$1" 2>"$work/fetch.err"; then
                [ "$(git --git-dir="$git_dir" rev-parse FETCH_HEAD)" = "$1" ] || fail "fetched the wrong commit for $1"
                return 0
            fi
            echo "build-nonpie: $remote did not serve $1 (round $round): $(tail -n 1 "$work/fetch.err")" >&2
        done
        [ "$round" = 3 ] || sleep $((round * 30))
    done
    fail "cannot fetch aports commit $1"
}

# origin_of NAME: "commit<TAB>repo<TAB>origin" of the pinned package NAME.
origin_of() {
    awk -F '\t' -v n="$1" '$4 == "apk" && $1 == n { print $8 "\t" $9 "\t" $7; exit }' "$work/inputs.tsv"
}

info="$work/BUILDINFO.txt"
{
    echo "alpine: $(cat /etc/alpine-release)"
    echo "image: ${HYPERBUTLER_IMAGE:-unknown}"
} >"$info"

build_origin() { # package-name
    line=$(origin_of "$1")
    [ -n "$line" ] || fail "$1 is not an Alpine package in $lock"
    commit=$(printf '%s' "$line" | cut -f1)
    repo=$(printf '%s' "$line" | cut -f2)
    origin=$(printf '%s' "$line" | cut -f3)
    printf '%s' "$origin" | grep -q -E '^[A-Za-z0-9][A-Za-z0-9._+-]*$' || fail "bad origin: $origin"
    printf '%s' "$repo" | grep -q -E '^[a-z]+$' || fail "bad repo: $repo"
    git --git-dir="$git_dir" cat-file -e "$commit^{commit}" 2>/dev/null || fetch_commit "$commit"
    git --git-dir="$git_dir" archive --format=tar "$commit" "$repo/$origin" | tar -x -C "$work/aports"
    dir="$work/aports/$repo/$origin"
    echo "$1: aports $commit $repo/$origin" >>"$info"
    # A fixed build time (moby stamps it into dockerd): the aports commit's, so a rebuild
    # from the same pins gives the same bytes.
    SOURCE_DATE_EPOCH=$(git --git-dir="$git_dir" show -s --format=%ct "$commit")
    export SOURCE_DATE_EPOCH

    echo "::group::$origin: dependencies and sources"
    (cd "$dir" && abuild -F deps && abuild -F fetch verify unpack prepare) || fail "abuild prepare failed for $origin"
    echo "::endgroup::"

    case "$origin" in
        docker)
            # moby's .binary adds -buildmode=pie to every dynamic build.
            set -- "$dir"/src/moby-*/hack/make/.binary
            [ "$#" -eq 1 ] && [ -f "$1" ] || fail "moby hack/make/.binary not found"
            grep -q 'BUILDFLAGS+=("-buildmode=pie")' "$1" || fail "moby .binary no longer adds -buildmode=pie where expected"
            sed -i 's/BUILDFLAGS+=("-buildmode=pie")/:/' "$1"
            echo "  patch: moby hack/make/.binary: no -buildmode=pie" >>"$info"
            ;;
        containerd)
            # Makefile.linux adds -buildmode=pie unless STATIC is set (the APKBUILD's
            # BUILDMODE=pie is not read by the Makefile).
            set -- "$dir"/src/containerd-*/Makefile.linux
            [ "$#" -eq 1 ] && [ -f "$1" ] || fail "containerd Makefile.linux not found"
            grep -q 'GO_GCFLAGS += -buildmode=pie' "$1" || fail "containerd Makefile.linux no longer adds -buildmode=pie where expected"
            sed -i 's/GO_GCFLAGS += -buildmode=pie//' "$1"
            echo "  patch: containerd Makefile.linux: no -buildmode=pie" >>"$info"
            ;;
    esac

    echo "::group::$origin: build"
    (cd "$dir" && abuild -F build) || fail "abuild build failed for $origin"
    echo "::endgroup::"
}

# abuild reads /usr/share/abuild/default.conf and /etc/abuild.conf, which put
# -buildmode=pie into GOFLAGS, every time it runs; the user configuration is read last.
cat /usr/share/abuild/default.conf /etc/abuild.conf 2>/dev/null | grep -n 'GOFLAGS' >"$work/abuild-conf.txt" || true
mkdir -p "$HOME/.abuild"
cat >"$HOME/.abuild/abuild.conf" <<'EOF'
GOFLAGS=$(printf '%s' "${GOFLAGS:-}" | sed 's/-buildmode=pie//g')
export GOFLAGS
EOF
build_origin docker-engine
build_origin containerd
echo "go: $(go version)" >>"$info"
echo "abuild GOFLAGS (before the user configuration): $(tr '\n' ' ' <"$work/abuild-conf.txt")" >>"$info"
echo "  patch: ~/.abuild/abuild.conf: -buildmode=pie removed from GOFLAGS" >>"$info"

# The four binaries the guest image ships (guest/prototype/system.spec).
pick() { # name
    # shellcheck disable=SC2046 # word splitting wanted: the paths have no spaces
    set -- $(find "$work/aports" -type f -name "$1" -perm -u+x -path '*/src/*' | grep -v -e '/vendor/' -e '/hack/' | sort)
    [ "$#" -ge 1 ] || fail "no $1 built"
    cp "$1" "$out/$(basename "$1")"
}
for b in dockerd docker-proxy containerd containerd-shim-runc-v2; do
    pick "$b"
done

echo "binaries:" >>"$info"
for b in dockerd docker-proxy containerd containerd-shim-runc-v2; do
    type=$(readelf -h "$out/$b" | awk '/Type:/ { print $2 }')
    relro=$(readelf -S -W "$out/$b" | awk '{ for (i = 1; i <= NF; i++) if ($i == ".data.rel.ro") print "0x" $(i + 4) }')
    echo "  $b: type $type, .data.rel.ro ${relro:-none}" >>"$info"
    [ "$type" = EXEC ] || fail "$b is $type, not a fixed-address executable"
done
cp "$info" "$out/BUILDINFO.txt"
cat "$out/BUILDINFO.txt"
(cd "$out" && sha256sum dockerd docker-proxy containerd containerd-shim-runc-v2 BUILDINFO.txt >SHA256SUMS)
cat "$out/SHA256SUMS"
[ -n "${HYPERBUTLER_HOST_UID:-}" ] && chown -R "$HYPERBUTLER_HOST_UID" "$out"
exit 0
