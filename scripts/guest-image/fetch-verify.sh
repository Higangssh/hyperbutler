#!/bin/sh
# Fetches every input in a guest sources.lock and verifies it. Runs inside the pinned
# Alpine container (see ci-entry.sh); POSIX sh, so BusyBox ash is enough.
#
#   fetch-verify.sh LOCK CACHE MANIFEST
#
# For each "<name> <sha256> <url>" line in LOCK:
#   1. downloads <url> (https only) to CACHE/dl/<name>/<file>;
#   2. checks the SHA-256 pin (sha256sum -c), and deletes the file on a mismatch;
#   3. for an Alpine .apk: runs `apk verify`, which checks the package's RSA signature
#      against the keys in /etc/apk/keys (the keys that came with the digest-pinned
#      alpine image) and the control segment's hash over the data segment. A package
#      that is not signed by one of those keys fails. Then it reads .PKGINFO for the
#      origin, aports commit and license, which the source bundle needs;
#   4. any other file is checked by its SHA-256 pin only (trust on first use). With
#      HYPERBUTLER_STRICT=1 such an entry is an error: a release must only contain inputs
#      whose signature and corresponding source can be verified.
#
# MANIFEST gets one tab-separated line per input:
#   name sha256 file kind pkgname pkgver origin commit repo license
set -eu

lock=$1
cache=$2
manifest=$3
strict=${HYPERBUTLER_STRICT:-0}

fail() { echo "fetch-verify: $*" >&2; exit 1; }

[ -f "$lock" ] || fail "lock file $lock not found"
mkdir -p "$cache/dl"
: >"$manifest"

n=0
unsigned=0
# Read with a here-document of the filtered file so the loop runs in this shell.
grep -v -E '^[[:space:]]*(#|$)' "$lock" >"$cache/lock.entries" || true
while read -r name sha url extra; do
    [ -z "${extra:-}" ] || fail "line for $name has more than three fields"
    printf '%s' "$name" | grep -q -E '^[A-Za-z0-9][A-Za-z0-9._+-]*$' || fail "bad name: $name"
    printf '%s' "$sha" | grep -q -E '^[0-9a-f]{64}$' || fail "bad sha256 for $name: $sha"
    case "$url" in
        https://*) ;;
        *) fail "$name: only https URLs are allowed: $url" ;;
    esac
    file=${url##*/}
    file=${file%%\?*}
    printf '%s' "$file" | grep -q -E '^[A-Za-z0-9][A-Za-z0-9._+-]*$' || fail "$name: bad file name in URL: $file"

    dir="$cache/dl/$name"
    mkdir -p "$dir"
    path="$dir/$file"
    if [ ! -f "$path" ]; then
        echo "fetch  $url"
        curl --proto '=https' --tlsv1.2 -fsSL --retry 3 -o "$path.part" "$url"
        mv "$path.part" "$path"
    fi
    if ! (cd "$dir" && printf '%s  %s\n' "$sha" "$file" | sha256sum -c -s -); then
        rm -f "$path"
        fail "sha256 mismatch for $name ($url)"
    fi

    kind=pinned
    pkgname=- pkgver=- origin=- commit=- repo=- license=-
    case "$file" in
        *.apk)
            # No --allow-untrusted: an unsigned or foreign-signed package fails here.
            apk verify "$path" >/dev/null || fail "$name: apk signature verification failed"
            kind=apk
            info=$(tar -xzOf "$path" .PKGINFO 2>/dev/null) || fail "$name: no .PKGINFO"
            field() { printf '%s\n' "$info" | sed -n "s/^$1 = //p" | head -n 1; }
            pkgname=$(field pkgname)
            pkgver=$(field pkgver)
            origin=$(field origin)
            commit=$(field commit)
            license=$(field license | tr '\t' ' ')
            [ -n "$pkgname" ] && [ -n "$pkgver" ] || fail "$name: .PKGINFO lacks pkgname/pkgver"
            # The signed metadata must name the file we pinned, so a pin cannot point at
            # a different (older, still signed) package than its file name says.
            [ "$file" = "$pkgname-$pkgver.apk" ] || fail "$name: file $file but .PKGINFO says $pkgname-$pkgver"
            printf '%s' "$commit" | grep -q -E '^[0-9a-f]{40}$' || fail "$name: no aports commit in .PKGINFO"
            # Alpine mirror layout: .../alpine/<branch>/<repo>/<arch>/<file>
            repo=$(printf '%s' "$url" | sed -n 's#^https://[^/]*/alpine/[^/]*/\([a-z]*\)/[^/]*/[^/]*$#\1#p')
            [ -n "$repo" ] || fail "$name: cannot tell the aports repository from $url"
            echo "ok     $name $pkgname-$pkgver (signature, sha256; origin $origin@$commit)"
            ;;
        *)
            unsigned=$((unsigned + 1))
            if [ "$strict" = 1 ]; then
                fail "$name: $file is not a signed Alpine package; a release build accepts only verifiable inputs (docs/release.md)"
            fi
            echo "::warning title=guest image::$name is checked by its sha256 pin only (no signature, no source)"
            echo "ok     $name (sha256 pin only)"
            ;;
    esac
    printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
        "$name" "$sha" "$file" "$kind" "$pkgname" "$pkgver" "$origin" "$commit" "$repo" "$license" >>"$manifest"
    n=$((n + 1))
done <"$cache/lock.entries"

[ "$n" -gt 0 ] || fail "no entries in $lock"
echo "verified $n input(s); $unsigned without a signature"
