# Builds the ADR 0005 (A2b) guest image on Windows, with no WSL: a stage-1 initramfs, a
# read-only erofs system disk, the kernel, and the manifest that ties them together.
#
#   powershell -ExecutionPolicy Bypass -File guest\prototype\build.ps1 [-Cache DIR] [-Out DIR]
#       [-Docker docker.exe] [-DockerHost npipe:////./pipe/docker_engine_hyperbutler] [-Strict]
#
# 1. Downloads every input in sources.lock into its own directory under -Cache and
#    checks its SHA-256 pin. A mismatch is fatal.
#    Then it checks every Alpine package's signature: the pinned files, the lock and
#    scripts/guest-image/fetch-verify.sh (the CI check) go into the digest-pinned Alpine
#    container on stdin, with no network, and `apk verify` checks each .apk against that
#    image's /etc/apk/keys. A bad signature is fatal. An input that is not an .apk is
#    accepted with a warning, or refused with -Strict (what a release requires). The
#    result is out\guest-inputs.tsv (what went in; dist/package.ps1 reads it).
# 2. Unpacks them with the bsdtar that ships with Windows (System32\tar.exe).
# 3. Picks the kernel modules in modules-boot.txt (initramfs) and modules.txt plus
#    modules-boot.txt (system disk), with their modules.dep closures, and decompresses
#    them (CONFIG_MODULE_DECOMPRESS is off in linux-virt).
# 4. Builds guest/cmd/mkinitramfs and mksysdisk, and cross-compiles guest/cmd/agent,
#    sockfwd, netfwd and hyperbutlerd for linux/amd64.
# 5. Builds the system tree (system.spec) as a cpio, turns it into erofs with mkfs.erofs
#    in a pinned Alpine container on a Docker Engine (hyperbutler's own is fine; the
#    shared script is mkfs-erofs.sh), and wraps that as out\system.vhd (a fixed VHD).
# 6. Builds out\initramfs-stage1.gz (initramfs.spec) and copies vmlinuz-virt.
# 7. Writes out\image.json, the SHA-256 and size of all three, last.
#
# Needs: Go (go.mod version), Windows 10 1803+ for tar.exe, a docker CLI and a running
# Docker Engine with network access (for apk). Runs as a standard user.

[CmdletBinding()]
param(
    [string]$Cache = (Join-Path $env:TEMP 'hyperbutler-guest-build'),
    [string]$Out = '',
    [int]$GzipLevel = 9,          # 0 writes an uncompressed cpio
    [string]$Name = 'initramfs-stage1.gz',
    [string]$Docker = 'docker',
    [string]$DockerHost = '',     # empty: the docker CLI's current context
    # The mkfs.erofs container, pinned by digest, and the erofs-utils it installs. apk
    # checks the package signature; the version pin keeps the image bytes stable.
    [string]$Builder = 'alpine:3.24@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a',
    # The same pin is in scripts/guest-image/ci-entry.sh; bump both together.
    [string]$ErofsUtils = '1.9.1-r0',
    # lz4hc: small image, decompression cheap enough that boot does not notice.
    [string]$Compress = 'lz4hc,level=12',
    # Refuse inputs that are not signed Alpine packages (HYPERBUTLER_STRICT=1 in CI).
    [switch]$Strict
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$ProgressPreference = 'SilentlyContinue'   # Invoke-WebRequest is very slow otherwise

# $PSScriptRoot is not reliably set inside param() defaults on Windows PowerShell 5.1.
$proto = Split-Path -Parent $MyInvocation.MyCommand.Path
if (-not $Out) { $Out = Join-Path $proto '..\out' }
$repo = (Resolve-Path (Join-Path $proto '..\..')).Path
$tar = Join-Path $env:SystemRoot 'System32\tar.exe'
New-Item -ItemType Directory -Force $Cache, $Out | Out-Null
$Out = (Resolve-Path $Out).Path

# Run-Builder runs $script (sh) in the pinned Alpine container with $inFile on stdin and
# stdout in $outFile, both binary. cmd.exe redirection keeps them binary (PowerShell 5.1
# pipes are text). The script travels base64-encoded, which survives cmd.exe and docker
# quoting, and a batch file avoids PowerShell's native-argument quoting.
function Run-Builder([string]$what, [string]$script, [string]$inFile, [string]$outFile, [string[]]$runArgs = @()) {
    $b64 = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes(($script -replace "`r", '')))
    $dargs = @()
    if ($DockerHost) { $dargs += '-H', $DockerHost }
    $dargs += @('run', '--rm', '-i', '--pull=missing') + $runArgs + @($Builder, 'sh', '-c', "`"echo $b64 | base64 -d >/b.sh && exec sh /b.sh`"")
    $bat = Join-Path $Cache "stage\$what.cmd"
    New-Item -ItemType Directory -Force (Split-Path $bat) | Out-Null
    [IO.File]::WriteAllText($bat, "@`"$Docker`" $($dargs -join ' ') < `"$inFile`" > `"$outFile.part`"`r`n@exit /b %ERRORLEVEL%`r`n")
    & cmd.exe /d /c $bat
    if ($LASTEXITCODE -ne 0) { throw "$what in $Builder failed (exit $LASTEXITCODE)" }
    Move-Item -Force "$outFile.part" $outFile
}

# --- 1. fetch and verify ------------------------------------------------------------
$src = @{}
$pin = @{}      # file -> sha256, checked again right before each unpack
$pinned = @()   # dl/<name>/<file>, for the signature check
foreach ($line in Get-Content (Join-Path $proto 'sources.lock')) {
    if ($line -match '^\s*(#|$)') { continue }
    $srcName, $sha, $url = -split $line
    if ($srcName -notmatch '^[A-Za-z0-9][A-Za-z0-9._+-]*$') { throw "bad name in sources.lock: $srcName" }
    if ($url -notmatch '^https://') { throw "$srcName`: only https URLs are allowed: $url" }
    $fname = [IO.Path]::GetFileName(([Uri]$url).AbsolutePath)
    if ($fname -notmatch '^[A-Za-z0-9][A-Za-z0-9._+-]*$') { throw "$srcName`: bad file name in URL: $fname" }
    $dir = Join-Path $Cache "dl\$srcName"
    New-Item -ItemType Directory -Force $dir | Out-Null
    $file = Join-Path $dir $fname
    $pinned += "dl/$srcName/$fname"
    if (-not (Test-Path $file)) {
        Write-Host "fetch  $url"
        Invoke-WebRequest -UseBasicParsing -Uri $url -OutFile "$file.part"
        Move-Item -Force "$file.part" $file
    }
    $got = (Get-FileHash -Algorithm SHA256 $file).Hash.ToLowerInvariant()
    if ($got -ne $sha) {
        Remove-Item -Force $file   # so the next run downloads it again
        throw "sha256 mismatch for $srcName`n  file: $file`n  want: $sha`n  got:  $got"
    }
    Write-Host "ok     $srcName $sha"
    $src[$srcName] = $file
    $pin[$file] = $sha
}

# Signatures. The container gets one tar on stdin (the lock, fetch-verify.sh and exactly
# the pinned files), has no network, and runs the same fetch-verify.sh as CI: SHA-256
# pin again, `apk verify` against the pinned image's keys, and the signed .PKGINFO must
# name the pinned file. Its manifest comes back on stdout.
$vtar = Join-Path $Cache 'stage\verify.tar'
New-Item -ItemType Directory -Force (Split-Path $vtar) | Out-Null
$targs = @('--format', 'ustar', '-cf', $vtar, '-C', $proto, 'sources.lock', '-C', (Join-Path $repo 'scripts\guest-image'), 'fetch-verify.sh', '-C', $Cache) + $pinned
& $tar @targs
if ($LASTEXITCODE -ne 0) { throw 'tar (verify input) failed' }
$strictFlag = if ($Strict) { '1' } else { '0' }
$verifyScript = @"
set -e
mkdir /in && cd /in && tar -xf - >&2
HYPERBUTLER_STRICT=$strictFlag sh /in/fetch-verify.sh /in/sources.lock /in /in/inputs.tsv >&2
cat /in/inputs.tsv
"@
Run-Builder 'verify-inputs' $verifyScript $vtar (Join-Path $Out 'guest-inputs.tsv') @('--network=none')
Remove-Item -Force $vtar
foreach ($l in Get-Content (Join-Path $Out 'guest-inputs.tsv')) {
    $c = $l -split "`t"
    if ($c.Count -ne 10 -or $null -eq $src[$c[0]]) { throw "unexpected line in guest-inputs.tsv: $l" }
}
if (@(Get-Content (Join-Path $Out 'guest-inputs.tsv')).Count -ne $src.Count) { throw 'guest-inputs.tsv does not list every input' }

# --- 2. unpack ------------------------------------------------------------------------
function Expand-Fresh([string]$archive, [string]$dest, [string[]]$skip = @(), [string[]]$only = @()) {
    # The cache is the user's own directory, but check the pin once more so the bytes
    # unpacked are the ones the container verified, not a later replacement.
    if ((Get-FileHash -Algorithm SHA256 $archive).Hash.ToLowerInvariant() -ne $pin[$archive]) {
        throw "$archive changed after it was verified"
    }
    if (Test-Path $dest) { Remove-Item -Recurse -Force $dest }
    New-Item -ItemType Directory -Force $dest | Out-Null
    # .apk files are three concatenated gzip members; bsdtar reads them as one tar.
    # $skip lists the symlinks these pinned archives contain (none are needed): creating
    # symlinks on NTFS needs Developer Mode or admin, and bsdtar fails on them otherwise.
    # A new upstream symlink shows up as a tar error here; add it to the list.
    $targs = @('-xf', $archive, '-C', $dest)
    foreach ($s in $skip) { $targs += '--exclude', $s }
    $targs += $only   # member names: extract just these
    $ErrorActionPreference = 'Continue'   # bsdtar warns on stderr about apk PAX headers
    & $tar @targs 2>&1 | ForEach-Object { "$_" } | Where-Object { $_ -notmatch 'Ignoring unknown extended header' } | ForEach-Object { Write-Host "tar: $_" }
    if ($LASTEXITCODE -ne 0) { throw "tar failed on $archive" }
}
$x = Join-Path $Cache 'x'
Expand-Fresh $src['linux-virt'] "$x\linux-virt" 'lib/modules/*/vmlinuz'
Expand-Fresh $src['busybox-static'] "$x\busybox-static"
# The engine packages. docker-engine's usr/bin/docker-init and libseccomp's soname link
# are symlinks; the spec installs the files they point to.
Expand-Fresh $src['docker-engine'] "$x\docker-engine" 'usr/bin/docker-init'
Expand-Fresh $src['containerd'] "$x\containerd"
Expand-Fresh $src['runc'] "$x\runc"
Expand-Fresh $src['libseccomp'] "$x\libseccomp" 'usr/lib/libseccomp.so.2'
Expand-Fresh $src['tini-static'] "$x\tini-static"
# iptables: only the files system.spec names. The package's command names and extension
# aliases are symlinks, and nine extension pairs differ only in case, which NTFS cannot hold.
Expand-Fresh $src['iptables'] "$x\iptables" @() @('usr/sbin/xtables-nft-multi', 'usr/lib/xtables/libxt_standard.so', 'usr/lib/xtables/libxt_tcp.so', 'usr/lib/xtables/libxt_udp.so', 'usr/lib/xtables/libxt_NAT.so')
Expand-Fresh $src['libxtables'] "$x\libxtables" 'usr/lib/libxtables.so.12'
Expand-Fresh $src['libnftnl'] "$x\libnftnl" 'usr/lib/libnftnl.so.11'
Expand-Fresh $src['libmnl'] "$x\libmnl" 'usr/lib/libmnl.so.0'
Expand-Fresh $src['ca-certificates-bundle'] "$x\ca-certificates-bundle" 'etc/ssl/cert.pem', 'etc/ssl1.1'
# e2fsprogs and its libraries share one tree. The skipped entries are the soname and
# fsck/mkfs alias symlinks; the spec installs the real files under their sonames.
$e2fs = "$x\e2fsprogs"
Expand-Fresh $src['e2fsprogs'] $e2fs 'sbin/fsck.*', 'sbin/mkfs.*'
foreach ($n in 'e2fsprogs-libs', 'libblkid', 'libcom_err', 'libeconf', 'libuuid') {
    Expand-Fresh $src[$n] "$x\$n" 'usr/lib/lib*.so.?'
    Copy-Item -Recurse -Force "$x\$n\usr" $e2fs
}
Expand-Fresh $src['musl'] "$x\musl" 'lib/libc.musl-x86_64.so.1'

# --- 3. kernel modules ----------------------------------------------------------------
$kdirs = @(Get-ChildItem -Directory "$x\linux-virt\lib\modules")
if ($kdirs.Count -ne 1) { throw "expected one kernel version in linux-virt, found $($kdirs.Count)" }
$kver = $kdirs[0].Name
$kmod = $kdirs[0].FullName

$dep = @{}       # "kernel/fs/overlayfs/overlay.ko.gz" -> @(deps)
$byName = @{}    # "overlay" -> path (module names treat - and _ alike)
foreach ($l in Get-Content "$kmod\modules.dep") {
    $path, $rest = $l -split ':', 2
    $dep[$path] = @(-split $rest | Where-Object { $_ })
    $n = ([IO.Path]::GetFileName($path) -replace '\.ko(\.gz|\.xz|\.zst)?$', '') -replace '-', '_'
    $byName[$n] = $path
}

# Stage-Modules decompresses the modules named in $lists, plus their modules.dep
# closure, into $stage\<kver> with a matching modules.dep.
function Stage-Modules([string[]]$lists, [string]$stage) {
    $want = [Collections.Generic.SortedSet[string]]::new([StringComparer]::Ordinal)
    $queue = [Collections.Generic.Queue[string]]::new()
    foreach ($list in $lists) {
        foreach ($m in Get-Content (Join-Path $proto $list)) {
            $m = $m.Trim()
            if ($m -eq '' -or $m.StartsWith('#')) { continue }
            $p = $byName[$m -replace '-', '_']
            if (-not $p) { throw "module $m ($list) is not in modules.dep (built in, or misspelled?)" }
            $queue.Enqueue($p)
        }
    }
    while ($queue.Count) {
        $p = $queue.Dequeue()
        if ($want.Add($p)) { foreach ($d in $dep[$p]) { $queue.Enqueue($d) } }
    }
    if (Test-Path $stage) { Remove-Item -Recurse -Force $stage }
    $sdir = Join-Path $stage $kver
    New-Item -ItemType Directory -Force $sdir | Out-Null
    $depOut = [Text.StringBuilder]::new()
    foreach ($p in $want) {
        $plain = $p -replace '\.gz$', ''
        $dst = Join-Path $sdir ($plain -replace '/', '\')
        New-Item -ItemType Directory -Force (Split-Path $dst) | Out-Null
        $in = [IO.File]::OpenRead((Join-Path $kmod ($p -replace '/', '\')))
        try {
            $gz = [IO.Compression.GZipStream]::new($in, [IO.Compression.CompressionMode]::Decompress)
            $o = [IO.File]::Create($dst)
            try { $gz.CopyTo($o) } finally { $o.Dispose(); $gz.Dispose() }
        } finally { $in.Dispose() }
        $deps = ($dep[$p] | ForEach-Object { $_ -replace '\.gz$', '' }) -join ' '
        [void]$depOut.Append("${plain}:").Append($(if ($deps) { " $deps" } else { '' })).Append("`n")
    }
    [IO.File]::WriteAllText("$sdir\modules.dep", $depOut.ToString())
    foreach ($f in 'modules.builtin', 'modules.order') { Copy-Item "$kmod\$f" $sdir }
    Write-Host "modules $kver ($($lists -join ', ')): $($want.Count) files"
}
$bootStage = Join-Path $Cache 'stage\modules-boot'
$sysStage = Join-Path $Cache 'stage\modules'
Stage-Modules @('modules-boot.txt') $bootStage
Stage-Modules @('modules-boot.txt', 'modules.txt') $sysStage

# --- 4. build the tools and the guest binaries ----------------------------------------
# The guest binaries (and the initramfs gzip) are byte-stable only within one Go version,
# so the image matches the CI build only with the same toolchain. Say which one this is.
$goVersion = (& go env GOVERSION)
$goMod = (Select-String -Path (Join-Path $repo 'go.mod') -Pattern '^go\s+(\S+)').Matches[0].Groups[1].Value
Write-Host "go     $goVersion (go.mod: go $goMod)"
if ($goVersion -notlike "go$goMod*") { Write-Warning "Go $goVersion is not the go.mod version $goMod; the image will not match the CI build" }
Push-Location $repo
try {
    foreach ($t in 'mkinitramfs', 'mksysdisk') {
        & go build -trimpath -o (Join-Path $Out "$t.exe") "./guest/cmd/$t"
        if ($LASTEXITCODE -ne 0) { throw "go build $t failed" }
    }
} finally { Pop-Location }
$mkinitramfs = Join-Path $Out 'mkinitramfs.exe'
$mksysdisk = Join-Path $Out 'mksysdisk.exe'

$guestBin = Join-Path $Out 'guest-bin'
New-Item -ItemType Directory -Force $guestBin | Out-Null
Push-Location $repo
$saved = $env:GOOS, $env:GOARCH, $env:CGO_ENABLED
try {
    $env:GOOS = 'linux'; $env:GOARCH = 'amd64'; $env:CGO_ENABLED = '0'
    foreach ($cmd in 'agent', 'sockfwd', 'netfwd', 'hyperbutlerd') {
        & go build -trimpath -buildvcs=false -ldflags '-s -w -buildid=' -o (Join-Path $guestBin $cmd) "./guest/cmd/$cmd"
        if ($LASTEXITCODE -ne 0) { throw "go build $cmd failed" }
    }
} finally { $env:GOOS, $env:GOARCH, $env:CGO_ENABLED = $saved; Pop-Location }

# --- 5. the system disk ----------------------------------------------------------------
# The tree is a deterministic uncompressed cpio (mtime 0, uid/gid 0), so file modes and
# links never touch NTFS. mkfs.erofs runs in a pinned Alpine container on any Docker
# Engine (hyperbutler's own works: no WSL); the cpio goes in on stdin and the image comes back
# on stdout. -T0 and a UUID derived from the cpio make the image a function of its input.
$cpio = Join-Path $Cache 'stage\system.cpio'
& $mkinitramfs -spec (Join-Path $proto 'system.spec') -mtime 0 -level 0 -o $cpio `
    -D "PROTO=$proto" -D "BUSYBOX=$x\busybox-static" -D "MODULES=$sysStage" -D "GUESTBIN=$guestBin" `
    -D "DOCKERENGINE=$x\docker-engine" -D "CONTAINERD=$x\containerd" -D "RUNC=$x\runc" -D "TINI=$x\tini-static" -D "SECCOMP=$x\libseccomp" `
    -D "IPTABLES=$x\iptables" -D "LIBXTABLES=$x\libxtables" -D "LIBNFTNL=$x\libnftnl" -D "LIBMNL=$x\libmnl" `
    -D "CACERTS=$x\ca-certificates-bundle" -D "E2FS=$e2fs" -D "MUSL=$x\musl"
if ($LASTEXITCODE -ne 0) { throw 'mkinitramfs (system) failed' }
$h = (Get-FileHash -Algorithm SHA256 $cpio).Hash.ToLowerInvariant()
$uuid = '{0}-{1}-4{2}-8{3}-{4}' -f $h.Substring(0, 8), $h.Substring(8, 4), $h.Substring(13, 3), $h.Substring(17, 3), $h.Substring(20, 12)
$erofs = Join-Path $Cache 'stage\system.erofs'
# mkfs-erofs.sh is the same script the CI build runs; it goes in as a quoted heredoc.
$mkfsScript = [IO.File]::ReadAllText((Join-Path $proto 'mkfs-erofs.sh'))
$script = "set -e`napk add --no-cache --quiet erofs-utils=$ErofsUtils >&2`n" +
    "cat >/m.sh <<'HYPERBUTLER_MKFS_EOF'`n$mkfsScript`nHYPERBUTLER_MKFS_EOF`n" +
    "sh /m.sh $uuid $Compress /sys.img`ncat /sys.img`n"
Run-Builder 'mkfs-erofs' $script $cpio $erofs
$sys = Join-Path $Out 'system.vhd'
& $mksysdisk -erofs $erofs -o $sys -mtime 0
if ($LASTEXITCODE -ne 0) { throw 'mksysdisk failed' }

# --- 6. the stage-1 initramfs and the kernel -------------------------------------------
$img = Join-Path $Out $Name
& $mkinitramfs -spec (Join-Path $proto 'initramfs.spec') -mtime 0 -level $GzipLevel -o $img `
    -D "PROTO=$proto" -D "BUSYBOX=$x\busybox-static" -D "BOOTMODULES=$bootStage"
if ($LASTEXITCODE -ne 0) { throw 'mkinitramfs failed' }
Copy-Item -Force "$x\linux-virt\boot\vmlinuz-virt" (Join-Path $Out 'vmlinuz-virt')

# --- 7. the manifest, last: a directory with image.json holds a complete set -----------
$man = Join-Path $Out 'image.json'
Remove-Item -Force $man -ErrorAction SilentlyContinue
& $mksysdisk -manifest $man -kernel (Join-Path $Out 'vmlinuz-virt') -initrd $img -system $sys
if ($LASTEXITCODE -ne 0) { throw 'mksysdisk -manifest failed' }

foreach ($f in 'vmlinuz-virt', $Name, 'system.vhd', 'image.json') {
    $p = Join-Path $Out $f
    '{0}  {1,11:N0} bytes  {2}' -f (Get-FileHash -Algorithm SHA256 $p).Hash.ToLowerInvariant(), (Get-Item $p).Length, $f
}

