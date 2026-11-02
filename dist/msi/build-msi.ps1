# Builds the hyperbutler MSI from the folder dist/package.ps1 stages (dist/out/<name>, the
# zip's contents), with WiX 5 (docs/install.md).
#
#   pwsh -File dist\msi\build-msi.ps1 -Stage DIR [-Wix PATH] [-Out DIR] [-MaxBytes N]
#
# -Wix is wix.exe from the WiX 5 .NET tool (`dotnet tool install wix --version 5.0.2`),
# default `wix` on PATH. The WiX extensions are fetched from nuget.org at the same version
# into a work folder next to the MSI, so the build never uses or changes a global WiX cache.
#
# The MSI version is the X.Y.Z of the stage's BUILDINFO.txt version when that is a release
# tag or its -alpha/-beta/-rc; anything else (a snapshot) builds as 0.0.0, which any release
# upgrades. The MSI allows same-version upgrades, so v0.2.0 replaces v0.2.0-rc.1.

[CmdletBinding()]
param(
    [Parameter(Mandatory)] [string]$Stage,
    [string]$Wix = 'wix',
    [string]$Out = '',
    [long]$MaxBytes = 150MB   # REQ-UX-002: the installer is at most 150 MB
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$here = Split-Path -Parent $MyInvocation.MyCommand.Path
$wixVersion = '5.0.2'
function Fail([string]$msg) { throw "build-msi: $msg" }

$Stage = (Resolve-Path -LiteralPath $Stage).Path
foreach ($f in 'hyperbutler.exe', 'WebView2Loader.dll', 'BUILDINFO.txt', 'image\image.json', 'clients\docker.exe') {
    if (-not (Test-Path -LiteralPath (Join-Path $Stage $f) -PathType Leaf)) { Fail "$Stage has no $f; give the folder dist/package.ps1 staged" }
}
$name = Split-Path -Leaf $Stage
if (-not $Out) { $Out = Split-Path -Parent $Stage }
New-Item -ItemType Directory -Force $Out | Out-Null
$Out = (Resolve-Path $Out).Path

$version = ''
foreach ($l in Get-Content -LiteralPath (Join-Path $Stage 'BUILDINFO.txt')) {
    if ($l -match '^version: (.+)$') { $version = $Matches[1].Trim() }
}
if (-not $version) { Fail 'BUILDINFO.txt names no version' }
$productVersion = '0.0.0'
# A release tag, or one of its pre-releases (-rc.1 and the like): not a `git describe` of a
# later commit (v0.2.0-5-gabc1234) or a dirty tree, which would share the tag's version.
if ($version -match '^v(\d{1,3})\.(\d{1,3})\.(\d{1,5})(?:-(?:alpha|beta|rc)(?:\.?\d+)?)?$') {
    if ([int]$Matches[1] -gt 255 -or [int]$Matches[2] -gt 255 -or [int]$Matches[3] -gt 65535) { Fail "version $version does not fit an MSI version" }
    $productVersion = "$($Matches[1]).$($Matches[2]).$($Matches[3])"
}

$wixOut = (& $Wix --version) | Select-Object -First 1
if ($LASTEXITCODE -ne 0 -or "$wixOut" -notmatch "^$([regex]::Escape($wixVersion))\b") { Fail "$Wix is '$wixOut', want WiX $wixVersion" }

$work = Join-Path $Out 'msi-work'
if (Test-Path $work) { Remove-Item -Recurse -Force $work }
New-Item -ItemType Directory $work | Out-Null
$msi = Join-Path $Out "$name.msi"
if (Test-Path $msi) { Remove-Item -Force $msi }

Push-Location $work
try {
    # `wix extension add` without -g keeps the extensions in .wix\ under the current folder.
    foreach ($ext in 'WixToolset.Util.wixext', 'WixToolset.UI.wixext') {
        & $Wix extension add "$ext/$wixVersion"
        if ($LASTEXITCODE -ne 0) { Fail "wix extension add $ext/$wixVersion failed" }
    }
    & $Wix build -arch x64 -nologo `
        -ext "WixToolset.Util.wixext/$wixVersion" -ext "WixToolset.UI.wixext/$wixVersion" `
        -d "ProductVersion=$productVersion" -d "Version=$version" `
        -bindpath "stage=$Stage\" `
        -o $msi (Join-Path $here 'hyperbutler.wxs')
    if ($LASTEXITCODE -ne 0) { Fail "wix build failed (exit $LASTEXITCODE)" }
    # The internal consistency evaluators. ICE43 and ICE57 take the Start menu for a
    # per-user folder and want an HKCU key path for its shortcut; in this per-machine
    # package (ALLUSERS=1) it is the all-users Start menu, and an HKCU key would be
    # written for the installing user only.
    & $Wix msi validate -sice ICE43 -sice ICE57 $msi
    if ($LASTEXITCODE -ne 0) { Fail "wix msi validate failed (exit $LASTEXITCODE)" }
} finally { Pop-Location }

$bytes = (Get-Item $msi).Length
$sizeLine = '{0}.msi: {1:N1} MB ({2:N0} bytes), MSI version {3}; REQ-UX-002 limit {4:N0} MB' -f $name, ($bytes / 1MB), $bytes, $productVersion, ($MaxBytes / 1MB)
Write-Host $sizeLine
if ($bytes -gt $MaxBytes) { Fail "the MSI is over REQ-UX-002's limit: $bytes > $MaxBytes bytes" }
if ($env:GITHUB_OUTPUT) {
    Add-Content -Encoding utf8 -LiteralPath $env:GITHUB_OUTPUT -Value @(
        "msi=$msi", "msi_sha256=$((Get-FileHash -Algorithm SHA256 $msi).Hash.ToLowerInvariant())", "msi_bytes=$bytes")
}
