# bench/noadmin: cross-user isolation and a standard user's hyperbutler through the
# installed broker service (ADR 0004 rules 5 and 6, M4; ADR 0006 "What stays
# unverified"). See README.md. Two parts, as two accounts on one machine:
#
#   powershell -File bench\noadmin\run.ps1 -Role owner [-Dir C:\Users\Public\hb-noadmin]
#   runas /user:hbtest "powershell -NoProfile -ExecutionPolicy Bypass -NoExit -File C:\Users\Public\hb-noadmin\run.ps1 -Role intruder"
#
# -Role owner runs as the main user, whose hyperbutler runs through the broker service.
# It reads (through the hyperbutler CLI only) the engine's Docker API pipe, its control
# events, the VM's runtime id and the owner's per-user broker endpoints, writes them to
# owner.json in -Dir (a folder both users can read; names and ids only, nothing secret),
# copies this script and the noadmin helper there, and checks as a positive control
# that the owner can open what the second user must not:
#
#  1. hyperbutler runs, and its engine booted through the HCS access broker;
#  2. the owner has a per-user broker endpoint; owner.json is written;
#  3. the owner opens its Docker API pipe, its broker endpoint and its engine's control
#     events, and connects to its VM's Docker relay over AF_HYPERV.
#
# -Role intruder runs as a second, standard account (not an administrator, not in
# Hyper-V Administrators), started with runas so its password is typed by a person:
#
#  a. whoami /groups and the token: not in Administrators, not in Hyper-V
#     Administrators, not elevated, medium integrity;
#  b. opening the owner's Docker API pipe, its broker endpoint(s) and its engine's
#     control events is refused with access denied (a missing name is a FAIL, not a
#     pass), and adding an instance to the owner's pipes fails;
#  c. `docker -H npipe:////./pipe/<owner's pipe> version` fails with access denied;
#  d. AF_HYPERV connects to the owner's VM on 2375, 2380, 1024 and 2385 are refused
#     with access denied (WSAEACCES);
#  e. its own hyperbutler, with HYPERBUTLER_HOME and DOCKER_CONFIG under its own %TEMP%
#     and its own Docker API pipe, starts through the broker service (--backend broker),
#     `docker run --rm hello-world` passes (or a -Image loaded from -ImageTar, for a
#     machine without network), then stops; the test installation is uninstalled;
#  f. its doctor says hcs and hyperv-admins info, broker ok;
#  g. (b) and (d) again afterwards: still refused, and the owner's pipes still exist.
#
# Each part prints PASS/FAIL per check, writes <role>-results.json next to owner.json
# and exits 1 if any check failed. Nothing binds a network address; neither user's
# %LOCALAPPDATA%\hyperbutler or ~\.docker is written.

[CmdletBinding()]
param(
    [Parameter(Mandatory)][ValidateSet('owner', 'intruder')][string]$Role,
    [string]$Dir = 'C:\Users\Public\hb-noadmin',
    [string]$Hyperbutler = 'C:\Program Files\hyperbutler\hyperbutler.exe',
    [string]$Docker = '',
    [string]$Helper = '',
    [string]$DebugPipe = '',
    [string]$ImageTar = '',
    [string]$Image = '',
    [string]$Memory = '2GiB'
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$script:failed = 0
$script:results = New-Object System.Collections.Generic.List[object]

# Run a program and capture its exit code, stdout and stderr (without PowerShell's
# stderr wrapping). The child inherits this process's environment.
function Invoke-Tool([string]$exe, [string[]]$argv, [string]$cwd = (Get-Location).ProviderPath) {
    $psi = New-Object System.Diagnostics.ProcessStartInfo
    $psi.FileName = $exe
    $psi.Arguments = ($argv | ForEach-Object { if ($_ -match '[\s"]') { '"' + ($_ -replace '"', '\"') + '"' } else { $_ } }) -join ' '
    $psi.WorkingDirectory = $cwd
    $psi.UseShellExecute = $false
    $psi.RedirectStandardOutput = $true
    $psi.RedirectStandardError = $true
    $p = [System.Diagnostics.Process]::Start($psi)
    $out = $p.StandardOutput.ReadToEndAsync()
    $err = $p.StandardError.ReadToEndAsync()
    $p.WaitForExit()
    [pscustomobject]@{ Code = $p.ExitCode; Out = $out.Result; Err = $err.Result }
}

function Check([string]$name, [bool]$ok, [string]$detail = '') {
    $detail = ($detail -replace '\s+', ' ').Trim()
    if ($ok) { Write-Host "PASS  $name  $detail" } else { $script:failed++; Write-Host "FAIL  $name  $detail" }
    $script:results.Add([pscustomobject]@{ name = $name; ok = $ok; detail = $detail })
}

# Prop reads a property of a JSON object, $null if it has none (StrictMode-safe).
function Prop($obj, [string]$name) {
    if ($null -eq $obj) { return $null }
    $p = $obj.PSObject.Properties[$name]
    if ($null -eq $p) { return $null }
    $p.Value
}

# From-Json parses JSON, $null for empty or invalid text (a tool that failed early).
function From-Json([string]$text) {
    if ([string]::IsNullOrWhiteSpace($text)) { return $null }
    # Through a variable: Windows PowerShell's ConvertFrom-Json writes an array as one
    # object, and returning the variable enumerates it.
    try { $o = $text | ConvertFrom-Json } catch { return $null }
    $o
}

# Run the helper and turn its JSON report into checks, each prefixed with $label.
function Invoke-Helper([string]$label, [string[]]$argv) {
    $r = Invoke-Tool $script:helper $argv
    if ($r.Code -eq 2 -or [string]::IsNullOrWhiteSpace($r.Out)) {
        Check ("$label noadmin $($argv[0])".Trim()) $false "exit $($r.Code): $($r.Err)"
        return
    }
    foreach ($c in @(From-Json $r.Out)) {
        Check ("$label $($c.name)".Trim()) ([bool]$c.ok) $c.detail
    }
}

function Npipe-Host([string]$pipe) { 'npipe:////./pipe/' + ($pipe -replace '^\\\\\.\\pipe\\', '') }

function HB([string[]]$argv) { Invoke-Tool $Hyperbutler $argv }

function Write-Results {
    $path = Join-Path $Dir "$Role-results.json"
    $doc = [pscustomobject]@{
        role = $Role; user = [Security.Principal.WindowsIdentity]::GetCurrent().Name
        finished = (Get-Date).ToUniversalTime().ToString('o'); failed = $script:failed
        checks = $script:results.ToArray()
    }
    try {
        [IO.File]::WriteAllText($path, ($doc | ConvertTo-Json -Depth 4))
        Write-Host "results: $path"
    } catch { Write-Host "cannot write ${path}: $_" }
}

if (-not (Test-Path -LiteralPath $Hyperbutler)) { throw "no hyperbutler at $Hyperbutler (install the MSI, or pass -Hyperbutler)" }
if (-not $Docker) { $Docker = Join-Path (Split-Path $Hyperbutler) 'clients\docker.exe' }
$ownerJson = Join-Path $Dir 'owner.json'

if ($Role -eq 'owner') {
    $st = HB @('status', '--json')
    $tmp = Join-Path $env:TEMP ("hb-noadmin-status-" + [guid]::NewGuid().ToString('N').Substring(0, 8) + '.json')
    [IO.File]::WriteAllText($tmp, $st.Out)
    $logs = HB @('logs', '-n', '2000')
    Check 'owner: the engine booted through the HCS access broker' ($logs.Out -match 'booting through the HCS access broker') 'hyperbutler logs'

    New-Item -ItemType Directory -Force $Dir | Out-Null
    $script:helper = $Helper
    if (-not $script:helper) {
        $script:helper = Join-Path $Dir 'noadmin.exe'
        $repo = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..\..'))
        if (-not (Test-Path -LiteralPath (Join-Path $repo 'go.mod'))) { throw "no repository at $repo to build the helper from; pass -Helper" }
        $go = (Get-Command go -ErrorAction SilentlyContinue)
        if (-not $go) { throw "no go on PATH to build the helper; pass -Helper" }
        $b = Invoke-Tool $go.Source @('build', '-o', $script:helper, './bench/noadmin') $repo
        Check 'owner: build the noadmin helper' ($b.Code -eq 0) $b.Err
    } elseif ([IO.Path]::GetFullPath($Helper) -ne [IO.Path]::GetFullPath((Join-Path $Dir 'noadmin.exe'))) {
        Copy-Item -LiteralPath $Helper (Join-Path $Dir 'noadmin.exe') -Force
        $script:helper = Join-Path $Dir 'noadmin.exe'
    }
    $self = Join-Path $Dir 'run.ps1'
    if ([IO.Path]::GetFullPath($PSCommandPath) -ne [IO.Path]::GetFullPath($self)) { Copy-Item -LiteralPath $PSCommandPath $self -Force }

    $rec = @('record', '-status', $tmp, '-out', $ownerJson)
    if ($DebugPipe) { $rec += @('-debug-pipe', $DebugPipe) }
    Invoke-Helper '' $rec
    Remove-Item -LiteralPath $tmp -ErrorAction SilentlyContinue
    if (Test-Path -LiteralPath $ownerJson) {
        Invoke-Helper 'owner (positive control):' @('probe', '-owner', $ownerJson, '-expect', 'allow')
    }
    Write-Results
    if ($script:failed -gt 0) { Write-Host "$($script:failed) check(s) failed"; exit 1 }
    Write-Host 'all checks passed. Now, from this desktop, as the second account (its password typed by you):'
    Write-Host "  runas /user:hbtest `"powershell -NoProfile -ExecutionPolicy Bypass -NoExit -File $self -Role intruder`""
    exit 0
}

# --- intruder ---
if (-not (Test-Path -LiteralPath $ownerJson)) { throw "no ${ownerJson}: run -Role owner first, as the owner" }
$owner = Get-Content -Raw -LiteralPath $ownerJson | ConvertFrom-Json
$script:helper = if ($Helper) { $Helper } else { Join-Path $Dir 'noadmin.exe' }
if (-not (Test-Path -LiteralPath $script:helper)) { throw "no helper at $($script:helper)" }

# Everything of this account's own run lives under its %TEMP%: a test installation, a
# docker config of its own, and its own Docker API pipe (the owner holds the default).
$tag = [guid]::NewGuid().ToString('N').Substring(0, 8)
$root = Join-Path $env:TEMP "hb-noadmin-$tag"
New-Item -ItemType Directory -Force (Join-Path $root 'docker') | Out-Null
$env:HYPERBUTLER_HOME = Join-Path $root 'home'
$env:DOCKER_CONFIG = Join-Path $root 'docker'
Remove-Item Env:DOCKER_HOST, Env:DOCKER_CONTEXT -ErrorAction SilentlyContinue
$pipe = "\\.\pipe\docker_engine_hbnoadmin_$tag"
Write-Host "as $([Security.Principal.WindowsIdentity]::GetCurrent().Name), against $($owner.user)'s VM $($owner.vmRuntimeId); test installation $root"

# a. A standard user.
Invoke-Helper '(a)' @('groups')

# b, d. The owner's pipes, events and VM services.
Invoke-Helper '(b,d)' @('probe', '-owner', $ownerJson, '-expect', 'deny')

# c. The docker CLI on the owner's pipe.
$r = Invoke-Tool $Docker @('-H', (Npipe-Host $owner.dockerPipe), 'version', '--format', '{{.Server.Version}}')
# Go formats Windows errors in English whatever the display language. The docker CLI
# reports ERROR_ACCESS_DENIED (os.ErrPermission) as its own "permission denied while
# trying to connect" and drops the Windows text; a missing pipe reads differently.
$denied = $r.Err -match 'Access is denied|permission denied while trying to connect'
Check "(c) docker -H $(Npipe-Host $owner.dockerPipe) version is refused" (($r.Code -ne 0) -and $denied) "exit $($r.Code): $($r.Err) $($r.Out)"

# e, f. Its own hyperbutler, through the broker service.
$started = $false
try {
    $r = HB @('start', '--backend', 'broker', '--pipe', $pipe, '--no-context', '--memory', $Memory)
    $started = $r.Code -eq 0
    Check '(e) hyperbutler start --backend broker' $started "exit $($r.Code): $($r.Err)"
    $logs = HB @('logs', '-n', '2000')
    Check '(e) its engine booted through the HCS access broker' ($logs.Out -match 'booting through the HCS access broker') ''
    $s = From-Json (HB @('status', '--json')).Out
    $state = Prop $s 'state'
    $rid = Prop $state 'runtimeId'
    Check '(e) status: running, on its own pipe and its own VM' (([bool](Prop $s 'running')) -and ((Prop $state 'pipe') -eq $pipe) -and $rid -and ($rid -ne $owner.vmRuntimeId)) "pipe $(Prop $state 'pipe'), VM $rid"

    $dh = Npipe-Host $pipe
    if ($ImageTar) {
        if (-not $Image) { $Image = 'busybox:latest' }
        $r = Invoke-Tool $Docker @('-H', $dh, 'load', '-i', $ImageTar)
        Check "(e) docker load -i $ImageTar" ($r.Code -eq 0) $r.Err
        $r = Invoke-Tool $Docker @('-H', $dh, 'run', '--rm', '--network', 'none', $Image, 'echo', "hb-noadmin-$tag")
        Check "(e) docker run --rm $Image" (($r.Code -eq 0) -and ($r.Out -match "hb-noadmin-$tag")) "exit $($r.Code): $($r.Err)"
    } else {
        if (-not $Image) { $Image = 'hello-world' }
        $r = Invoke-Tool $Docker @('-H', $dh, 'run', '--rm', $Image)
        Check "(e) docker run --rm $Image" (($r.Code -eq 0) -and ($r.Out -match 'Hello from Docker')) "exit $($r.Code): $($r.Err)"
    }

    $d = HB @('doctor', '--json')
    $checks = @(Prop (From-Json $d.Out) 'checks')
    foreach ($want in @('hcs=info', 'hyperv-admins=info', 'broker=ok')) {
        $id, $status = $want -split '='
        $c = $checks | Where-Object { $_.id -eq $id } | Select-Object -First 1
        Check "(f) doctor $id is $status" (($null -ne $c) -and ($c.status -eq $status)) $(if ($c) { "$($c.status): $($c.summary)" } else { 'no such check' })
    }
} finally {
    if ($started) {
        $r = HB @('stop')
        Check '(e) hyperbutler stop' ($r.Code -eq 0) $r.Err
    }
    if (Test-Path -LiteralPath $env:HYPERBUTLER_HOME) {
        $r = HB @('uninstall', '--yes')
        Check '(e) uninstall the test installation' ($r.Code -eq 0) $r.Err
    }
    Remove-Item -Recurse -Force -LiteralPath $root -ErrorAction SilentlyContinue
}

# g. Still refused, and the owner's engine is still there (denied, not missing).
Invoke-Helper '(g)' @('probe', '-owner', $ownerJson, '-expect', 'deny')

Write-Results
if ($script:failed -gt 0) { Write-Host "$($script:failed) check(s) failed"; exit 1 }
Write-Host 'all checks passed'
