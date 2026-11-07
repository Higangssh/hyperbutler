#!/bin/sh
# Downloads the corresponding source of the sandbox image's Debian packages (ADR 0009).
# Runs as root inside the digest-pinned builder container that debian.lock names;
# build-sandbox.sh sends it, with its inputs, as one tar on stdin.
#
#   sandbox-sources.sh DIR
#
# DIR holds debian.lock and sandbox-packages.tsv (build-rootfs.sh's list; its fourth and
# fifth columns name the source package and version of each binary package). stdout: one
# tar (sorted, mtime 0, numeric owner 0):
#   hyperbutler-sandbox-sources/MANIFEST.tsv  source package, version, file, bytes, SHA-256
#   hyperbutler-sandbox-sources/README.txt
#   hyperbutler-sandbox-sources/debian.lock
#   hyperbutler-sandbox-sources/sandbox-packages.tsv
#   hyperbutler-sandbox-sources/sources/<every .dsc, orig and debian tarball>
#
# Every source package at the exact version in the list, or a failure: a version the
# snapshot does not have stops the build, it is never skipped. Needs no capabilities.
set -eu

fail() { echo "sandbox-sources: $*" >&2; exit 1; }

[ "$#" -eq 1 ] || { echo "usage: sandbox-sources.sh DIR" >&2; exit 2; }
in=$1
tsv="$in/sandbox-packages.tsv"
field() { awk -v k="$1" '$1 == k { print $2 }' "$in/debian.lock"; }
ts=$(field snapshot)
suite=$(field suite)
case "$ts" in [0-9]*T[0-9]*Z) ;; *) fail "debian.lock: bad snapshot $ts" ;; esac

# The unique (source package, version) pairs. Several binary packages share a source; a
# binary-only rebuild (1.2-3+b1) names the version its source really has (1.2-3).
tab=$(printf '\t')
want="$in/want.tsv"
awk -F '\t' 'NF != 5 || $4 == "" || $5 == "" {
        print "sandbox-packages.tsv line " NR ": want package, version, arch, source, source version" > "/dev/stderr"
        exit 1
    }
    { print $4 "\t" $5 }' "$tsv" >"$want.all"
LC_ALL=C sort -u "$want.all" >"$want"
[ -s "$want" ] || fail "no packages in $tsv"
# Both go on apt's command line and into file names.
bad=$(LC_ALL=C grep -Ev "^[a-z0-9][a-z0-9+.-]*${tab}[0-9A-Za-z.+~:-]+\$" "$want" | head -n 1)
[ -z "$bad" ] || fail "not a Debian source package and version: $bad"

# The same scheme as build-rootfs.sh, with deb-src lines: the indexes come from the
# snapshot alone, so every file is checked against the snapshot's signed Sources indexes;
# the files come from Debian's own mirrors first and from the snapshot for what they no
# longer carry. A snapshot's Release files are past their Valid-Until by design.
key=/usr/share/keyrings/debian-archive-keyring.gpg
snap=http://snapshot.debian.org/archive
mirrors=/etc/apt/mirrors
mkdir -p "$mirrors"
echo "$snap/debian/$ts" >"$mirrors/debian.list"
echo "$snap/debian-security/$ts" >"$mirrors/security.list"
aptopt="-o Acquire::Check-Valid-Until=false -o Acquire::Retries=5"
rm -f /etc/apt/sources.list.d/*
{
    echo "deb-src [signed-by=$key] mirror+file:$mirrors/debian.list $suite main"
    echo "deb-src [signed-by=$key] mirror+file:$mirrors/debian.list $suite-updates main"
    echo "deb-src [signed-by=$key] mirror+file:$mirrors/security.list $suite-security main"
} >/etc/apt/sources.list
# shellcheck disable=SC2086
apt-get $aptopt -qq update >&2
printf 'http://deb.debian.org/debian\tpriority:1\n%s/debian/%s\tpriority:9\n' "$snap" "$ts" >"$mirrors/debian.list"
printf 'http://security.debian.org/debian-security\tpriority:1\n%s/debian-security/%s\tpriority:9\n' "$snap" "$ts" >"$mirrors/security.list"

root="$in/out/hyperbutler-sandbox-sources"
files="$root/sources"
mkdir -p "$files"
# apt's download process runs as _apt and must be able to write here.
chown _apt "$files"
cp "$in/debian.lock" "$tsv" "$root/"

# src: makes apt look the name up among source packages only: a binary package of the same
# name can belong to another source. One call, so a file two versions share is fetched once
# and any missing version fails the lot.
# shellcheck disable=SC2046,SC2086 # one word per package; the characters are checked above
(cd "$files" && apt-get $aptopt -q source --download-only $(awk -F '\t' '{ print "src:" $1 "=" $2 }' "$want")) >&2

# apt compares versions, and "1.0" equals "1.0-0" there. Hold what came down to the list
# exactly, and to what each .dsc lists, and allow nothing else.
man="$root/MANIFEST.tsv"
claimed="$in/claimed"
: >"$man"
: >"$claimed"
row() { # source version file
    bytes=$(wc -c <"$files/$3")
    printf '%s\t%s\t%s\t%s\t%s\n' "$1" "$2" "$3" "$((bytes + 0))" "$(sha256sum "$files/$3" | cut -d' ' -f1)" >>"$man"
    echo "$3" >>"$claimed"
}
count=0
while IFS="$tab" read -r src ver; do
    dsc="${src}_${ver#*:}.dsc"
    [ -f "$files/$dsc" ] || fail "$src $ver: apt fetched no $dsc"
    got=$(awk '/^Source: / { print $2; exit }' "$files/$dsc")
    [ "$got" = "$src" ] || fail "$dsc: Source is $got, want $src"
    got=$(awk '/^Version: / { print $2; exit }' "$files/$dsc")
    [ "$got" = "$ver" ] || fail "$dsc: Version is $got, want $ver"
    row "$src" "$ver" "$dsc"
    # Checksums-Sha256 lines are " <hash> <size> <file>". apt has already checked them
    # against the index; this catches a .dsc whose files apt did not bring.
    awk '/^Checksums-Sha256:/ { on = 1; next } on && /^ / { print $1, $3; next } { on = 0 }' "$files/$dsc" >"$in/dsc.files"
    [ -s "$in/dsc.files" ] || fail "$dsc lists no files"
    while read -r hash name; do
        case "$name" in '' | .* | */*) fail "$dsc: bad file name '$name'" ;; esac
        [ -f "$files/$name" ] || fail "$dsc: $name was not downloaded"
        row "$src" "$ver" "$name"
        [ "$(tail -n 1 "$man" | cut -f5)" = "$hash" ] || fail "$dsc: $name does not match its SHA-256"
    done <"$in/dsc.files"
    count=$((count + 1))
done <"$want"
LC_ALL=C sort -o "$man" "$man"
(cd "$files" && ls -A) | LC_ALL=C sort -u >"$in/present"
LC_ALL=C sort -u -o "$claimed" "$claimed"
extra=$(LC_ALL=C comm -23 "$in/present" "$claimed" | head -n 5)
[ -z "$extra" ] || fail "files no listed source package names: $extra"

cat >"$root/README.txt" <<EOF
Corresponding source for the hyperbutler agent sandbox image's Debian packages.

The sandbox image's system disk is Debian $suite at snapshot.debian.org timestamp $ts
(debian.lock). sandbox-packages.tsv lists every binary package in it, with the source
package and version it was built from. For each of those this bundle has, in sources/:

  <package>_<version>.dsc                     the signed source control file
  <package>_<upstream>.orig*.tar.*            the upstream source (and its signature, if any)
  <package>_<version>.debian.tar.*            Debian's patches and packaging

(a native package has one .tar.* instead). MANIFEST.tsv lists every file with its size and
SHA-256: source package, version, file, bytes, SHA-256. Each file was checked against the
snapshot's signed Sources indexes when it was downloaded.

To unpack a package, on Debian: dpkg-source -x sources/<package>_<version>.dsc
To rebuild it, in the unpacked directory: dpkg-buildpackage -us -uc -b

The kernel, its modules and the stage-1 initramfs are the guest image's; their source is
in the guest source bundle of the same release. The hyperbutler code in the image
(guest/cmd/*) is Apache-2.0; its source is the hyperbutler repository at the tag of this
release.
EOF

total=$(awk -F '\t' '!seen[$3]++ { n += $4 } END { printf "%.0f", n }' "$man")
echo "sandbox-sources: $count source packages, $(wc -l <"$in/present") files, $total bytes" >&2
tar --sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner --mode=u+rwX,go+rX,go-w --format=gnu \
    -C "$in/out" -cf - hyperbutler-sandbox-sources
