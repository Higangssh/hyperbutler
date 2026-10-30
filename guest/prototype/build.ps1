# Builds the ADR 0005 prototype initramfs on Windows, with no WSL and no Linux tools.
#
#   powershell -ExecutionPolicy Bypass -File guest\prototype\build.ps1 [-Cache DIR] [-Out DIR]
#
# 1. Downloads every input in sources.lock into its own directory under -Cache and
#    checks its SHA-256 pin. A mismatch is fatal.
# 2. Unpacks them with the bsdtar that ships with Windows (System32\tar.exe).
# 3. Picks the kernel modules in modules.txt plus their modules.dep closure and
#    decompresses them (CONFIG_MODULE_DECOMPRESS is off in linux-virt).
# 4. Cross-compiles guest/cmd/agent, sockfwd and netfwd for linux/amd64, builds out\initramfs-winbox.gz
#    with guest/cmd/mkinitramfs, and copies the matching vmlinuz-virt next to it.
#
# Needs: Go (go.mod version), Windows 10 1803+ for tar.exe. Runs as a standard user.

[CmdletBinding()]
param(
    [string]$Cache = (Join-Path $env:TEMP 'winbox-guest-build'),
    [string]$Out = '',
    [int]$GzipLevel = 9,          # 0 writes an uncompressed cpio
    [string]$Name = 'initramfs-winbox.gz'
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

# --- 1. fetch and verify ------------------------------------------------------------
$src = @{}
foreach ($line in Get-Content (Join-Path $proto 'sources.lock')) {
    if ($line -match '^\s*(#|$)') { continue }
    $srcName, $sha, $url = -split $line
    $dir = Join-Path $Cache "dl\$srcName"
    New-Item -ItemType Directory -Force $dir | Out-Null
    $file = Join-Path $dir ([IO.Path]::GetFileName(([Uri]$url).AbsolutePath))
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
}

# --- 2. unpack ------------------------------------------------------------------------
function Expand-Fresh([string]$archive, [string]$dest, [string[]]$skip = @()) {
    if (Test-Path $dest) { Remove-Item -Recurse -Force $dest }
    New-Item -ItemType Directory -Force $dest | Out-Null
    # .apk files are three concatenated gzip members; bsdtar reads them as one tar.
    # $skip lists the symlinks these pinned archives contain (none are needed): creating
    # symlinks on NTFS needs Developer Mode or admin, and bsdtar fails on them otherwise.
    # A new upstream symlink shows up as a tar error here; add it to the list.
    $targs = @('-xf', $archive, '-C', $dest)
    foreach ($s in $skip) { $targs += '--exclude', $s }
    $ErrorActionPreference = 'Continue'   # bsdtar warns on stderr about apk PAX headers
    & $tar @targs 2>&1 | ForEach-Object { "$_" } | Where-Object { $_ -notmatch 'Ignoring unknown extended header' } | ForEach-Object { Write-Host "tar: $_" }
    if ($LASTEXITCODE -ne 0) { throw "tar failed on $archive" }
}
$x = Join-Path $Cache 'x'
Expand-Fresh $src['linux-virt'] "$x\linux-virt" 'lib/modules/*/vmlinuz'
Expand-Fresh $src['busybox-static'] "$x\busybox-static"
Expand-Fresh $src['docker-static'] "$x\docker-static"
Expand-Fresh $src['ca-certificates-bundle'] "$x\ca-certificates-bundle" 'etc/ssl/cert.pem', 'etc/ssl1.1'

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
$want = [Collections.Generic.SortedSet[string]]::new([StringComparer]::Ordinal)
$queue = [Collections.Generic.Queue[string]]::new()
foreach ($m in Get-Content (Join-Path $proto 'modules.txt')) {
    $m = $m.Trim()
    if ($m -eq '' -or $m.StartsWith('#')) { continue }
    $p = $byName[$m -replace '-', '_']
    if (-not $p) { throw "module $m is not in modules.dep (built in, or misspelled?)" }
    $queue.Enqueue($p)
}
while ($queue.Count) {
    $p = $queue.Dequeue()
    if ($want.Add($p)) { foreach ($d in $dep[$p]) { $queue.Enqueue($d) } }
}

$stage = Join-Path $Cache 'stage\modules'
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
Write-Host "modules $kver : $($want.Count) files"

# --- 4. build -------------------------------------------------------------------------
$tool = Join-Path $Out 'mkinitramfs.exe'
Push-Location $repo
try {
    & go build -trimpath -o $tool ./guest/cmd/mkinitramfs
    if ($LASTEXITCODE -ne 0) { throw 'go build failed' }
} finally { Pop-Location }

$guestBin = Join-Path $Out 'guest-bin'
New-Item -ItemType Directory -Force $guestBin | Out-Null
Push-Location $repo
$saved = $env:GOOS, $env:GOARCH, $env:CGO_ENABLED
try {
    $env:GOOS = 'linux'; $env:GOARCH = 'amd64'; $env:CGO_ENABLED = '0'
    foreach ($cmd in 'agent', 'sockfwd', 'netfwd') {
        & go build -trimpath -buildvcs=false -ldflags '-s -w -buildid=' -o (Join-Path $guestBin $cmd) "./guest/cmd/$cmd"
        if ($LASTEXITCODE -ne 0) { throw "go build $cmd failed" }
    }
} finally { $env:GOOS, $env:GOARCH, $env:CGO_ENABLED = $saved; Pop-Location }

$img = Join-Path $Out $Name
& $tool -spec (Join-Path $proto 'initramfs.spec') -mtime 0 -level $GzipLevel -o $img `
    -D "PROTO=$proto" -D "BUSYBOX=$x\busybox-static" -D "DOCKER=$x\docker-static\docker" -D "MODULES=$stage" -D "GUESTBIN=$guestBin" -D "CACERTS=$x\ca-certificates-bundle"
if ($LASTEXITCODE -ne 0) { throw 'mkinitramfs failed' }
Copy-Item -Force "$x\linux-virt\boot\vmlinuz-virt" (Join-Path $Out 'vmlinuz-virt')

foreach ($f in 'vmlinuz-virt', $Name) {
    $p = Join-Path $Out $f
    '{0}  {1,11:N0} bytes  {2}' -f (Get-FileHash -Algorithm SHA256 $p).Hash.ToLowerInvariant(), (Get-Item $p).Length, $f
}
