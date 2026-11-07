# Assembles the portable hyperbutler release zip. No installer and no admin: unzip, add the
# folder to PATH, run `hyperbutler start`.
#
#   pwsh -File dist\package.ps1 -ImageDir DIR [-SourceBundle FILE] [-ReleaseUrl URL]
#        [-ImageFiles a,b] [-Version vX.Y.Z] [-Out DIR] [-NoClients] [-Snapshot]
#
# -ImageDir is the guest-image artifact (.github/workflows/guest-image.yml): the image
# files, SHA256SUMS, guest-inputs.tsv, BUILDINFO.txt and licenses/. -SourceBundle is the
# guest-sources.tar from the same run; it is published as a separate release asset, and
# SOURCE-OFFER.txt in the zip names it by file name and SHA-256.
#
# The image files go into image\ in the zip, not next to hyperbutler.exe: Explorer shows
# system.vhd as a disk image there, and a double-click offers to mount it. hyperbutler.exe
# looks for its image in the image\ folder next to it.
#
# The SHA-256 of image.json (as copied into the zip) is linked into hyperbutler.exe
# (-X ...imageset.pinnedManifest), so the release binary boots only that image (ADR 0005,
# Decision 4).
#
# The docker CLI and the compose and buildx plugins (clients\ in the zip) are built here
# from the source modules pinned in dist/clients.lock: fetched through the Go module proxy,
# checked against sum.golang.org and against the lock's own h1: hash, and built with
# their upstream build tags and version flags, CGO_ENABLED=0, -trimpath. Their
# dependencies are checked against the source's own go.sum. -NoClients leaves them out
# (-Snapshot only).
#
# Without -Snapshot (the release workflow), every check is fatal: a clean tree, a tag
# version that hyperbutler.exe reports itself, image hashes that match the artifact's
# SHA256SUMS, an image.json to pin, guest license texts, a source bundle, the bundled
# clients, and REQ-UX-002's 150 MB limit. -Snapshot relaxes all but the last for local
# dry runs and marks the zip as not for redistribution; its name ends in -snapshot, so
# no -Version makes it look like a release. The client source pins and the size limit
# always apply, and so do the module proxy and checksum database (EnterPinnedGoEnv).
#
# Reproducibility: CGO_ENABLED=0, -trimpath, -buildvcs=true (the version comes from the
# git tag, as `git describe` sees it), -ldflags "-s -w -buildid= -X <image pin>",
# GOTOOLCHAIN=local, and zip entries in ordinal order with the commit time as their
# timestamp. The same commit, Go version and guest image give the same hyperbutler.exe;
# the zip also depends on the .NET deflate implementation, so compare hyperbutler.exe,
# not the zip, across machines.

[CmdletBinding()]
param(
    [Parameter(Mandatory)] [string]$ImageDir,
    # The ADR 0005 A2b image: hyperbutler.exe boots it from image\ next to it.
    [string[]]$ImageFiles = @('vmlinuz-virt', 'initramfs-stage1.gz', 'system.vhd', 'image.json'),
    [string]$SourceBundle = '',
    [string]$ReleaseUrl = '',
    [string]$Version = '',
    [string]$Out = '',
    [string]$ClientLock = '',   # default: clients.lock next to this script
    [switch]$NoClients,
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
# The architecture of hyperbutler.exe and of the WebView2Loader.dll next to it. Releases
# are windows/amd64 so far; a build for another one must take that one's loader, so the
# loader step refuses an architecture it has no pinned loader for.
$goarch = 'amd64'
Add-Type -AssemblyName System.IO.Compression.FileSystem
function Sha256([string]$path) { (Get-FileHash -Algorithm SHA256 -LiteralPath $path).Hash.ToLowerInvariant() }
# What go fetches from the network (the client sources and their dependencies, and
# go-winres) comes through proxy.golang.org and is checked against sum.golang.org,
# whatever the caller's environment or go env file says: GOENV=off ignores the file,
# and every variable that would skip the proxy or the checksum database is cleared.
$pinnedGoEnv = [ordered]@{
    GOENV = 'off'; GOPROXY = 'https://proxy.golang.org'; GOSUMDB = 'sum.golang.org'
    GONOSUMDB = $null; GONOSUMCHECK = $null; GONOPROXY = $null; GOPRIVATE = $null; GOINSECURE = $null; GOFLAGS = $null
}
function EnterPinnedGoEnv {
    $saved = @{}
    foreach ($k in $pinnedGoEnv.Keys) {
        $saved[$k] = [Environment]::GetEnvironmentVariable($k)
        [Environment]::SetEnvironmentVariable($k, $pinnedGoEnv[$k])
    }
    $saved
}
function ExitPinnedGoEnv([hashtable]$saved) {
    foreach ($k in $saved.Keys) { [Environment]::SetEnvironmentVariable($k, $saved[$k]) }
}
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
    # A snapshot never carries a release's name, even with -Version vX.Y.Z.
    $name = "hyperbutler-$Version-windows-amd64"
    if ($Snapshot) { $name += '-snapshot' }
    $stage = Join-Path $Out $name
    if (Test-Path $stage) { Remove-Item -Recurse -Force $stage }
    New-Item -ItemType Directory -Force $stage | Out-Null
    Write-Host "packaging $name from $commit"

    # --- guest image ------------------------------------------------------------------------
    # Copied first and checked as copied: the zip carries these bytes, and hyperbutler.exe
    # pins the image.json among them.
    $imgSums = @{}
    $sumsFile = Join-Path $ImageDir 'SHA256SUMS'
    if (Test-Path $sumsFile) {
        foreach ($l in Get-Content $sumsFile) {
            if ($l -match '^([0-9a-f]{64}) [ *](.+)$') { $imgSums[$Matches[2]] = $Matches[1] }
        }
    } else { Warn "$sumsFile is missing; the image files cannot be checked against the build" }
    $imageLines = @()
    $pin = ''
    # The folder hyperbutler.exe looks in (imageDirName in cmd/hyperbutler).
    $imageSub = 'image'
    $imageStage = Join-Path $stage $imageSub
    New-Item -ItemType Directory -Force $imageStage | Out-Null
    foreach ($f in $ImageFiles) {
        if ($f -notmatch '^[A-Za-z0-9][A-Za-z0-9._-]*$') { Fail "bad image file name: $f" }
        $src = Join-Path $ImageDir $f
        if (-not (Test-Path -LiteralPath $src -PathType Leaf)) { Fail "guest image file $src not found" }
        $dst = Join-Path $imageStage $f
        Copy-Item -LiteralPath $src $dst
        $h = Sha256 $dst
        if ($imgSums.Count -and $imgSums[$f] -ne $h) { Fail "$f does not match the image SHA256SUMS" }
        if ($f -eq 'image.json') { $pin = $h }
        $imageLines += "  $h  $imageSub/$f"
    }
    if (-not $pin) { Warn 'no image.json among -ImageFiles; hyperbutler.exe is not pinned to a guest image' }

    # --- hyperbutler.exe -------------------------------------------------------------------------
    $exe = Join-Path $stage 'hyperbutler.exe'
    # The icon and version resource (cmd/hyperbutler/winres), stamped with this version. The
    # .syso files are git-ignored, so generating them leaves the tree clean for -buildvcs.
    # The numeric file version is X.Y.Z.0 of a vX.Y.Z version, else 0.0.0.0: go-winres reads
    # the leading digits of any string, so a bare commit (26a483c) became 26.0.0.0, which
    # Windows Installer takes as newer than a release's hyperbutler.exe and then skips the
    # release's copy while the upgrade removes the old one (docs/install.md).
    $resVersion = $Version.TrimStart('v')
    $fileVersion = '0.0.0.0'
    if ($Version -match '^v(\d+)\.(\d+)\.(\d+)(?:[-+]|$)') { $fileVersion = "$($Matches[1]).$($Matches[2]).$($Matches[3]).0" }
    $goSaved = EnterPinnedGoEnv
    try {
        Exec go run github.com/tc-hib/go-winres@v0.3.3 make --in cmd/hyperbutler/winres/winres.json `
            --out cmd/hyperbutler/rsrc --arch 'amd64,arm64' --product-version $resVersion --file-version $fileVersion | Out-Null
    } finally { ExitPinnedGoEnv $goSaved }
    $saved = $env:GOOS, $env:GOARCH, $env:CGO_ENABLED, $env:GOTOOLCHAIN, $env:GOFLAGS
    try {
        $env:GOOS = 'windows'; $env:GOARCH = $goarch; $env:CGO_ENABLED = '0'
        $env:GOTOOLCHAIN = 'local'; $env:GOFLAGS = '-mod=readonly'
        $ldflags = '-s -w -buildid='
        if ($pin) { $ldflags += " -X github.com/Higangssh/hyperbutler/internal/imageset.pinnedManifest=$pin" }
        Exec go build -trimpath -buildvcs=true -ldflags $ldflags -o $exe ./cmd/hyperbutler | Out-Null
        $goVersion = (Exec go version) | Select-Object -First 1
        # The pin as the binary reports it: a renamed variable would make -X a silent no-op.
        if ($env:PROCESSOR_ARCHITECTURE -eq 'AMD64') {
            $vj = (Exec $exe version --json) -join "`n" | ConvertFrom-Json
            if ([bool]$vj.imagePinned -ne [bool]$pin -or ($pin -and $vj.imageManifestSha256 -ne $pin)) {
                Fail "hyperbutler.exe reports image pin '$($vj.imageManifestSha256)', expected '$pin'"
            }
        } else { Warn "cannot run the amd64 hyperbutler.exe here to check its image pin" }

        # The HCS access broker (ADR 0004), pinned to the same image.json and stamped with
        # this version, so it boots the image\ folder next to it, which is hyperbutler.exe's
        # too. The MSI installs it as the service (dist/msi, `hyperbutler setup broker
        # install`); in the zip it is there for `hyperbutler-broker install` by hand.
        $brokerExe = Join-Path $stage 'hyperbutler-broker.exe'
        $brokerLdflags = "$ldflags -X main.version=$Version"
        Exec go build -trimpath -buildvcs=true -ldflags $brokerLdflags -o $brokerExe ./cmd/hyperbutler-broker | Out-Null
        if ($env:PROCESSOR_ARCHITECTURE -eq 'AMD64') {
            $bj = (Exec $brokerExe -version-json) -join "`n" | ConvertFrom-Json
            if ([bool]$bj.imagePinned -ne [bool]$pin -or ($pin -and $bj.imageManifestSha256 -ne $pin)) {
                Fail "hyperbutler-broker.exe reports image pin '$($bj.imageManifestSha256)', expected '$pin'"
            }
            if ($bj.version -ne $Version) { Fail "hyperbutler-broker.exe reports version '$($bj.version)', expected '$Version'" }
        } else { Warn "cannot run the amd64 hyperbutler-broker.exe here to check its image pin" }
        $goroot = (Exec go env GOROOT) | Select-Object -First 1

        # What the binary says about itself: its own version and the modules linked in.
        $info = @(Exec go version -m $exe)
        $modVersion = ''
        $deps = [Collections.Generic.List[object]]::new()
        foreach ($l in $info) {
            $f = $l.Trim() -split "`t"
            if ($f[0] -eq 'mod' -and $f.Count -ge 3) { $modVersion = $f[2] }
            elseif ($f[0] -eq 'dep' -and $f.Count -ge 3) { $deps.Add([pscustomobject]@{ Path = $f[1]; Version = $f[2] }) }
            elseif ($f[0] -eq '=>') { Fail "replaced module in hyperbutler.exe: $l" }
        }
        if ($modVersion -ne $Version) {
            Warn "hyperbutler.exe reports version '$modVersion', expected '$Version' (is HEAD tagged and the tree clean?)"
        }
        # The guest binaries (guest/cmd/*, GOOS=linux) are Go too and ship inside the
        # initramfs. Add the modules they link that hyperbutler.exe does not.
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
        $index.Add('== Go: linked into hyperbutler.exe (`go version -m`) or into the guest binaries')
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

        # --- WebView2Loader.dll, for the window (hyperbutler gui, ADR 0008) ------------------
        # Microsoft's WebView2 SDK loader (BSD-3-Clause), next to hyperbutler.exe: the GUI
        # loads it from there by full path. It comes from the Microsoft.Web.WebView2 NuGet
        # package, downloaded from nuget.org at a pinned version and checked against the
        # package's pinned SHA-256; the loader for the build's architecture must match its
        # own pin and carry a valid Microsoft signature, and the license is the package's.
        # (Not from github.com/jchv/go-webview2, which hyperbutler.exe no longer links: its
        # copy of the SDK was a 2021 loader without ICoreWebView2EnvironmentOptions3, the
        # interface that keeps crash dumps local.)
        $wv2Ver = '1.0.4258.31'
        $wv2Url = "https://api.nuget.org/v3-flatcontainer/microsoft.web.webview2/$wv2Ver/microsoft.web.webview2.$wv2Ver.nupkg"
        $wv2Pin = '56f7f4b8bf9aee4b8efefbbdd4f67d5f74ebd1b100ed0806da71bf76af481aa9'
        $loaders = @{   # GOARCH -> the package's loader and its SHA-256
            amd64 = @('build/native/x64/WebView2Loader.dll', '3426dcc55fdfb8b5e7ac623cf33b4ccf283fbf68a0c5a65bddfdb4d749a8abee')
            arm64 = @('build/native/arm64/WebView2Loader.dll', '4f48cb32d34298c86cca2108057eea0d900b5e422575e926e8086318a0b9cbf0')
        }
        $wv2LicPin = '0af8f1b807512aae39c2ac1aa4d0cae65cabecb6fd554b8439a5162a0d6eca55'
        if (-not $loaders.ContainsKey($goarch)) { Fail "no WebView2Loader.dll is pinned for GOARCH=$goarch (only $($loaders.Keys -join ', '))" }
        $wv2Dir = Join-Path $Out 'webview2-sdk'
        if (Test-Path $wv2Dir) { Remove-Item -Recurse -Force $wv2Dir }
        New-Item -ItemType Directory $wv2Dir | Out-Null
        $nupkg = Join-Path $wv2Dir "microsoft.web.webview2.$wv2Ver.nupkg"
        Invoke-WebRequest -UseBasicParsing -Uri $wv2Url -OutFile $nupkg
        if ((Sha256 $nupkg) -ne $wv2Pin) { Fail "the Microsoft.Web.WebView2 $wv2Ver package does not match its pin" }
        $zip = [IO.Compression.ZipFile]::OpenRead($nupkg)
        try {
            foreach ($e in @($loaders[$goarch][0], 'LICENSE.txt')) {
                $entry = $zip.GetEntry($e)
                if (-not $entry) { Fail "the Microsoft.Web.WebView2 package has no $e" }
                [IO.Compression.ZipFileExtensions]::ExtractToFile($entry, (Join-Path $wv2Dir (Split-Path -Leaf $e)), $true)
            }
        } finally { $zip.Dispose() }
        $loaderSrc = Join-Path $wv2Dir 'WebView2Loader.dll'
        $loaderPin = $loaders[$goarch][1]
        if ((Sha256 $loaderSrc) -ne $loaderPin) { Fail "WebView2Loader.dll ($goarch) in Microsoft.Web.WebView2 $wv2Ver does not match its pin" }
        if ((Sha256 (Join-Path $wv2Dir 'LICENSE.txt')) -ne $wv2LicPin) { Fail "the WebView2 SDK license changed; review it and update the pin" }
        $sig = Get-AuthenticodeSignature -LiteralPath $loaderSrc
        if ($sig.Status -ne 'Valid' -or $sig.SignerCertificate.Subject -notmatch '^CN=Microsoft Corporation,') {
            Fail "WebView2Loader.dll is not validly signed by Microsoft ($($sig.Status))"
        }
        Copy-Item -LiteralPath $loaderSrc (Join-Path $stage 'WebView2Loader.dll')
        $wv2Lic = Join-Path $tp 'webview2'
        New-Item -ItemType Directory -Force $wv2Lic | Out-Null
        Copy-Item -LiteralPath (Join-Path $wv2Dir 'LICENSE.txt') (Join-Path $wv2Lic 'LICENSE.txt')
        $index.Add('')
        $index.Add("== WebView2Loader.dll: Microsoft Edge WebView2 SDK loader, $goarch, Microsoft-signed")
        $index.Add("Microsoft.Web.WebView2 $wv2Ver loader  (nuget.org package sha256 $wv2Pin; dll sha256 $loaderPin)  webview2/LICENSE.txt (BSD-3-Clause)")
    } finally {
        $env:GOOS, $env:GOARCH, $env:CGO_ENABLED, $env:GOTOOLCHAIN, $env:GOFLAGS = $saved
    }

    # --- Windows clients: docker CLI, compose and buildx plugins, built from source -------
    # In clients\, not next to hyperbutler.exe: the folder the README puts on PATH holds no
    # docker.exe, so a docker CLI installed later is never shadowed by this one.
    $clientLines = @()
    if ($NoClients) {
        Warn '-NoClients: the zip has no docker CLI'
    } else {
        if (-not $ClientLock) { $ClientLock = Join-Path $dist 'clients.lock' }
        $want = @{
            'docker-cli'     = 'clients/docker.exe'
            'docker-compose' = 'clients/cli-plugins/docker-compose.exe'
            'docker-buildx'  = 'clients/cli-plugins/docker-buildx.exe'
        }
        $entries = @()
        foreach ($l in Get-Content $ClientLock) {
            if ($l -match '^\s*(#|$)') { continue }
            $c = -split $l
            if ($c.Count -ne 5 -or -not $want.ContainsKey($c[0]) -or $c[2] -notmatch '^h1:[A-Za-z0-9+/]{43}=$' -or
                $c[3] -notmatch '^[0-9a-f]{40}$' -or $c[4] -notmatch '^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$') { Fail "bad line in ${ClientLock}: $l" }
            if ($c[1] -notmatch '^([a-z0-9.-]+(/[A-Za-z0-9._~-]+)+)@(v[0-9][0-9A-Za-z.+-]*)$') { Fail "bad source module in ${ClientLock}: $l" }
            $entries += [pscustomobject]@{ Name = $c[0]; Mod = $Matches[1]; ModVersion = $Matches[3]; Sum = $c[2]; Commit = $c[3]; Time = $c[4] }
        }
        foreach ($k in $want.Keys) { if (@($entries | Where-Object Name -EQ $k).Count -ne 1) { Fail "$ClientLock must pin $k exactly once" } }

        # A fresh work directory outside the stage, and outside this repository's module,
        # so its go.mod and go.sum stay untouched.
        $work = Join-Path $Out 'clients'
        if (Test-Path $work) { Remove-Item -Recurse -Force $work }
        New-Item -ItemType Directory $work | Out-Null
        $modTmp = Join-Path $work 'gomod'
        New-Item -ItemType Directory $modTmp | Out-Null
        # go mod download -json prints one object per module; each ends with a "}" line.
        function ModDirs([string[]]$mods) {
            $dirs = @{}
            for ($i = 0; $i -lt $mods.Count; $i += 100) {
                $chunk = $mods[$i..([Math]::Min($i + 99, $mods.Count - 1))]
                $buf = [Collections.Generic.List[string]]::new()
                foreach ($l in (Exec go mod download -json @chunk)) {
                    $buf.Add($l)
                    if ($l -eq '}') {
                        $j = ($buf -join "`n") | ConvertFrom-Json
                        $buf.Clear()
                        if ($j.PSObject.Properties['Error'] -or -not $j.PSObject.Properties['Dir']) {
                            Fail "go mod download $($j.Path)@$($j.Version) failed"
                        }
                        $dirs["$($j.Path)@$($j.Version)"] = $j
                    }
                }
            }
            $dirs
        }
        $savedEnv = $env:GOOS, $env:GOARCH, $env:CGO_ENABLED, $env:GOTOOLCHAIN, $env:GOFLAGS
        $index.Add('')
        $index.Add('== Windows clients in clients/: docker.exe, cli-plugins/docker-compose.exe and')
        $index.Add('   cli-plugins/docker-buildx.exe, built from the source modules in dist/clients.lock')
        $index.Add('')
        $goSaved = EnterPinnedGoEnv
        try {
            foreach ($e in $entries | Sort-Object Name) {
                $env:GOOS = $null; $env:GOARCH = $null; $env:CGO_ENABLED = $null
                $env:GOTOOLCHAIN = 'local'; $env:GOFLAGS = ''
                # The source, through the module proxy, checked by go against sum.golang.org
                # and here against the lock.
                Push-Location $modTmp
                try { $src = (ModDirs @("$($e.Mod)@$($e.ModVersion)")).Values | Select-Object -First 1 } finally { Pop-Location }
                if ($src.Sum -cne $e.Sum) { Fail "$($e.Mod)@$($e.ModVersion) has hash $($src.Sum), $ClientLock pins $($e.Sum)" }
                if ($src.PSObject.Properties['Origin'] -and $src.Origin.PSObject.Properties['Hash'] -and $src.Origin.Hash -ne $e.Commit) {
                    Fail "$($e.Mod)@$($e.ModVersion) is commit $($src.Origin.Hash), $ClientLock says $($e.Commit)"
                }
                $tree = Join-Path $work $e.Name
                Copy-Item -Recurse -LiteralPath $src.Dir $tree
                Get-ChildItem -LiteralPath $tree -Recurse -Force | ForEach-Object { if ($_.Attributes -band [IO.FileAttributes]::ReadOnly) { $_.Attributes = $_.Attributes -bxor [IO.FileAttributes]::ReadOnly } }

                $dst = Join-Path $stage ($want[$e.Name] -replace '/', '\')
                New-Item -ItemType Directory -Force (Split-Path -Parent $dst) | Out-Null
                $short = $e.Commit.Substring(0, 7)
                if ($e.Name -eq 'docker-cli') {
                    # docker/cli has no go.mod: its scripts use vendor.mod and vendor.sum as
                    # go.mod and go.sum. The module zip has no vendored code, so the
                    # dependencies come through the proxy, checked against vendor.sum.
                    Remove-Item -Recurse -Force (Join-Path $tree 'vendor')
                    Copy-Item (Join-Path $tree 'vendor.mod') (Join-Path $tree 'go.mod')
                    Copy-Item (Join-Path $tree 'vendor.sum') (Join-Path $tree 'go.sum')
                    $v = $e.ModVersion.TrimStart('v') -replace '\+incompatible$', ''
                    # scripts/build/.variables: these -X flags and grpcnotrace. Upstream's
                    # Windows build uses cgo only for the osusergo tag it implies.
                    $tags = 'grpcnotrace'
                    $ld = "-s -w -buildid= -X github.com/docker/cli/cli/version.GitCommit=$short " +
                        "-X github.com/docker/cli/cli/version.BuildTime=$($e.Time) -X github.com/docker/cli/cli/version.Version=$v"
                    $pkg = './cmd/docker'
                } elseif ($e.Name -eq 'docker-buildx') {
                    # Dockerfile, target buildx-build: CGO_ENABLED=0, no tags, and these -X
                    # flags, with the tag as the version and the whole commit as the revision.
                    $tags = ''
                    $ld = "-s -w -buildid= -X github.com/docker/buildx/version.Version=$($e.ModVersion) " +
                        "-X github.com/docker/buildx/version.Revision=$($e.Commit) -X github.com/docker/buildx/version.Package=github.com/docker/buildx"
                    $pkg = './cmd/buildx'
                } else {
                    # Makefile and Dockerfile: CGO_ENABLED=0, the e2e tag, and the version flag.
                    $tags = 'e2e'
                    $ld = "-s -w -buildid= -X github.com/docker/compose/v5/internal.Version=$($e.ModVersion)"
                    $pkg = './cmd'
                }
                $env:GOOS = 'windows'; $env:GOARCH = 'amd64'; $env:CGO_ENABLED = '0'; $env:GOFLAGS = '-mod=readonly'
                Push-Location $tree
                try {
                    Write-Host "building $($want[$e.Name]) from $($e.Mod)@$($e.ModVersion)"
                    # Windows PowerShell drops an empty argument, so no tags means no -tags.
                    $tagArgs = if ($tags) { @('-tags', $tags) } else { @() }
                    Exec go build -trimpath -buildvcs=false @tagArgs -ldflags $ld -o $dst $pkg | Out-Null
                } finally { Pop-Location }
                $env:GOFLAGS = ''

                # License texts: the source module's and those of every module linked in.
                $deps = [Collections.Generic.List[string]]::new()
                foreach ($l in @(Exec go version -m $dst)) {
                    $p = $l.Trim() -split "`t"
                    if ($p[0] -eq 'dep' -and $p.Count -ge 3) { $deps.Add("$($p[1])@$($p[2])") }
                    elseif ($p[0] -eq '=>') {
                        if ($p.Count -lt 3 -or $p[2] -notmatch '^v') { Fail "$($want[$e.Name]) has a local replacement: $l" }
                        $deps[$deps.Count - 1] = "$($p[1])@$($p[2])"
                    }
                }
                if ($deps.Count -lt 10) { Fail "$($want[$e.Name]) lists only $($deps.Count) modules" }
                Push-Location $modTmp
                try { $dirs = ModDirs @($deps | Sort-Object -Unique) } finally { Pop-Location }

                $base = Join-Path $tp ('clients\' + $e.Name)
                New-Item -ItemType Directory -Force $base | Out-Null
                $own = @(Get-ChildItem -LiteralPath $src.Dir -File | Where-Object { $_.Name -match $licRe } | Sort-Object Name)
                if (-not @($own | Where-Object Name -Like 'LICEN*').Count) { Fail "no LICENSE in $($e.Mod)@$($e.ModVersion)" }
                foreach ($lf in $own) { Copy-Item -LiteralPath $lf.FullName (Join-Path $base $lf.Name) }
                $index.Add("$($want[$e.Name])  $($e.Mod) $($e.ModVersion) (commit $($e.Commit))  " + (($own | ForEach-Object { "clients/$($e.Name)/$($_.Name)" }) -join ' '))
                $index.Add('  Go modules linked in (`go version -m`):')
                foreach ($k in $dirs.Keys | Sort-Object) {
                    $d = $dirs[$k]
                    $files = @(Get-ChildItem -LiteralPath $d.Dir -File | Where-Object { $_.Name -match $licRe } | Sort-Object Name)
                    if (-not $files.Count) { Fail "no license file in $k (linked into $($want[$e.Name]))" }
                    $mdst = Join-Path $base ('go\' + ($d.Path -replace '/', '\'))
                    New-Item -ItemType Directory -Force $mdst | Out-Null
                    foreach ($lf in $files) { Copy-Item -LiteralPath $lf.FullName (Join-Path $mdst $lf.Name) }
                    $index.Add("  $($d.Path)  $($d.Version)  " + (($files | ForEach-Object { "clients/$($e.Name)/go/$($d.Path)/$($_.Name)" }) -join ' '))
                }
                $clientLines += "  $(Sha256 $dst)  $($want[$e.Name])  ($($e.Mod)@$($e.ModVersion) $($e.Sum), commit $($e.Commit); $(if ($tags) { "-tags $tags " })-ldflags `"$ld`")"
            }
        } finally {
            ExitPinnedGoEnv $goSaved
            $env:GOOS, $env:GOARCH, $env:CGO_ENABLED, $env:GOTOOLCHAIN, $env:GOFLAGS = $savedEnv
        }
    }

    $index.Add('')
    $index.Add('== In the guest image (vmlinuz-virt, initramfs-stage1.gz and system.vhd)')
    $index.Add('')
    $inputs = Join-Path $ImageDir 'guest-inputs.tsv'
    $gpl = @()
    if (Test-Path $inputs) {
        $index.Add('name  package  license  (signature checked by apk verify, or sha256 pin only)')
        $n = 0
        foreach ($l in Get-Content $inputs) {
            $c = $l -split "`t"
            # A line that does not parse must not skip the signature check below.
            if ($c.Count -ne 10) { Fail "bad line in ${inputs}: $l" }
            $n++
            $pkg = if ($c[3] -eq 'apk') { "$($c[4])-$($c[5])" } else { $c[2] }
            $how = if ($c[3] -eq 'apk') { 'signed' } else { 'pin only' }
            # A release ships only signed Alpine packages, whose source and license texts
            # the image job can collect. kind is 'apk' only after apk verify passed
            # (fetch-verify.sh); anything else is a pin-only input.
            if ($c[3] -ne 'apk') { Warn "guest input $($c[0]) ($($c[2])) is not a signed Alpine package" }
            $index.Add("$($c[0])  $pkg  $($c[9])  ($how)")
            if ($c[9] -match 'GPL') { $gpl += "  $pkg ($($c[9]))" }
        }
        if ($n -eq 0) { Warn "$inputs lists no guest inputs" }
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
        $sbName = "hyperbutler-$Version-guest-sources.tar"
        $offer += "  $sbName"
        $offer += "  sha256 $(Sha256 $sb)"
        if ($ReleaseUrl) { $offer += "  $($ReleaseUrl.TrimEnd('/'))/$sbName" }
        $offer += @('', 'It stays available for as long as this release is offered.')
    } else {
        Warn 'no -SourceBundle; a release must publish the guest source bundle'
        $offer += '  SNAPSHOT BUILD: no source bundle was produced. Do not redistribute this archive.'
    }
    $offer += @('', 'hyperbutler itself is Apache-2.0; its source is https://github.com/Higangssh/hyperbutler at', "commit $commit.")
    WriteText (Join-Path $stage 'SOURCE-OFFER.txt') $offer

    # --- licenses, readme, build info -----------------------------------------------------------
    Copy-Item (Join-Path $repo 'LICENSE') $stage
    Copy-Item (Join-Path $repo 'NOTICE') $stage
    WriteText (Join-Path $stage 'README.txt') @(
        "hyperbutler $Version for Windows x64",
        '',
        'A Docker-compatible container runtime for Windows that does not use WSL.',
        '',
        'Quick start (no installer, no admin rights for these steps):',
        '  1. Extract the whole archive into one folder, for example %LOCALAPPDATA%\Programs\hyperbutler,',
        '     and keep its subfolders: hyperbutler.exe boots the guest image in image\ next to it.',
        '     Leave the files in image\ alone (Windows offers to mount system.vhd). To upgrade,',
        '     run hyperbutler stop and extract the new archive there, replacing every file.',
        '  2. Add that folder to your user PATH. If you have no docker CLI yet, add its',
        '     clients\ subfolder too: it holds docker.exe and, in cli-plugins\, docker-compose.exe',
        '     and docker-buildx.exe.',
        '  3. hyperbutler start',
        '  4. docker run --rm hello-world',
        '  5. hyperbutler stop',
        '',
        'A docker CLI you already have (Docker Desktop, another install) keeps working: the',
        'hyperbutler docker context works with any docker CLI from 19.03 on, and hyperbutler',
        'never changes PATH or copies the bundled CLI anywhere. `hyperbutler status` shows the',
        'docker.exe in use. hyperbutler.exe boots only the guest image files of this archive;',
        'if it refuses them, extract the archive again.',
        '',
        'Prerequisites (Virtual Machine Platform, Hyper-V Administrators membership) and the',
        'current status: https://github.com/Higangssh/hyperbutler#quick-start',
        '',
        'LICENSE, NOTICE, THIRD-PARTY/ and SOURCE-OFFER.txt describe the licenses of',
        'everything in this archive. SHA256SUMS lists every file.'
    )
    $bi = @(
        "version: $Version",
        "commit: $commit",
        "snapshot: $([bool]$Snapshot)",
        "go: $goVersion",
        "build: GOOS=windows GOARCH=$goarch CGO_ENABLED=0 go build -trimpath -buildvcs=true -ldflags `"$ldflags`" ./cmd/hyperbutler",
        "hyperbutler.exe: $(Sha256 $exe)",
        "broker build: GOOS=windows GOARCH=$goarch CGO_ENABLED=0 go build -trimpath -buildvcs=true -ldflags `"$brokerLdflags`" ./cmd/hyperbutler-broker",
        "hyperbutler-broker.exe: $(Sha256 $brokerExe)",
        "guest image pin (image.json sha256): $(if ($pin) { $pin } else { 'none' })",
        'guest image:'
    ) + $imageLines + @('docker CLI, compose and buildx plugins (built from source, dist/clients.lock):') + $(if ($clientLines.Count) { $clientLines } else { @('  none (-NoClients)') })
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
    # The largest files, in any folder (the image is in image\).
    $all | ForEach-Object { [pscustomobject]@{ Rel = $_.Rel; Length = (Get-Item -LiteralPath $_.Full).Length } } |
        Sort-Object Length -Descending | Select-Object -First 8 |
        ForEach-Object { Write-Host ('  {0,12:N0}  {1}' -f $_.Length, $_.Rel) }
    if ($zipBytes -gt $MaxBytes) { Fail "the zip is over REQ-UX-002's limit: $zipBytes > $MaxBytes bytes" }

    if ($env:GITHUB_OUTPUT) {
        Add-Content -Encoding utf8 -LiteralPath $env:GITHUB_OUTPUT -Value @(
            "zip=$zip", "name=$name", "version=$Version", "sha256=$(Sha256 $zip)", "bytes=$zipBytes")
    }
    if ($env:GITHUB_STEP_SUMMARY) { Add-Content -Encoding utf8 -LiteralPath $env:GITHUB_STEP_SUMMARY -Value "### Package`n`n$sizeLine" }
} finally {
    Pop-Location
}
