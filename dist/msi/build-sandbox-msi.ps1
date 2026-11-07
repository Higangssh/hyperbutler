# Builds the agent sandbox image's MSI (sandbox.wxs, docs/install.md) with WiX 5.
# dist/package.ps1 runs it before it links hyperbutler.exe, which pins the result by hash.
#
#   pwsh -File dist\msi\build-sandbox-msi.ps1 -ImageDir DIR -Version vX.Y.Z -Name NAME -ProductCode GUID [-Wix PATH] [-Out DIR]
#
# -ProductCode is uppercase, without braces; package.ps1 makes a new one per build and pins it.
# -ImageDir holds the image files: the sandbox-image artifact, or guest/sandbox/build.ps1's
# output folder. -Name is the file name without .msi. It is a release asset of its own, so
# REQ-UX-002's limit does not apply; -MaxBytes is the limit sandboxmsi.MaxPinBytes takes.

[CmdletBinding()]
param(
    [Parameter(Mandatory)] [string]$ImageDir,
    [Parameter(Mandatory)] [string]$Version,
    [Parameter(Mandatory)] [string]$Name,
    [Parameter(Mandatory)] [string]$ProductCode,
    [string]$Wix = 'wix',
    [string]$Out = '',
    [long]$MaxBytes = 1GB
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$here = Split-Path -Parent $MyInvocation.MyCommand.Path
$wixVersion = '5.0.2'
function Fail([string]$msg) { throw "build-sandbox-msi: $msg" }
. (Join-Path $here 'msi-version.ps1')

$ImageDir = (Resolve-Path -LiteralPath $ImageDir).Path
foreach ($f in 'image.json', 'vmlinuz-virt', 'initramfs-stage1.gz', 'system.vhd', 'sandbox-packages.tsv') {
    if (-not (Test-Path -LiteralPath (Join-Path $ImageDir $f) -PathType Leaf)) { Fail "$ImageDir has no $f" }
}
if ($Name -notmatch '^[A-Za-z0-9][A-Za-z0-9._+-]*$') { Fail "bad name: $Name" }
if ($ProductCode -cnotmatch '^[0-9A-F]{8}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{12}$') { Fail "bad product code: $ProductCode" }
if (-not $Out) { $Out = Split-Path -Parent $ImageDir }
New-Item -ItemType Directory -Force $Out | Out-Null
$Out = (Resolve-Path $Out).Path
$productVersion = MsiVersion $Version

$wixOut = (& $Wix --version) | Select-Object -First 1
if ($LASTEXITCODE -ne 0 -or "$wixOut" -notmatch "^$([regex]::Escape($wixVersion))\b") { Fail "$Wix is '$wixOut', want WiX $wixVersion" }

$msi = Join-Path $Out "$Name.msi"
if (Test-Path $msi) { Remove-Item -Force $msi }
$work = Join-Path $Out 'sandbox-msi-work'
if (Test-Path $work) { Remove-Item -Recurse -Force $work }
New-Item -ItemType Directory $work | Out-Null
$hasOffer = [int](Test-Path -LiteralPath (Join-Path $ImageDir 'SANDBOX-SOURCES.txt') -PathType Leaf)

Push-Location $work
try {
    # No extensions: sandbox.wxs has no UI and no custom action of its own.
    & $Wix build -arch x64 -nologo `
        -d "ProductVersion=$productVersion" -d "Version=$Version" -d "HasSourceOffer=$hasOffer" -d "ProductCode={$ProductCode}" `
        -bindpath "image=$ImageDir\\" `
        -o $msi (Join-Path $here 'sandbox.wxs')
    if ($LASTEXITCODE -ne 0) { Fail "wix build failed (exit $LASTEXITCODE)" }
    # The internal consistency evaluators; see build-msi.ps1. Nothing to leave out: this
    # package has no shortcut.
    & $Wix msi validate $msi
    if ($LASTEXITCODE -ne 0) { Fail "wix msi validate failed (exit $LASTEXITCODE)" }
} finally { Pop-Location }
Remove-Item -Recurse -Force $work

$bytes = (Get-Item -LiteralPath $msi).Length
Write-Host ('{0}.msi: {1:N1} MB ({2:N0} bytes), MSI version {3}' -f $Name, ($bytes / 1MB), $bytes, $productVersion)
if ($bytes -gt $MaxBytes) { Fail "the MSI is over its limit: $bytes > $MaxBytes bytes" }
if ($env:GITHUB_OUTPUT) {
    Add-Content -Encoding utf8 -LiteralPath $env:GITHUB_OUTPUT -Value @(
        "sandbox_msi=$msi", "sandbox_msi_name=$Name", "sandbox_msi_sha256=$((Get-FileHash -Algorithm SHA256 -LiteralPath $msi).Hash.ToLowerInvariant())", "sandbox_msi_bytes=$bytes")
}
