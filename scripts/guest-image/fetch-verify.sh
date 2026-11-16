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
#   4. engine-nonpie (the engine rebuilt without PIE, ADR 0005 Follow-up 10): its pin, a
#      guest-inputs URL, and a BUILDINFO.txt that names the aports commits of the pinned
#      docker-engine and containerd packages above it;
#   5. any other file is checked by its SHA-256 pin only (trust on first use). With
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
# Every package must come from the Alpine release and architecture of the container that
# verifies it (the digest-pinned image, whose keys are the trust root): the same keys
# sign every branch, so the signature alone would also accept an edge or old-branch
# package.
branch=v$(cut -d. -f1,2 /etc/alpine-release)
arch=$(apk --print-arch)

fail() { echo "fetch-verify: $*" >&2; exit 1; }

# Fallback for a pinned file the upstream URL no longer serves: <mirror>/<file name>.
# The default is a pre-release of this repository that holds a copy of every pinned
# package (docs/adr/0005-guest-image.md). Set HYPERBUTLER_INPUTS_MIRROR= to turn it off.
mirror=${HYPERBUTLER_INPUTS_MIRROR-https://github.com/Higangssh/hyperbutler/releases/download/guest-inputs}
case "$mirror" in
    "" | https://*) ;;
    *) fail "HYPERBUTLER_INPUTS_MIRROR must be an https URL: $mirror" ;;
esac
mirror=${mirror%/}

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
        if ! curl --proto '=https' --tlsv1.2 -fsSL --retry 3 -o "$path.part" "$url"; then
            # Alpine removes a package from its mirrors once a newer build replaces it.
            # The project keeps a copy of every pinned file; the checks below are the same
            # whichever host served the bytes, and the Alpine repo is still taken from $url.
            [ -n "$mirror" ] || fail "$name: download failed and no fallback mirror is set: $url"
            echo "::warning title=guest image::$name: $url is gone upstream; using the guest-inputs copy. The pin is likely superseded (ADR 0005)."
            echo "fetch  $mirror/$file (fallback)"
            curl --proto '=https' --tlsv1.2 -fsSL --retry 3 -o "$path.part" "$mirror/$file" ||
                fail "$name: download failed from $url and from $mirror/$file"
        fi
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
            # Alpine mirror layout: .../alpine/<branch>/<repo>/<arch>/<file>, and only this
            # container's branch and arch.
            repo=$(printf '%s' "$url" | sed -En "s#^https://[^/]+/alpine/$branch/(main|community)/$arch/[^/]+\$#\\1#p")
            [ -n "$repo" ] || fail "$name: not an Alpine $branch main/community $arch URL: $url"
            case "$(field arch)" in "$arch" | noarch) ;; *) fail "$name: .PKGINFO arch is '$(field arch)', not $arch or noarch" ;; esac
            echo "ok     $name $pkgname-$pkgver (signature, sha256; origin $origin@$commit)"
            ;;
        engine-nonpie-docker-*-containerd-*.tar)
            # dockerd, docker-proxy, containerd and the shim rebuilt without PIE by
            # scripts/guest-image/build-nonpie.sh (ADR 0005, Follow-up 10). The project
            # builds it and keeps the only copy, so its pin is the trust; its source is
            # the docker-engine and containerd packages' aports directories and distfiles,
            # which must be the pinned packages above it in the lock, at the same commits.
            [ "$name" = engine-nonpie ] || fail "$name: an $file must be the engine-nonpie entry"
            case "$url" in
                https://github.com/Higangssh/hyperbutler/releases/download/guest-inputs/*) ;;
                *) fail "$name: must come from the guest-inputs release: $url" ;;
            esac
            kind=rebuild
            binfo=$(tar -xOf "$path" BUILDINFO.txt 2>/dev/null) || fail "$name: no BUILDINFO.txt"
            want=engine-nonpie
            licenses=
            for pkg in docker-engine containerd; do
                # commit, version, repo/origin of the pinned package
                pin=$(awk -F '\t' -v p="$pkg" '$1 == p && $4 == "apk" { print $8 "\t" $6 "\t" $9 "/" $7; exit }' "$manifest")
                [ -n "$pin" ] || fail "$name: $pkg must be a pinned Alpine package listed before it"
                pcommit=$(printf '%s' "$pin" | cut -f1)
                pver=$(printf '%s' "$pin" | cut -f2)
                porigin=$(printf '%s' "$pin" | cut -f3)
                printf '%s\n' "$binfo" | grep -q -x -F "$pkg: aports $pcommit $porigin" ||
                    fail "$name: BUILDINFO.txt was not built from $pkg's aports commit $pcommit"
                want="$want-${pkg#docker-engine}-$pver"
                plic=$(awk -F '\t' -v p="$pkg" '$1 == p { print $10; exit }' "$manifest")
                case " $licenses " in *" $plic "*) ;; *) licenses="${licenses:+$licenses }$plic" ;; esac
            done
            # engine-nonpie-docker-<ver>-containerd-<ver>.tar
            want=$(printf '%s' "$want" | sed 's/^engine-nonpie--/engine-nonpie-docker-/').tar
            [ "$file" = "$want" ] || fail "$name: $file, but the pinned packages make $want"
            for b in dockerd docker-proxy containerd containerd-shim-runc-v2; do
                tar -tf "$path" "usr/bin/$b" >/dev/null 2>&1 || fail "$name: no usr/bin/$b"
            done
            pkgname=engine-nonpie
            license=$licenses
            echo "ok     $name (sha256; rebuilt from the docker-engine and containerd pins)"
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
