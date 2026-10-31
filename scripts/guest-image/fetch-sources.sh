#!/bin/sh
# Builds the guest image's corresponding-source bundle and license texts from the
# manifest fetch-verify.sh wrote. Runs inside the pinned Alpine container.
#
#   fetch-sources.sh MANIFEST WORK BUNDLE LICENSES
#
# For every Alpine package in MANIFEST it takes the aports commit and origin from the
# package's signed .PKGINFO, fetches that exact aports directory (the APKBUILD, Alpine's
# patches and the kernel config), and runs `abuild fetch verify` there. abuild downloads
# the upstream source tarballs and checks them against the sha512sums in the APKBUILD,
# so every byte in the bundle is pinned by the signed package's commit.
#
# BUNDLE becomes one tar (sorted, mtime 0, numeric owner 0):
#   hyperbutler-guest-sources/MANIFEST.tsv
#   hyperbutler-guest-sources/aports/<commit>/<repo>/<origin>/...
#   hyperbutler-guest-sources/distfiles/<upstream tarballs and patches>
#   hyperbutler-guest-sources/README.txt
# LICENSES gets the license files found at the top of each upstream tarball (and the
# kernel's LICENSES/ directory), under LICENSES/<origin>/. An origin with no license file
# is an error unless dist/licenses/<origin>/ in the repository supplies one.
#
# Inputs that are not Alpine packages have no source here. fetch-verify.sh refuses them
# when HYPERBUTLER_STRICT=1, which the release workflow sets.
set -eu

manifest=$1
work=$2
bundle=$3
licenses=$4
src=${HYPERBUTLER_SRC:-/src}

fail() { echo "fetch-sources: $*" >&2; exit 1; }

root="$work/hyperbutler-guest-sources"
dist="$root/distfiles"
git_dir="$work/aports.git"
rm -rf "$root" "$git_dir"
mkdir -p "$dist" "$licenses"
cp "$manifest" "$root/MANIFEST.tsv"

# abuild tries $DISTFILES_MIRROR/<file> before a source's upstream URL. Alpine's builders
# upload every distfile they used there, so the bundle stays buildable when an upstream
# host is down or turns CI clients away (gitlab.alpinelinux.org has answered archive
# downloads with HTTP 418). abuild still checks the bytes against the APKBUILD's
# sha512sums, so the mirror cannot substitute content. The branch is the container's
# Alpine release, which is bumped together with sources.lock.
branch="v$(cut -d. -f1,2 /etc/alpine-release)"
DISTFILES_MIRROR="${HYPERBUTLER_DISTFILES_MIRROR:-https://distfiles.alpinelinux.org/distfiles/$branch}"
export DISTFILES_MIRROR
echo "fetch-sources: distfiles mirror $DISTFILES_MIRROR"

git init -q --bare "$git_dir"
git --git-dir="$git_dir" remote add alpine https://gitlab.alpinelinux.org/alpine/aports.git
git --git-dir="$git_dir" remote add github https://github.com/alpinelinux/aports.git
fetch_commit() { # commit
    # A full (not partial) shallow fetch of one commit: about 40 MB, and git archive then
    # needs no lazy blob fetches. Both servers allow fetching a reachable commit by hash.
    for remote in alpine github; do
        if git --git-dir="$git_dir" fetch -q --depth 1 "$remote" "$1" 2>/dev/null; then
            # The object name is the commit hash, so the mirror cannot substitute content.
            [ "$(git --git-dir="$git_dir" rev-parse FETCH_HEAD)" = "$1" ] || fail "fetched the wrong commit for $1"
            return 0
        fi
        echo "fetch-sources: $remote did not serve $1, trying the next remote" >&2
    done
    fail "cannot fetch aports commit $1"
}

# Unique (commit, repo, origin) triples of the Alpine packages.
awk -F '\t' '$4 == "apk" { print $8 "\t" $9 "\t" $7 }' "$manifest" | sort -u >"$work/origins.tsv"
[ -s "$work/origins.tsv" ] || fail "no Alpine packages in $manifest"

while IFS="$(printf '\t')" read -r commit repo origin; do
    printf '%s' "$origin" | grep -q -E '^[A-Za-z0-9][A-Za-z0-9._+-]*$' || fail "bad origin: $origin"
    printf '%s' "$repo" | grep -q -E '^[a-z]+$' || fail "bad repo: $repo"
    echo "source $repo/$origin @ $commit"
    git --git-dir="$git_dir" cat-file -e "$commit^{commit}" 2>/dev/null || fetch_commit "$commit"
    dest="$root/aports/$commit"
    mkdir -p "$dest"
    git --git-dir="$git_dir" archive --format=tar "$commit" "$repo/$origin" | tar -x -C "$dest"
    [ -f "$dest/$repo/$origin/APKBUILD" ] || fail "no APKBUILD for $repo/$origin at $commit"
    # abuild sources the APKBUILD (shell code from the pinned commit), downloads the
    # remote sources into SRCDEST and verifies their sha512sums. -F: we are root.
    (cd "$dest/$repo/$origin" && SRCDEST="$dist" abuild -F fetch verify) ||
        fail "abuild fetch/verify failed for $repo/$origin"

    # License texts: top-level LICENSE/COPYING/NOTICE/COPYRIGHT files of every upstream
    # tarball this origin uses, plus the kernel's LICENSES/{preferred,exceptions}.
    lic="$licenses/$origin"
    mkdir -p "$lic"
    # A "name::url" source is saved under its local name; a plain URL under its basename.
    # shellcheck disable=SC2016 # $source is expanded by the APKBUILD's own shell
    files=$(cd "$dest/$repo/$origin" && sh -c '. ./APKBUILD >/dev/null 2>&1
        for s in $source; do
            case "$s" in *::*) echo "${s%%::*}" ;; *) echo "${s##*/}" ;; esac
        done')
    for f in $files; do
        a="$dist/$f"
        [ -f "$a" ] || continue
        case "$f" in
            *.tar.gz | *.tgz | *.tar.xz | *.tar.bz2 | *.tar.zst) ;;
            *) continue ;;
        esac
        tar -tf "$a" 2>/dev/null |
            grep -E '^[^/]+/((COPYING|LICEN[CS]E|NOTICE|COPYRIGHT)[^/]*|LICENSES/(preferred|exceptions)/[^/]+)$' \
                >"$work/lic.list" || true
        if [ -s "$work/lic.list" ]; then
            mkdir -p "$lic/${f%%.tar*}"
            tar -xf "$a" -C "$lic/${f%%.tar*}" --strip-components=1 -T "$work/lic.list"
        fi
    done
    if [ -d "$src/dist/licenses/$origin" ]; then
        cp -R "$src/dist/licenses/$origin/." "$lic/"
    fi
    [ -n "$(find "$lic" -type f | head -n 1)" ] ||
        fail "no license text found for $origin; add it under dist/licenses/$origin/"
done <"$work/origins.tsv"

cat >"$root/README.txt" <<'EOF'
Corresponding source for the hyperbutler guest image.

The guest image (vmlinuz-virt and the initramfs or system disk) is built from the
Alpine Linux packages listed in MANIFEST.tsv. For each package this bundle has:

  aports/<commit>/<repo>/<origin>/   the APKBUILD, patches and configuration that Alpine
                                     built the package from, at the commit recorded in
                                     the package's signed .PKGINFO
  distfiles/                         the upstream source archives those APKBUILDs name,
                                     verified against their sha512sums

To rebuild a package, copy its aports directory into an aports checkout on Alpine Linux,
put the distfiles in $SRCDEST, and run `abuild -r`.

The hyperbutler code in the image (guest/cmd/*) is Apache-2.0; its source is the hyperbutler
repository at the tag of this release.
EOF

tar --sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner --format=gnu \
    -C "$work" -cf "$bundle" hyperbutler-guest-sources
echo "source bundle: $(du -h "$bundle" | cut -f1) $bundle"
