# Assembles the portable winbox release zip. No installer and no admin: unzip, add the
# folder to PATH, run `winbox start`.
#
#   pwsh -File dist\package.ps1 -ImageDir DIR [-SourceBundle FILE] [-ReleaseUrl URL]
#        [-ImageFiles a,b] [-Version vX.Y.Z] [-Out DIR] [-Snapshot]
#
# -ImageDir is the guest-image artifact (.github/workflows/guest-image.yml): the image
# files, SHA256SUMS, guest-inputs.tsv, BUILDINFO.txt and licenses/. -SourceBundle is the
# guest-sources.tar from the same run; it is published as a separate release asset, and
# SOURCE-OFFER.txt in the zip names it by file name and SHA-256.
#
# Without -Snapshot (the release workflow), every check is fatal: a clean tree, a tag
# version that winbox.exe reports itself, image hashes that match the artifact's
# SHA256SUMS, guest license texts, a source bundle, and REQ-UX-002's 150 MB limit.
# -Snapshot relaxes the first five for local dry runs and marks the zip as not for
# redistribution. The size limit always applies.
#
# Reproducibility: CGO_ENABLED=0, -trimpath, -buildvcs=true (the version comes from the
# git tag, as `git describe` sees it), -ldflags "-s -w -buildid=", GOTOOLCHAIN=local, and
# zip entries in ordinal order with the commit time as their timestamp. The same commit
# and Go version give the same winbox.exe; the zip also depends on the .NET deflate
# implementation, so compare winbox.exe, not the zip, across machines.

[CmdletBinding()]
param(
    [Parameter(Mandatory)] [string]$ImageDir,
    [string[]]$ImageFiles = @('vmlinuz-virt', 'initramfs-winbox.gz'),
    [string]$SourceBundle = '',
    [string]$ReleaseUrl = '',
    [string]$Version = '',
    [string]$Out = '',
    [switch]$Snapshot,
    [long]$MaxBytes = 150MB   # REQ-UX-002: the installer is at most 150 MB
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
Add-Type -AssemblyName System.IO.Compression

$dist = Split-Path -Parent $MyInvocation.MyCommand.Path
$repo = (Resolve-Path (Join-Path $dist '..')).Path
if (-not $Out) { $Out = Join-Path $dist 'out' }
New-Item -ItemType Directory -Force $Out | Out-Null
$Out = (Resolve-Path $Out).Path
$ImageDir = (Resolve-Path $ImageDir).Path

function Fail([string]$msg) { throw "package: $msg" }
function Warn([string]$msg) {
    if ($Snapshot) { Write-Warning "$msg (allowed with -Snapshot)" } else { Fail $msg }
}
function Exec([string]$exe) {
    $o = & $exe @args
    if ($LASTEXITCODE -ne 0) { Fail "$exe $($args -join ' ') failed (exit $LASTEXITCODE)" }
    # Emit nothing (not $null) for no output, so @(Exec ...).Count is 0.
    $o | Where-Object { $null -ne $_ }
}
function Sha256([string]$path) { (Get-FileHash -Algorithm SHA256 -LiteralPath $path).Hash.ToLowerInvariant() }
function WriteText([string]$path, [string[]]$lines) {
    # LF and UTF-8 without BOM on both PowerShell editions, so the bytes don't depend on
    # the shell that ran this.
    [IO.File]::WriteAllText($path, (($lines -join "`n") + "`n"), [Text.UTF8Encoding]::new($false))
}

Push-Location $repo
try {
    # --- version and tree state -----------------------------------------------------------
    $dirty = @(Exec git status --porcelain)
    if ($dirty.Count) { Warn "the working tree has uncommitted changes" }
    $commit = (Exec git rev-parse HEAD) | Select-Object -First 1
    $commitTime = [long]((Exec git log -1 --format=%ct HEAD) | Select-Object -First 1)
    if (-not $Version) {
        $Version = (Exec git describe --tags --match 'v[0-9]*' --always --dirty) | Select-Object -First 1
    }
    if ($Version -notmatch '^[A-Za-z0-9._+-]+$') { Fail "bad version: $Version" }
    if (-not $Snapshot -and $Version -notmatch '^v\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$') {
        Fail "a release version must be a semver tag (vX.Y.Z[-pre]), got $Version"
    }
    $name = "winbox-$Version-windows-amd64"
    $stage = Join-Path $Out $name
    if (Test-Path $stage) { Remove-Item -Recurse -Force $stage }
    New-Item -ItemType Directory -Force $stage | Out-Null
    Write-Host "packaging $name from $commit"

    # --- winbox.exe -------------------------------------------------------------------------
    $exe = Join-Path $stage 'winbox.exe'
    $saved = $env:GOOS, $env:GOARCH, $env:CGO_ENABLED, $env:GOTOOLCHAIN, $env:GOFLAGS
    try {
        $env:GOOS = 'windows'; $env:GOARCH = 'amd64'; $env:CGO_ENABLED = '0'
        $env:GOTOOLCHAIN = 'local'; $env:GOFLAGS = '-mod=readonly'
        Exec go build -trimpath -buildvcs=true -ldflags '-s -w -buildid=' -o $exe ./cmd/winbox | Out-Null
        $goVersion = (Exec go version) | Select-Object -First 1
        $goroot = (Exec go env GOROOT) | Select-Object -First 1

        # What the binary says about itself: its own version and the modules linked in.
        $info = @(Exec go version -m $exe)
        $modVersion = ''
        $deps = [Collections.Generic.List[object]]::new()
        foreach ($l in $info) {
            $f = $l.Trim() -split "`t"
            if ($f[0] -eq 'mod' -and $f.Count -ge 3) { $modVersion = $f[2] }
            elseif ($f[0] -eq 'dep' -and $f.Count -ge 3) { $deps.Add([pscustomobject]@{ Path = $f[1]; Version = $f[2] }) }
            elseif ($f[0] -eq '=>') { Fail "replaced module in winbox.exe: $l" }
        }
        if ($modVersion -ne $Version) {
            Warn "winbox.exe reports version '$modVersion', expected '$Version' (is HEAD tagged and the tree clean?)"
        }
        # The guest binaries (guest/cmd/*, GOOS=linux) are Go too and ship inside the
        # initramfs. Add the modules they link that winbox.exe does not.
        $env:GOOS = 'linux'
        $seen = @{}
        foreach ($d in $deps) { $seen["$($d.Path)@$($d.Version)"] = $true }
        $mod = (Exec go list -m) | Select-Object -First 1
        foreach ($l in @(Exec go list -deps -f '{{with .Module}}{{.Path}} {{.Version}}{{end}}' ./guest/cmd/...)) {
            $p, $v = -split $l
            if (-not $v -or $p -eq $mod -or $seen["$p@$v"]) { continue }
            $seen["$p@$v"] = $true
            $deps.Add([pscustomobject]@{ Path = $p; Version = $v })
        }
        $env:GOOS = 'windows'

        # --- third-party licenses: Go --------------------------------------------------------
        $tp = Join-Path $stage 'THIRD-PARTY'
        $index = [Collections.Generic.List[string]]::new()
        $index.Add('Third-party components shipped in this archive, and their license texts.')
        $index.Add('')
        $index.Add('== Go: linked into winbox.exe (`go version -m`) or into the guest binaries')
        $index.Add('   (guest/cmd/*, `go list -deps` for linux)')
        $index.Add('')
        $std = Join-Path $tp 'go\std'
        New-Item -ItemType Directory -Force $std | Out-Null
        foreach ($f in 'LICENSE', 'PATENTS') { Copy-Item (Join-Path $goroot $f) $std }
        $index.Add("Go standard library and runtime  $($goVersion -replace '^go version ', '')  go/std/LICENSE")
        $licRe = '^(LICEN[CS]E|COPYING|NOTICE|PATENTS|AUTHORS)([.-].*)?$'
        foreach ($d in $deps) {
            $j = (Exec go mod download -json "$($d.Path)@$($d.Version)") -join "`n" | ConvertFrom-Json
            if (-not $j.Dir) { Fail "no module directory for $($d.Path)@$($d.Version)" }
            $files = @(Get-ChildItem -LiteralPath $j.Dir -File | Where-Object { $_.Name -match $licRe } | Sort-Object Name)
            if (-not $files.Count) { Fail "no license file in $($d.Path)@$($d.Version)" }
            $dst = Join-Path $tp ('go\' + ($d.Path -replace '/', '\'))
            New-Item -ItemType Directory -Force $dst | Out-Null
            foreach ($f in $files) { Copy-Item -LiteralPath $f.FullName (Join-Path $dst $f.Name) }
            $index.Add("$($d.Path)  $($d.Version)  " + (($files | ForEach-Object { "go/$($d.Path)/$($_.Name)" }) -join ' '))
        }
    } finally {
        $env:GOOS, $env:GOARCH, $env:CGO_ENABLED, $env:GOTOOLCHAIN, $env:GOFLAGS = $saved
    }

    # --- guest image ------------------------------------------------------------------------
    $imgSums = @{}
    $sumsFile = Join-Path $ImageDir 'SHA256SUMS'
    if (Test-Path $sumsFile) {
        foreach ($l in Get-Content $sumsFile) {
            if ($l -match '^([0-9a-f]{64}) [ *](.+)$') { $imgSums[$Matches[2]] = $Matches[1] }
        }
    } else { Warn "$sumsFile is missing; the image files cannot be checked against the build" }
    $imageLines = @()
    foreach ($f in $ImageFiles) {
        if ($f -notmatch '^[A-Za-z0-9][A-Za-z0-9._-]*$') { Fail "bad image file name: $f" }
        $src = Join-Path $ImageDir $f
        if (-not (Test-Path -LiteralPath $src -PathType Leaf)) { Fail "guest image file $src not found" }
        $h = Sha256 $src
        if ($imgSums.Count -and $imgSums[$f] -ne $h) { Fail "$f does not match the image SHA256SUMS" }
        Copy-Item -LiteralPath $src (Join-Path $stage $f)
        $imageLines += "  $h  $f"
    }

    $index.Add('')
    $index.Add('== In the guest image (vmlinuz-virt and the initramfs)')
    $index.Add('')
    $inputs = Join-Path $ImageDir 'guest-inputs.tsv'
    $gpl = @()
    if (Test-Path $inputs) {
        $index.Add('name  package  license  (signature checked by apk verify, or sha256 pin only)')
        foreach ($l in Get-Content $inputs) {
            $c = $l -split "`t"
            if ($c.Count -lt 10) { continue }
            $pkg = if ($c[3] -eq 'apk') { "$($c[4])-$($c[5])" } else { $c[2] }
            $how = if ($c[3] -eq 'apk') { 'signed' } else { 'pin only' }
            # A release ships only signed Alpine packages, whose source and license texts
            # the image job can collect (the Docker static tarball has neither).
            if ($c[3] -ne 'apk') { Warn "guest input $($c[0]) ($($c[2])) is not a signed Alpine package" }
            $index.Add("$($c[0])  $pkg  $($c[9])  ($how)")
            if ($c[9] -match 'GPL') { $gpl += "  $pkg ($($c[9]))" }
        }
    } else { Warn "$inputs is missing; the guest components cannot be listed" }
    $index.Add('Guest binaries built from this repository (guest/cmd/*): Apache-2.0, plus the Go modules listed above.')
    $guestLic = Join-Path $ImageDir 'licenses'
    if (Test-Path $guestLic) {
        Copy-Item -Recurse $guestLic (Join-Path $tp 'guest')
        $index.Add('License texts from the upstream sources: guest/<origin>/...')
    } else { Warn "no guest license texts in $guestLic (the image build runs with with-sources)" }
    WriteText (Join-Path $tp 'README.txt') $index

    # --- GPL corresponding source ---------------------------------------------------------------
    $offer = @(
        'Corresponding source for the GPL and LGPL components of the guest image',
        '',
        'The guest image in this archive contains, among others:'
    ) + $gpl + @(
        '',
        'Their complete corresponding source (Alpine aports directories at the commits the',
        'packages were built from, and the upstream source archives) is published next to',
        'this archive, in the same GitHub release, as:',
        ''
    )
    if ($SourceBundle) {
        $sb = (Resolve-Path $SourceBundle).Path
        $sbName = "winbox-$Version-guest-sources.tar"
        $offer += "  $sbName"
        $offer += "  sha256 $(Sha256 $sb)"
        if ($ReleaseUrl) { $offer += "  $($ReleaseUrl.TrimEnd('/'))/$sbName" }
        $offer += @('', 'It stays available for as long as this release is offered.')
    } else {
        Warn 'no -SourceBundle; a release must publish the guest source bundle'
        $offer += '  SNAPSHOT BUILD: no source bundle was produced. Do not redistribute this archive.'
    }
    $offer += @('', 'winbox itself is Apache-2.0; its source is https://github.com/Higangssh/winbox at', "commit $commit.")
    WriteText (Join-Path $stage 'SOURCE-OFFER.txt') $offer

    # --- licenses, readme, build info -----------------------------------------------------------
    Copy-Item (Join-Path $repo 'LICENSE') $stage
    Copy-Item (Join-Path $repo 'NOTICE') $stage
    WriteText (Join-Path $stage 'README.txt') @(
        "winbox $Version for Windows x64",
        '',
        'A Docker-compatible container runtime for Windows that does not use WSL.',
        '',
        'Quick start (no installer, no admin rights for these steps):',
        '  1. Keep all files of this archive in one folder, for example %LOCALAPPDATA%\Programs\winbox.',
        '  2. Add that folder to your user PATH.',
        '  3. winbox start',
        '  4. docker run --rm hello-world   (needs the docker CLI)',
        '  5. winbox stop',
        '',
        'Prerequisites (Virtual Machine Platform, Hyper-V Administrators membership) and the',
        'current status: https://github.com/Higangssh/winbox#quick-start',
        '',
        'LICENSE, NOTICE, THIRD-PARTY/ and SOURCE-OFFER.txt describe the licenses of',
        'everything in this archive. SHA256SUMS lists every file.'
    )
    $bi = @(
        "version: $Version",
        "commit: $commit",
        "snapshot: $([bool]$Snapshot)",
        "go: $goVersion",
        'build: GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -buildvcs=true -ldflags "-s -w -buildid=" ./cmd/winbox',
        "winbox.exe: $(Sha256 $exe)",
        'guest image:'
    ) + $imageLines
    $gbi = Join-Path $ImageDir 'BUILDINFO.txt'
    if (Test-Path $gbi) { $bi += 'guest image build:'; $bi += (Get-Content $gbi | ForEach-Object { "  $_" }) }
    WriteText (Join-Path $stage 'BUILDINFO.txt') $bi

    # --- SHA256SUMS and the zip --------------------------------------------------------------------
    $items = [object[]]@(Get-ChildItem -LiteralPath $stage -Recurse -File | ForEach-Object {
            [pscustomobject]@{ Rel = $_.FullName.Substring($stage.Length + 1).Replace('\', '/'); Full = $_.FullName }
        })
    # Ordinal order (Sort-Object is culture-aware), so the order is the same everywhere.
    $keys = [string[]]@($items | ForEach-Object { $_.Rel })
    [Array]::Sort($keys, $items, [StringComparer]::Ordinal)
    $all = @($items)
    WriteText (Join-Path $stage 'SHA256SUMS') @($all | ForEach-Object { "$(Sha256 $_.Full)  $($_.Rel)" })
    $all += [pscustomobject]@{ Rel = 'SHA256SUMS'; Full = (Join-Path $stage 'SHA256SUMS') }

    $zip = Join-Path $Out "$name.zip"
    if (Test-Path $zip) { Remove-Item -Force $zip }
    $stamp = [DateTimeOffset]::FromUnixTimeSeconds([Math]::Max($commitTime, 315532800)) # zip epoch is 1980
    $fs = [IO.File]::Open($zip, [IO.FileMode]::CreateNew)
    try {
        $za = [IO.Compression.ZipArchive]::new($fs, [IO.Compression.ZipArchiveMode]::Create)
        try {
            foreach ($e in $all) {
                # Already-compressed files are stored, everything else deflated.
                $level = if ($e.Rel -match '\.(gz|xz|zst|erofs)$') { [IO.Compression.CompressionLevel]::NoCompression } else { [IO.Compression.CompressionLevel]::Optimal }
                $entry = $za.CreateEntry("$name/$($e.Rel)", $level)
                $entry.LastWriteTime = $stamp
                $w = $entry.Open()
                try {
                    $r = [IO.File]::OpenRead($e.Full)
                    try { $r.CopyTo($w) } finally { $r.Dispose() }
                } finally { $w.Dispose() }
            }
        } finally { $za.Dispose() }
    } finally { $fs.Dispose() }

    # --- REQ-UX-002 ---------------------------------------------------------------------------
    $zipBytes = (Get-Item $zip).Length
    $rawBytes = ($all | ForEach-Object { (Get-Item -LiteralPath $_.Full).Length } | Measure-Object -Sum).Sum
    $sizeLine = '{0}: {1:N1} MB zipped ({2:N0} bytes), {3:N1} MB unpacked; REQ-UX-002 limit {4:N0} MB' -f `
        "$name.zip", ($zipBytes / 1MB), $zipBytes, ($rawBytes / 1MB), ($MaxBytes / 1MB)
    Write-Host $sizeLine
    Get-ChildItem -LiteralPath $stage -File | Sort-Object Length -Descending |
        ForEach-Object { Write-Host ('  {0,12:N0}  {1}' -f $_.Length, $_.Name) }
    if ($zipBytes -gt $MaxBytes) { Fail "the zip is over REQ-UX-002's limit: $zipBytes > $MaxBytes bytes" }

    if ($env:GITHUB_OUTPUT) {
        Add-Content -Encoding utf8 -LiteralPath $env:GITHUB_OUTPUT -Value @(
            "zip=$zip", "name=$name", "version=$Version", "sha256=$(Sha256 $zip)", "bytes=$zipBytes")
    }
    if ($env:GITHUB_STEP_SUMMARY) { Add-Content -Encoding utf8 -LiteralPath $env:GITHUB_STEP_SUMMARY -Value "### Package`n`n$sizeLine" }
} finally {
    Pop-Location
}
