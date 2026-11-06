# Builds the agent sandbox image (ADR 0009) on Windows, with no WSL: a Debian slim system
# disk with sbxd, booted by the Docker image's kernel and stage-1 initramfs, and its own
# image.json.
#
#   powershell -ExecutionPolicy Bypass -File guest\sandbox\build.ps1 [-Image DIR] [-Cache DIR]
#       [-Out DIR] [-Docker docker.exe] [-DockerHost npipe:////./pipe/...]
#
# 1. Checks that -Image (a finished guest\prototype\build.ps1 output) and the kernel
#    modules that build staged under -Cache belong together: the version in the kernel's
#    header must name the one modules directory there.
# 2. Cross-compiles sbxd and netfwd for linux/amd64 and builds mksysdisk.
# 3. Sends debian.lock, init, the two binaries and the modules, as one tar on stdin, into
#    the digest-pinned Debian container debian.lock names, which runs build-rootfs.sh:
#    mmdebstrap from the pinned snapshot (apt checks every signature against the image's
#    keyring), then mkfs.erofs. A tar with system.erofs and sandbox-packages.tsv comes back.
# 4. Wraps the erofs image as system.vhd, copies the kernel and the initramfs from -Image,
#    and writes image.json last.
#
# Needs: Go (go.mod version), the docker CLI and a running Docker Engine with network
# access (snapshot.debian.org). Runs as a standard user.

[CmdletBinding()]
param(
    [string]$Image = '',
    [string]$Cache = (Join-Path $env:TEMP 'hyperbutler-guest-build'),
    [string]$Out = '',
    [string]$Docker = 'docker',
    [string]$DockerHost = ''
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$here = Split-Path -Parent $MyInvocation.MyCommand.Path
$repo = (Resolve-Path (Join-Path $here '..\..')).Path
if (-not $Image) { $Image = Join-Path $repo 'guest\out' }
if (-not $Out) { $Out = Join-Path $repo 'guest\out-sandbox' }
$tar = Join-Path $env:SystemRoot 'System32\tar.exe'
New-Item -ItemType Directory -Force $Out | Out-Null
$Out = (Resolve-Path $Out).Path
$stage = Join-Path $Cache 'sandbox'
if (Test-Path $stage) { Remove-Item -Recurse -Force $stage }
New-Item -ItemType Directory -Force $stage | Out-Null

function Sha([string]$p) { (Get-FileHash -Algorithm SHA256 $p).Hash.ToLowerInvariant() }

# --- 1. the Docker image and its modules ------------------------------------------------
foreach ($f in 'image.json', 'vmlinuz-virt', 'initramfs-stage1.gz') {
    if (-not (Test-Path (Join-Path $Image $f))) { throw "$Image has no $f; run guest\prototype\build.ps1 first" }
}
$kernel = Join-Path $Image 'vmlinuz-virt'
# The bzImage setup header's kernel_version field (offset 0x20E) points, less 0x200, at
# the version string, whose first word names the modules directory.
$kb = [IO.File]::ReadAllBytes($kernel)
$vp = [BitConverter]::ToUInt16($kb, 0x20E) + 0x200
$ve = [Array]::IndexOf($kb, [byte]0, $vp)
if ($kb[0x202] -ne 0x48 -or $kb[0x203] -ne 0x64 -or $ve -lt 0) { throw "$kernel is not a bzImage" }
$kver = ([Text.Encoding]::ASCII.GetString($kb, $vp, $ve - $vp) -split ' ')[0]
$modules = Join-Path $Cache 'stage\modules'
$kdirs = @(Get-ChildItem -Directory $modules -ErrorAction SilentlyContinue)
if ($kdirs.Count -ne 1 -or $kdirs[0].Name -ne $kver) {
    throw "$modules does not hold the modules of kernel $kver alone; run guest\prototype\build.ps1 with the same -Cache first"
}
Write-Host "kernel $kver, $(Sha $kernel)"

# --- 2. binaries -------------------------------------------------------------------------
$goVersion = (& go env GOVERSION)
Write-Host "go     $goVersion"
Push-Location $repo
$saved = $env:GOOS, $env:GOARCH, $env:CGO_ENABLED
try {
    & go build -trimpath -o (Join-Path $stage 'mksysdisk.exe') ./guest/cmd/mksysdisk
    if ($LASTEXITCODE -ne 0) { throw 'go build mksysdisk failed' }
    $env:GOOS = 'linux'; $env:GOARCH = 'amd64'; $env:CGO_ENABLED = '0'
    foreach ($cmd in 'sbxd', 'netfwd') {
        & go build -trimpath -buildvcs=false -ldflags '-s -w -buildid=' -o (Join-Path $stage "in\$cmd") "./guest/cmd/$cmd"
        if ($LASTEXITCODE -ne 0) { throw "go build $cmd failed" }
    }
} finally { $env:GOOS, $env:GOARCH, $env:CGO_ENABLED = $saved; Pop-Location }
$mksysdisk = Join-Path $stage 'mksysdisk.exe'

# --- 3. the system tree, in the pinned container ----------------------------------------
$in = Join-Path $stage 'in'
Copy-Item (Join-Path $here 'debian.lock'), (Join-Path $here 'init'), (Join-Path $here 'build-rootfs.sh') $in
Copy-Item -Recurse $modules (Join-Path $in 'modules')
$inTar = Join-Path $stage 'in.tar'
& $tar --format ustar -cf $inTar -C $in build-rootfs.sh debian.lock init sbxd netfwd modules
if ($LASTEXITCODE -ne 0) { throw 'tar (input) failed' }

$builder = (Select-String -Path (Join-Path $here 'debian.lock') -Pattern '^builder\s+(\S+)').Matches[0].Groups[1].Value
if ($builder -notmatch '@sha256:[0-9a-f]{64}$') { throw "debian.lock: the builder is not pinned by digest: $builder" }
# The script travels in the input tar (on a command line it would pass cmd.exe's 8191
# characters), and the tars through cmd.exe redirection, which keeps them binary.
$dargs = @()
if ($DockerHost) { $dargs += '-H', $DockerHost }
$dargs += @('run', '--rm', '-i', '--pull=missing', '--cap-add', 'SYS_ADMIN', $builder, 'sh', '-c', '"mkdir /in && tar -xf - -C /in && exec sh /in/build-rootfs.sh /in"')
$outTar = Join-Path $stage 'out.tar'
$bat = Join-Path $stage 'build-rootfs.cmd'
[IO.File]::WriteAllText($bat, "@`"$Docker`" $($dargs -join ' ') < `"$inTar`" > `"$outTar.part`"`r`n@exit /b %ERRORLEVEL%`r`n")
& cmd.exe /d /c $bat
if ($LASTEXITCODE -ne 0) { throw "build-rootfs.sh in $builder failed (exit $LASTEXITCODE)" }
Move-Item -Force "$outTar.part" $outTar
$res = Join-Path $stage 'out'
New-Item -ItemType Directory -Force $res | Out-Null
& $tar -xf $outTar -C $res
if ($LASTEXITCODE -ne 0) { throw 'tar (output) failed' }

# --- 4. the disk, the boot files and the manifest, last ----------------------------------
$man = Join-Path $Out 'image.json'
Remove-Item -Force $man -ErrorAction SilentlyContinue
$sys = Join-Path $Out 'system.vhd'
& $mksysdisk -erofs (Join-Path $res 'system.erofs') -o $sys -mtime 0
if ($LASTEXITCODE -ne 0) { throw 'mksysdisk failed' }
Copy-Item -Force (Join-Path $res 'sandbox-packages.tsv') $Out
Copy-Item -Force $kernel, (Join-Path $Image 'initramfs-stage1.gz') $Out
& $mksysdisk -manifest $man -kernel (Join-Path $Out 'vmlinuz-virt') -initrd (Join-Path $Out 'initramfs-stage1.gz') -system $sys
if ($LASTEXITCODE -ne 0) { throw 'mksysdisk -manifest failed' }

foreach ($f in 'vmlinuz-virt', 'initramfs-stage1.gz', 'system.vhd', 'sandbox-packages.tsv', 'image.json') {
    $p = Join-Path $Out $f
    '{0}  {1,13:N0} bytes  {2}' -f (Sha $p), (Get-Item $p).Length, $f
}
